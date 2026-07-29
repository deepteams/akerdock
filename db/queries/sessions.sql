-- Browser sessions (PRD §698).

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1 AND deleted_at IS NULL;

-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, csrf_token, current_team_id, ip, user_agent, expires_at, mfa_pending)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ClearMfaPendingForUser :exec
-- Lift the forced-enrollment gate on all of a user's sessions once they confirm
-- an MFA factor (ADR — mfa_required).
UPDATE sessions SET mfa_pending = false WHERE user_id = $1 AND mfa_pending = true;

-- name: GetSessionByTokenHash :one
-- A session is only valid while it is unrevoked AND unexpired: both are checked
-- in SQL so no caller can forget one of them.
SELECT s.*, u.email, u.name AS user_name, u.deleted_at AS user_deleted_at
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1
  AND s.revoked_at IS NULL
  AND s.expires_at > now()
  AND u.deleted_at IS NULL;

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = now() WHERE id = $1;

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeAllSessionsOfUser :execrows
-- Used on logout-everywhere and on any credential change: a password reset that
-- leaves old sessions alive has reset nothing.
UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL;

-- name: PurgeExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at < now() - interval '7 days';

-- name: RecordFailedLogin :one
-- Counts the failure and locks the account past the threshold. Returns the row
-- so the caller can tell "locked now" from "still open".
UPDATE users
SET failed_login_count = failed_login_count + 1,
    locked_until = CASE
        WHEN failed_login_count + 1 >= sqlc.arg(max_attempts)::int
        THEN now() + make_interval(mins => sqlc.arg(lock_minutes)::int)
        ELSE locked_until
    END,
    updated_at = now()
WHERE id = $1
RETURNING failed_login_count, locked_until;

-- name: ClearFailedLogins :exec
UPDATE users SET failed_login_count = 0, locked_until = NULL, updated_at = now()
WHERE id = $1;

-- name: GetTeamMembershipForUser :one
-- The team a session acts in, with its role and public UUID (the dashboard
-- addresses team endpoints by UUID). Falls back to the personal team.
-- Carries the user's instance-root flag (users.is_root) so the session identity
-- can gate instance-wide settings (rbac-matrix §3.5).
-- A custom role (custom_role_id), when set, OVERRIDES the system role: its
-- granular permissions are carried back for the session identity (ADR-038).
--
-- `preferred_team_id` is the team the session asked to act in (its
-- current_team_id, or the user's remembered last_team_id at login). It is a
-- PREFERENCE, not a filter: the row comes back only if the user really holds a
-- membership in that team, so a session pinned to a team the user was removed
-- from silently falls back to their oldest one instead of keeping an access
-- nobody granted any more (INV-001). Pass 0 for "no preference".
--
-- Soft-deleted teams are excluded: a deleted team must stop being a place one
-- can act in, whatever a stale session row still points at.
SELECT tm.team_id, tm.role, t.uuid AS team_uuid, t.name AS team_name, u.is_root,
       cr.permissions AS custom_permissions, cr.uuid AS custom_role_uuid,
       cr.name AS custom_role_name
FROM team_memberships tm
JOIN teams t ON t.id = tm.team_id AND t.deleted_at IS NULL
JOIN users u ON u.id = tm.user_id
LEFT JOIN custom_roles cr ON cr.id = tm.custom_role_id
WHERE tm.user_id = $1
ORDER BY (tm.team_id = sqlc.arg(preferred_team_id)::bigint) DESC, tm.team_id
LIMIT 1;

-- name: ListTeamMembershipsForUser :many
-- Every team the user may act in — the source of the dashboard's team switcher.
-- Deliberately NOT /teams (which lists the instance's teams for the root): the
-- switcher must offer memberships only, or switching would become a way to
-- enter a team nobody added you to.
SELECT tm.team_id, tm.role, t.uuid AS team_uuid, t.name AS team_name, t.personal
FROM team_memberships tm
JOIN teams t ON t.id = tm.team_id AND t.deleted_at IS NULL
WHERE tm.user_id = $1
ORDER BY t.personal DESC, lower(t.name), tm.team_id;

-- name: SetSessionCurrentTeam :execrows
-- Moves a live session into another team (PRD §37). Revoked sessions are not
-- matched: nothing may be done through a session that is already dead.
UPDATE sessions SET current_team_id = $2 WHERE id = $1 AND revoked_at IS NULL;

-- name: SetUserLastTeam :exec
-- Remembers the team across sessions, so the next login opens where the user
-- left off rather than on their oldest team.
UPDATE users SET last_team_id = $2, updated_at = now() WHERE id = $1;

-- name: CreatePersonalTeam :one
INSERT INTO teams (name, personal) VALUES ($1, true) RETURNING *;

-- name: AddTeamMember :exec
INSERT INTO team_memberships (team_id, user_id, role, custom_role_id) VALUES ($1, $2, $3, $4)
ON CONFLICT (team_id, user_id) DO NOTHING;
