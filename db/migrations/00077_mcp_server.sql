-- ADR-043: built-in MCP server. `mcp_enabled` gates the whole surface (off by
-- default, PRD §12). The OAuth 2.1 side for remote clients needs three short
-- tables: dynamically registered clients, single-use authorization codes with
-- their PKCE challenge, and opaque access tokens stored hashed like every
-- other credential. All of it is read-only by construction — a grant carries
-- no permission beyond reading one team's inventory. Expand-only.

-- +goose Up
ALTER TABLE instance_settings ADD COLUMN mcp_enabled boolean NOT NULL DEFAULT false;

CREATE TABLE mcp_oauth_clients (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    client_id text NOT NULL UNIQUE,
    client_name text NOT NULL,
    redirect_uris text[] NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE mcp_oauth_codes (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code_hash text NOT NULL UNIQUE,
    client_id text NOT NULL,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    redirect_uri text NOT NULL,
    code_challenge text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE mcp_access_tokens (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    token_hash text NOT NULL UNIQUE,
    client_id text NOT NULL,
    client_name text NOT NULL,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    last_used_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX mcp_access_tokens_team_idx ON mcp_access_tokens (team_id);
CREATE INDEX mcp_access_tokens_user_idx ON mcp_access_tokens (user_id);

-- +goose Down
DROP TABLE mcp_access_tokens;
DROP TABLE mcp_oauth_codes;
DROP TABLE mcp_oauth_clients;
ALTER TABLE instance_settings DROP COLUMN mcp_enabled;
