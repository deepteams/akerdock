-- Environment variables (data-dictionary §8.5). Every value is envelope
-- encrypted; is_secret only drives UI masking (INV-003) — this avoids any
-- classification mistake.

-- +goose Up
CREATE TABLE environment_variables (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    resource_id bigint NOT NULL REFERENCES resources (id) ON DELETE CASCADE,
    key text NOT NULL CHECK (key ~ '^[A-Za-z_][A-Za-z0-9_]*$'),
    value_enc bytea NOT NULL,
    is_secret boolean NOT NULL DEFAULT false,
    is_build_time boolean NOT NULL DEFAULT false,
    is_literal boolean NOT NULL DEFAULT false,
    is_multiline boolean NOT NULL DEFAULT false,
    is_locked boolean NOT NULL DEFAULT false,
    is_preview boolean NOT NULL DEFAULT false,
    is_generated boolean NOT NULL DEFAULT false,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (resource_id, key, is_preview)
);

CREATE INDEX environment_variables_resource_id_idx ON environment_variables (resource_id);

-- +goose Down
DROP TABLE environment_variables;
