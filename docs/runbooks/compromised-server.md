# Runbook — Compromised target server

> References: PRD §23.1 ("A compromised target server must not give access to the other servers: separable keys/credentials and secrets distributed on a strict need-to-know basis"), §23.2, §20.7 (adoption), INV-008; deployment-engine spec §5.1–5.2 (what is materialized on the server); data dictionary §6.1 (`servers`), §6.3 (`private_keys`), §12.

## Symptoms

- Security alert (IDS, cloud provider, abuse report), unknown containers/processes, abnormal outbound traffic, inconsistent Sentinel metrics, unexplained modification of `/var/lib/akerdock/` on the server.
- On the AkerDock side: deployments failing strangely on this server, proxy checksum drift, unmanaged resources appearing (`docker ps` without the `akerdock.managed` label).

## Impact

What the attacker possesses (root on the target server):

- **All the server's workloads** and their data (volumes, databases residing there).
- **The secrets distributed to the server** — and only those (§23.1, need-to-know distribution): `runtime.env`/`build.env`/`secrets/` files under `/var/lib/akerdock/applications/*/env/` (spec §5.1–5.2), TLS certificates (`/var/lib/akerdock/proxy/`), registry credentials present in `/root/.docker/config.json` after `docker login`, the server's Sentinel token.
- What the attacker does **not** possess: the control plane database, the master key, the **private SSH keys** (they never leave the control plane; only the *public* key is in `authorized_keys`), the secrets of other servers/teams. The architecture is push-based: the server has no credential to contact the control plane, apart from the Sentinel token (limited to metrics push).

## Diagnosis

1. **Scope of the SSH key** — is the server's key shared? (it should not be):
   ```sql
   SELECT s2.uuid, s2.name FROM servers s1
   JOIN servers s2 ON s2.private_key_id = s1.private_key_id AND s2.deleted_at IS NULL
   WHERE s1.uuid = '<server_uuid>';
   -- + git sources using this key (deploy keys): check the private_keys references
   ```
2. **Inventory of what was exposed on this server**:
   ```sh
   curl -sS "$AKD/servers/$SERVER_UUID/resources" -H "Authorization: Bearer $TOKEN"
   ```
   ```sql
   -- Environment variables of the server's resources
   -- (the values were materialized in cleartext in runtime.env/build.env on the server, spec §5.2):
   SELECT r.uuid AS resource_uuid, r.name, r.resource_type, ev.key, ev.is_secret, ev.is_preview
   FROM resources r
   JOIN destinations d ON d.id = r.destination_id
   JOIN servers s ON s.id = d.server_id
   LEFT JOIN environment_variables ev ON ev.resource_id = r.id
   WHERE s.uuid = '<server_uuid>' AND r.deleted_at IS NULL
   ORDER BY r.name, ev.key;
   ```
   Complete the inventory: **registry** credentials used by the server's apps (`registry_credentials` referenced by their build configs), **S3** credentials used by the backup plans of the server's databases, **domains/certificates** served by its proxy, **databases** residing there (`database_credentials`).
3. **What the attacker could have done on the AkerDock side**: nothing directly (no inbound credential), but check the audit for any abnormal activity around the server:
   ```sql
   SELECT occurred_at, actor_kind, actor_display, action, result, ip
   FROM audit_events WHERE target_uuid = '<server_uuid>' ORDER BY occurred_at DESC LIMIT 50;
   ```

## Step-by-step resolution

### 1. Isolate (without destroying evidence)

1. **Freeze orchestration**: put the server in maintenance to prevent any new job from targeting it (the `preparing` state requires a `ready` server, spec §4) — no dedicated endpoint in OpenAPI v1 **(future CLI/API candidate)**:
   ```sql
   UPDATE servers SET status = 'maintenance', updated_at = now() WHERE uuid = '<server_uuid>';
   ```
