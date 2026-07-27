-- Manual-first previews (preview_deploy_on_open=false): a fresh preview row is
-- a RESERVATION (URL + credential), not a deploy order — yet it sits at the
-- same 'queued' status the capacity queue promotes. Without a marker, the
-- scheduler auto-deployed reserved previews within a minute, defeating the
-- setting. `deploy_requested_at` records the first explicit human deploy order
-- (/deploy, /rebuild, the Previews tab, a fork approval): the capacity queue
-- only promotes a manual-first preview once it is set. Nullable, expand-only.

-- +goose Up
ALTER TABLE previews ADD COLUMN deploy_requested_at timestamptz;

-- +goose Down
ALTER TABLE previews DROP COLUMN deploy_requested_at;
