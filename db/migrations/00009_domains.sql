-- Domains routed by the per-server proxy (data-dictionary §8.4).
-- service_component_id has no FK yet: service_components lands with the
-- compose services. The (fqdn, path) uniqueness is instance-global: it
-- removes routing ambiguity and cross-team collisions (INV-002).

-- +goose Up
CREATE TABLE domains (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    application_id bigint REFERENCES applications (id) ON DELETE CASCADE,
    service_component_id bigint,
    fqdn citext NOT NULL CHECK (fqdn ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$'),
    path text NOT NULL DEFAULT '/',
    target_port integer CHECK (target_port BETWEEN 1 AND 65535),
    is_generated boolean NOT NULL DEFAULT false,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((application_id IS NOT NULL)::int + (service_component_id IS NOT NULL)::int = 1),
    UNIQUE (fqdn, path)
);

CREATE INDEX domains_application_id_idx ON domains (application_id);

-- +goose Down
DROP TABLE domains;
