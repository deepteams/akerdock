-- A tunnel bridge lives in one API process. If that process disappears before
-- its teardown defer records ended_at, the database needs a durable liveness
-- signal to distinguish the dead socket from a genuinely open tunnel.
--
-- Nullable is deliberate rolling-upgrade compatibility: an N-1 API does not
-- write heartbeats, so NULL means "legacy session" and remains governed by the
-- existing four-hour maximum rather than being cut while N-1 still serves it.

-- +goose Up
ALTER TABLE port_forward_sessions
    ADD COLUMN last_heartbeat_at timestamptz;

-- +goose Down
ALTER TABLE port_forward_sessions DROP COLUMN last_heartbeat_at;
