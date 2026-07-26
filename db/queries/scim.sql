-- SCIM 2.0 provisioning (ADR-038 bis). A SCIM token is scoped to one team; the
-- endpoints authenticate with it and act only within that team.

-- name: CreateScimToken :one
INSERT INTO scim_tokens (team_id, name, token_hash, created_by)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetScimTokenByHash :one
-- Resolves a bearer SCIM token to its team; unrevoked only.
SELECT s.id, s.uuid, s.team_id, t.uuid AS team_uuid
FROM scim_tokens s
JOIN teams t ON t.id = s.team_id AND t.deleted_at IS NULL
WHERE s.token_hash = $1 AND s.revoked_at IS NULL;

-- name: TouchScimTokenUsed :exec
UPDATE scim_tokens SET last_used_at = now() WHERE id = $1;

-- name: ListScimTokensPage :many
SELECT * FROM scim_tokens
WHERE team_id = $1 AND revoked_at IS NULL
ORDER BY id DESC;

-- name: RevokeScimToken :execrows
UPDATE scim_tokens SET revoked_at = now() WHERE uuid = $1 AND team_id = $2 AND revoked_at IS NULL;

-- name: GetTeamMemberByExternalID :one
-- Idempotent match for a SCIM-provisioned member (the IdP's externalId).
SELECT m.id AS membership_id, m.role, m.external_id, m.custom_role_id,
       u.id AS user_id, u.uuid AS user_uuid, u.email, u.name
FROM team_memberships m
JOIN users u ON u.id = m.user_id AND u.deleted_at IS NULL
WHERE m.team_id = $1 AND m.external_id = $2;

-- name: GetScimMember :one
-- A team member by user UUID (the SCIM resource id), with the internal user id
-- needed to revoke sessions on deprovision.
SELECT u.id AS user_id, u.uuid AS user_uuid, u.email, u.name, m.role, m.external_id
FROM team_memberships m
JOIN users u ON u.id = m.user_id AND u.deleted_at IS NULL
WHERE m.team_id = $1 AND u.uuid = $2;

-- name: SetMembershipExternalID :exec
UPDATE team_memberships tm SET external_id = sqlc.arg(external_id)
FROM users u
WHERE tm.user_id = u.id AND u.uuid = sqlc.arg(user_uuid) AND tm.team_id = sqlc.arg(team_id);

-- name: RemoveTeamMemberByUUID :execrows
-- Deprovision: drop the membership (the account and its sessions are handled
-- separately). Team-scoped by the member's user UUID.
DELETE FROM team_memberships tm
USING users u
WHERE tm.user_id = u.id AND u.uuid = $1 AND tm.team_id = $2;

-- name: RevokeApiTokensForUserInTeam :execrows
-- Deprovision: revoke every API token the user holds in this team.
UPDATE api_tokens SET revoked_at = now()
WHERE team_id = $1 AND created_by = $2 AND revoked_at IS NULL;

-- name: ListTeamMembersForScim :many
-- SCIM Users/Groups source: every member with its effective role (system role,
-- or the custom role uuid when set) so groups (=roles) can be assembled in Go.
SELECT m.role, m.external_id, m.created_at AS joined_at,
       u.uuid AS user_uuid, u.email, u.name,
       cr.uuid AS custom_role_uuid, cr.name AS custom_role_name
FROM team_memberships m
JOIN users u ON u.id = m.user_id AND u.deleted_at IS NULL
LEFT JOIN custom_roles cr ON cr.id = m.custom_role_id
WHERE m.team_id = $1
ORDER BY m.id DESC;
