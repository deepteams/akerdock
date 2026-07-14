-- Automated Docker cleanup (PRD §3.7): the cleanup_* settings existed since
-- 00006; what was missing is the scheduling STATE — the cron pass has to own
-- next_run_at/last_run_at exactly like the backup plans do, or a missed
-- window is indistinguishable from "never scheduled".

-- +goose Up
ALTER TABLE servers ADD COLUMN cleanup_next_run_at timestamptz;
ALTER TABLE servers ADD COLUMN cleanup_last_run_at timestamptz;

-- +goose Down
ALTER TABLE servers DROP COLUMN cleanup_last_run_at;
ALTER TABLE servers DROP COLUMN cleanup_next_run_at;
