-- Browser sessions (PRD §698).

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1 AND deleted_at IS NULL;

-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, csrf_token, current_team_id, ip, user_agent, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

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
SELECT tm.team_id, tm.role, t.uuid AS team_uuid, u.is_root,
       cr.permissions AS custom_permissions, cr.uuid AS custom_role_uuid,
       cr.name AS custom_role_name
FROM team_memberships tm
JOIN teams t ON t.id = tm.team_id
JOIN users u ON u.id = tm.user_id
LEFT JOIN custom_roles cr ON cr.id = tm.custom_role_id
WHERE tm.user_id = $1
ORDER BY tm.team_id
LIMIT 1;

-- name: CreatePersonalTeam :one
INSERT INTO teams (name, personal) VALUES ($1, true) RETURNING *;

-- name: AddTeamMember :exec
INSERT INTO team_memberships (team_id, user_id, role) VALUES ($1, $2, $3)
ON CONFLICT (team_id, user_id) DO NOTHING;
