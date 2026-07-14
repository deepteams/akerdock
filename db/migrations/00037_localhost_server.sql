-- Pre-registered localhost server (PRD §3 parity, instance-config §6.2).
--
-- The seed must run ONCE in the lifetime of the instance, not once per boot:
-- an operator who deletes the localhost server must not find it resurrected
-- at the next restart. "Has a server ever been seeded" cannot be derived from
-- the servers table (deletion erases the evidence), so the fact is recorded
-- on the instance_settings singleton.
--
-- Default false on purpose: instances installed before this migration get the
-- localhost server seeded at their next boot, exactly like fresh ones.

-- +goose Up
ALTER TABLE instance_settings ADD COLUMN localhost_seeded boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE instance_settings DROP COLUMN localhost_seeded;
