-- Web terminal sessions (PRD §5.7/§24.4, ADR-024, data-dictionary §10.6).

-- name: CreateTerminalSession :one
INSERT INTO terminal_sessions (
    team_id, user_id, target_kind, server_id, resource_id, target_name,
    target_component, client_ip, token_hash, token_expires_at
) VALUES (
    $1, sqlc.narg(user_id), $2, sqlc.narg(server_id), sqlc.narg(resource_id), $3,
    sqlc.narg(target_component), sqlc.narg(client_ip), $4, $5
)
RETURNING *;

-- name: ClaimTerminalSession :one
-- Single-use attach: the WHERE consumes the token atomically — a replayed
-- token matches zero rows, whatever the race. started_at is reset at claim
-- time so idle/max-duration windows measure the live session, not the gap
-- between issuance and attach.
UPDATE terminal_sessions
SET claimed_at = now(), started_at = now()
WHERE token_hash = $1
  AND claimed_at IS NULL
  AND ended_at IS NULL
  AND token_expires_at > now()
RETURNING *;

-- name: EndTerminalSession :execrows
-- Idempotent: only the first close wins, so end_reason keeps the true cause
-- when the WS teardown and a timeout race each other.
UPDATE terminal_sessions
SET ended_at = now(), end_reason = $2
WHERE id = $1 AND ended_at IS NULL;

-- name: CountOpenTerminalSessions :one
-- Live sessions plus still-claimable tokens: both hold a slot of the per-team
-- cap, otherwise issuing tokens in a burst would bypass it.
SELECT count(*) FROM terminal_sessions
WHERE team_id = $1
  AND ended_at IS NULL
  AND (claimed_at IS NOT NULL OR token_expires_at > now());

-- name: SweepTerminalSessions :execrows
-- Crash net: sessions live in-process, so a row left open past any possible
-- lifetime is a control-plane restart, not a session. Unclaimed expired
-- tokens are closed as revoked.
UPDATE terminal_sessions
SET ended_at = now(),
    end_reason = CASE WHEN claimed_at IS NULL THEN 'revoked'::terminal_end_reason
                      ELSE 'disconnect'::terminal_end_reason END
WHERE ended_at IS NULL
  AND (
    (claimed_at IS NULL AND token_expires_at < now())
    OR (claimed_at IS NOT NULL AND started_at < now() - make_interval(secs => sqlc.arg(max_duration_seconds)::int))
  );

-- name: PurgeTerminalSessions :execrows
DELETE FROM terminal_sessions
WHERE ended_at IS NOT NULL
  AND ended_at < now() - make_interval(days => sqlc.arg(retention_days)::int);

-- name: SetSessionMfaVerified :exec
-- Passkey step-up (rbac-matrix §5): stamps the browser session; freshness is
-- judged by the caller against the step-up window.
UPDATE sessions SET mfa_verified_at = now() WHERE id = $1;
