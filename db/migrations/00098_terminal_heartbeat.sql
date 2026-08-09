-- An attached terminal is activity, and it needs a row to say so on (ADR-067
-- §1). Scale-to-zero has exactly one activity source, the waker's per-resource
-- file, and the waker only writes it when it serves a PROXIED HTTP request. A
-- container terminal is a TTY exec on the agent channel: its bytes do cross the
-- agent, but they cross it as an opaque exec attach, not as something the waker
-- module is in a position to attribute to a resource's activity clock. So a
-- developer working in a shell for half an hour read as perfect idleness, and
-- the scheduler stopped the very container they were typing in — the same hole
-- 00094 closed for the tunnel, seen from the other door.
--
-- This is port_forward_sessions.last_heartbeat_at (00087) for the other family,
-- added for the same two reasons. The bridge already pings its peer every 20 s
-- and had nowhere durable to record it: the beat needs a statement whose
-- zero-rows answer means "this session is durably over" — finalized by another
-- replica, by the sweep, or by a re-claim that superseded this attach — and an
-- operator reading the table needs to tell a live shell from a row left behind
-- by a control plane that crashed.
--
-- Nullable, deliberately, and for rolling upgrades (PRD §18.2): an N-1 API
-- writes no beat, so NULL means "legacy session" and nothing may age a row by
-- this column. The terminal sweep keeps judging rows by started_at and the
-- token's TTL exactly as it did — what closes a terminal row is not this
-- decision's business.
--
-- No index, unlike 00088. That one exists because the tunnel's sweep scans by
-- heartbeat; nothing selects terminal_sessions on this column, and an index no
-- statement reads is write amplification on every beat of every shell.
--
-- The end reason costs nothing here: terminal_end_reason is one enum shared by
-- both session tables, and 00094 already added `target_stopped` to it for the
-- tunnel. The terminal gains a Go constant and no schema at all.

-- +goose Up
ALTER TABLE terminal_sessions
    ADD COLUMN last_heartbeat_at timestamptz;

-- +goose Down
ALTER TABLE terminal_sessions DROP COLUMN last_heartbeat_at;
