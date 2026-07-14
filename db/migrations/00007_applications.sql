-- Application aggregate + execution aggregate core (data-dictionary §5.3,
-- §8.1–§8.3, §10.1–§10.2): resources union base, applications extension,
-- build/runtime configs, deployments and their steps. Adds the deferred FK
-- on jobs.resource_id. deployments.error_message comes from the OpenAPI
-- Deployment schema and was missing from the data dictionary (amended).

-- +goose Up
CREATE TYPE resource_type AS ENUM ('application', 'database', 'service');
CREATE TYPE resource_desired_status AS ENUM ('stopped', 'running', 'deleting', 'deleted');
CREATE TYPE build_pack AS ENUM ('nixpacks', 'railpack', 'static', 'dockerfile', 'compose', 'image');
CREATE TYPE redirect_direction AS ENUM ('both', 'www', 'non_www');
CREATE TYPE deployment_status AS ENUM ('queued', 'preparing', 'cloning', 'building', 'pushing', 'starting', 'healthchecking', 'switching', 'finishing', 'succeeded', 'failed', 'cancelled', 'retrying', 'superseded');
CREATE TYPE deployment_step_status AS ENUM ('pending', 'running', 'succeeded', 'failed', 'skipped', 'cancelled');
CREATE TYPE deployment_trigger AS ENUM ('manual', 'webhook', 'api', 'preview', 'schedule', 'config_apply', 'cli_local');
CREATE TYPE preview_protection AS ENUM ('none', 'basic_auth', 'signed_link');

CREATE TABLE resources (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE RESTRICT,
    environment_id bigint NOT NULL REFERENCES environments (id) ON DELETE RESTRICT,
    destination_id bigint NOT NULL REFERENCES destinations (id) ON DELETE RESTRICT,
    resource_type resource_type NOT NULL,
    name text NOT NULL,
    description text,
    desired_status resource_desired_status NOT NULL DEFAULT 'stopped',
    observed_status resource_observed_status NOT NULL DEFAULT 'unknown',
    observed_at timestamptz,
    last_online_at timestamptz,
    remnants jsonb,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    version integer NOT NULL DEFAULT 1
);

CREATE INDEX resources_team_id_id_idx ON resources (team_id, id DESC);
CREATE INDEX resources_environment_id_idx ON resources (environment_id);
CREATE INDEX resources_destination_id_idx ON resources (destination_id);
CREATE UNIQUE INDEX resources_environment_name_key ON resources (environment_id, name) WHERE deleted_at IS NULL;

-- team_id is denormalized from environment → project → team; the trigger
-- guarantees consistency (data-dictionary §5.3).
-- +goose StatementBegin
CREATE FUNCTION resources_team_consistency() RETURNS trigger AS $$
DECLARE
    env_team bigint;
BEGIN
    SELECT p.team_id INTO env_team
    FROM environments e JOIN projects p ON p.id = e.project_id
    WHERE e.id = NEW.environment_id;
    IF env_team IS NULL OR env_team <> NEW.team_id THEN
        RAISE EXCEPTION 'resources.team_id % inconsistent with environment %', NEW.team_id, NEW.environment_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER resources_team_consistency_trg
    BEFORE INSERT OR UPDATE OF team_id, environment_id ON resources
    FOR EACH ROW EXECUTE FUNCTION resources_team_consistency();

