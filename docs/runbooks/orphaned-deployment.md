# Runbook — Recovering an orphaned deployment

> References: deployment-engine spec §2.3 (leases), §2.5 (resume by inspection), §4 (state machine, "crash during state" column), §7.2 (switchover), §9 (compensations C1/C2/C3); PRD §21.1, INV-004/005/006/013; data dictionary §10.1–10.2 (`deployments`, `deployment_steps`), §11.8 (`jobs`).

## Symptoms

- A deployment stays in a non-terminal state (`preparing` … `finishing`) without progress; the UI timeline is frozen.
- A `<app_uuid>-next` container lingers on the server while no deployment is active.
- Subsequent deployments of the application stay in `queued` (§3.1 lock not released).
- Dead/restarted worker: `leased_by` points to a worker that no longer exists, `lease_expires_at` in the past.

## Impact

- **The application in production is not supposed to be affected**: the old container stays routed until the switchover has happened (INV-005), and a failure never deletes the last healthy container nor the volumes (INV-006).
- New deployments of the same application/destination are blocked behind the lock.
- Sensitive case: orphan in `switching` state — the switchover may have happened, partially or not at all.

## Diagnosis

### 1. Read the write-ahead state (the database says "what may have started", spec §4)

```sql
-- Non-terminal deployments and their job:
SELECT d.uuid, d.status, d.attempt, d.updated_at, d.commit_sha, d.image_digest,
       j.uuid AS job_uuid, j.status AS job_status, j.leased_by, j.heartbeat_at, j.lease_expires_at
FROM deployments d
LEFT JOIN jobs j ON j.payload->>'deployment_uuid' = d.uuid::text
WHERE d.status NOT IN ('succeeded','failed','cancelled','superseded')
ORDER BY d.updated_at;

-- Last committed checkpoint (steps):
SELECT seq, name, status, exit_code, started_at, finished_at
FROM deployment_steps WHERE deployment_id = (SELECT id FROM deployments WHERE uuid = '<uuid>')
ORDER BY seq;
```

**Normal case**: `lease_expires_at < now()` → the lease scan (every 30 s) puts the job back to `queued` with `recovered = true`, and the worker that takes it over applies the inspection + resume rules itself (spec §2.5, §4). **Let it run.** Only intervene manually if: the job is in `dead_letter`, the worker fleet is down, or automatic recovery is looping.

### 2. Remote inspection (never decide without it — INV-004, §22.1)

On the target server (`<app>` = application UUID, `<sha12>` = first 12 characters of the SHA):

```sh
# Was the deployment's image produced?
docker image inspect AkerDock/<app>:<sha12> \
  --format '{{index .Config.Labels "akerdock.deployment_uuid"}}' 2>/dev/null

# Candidate and current container:
docker container inspect <app>-next --format '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}} {{index .Config.Labels "akerdock.deployment_uuid"}}' 2>/dev/null
docker container inspect <app>      --format '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}} {{index .Config.Labels "akerdock.deployment_uuid"}}' 2>/dev/null

# Who does the proxy point to? (file = source of truth for routing, spec §7.1)
grep -A2 'url:' /var/lib/akerdock/proxy/dynamic/<app>.yaml
sha256sum /var/lib/akerdock/proxy/dynamic/<app>.yaml     # compare against the checksum recorded in the database

# Leftover clone?
ls -d /var/lib/akerdock/applications/<app>/source/<deployment_uuid> 2>/dev/null
```

## Step-by-step resolution

Apply the resume rule for the state in which the deployment froze (spec §4 table). Operator summary:

| Frozen state | Decision |
|---|---|
| `preparing`, `cloning` | Resume (idempotent) or compensate **C1**: `rm -rf source/<deployment_uuid>`; nothing else was touched |
| `building` | Image present **with** the right `akerdock.deployment_uuid` label → the step had finished, resume; absent/partial → rebuild or C1 |
| `pushing` | Replayable (idempotent push); re-resolve the digest |
| `starting`, `healthchecking` | Candidate `unhealthy`/`exited`/absent → compensation **C2**; candidate `healthy` and fresh → resume possible |
| `switching` | **Critical case**, see below |
| `finishing` | Everything is idempotent → replay; at worst degraded `succeeded` |

