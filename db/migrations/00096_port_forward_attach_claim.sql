-- The attach claim stops being spent by the REQUEST that tries and starts being
-- spent by the SESSION it opens (ADR-065). The single-use rule existed to stop a
-- replay; "one HTTP request per mint" was only ever a proxy for that, and it
-- stopped being an exact one the day the attach acquired a transport ladder
-- above it (ADR-064): a rung that gives up burns a token that never carried a
-- byte, and every remaining rung is then told the token was "already used".
--
-- Two columns turn that rule into the one it was standing in for — at most one
-- live session per mint, held by one attacher, for the token's TTL and no
-- longer:
--
--   attach_key_hash — the first claimant's per-mint attach key, hashed exactly
--     like the token (§23.2). The claim stamps it, a re-claim must present the
--     same one, and a different one matches zero rows, which is the replay. It
--     is nullable ONLY so this migration is additive: the claim itself never
--     writes NULL, and an attach that presents no key at all (an N-1 CLI mid
--     rolling upgrade, the dashboard's browser terminal) gets server-generated
--     random bytes rather than a NULL or a sentinel — no presentable key hashes
--     to those, so such a session stays strictly single-use. A column left
--     matching ANYTHING would turn the compatibility shim into the replay hole
--     the rule exists to keep shut (ADR-065 §7).
--
--   attach_seq — the attach generation. It is what makes supersession decidable
--     rather than guessed: a claim returning a generation above the first means
--     a previous attach lost and must be cut, the heartbeat matches on it so an
--     attach superseded on another replica ends itself within one beat, and the
--     close is guarded by it so a superseded socket cannot finalize the row its
--     successor is using.
--
-- Both are additive and rolling-upgrade safe. An N-1 replica runs the old claim,
-- which touches neither column: it leaves the row at generation 0 with a NULL
-- key, which the new claim reads as "not yet stamped" and takes over normally.
-- Nothing here changes what a token costs or how long it lives — the TTL is the
-- whole of the re-claim window, and an expired token is refused exactly as
-- before.

-- +goose Up
ALTER TABLE port_forward_sessions
    ADD COLUMN attach_key_hash bytea,
    ADD COLUMN attach_seq bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE port_forward_sessions
    DROP COLUMN attach_seq,
    DROP COLUMN attach_key_hash;
