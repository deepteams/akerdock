-- Scoped role assignments (ADR-046): a role held on ONE project or ONE
-- environment, as an exception to the member's team-wide base role.
--
-- The base role stays on team_memberships. Folding the team level in here would
-- have been more uniform and would cost three things: the "last admin cannot be
-- demoted" guard becomes a query over two places, the members list needs a join
-- to say what someone is, and "member with no row" becomes a state the code has
-- to interpret. Every member having a base role is an invariant worth keeping
-- cheap (ADR-046 §1).

-- +goose Up
CREATE TABLE role_assignments (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Exactly one role source: a system role, or a custom role of the team.
    role team_role,
    custom_role_id bigint REFERENCES custom_roles (id) ON DELETE CASCADE,
    -- Exactly one scope target. The team level is the membership row, never a
    -- row here with two NULLs.
    project_id bigint REFERENCES projects (id) ON DELETE CASCADE,
    environment_id bigint REFERENCES environments (id) ON DELETE CASCADE,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT role_assignments_one_role CHECK (
        (role IS NOT NULL AND custom_role_id IS NULL)
        OR (role IS NULL AND custom_role_id IS NOT NULL)
    ),
    CONSTRAINT role_assignments_one_scope CHECK (
        (project_id IS NOT NULL AND environment_id IS NULL)
        OR (project_id IS NULL AND environment_id IS NOT NULL)
    )
);

-- NULLS NOT DISTINCT is load-bearing: with a plain UNIQUE, rows whose
-- custom_role_id (or project_id) is NULL would duplicate freely, because NULL
-- never equals NULL. Same reasoning as the notification-rule index (00024).
CREATE UNIQUE INDEX role_assignments_unique_idx ON role_assignments
    (user_id, project_id, environment_id, role, custom_role_id) NULLS NOT DISTINCT;

-- The hot path: every request resolves the caller's assignments once.
CREATE INDEX role_assignments_user_idx ON role_assignments (user_id, team_id);
CREATE INDEX role_assignments_project_idx ON role_assignments (project_id) WHERE project_id IS NOT NULL;
CREATE INDEX role_assignments_environment_idx ON role_assignments (environment_id) WHERE environment_id IS NOT NULL;

-- `none` is the empty base role: without it nothing partitions, because
-- restricting somebody TO a project requires their team-level role to grant
-- nothing (`member` grants everything, `reviewer` still sees every preview).
-- Added last, like every enum value in this schema: PostgreSQL forbids using a
-- new value in the transaction that adds it, and nothing above uses it.
ALTER TYPE team_role ADD VALUE IF NOT EXISTS 'none';

-- +goose Down
DROP TABLE role_assignments;
-- The `none` enum value is left in place: PostgreSQL does not remove one, and a
-- down migration that cannot restore the type exactly is worse than one that
-- leaves an unused value behind.
