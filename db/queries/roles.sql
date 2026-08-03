-- Rôles custom d'une team (ADR-038). Composés à partir des permissions
-- granulaires du catalogue ; toujours team-scoped par team_id + uuid.

-- name: CreateCustomRole :one
INSERT INTO custom_roles (team_id, name, description, permissions)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListCustomRolesPage :many
SELECT * FROM custom_roles
WHERE team_id = sqlc.arg(team_id)
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: GetCustomRoleByUUID :one
SELECT * FROM custom_roles WHERE uuid = $1 AND team_id = $2;

-- name: GetCustomRoleByID :one
-- Read path of a simulated custom role (ADR-058): the session stores the id, and
-- resolving it back to permissions happens on every authenticated request while
-- the mode is on. The team is checked by the caller against the session's team.
SELECT * FROM custom_roles WHERE id = $1;

-- name: UpdateCustomRole :one
-- Partial update: name/description/permissions. Permissions arrive already
-- validated and closed under prerequisites by the handler.
UPDATE custom_roles SET
    name = COALESCE(sqlc.narg(name), name),
    description = CASE WHEN sqlc.arg(set_description)::boolean THEN sqlc.narg(description) ELSE description END,
    permissions = COALESCE(sqlc.narg(permissions), permissions),
    updated_at = now()
WHERE uuid = sqlc.arg(uuid) AND team_id = sqlc.arg(team_id)
RETURNING *;

-- name: DeleteCustomRole :execrows
DELETE FROM custom_roles WHERE uuid = $1 AND team_id = $2;

-- name: CountCustomRoleMembers :one
SELECT count(*) FROM team_memberships WHERE custom_role_id = $1;

-- Member role management (ADR-038).

-- name: GetTeamMemberByUUID :one
SELECT m.id AS membership_id, m.role, m.custom_role_id, m.created_at AS joined_at,
       u.uuid AS user_uuid, u.email, u.name,
       cr.uuid AS custom_role_uuid, cr.name AS custom_role_name
FROM team_memberships m
JOIN users u ON u.id = m.user_id AND u.deleted_at IS NULL
LEFT JOIN custom_roles cr ON cr.id = m.custom_role_id
WHERE m.team_id = sqlc.arg(team_id) AND u.uuid = sqlc.arg(user_uuid);

-- name: UpdateTeamMemberRole :execrows
-- Set a member's system role and (re)assign or clear its custom role. When
-- custom_role_id is non-null it overrides the system role at resolution time;
-- role is kept as the fallback. Team-scoped by the member's user UUID.
UPDATE team_memberships tm SET
    role = sqlc.arg(role),
    custom_role_id = sqlc.narg(custom_role_id),
    updated_at = now()
FROM users u
WHERE tm.user_id = u.id AND u.uuid = sqlc.arg(user_uuid) AND tm.team_id = sqlc.arg(team_id);

-- name: CountTeamAdmins :one
-- Effective admins: a member carrying a custom role is NOT an admin, whatever the
-- fallback role column says. Guards against removing the last admin.
SELECT count(*) FROM team_memberships
WHERE team_id = $1 AND role = 'admin' AND custom_role_id IS NULL;
