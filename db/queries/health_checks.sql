-- Health checks (§8.8): one row per resource, gate the rolling update.

-- name: UpsertHealthCheck :one
INSERT INTO health_checks (resource_id, enabled, method, path, port, interval_seconds, timeout_seconds, retries, start_period_seconds)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (resource_id) DO UPDATE SET
    enabled = EXCLUDED.enabled, method = EXCLUDED.method, path = EXCLUDED.path,
    port = EXCLUDED.port, interval_seconds = EXCLUDED.interval_seconds,
    timeout_seconds = EXCLUDED.timeout_seconds, retries = EXCLUDED.retries,
    start_period_seconds = EXCLUDED.start_period_seconds, updated_at = now()
RETURNING *;

-- name: GetHealthCheck :one
SELECT * FROM health_checks WHERE resource_id = $1;
