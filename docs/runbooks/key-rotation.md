# Runbook — Key and secret rotation

> References: PRD §23.2, §19.2, §27.3; ADR-003 (envelope encryption, key versioning); data dictionary §2.7 (`*_enc` format), §12 (inventory of encrypted columns), §4.8 (`api_tokens`), §6.3 (`private_keys`); OpenAPI (`/private-keys`, `/servers/{uuid}`, `/servers/{uuid}/validate`, `/teams/{team_uuid}/tokens`, `/system/encryption`, `/system/encryption/rotate`).

## Symptoms

Not applicable in normal operation (planned rotation). Emergency triggers: suspected leak of the master key, of an SSH key, of a webhook/OAuth secret or of an API token; departure of a privileged operator; compliance requirement.

## Impact

- Master key rotation: **no interruption** — re-encryption is progressive per key version, with no blocking rewrite (§19.2, ADR-003).
- Server SSH key rotation: brief window during which a deployment to that server may fail (automatic infra retry); workloads are not affected.
- API token revocation: immediately cuts off the integrations (CI, MCP) that use them.

## Diagnosis

Overview via the API (`root` permission): `GET /system/encryption` — active key version, versions still referenced in the database, row counters per version and per encrypted column, re-encryption job in progress if any.

Format reminder (data dictionary §2.7): each `*_enc` column is `key_version (4 bytes big-endian) || nonce (12) || ciphertext AES-256-GCM`. The key version is therefore readable in SQL. Histogram of versions in use (equivalent SQL fallback):

```sql
-- example on private_keys; repeat on each encrypted column (list in data dictionary §12):
-- private_keys.private_key_enc, mfa_factors.secret_enc, cloud_credentials.config_enc,
-- registry_credentials.password_enc, s3_storages.access_key_enc + secret_key_enc,
-- github_apps.client_secret_enc + webhook_secret_enc + app_private_key_enc,
-- webhook_endpoints.secret_enc, environment_variables.value_enc, shared_variables.value_enc,
-- database_credentials.password_enc, servers.ca_key_enc + log_drain_config_enc,
-- notification_channels.config_enc, instance_settings.transactional_email_config_enc
SELECT (get_byte(private_key_enc,0)<<24) | (get_byte(private_key_enc,1)<<16)
     | (get_byte(private_key_enc,2)<<8)  |  get_byte(private_key_enc,3) AS key_version,
       count(*)
FROM private_keys GROUP BY 1 ORDER BY 1;
```

Servers sharing the same SSH key (to know before any revocation):

```sql
SELECT pk.uuid AS key_uuid, pk.name, count(s.id) AS servers, array_agg(s.name) AS server_names
FROM private_keys pk LEFT JOIN servers s ON s.private_key_id = pk.id AND s.deleted_at IS NULL
GROUP BY 1,2 ORDER BY 3 DESC;
```

## Step-by-step resolution

### A. Master key rotation (envelope — ADR-003)

1. **Add** a new version to the key file, **without deleting the old ones**:
   ```sh
   cd /var/lib/akerdock
   cp keys/master.key keys/master.key.bak-$(date -u +%Y%m%d)
   printf '2:%s\n' "$(openssl rand -base64 32)" >> keys/master.key
   ```
   ⚠️ **Never remove a version while at least one row in the database references it**: the corresponding ciphertexts would become permanently unreadable.
2. **Reload**: `docker compose up -d AkerDock` — the active version for new encryptions becomes the highest one **(normative: spec [instance-config](../specs/instance-config.md) §3)**.
3. Immediately back up the new `master.key` off the machine (same rules as at installation).
4. **Progressive re-encryption**: rows are rewritten lazily on read/write (§19.2). To force full convergence — necessary if the rotation is in response to a leak — trigger active re-encryption to the active version:
   ```sh
   curl -sS -X POST "$AKD/system/encryption/rotate" \
     -H "Authorization: Bearer $ROOT_TOKEN" -H "Idempotency-Key: rotate-$(date -u +%Y%m%d)"
   # 202 + audited job; batched rewrite, non-blocking; 409 if a re-encryption is already running
   ```
   Track progress with `GET /system/encryption` (row counters per key version and per column); the Diagnosis SQL histogram remains the fallback, column by column.
5. When **no row at all** carries the old version anymore (`GET /system/encryption`: version 2 is the only one referenced — or SQL histogram = version 2 only, across the 16 columns of the §12 list): remove the `1:` line from `master.key`, reload, re-back up off the machine.

⚠️ **Confirmed master key leak case**: rotation is not enough if the attacker also has a database dump (they can read everything that was encrypted with the old version). Then treat every secret as compromised: rotation **at the source** (DB passwords, DNS/registry/S3 tokens, webhook secrets, SSH keys — see sections B/C/D), not just re-encryption.

