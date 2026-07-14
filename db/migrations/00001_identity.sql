-- Identity aggregate + instance settings (data-dictionary §4, §6.3, §11.7).
-- Conventions per data-dictionary §2: bigint identity PK, public uuid,
-- created_at/updated_at, optimistic-lock version, tombstone deleted_at.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TYPE team_role AS ENUM ('owner', 'admin', 'member');
CREATE TYPE oauth_provider AS ENUM ('github', 'gitlab', 'google', 'azure', 'bitbucket', 'oidc');
CREATE TYPE mfa_type AS ENUM ('totp');

CREATE TABLE users (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    email citext NOT NULL,
    name text NOT NULL,
    password_hash text,
    is_root boolean NOT NULL DEFAULT false,
    email_verified_at timestamptz,
    failed_login_count integer NOT NULL DEFAULT 0,
    locked_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    version integer NOT NULL DEFAULT 1
);

-- Email reusable after tombstone (ERD §11).
CREATE UNIQUE INDEX users_email_key ON users (email) WHERE deleted_at IS NULL;
CREATE INDEX users_is_root_idx ON users (is_root) WHERE is_root;

CREATE TABLE identities (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider oauth_provider NOT NULL,
    provider_subject text NOT NULL,
    email citext,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_subject)
);

CREATE INDEX identities_user_id_idx ON identities (user_id);

CREATE TABLE mfa_factors (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type mfa_type NOT NULL DEFAULT 'totp',
    secret_enc bytea NOT NULL,
    recovery_code_hashes text[] NOT NULL DEFAULT '{}',
    confirmed_at timestamptz,
    last_used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, type)
);

CREATE TABLE teams (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    name text NOT NULL,
    description text,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    version integer NOT NULL DEFAULT 1
);

CREATE TABLE sessions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash text NOT NULL UNIQUE,
    current_team_id bigint REFERENCES teams (id) ON DELETE SET NULL,
    mfa_verified_at timestamptz,
    ip inet,
    user_agent text,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE TABLE team_memberships (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role team_role NOT NULL DEFAULT 'member',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (team_id, user_id)
);

CREATE INDEX team_memberships_user_id_idx ON team_memberships (user_id);

CREATE TABLE invitations (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    email citext NOT NULL,
    role team_role NOT NULL DEFAULT 'member' CHECK (role <> 'owner'),
    token_hash text NOT NULL UNIQUE,
    invited_by bigint REFERENCES users (id) ON DELETE SET NULL,
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One active invitation per (team, email) at a time (ERD §11).
CREATE UNIQUE INDEX invitations_active_key ON invitations (team_id, email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE TABLE api_tokens (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    name text NOT NULL,
    token_prefix text NOT NULL,
    token_hash text NOT NULL UNIQUE,
    permissions text[] NOT NULL DEFAULT '{read}'
        CHECK (permissions <@ ARRAY['read', 'read:sensitive', 'write', 'deploy', 'root']::text[]),
    ip_allowlist cidr[],
    expires_at timestamptz,
    last_used_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX api_tokens_team_id_id_idx ON api_tokens (team_id, id DESC);
CREATE INDEX api_tokens_token_prefix_idx ON api_tokens (token_prefix);

CREATE TABLE private_keys (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE RESTRICT,
    name text NOT NULL,
    description text,
    fingerprint_sha256 text NOT NULL,
    public_key text NOT NULL,
    private_key_enc bytea NOT NULL,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    version integer NOT NULL DEFAULT 1,
    UNIQUE (team_id, fingerprint_sha256)
);

CREATE INDEX private_keys_team_id_id_idx ON private_keys (team_id, id DESC);

CREATE TABLE instance_settings (
    id smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    fqdn text,
    timezone text NOT NULL DEFAULT 'UTC',
    registration_enabled boolean NOT NULL DEFAULT false,
    api_enabled boolean NOT NULL DEFAULT false,
    dns_validation_server text NOT NULL DEFAULT '1.1.1.1',
    transactional_email_config_enc bytea,
    auto_update_enabled boolean NOT NULL DEFAULT true,
    auto_update_cron text,
    onboarding_completed_at timestamptz,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    version integer NOT NULL DEFAULT 1
);

-- +goose Down
DROP TABLE instance_settings;
DROP TABLE private_keys;
DROP TABLE api_tokens;
DROP TABLE invitations;
DROP TABLE team_memberships;
DROP TABLE sessions;
DROP TABLE teams;
DROP TABLE mfa_factors;
DROP TABLE identities;
DROP TABLE users;
DROP TYPE mfa_type;
DROP TYPE oauth_provider;
DROP TYPE team_role;
