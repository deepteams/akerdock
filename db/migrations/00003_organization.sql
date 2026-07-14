-- Organization aggregate, first part (data-dictionary §5.1/§5.2):
-- projects and environments. The resources table lands with the
-- application endpoints.

-- +goose Up
CREATE TABLE projects (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE RESTRICT,
    name text NOT NULL,
    slug text NOT NULL CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    description text,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    version integer NOT NULL DEFAULT 1
);

CREATE INDEX projects_team_id_id_idx ON projects (team_id, id DESC);
-- Slug reusable after tombstone (ERD §11).
CREATE UNIQUE INDEX projects_team_slug_key ON projects (team_id, slug) WHERE deleted_at IS NULL;

CREATE TABLE environments (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    project_id bigint NOT NULL REFERENCES projects (id) ON DELETE RESTRICT,
    name text NOT NULL,
    slug text NOT NULL CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    description text,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    version integer NOT NULL DEFAULT 1
);

CREATE INDEX environments_project_id_idx ON environments (project_id);
CREATE UNIQUE INDEX environments_project_slug_key ON environments (project_id, slug) WHERE deleted_at IS NULL;

-- +goose Down
DROP TABLE environments;
DROP TABLE projects;
