-- Infrastructure aggregate: servers and destinations (data-dictionary
-- §6.1/§6.2, server state machine §21.2).

-- +goose Up
CREATE TYPE server_status AS ENUM ('pending', 'validating', 'ready', 'unreachable', 'maintenance', 'deleting');
CREATE TYPE proxy_type AS ENUM ('traefik', 'caddy', 'none');
CREATE TYPE proxy_desired_state AS ENUM ('running', 'stopped');
CREATE TYPE resource_observed_status AS ENUM ('unknown', 'starting', 'healthy', 'unhealthy', 'exited', 'missing');
CREATE TYPE log_drain_kind AS ENUM ('none', 'axiom', 'new_relic', 'fluentbit');

CREATE TABLE servers (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE RESTRICT,
    name text NOT NULL,
    description text,
    host text NOT NULL,
    port integer NOT NULL DEFAULT 22 CHECK (port BETWEEN 1 AND 65535),
    ssh_user text NOT NULL DEFAULT 'root',
    use_sudo boolean NOT NULL DEFAULT false,
    ssh_timeout_seconds integer NOT NULL DEFAULT 30 CHECK (ssh_timeout_seconds > 0),
    private_key_id bigint NOT NULL REFERENCES private_keys (id) ON DELETE RESTRICT,
    status server_status NOT NULL DEFAULT 'pending',
    observed_at timestamptz,
    unreachable_since timestamptz,
    os_name text,
    architecture text CHECK (architecture IN ('amd64', 'arm64')),
    docker_version text,
    is_localhost boolean NOT NULL DEFAULT false,
    is_build_server boolean NOT NULL DEFAULT false,
    wildcard_domain text,
    proxy_type proxy_type NOT NULL DEFAULT 'traefik',
    proxy_desired_state proxy_desired_state NOT NULL DEFAULT 'running',
    proxy_observed_status resource_observed_status NOT NULL DEFAULT 'unknown',
    proxy_http_port integer NOT NULL DEFAULT 80 CHECK (proxy_http_port BETWEEN 1 AND 65535),
    proxy_https_port integer NOT NULL DEFAULT 443 CHECK (proxy_https_port BETWEEN 1 AND 65535),
    concurrent_builds integer NOT NULL DEFAULT 2 CHECK (concurrent_builds > 0),
    deployment_queue_limit integer NOT NULL DEFAULT 25 CHECK (deployment_queue_limit > 0),
    cleanup_enabled boolean NOT NULL DEFAULT false,
    cleanup_disk_threshold_pct integer CHECK (cleanup_disk_threshold_pct BETWEEN 1 AND 100),
    cleanup_cron text,
    cleanup_prune_volumes boolean NOT NULL DEFAULT false,
    cleanup_prune_networks boolean NOT NULL DEFAULT false,
    sentinel_enabled boolean NOT NULL DEFAULT false,
    sentinel_token_hash text,
    sentinel_push_interval_seconds integer NOT NULL DEFAULT 10 CHECK (sentinel_push_interval_seconds > 0),
    sentinel_metrics_retention_days integer NOT NULL DEFAULT 7 CHECK (sentinel_metrics_retention_days > 0),
    log_drain_kind log_drain_kind NOT NULL DEFAULT 'none',
    log_drain_config_enc bytea,
    ca_cert text,
    ca_key_enc bytea,
    cloud_credential_id bigint,
    cloud_external_id text,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    version integer NOT NULL DEFAULT 1
);

CREATE INDEX servers_team_id_id_idx ON servers (team_id, id DESC);
CREATE UNIQUE INDEX servers_team_name_key ON servers (team_id, name) WHERE deleted_at IS NULL;
CREATE INDEX servers_not_ready_idx ON servers (status) WHERE status <> 'ready';

CREATE TABLE destinations (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    server_id bigint NOT NULL REFERENCES servers (id) ON DELETE RESTRICT,
    name text NOT NULL,
    network text NOT NULL,
    is_default boolean NOT NULL DEFAULT false,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (server_id, network)
);

CREATE UNIQUE INDEX destinations_default_key ON destinations (server_id) WHERE is_default;

-- +goose Down
DROP TABLE destinations;
DROP TABLE servers;
DROP TYPE log_drain_kind;
DROP TYPE resource_observed_status;
DROP TYPE proxy_desired_state;
DROP TYPE proxy_type;
DROP TYPE server_status;
