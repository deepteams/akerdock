-- Scoped role assignments (ADR-046 §1): exceptions to a member's team-wide base
-- role, held on one project or one environment.

-- name: ListRoleAssignmentsForUser :many
-- The resolution's hot path: everything one caller holds in one team, loaded
-- once per request. Project and environment UUIDs come along so the scope can
-- be rendered (`project:<uuid>`) without a second round trip.
SELECT ra.id, ra.uuid, ra.role, ra.project_id, ra.environment_id,
       cr.permissions AS custom_permissions, cr.name AS custom_role_name,
       p.uuid AS project_uuid, p.name AS project_name,
       e.uuid AS environment_uuid, e.name AS environment_name,
       e.project_id AS environment_project_id
FROM role_assignments ra
LEFT JOIN custom_roles cr ON cr.id = ra.custom_role_id
LEFT JOIN projects p ON p.id = ra.project_id
LEFT JOIN environments e ON e.id = ra.environment_id
WHERE ra.user_id = $1 AND ra.team_id = $2;

-- name: ListRoleAssignmentsForTeam :many
-- The team's assignments, for the management screen and the access review.
SELECT ra.id, ra.uuid, ra.role, ra.created_at,
       u.uuid AS user_uuid, u.email,
       cr.uuid AS custom_role_uuid, cr.name AS custom_role_name,
       cr.permissions AS custom_permissions,
       p.uuid AS project_uuid, p.name AS project_name,
       e.uuid AS environment_uuid, e.name AS environment_name
FROM role_assignments ra
JOIN users u ON u.id = ra.user_id AND u.deleted_at IS NULL
LEFT JOIN custom_roles cr ON cr.id = ra.custom_role_id
LEFT JOIN projects p ON p.id = ra.project_id
LEFT JOIN environments e ON e.id = ra.environment_id
WHERE ra.team_id = $1
ORDER BY u.email, ra.id;

-- name: ListRoleAssignmentsForTeamMembers :many
-- Every assignment of the team, keyed by user id — what the reverse resolution
-- (the access review) walks once instead of querying per member.
SELECT ra.user_id, ra.role, ra.project_id, ra.environment_id,
       cr.permissions AS custom_permissions, cr.name AS custom_role_name
FROM role_assignments ra
LEFT JOIN custom_roles cr ON cr.id = ra.custom_role_id
WHERE ra.team_id = $1;

-- name: CreateRoleAssignment :one
INSERT INTO role_assignments (
    team_id, user_id, role, custom_role_id, project_id, environment_id, created_by
) VALUES (
    $1, $2, sqlc.narg(role), sqlc.narg(custom_role_id),
    sqlc.narg(project_id), sqlc.narg(environment_id), sqlc.narg(created_by)
)
RETURNING *;

-- name: DeleteRoleAssignment :execrows
DELETE FROM role_assignments WHERE uuid = $1 AND team_id = $2;

-- name: GetRoleAssignmentByUUID :one
SELECT * FROM role_assignments WHERE uuid = $1 AND team_id = $2;

-- name: CountRoleAssignmentsForTeam :one
-- Scoping is inert until someone uses it: with no assignment in a team, the
-- resolution short-circuits to the base role and behaves exactly as before.
SELECT count(*) FROM role_assignments WHERE team_id = $1;

-- name: GetTeamMemberIDByUserUUID :one
-- The member behind a user uuid. An assignment to somebody who is not a member
-- of the team is a grant to somebody who is not in the room, so this returning
-- no row is a 404 rather than a silent insert.
SELECT m.user_id
FROM team_memberships m
JOIN users u ON u.id = m.user_id AND u.deleted_at IS NULL
WHERE m.team_id = $1 AND u.uuid = $2;
