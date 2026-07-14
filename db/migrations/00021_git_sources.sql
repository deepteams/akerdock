-- Git sources (data-dictionary §7.1, PRD §5.1): how a repository is reached.
-- v1 covers `public` (no credential) and `deploy_key` (an SSH key of the same
-- team, INV-002). `github_app` is declared by the enum but its FK lands with
-- the github_apps table.

-- +goose Up
CREATE TYPE git_source_kind AS ENUM ('public', 'deploy_key', 'github_app');
CREATE TYPE git_provider AS ENUM ('github', 'gitlab', 'bitbucket', 'gitea', 'other');

CREATE TABLE git_sources (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE RESTRICT,
    name text NOT NULL,
    kind git_source_kind NOT NULL,
    provider git_provider NOT NULL,
    api_url text,
    html_url text,
    -- RESTRICT: a key still used to clone a repository cannot be deleted.
    private_key_id bigint REFERENCES private_keys (id) ON DELETE RESTRICT,
    -- FK added by the migration that creates github_apps.
    github_app_id bigint,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    version integer NOT NULL DEFAULT 1,
    UNIQUE (team_id, name),
    CHECK (kind <> 'deploy_key' OR private_key_id IS NOT NULL),
    CHECK (kind <> 'github_app' OR github_app_id IS NOT NULL)
);

CREATE INDEX git_sources_team_id_idx ON git_sources (team_id);

-- The application already carries git_source_id (migration 00007) without its
-- FK, since the table did not exist yet.
ALTER TABLE applications
    ADD CONSTRAINT applications_git_source_id_fkey
    FOREIGN KEY (git_source_id) REFERENCES git_sources (id) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE applications DROP CONSTRAINT applications_git_source_id_fkey;
DROP TABLE git_sources;
DROP TYPE git_provider;
DROP TYPE git_source_kind;
