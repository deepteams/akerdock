-- Teams (§10.1). Cursor pagination follows ERD §12: (team_id, id DESC)
-- ordering, opaque cursor carrying the last seen internal id.

-- name: GetTeamByID :one
SELECT * FROM teams WHERE id = $1 AND deleted_at IS NULL;

-- name: GetTeamByUUID :one
SELECT * FROM teams WHERE uuid = $1 AND deleted_at IS NULL;

-- name: UpdateTeam :one
-- Partial update of a team's name/description (§10.1).
UPDATE teams SET
    name = COALESCE(sqlc.narg(name), name),
    description = CASE WHEN sqlc.arg(set_description)::boolean THEN sqlc.narg(description) ELSE description END,
    updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: ListTeamsPage :many
SELECT * FROM teams
WHERE deleted_at IS NULL AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListTeamMembersPage :many
SELECT m.id AS membership_id, m.role, m.created_at AS joined_at,
       u.uuid AS user_uuid, u.email, u.name,
       cr.uuid AS custom_role_uuid, cr.name AS custom_role_name
FROM team_memberships m
JOIN users u ON u.id = m.user_id AND u.deleted_at IS NULL
LEFT JOIN custom_roles cr ON cr.id = m.custom_role_id
WHERE m.team_id = sqlc.arg(team_id)
  AND (sqlc.arg(after_id)::bigint = 0 OR m.id < sqlc.arg(after_id))
ORDER BY m.id DESC
LIMIT sqlc.arg(page_limit);

-- Invitations (§10.1). The link token is hashed like any credential; the
-- clear value is returned only once, at creation.

-- name: CreateInvitation :one
INSERT INTO invitations (team_id, email, role, token_hash, expires_at, custom_role_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: AcceptInvitation :one
-- Atomically claim a still-pending invitation by its link hash: the WHERE clause
-- is the single-use guard (accepted/revoked/expired all fail to match). Returns
-- the target team, role and optional custom role so the caller can add the
-- membership. Team-scoping is inherent — the invitation names its own team.
UPDATE invitations SET accepted_at = now()
WHERE token_hash = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
RETURNING team_id, email, role, custom_role_id;

-- name: ListInvitationsPage :many
SELECT i.*, cr.uuid AS custom_role_uuid, cr.name AS custom_role_name
FROM invitations i
LEFT JOIN custom_roles cr ON cr.id = i.custom_role_id
WHERE i.team_id = sqlc.arg(team_id)
  AND (sqlc.arg(after_id)::bigint = 0 OR i.id < sqlc.arg(after_id))
ORDER BY i.id DESC
LIMIT sqlc.arg(page_limit);

-- name: RevokeInvitation :execrows
UPDATE invitations SET revoked_at = now()
WHERE uuid = $1 AND team_id = $2 AND accepted_at IS NULL AND revoked_at IS NULL;

-- name: RotateInvitation :one
-- Regenerate the link of a still-pending invitation: rotate the token hash and
-- push the expiry out. Returns nothing if the invitation is not pending.
UPDATE invitations
SET token_hash = sqlc.arg(token_hash), expires_at = sqlc.arg(expires_at)
WHERE uuid = sqlc.arg(uuid) AND team_id = sqlc.arg(team_id)
  AND accepted_at IS NULL AND revoked_at IS NULL
RETURNING *;
