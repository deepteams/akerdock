-- The two end reasons ADR-066 and ADR-067 introduce, added together and ahead
-- of the code that sends them.
--
-- They live in one migration on purpose: `terminal_end_reason` has been the
-- type of `port_forward_sessions.end_reason` since 00056, so both families read
-- from this single enum, and PostgreSQL forbids using a value in the
-- transaction that adds it. Adding them here lets three concurrent workstreams
-- — the egress attach, the terminal attach and the waker — reference values
-- that already exist instead of each racing to add its own.
--
-- `target_unreachable` (ADR-066): the attach now answers before it dials, so a
-- target that turns out to be unreachable is no longer a 409 at open. A failure
-- belonging to no particular stream ends the session with this, because
-- `disconnect` reads as "the connection to the manager dropped" and would send
-- a developer to check their own network for a preview that no longer exists.
-- On the terminal it also replaces a worse lie: a failed PTY currently persists
-- `revoked`, which claims an administrator acted when none did.
--
-- `wake_failed` (ADR-067): a scale-to-zero wake that did not come up inside its
-- budget. One value covers the timeout and the operational failure — they are
-- the same event to the person reading it, and the waker's own message names
-- the container that stalled.

-- +goose Up
ALTER TYPE terminal_end_reason ADD VALUE IF NOT EXISTS 'target_unreachable';
ALTER TYPE terminal_end_reason ADD VALUE IF NOT EXISTS 'wake_failed';

-- +goose Down
-- PostgreSQL cannot drop an enum value; an unused one is harmless (00062/00094).
SELECT 1;
