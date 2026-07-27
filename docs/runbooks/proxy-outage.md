# Runbook — Proxy down or corrupted configuration

> References: PRD §4.1 (proxy lifecycle, "stopping the proxy cuts all inbound traffic of the server"), §18.3 (routing: deterministic generation + validation + checksum), §20.1(5); ADR-009 (intermediate representation); deployment-engine spec §7.1–7.2; data dictionary §11.1 (`proxy_config_revisions`), §6.1 (`proxy_*` columns of `servers`).

## Symptoms

- **All** sites on the same server are down (timeout, connection refused, 502) — apps on other servers respond.
- Dashboard: the server's `proxy_observed_status` is `unhealthy`/`exited`; "proxy" notification (§11).
- Corrupted config: the proxy runs but routes wrong (Traefik 404 "page not found", wrong backend), Traefik logs full of `file` provider errors.

## Impact

- All **inbound traffic** of the server is cut or degraded (§4.1). Application containers, databases and tasks **keep running**: only external HTTP(S) access is broken.
- The control plane and the other servers are not affected (per-server proxy, §3.3).

## Diagnosis

1. **The proxy container** (on the server):
   ```sh
   ssh <user>@<server> "docker ps -a --filter label=akerdock.type=proxy --format '{{.Names}}\t{{.Status}}\t{{.Image}}'"
   ssh <user>@<server> "docker logs --tail 100 \$(docker ps -aq --filter label=akerdock.type=proxy)"
   ssh <user>@<server> "curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:80 ; curl -sk -o /dev/null -w '%{http_code}\n' https://127.0.0.1:443"
   ```
   (Ports 80/443 = defaults; check `servers.proxy_http_port`/`proxy_https_port`, configurable per server — §27.1.)
2. **The dynamic configuration** — drift from the last applied revision (§18.3):
   ```sql
   SELECT revision, status, checksum_sha256, applied_at, error
   FROM proxy_config_revisions
   WHERE server_id = (SELECT id FROM servers WHERE uuid = '<server_uuid>')
   ORDER BY revision DESC LIMIT 5;
   ```
   ```sh
   ssh <user>@<server> "ls -la /var/lib/akerdock/proxy/dynamic/ && sha256sum /var/lib/akerdock/proxy/dynamic/*.yaml"
   ```
   A file with a checksum unknown to the database = manual edit or corruption; an invalid YAML file is named explicitly in the Traefik logs (`error while parsing dynamic configuration`).
3. **Desired vs observed state**: `proxy_desired_state` must be `running` — if it is `stopped`, someone stopped the proxy deliberately (audit: `action LIKE 'server.proxy%'` in `audit_events`).
4. Distinguish from a **certificates** failure (sites up over HTTP, TLS errors only) → [certificates.md](certificates.md).

## Step-by-step resolution

### A. Proxy container stopped or crash-looping

1. Restart via the server UI (proxy lifecycle: start/restart, §4.1) — that is the audited path. Failing that, on the server:
   ```sh
   ssh <user>@<server> "docker restart \$(docker ps -aq --filter label=akerdock.type=proxy)"
   ```
2. If it crashes at boot because of an invalid static/dynamic file → case B.
3. If it crashes for another reason (port already taken, corrupted image): free the port (`ss -ltnp | grep ':80'`), or re-provision the proxy (case C).

### B. Corrupted dynamic configuration — restore the last valid revision

The file is authoritative for routing (spec §7.1); the database keeps every generated revision with its content (`proxy_config_revisions.content`):

1. Extract the last `applied` revision **(future CLI candidate — `AkerDock proxy restore <server>`)**:
   ```sh
   docker compose exec -T postgres psql -U AkerDock AkerDock -At -c "
     SELECT content FROM proxy_config_revisions
     WHERE server_id = (SELECT id FROM servers WHERE uuid = '<server_uuid>')
       AND status = 'applied'
     ORDER BY revision DESC LIMIT 1;" > /tmp/proxy-restore.yaml
   ```
2. Save the corrupted state then apply **atomically** (same mechanics as the engine: tmp + `mv -f`, spec §7.2.3):
   ```sh
   scp /tmp/proxy-restore.yaml <user>@<server>:/var/lib/akerdock/proxy/dynamic/.restore.tmp
   ssh <user>@<server> "cp -a /var/lib/akerdock/proxy/dynamic /var/lib/akerdock/tmp/dynamic-corrupted-\$(date -u +%s) \
     && mv -f /var/lib/akerdock/proxy/dynamic/.restore.tmp /var/lib/akerdock/proxy/dynamic/<target_file>.yaml"
   ```
   Traefik's `file` provider (`watch: true`) reloads without a restart.
   > If the corruption affects several applications, a safer alternative: **redeploy each affected application** — every deployment regenerates its `/var/lib/akerdock/proxy/dynamic/<app_uuid>.yaml` file deterministically from the intermediate representation (spec §7.2.7).
3. ⚠️ Never "hand-fix" a dynamic file and leave it at that: the database would not know that content (diverging checksum) and the next generation will overwrite it. Any manual fix must converge to a redeploy/regeneration by AkerDock.

### C. Full proxy redeployment

If the proxy container itself is unrecoverable (corrupted image, broken static config):

1. ```sh
   ssh <user>@<server> "docker stop \$(docker ps -aq --filter label=akerdock.type=proxy) ; docker rm \$(docker ps -aq --filter label=akerdock.type=proxy)"
   ```
   ⚠️ **Total inbound traffic outage for the server** between the `rm` and the end of re-provisioning — a window to announce. Certificates and dynamic files under `/var/lib/akerdock/proxy/` (bind mount) **survive** the container removal.
2. Re-run the server validation, which redeploys and verifies the proxy (onboarding workflow §20.1, step 5):
   ```sh
   curl -sS -X POST "$AKD/servers/$SERVER_UUID/validate" -H "Authorization: Bearer $TOKEN"
   ```

## Verification

- Proxy container `running`; `proxy_observed_status` back to `healthy` on the dashboard.
- **Checksum aligned**: remote `sha256sum` = `checksum_sha256` of the last `applied` revision in the database (§18.3).
- Every critical domain responds through the proxy:
  ```sh
  curl -fsS -o /dev/null -w '%{http_code} %{ssl_verify_result}\n' --resolve <fqdn>:443:<server_ip> https://<fqdn>/
  ```
- A test deployment on this server passes the `switching` step (proxy verification included, spec §7.2.4).
- Traefik logs silent about the `file` provider.

## Prevention

- Never edit `/var/lib/akerdock/proxy/dynamic/` by hand; proxy config editing goes through the UI (§4.1), versioned in `proxy_config_revisions`.
- Watch the "proxy outdated" notification (§11) and update the proxy image in a chosen window.
- Enable the built-in uptime monitoring (§27.17) on at least one domain per server: detection in seconds rather than at the first user ticket.
- Revision retention ("purge keeping the last N per server", data dictionary §11.1) is your rollback depth — do not reduce it to 1.
