-- Only open rows participate in the crash sweep. The team/session history can
-- grow for years without making the 90-second liveness pass scan all of it.
--
-- This is deliberately separate from the column expansion: PostgreSQL cannot
-- build a concurrent index in a transaction, while the column migration should
-- remain atomic.

-- +goose NO TRANSACTION

-- +goose Up
CREATE INDEX CONCURRENTLY port_forward_sessions_heartbeat_idx
    ON port_forward_sessions (last_heartbeat_at)
    WHERE ended_at IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY port_forward_sessions_heartbeat_idx;
