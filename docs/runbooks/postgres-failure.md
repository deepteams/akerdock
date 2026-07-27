# Runbook — Internal PostgreSQL database failure

> References: PRD §7.5, §16.4 (RPO ≤ 24 h, RTO ≤ 2 h), §22.1, INV-007, INV-013; deployment-engine spec §2.3–2.5 (leases, resume by inspection); data dictionary §9.5–9.6 (`database_backup_plans`, `backup_executions`), §11.8 (`jobs`).

## Symptoms

- UI/API returning 5xx errors; `GET /api/v1/health` fails or reports the database unreachable.
- `docker compose ps`: `postgres` service `Exited`/`Restarting`, or `AkerDock` crash-looping with connection errors in the logs.
- Notifications stopped, deployments frozen, webhooks unprocessed.
- **Deployed applications keep serving traffic**: the control plane is not in the request path (INV-007). If the apps are also down, this is not (only) this runbook.

## Impact

- No control action possible (deploy, rollback, terminal) while the database is down.
- `leased`/`running` jobs are interrupted; they will be resumed automatically (INV-013).
- In case of restore: **loss of everything after the backup** (RPO ≤ 24 h with a daily backup, §16.4).

## Diagnosis

```sh
cd /var/lib/akerdock
docker compose ps
docker compose logs --tail 200 postgres
docker compose exec postgres pg_isready -U AkerDock || echo "PG DOWN"
df -h /var/lib/akerdock                          # full disk = cause #1
docker compose exec postgres psql -U AkerDock AkerDock -c "SELECT 1;"   # if PG responds
```

Classify:

1. **Transient** (OOM kill, full disk, machine restart) → resolution A.
2. **Corruption / data loss** (`invalid page`, `could not read block`, lost volume) → resolution B (restore).

Locate the last usable backup — if the database still responds partially:

```sql
SELECT be.uuid, be.status, be.filename, be.size_bytes, be.checksum_sha256,
       be.uploaded_to_s3, be.finished_at
FROM backup_executions be
JOIN database_backup_plans p ON p.id = be.backup_plan_id
WHERE p.is_instance_backup AND be.status IN ('succeeded','partial')
ORDER BY be.finished_at DESC LIMIT 5;
```

Otherwise: local files `/var/lib/akerdock/backups/…` (§7.2) and/or the plan's S3 bucket (`aws s3 ls s3://<bucket>/<prefix>/ --endpoint-url <endpoint>`).

## Step-by-step resolution

### A. Transient failure

1. Clear the cause (disk: purge old `backups/`, Docker logs `docker system df`; RAM: check the OOM killer `dmesg | grep -i oom`).
2. `docker compose up -d` then watch `docker compose logs -f postgres` (automatic WAL recovery on startup).
3. Go directly to **Verification** — no restore if the recovery succeeds.

### B. Restore from backup

1. **Stop the control plane**, keep the corrupted state:
   ```sh
   docker compose stop AkerDock
   docker compose stop postgres
   mv postgres postgres.corrupted-$(date -u +%Y%m%d)    # do NOT delete (forensics/partial recovery)
   mkdir postgres
   ```
2. Retrieve the dump (local or S3) and **verify its integrity**:
   ```sh
   sha256sum backups/akerdock-instance-….dump    # compare against backup_executions.checksum_sha256 if known
   ```
3. Restart PostgreSQL alone, restore:
   ```sh
   docker compose up -d postgres
   # wait for pg_isready, then:
   docker compose exec -T postgres pg_restore -U AkerDock -d AkerDock --no-owner --exit-on-error \
     < backups/akerdock-instance-….dump
   ```
   ⚠️ **Point of no return**: restore into an **empty** database only. A restore into a non-empty database requires the reinforced confirmation defined in §20.5 — when operating manually, simply do not do it.
4. Verify that the **current master key** matches the restored data: if a rotation happened **after** the backup, the `master.key` file must still contain the old key version (see [key-rotation.md](key-rotation.md) — this is why a version is never deleted too early).
5. Restart the control plane: `docker compose up -d AkerDock`.

### C. Job recovery after restore (INV-013, spec §2.3/§2.5)

Nothing to force: the **expired-lease scan** (every 30 s) puts `leased`/`running` jobs whose lease is dead back to `queued`, with the `recovered = true` marker. Each recovered deployment job starts with a **remote inspection** (image labeled `akerdock.deployment_uuid`, `<uuid>-next`/`<uuid>` containers, proxy file checksum) before resuming, compensating or finishing — **never replay manually blind** (§22.1).

Monitor the recovery:

```sql
SELECT queue, status, count(*) FROM jobs GROUP BY 1,2 ORDER BY 1,2;
SELECT uuid, status, updated_at FROM deployments
WHERE status NOT IN ('succeeded','failed','cancelled','superseded')
ORDER BY updated_at;
```

Deployments not resumed cleanly end up in `failed`/`dead_letter` → [queue-dead-letter.md](queue-dead-letter.md) and [orphaned-deployment.md](orphaned-deployment.md).

### D. Control plane ↔ servers reconciliation (RPO window)

Everything that happened **between the backup and the failure** is absent from the database. Consequences to handle:

1. **Stale observed statuses**: reconciliation converges on its own; the UI shows "unknown/stale" rather than a false `running` (§19.2) — wait one cycle before concluding.
2. **Deployments that succeeded after the backup**: the database believes an older version is running. Inventory reality on each server:
   ```sh
   ssh <user>@<server> "docker ps --filter label=akerdock.managed=true \
     --format '{{.Names}}\t{{.Label \"akerdock.deployment_uuid\"}}\t{{.Label \"akerdock.commit_sha\"}}'"
   ```
   Cross-check against `deployments.uuid` in the database; an unknown `deployment_uuid` = a deployment lost in the RPO window. **Do not "fix" it by stopping the container**: re-trigger a normal deployment of the resource to realign database and reality (the current snapshot/SHA takes over).
3. **Lost webhook deliveries**: the `(provider, delivery_id)` deduplication lost its memory; pushes that occurred during the outage triggered nothing. Manually redeploy the auto-deploy applications whose repo moved (`POST /applications/{uuid}/deploy`).
4. **Objects created after the backup** (tokens, keys, resources): to be re-created; re-created API tokens change value.

## Verification

- `curl -fsS http://localhost:8080/api/v1/health` → 200; UI login OK.
- Queue alive: recent jobs `succeeded`; no more `leased` with `lease_expires_at < now()`.
- An end-to-end test deployment succeeds.
- Dashboard: servers `ready`, observed statuses refreshed (recent `observed_at`), no unexplained "priority actions" alert.
- Immediately relaunch a "Backup Now" of the instance plan and verify `succeeded`.

## Prevention

- Instance backup plan **at least daily** with an **S3** destination and upload verification (§7.2, §7.5); the `partial` status (local success, S3 failure) is an alert to handle, not a success (§20.5).
- Automatic **restore drills** (§20.5, §22.3): a backup that has never been restored is not reliable.
- Disk alert on the instance host (cause #1 of PG failure); size `/var/lib/akerdock`.
- Local backup retention ≠ 0 even with S3 (faster restore); documented RTO ≤ 2 h (§16.4) — time the drill.
