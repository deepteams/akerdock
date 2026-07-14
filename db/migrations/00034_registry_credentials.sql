-- Private container registries (data-dictionary §6.5, §5.1/§5.2). The password
-- is envelope encrypted at rest, and reaches the server ONLY through the stdin
-- of `docker login --password-stdin` — never in argv, where any `ps` on the
-- host would read it (INV-003).
--
-- The referencing columns already existed (00007, 00011) without a foreign key,
-- because the table they point at did not exist yet. They get one now, with
-- RESTRICT: deleting a credential that a build config or a rollback artifact
-- still depends on would leave a deployment that cannot pull its own image.

-- +goose Up
CREATE TABLE registry_credentials (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE RESTRICT,
    name text NOT NULL,
    registry_url text NOT NULL,
    username text NOT NULL,
    password_enc bytea NOT NULL,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    version integer NOT NULL DEFAULT 1,
    UNIQUE (team_id, name)
);

CREATE INDEX registry_credentials_team_id_idx ON registry_credentials (team_id);

ALTER TABLE build_configs
    ADD CONSTRAINT build_configs_registry_credential_fk
        FOREIGN KEY (registry_credential_id) REFERENCES registry_credentials (id) ON DELETE RESTRICT,
    ADD CONSTRAINT build_configs_push_registry_credential_fk
        FOREIGN KEY (push_registry_credential_id) REFERENCES registry_credentials (id) ON DELETE RESTRICT;

ALTER TABLE deployment_artifacts
    ADD CONSTRAINT deployment_artifacts_registry_credential_fk
        FOREIGN KEY (registry_credential_id) REFERENCES registry_credentials (id) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE deployment_artifacts DROP CONSTRAINT deployment_artifacts_registry_credential_fk;
ALTER TABLE build_configs
    DROP CONSTRAINT build_configs_push_registry_credential_fk,
    DROP CONSTRAINT build_configs_registry_credential_fk;
DROP TABLE registry_credentials;
