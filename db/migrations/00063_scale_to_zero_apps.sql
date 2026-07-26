-- Scale-to-zero for production applications (ADR-037), extending the
-- preview-only scale-to-zero (ADR-036). The two opt-ins are kept separate:
-- previews and production are different risk decisions.
--
-- Expand-only (the migration policy forbids RENAME/DROP in Up): the 00062
-- columns were preview-scoped, so their values move to new preview_* columns,
-- and the bare scale_to_zero is repurposed as the APPLICATION's own flag
-- (reset to default-off). scale_slept_at marks a deliberate sleep (NULL = awake)
-- so the UI/monitoring never read it as a crash.

-- +goose Up
ALTER TABLE applications
    ADD COLUMN preview_scale_to_zero boolean NOT NULL DEFAULT false,
    ADD COLUMN preview_scale_to_zero_after_minutes integer NOT NULL DEFAULT 30,
    ADD COLUMN scale_slept_at timestamptz;

-- Move the existing (preview-scoped) values into the new preview_* columns...
UPDATE applications SET
    preview_scale_to_zero = scale_to_zero,
    preview_scale_to_zero_after_minutes = scale_to_zero_after_minutes;

-- ...then repurpose scale_to_zero / scale_to_zero_after_minutes as the
-- application's own scale-to-zero, reset to the default-off state.
UPDATE applications SET
    scale_to_zero = false,
    scale_to_zero_after_minutes = 30;

-- +goose Down
ALTER TABLE applications
    DROP COLUMN scale_slept_at,
    DROP COLUMN preview_scale_to_zero_after_minutes,
    DROP COLUMN preview_scale_to_zero;
