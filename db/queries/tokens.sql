-- API token management (§10.3). Token values are never stored nor
-- returned: only the SHA-256 hash and the identification prefix.

-- name: ListApiTokensPage :many
SELECT * FROM api_tokens
WHERE team_id = sqlc.arg(team_id) AND revoked_at IS NULL
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: CreateApiToken :one
INSERT INTO api_tokens (team_id, name, token_prefix, token_hash, permissions, ip_allowlist, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: RevokeApiTokenByUUID :execrows
UPDATE api_tokens SET revoked_at = now(), updated_at = now()
WHERE uuid = $1 AND team_id = $2 AND revoked_at IS NULL;
