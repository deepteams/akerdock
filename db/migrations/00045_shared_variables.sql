-- Shared variables (PRD §5.4, §3.1; data-dictionary §8.6): hierarchical
-- values referenced as {{team.VAR}} / {{project.VAR}} / {{environment.VAR}}
-- inside resource variables, plus server-scoped variables injected into
-- every resource deployed on that server. team_id is always set (INV-001);
-- the scope names the inheritance level.

-- +goose Up
CREATE TYPE shared_variable_scope AS ENUM ('team', 'project', 'environment', 'server');

CREATE TABLE shared_variables (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    scope shared_variable_scope NOT NULL,
    project_id bigint REFERENCES projects (id) ON DELETE CASCADE,
    environment_id bigint REFERENCES environments (id) ON DELETE CASCADE,
    server_id bigint REFERENCES servers (id) ON DELETE CASCADE,
    key text NOT NULL CHECK (key ~ '^[A-Za-z_][A-Za-z0-9_]*$'),
    value_enc bytea NOT NULL,
    is_secret boolean NOT NULL DEFAULT false,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (scope = 'team' AND project_id IS NULL AND environment_id IS NULL AND server_id IS NULL)
        OR (scope = 'project' AND project_id IS NOT NULL AND environment_id IS NULL AND server_id IS NULL)
        OR (scope = 'environment' AND environment_id IS NOT NULL AND project_id IS NULL AND server_id IS NULL)
        OR (scope = 'server' AND server_id IS NOT NULL AND project_id IS NULL AND environment_id IS NULL)
    )
);

CREATE INDEX shared_variables_team_id_idx ON shared_variables (team_id);
CREATE UNIQUE INDEX shared_variables_team_key_idx ON shared_variables (team_id, key) WHERE scope = 'team';
CREATE UNIQUE INDEX shared_variables_project_key_idx ON shared_variables (project_id, key) WHERE scope = 'project';
CREATE UNIQUE INDEX shared_variables_environment_key_idx ON shared_variables (environment_id, key) WHERE scope = 'environment';
CREATE UNIQUE INDEX shared_variables_server_key_idx ON shared_variables (server_id, key) WHERE scope = 'server';

-- +goose Down
DROP TABLE shared_variables;
DROP TYPE shared_variable_scope;
