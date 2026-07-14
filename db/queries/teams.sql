-- Teams (§10.1). Cursor pagination follows ERD §12: (team_id, id DESC)
-- ordering, opaque cursor carrying the last seen internal id.

-- name: GetTeamByID :one
SELECT * FROM teams WHERE id = $1 AND deleted_at IS NULL;

-- name: GetTeamByUUID :one
SELECT * FROM teams WHERE uuid = $1 AND deleted_at IS NULL;

-- name: ListTeamsPage :many
SELECT * FROM teams
WHERE deleted_at IS NULL AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListTeamMembersPage :many
SELECT m.id AS membership_id, m.role, m.created_at AS joined_at,
       u.uuid AS user_uuid, u.email, u.name
FROM team_memberships m
JOIN users u ON u.id = m.user_id AND u.deleted_at IS NULL
WHERE m.team_id = sqlc.arg(team_id)
  AND (sqlc.arg(after_id)::bigint = 0 OR m.id < sqlc.arg(after_id))
ORDER BY m.id DESC
LIMIT sqlc.arg(page_limit);

-- Invitations (§10.1). The link token is hashed like any credential; the
-- clear value is returned only once, at creation.

-- name: CreateInvitation :one
INSERT INTO invitations (team_id, email, role, token_hash, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListInvitationsPage :many
SELECT * FROM invitations
WHERE team_id = sqlc.arg(team_id)
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: RevokeInvitation :execrows
UPDATE invitations SET revoked_at = now()
WHERE uuid = $1 AND team_id = $2 AND accepted_at IS NULL AND revoked_at IS NULL;
