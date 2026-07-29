-- API token management (§10.3). Token values are never stored nor
-- returned: only the SHA-256 hash and the identification prefix.

-- name: ListApiTokensPage :many
-- Two readings of the same list, told apart by created_by: the personal one
-- ("my tokens", the caller's own) and the team-wide one an admin needs to see
-- what exists. NULL means no filter — the team reading; a value means only
-- that person's. The owner's email rides along so the team reading can say
-- WHOSE a token is, which is the only thing that makes it actionable.
SELECT t.*, u.email AS owner_email
FROM api_tokens t
LEFT JOIN users u ON u.id = t.created_by
WHERE t.team_id = sqlc.arg(team_id) AND t.revoked_at IS NULL
  AND (sqlc.narg(created_by)::bigint IS NULL OR t.created_by = sqlc.narg(created_by))
  AND (sqlc.arg(after_id)::bigint = 0 OR t.id < sqlc.arg(after_id))
ORDER BY t.id DESC
LIMIT sqlc.arg(page_limit);

-- name: CreateApiToken :one
-- created_by is the human who minted the token, and it is NOT bookkeeping: the
-- middleware intersects a token's permissions with its creator's on every
-- request (rbac-matrix §4.2), so a token without one is a token nothing can
-- narrow when its creator is demoted or leaves. It is also how a CLI token is
-- tied back to the person an access grant was issued to (ADR-045 §5).
INSERT INTO api_tokens (team_id, name, token_prefix, token_hash, permissions, ip_allowlist, expires_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, sqlc.narg(created_by))
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
