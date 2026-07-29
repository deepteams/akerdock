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

-- name: GetTokenCreatorAuthority :one
-- What a token's creator holds in the token's team, re-read on every request
-- (rbac-matrix §4.2). A token never grants more than its creator, so this is
-- the ceiling its own scopes are intersected with — and it is why a demoted
-- creator narrows their tokens without anyone revoking anything.
--
-- No row means the creator is no longer a member of that team: the token then
-- holds nothing, which is the convergence the rule exists for.
SELECT tm.role, tm.user_id, cr.permissions AS custom_permissions
FROM team_memberships tm
LEFT JOIN custom_roles cr ON cr.id = tm.custom_role_id
JOIN users u ON u.id = tm.user_id AND u.deleted_at IS NULL
WHERE tm.user_id = $1 AND tm.team_id = $2;
