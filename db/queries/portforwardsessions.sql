-- CLI TCP tunnel sessions (ADR-032, data-dictionary §10.7). Mirrors the
-- terminal-session lifecycle.

-- name: CreatePortForwardSession :one
INSERT INTO port_forward_sessions (
    team_id, user_id, server_id, resource_id, preview_id, target_name,
    target_component, target_port, client_ip, token_hash, token_expires_at
) VALUES (
    $1, sqlc.narg(user_id), sqlc.narg(server_id), sqlc.narg(resource_id),
    sqlc.narg(preview_id), $2, sqlc.narg(target_component), $3,
    sqlc.narg(client_ip), $4, $5
)
RETURNING *;

-- name: ClaimPortForwardSession :one
-- Single-use attach: consumes the token atomically (a replay matches zero rows).
UPDATE port_forward_sessions
SET claimed_at = now(), started_at = now()
WHERE token_hash = $1
  AND claimed_at IS NULL
  AND ended_at IS NULL
  AND token_expires_at > now()
RETURNING *;

-- name: EndPortForwardSession :execrows
UPDATE port_forward_sessions
SET ended_at = now(), end_reason = $2
WHERE id = $1 AND ended_at IS NULL;

-- name: CountOpenPortForwardSessions :one
SELECT count(*) FROM port_forward_sessions
WHERE team_id = $1
  AND ended_at IS NULL
  AND (claimed_at IS NOT NULL OR token_expires_at > now());

-- name: GetPortForwardSessionByUUID :one
SELECT * FROM port_forward_sessions WHERE uuid = $1 AND team_id = $2;

-- name: ListPortForwardSessionsPage :many
-- The operator's view of the team's tunnels. Newest first, like the grant list:
-- the question asked is almost always "what is forwarded right now". The joins
-- are LEFT because a session outlives its user and its endpoint — a row whose
-- target was deleted still has to be readable, or the audit trail has holes
-- exactly where something was removed.
SELECT s.*, u.email AS user_email, e.uuid AS endpoint_uuid
FROM port_forward_sessions s
LEFT JOIN users u ON u.id = s.user_id
LEFT JOIN external_endpoints e ON e.id = s.external_endpoint_id
WHERE s.team_id = $1
  AND s.id < sqlc.arg(before_id)::bigint
  AND (sqlc.narg(endpoint_id)::bigint IS NULL OR s.external_endpoint_id = sqlc.narg(endpoint_id))
  -- Same definition of "open" as the team cap, so the list and the 409 never
  -- disagree about how many sessions exist.
  AND (NOT sqlc.arg(active_only)::boolean
       OR (s.ended_at IS NULL AND (s.claimed_at IS NOT NULL OR s.token_expires_at > now())))
ORDER BY s.id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: PurgePortForwardSessions :execrows
DELETE FROM port_forward_sessions
WHERE ended_at IS NOT NULL
  AND ended_at < now() - make_interval(days => sqlc.arg(retention_days)::int);
