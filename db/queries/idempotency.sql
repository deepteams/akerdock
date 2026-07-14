-- HTTP idempotency (§24.1).

-- name: ClaimIdempotencyKey :one
-- Inserts the key, or returns the existing row when the key was already
-- used: the caller compares the request hash and replays the response.
INSERT INTO idempotency_keys (team_id, key, endpoint, request_hash)
VALUES ($1, $2, $3, $4)
ON CONFLICT (team_id, key, endpoint) DO UPDATE SET key = EXCLUDED.key
RETURNING *, (xmax = 0) AS is_new;

-- name: CompleteIdempotencyKey :exec
UPDATE idempotency_keys
SET status_code = $2, response_body = $3, completed_at = now()
WHERE id = $1;

-- name: PurgeIdempotencyKeys :exec
DELETE FROM idempotency_keys WHERE created_at < now() - interval '24 hours';
