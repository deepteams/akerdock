-- Uptime monitoring (ADR-017).

-- name: CreateUptimeCheck :one
INSERT INTO uptime_checks (uuid, team_id, resource_id, name, kind, target, interval_seconds, timeout_seconds, failure_threshold, success_threshold, enabled)
VALUES ($1, $2, sqlc.narg(resource_id), $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetUptimeCheckByUUID :one
SELECT * FROM uptime_checks
WHERE uuid = $1 AND team_id = $2 AND deleted_at IS NULL;

-- name: ListUptimeChecksPage :many
SELECT * FROM uptime_checks
WHERE team_id = sqlc.arg(team_id) AND deleted_at IS NULL
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: UpdateUptimeCheck :execrows
UPDATE uptime_checks
SET name = $2, target = $3, interval_seconds = $4, timeout_seconds = $5,
    failure_threshold = $6, success_threshold = $7, enabled = $8,
    -- The schedule may have changed: the scheduler recomputes the window.
    next_run_at = NULL,
    updated_at = now(), version = version + 1
WHERE id = $1 AND version = sqlc.arg(expected_version) AND deleted_at IS NULL;

-- name: SoftDeleteUptimeCheck :execrows
UPDATE uptime_checks SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListDueUptimeChecks :many
-- The prober's work list: enabled checks whose window has passed (or was
-- never seeded). Owned by the scheduler leader — no extra locking needed.
SELECT * FROM uptime_checks
WHERE enabled AND deleted_at IS NULL
  AND (next_run_at IS NULL OR next_run_at <= now())
ORDER BY id;

-- name: RecordUptimeResult :exec
INSERT INTO uptime_check_results (check_id, ok, latency_ms, status_code, error)
VALUES ($1, $2, sqlc.narg(latency_ms), sqlc.narg(status_code), sqlc.narg(error));

-- name: SetUptimeCheckState :exec
-- The prober's state write: counters, verdict, and the next window. Never
-- bumps `version` (not a user edit — it must not conflict with a PATCH).
UPDATE uptime_checks
SET status = $2, status_since = coalesce(sqlc.narg(status_since), status_since),
    consecutive_failures = $3, consecutive_successes = $4,
    last_checked_at = now(), last_latency_ms = sqlc.narg(last_latency_ms),
    last_error = sqlc.narg(last_error),
    next_run_at = sqlc.arg(next_run_at)
WHERE id = $1;

-- name: ListUptimeResultsPage :many
SELECT * FROM uptime_check_results
WHERE check_id = sqlc.arg(check_id)
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: GetResourceByUUIDForTeam :one
-- The optional resource link of a check (INV-002 isolation).
SELECT * FROM resources
WHERE uuid = $1 AND team_id = $2 AND deleted_at IS NULL;

-- name: PurgeUptimeResults :execrows
-- History retention (§19.2): raw probe results are bounded; the check row
-- keeps the current verdict forever.
DELETE FROM uptime_check_results
WHERE checked_at < now() - make_interval(days => sqlc.arg(retention_days)::int);
