-- Web terminal sessions (PRD §5.7/§24.4, ADR-024, data-dictionary §10.6).

-- name: CreateTerminalSession :one
INSERT INTO terminal_sessions (
    team_id, user_id, target_kind, server_id, resource_id, target_name,
    target_component, preview_id, client_ip, token_hash, token_expires_at
) VALUES (
    $1, sqlc.narg(user_id), $2, sqlc.narg(server_id), sqlc.narg(resource_id), $3,
    sqlc.narg(target_component), sqlc.narg(preview_id), sqlc.narg(client_ip), $4, $5
)
RETURNING *;

-- name: ClaimTerminalSession :one
-- Idempotent within the TTL, bound to the attacher (ADR-065): a first claim
-- stamps the attacher's key hash, a re-claim must present the same one, a
-- different one matches zero rows — which is the replay the rule exists to
-- stop. The idempotence lives in the WHERE and nowhere else: read-then-write
-- would race two rungs of the same ladder into two attaches that both believe
-- they own the session, the one failure mode strict single-use never had.
--
-- started_at is reset at claim time so idle/max-duration windows measure the
-- live session, not the gap between issuance and attach — but only on the
-- FIRST claim. The right-hand sides read pre-update values, so coalesce and the
-- attach_seq test pin both stamps to that claim; a retry loop must not buy
-- itself duration. token_expires_at > now() is unchanged and is the whole of
-- the re-claim window: there is no new lifetime here.
--
-- last_heartbeat_at is stamped here as well as on every beat (ADR-067 §1): the
-- column means "the last moment this session was known alive", and a claim is
-- such a moment. Leaving it NULL until the first beat twenty seconds later
-- would make a shell that attached a second ago indistinguishable from a row
-- written by a release that cannot heartbeat at all.
UPDATE terminal_sessions
SET claimed_at        = coalesce(claimed_at, now()),
    started_at        = CASE WHEN attach_seq = 0 THEN now() ELSE started_at END,
    last_heartbeat_at = now(),
    attach_key_hash   = $2,
    attach_seq        = attach_seq + 1
WHERE token_hash = $1
  AND ended_at IS NULL
  AND token_expires_at > now()
  AND (attach_key_hash IS NULL OR attach_key_hash = $2)
RETURNING *;

-- name: MarkTerminalSessionStreamed :execrows
-- Stamped once, when the session's single data stream joins — affordable only
-- because a terminal has exactly one (ADR-065 §6). Read by the sweep alone: it
-- is what tells an abandoned claim from a live shell, and it is deliberately
-- not a re-claim condition.
UPDATE terminal_sessions
SET streamed_at = now()
WHERE id = $1 AND streamed_at IS NULL;

-- name: HeartbeatTerminalSession :execrows
-- The bridge already pings its peer every 20 s. Persisting one successful beat
-- is what lets another replica, or the process that starts after a crash, tell
-- a live shell from a ghost — and it is the statement ADR-067 §1 hangs the
-- target's activity signal off, because a beat is the only moment an attached
-- session talks to the control plane while a developer sits and reads.
--
-- Generation-aware exactly like the tunnel's (ADR-065 §5): zero rows updated is
-- one sentence with three causes — the scheduler or another replica finalized
-- this row, or another attach superseded this one — and one conclusion, that
-- the socket must not outlive its own durable authorization.
UPDATE terminal_sessions
SET last_heartbeat_at = now()
WHERE id = $1
  AND attach_seq = $2
  AND claimed_at IS NOT NULL
  AND ended_at IS NULL;

-- name: GetTerminalSessionEndReason :one
-- The second half of the beat above, and the ONLY caller: read once, when the
-- heartbeat matched zero rows, so the socket can report the word its session
-- actually ended with instead of guessing one. It is never on the beat's common
-- path — a session reaches this statement at most once, on the beat that
-- discovers it is over — which is why the liveness update stays a single
-- statement rather than growing a RETURNING and a join for the case that
-- happens once.
--
-- NULL is a real answer and not an error: the row is still open, which is the
-- generation case (another attach superseded this one) rather than the
-- finalized one. The caller falls back to `disconnect` there, deliberately.
SELECT end_reason FROM terminal_sessions WHERE id = $1;

-- name: EndTerminalSession :execrows
-- Idempotent: only the first close wins, so end_reason keeps the true cause
-- when the WS teardown and a timeout race each other.
--
-- attach_seq is the optional generation guard of ADR-065 §5: an attach
-- finalizes the session only while it is still THE attach, so a displaced one
-- updates zero rows and the row stays open for the winner. Revocation, the
-- operator cut and the sweep pass no generation and finalize unconditionally —
-- their verdict is about the session, not about whichever socket holds it.
UPDATE terminal_sessions
SET ended_at = now(), end_reason = $2
WHERE id = $1
  AND ended_at IS NULL
  AND (sqlc.narg(attach_seq)::bigint IS NULL OR attach_seq = sqlc.narg(attach_seq)::bigint);

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
--
-- The third clause is what bounds ADR-065's abandoned attach. A client that
-- vanished between the session request and its data stream now leaves the row
-- claimed and re-claimable instead of ended, and this table has no heartbeat to
-- age it: without this, that row would hold one of the twenty per-team slots
-- until the max-duration ceiling. A claimed row that never carried its PTY is
-- therefore closed once its token dies — the slot is held for at most the TTL
-- plus one sweep interval — and as disconnect, which is what happened.
UPDATE terminal_sessions
SET ended_at = now(),
    end_reason = CASE WHEN claimed_at IS NULL THEN 'revoked'::terminal_end_reason
                      ELSE 'disconnect'::terminal_end_reason END
WHERE ended_at IS NULL
  AND (
    (claimed_at IS NULL AND token_expires_at < now())
    OR (claimed_at IS NOT NULL AND started_at < now() - make_interval(secs => sqlc.arg(max_duration_seconds)::int))
    OR (claimed_at IS NOT NULL AND streamed_at IS NULL AND token_expires_at < now())
  );

-- name: PurgeTerminalSessions :execrows
DELETE FROM terminal_sessions
WHERE ended_at IS NOT NULL
  AND ended_at < now() - make_interval(days => sqlc.arg(retention_days)::int);

-- name: SetSessionMfaVerified :exec
-- Passkey step-up (rbac-matrix §5): stamps the browser session; freshness is
-- judged by the caller against the step-up window.
UPDATE sessions SET mfa_verified_at = now() WHERE id = $1;
