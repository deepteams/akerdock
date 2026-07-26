-- Preview TTL warning (§20.4.3): a heads-up before the reaper destroys an idle
-- preview, so a developer can /keep it. expiry_warned_at marks that the warning
-- was already emitted (fire once) — cleared on any redeploy or /keep.

-- +goose Up
ALTER TABLE previews
    ADD COLUMN expiry_warned_at timestamptz;

-- +goose Down
ALTER TABLE previews DROP COLUMN expiry_warned_at;
