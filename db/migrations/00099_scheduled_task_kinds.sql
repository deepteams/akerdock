-- A scheduled task gains a kind (ADR-071): `container_command` is the existing
-- exec, `github_workflow` asks GitHub to dispatch a workflow instead — the
-- clock stays here because GitHub's own `on: schedule` is documented
-- best-effort (delays, drops, silent 60-day auto-disable). One table, one
-- scheduler, one history: the kinds differ in exactly one step, what happens
-- when the occurrence fires, so a second table would duplicate everything else.
--
-- The per-kind field rules are CHECK constraints, not handler discipline: a row
-- that says "exec" with no command, or "dispatch" with a container to exec in,
-- is contradictory and must be unrepresentable. `kind` itself is immutable in
-- the API (a flip would inherit a history whose rows mean something else); the
-- schema does not need to enforce that, the constraints make a flipped row
-- invalid unless every per-kind field moves with it anyway.

-- +goose Up
CREATE TYPE task_kind AS ENUM ('container_command', 'github_workflow');

ALTER TABLE scheduled_tasks
    ADD COLUMN kind task_kind NOT NULL DEFAULT 'container_command',
    ADD COLUMN workflow_file text,
    ADD COLUMN workflow_ref text,
    ADD COLUMN workflow_inputs jsonb,
    ALTER COLUMN command DROP NOT NULL;

ALTER TABLE scheduled_tasks
    ADD CONSTRAINT scheduled_tasks_command_matches_kind
        CHECK ((kind = 'container_command') = (command IS NOT NULL)),
    ADD CONSTRAINT scheduled_tasks_workflow_file_matches_kind
        CHECK ((kind = 'github_workflow') = (workflow_file IS NOT NULL)),
    ADD CONSTRAINT scheduled_tasks_container_only_for_command
        CHECK (kind = 'container_command' OR container IS NULL),
    ADD CONSTRAINT scheduled_tasks_workflow_extras_only_for_workflow
        CHECK (kind = 'github_workflow' OR (workflow_ref IS NULL AND workflow_inputs IS NULL));

-- +goose Down
-- Down restores `command NOT NULL`, which workflow tasks cannot satisfy: they
-- are rows of a feature that no longer exists at this schema version, so they
-- go with it (their executions CASCADE).
DELETE FROM scheduled_tasks WHERE kind = 'github_workflow';

ALTER TABLE scheduled_tasks
    DROP CONSTRAINT scheduled_tasks_command_matches_kind,
    DROP CONSTRAINT scheduled_tasks_workflow_file_matches_kind,
    DROP CONSTRAINT scheduled_tasks_container_only_for_command,
    DROP CONSTRAINT scheduled_tasks_workflow_extras_only_for_workflow;

ALTER TABLE scheduled_tasks
    ALTER COLUMN command SET NOT NULL,
    DROP COLUMN kind,
    DROP COLUMN workflow_file,
    DROP COLUMN workflow_ref,
    DROP COLUMN workflow_inputs;

DROP TYPE task_kind;
