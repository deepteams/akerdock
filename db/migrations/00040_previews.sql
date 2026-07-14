-- PR previews (data-dictionary §8.9, PRD §20.4, ADR-011).
--
-- The identity (application, provider, pr_id) is deterministic and NEVER
-- recycled for another application; previews.uuid is the base of every
-- Docker name of the preview instance (INV-011). Forks are flagged and
-- deploy nothing until a maintainer approves (INV-010).

-- +goose Up
CREATE TYPE preview_status AS ENUM ('queued', 'deploying', 'active', 'failed', 'destroying', 'cleanup_failed', 'destroyed');

CREATE TABLE previews (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    application_id bigint NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    provider git_provider NOT NULL,
    pr_id integer NOT NULL,
    source_branch text,
    head_sha text,
    is_fork boolean NOT NULL DEFAULT false,
    fork_approved_by bigint REFERENCES users (id) ON DELETE SET NULL,
    fork_approved_at timestamptz,
    fqdn citext,
    status preview_status NOT NULL DEFAULT 'queued',
    cleanup_error text,
    last_deployed_at timestamptz,
    last_activity_at timestamptz,
    destroyed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (application_id, provider, pr_id)
);

CREATE INDEX previews_live_idx ON previews (application_id) WHERE status NOT IN ('destroyed');
CREATE INDEX previews_activity_idx ON previews (last_activity_at);

ALTER TABLE deployments
    ADD COLUMN preview_id bigint REFERENCES previews (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE deployments DROP COLUMN preview_id;
DROP TABLE previews;
DROP TYPE preview_status;
