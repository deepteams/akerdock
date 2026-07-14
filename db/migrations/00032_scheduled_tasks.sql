-- Scheduled tasks (PRD §192, data-dictionary §9.7): a cron that runs a command
-- inside a deployed resource's container.
--
-- Two decisions are worth spelling out, because both concern things that would
-- otherwise fail in silence:
--
--   * `skipped` is a first-class execution status. A task that did not run
--     because the previous one was still going, or because the instance was
--     down at its occurrence, leaves a row saying so — with the reason. The
--     alternative is an empty history that reads exactly like "nothing was ever
--     scheduled", which is how a nightly job goes missing for a month.
--
--   * The output is stored TRUNCATED and bounded. A command that prints a
--     hundred megabytes must not be able to fill the control plane's database;
--     the row records that truncation happened, so what is shown is never
--     mistaken for the whole output.

-- +goose Up
CREATE TYPE task_execution_status AS ENUM ('running', 'succeeded', 'failed', 'skipped');

-- What to do when an occurrence comes due while the previous run is still
-- going. `skip` is the default: a cron that fires faster than it completes
-- must not pile up executions on the server.
CREATE TYPE task_overlap_policy AS ENUM ('skip', 'queue');

-- What to do with an occurrence the scheduler did not see in time — the
-- instance was down, or the leader was elsewhere. `run` fires it once on
-- recovery (a nightly cleanup that missed midnight is still worth running);
-- `skip` abandons it (a "9am digest" sent at 3pm is noise).
CREATE TYPE task_missed_run_policy AS ENUM ('run', 'skip');

CREATE TABLE scheduled_tasks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE RESTRICT,
    resource_id bigint NOT NULL REFERENCES resources (id) ON DELETE CASCADE,
    -- Which container of the resource. NULL = the resource's own container,
    -- which is the only case an application has; a compose stack (P2) names a
    -- service here.
    container text,
    name text NOT NULL,
    command text NOT NULL,
    cron_expression text NOT NULL,
    timezone text NOT NULL DEFAULT 'UTC',
    enabled boolean NOT NULL DEFAULT true,
    overlap_policy task_overlap_policy NOT NULL DEFAULT 'skip',
    missed_run_policy task_missed_run_policy NOT NULL DEFAULT 'run',
    timeout_seconds integer NOT NULL DEFAULT 300 CHECK (timeout_seconds > 0),
    next_run_at timestamptz,
    last_run_at timestamptz,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    version integer NOT NULL DEFAULT 1,
    UNIQUE (resource_id, name)
);

CREATE INDEX scheduled_tasks_resource_id_idx ON scheduled_tasks (resource_id);
-- Partial index: the scheduler only ever asks for enabled, live tasks.
CREATE INDEX scheduled_tasks_due_idx
    ON scheduled_tasks (next_run_at)
    WHERE enabled AND deleted_at IS NULL;

CREATE TABLE task_executions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    scheduled_task_id bigint NOT NULL REFERENCES scheduled_tasks (id) ON DELETE CASCADE,
    status task_execution_status NOT NULL DEFAULT 'running',
    -- Why an occurrence did not run. Set only for `skipped`, and always set
    -- then: a skip without a reason is a mystery in the history.
    skip_reason text,
    exit_code integer,
    -- Combined stdout/stderr, truncated. `output_truncated` says so, so a
    -- reader never mistakes the head of the output for all of it.
    output text,
    output_truncated boolean NOT NULL DEFAULT false,
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    duration_ms integer,
    CHECK ((status = 'skipped') = (skip_reason IS NOT NULL))
);

CREATE INDEX task_executions_task_id_idx ON task_executions (scheduled_task_id, id DESC);

-- +goose Down
DROP TABLE task_executions;
DROP TABLE scheduled_tasks;
DROP TYPE task_missed_run_policy;
DROP TYPE task_overlap_policy;
DROP TYPE task_execution_status;
