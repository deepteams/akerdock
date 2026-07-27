# Runbook — Total loss of the instance: restore on a fresh machine

> References: PRD §7.5, §16.4 (RTO ≤ 2 h), §22.1 ("A documented procedure restores PostgreSQL, encryption keys, SSH keys, proxy configurations and required files"), INV-007; ADR-021; ADR-003; deployment-engine spec §2.5, §7.

## Symptoms

- The machine hosting the instance is lost (dead disk, deleted VPS, datacenter) or unrecoverable.
- **The applications, databases and proxies on the target servers keep running normally** (INV-007): end users see nothing — only orchestration is lost.

## Impact

- No more UI/API/deployments/notifications/scheduled backups until the instance is restored.
- RPO window: everything after the last database backup is lost (see [postgres-failure.md](postgres-failure.md) §D).
- Existing certificates keep being served by the proxies; their **ACME renewals** do not depend on the instance (the proxy handles them locally) but domain changes do.

## Restore prerequisites — the 3 pieces

| Piece | Source | Without it |
|---|---|---|
| PostgreSQL dump | S3 of the `is_instance_backup` plan, or exfiltrated local copy | Full manual rebuild (re-onboarding of all servers) |
| **Master key** (`master.key`, all versions) | Vault/secret manager off the machine ([install.md](install.md) step 2) | ⚠️ **All the secrets in the dump are unrecoverable** (ADR-003): SSH keys, encrypted variables, S3/registry/cloud credentials — see "Degraded case" below |
| `docker-compose.yml` + `.env` (or their content: **exact image tag**, port, PG password) | Off-machine copy / infra Git | Reconstructible from [install.md](install.md), but the **image tag MUST match the dump's schema** |

## Diagnosis

Before restoring, secure what still exists:

```sh
# The most recent dump on S3 (endpoint/bucket of the instance plan):
aws s3 ls s3://<bucket>/<prefix>/ --endpoint-url <endpoint> | sort | tail -5
# The target servers are still running (from your workstation, with emergency SSH access):
ssh <user>@<server> "docker ps --filter label=akerdock.managed=true --format '{{.Names}}\t{{.Status}}'"
```

## Step-by-step resolution

### 1. Fresh machine

Provision a machine that meets the prerequisites of [install.md](install.md) (Docker ≥ 24, Compose v2). If possible, reuse the same IP; otherwise plan for the DNS step (5).

### 2. Rebuild the directory tree

```sh
mkdir -p /var/lib/akerdock/keys /var/lib/akerdock/postgres /var/lib/akerdock/backups
# Restore the 3 pieces:
#   /var/lib/akerdock/docker-compose.yml, /var/lib/akerdock/.env  (image tag = the one before the loss ⚠️)
#   /var/lib/akerdock/keys/master.key   (0600 root:root, umask 077)
chmod 0700 /var/lib/akerdock/keys && chmod 0600 /var/lib/akerdock/keys/master.key
```

⚠️ Starting with an **image tag more recent** than the dump's would apply migrations at boot: feasible, but it mixes restore and upgrade. Restore identically first, upgrade afterwards ([upgrade-downgrade.md](upgrade-downgrade.md)).

### 3. Restore the database

```sh
cd /var/lib/akerdock
docker compose up -d postgres
# wait for pg_isready:
docker compose exec postgres pg_isready -U AkerDock
docker compose exec -T postgres pg_restore -U AkerDock -d AkerDock --no-owner --exit-on-error \
  < /path/to/dump/akerdock-instance-….dump
docker compose up -d AkerDock
```

### 4. Verify decryption (the proof that the master key is the right one)

In the UI, reveal any secret (`read:sensitive` permission) or validate a server (step 6) — the SSH connection requires decrypting `private_keys.private_key_enc`. A decryption error here = wrong key version in `master.key`: do not go any further, find the right file.

### 5. Repoint the instance DNS

Point the instance FQDN (§14.2) to the new IP. Inbound webhooks (GitHub/GitLab…) start arriving again as soon as propagation completes — this is intended.

### 6. Reconnection to the existing servers

The servers, keys and resources are in the dump; nothing to re-register. For each server:

```sh
curl -sS -X POST "$AKD/servers/$SERVER_UUID/validate" -H "Authorization: Bearer $TOKEN"
```

Validation retests SSH, Docker, network, proxy and Sentinel (§20.1) and moves the server back to `ready`. No `authorized_keys` to modify: the restored private keys are the ones the servers already know.

### 7. Reconciliation of observed states (INV-007)

The workloads kept running during the outage — the database is behind reality:

1. **Observed statuses**: marked stale (old `observed_at`); reconciliation refreshes them; destructive actions are suspended as long as the observation is too old (§21.2) — expected behavior, do not force.
2. **Jobs in flight at the time of the loss**: expired leases → automatic recovery through remote inspection (spec §2.5). Handle leftovers via [orphaned-deployment.md](orphaned-deployment.md) (possible `-next` containers).
3. **Proxy drift**: compare the actual checksum to the last known applied one:
   ```sql
   SELECT s.name, r.revision, r.checksum_sha256, r.applied_at
   FROM proxy_config_revisions r JOIN servers s ON s.id = r.server_id
   WHERE r.status = 'applied'
     AND r.revision = (SELECT max(revision) FROM proxy_config_revisions WHERE server_id = s.id AND status = 'applied');
   ```
   ```sh
   ssh <user>@<server> "cat /var/lib/akerdock/proxy/dynamic/*.yaml | sha256sum"
   ```
   Discrepancy = a deployment happened in the RPO window; see [postgres-failure.md](postgres-failure.md) §D.2 (redeploy the resource to realign).
4. **RPO window** (lost deployments/webhooks/objects): run through [postgres-failure.md](postgres-failure.md) §D in full.
5. **Sentinel**: the agents push to the old endpoint until DNS/IP have converged; they reconnect on their own after step 5 (push architecture, §3.8).

### Degraded case: dump present, master key lost

The unencrypted data (teams, projects, servers, resources, history) is intact; **everything that is `*_enc` is lost** (the 16 columns of data dictionary §12). You then have to, in order: create a fresh master key, re-enter the SSH keys (or generate new ones and deposit them on each server through emergency access), re-enter secret variables, S3/registry/cloud credentials, webhook secrets, SMTP config. It is long, and it is exactly what the separate backup of `master.key` prevents.

## Verification

- [ ] `GET /health` 200, login + 2FA OK, `GET /version` = expected tag;
- [ ] all servers `ready` after validation; observed statuses fresh (recent `observed_at`);
- [ ] an end-to-end test deployment succeeds;
- [ ] a test webhook (push) arrives with `signature_valid = true`;
- [ ] instance backup re-run ("Backup Now") → `succeeded` with verified S3 upload;
- [ ] time it: the product objective is **RTO ≤ 2 h** (§16.4).

## Prevention

- The 3 pieces (S3 dump + `master.key` + compose/.env) stored **off the machine and in two distinct locations** (key kept separate from the dumps, §23.1).
- Full restore drill (this procedure, on a throwaway VM) at least once, timed — it is the only way to guarantee the RTO.
- Instance FQDN with a short DNS TTL (≤ 300 s) to speed up step 5.
- Never host a critical workload on the instance's `localhost` server (§3.1: discouraged in production) — losing the machine would take out the control plane **and** the workloads.
