-- Scale-to-zero (ADR-036, proxy-contract §8): a preview (previews-first, §8.3)
-- can be put to sleep after a window of inactivity and woken on the first
-- request by the akerdock-waker helper. Opt-in per application, off by default.
--
-- scale_to_zero_after_minutes is the idle window before sleeping; the waker
-- reports activity into a per-resource file the control plane reads over SSH.
-- The two new preview_status values model the sleep/wake state machine.

-- +goose Up
ALTER TYPE preview_status ADD VALUE 'sleeping';
ALTER TYPE preview_status ADD VALUE 'waking';

ALTER TABLE applications
    ADD COLUMN scale_to_zero boolean NOT NULL DEFAULT false,
    ADD COLUMN scale_to_zero_after_minutes integer NOT NULL DEFAULT 30;

-- +goose Down
ALTER TABLE applications
    DROP COLUMN scale_to_zero_after_minutes,
    DROP COLUMN scale_to_zero;
-- The enum values stay: PostgreSQL cannot drop enum values, and unused values
-- are harmless.
