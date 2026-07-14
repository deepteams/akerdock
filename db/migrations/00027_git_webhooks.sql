-- Incoming Git webhooks (data-dictionary §7.4/§7.5, spec git-webhook-protocols).
-- Signed by the provider, not by a Bearer token: the endpoint UUID names the
-- target without revealing it, and the signature authenticates the caller.

-- +goose Up
CREATE TYPE webhook_provider AS ENUM ('github', 'gitlab', 'bitbucket', 'gitea', 'generic');
CREATE TYPE webhook_delivery_status AS ENUM ('received', 'accepted', 'ignored', 'duplicate', 'failed');

CREATE TABLE webhook_endpoints (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    application_id bigint NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    provider webhook_provider NOT NULL,
    -- HMAC secret, envelope-encrypted (§23.2). Returned to the operator exactly
    -- once, at creation: it must be pasted into the provider's UI.
    secret_enc bytea NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (application_id, provider)
);

CREATE TABLE webhook_deliveries (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    provider webhook_provider NOT NULL,
    delivery_id text NOT NULL,
    webhook_endpoint_id bigint REFERENCES webhook_endpoints (id) ON DELETE SET NULL,
    event_type text,
    signature_valid boolean NOT NULL DEFAULT false,
    status webhook_delivery_status NOT NULL DEFAULT 'received',
    ignore_reason text,
    payload jsonb,
    team_id bigint REFERENCES teams (id) ON DELETE SET NULL,
    application_id bigint REFERENCES applications (id) ON DELETE SET NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- No provider signs a timestamp, so replay protection rests entirely on
    -- this uniqueness (INV-009): the same delivery can never deploy twice.
    UNIQUE (provider, delivery_id)
);

CREATE INDEX webhook_deliveries_received_idx ON webhook_deliveries (received_at DESC);
CREATE INDEX webhook_deliveries_team_idx ON webhook_deliveries (team_id);

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE webhook_endpoints;
DROP TYPE webhook_delivery_status;
DROP TYPE webhook_provider;
