# Runbook — Certificates: ACME failures, self-signed fallback, expirations, custom, wildcard

> References: PRD §4.2–4.3 (Let's Encrypt HTTP-01, self-signed fallback, DNS-01 wildcard via Lego, custom certs in `proxy/certs`, validation DNS), §14.2 (`dns_validation_server`); proxy-contract spec §7 (certificates, §7.6 synchronization); deployment-engine spec §5.1 (`/var/lib/akerdock/proxy/certs/`); data dictionary §6.7 (`certificates` table, observed reflection), §11.7 (`instance_settings.dns_validation_server`), §6.1 (`servers.wildcard_domain`, `proxy_http_port`).

> Note: the control plane maintains an **observed reflection** of certificates (`certificates` table) exposed by the API: `GET /servers/{uuid}/certificates` (`expiring_within_days` filter), `GET /certificates/{uuid}`, `POST /certificates/{uuid}/renew` (202 + audited job). Fine-grained diagnosis always goes through the server and the proxy logs. **Normative** locations (proxy-contract §7.2/§7.5): ACME storage `/var/lib/akerdock/proxy/acme.json` (0600), DNS-01 credentials `/var/lib/akerdock/proxy/acme.env` (0600).

## Symptoms

- Browser: TLS warning (**self-signed** certificate = the issuance-failure fallback is active, §4.3) or **expired** certificate.
- Proxy logs: ACME errors (`unable to obtain ACME certificate`, `acme: error: 429 … rateLimited`, `NXDOMAIN`, `connection refused` on the challenge).
- A freshly added domain never reaches valid HTTPS; the server's other domains are OK.

## Impact

- Self-signed active: the site responds but clients see a warning; integrations that verify TLS fail.
- Expired: most clients refuse the connection — equivalent to an outage for browsers.
- An ACME failure only affects the domain(s) concerned, not HTTP routing nor the server's other certificates.

## Diagnosis

1. **What is being served, and until when?** — first the control plane reflection, then the TLS reality:
   ```sh
   curl -sS "$AKD/servers/$SERVER_UUID/certificates?expiring_within_days=30" \
     -H "Authorization: Bearer $TOKEN"    # inventory: kind, domains, status, not_after, last_error
   echo | openssl s_client -connect <fqdn>:443 -servername <fqdn> 2>/dev/null \
     | openssl x509 -noout -issuer -subject -enddate
   # issuer = Let's Encrypt (R…) expected; "TRAEFIK DEFAULT CERT" or self-signed = fallback active
   # (the reflection is synchronized after each proxy apply — check observed_at when in doubt)
   ```
2. **Proxy ACME logs**:
   ```sh
   ssh <user>@<server> "docker logs --tail 200 \$(docker ps -q --filter label=akerdock.type=proxy) 2>&1 | grep -i acme"
   ```
3. **DNS** — does the domain point to the server, as seen from the instance's validation DNS (§4.2, default `1.1.1.1`, custom: `instance_settings.dns_validation_server`)?
   ```sh
   dig +short <fqdn> @1.1.1.1
   ssh <user>@<server> "curl -s ifconfig.me"     # server's public IP — must match
   ```
4. **Port 80** — HTTP-01 requires that Let's Encrypt reach the server's **public port 80**:
   ```sh
   curl -s -o /dev/null -w '%{http_code}\n' http://<fqdn>/.well-known/acme-challenge/probe
   # 404 served by Traefik = reachable (sufficient); timeout/refused = firewall or diverted port
   ```
   ⚠️ If `servers.proxy_http_port` ≠ 80 (upstream reverse proxy, §27.1), HTTP-01 can only succeed if the upstream forwards port 80 to the proxy — otherwise use DNS-01.
5. **Let's Encrypt rate limits**: `429 rateLimited` in the logs. Usual limits: 5 exact-duplicate certificates/week, 50 certificates/registered domain/week, 5 validation failures/account/hostname/hour. The error indicates the retry delay.

## Step-by-step resolution

### A. Issuance failure (self-signed fallback active)

1. Fix the cause identified during diagnosis:
   - **DNS**: fix the A/AAAA record (or the wildcard entry); wait for propagation as seen from the validation DNS (diagnosis 3).
   - **Port 80**: open it at the **cloud provider's** firewall level (§10.4 — Docker bypasses UFW); check that no other process is listening (`ss -ltnp | grep ':80'`).
   - **Rate limit**: wait out the indicated delay. ⚠️ Do not loop retries (each validation failure consumes the "5 failures/hour" limit). To debug without consuming quota, test the chain with the Let's Encrypt staging CA on a throwaway domain.
2. Force a new issuance attempt:
   ```sh
   curl -sS -X POST "$AKD/certificates/$CERT_UUID/renew" -H "Authorization: Bearer $TOKEN"
   # 202 + audited job: backup then targeted removal of the acme.json entry, restart of the
   # proxy, resynchronization of the reflection; 422 for a custom/self_signed certificate
   ```
   Failing that, redeploy the application (regeneration of the proxy config) or restart the proxy — Traefik retries issuance for domains without a valid certificate at startup.
3. If Traefik has memorized a corrupted ACME state: the `renew` job (A.2) performs exactly the targeted edit; by hand (fallback), intervene on the ACME storage (**normative** location: `/var/lib/akerdock/proxy/acme.json`):
   ```sh
   ssh <user>@<server> "cp -a /var/lib/akerdock/proxy/acme.json /var/lib/akerdock/tmp/acme.json.bak-\$(date -u +%s)"
   # targeted edit: remove only the entry for the failing domain, then docker restart <proxy>
   ```
   ⚠️ **Deleting the entire `acme.json` forces re-issuance of ALL the server's certificates** → direct risk of rate limiting. Always take a backup first, and make a targeted edit.

### B. Expired certificate

An expired certificate = a **renewal** that has been failing for weeks (Traefik renews ~30 days before expiry). Run through diagnosis A — the cause is almost always modified DNS, port 80 closed since the initial issuance, or a proxy that stayed down during the renewal window. Fix it then force re-issuance (A.2).

### C. Custom certificates (§4.3)

1. Deposit key + fullchain on the server:
   ```sh
   scp fullchain.pem privkey.pem <user>@<server>:/var/lib/akerdock/proxy/certs/<fqdn>/
   ssh <user>@<server> "chmod 0600 /var/lib/akerdock/proxy/certs/<fqdn>/privkey.pem"
   ```
2. Reference them via the dynamic configuration managed by AkerDock (server/domain UI — the generated config adds the `tls.certificates` section).
3. Classic checks: key/cert match (`openssl x509 -noout -modulus | openssl md5` vs `openssl rsa -noout -modulus | openssl md5`), **chain order** (leaf first), dates.
4. ⚠️ Custom certs do not renew themselves (`POST /certificates/{uuid}/renew` responds `422` for a `custom`): their expiration is tracked in the `certificates` reflection (D-30/D-7 alert) — check that they do appear in `GET /servers/{uuid}/certificates` after deposit (Prevention).

### D. Wildcard via DNS-01 (§4.3)

1. Prerequisites: DNS provider supported by Lego (Cloudflare, Route 53, OVH, Hetzner…) and its credentials configured for the server's proxy (materialized in `/var/lib/akerdock/proxy/acme.env`, 0600 — normative location; referenced via `certificates.dns_credential_id`); `servers.wildcard_domain` filled in (§4.2).
2. Typical failure: invalid/expired DNS credentials (rotate them → [key-rotation.md](key-rotation.md)) or slow TXT propagation. Check the challenge:
   ```sh
   dig +short TXT _acme-challenge.<domain> @1.1.1.1
   ```
3. DNS-01 does **not** depend on port 80 — it is the fallback solution when port 80 is structurally unavailable (diagnosis 4).

## Verification

```sh
echo | openssl s_client -connect <fqdn>:443 -servername <fqdn> 2>/dev/null \
  | openssl x509 -noout -issuer -enddate      # issuer Let's Encrypt, notAfter ≈ +90 days
curl -fsS -o /dev/null https://<fqdn>/        # without -k: the chain validates end to end
```

- No more ACME errors in the proxy logs.
- For a wildcard: two distinct subdomains serve the same `*.domain` certificate.
- "Force HTTPS" (§4.3): `curl -s -o /dev/null -w '%{http_code}' http://<fqdn>/` → 301/308.

## Prevention

- **Monitor expirations**: `GET /servers/{uuid}/certificates?expiring_within_days=14` (`certificates` reflection, index on `not_after`; built-in alert at D-30/D-7 — proxy-contract §7.3); in addition, built-in uptime check (§27.17) or external cron `openssl x509 -checkend 1209600` on each critical FQDN.
- Prefer a **wildcard per server** (§4.2) when subdomains proliferate: fewer issuances, fewer rate limits.
- Do not close port 80 "for security" on an HTTP-01 server: renewal depends on it (the Force HTTPS redirect functionally neutralizes it).
- Any DNS change to a served domain = re-check issuance at the next renewal (set a reminder).
- Back up `/var/lib/akerdock/proxy/` (acme.json + custom certs) in the server backups: in case of a rebuild, this avoids a mass re-issuance (rate limits).
