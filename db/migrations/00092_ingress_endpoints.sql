-- Ingress endpoints: declared public URLs relayed to a developer's machine
-- (ADR-060). The mirror of external_endpoints (ADR-045): there the platform
-- dials OUT to a declared host:port, here the platform accepts visitors on a
-- declared FQDN and relays them to whoever holds the attach socket.
--
-- The hostname is a property of a DECLARED resource, never of a session — no
-- random per-session hostnames (certificate churn, unregisterable webhook
-- URLs). The FQDN is registered in `domains`, so the instance-wide
-- (fqdn, path) uniqueness makes a collision with an application, a compose
-- component or another endpoint impossible by construction (INV-002).
--
-- `access` reuses the ADR-042 wall vocabulary with `sso` as the default: a
-- fresh endpoint is reachable by the team's authenticated users and nobody
-- else. `none` (the webhook case) is a conscious admin-level opt-out. noindex
-- and force-HTTPS are unconditional and therefore not columns.

-- +goose Up
CREATE TYPE ingress_access AS ENUM ('sso', 'basic_auth', 'none');

CREATE TABLE ingress_endpoints (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    name text NOT NULL CHECK (btrim(name) <> ''),
    description text,
    -- Same syntactic shape as domains.fqdn: an exact hostname, no wildcard.
    fqdn citext NOT NULL CHECK (fqdn ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$'),
    -- The ingress server: the vantage point whose Traefik terminates the
    -- hostname (ADR-060 §1). Mirror of the egress server; equally mandatory.
    server_id bigint NOT NULL REFERENCES servers (id) ON DELETE RESTRICT,
    access ingress_access NOT NULL DEFAULT 'sso',
    -- bcrypt hash when access = 'basic_auth' (never the clear credential).
    basic_auth_hash text,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    version integer NOT NULL DEFAULT 1,
    CHECK (access <> 'basic_auth' OR basic_auth_hash IS NOT NULL)
);

CREATE UNIQUE INDEX ingress_endpoints_team_name_idx ON ingress_endpoints (team_id, lower(name));
CREATE INDEX ingress_endpoints_server_idx ON ingress_endpoints (server_id);

-- Register the FQDN in the routing namespace. The exactly-one owner CHECK
-- widens from two owner kinds to three; `domains_check` is the auto-assigned
-- name of the original table-level CHECK (00009).
ALTER TABLE domains ADD COLUMN ingress_endpoint_id bigint REFERENCES ingress_endpoints (id) ON DELETE CASCADE;
ALTER TABLE domains DROP CONSTRAINT domains_check;
ALTER TABLE domains ADD CONSTRAINT domains_one_owner CHECK (
    (application_id IS NOT NULL)::int
    + (service_component_id IS NOT NULL)::int
    + (ingress_endpoint_id IS NOT NULL)::int = 1
);
CREATE INDEX domains_ingress_endpoint_id_idx ON domains (ingress_endpoint_id);

-- Attach sessions (ADR-060 §3/§6). Mirrors port_forward_sessions' mint/claim
-- discipline, but the socket lives on the ingress server's agent: liveness is
-- agent-reported (last_seen_at), never a control-plane heartbeat.
--
-- endpoint_id is SET NULL so a session outlives its endpoint in the audit
-- trail; the live socket is cut at deletion through the command channel.
CREATE TABLE ingress_tunnel_sessions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    endpoint_id bigint REFERENCES ingress_endpoints (id) ON DELETE SET NULL,
    user_id bigint REFERENCES users (id) ON DELETE SET NULL,
    client_ip inet,
    token_hash text NOT NULL UNIQUE,
    token_expires_at timestamptz NOT NULL,
    claimed_at timestamptz,
    started_at timestamptz,
    last_seen_at timestamptz,
    ended_at timestamptz,
    end_reason terminal_end_reason,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Exclusive occupancy by construction (ADR-060 §6): at most one non-ended
-- session per endpoint — a pending mint occupies too, until claimed, expired
-- (finalized by the mint path or the sweep) or ended.
CREATE UNIQUE INDEX ingress_tunnel_sessions_occupancy_idx
    ON ingress_tunnel_sessions (endpoint_id) WHERE ended_at IS NULL;
CREATE INDEX ingress_tunnel_sessions_team_idx ON ingress_tunnel_sessions (team_id, id DESC);

-- The SSO wall reuses the ADR-030/042 access-token machinery verbatim
-- (ADR-060 §5): an access token may now be scoped to an ingress endpoint,
-- and to nothing else at the same time.
ALTER TABLE preview_access_tokens DROP CONSTRAINT preview_access_tokens_one_target;
ALTER TABLE preview_access_tokens
    ADD COLUMN ingress_endpoint_id bigint REFERENCES ingress_endpoints (id) ON DELETE CASCADE;
ALTER TABLE preview_access_tokens
    ADD CONSTRAINT preview_access_tokens_one_target CHECK (
        (ingress_endpoint_id IS NOT NULL AND preview_id IS NULL AND application_id IS NULL AND resource_id IS NULL)
        OR (
            ingress_endpoint_id IS NULL
            AND (
                (preview_id IS NOT NULL AND application_id IS NULL AND resource_id IS NULL)
                OR (
                    preview_id IS NULL
                    AND (application_id IS NOT NULL OR resource_id IS NOT NULL)
                    AND (application_id IS NULL OR resource_id IS NULL OR application_id = resource_id)
                )
            )
        )
    );
CREATE INDEX preview_access_tokens_ingress_idx ON preview_access_tokens (ingress_endpoint_id);

-- +goose Down
ALTER TABLE preview_access_tokens DROP CONSTRAINT preview_access_tokens_one_target;
DELETE FROM preview_access_tokens WHERE ingress_endpoint_id IS NOT NULL;
ALTER TABLE preview_access_tokens DROP COLUMN ingress_endpoint_id;
ALTER TABLE preview_access_tokens
    ADD CONSTRAINT preview_access_tokens_one_target CHECK (
        (preview_id IS NOT NULL AND application_id IS NULL AND resource_id IS NULL)
        OR (
            preview_id IS NULL
            AND (application_id IS NOT NULL OR resource_id IS NOT NULL)
            AND (application_id IS NULL OR resource_id IS NULL OR application_id = resource_id)
        )
    );
DROP TABLE ingress_tunnel_sessions;
ALTER TABLE domains DROP CONSTRAINT domains_one_owner;
ALTER TABLE domains DROP COLUMN ingress_endpoint_id;
ALTER TABLE domains ADD CONSTRAINT domains_check CHECK (
    (application_id IS NOT NULL)::int + (service_component_id IS NOT NULL)::int = 1
);
DROP TABLE ingress_endpoints;
DROP TYPE ingress_access;
