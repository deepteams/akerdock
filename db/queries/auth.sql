-- Bearer token authentication (§10.3, ERD §12: prefix pre-filter then
-- constant-time hash comparison in the application).

-- name: GetActiveApiTokensByPrefix :many
SELECT t.*, tm.uuid AS team_uuid
FROM api_tokens t
JOIN teams tm ON tm.id = t.team_id AND tm.deleted_at IS NULL
WHERE t.token_prefix = $1 AND t.revoked_at IS NULL;

-- name: TouchApiTokenLastUsed :exec
UPDATE api_tokens SET last_used_at = now() WHERE id = $1;
