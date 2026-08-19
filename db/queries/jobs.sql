-- Durable job queue (ADR-002, §21.3). Dequeue uses FOR UPDATE SKIP LOCKED
-- on the partial index of eligible jobs; lock_key exclusivity is enforced
-- both here (NOT EXISTS) and by the partial unique index as the net.

-- name: EnqueueJob :one
INSERT INTO jobs (uuid, queue, job_type, payload, priority, run_at, max_attempts, idempotency_key, lock_key, team_id, resource_id, correlation_id, retry_of_id, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
RETURNING *;

-- name: GetJobByIdempotencyKey :one
SELECT * FROM jobs WHERE idempotency_key = $1;

-- name: DequeueJob :one
WITH next AS (
    SELECT j.id FROM jobs j
    WHERE j.status = 'queued'
      AND j.queue = ANY(sqlc.arg(queues)::text[])
      AND j.run_at <= now()
      AND (j.lock_key IS NULL OR NOT EXISTS (
          SELECT 1 FROM jobs h
          WHERE h.lock_key = j.lock_key AND h.status IN ('leased', 'running')))
    ORDER BY j.priority DESC, j.run_at, j.id
    LIMIT 1
    FOR UPDATE OF j SKIP LOCKED
)
UPDATE jobs SET
    status = 'leased',
    leased_by = sqlc.arg(worker_id),
    lease_expires_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::int),
    heartbeat_at = now(),
    attempt = attempt + 1,
    updated_at = now()
FROM next
WHERE jobs.id = next.id
RETURNING jobs.*;

-- name: MarkJobRunning :execrows
UPDATE jobs SET status = 'running', updated_at = now()
WHERE id = $1 AND leased_by = $2 AND status = 'leased';

-- name: HeartbeatJob :execrows
UPDATE jobs SET heartbeat_at = now(),
    lease_expires_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::int),
    updated_at = now()
WHERE id = $1 AND leased_by = $2 AND status IN ('leased', 'running');

-- name: SucceedJob :execrows
UPDATE jobs SET status = 'succeeded', result = $2, finished_at = now(),
    leased_by = NULL, lease_expires_at = NULL, updated_at = now()
WHERE id = $1 AND leased_by = sqlc.arg(worker_id) AND status IN ('leased', 'running');

-- name: FailJob :execrows
UPDATE jobs SET
    status = CASE WHEN sqlc.arg(to_dead_letter)::boolean THEN 'dead_letter'::job_status ELSE 'retry_wait'::job_status END,
    last_error = $2,
    run_at = sqlc.arg(next_run_at),
    dead_lettered_at = CASE WHEN sqlc.arg(to_dead_letter)::boolean THEN now() ELSE NULL END,
    finished_at = CASE WHEN sqlc.arg(to_dead_letter)::boolean THEN now() ELSE NULL END,
    leased_by = NULL, lease_expires_at = NULL, updated_at = now()
WHERE id = $1 AND leased_by = sqlc.arg(worker_id) AND status IN ('leased', 'running');

-- name: UpdateJobSteps :exec
UPDATE jobs SET steps = $2, updated_at = now() WHERE id = $1;

-- Reaper (INV-013): expired leases go back to the queue via retry_wait, or
-- to dead_letter when attempts are exhausted. Handlers must inspect the
-- remote effect before redoing work (§21.3).
-- name: ReapExpiredLeases :many
-- A crashed worker is not a failed job (§2.5): the attempt is GIVEN BACK
-- (attempt - 1) so a deployment with max_attempts = 1 can still be resumed —
-- the resume inspects the remote state first, it never replays blindly.
-- resume_count bounds it: a job that kills its worker every time is dead-
-- lettered instead of looping forever.
UPDATE jobs SET
    status = CASE WHEN resume_count >= @max_resumes::int THEN 'dead_letter'::job_status ELSE 'retry_wait'::job_status END,
    dead_lettered_at = CASE WHEN resume_count >= @max_resumes::int THEN now() ELSE dead_lettered_at END,
    finished_at = CASE WHEN resume_count >= @max_resumes::int THEN now() ELSE finished_at END,
    resume_count = resume_count + 1,
    attempt = CASE WHEN resume_count >= @max_resumes::int THEN attempt ELSE greatest(attempt - 1, 0) END,
    last_error = coalesce(last_error, 'lease expired (worker crash or hang)'),
    run_at = now(), leased_by = NULL, lease_expires_at = NULL, updated_at = now()