### B. Rotation of a server SSH key

Keys are separable per server (§23.1) — rotation is done server by server, with no interruption:

1. Generate and register a new key (see [install.md](install.md) step 5): `POST /private-keys` → note `private_key_uuid`.
2. **Install the new public key first** on the server (via the AkerDock web terminal, or direct SSH access):
   ```sh
   echo 'ssh-ed25519 AAAA… akerdock-server-01-2026' >> ~/.ssh/authorized_keys
   ```
3. Switch the server to the new key:
   ```sh
   curl -sS -X PATCH "$AKD/servers/$SERVER_UUID" \
     -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d "{\"private_key_uuid\":\"$NEW_KEY_UUID\"}"
   curl -sS -X POST "$AKD/servers/$SERVER_UUID/validate" -H "Authorization: Bearer $TOKEN"
   ```
4. After `ready` validation: **remove the old public key** from `authorized_keys` on the server.
5. Delete the old key on the AkerDock side (`DELETE /private-keys/{uuid}`) — refused with **RESTRICT** as long as a server or a git source references it (§19.2): use the "servers sharing a key" query from the Diagnosis to find the remaining references.

⚠️ Never swap steps 4 and 3: removing the old key from `authorized_keys` before the switch locks you out (recovery = provider console).

### C. Rotation of webhook / OAuth secrets

- **Inbound webhook (HMAC, `webhook_endpoints.secret_enc`)**: generate a new secret (`openssl rand -hex 32`), update it **on the AkerDock side first** (application UI → webhook; no dedicated endpoint in OpenAPI v1 — **future API/CLI candidate**), **then on the Git provider side** (repo settings). In this order, the invalidity window produces `signature_valid = false` deliveries visible in `webhook_deliveries`, without undue triggering (INV-009).
- **GitHub App (`github_apps.client_secret_enc`, `webhook_secret_enc`, `app_private_key_enc`)**: regenerate on github.com (Settings → Developer settings → GitHub Apps), copy into the AkerDock UI. The two secrets can briefly coexist on the GitHub side (multiple client secrets) — take advantage of this for a zero-downtime rotation.
- **Dashboard OAuth (Azure/GitHub/GitLab/Google/Bitbucket/OIDC, §10.2)**: regenerate the client secret at the IdP, copy into the instance settings. Open sessions are not cut off; only new logins use the new secret.

Immediate verification: trigger an event (test push) and check:

```sql
SELECT delivery_id, event_type, signature_valid, status, ignore_reason, received_at
FROM webhook_deliveries ORDER BY received_at DESC LIMIT 5;
```

### D. Emergency revocation of API tokens

Token by token (audited, preferred):

```sh
curl -sS "$AKD/teams/$TEAM_UUID/tokens" -H "Authorization: Bearer $TOKEN"     # inventory
curl -sS -X DELETE "$AKD/teams/$TEAM_UUID/tokens/$TOKEN_UUID" -H "Authorization: Bearer $TOKEN"
```

**Mass** revocation (general leak, SQL last resort — bypasses the audit, log it manually) **(future CLI candidate)**:

```sql
UPDATE api_tokens SET revoked_at = now(), updated_at = now() WHERE revoked_at IS NULL;
```

Shut down the entire API for the duration of the investigation (reversible): `POST /system/api/disable` (`root` permission) or the instance settings toggle — after this call, only `GET /health` and re-enabling via the UI remain available. Identify recent usage of the compromised tokens:

```sql
SELECT token_prefix, name, last_used_at, ip_allowlist, permissions
FROM api_tokens ORDER BY last_used_at DESC NULLS LAST LIMIT 20;
-- and the audit of the token's actions:
SELECT occurred_at, action, target_kind, target_uuid, result, ip
FROM audit_events WHERE actor_kind = 'token' AND actor_uuid = '<token_uuid>'
ORDER BY occurred_at DESC LIMIT 100;
```

## Verification

- Master key: `GET /system/encryption` (or SQL histograms) = only the new version referenced, re-encryption job `succeeded`; a `docker compose restart AkerDock` then revealing a secret (with `read:sensitive`) proves that the new key decrypts.
- SSH key: `POST /servers/{uuid}/validate` → `ready`; a test deployment passes.
- Webhook: test delivery `signature_valid = true`, deployment triggered.
- Tokens: the old token responds `401`; the `token.*` audit entry exists.

## Prevention

- One SSH key **per server** (never shared); calendar-based rotation (e.g. yearly) rather than reactive.
- API tokens with `expires_at`, minimal permissions (`deploy` for CI, §16.3) and CIDR `ip_allowlist` (§10.3).
- The master key and the database dumps MUST **never** be stored in the same place (§23.1).
- Test the master key rotation procedure cold (staging) before needing it hot; the "key rotation during a job" scenarios are part of the mandatory tests (§23.5).
