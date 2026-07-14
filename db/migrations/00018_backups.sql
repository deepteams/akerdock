-- Database backups (data-dictionary §9.5/§9.6, PRD §7, ADR-014). S3
-- credentials are envelope encrypted. A `partial` execution means the local
-- dump succeeded but the S3 upload failed — an explicit status, never a
-- silent success (§20.5).

-- +goose Up
CREATE TYPE backup_execution_status AS ENUM ('running', 'succeeded', 'partial', 'failed');

CREATE TABLE s3_storages (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE RESTRICT,
    name text NOT NULL,
    endpoint text NOT NULL,
    region text,
    bucket text NOT NULL,
    path_prefix text,
    access_key_enc bytea NOT NULL,
    secret_key_enc bytea NOT NULL,
    -- A storage is only usable once a write/read/delete round-trip passed
    -- (§20.5): no silent misconfiguration.
    is_usable boolean NOT NULL DEFAULT false,
    last_check_error text,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    version integer NOT NULL DEFAULT 1,
    UNIQUE (team_id, name)
);

CREATE TABLE database_backup_plans (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    database_id bigint REFERENCES databases (id) ON DELETE CASCADE,
    service_component_id bigint,
    is_instance_backup boolean NOT NULL DEFAULT false,
    enabled boolean NOT NULL DEFAULT true,
    cron_expression text NOT NULL,
    timezone text NOT NULL DEFAULT 'UTC',
    dump_all boolean NOT NULL DEFAULT false,
    included_databases text[],
    excluded_collections text[],
    timeout_seconds integer NOT NULL DEFAULT 3600 CHECK (timeout_seconds > 0),
    s3_storage_id bigint REFERENCES s3_storages (id) ON DELETE RESTRICT,
    s3_only boolean NOT NULL DEFAULT false,
    save_local boolean NOT NULL DEFAULT true,
    retention_local_max_count integer NOT NULL DEFAULT 0 CHECK (retention_local_max_count >= 0),
    retention_local_max_days integer NOT NULL DEFAULT 0 CHECK (retention_local_max_days >= 0),
    retention_s3_max_count integer NOT NULL DEFAULT 0 CHECK (retention_s3_max_count >= 0),
    retention_s3_max_days integer NOT NULL DEFAULT 0 CHECK (retention_s3_max_days >= 0),
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    version integer NOT NULL DEFAULT 1,
    -- Exactly one target (§9.5).
    CHECK ((database_id IS NOT NULL)::int + (service_component_id IS NOT NULL)::int + is_instance_backup::int = 1)
);

CREATE INDEX database_backup_plans_database_id_idx ON database_backup_plans (database_id);

CREATE TABLE backup_executions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    backup_plan_id bigint NOT NULL REFERENCES database_backup_plans (id) ON DELETE CASCADE,
    job_id bigint REFERENCES jobs (id) ON DELETE SET NULL,
    status backup_execution_status NOT NULL DEFAULT 'running',
    filename text,
    size_bytes bigint,
    checksum_sha256 text,
    engine_version text,
    uploaded_to_s3 boolean NOT NULL DEFAULT false,
    s3_upload_error text,
    local_deleted_at timestamptz,
    error_message text,
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX backup_executions_plan_created_idx ON backup_executions (backup_plan_id, created_at DESC);

-- +goose Down
DROP TABLE backup_executions;
DROP TABLE database_backup_plans;
DROP TABLE s3_storages;
DROP TYPE backup_execution_status;
