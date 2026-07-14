-- Scheduled execution of the backup plans (§7.1, ADR-014). The cron
-- expression alone is not enough: the scheduler needs a due date it can
-- claim, so that a plan fires once per occurrence even with several
-- scheduler processes and across restarts.

-- +goose Up
ALTER TABLE database_backup_plans
    ADD COLUMN next_run_at timestamptz,
    ADD COLUMN last_run_at timestamptz;

-- Partial index: the scheduler only ever asks for enabled, due plans.
CREATE INDEX database_backup_plans_due_idx
    ON database_backup_plans (next_run_at)
    WHERE enabled AND deleted_at IS NULL;

-- +goose Down
DROP INDEX database_backup_plans_due_idx;
ALTER TABLE database_backup_plans
    DROP COLUMN next_run_at,
    DROP COLUMN last_run_at;
