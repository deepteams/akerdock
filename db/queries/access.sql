-- Access review (ADR-046 §9): the subjects holding platform permissions on a
-- resource. Read-only and deliberately un-cached — a denormalized copy of
-- who-can-see-what drifts from the rules it summarizes, and a review reading a
-- stale copy asserts a safety nobody verified.

-- name: ListTeamMembersForAccess :many
-- Every member with the material needed to resolve what they hold: the system
-- role, the custom role's permissions when one overrides it, and the instance
-- root flag (a root reaches everything and must not be silently absent).
SELECT m.role, u.uuid AS user_uuid, u.email, u.name, u.is_root,
       cr.uuid AS custom_role_uuid, cr.name AS custom_role_name,
       cr.permissions AS custom_permissions
FROM team_memberships m
JOIN users u ON u.id = m.user_id AND u.deleted_at IS NULL
LEFT JOIN custom_roles cr ON cr.id = m.custom_role_id
WHERE m.team_id = $1
ORDER BY u.email;

-- name: GetTeamMemberForAccess :one
SELECT m.role, u.uuid AS user_uuid, u.email, u.name, u.is_root,
       cr.uuid AS custom_role_uuid, cr.name AS custom_role_name,
       cr.permissions AS custom_permissions
FROM team_memberships m
JOIN users u ON u.id = m.user_id AND u.deleted_at IS NULL
LEFT JOIN custom_roles cr ON cr.id = m.custom_role_id
WHERE m.team_id = $1 AND u.uuid = $2;

-- name: ListApiTokensForAccess :many
-- Live tokens with their creator: a token's reach is its creator's (ADR-046
-- §7), so the creator is who an operator talks to about it. Revoked and expired
-- tokens are excluded — they grant nothing, and a review crowded with dead rows
-- is a review nobody finishes.
SELECT t.uuid, t.name, t.permissions, t.expires_at, t.last_used_at,
       u.uuid AS creator_uuid, u.email AS creator_email
FROM api_tokens t
LEFT JOIN users u ON u.id = t.created_by
WHERE t.team_id = $1
  AND t.revoked_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at > now())
ORDER BY t.id DESC;

-- name: ListInstanceRootsForAccess :many
-- Instance roots that are NOT members of this team: they reach it all the same
-- (rbac-matrix §3.9), and a view that omits them lies by exactly the account an
-- auditor asks about first.
SELECT u.uuid, u.email, u.name
FROM users u
WHERE u.is_root AND u.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM team_memberships m WHERE m.user_id = u.id AND m.team_id = $1
  )
ORDER BY u.email;

-- name: CountEnvironmentResources :one
-- How many deployable resources an environment covers — the weight behind
-- "member on billing" in the per-member view.
SELECT count(*) FROM resources
WHERE environment_id = $1 AND deleted_at IS NULL;

-- name: CountProjectResources :one
SELECT count(*) FROM resources r
JOIN environments e ON e.id = r.environment_id
WHERE e.project_id = $1 AND r.deleted_at IS NULL;

-- name: CountTeamResources :one
SELECT count(*) FROM resources
WHERE team_id = $1 AND deleted_at IS NULL;