CREATE TABLE applications (
    id bigint PRIMARY KEY REFERENCES resources (id) ON DELETE CASCADE,
    git_source_id bigint,
    repository_id bigint,
    git_repository_url text,
    git_branch text,
    base_directory text NOT NULL DEFAULT '/',
    enable_submodules boolean NOT NULL DEFAULT false,
    enable_lfs boolean NOT NULL DEFAULT false,
    enable_shallow_clone boolean NOT NULL DEFAULT false,
    auto_deploy_enabled boolean NOT NULL DEFAULT true,
    watch_paths text,
    previews_enabled boolean NOT NULL DEFAULT false,
    preview_url_template text NOT NULL DEFAULT '{{pr_id}}.{{domain}}',
    preview_public_prs_enabled boolean NOT NULL DEFAULT false,
    preview_fork_approval_enabled boolean NOT NULL DEFAULT false,
    preview_max_concurrent integer CHECK (preview_max_concurrent > 0),
    preview_ttl_minutes integer CHECK (preview_ttl_minutes > 0),
    preview_protection preview_protection NOT NULL DEFAULT 'basic_auth',
    preview_require_label text,
    preview_comment_commands_enabled boolean NOT NULL DEFAULT false,
    preview_exclude_drafts boolean NOT NULL DEFAULT false,
    preview_cancel_obsolete_builds boolean NOT NULL DEFAULT false,
    rollback_on_degraded_health boolean NOT NULL DEFAULT false,
    bake_time_seconds integer CHECK (bake_time_seconds > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE build_configs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    application_id bigint NOT NULL UNIQUE REFERENCES applications (id) ON DELETE CASCADE,
    build_pack build_pack NOT NULL DEFAULT 'nixpacks',
    install_command text,
    build_command text,
    start_command text,
    publish_directory text,
    is_spa boolean NOT NULL DEFAULT false,
    custom_nginx_config text,
    dockerfile_path text,
    dockerfile_content text,
    auto_inject_build_args boolean NOT NULL DEFAULT true,
    inject_source_commit boolean NOT NULL DEFAULT false,
    compose_file_path text,
    raw_compose boolean NOT NULL DEFAULT false,
    image_name text,
    image_tag text,
    registry_credential_id bigint,
    push_enabled boolean NOT NULL DEFAULT false,
    push_image_name text,
    push_image_tag text,
    push_tag_with_commit_sha boolean NOT NULL DEFAULT false,
    push_registry_credential_id bigint,
    use_build_server boolean NOT NULL DEFAULT false,
    use_build_secrets boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE runtime_configs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    application_id bigint NOT NULL UNIQUE REFERENCES applications (id) ON DELETE CASCADE,
    ports_exposes text,
    ports_mappings jsonb,
    custom_docker_options text,
    custom_labels text,
    pre_deployment_command text,
    post_deployment_command text,
    stop_grace_period_seconds integer NOT NULL DEFAULT 10 CHECK (stop_grace_period_seconds >= 0),
    restart_limit integer CHECK (restart_limit > 0),
    memory_limit text,
    memory_reservation text,
    memory_swap text,
    memory_swappiness integer CHECK (memory_swappiness BETWEEN 0 AND 100),
    cpu_limit numeric(6, 2) CHECK (cpu_limit > 0),
    cpu_sets text,
    cpu_shares integer CHECK (cpu_shares > 0),
    force_https boolean NOT NULL DEFAULT true,
    redirect_direction redirect_direction NOT NULL DEFAULT 'both',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE deployments (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    resource_id bigint NOT NULL REFERENCES resources (id) ON DELETE CASCADE,
    status deployment_status NOT NULL DEFAULT 'queued',
    attempt integer NOT NULL DEFAULT 1,
    retry_of_id bigint REFERENCES deployments (id) ON DELETE SET NULL,
    superseded_by_id bigint REFERENCES deployments (id) ON DELETE SET NULL,
    is_rollback boolean NOT NULL DEFAULT false,
    trigger deployment_trigger NOT NULL,
    triggered_by bigint REFERENCES users (id) ON DELETE SET NULL,
    api_token_id bigint REFERENCES api_tokens (id) ON DELETE SET NULL,
    git_branch text,
    commit_sha text,
    is_local_source boolean NOT NULL DEFAULT false,
    context_digest text,
    force_rebuild boolean NOT NULL DEFAULT false,
    image_name text,
    image_tag text,
    image_digest text,
    config_snapshot jsonb,
    config_diff jsonb,
    error_message text,
    server_id bigint NOT NULL REFERENCES servers (id) ON DELETE RESTRICT,
    build_server_id bigint REFERENCES servers (id) ON DELETE SET NULL,
    queued_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX deployments_resource_id_id_idx ON deployments (resource_id, id DESC);
CREATE INDEX deployments_server_queue_idx ON deployments (server_id, created_at)
    WHERE status NOT IN ('succeeded', 'failed', 'cancelled', 'superseded');

CREATE TABLE deployment_steps (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    deployment_id bigint NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,
    seq integer NOT NULL,
    name text NOT NULL,
    status deployment_step_status NOT NULL DEFAULT 'pending',
    exit_code integer,
    log text,
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (deployment_id, seq)
);

ALTER TABLE jobs ADD CONSTRAINT jobs_resource_id_fkey
    FOREIGN KEY (resource_id) REFERENCES resources (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE jobs DROP CONSTRAINT jobs_resource_id_fkey;
-- The referenced resources disappear with this migration: clear the
-- dangling references so the FK can be re-created on a later up.
UPDATE jobs SET resource_id = NULL WHERE resource_id IS NOT NULL;
DROP TABLE deployment_steps;
DROP TABLE deployments;
DROP TABLE runtime_configs;
DROP TABLE build_configs;
DROP TABLE applications;
DROP TRIGGER resources_team_consistency_trg ON resources;
DROP FUNCTION resources_team_consistency();
DROP TABLE resources;
DROP TYPE preview_protection;
DROP TYPE deployment_trigger;
DROP TYPE deployment_step_status;
DROP TYPE deployment_status;
DROP TYPE redirect_direction;
DROP TYPE build_pack;
DROP TYPE resource_desired_status;
DROP TYPE resource_type;
