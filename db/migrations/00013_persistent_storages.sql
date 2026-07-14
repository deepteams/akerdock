-- Persistent storages (data-dictionary §8.7, PRD §8): named volumes and
-- bind mounts. Remote data survives redeployments and application deletion
-- unless explicitly destroyed (INV-008). File mounts (editable content)
-- land with the compose services.

-- +goose Up
CREATE TYPE storage_kind AS ENUM ('volume', 'bind', 'file');

CREATE TABLE persistent_storages (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    resource_id bigint NOT NULL REFERENCES resources (id) ON DELETE CASCADE,
    kind storage_kind NOT NULL,
    name text,
    host_path text CHECK (host_path IS NULL OR (host_path LIKE '/%' AND host_path NOT LIKE '%..%')),
    mount_path text NOT NULL CHECK (mount_path LIKE '/%' AND mount_path NOT LIKE '%..%'),
    content text CHECK (content IS NULL OR length(content) <= 5 * 1024 * 1024),
    is_directory boolean NOT NULL DEFAULT false,
    file_mode text CHECK (file_mode IS NULL OR file_mode ~ '^[0-7]{3,4}$'),
    owner_uid integer,
    group_gid integer,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (resource_id, mount_path),
    -- Kind consistency (§8.7): volume ⇒ name, bind/file ⇒ host_path.
    CHECK ((kind = 'volume' AND name IS NOT NULL) OR (kind IN ('bind', 'file') AND host_path IS NOT NULL))
);

CREATE INDEX persistent_storages_resource_id_idx ON persistent_storages (resource_id);

-- +goose Down
DROP TABLE persistent_storages;
DROP TYPE storage_kind;
