-- External endpoints: declared bastion targets outside the server (ADR-045).
--
-- The address is a property of a DECLARED resource, never of a request: the
-- mint names an endpoint and the tunnel protocol stays addressless, which is
-- what keeps a `write` holder from turning the tunnel into a port scanner.
-- `host` + `port` are an exact pair — no CIDR, no range, no wildcard — and
-- `server_id` is the egress server the dial leaves from (an endpoint only means
-- something relative to the vantage point that reaches it).
--
-- `criticality` is the single per-endpoint dimension governing the access
-- regime (ADR-045 §6): `standard` behaves exactly like an ADR-032 tunnel,
-- `sensitive` (the default) requires an access grant obtained in the dashboard
-- behind a fresh second factor.
--
-- A note on the session/target constraint below: the ADR calls for "exactly one
-- target kind". A CHECK can only enforce "at most one", because both columns
-- are ON DELETE SET NULL and an old session whose resource was deleted
-- legitimately has neither. Creation-time exactness lives in the handler and is
-- unit-tested; the CHECK guards the invariant that cannot be repaired later —
-- a row pointing at two targets at once.

-- +goose Up
CREATE TYPE external_endpoint_criticality AS ENUM ('standard', 'sensitive');

CREATE TABLE external_endpoints (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    name text NOT NULL CHECK (btrim(name) <> ''),
    description text,
    -- A single host, syntactically: no scheme, no path, no space, no comma.
    -- The exact-pair rule is what bounds this feature.
    host text NOT NULL CHECK (btrim(host) <> '' AND host !~ '[\s/,:]'),
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    server_id bigint NOT NULL REFERENCES servers (id) ON DELETE RESTRICT,
    -- Optional RBAC scope (ADR-038): a production replica can be restricted to
    -- the people who already hold rights on that project/environment.
    project_id bigint REFERENCES projects (id) ON DELETE CASCADE,
    environment_id bigint REFERENCES environments (id) ON DELETE CASCADE,
    criticality external_endpoint_criticality NOT NULL DEFAULT 'sensitive',
    -- Minutes rather than an interval: the API speaks minutes and the 8 h cap
    -- (ADR-045 §5) is expressible as a plain CHECK.
    max_grant_minutes integer NOT NULL DEFAULT 240 CHECK (max_grant_minutes BETWEEN 1 AND 480),
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    version integer NOT NULL DEFAULT 1
);

CREATE UNIQUE INDEX external_endpoints_team_name_idx ON external_endpoints (team_id, lower(name));
CREATE INDEX external_endpoints_server_idx ON external_endpoints (server_id);

-- Access grants (ADR-045 §5): a bounded, reasoned, re-authenticated window
-- during which the holder may mint tunnels to this endpoint. The grant is the
-- session deadline, not merely a permission to start one.
CREATE TABLE external_endpoint_grants (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    endpoint_id bigint NOT NULL REFERENCES external_endpoints (id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    reason text NOT NULL CHECK (btrim(reason) <> ''),
    -- Which factor was actually consumed, for the audit trail. The server picks
    -- it (strongest the user holds); the client never chooses.
    factor text NOT NULL CHECK (factor IN ('passkey', 'totp')),
    -- Stored apart from user_id from the start so that third-party approval is
    -- a later feature rather than a migration (ADR-045 §5, self-service in v1).
    granted_by bigint REFERENCES users (id) ON DELETE SET NULL,
    -- A renewal chains to the grant it extended: renewals are unbounded in
    -- total but each one costs a fresh factor, and the chain must stay visible.
    renewed_from bigint REFERENCES external_endpoint_grants (id) ON DELETE SET NULL,
    requested_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    revoked_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > requested_at)
);

-- The hot path: "does this user hold a live grant on this endpoint right now".
CREATE INDEX external_endpoint_grants_live_idx
    ON external_endpoint_grants (endpoint_id, user_id, expires_at)
    WHERE revoked_at IS NULL;

-- Port-forward sessions can now target an external endpoint instead of a
-- container, and carry the instant they are actually cut (`authorized_until`,
-- announced to the CLI at open and quoted again in the expiry warning).
ALTER TABLE port_forward_sessions
    ADD COLUMN external_endpoint_id bigint REFERENCES external_endpoints (id) ON DELETE SET NULL;
ALTER TABLE port_forward_sessions
    ADD COLUMN grant_id bigint REFERENCES external_endpoint_grants (id) ON DELETE SET NULL;
ALTER TABLE port_forward_sessions
    ADD COLUMN authorized_until timestamptz;
ALTER TABLE port_forward_sessions
    ADD CONSTRAINT port_forward_sessions_one_target
    CHECK (NOT (resource_id IS NOT NULL AND external_endpoint_id IS NOT NULL));

CREATE INDEX port_forward_sessions_endpoint_idx
    ON port_forward_sessions (external_endpoint_id);
CREATE INDEX port_forward_sessions_grant_idx
    ON port_forward_sessions (grant_id) WHERE grant_id IS NOT NULL;

-- A tunnel cut because its grant ran out is neither an idle timeout nor a
-- revocation, and the developer is told exactly that. Added last: PostgreSQL
-- forbids using a new enum value in the transaction that adds it, and nothing
-- above uses it.
ALTER TYPE terminal_end_reason ADD VALUE IF NOT EXISTS 'grant_expired';

-- +goose Down
DROP INDEX port_forward_sessions_grant_idx;
DROP INDEX port_forward_sessions_endpoint_idx;
ALTER TABLE port_forward_sessions DROP CONSTRAINT port_forward_sessions_one_target;
ALTER TABLE port_forward_sessions DROP COLUMN authorized_until;
ALTER TABLE port_forward_sessions DROP COLUMN grant_id;
ALTER TABLE port_forward_sessions DROP COLUMN external_endpoint_id;
DROP TABLE external_endpoint_grants;
DROP TABLE external_endpoints;
DROP TYPE external_endpoint_criticality;