WHERE status IN ('leased', 'running') AND lease_expires_at < now()
RETURNING id, uuid, status;

-- name: PromoteWaitingJobs :execrows
UPDATE jobs SET status = 'queued', updated_at = now()
WHERE status IN ('retry_wait', 'scheduled') AND run_at <= now();

-- name: GetJobByUUIDForTeam :one
SELECT * FROM jobs WHERE uuid = $1 AND team_id = $2;

-- name: ListJobsPage :many
SELECT * FROM jobs
WHERE team_id = sqlc.arg(team_id)
  AND (sqlc.narg(status_filter)::job_status IS NULL OR status = sqlc.narg(status_filter))
  AND (sqlc.narg(queue_filter)::text IS NULL OR queue = sqlc.narg(queue_filter))
  AND (sqlc.narg(type_filter)::text IS NULL OR job_type = sqlc.narg(type_filter))
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: GetJobUUIDByID :one
SELECT uuid FROM jobs WHERE id = $1;

-- name: ForgetDeadLetterJob :execrows
UPDATE jobs SET status = 'cancelled', finished_at = now(), updated_at = now()
WHERE id = $1 AND status = 'dead_letter';

-- name: CountActiveJobsByLockKey :one
SELECT count(*) FROM jobs
WHERE lock_key = $1 AND status IN ('scheduled', 'queued', 'leased', 'running', 'retry_wait');

-- name: GetActiveJobByLockKey :one
-- The queued-or-running job of one lock key (ADR-080 UX): what the model
-- page shows, what the double-enqueue guard names in its 409.
SELECT uuid, status, job_type, cancel_requested_at FROM jobs
WHERE lock_key = $1 AND status IN ('scheduled', 'queued', 'leased', 'running', 'retry_wait')
ORDER BY id DESC LIMIT 1;

-- name: CancelQueuedJob :execrows
-- The enqueue you regret: a job that has NOT started stops here and now.
-- One that already runs takes the cooperative path below when its family
-- has a checkpoint — killing it mid-mutation would leave the server in a
-- state nobody asked for. Zero rows = not cancellable this way, the caller
-- tries the cooperative request before answering 409.
UPDATE jobs SET status = 'cancelled', finished_at = now(), updated_at = now()
WHERE id = $1 AND status IN ('scheduled', 'queued', 'retry_wait');

-- name: RequestJobCancel :execrows
-- Cooperative cancellation of a job already in flight, by id. Only the
-- families that actually poll the flag are eligible — a job type absent
-- from this list would take the flag and ignore it, which reads to the
-- operator as a cancel that did nothing. Setting it twice is not an error,
-- but zero rows must mean "not cancellable", so an already-flagged job
-- still counts as a row.
UPDATE jobs SET cancel_requested_at = coalesce(cancel_requested_at, now()), updated_at = now()
WHERE id = $1
  AND status IN ('leased', 'running')
  AND job_type IN ('deployment.run', 'model.provision', 'model.start', 'model.stop', 'model.restart', 'model.delete');

-- Cooperative cancellation (§2.6): the worker checks the flag at each
-- checkpoint between steps, before the switching barrier (§21.1).
-- name: RequestDeploymentJobCancel :execrows
UPDATE jobs SET cancel_requested_at = now(), updated_at = now()
WHERE job_type = 'deployment.run'
  AND (payload->>'deployment_id')::bigint = sqlc.arg(deployment_id)::bigint
  AND status IN ('scheduled', 'queued', 'leased', 'running');

-- name: IsJobCancelRequested :one
SELECT (cancel_requested_at IS NOT NULL)::boolean AS cancelled FROM jobs WHERE id = $1;

-- Retention (§19.2, §22.2): terminal jobs are purged; dead_letter rows are
-- kept until an operator retries or forgets them.
-- name: PurgeTerminalJobs :execrows
DELETE FROM jobs
WHERE status IN ('succeeded', 'cancelled')
  AND finished_at < now() - make_interval(days => sqlc.arg(retention_days)::int);