2. **Revoke only this server's SSH key** (keys are separable, §23.1 — the other servers are not affected). The private key has not leaked, but we cut the orchestration channel to a hostile machine and prepare the re-installation: stop using it, and if the key was shared (diagnosis 1), **rotate immediately on the other servers** via [key-rotation.md](key-rotation.md) §B.
3. **Isolate the network** at the cloud provider's firewall level (§10.4: do not rely on UFW, Docker bypasses it): block everything except your investigation IP. If the server carried production traffic, this is an assumed service outage — a compromised server must not keep serving your users.
4. **Revoke the server's Sentinel token** (regeneration in the server UI; the `servers.sentinel_token_hash` hash changes) to cut the only inbound channel to the instance.

⚠️ Do not delete the `Server` object: deletion is RESTRICT as long as resources are attached to it (INV-008), and you would lose the inventory. "Remove from AkerDock" ≠ "destroy the VPS" (§3.2) — and neither is done before the investigation is over.

### 2. Targeted rotation (everything that was distributed to the server)

Treat as compromised, **at the source**:

- All **secret variables** of the server's resources (inventory from Diagnosis 2): regenerate the values at the relevant providers (third-party API keys, etc.) and update them in AkerDock.
- **Passwords of the databases** hosted on the server (`database_credentials`) and of any external database whose URL appeared in a `runtime.env` on the server.
- **Registry credentials** used by the server's apps (rotation on the registry side + `registry_credentials` update).
- **S3 credentials** of the backup plans executed from this server (rotation on the provider side + `s3_storages` update; `ListObjectsV2` re-verification §7.4).
- **TLS certificates**: the private keys were on the server (`/var/lib/akerdock/proxy/certs`, ACME storage) — revoke the custom ones, force ACME re-issuance on the replacement server ([certificates.md](certificates.md)).
- **Database SSL CA** of the server (`servers.ca_key_enc`): regenerate from the UI (§6.3).
- **Git deploy keys**: normally deleted after clone (spec §5.3.1), but if a long-lived compromise is suspected, remove them from the repos and generate new ones.

### 3. Decision: reinstall or adopt

- **Reinstall (strongly recommended)**: OS reinstalled at the provider → new AkerDock server (new dedicated SSH key, `POST /servers` + validate) → redeploy the resources from their configuration (source of truth = PostgreSQL, §18.3) → restore the **data** (volumes, databases) **only from backups predating the compromise**, after checking the checksums (`backup_executions.checksum_sha256`). ⚠️ A backup made after the intrusion may be booby-trapped/tampered with.
- **Adopt (§20.7)**: only if the forensics conclude a false positive or a strictly contained compromise (e.g. a single application container with no escape): targeted rotation anyway, then revalidation of the server. When in doubt: reinstall.

### 4. Closure

Once the resources are redeployed elsewhere or on the reinstalled server: delete the remaining resources of the old server object (preview + explicit data/object choice, §20.6), then the server; finally destroy the VPS at the provider (a separate, confirmed action, §3.2). Record the incident (timeline, scope, rotations performed).

## Verification

- The old public key no longer opens anything (it is no longer in any active `authorized_keys`); the key is no longer referenced (`private_keys` deletable without RESTRICT).
- The rotated secrets work: test deployment OK, backups OK (S3 re-verified `is_usable = true`), webhooks OK.
- No resource from the inventory has been forgotten: re-run the inventory query → empty list or migrated resources.
- Audit: the rotations and deletions appear in `audit_events`.

## Prevention

- **One SSH key per server**, never shared (makes this runbook local instead of global).
- Secrets distributed on a strict need-to-know basis (§23.1): do not put global "convenience" variables in resources; use BuildKit build secrets (outside the image, spec §5.2).
- Isolated builders for untrusted code (ADR-005); fork previews without secrets (INV-010).
- Restrictive provider firewall (SSH from the instance's IP only if possible); regular server patching — the operator's responsibility, outside the product's scope (ADR-027).
- Backups with checksums and sufficient retention to have a point **predating** an intrusion discovered late.
