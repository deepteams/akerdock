-- Proxy configuration revisions (data-dictionary §11.1, proxy-contract
-- §6.2–§6.4): every generated routing file is a checksummed revision,
-- applied atomically, verified through the proxy API, and rolled back to
-- the last applied revision on failure. The content never holds a secret.

-- +goose Up
CREATE TYPE proxy_revision_status AS ENUM ('generated', 'applied', 'failed', 'rolled_back');

CREATE TABLE proxy_config_revisions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    server_id bigint NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    revision integer NOT NULL,
    -- Scope of the revision: one dynamic file per application (§5.1).
    scope text NOT NULL,
    proxy_type proxy_type NOT NULL,
    checksum_sha256 text NOT NULL,
    content text NOT NULL,
    status proxy_revision_status NOT NULL DEFAULT 'generated',
    error text,
    applied_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (server_id, revision)
);

CREATE INDEX proxy_config_revisions_scope_idx ON proxy_config_revisions (server_id, scope, id DESC);

-- +goose Down
DROP TABLE proxy_config_revisions;
DROP TYPE proxy_revision_status;
