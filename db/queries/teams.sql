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

-- name: GetPendingInvitationByTokenHash :one
-- Reads a still-pending invitation WITHOUT claiming it, so the landing page of
-- an invitation link can say which team and which address it is for before
-- asking the invitee to create their account. Same pending guard as the claim:
-- an accepted, revoked or expired link resolves to nothing at all.
--
-- Returning the email is not an enumeration risk: the link token is a secret
-- issued to that address, so holding it already proves possession of the
-- invitation. Nothing here is reachable without it.
SELECT i.id, i.team_id, i.email, i.role, i.custom_role_id, i.expires_at,
       t.name AS team_name
FROM invitations i
JOIN teams t ON t.id = i.team_id AND t.deleted_at IS NULL
WHERE i.token_hash = $1
  AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now();

-- name: ListPendingInvitationsByEmail :many
-- Every still-pending invitation issued to an email. Used by the OAuth/SSO
-- signup path: an invitation authorizes account creation even when open
-- registration is off — the admin who issued it vouched for this exact address.
SELECT id, team_id, role, custom_role_id
FROM invitations
WHERE lower(email) = lower(sqlc.arg(email)::text)
  AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now();

-- name: AcceptInvitationByID :one
-- Atomically claim one pending invitation by id (the email-based signup already
-- matched the address). Same single-use guard as AcceptInvitation; no match
-- when it was revoked or expired between the listing and the claim.
UPDATE invitations SET accepted_at = now()
WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
RETURNING team_id, role, custom_role_id;

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

-- name: CreateTeam :one
-- A team created by the instance root (ADR-038): not personal — it exists to
-- be shared, unlike the bootstrap team of a user.
INSERT INTO teams (name, description, personal) VALUES ($1, sqlc.narg(description), false)
RETURNING *;
