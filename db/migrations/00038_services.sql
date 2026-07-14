-- Docker Compose stacks (compose-spec.md, data-dictionary §9.1–9.2).
--
-- services extends resources 1—1 (resource_type = 'service'): a standalone
-- compose stack whose file is the source of truth, edited in the UI.
--
-- service_components are the sub-containers of a stack — one per compose
-- service, resynchronized at every edit of the file. They hang off RESOURCES,
-- not services: an application built with the compose build pack (PRD §5.2,
-- "domaine par service") carries components too, and both extensions share
-- the resources identity (data-dictionary §9.2, amended with this migration).
--
-- domains.service_component_id and database_backup_plans.service_component_id
-- were created without their FK ("lands with the compose work") — this is
-- that work.

-- +goose Up
CREATE TABLE services (
    id bigint PRIMARY KEY REFERENCES resources (id) ON DELETE CASCADE,
    compose_content text NOT NULL,
    template_slug text,
    template_version text,
    template_repository text,
    connect_to_predefined_network boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE service_components (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    resource_id bigint NOT NULL REFERENCES resources (id) ON DELETE CASCADE,
    -- Compose service name, validated at parse time too (compose-spec §2.2).
    name text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9_.-]*$'),
    image text,
    -- Image-based database detection (compose-spec §10): makes the component
    -- a valid target of a database_backup_plan.
    is_database boolean NOT NULL DEFAULT false,
    database_engine db_engine,
    -- One-shot jobs opt out of the aggregated stack health (compose-spec §7.3).
    exclude_from_hc boolean NOT NULL DEFAULT false,
    -- Default routing port (compose-spec §6): first `expose` of the service,
    -- resolved at validation time — the proxy renderer cannot re-read the file.
    default_route_port integer CHECK (default_route_port BETWEEN 1 AND 65535),
    observed_status resource_observed_status NOT NULL DEFAULT 'unknown',
    observed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (resource_id, name)
);

CREATE INDEX service_components_resource_id_idx ON service_components (resource_id);

ALTER TABLE domains
    ADD CONSTRAINT domains_service_component_id_fkey
    FOREIGN KEY (service_component_id) REFERENCES service_components (id) ON DELETE CASCADE;
CREATE INDEX domains_service_component_id_idx ON domains (service_component_id);

ALTER TABLE database_backup_plans
    ADD CONSTRAINT database_backup_plans_service_component_id_fkey
    FOREIGN KEY (service_component_id) REFERENCES service_components (id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE database_backup_plans DROP CONSTRAINT database_backup_plans_service_component_id_fkey;
ALTER TABLE domains DROP CONSTRAINT domains_service_component_id_fkey;
DROP INDEX domains_service_component_id_idx;
DROP TABLE service_components;
DROP TABLE services;
