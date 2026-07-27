# Runbook — Dead-lettered jobs

> References: PRD §21.3 (job state machine), §22.1 (error classification), §24.3 (queues/priorities); ADR-002 (PostgreSQL queue); deployment-engine spec §2.4 (retry, backoff, dead-letter: "Replay from the dead-letter = an audited manual action that creates a **new linked attempt**"); data dictionary §11.8 (`jobs`); OpenAPI (`GET /jobs`, `GET /jobs/{uuid}`, `POST /jobs/{uuid}/retry`, `POST /jobs/{uuid}/forget`).

## Symptoms

- "Priority actions" entries on the dashboard; repeated `deployment.failed` notifications (or backup/cleanup failed).
- Jobs in `dead_letter` status (kept until a retry/forget intervention, data dictionary §11.8).
- Indirect symptom: an operation (deployment, backup, deletion) that "never happens anymore" — its job died silently if notifications are misconfigured.

## Impact

- A dead-lettered job will **never** be replayed automatically: the operation it carried is waiting for a human decision.
- Special case `resource.delete`: a dead-lettered deletion job leaves a **reconcilable tombstone** with the list of remote remnants (§20.6.4) — "forget" without cleanup leaves orphaned objects on the server.

## Diagnosis

### 1. Inventory

```sh
curl -sS "$AKD/jobs?status=dead_letter" -H "Authorization: Bearer $TOKEN"
# paginated list (cursor); extra filters: &queue=deploy, &type=resource.delete
```

Equivalent SQL (fallback, cross-team view):

```sql
SELECT uuid, queue, job_type, attempt, max_attempts, priority,
       left(last_error, 120) AS last_error, correlation_id, dead_lettered_at
FROM jobs WHERE status = 'dead_letter'
ORDER BY dead_lettered_at DESC;
```

### 2. Recurring causes (aggregation)

```sql
SELECT job_type, left(last_error, 80) AS err, count(*) AS n,
       min(dead_lettered_at) AS first_seen, max(dead_lettered_at) AS last_seen
FROM jobs WHERE status = 'dead_letter'
GROUP BY 1, 2 ORDER BY n DESC;
```

Reading per the classification (§22.1, spec §2.4):

- **Transient errors** (SSH unreachable, network timeout, registry 5xx, disk full) that reached the dead-letter = the outage lasted longer than the backoff window (3 attempts, exponential backoff 30 s → 15 min for `deployment.run`) → look for the underlying infrastructure incident (server `unreachable`? registry down?) **before** any replay, otherwise the replay will die again.
- **Deterministic errors** (failed build, healthcheck never healthy, invalid config): for `deployment.run` they normally do **not** go through the dead-letter (direct failure without auto retry, spec §2.4) — seeing any in dead-letter suggests a misclassification (bug to report). For the other types (backup, cleanup, delete: `max_attempts` default 5), it is the sign of a cause to fix before replaying.

### 3. Context of a specific job

```sql
-- Full chain via correlation (job → events → audit):
SELECT event_type, occurred_at, payload FROM outbox_events
WHERE correlation_id = '<correlation_id>' ORDER BY id;
SELECT occurred_at, action, result, actor_display FROM audit_events
WHERE correlation_id = '<correlation_id>' ORDER BY occurred_at;
-- If it is a deployment: its steps and logs
SELECT seq, name, status, exit_code FROM deployment_steps
WHERE deployment_id = (SELECT id FROM deployments WHERE uuid = (
  SELECT payload->>'deployment_uuid' FROM jobs WHERE uuid = '<job_uuid>')::uuid)
ORDER BY seq;
```

Generic API tracking of a job: `GET /jobs/{job_uuid}`.

## Step-by-step resolution

### A. Retry (replay)

Absolute rule (spec §2.4): a replay is an **audited manual action that creates a new linked attempt**. ⚠️ **Never** put the `dead_letter` row back to `queued` via UPDATE: that rewrites history (`attempt`, attempt linkage) and bypasses the audit trail.

1. Fix the cause first (server reachable again, disk freed, credentials repaired, config fixed).
2. Replay through the business channel, which creates the linked attempt:
   - Deployment: the UI retry button (`failed → retrying → preparing` transition, same snapshot/SHA) or a new `POST /applications/{uuid}/deploy` (new snapshot) depending on whether the config had to change or not.
   - Backup: `POST /databases/{db}/backups/{plan}/execute`.
   - Server validation: `POST /servers/{uuid}/validate`.
   - Resource deletion: relaunch the deletion from the UI (tombstone retry, §20.6.4).
   - Other types without a dedicated business endpoint: generic retry
     ```sh
     curl -sS -X POST "$AKD/jobs/$JOB_UUID/retry" -H "Authorization: Bearer $TOKEN"
     # 202: new job with retry_of_uuid → original job (linked attempt, spec §2.4);
     # 409 invalid_state if the job is not (or no longer) in dead_letter
     ```
     or the "priority actions" UI.
3. After a successful replay, close the dead-letter entry (see B) if the product does not do it automatically.

### B. Forget (abandon)

Reserve for jobs whose operation no longer makes sense (resource deleted in the meantime, duplicate, decision to abandon).

⚠️ Before forgetting a **`resource.delete`** job: check the tombstone's list of remote remnants (§20.6.4, `resources.remnants` column) and clean up manually on the server, otherwise orphaned containers/volumes:

```sql
SELECT uuid, name, remnants FROM resources
WHERE deleted_at IS NOT NULL AND remnants IS NOT NULL;
```

Via the API (audited, the job moves to `cancelled`) or the UI ("forget"):

```sh
curl -sS -X POST "$AKD/jobs/$JOB_UUID/forget" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"acknowledge_remnants": true}'
# acknowledge_remnants=true REQUIRED if the job leaves remote remnants,
# otherwise 409 remnants_present (with the list of remnants in details)
```

SQL last resort (bypasses the audit trail and the remnants check — record manually):

```sql
UPDATE jobs SET status = 'cancelled', finished_at = now(), updated_at = now()
WHERE uuid = '<job_uuid>' AND status = 'dead_letter';
```

### C. Handling a wave (same cause, N jobs)

1. Fix the common cause (e.g. server reachable again).
2. Replay **one** representative job; verify its success.
3. Replay the rest in batches through the business channel (loop over `POST /applications/{uuid}/deploy`, `POST /jobs/{uuid}/retry`, etc.). Beware of the caps: `concurrent_builds` (2/server) and `deployment_queue_limit` (25/server, `429 deployment_queue_full`) — spread the replays out.

## Verification

- No more unexplained `dead_letter`: the inventory query only shows jobs currently under decision-making.
- Replays appear as **new linked attempts** (deployments: `retry_of_id` set, `attempt` incremented; jobs: `retry_of_uuid` in `GET /jobs/{uuid}`) and in `audit_events`.
- The root cause is fixed: no new dead-letters with the same `(job_type, last_error)` since the fix (aggregation query, `last_seen` column).

## Prevention

- Alert on the **dead-letters metric** (spec §12.3, OTLP) and on `deployment.failed` events (§11) — a silent dead-letter is a deferred incident.
- Tune per-application timeouts when they are the recurring cause (clone 600 s, build 3600 s, pull/push 900 s — spec §11) rather than replaying in a loop.
- Provider outages must not saturate the workers: the circuit breaker (§22.1) exists for that — a wave of dead-letters on the same provider must trip it, otherwise it is a bug.
- Purge: finished jobs are purged by retention, `dead_letter` ones are **kept until intervention** (data dictionary §11.8) — process the queue regularly so it remains a signal.