### `switching` case (spec §4, rule a/b/c — never a second switchover without inspection, INV-005)

- **(a)** The proxy file still points to the **old** container: the switchover did not happen. Candidate still `healthy` → let automatic recovery replay the switchover; candidate dead → compensation **C2** (below), deployment `failed`.
- **(b)** The file points to the **candidate** (by IP), the old container still exists: the switchover happened → finish the sequence: `docker stop -t <grace> <app> && docker rm <app>`, then `docker rename <app>-next <app>`, then stabilization (file regenerated to `url: http://<app>:<port>` — resume in `finishing`).
- **(c)** The old one is absent, the rename not done: resume at the rename (`docker rename <app>-next <app>`) then `finishing`.

⚠️ **Point of no return**: as soon as the verified proxy file points to the candidate (cases b/c), **never implicitly "un-switch"** (compensation C3: the new one stays in production; going back is an explicit rollback).

### Cleaning up the candidate without touching the old one (compensation C2, INV-005/006)

Only if the decision is "compensate" (case a with a dead candidate, or an abandoned `-next` candidate of a deployment already `failed`):

```sh
# 1. Capture the candidate's logs BEFORE deletion (diagnostics):
docker logs --tail 200 <app>-next > /tmp/<deployment_uuid>-next.log 2>&1
# 2. If the proxy file was modified without a conclusive check: re-point it to the old one
#    (content of the last 'applied' revision — see proxy-outage.md — written to .tmp then mv -f)
# 3. Delete the candidate ONLY:
docker stop -t 10 <app>-next && docker rm <app>-next
# 4. Purge the deployment's clone:
rm -rf /var/lib/akerdock/applications/<app>/source/<deployment_uuid>
```

⚠️ Absolute prohibitions during compensation: touching the `<app>` container (the old one), its **volumes**, or the **images** carrying `akerdock.retain=true` (INV-006, spec §9.1).

### Closing out in the database (last resort, if automatic recovery is impossible)

**(future CLI candidate — `AkerDock deployment resolve <uuid> --failed`)**. Bypasses the audit trail: record manually.

```sql
BEGIN;
UPDATE deployments SET status = 'failed', finished_at = now(), updated_at = now()
WHERE uuid = '<deployment_uuid>'
  AND status NOT IN ('succeeded','failed','cancelled','superseded');
-- Terminate the associated job if it is not already terminal:
UPDATE jobs SET status = 'cancelled', finished_at = now(), updated_at = now()
WHERE payload->>'deployment_uuid' = '<deployment_uuid>'
  AND status IN ('queued','leased','running','retry_wait');
-- Release the application lock (semantic table resource_locks, spec §3.1):
DELETE FROM resource_locks
WHERE application_uuid = '<app_uuid>' AND destination_uuid = '<destination_uuid>';
COMMIT;
```

Only close out as `failed` **after** the remote compensation (otherwise a ghost `-next` remains on the server). To relaunch cleanly: manual retry from the UI (`failed → retrying → preparing`, same snapshot and same SHA, spec §4) or a new `POST /applications/{uuid}/deploy`.

## Verification

- The old version is still serving: `curl -fsS https://<fqdn>/<health_path>` through the proxy.
- No more `<app>-next` container on the server; no more `source/<deployment_uuid>` clone.
- The proxy file's checksum matches the last `applied` revision in the database.
- Lock released: a new deployment of the application leaves `queued` and proceeds normally.
- The expected rollback images are intact (`docker image ls --filter label=akerdock.retain=true`).

## Prevention

- Recovery is **designed to be automatic** (90 s leases, 20 s heartbeat, 30 s scan, resume by inspection): most "orphans" resolve themselves in < 2 min. Instrument the `expired leases` and `retries` metrics (spec §12.3) and alert on their growth rather than intervening case by case.
- In multi-instance, size the workers so that a single worker does not carry all the long jobs.
- Never kill a worker during `switching` if avoidable (graceful shutdown of instances during upgrades — [upgrade-downgrade.md](upgrade-downgrade.md)).
