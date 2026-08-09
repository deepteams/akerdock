-- The terminal half of ADR-065: an attach token is spent by the session it
-- opens, not by the request that tries. Three columns, all additive, so an N-1
-- replica that never reads them keeps serving the old strict single-use claim
-- against the same rows (PRD §18.2).
--
-- attach_key_hash is the SHA-256 of the first claimant's ephemeral attach key —
-- the credential that already binds a data stream to the session request that
-- spent the token. Storing it turns "one request per mint" into "one attacher
-- per mint": the same client may re-claim while its token lives, a different
-- one matches zero rows, which is the replay the single-use rule exists to
-- stop. Nullable only because the column is added to live rows; the claim never
-- writes NULL — an attach that presents no key (an N-1 CLI, the dashboard's
-- browser terminal) stores server-generated random bytes instead, so it stays
-- strictly single-use rather than becoming freely re-claimable for 60 s.
--
-- attach_seq is the attach generation. It is what makes supersession decidable:
-- the row that finalizes a session must be the attach that still holds it, and
-- a displaced one carries a stale value and updates zero rows.
--
-- streamed_at records whether the session's single data stream ever joined —
-- the first durable trace the terminal has of its PTY actually attaching. It is
-- read by the sweep and by nothing else: because an abandoned attach now leaves
-- its row claimed and re-claimable, and CountOpenTerminalSessions has no
-- freshness test, a row that never carried its PTY would otherwise hold one of
-- the twenty per-team slots until the max-duration ceiling, hours later. It is
-- deliberately NOT a re-claim condition (ADR-065 §6): the token's TTL is the
-- bound, and a session that has served bytes is re-claimable exactly like one
-- that has not.

-- +goose Up
ALTER TABLE terminal_sessions
    ADD COLUMN attach_key_hash bytea,
    ADD COLUMN attach_seq bigint NOT NULL DEFAULT 0,
    ADD COLUMN streamed_at timestamptz;

-- +goose Down
ALTER TABLE terminal_sessions
    DROP COLUMN attach_key_hash,
    DROP COLUMN attach_seq,
    DROP COLUMN streamed_at;
