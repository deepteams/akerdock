-- Backups (§7, ADR-014).

-- name: CreateBackupPlan :one
INSERT INTO database_backup_plans (uuid, database_id, cron_expression, timezone, enabled, dump_all, included_databases, timeout_seconds, s3_storage_id, s3_only, save_local, retention_local_max_count, retention_local_max_days, retention_s3_max_count, retention_s3_max_days, drill_enabled, drill_interval_days)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
RETURNING *;

-- name: GetBackupPlanByUUID :one
SELECT p.* FROM database_backup_plans p
WHERE p.uuid = $1 AND p.database_id = $2 AND p.deleted_at IS NULL;

-- name: GetBackupPlanByID :one
SELECT * FROM database_backup_plans WHERE id = $1 AND deleted_at IS NULL;

-- name: ListBackupPlansForDatabase :many
SELECT * FROM database_backup_plans
WHERE database_id = $1 AND deleted_at IS NULL
ORDER BY id DESC;

-- name: ListSchedulableBackupPlans :many
-- Enabled plans of non-deleted databases. The scheduler owns the cron: it
-- seeds next_run_at when it is NULL and fires the plans that are due.
SELECT p.*, r.team_id FROM database_backup_plans p
JOIN resources r ON r.id = p.database_id
WHERE p.enabled AND p.deleted_at IS NULL AND r.deleted_at IS NULL
ORDER BY p.id;

-- name: SetBackupPlanSchedule :exec
-- Advances the plan's cron window. Written by the scheduler only, so it does
-- not bump `version` (it is not a user-visible edit and must not conflict
-- with an optimistic-locking PATCH).
UPDATE database_backup_plans
SET next_run_at = sqlc.arg(next_run_at), last_run_at = coalesce(sqlc.narg(last_run_at), last_run_at)
WHERE id = $1;

-- name: UpdateBackupPlan :execrows
UPDATE database_backup_plans
SET cron_expression = $2, timezone = $3, enabled = $4, dump_all = $5,
    s3_storage_id = $6, s3_only = $7, save_local = $8,
    retention_local_max_count = $9, retention_local_max_days = $10,
    retention_s3_max_count = sqlc.arg(retention_s3_max_count),
    retention_s3_max_days = sqlc.arg(retention_s3_max_days),
    drill_enabled = sqlc.arg(drill_enabled),
    drill_interval_days = sqlc.arg(drill_interval_days),
    -- The schedule changed: the scheduler recomputes the next occurrence.
    next_run_at = NULL,
    updated_at = now(), version = version + 1
WHERE id = $1 AND version = sqlc.arg(expected_version) AND deleted_at IS NULL;

-- name: SoftDeleteBackupPlan :execrows
UPDATE database_backup_plans SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateBackupExecution :one
INSERT INTO backup_executions (uuid, backup_plan_id, status, started_at)
VALUES ($1, $2, 'running', now())
RETURNING *;

-- name: FinishBackupExecution :exec
UPDATE backup_executions
SET status = $2, filename = $3, size_bytes = $4, checksum_sha256 = $5,
    engine_version = $6, uploaded_to_s3 = $7, s3_upload_error = $8,
    error_message = $9, s3_key = sqlc.narg(s3_key), finished_at = now()
WHERE id = $1;


-- name: GetBackupExecutionByUUID :one
SELECT e.* FROM backup_executions e
WHERE e.uuid = $1 AND e.backup_plan_id = $2;

-- name: ListBackupExecutionsPage :many
SELECT * FROM backup_executions
WHERE backup_plan_id = sqlc.arg(plan_id)
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListExpiredLocalBackups :many
-- Retention (§7.2): the count and age rules are cumulative — a backup expires
-- if it falls outside EITHER — and 0 means unlimited. The last successful
-- backup of a plan is never dropped, whatever the rules say.
WITH ranked AS (
    -- The rank counts every live backup, newest first, so `max_count = 1`
    -- keeps exactly one. The last successful backup is excluded below, not
    -- here: it must not consume a retention slot, it must survive regardless.
    SELECT e.*, row_number() OVER (ORDER BY e.id DESC) AS rank
    FROM backup_executions e
    WHERE e.backup_plan_id = $1 AND e.status IN ('succeeded', 'partial')
      AND e.local_deleted_at IS NULL AND e.filename IS NOT NULL
)
SELECT id, uuid, backup_plan_id, job_id, status, filename, size_bytes, checksum_sha256,
       engine_version, uploaded_to_s3, s3_upload_error, s3_key, local_deleted_at,
       s3_deleted_at, error_message, started_at, finished_at, created_at
