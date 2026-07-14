-- Deployment artifacts (data-dictionary §10.3, ADR-006): the rollback
-- candidates. Local images are protected from the automated cleanup
-- (INV-015); with a registry, the OCI digest makes rollback reproducible.

-- +goose Up
CREATE TYPE artifact_kind AS ENUM ('local_image', 'registry_image');

CREATE TABLE deployment_artifacts (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    deployment_id bigint NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,
    kind artifact_kind NOT NULL,
    image_name text NOT NULL,
    image_tag text,
    image_digest text,
    server_id bigint REFERENCES servers (id) ON DELETE CASCADE,
    registry_credential_id bigint,
    protected_from_cleanup boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX deployment_artifacts_deployment_id_idx ON deployment_artifacts (deployment_id);

-- +goose Down
DROP TABLE deployment_artifacts;
DROP TYPE artifact_kind;
