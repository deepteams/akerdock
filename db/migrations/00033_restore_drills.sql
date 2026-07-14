-- Restore drills (ADR-014, §20.5): a backup that has never been restored is not
-- a backup, it is a file. The drill restores the latest dump into a DISPOSABLE
-- database on the same server, counts what came back, and destroys it.
--
-- The counting is what makes the drill more than a smoke test. A dump can
-- gunzip cleanly, restore without an error, and contain nothing — an empty
-- `psql` restore exits 0. So the number of tables is recorded AT BACKUP TIME,
-- from the live database, and the drill asserts the restored copy has the same
-- number. A backup that silently lost its tables fails the drill instead of
-- waiting for the day someone actually needs it.

-- +goose Up
CREATE TYPE restore_drill_status AS ENUM ('running', 'succeeded', 'failed');

ALTER TABLE database_backup_plans
    ADD COLUMN drill_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN drill_interval_days integer NOT NULL DEFAULT 7 CHECK (drill_interval_days > 0),
    ADD COLUMN last_drill_at timestamptz,
    ADD COLUMN last_drill_status restore_drill_status;

-- Recorded from the SOURCE database at dump time: what the dump is supposed to
-- contain. Nullable, because backups taken before this column existed have no
-- reference count — and a drill with nothing to compare against says so rather
-- than inventing a verdict.
ALTER TABLE backup_executions
    ADD COLUMN table_count integer;

CREATE TABLE restore_drills (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    plan_id bigint NOT NULL REFERENCES database_backup_plans (id) ON DELETE CASCADE,
    execution_id bigint REFERENCES backup_executions (id) ON DELETE SET NULL,
    status restore_drill_status NOT NULL DEFAULT 'running',
    tables_expected integer,
    tables_restored integer,
    -- Why the drill failed, in the operator's words. Never a credential.
    error_message text,
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    duration_ms integer
);

CREATE INDEX restore_drills_plan_id_idx ON restore_drills (plan_id, id DESC);

-- +goose Down
DROP TABLE restore_drills;
ALTER TABLE backup_executions DROP COLUMN table_count;
ALTER TABLE database_backup_plans
    DROP COLUMN drill_enabled,
    DROP COLUMN drill_interval_days,
    DROP COLUMN last_drill_at,
    DROP COLUMN last_drill_status;
DROP TYPE restore_drill_status;