FROM ranked
WHERE id < (SELECT max(b.id) FROM backup_executions b WHERE b.backup_plan_id = $1 AND b.status = 'succeeded')
  AND ((sqlc.arg(keep_count)::int > 0 AND rank > sqlc.arg(keep_count)::int)
       OR (sqlc.arg(max_days)::int > 0 AND created_at < now() - make_interval(days => sqlc.arg(max_days)::int)));

-- name: ListExpiredS3Backups :many
-- Same rules, applied to the objects in the bucket. A backup can outlive its
-- local copy in S3, or the reverse — the two retentions are independent.
WITH ranked AS (
    SELECT e.*, row_number() OVER (ORDER BY e.id DESC) AS rank
    FROM backup_executions e
    WHERE e.backup_plan_id = $1 AND e.uploaded_to_s3
      AND e.s3_deleted_at IS NULL AND e.s3_key IS NOT NULL
)
SELECT id, uuid, backup_plan_id, job_id, status, filename, size_bytes, checksum_sha256,
       engine_version, uploaded_to_s3, s3_upload_error, s3_key, local_deleted_at,
       s3_deleted_at, error_message, started_at, finished_at, created_at
FROM ranked
WHERE id < (SELECT max(b.id) FROM backup_executions b WHERE b.backup_plan_id = $1 AND b.status = 'succeeded')
  AND ((sqlc.arg(keep_count)::int > 0 AND rank > sqlc.arg(keep_count)::int)
       OR (sqlc.arg(max_days)::int > 0 AND created_at < now() - make_interval(days => sqlc.arg(max_days)::int)));

-- name: MarkBackupS3Deleted :exec
UPDATE backup_executions SET s3_deleted_at = now() WHERE id = $1;

-- name: MarkBackupLocalDeleted :exec
UPDATE backup_executions SET local_deleted_at = now() WHERE id = $1;

-- name: GetBackupExecutionByID :one
SELECT * FROM backup_executions WHERE id = $1;

-- Restore drills (ADR-014). A backup that has never been restored is a file,
-- not a backup.

-- name: SetBackupExecutionTableCount :exec
UPDATE backup_executions SET table_count = $2 WHERE id = $1;

-- name: GetLatestSuccessfulBackupExecution :one
SELECT * FROM backup_executions
WHERE backup_plan_id = $1 AND status IN ('succeeded', 'partial') AND filename IS NOT NULL
ORDER BY id DESC
LIMIT 1;

-- name: ListDrillablePlans :many
-- Enabled plans whose drill window has elapsed. A plan that has never been
-- drilled (last_drill_at IS NULL) is due immediately: the first drill is the
-- one that tells you whether the backups were ever any good.
SELECT sqlc.embed(p), d.team_id
FROM database_backup_plans p
JOIN databases db ON db.id = p.database_id
JOIN resources d ON d.id = db.id
WHERE p.enabled AND p.drill_enabled AND p.deleted_at IS NULL AND d.deleted_at IS NULL
  AND (p.last_drill_at IS NULL OR p.last_drill_at < now() - make_interval(days => p.drill_interval_days));

-- name: CreateRestoreDrill :one
INSERT INTO restore_drills (plan_id, execution_id, tables_expected)
VALUES ($1, $2, $3)
RETURNING *;

-- name: FinishRestoreDrill :exec
UPDATE restore_drills
SET status = $2, tables_restored = $3, error_message = $4,
    finished_at = now(),
    duration_ms = (EXTRACT(EPOCH FROM (now() - started_at)) * 1000)::int
WHERE id = $1;

-- name: SetPlanDrillResult :exec
UPDATE database_backup_plans
SET last_drill_at = now(), last_drill_status = $2
WHERE id = $1;

-- name: ListRestoreDrillsPage :many
SELECT sqlc.embed(d), e.uuid AS execution_uuid
FROM restore_drills d
LEFT JOIN backup_executions e ON e.id = d.execution_id
WHERE d.plan_id = sqlc.arg(plan_id)
  AND (sqlc.arg(after_id)::bigint = 0 OR d.id < sqlc.arg(after_id))
ORDER BY d.id DESC
LIMIT sqlc.arg(page_limit);

-- name: CountRunningRestoreDrills :one
SELECT count(*) FROM restore_drills WHERE plan_id = $1 AND status = 'running';
