-- GitHub Apps (data-dictionary §7.2, git-webhook-protocols §2) and the
-- discovered-repositories cache (§7.3).
--
-- The manifest flow (protocols §2.1) creates the row as a DRAFT first: the
-- credentials only exist once GitHub answers the conversion call, so app_id
-- and the encrypted secrets are nullable until then — amended in the data
-- dictionary alongside this migration. The anti-CSRF state token of the
-- callback is stored hashed, with its expiry: one-shot, 10 minutes.

-- +goose Up
CREATE TABLE github_apps (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE RESTRICT,
    name text NOT NULL,
    -- NULL while the manifest flow is in flight (draft).
    app_id bigint,
    slug text,
    installation_id bigint,
    client_id text,
    client_secret_enc bytea,
    webhook_secret_enc bytea,
    app_private_key_enc bytea,
    api_url text NOT NULL DEFAULT 'https://api.github.com',
    html_url text NOT NULL DEFAULT 'https://github.com',
    -- Manifest callback protection: SHA-256 of the one-shot state token and
    -- its expiry; cleared at conversion.
    manifest_state_hash text,
    manifest_state_expires_at timestamptz,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    version integer NOT NULL DEFAULT 1,
    UNIQUE (team_id, name)
);

CREATE INDEX github_apps_team_id_idx ON github_apps (team_id);
-- One row per GitHub app per team, once converted (drafts excluded).
CREATE UNIQUE INDEX github_apps_team_app_key ON github_apps (team_id, app_id) WHERE app_id IS NOT NULL;
-- Webhook routing: X-GitHub-Hook-Installation-Target-ID names the app_id.
CREATE INDEX github_apps_app_id_idx ON github_apps (app_id) WHERE app_id IS NOT NULL;

CREATE TABLE repositories (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    git_source_id bigint NOT NULL REFERENCES git_sources (id) ON DELETE CASCADE,
    -- Provider-side ID: stable across renames (INV-009).
    external_id text NOT NULL,
    full_name text NOT NULL,
    default_branch text,
    html_url text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (git_source_id, external_id)
);

CREATE INDEX repositories_git_source_id_idx ON repositories (git_source_id);
CREATE INDEX repositories_full_name_idx ON repositories (full_name);

-- The FKs announced by migrations 00021 and 00007.
ALTER TABLE git_sources
    ADD CONSTRAINT git_sources_github_app_id_fkey
    FOREIGN KEY (github_app_id) REFERENCES github_apps (id) ON DELETE RESTRICT;
ALTER TABLE applications
    ADD CONSTRAINT applications_repository_id_fkey
    FOREIGN KEY (repository_id) REFERENCES repositories (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE applications DROP CONSTRAINT applications_repository_id_fkey;
ALTER TABLE git_sources DROP CONSTRAINT git_sources_github_app_id_fkey;
DROP TABLE repositories;
DROP TABLE github_apps;
