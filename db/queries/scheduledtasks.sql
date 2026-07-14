-- Scheduled tasks (§192). The scheduler owns next_run_at, exactly like the
-- backup plans: the cron expression alone cannot tell whether an occurrence
-- has already fired.

-- name: CreateScheduledTask :one
INSERT INTO scheduled_tasks (team_id, resource_id, name, command, container, cron_expression, timezone, enabled, overlap_policy, missed_run_policy, timeout_seconds, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetScheduledTaskByUUID :one
SELECT sqlc.embed(t), r.uuid AS application_uuid
FROM scheduled_tasks t
JOIN resources r ON r.id = t.resource_id
WHERE t.uuid = $1 AND t.team_id = $2 AND t.deleted_at IS NULL;

-- name: GetScheduledTaskByID :one
SELECT sqlc.embed(t), r.uuid AS application_uuid
FROM scheduled_tasks t
JOIN resources r ON r.id = t.resource_id
WHERE t.id = $1 AND t.deleted_at IS NULL;

-- name: ListScheduledTasksPage :many
SELECT sqlc.embed(t), r.uuid AS application_uuid
FROM scheduled_tasks t
JOIN resources r ON r.id = t.resource_id
WHERE t.resource_id = sqlc.arg(resource_id) AND t.deleted_at IS NULL
  AND (sqlc.arg(after_id)::bigint = 0 OR t.id < sqlc.arg(after_id))
ORDER BY t.id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListSchedulableTasks :many
-- Every enabled task of a live resource. The scheduler decides what is due:
-- a task whose next_run_at is NULL has never been scheduled and must be
-- seeded, so it cannot be filtered out here.
SELECT sqlc.embed(t), r.uuid AS application_uuid
FROM scheduled_tasks t
JOIN resources r ON r.id = t.resource_id
WHERE t.enabled AND t.deleted_at IS NULL AND r.deleted_at IS NULL;

-- name: SetScheduledTaskSchedule :exec
UPDATE scheduled_tasks
SET next_run_at = COALESCE(sqlc.narg(next_run_at)::timestamptz, next_run_at),
    last_run_at = COALESCE(sqlc.narg(last_run_at)::timestamptz, last_run_at)
WHERE id = $1;

-- name: UpdateScheduledTask :execrows
UPDATE scheduled_tasks
SET name = COALESCE(sqlc.narg(name), name),
    command = COALESCE(sqlc.narg(command), command),
    container = CASE WHEN sqlc.arg(set_container)::boolean THEN sqlc.narg(container) ELSE container END,
    cron_expression = COALESCE(sqlc.narg(cron_expression), cron_expression),
    timezone = COALESCE(sqlc.narg(timezone), timezone),
    enabled = COALESCE(sqlc.narg(enabled), enabled),
    overlap_policy = COALESCE(sqlc.narg(overlap_policy), overlap_policy),
    missed_run_policy = COALESCE(sqlc.narg(missed_run_policy), missed_run_policy),
    timeout_seconds = COALESCE(sqlc.narg(timeout_seconds), timeout_seconds),
    -- A changed cron means the old window is meaningless: dropping next_run_at
    -- forces the scheduler to recompute it from the new expression.
    next_run_at = CASE WHEN sqlc.narg(cron_expression) IS NOT NULL OR sqlc.narg(timezone) IS NOT NULL THEN NULL ELSE next_run_at END,
    updated_at = now(),
    updated_by = sqlc.narg(updated_by),
    version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND deleted_at IS NULL;

-- name: SoftDeleteScheduledTask :execrows
UPDATE scheduled_tasks SET deleted_at = now(), enabled = false
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateTaskExecution :one
INSERT INTO task_executions (scheduled_task_id, status, skip_reason)
VALUES ($1, $2, $3)
RETURNING *;

-- name: FinishTaskExecution :exec
UPDATE task_executions
SET status = $2, exit_code = $3, output = $4, output_truncated = $5,
    finished_at = now(),
    duration_ms = (EXTRACT(EPOCH FROM (now() - started_at)) * 1000)::int
WHERE id = $1;

-- name: ListTaskExecutionsPage :many
SELECT * FROM task_executions
WHERE scheduled_task_id = sqlc.arg(task_id)
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: CountRunningTaskExecutions :one
SELECT count(*) FROM task_executions
WHERE scheduled_task_id = $1 AND status = 'running';
