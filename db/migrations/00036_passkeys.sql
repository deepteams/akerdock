-- Passkeys (WebAuthn) for the dashboard login (PRD §698 hardening).
--
-- Two tables, because a ceremony and a credential have opposite lifecycles:
--
--   * passkey_credentials is the durable outcome: one row per authenticator a
--     user enrolled. The credential's public key is PUBLIC by construction —
--     nothing here is a secret, so nothing is envelope-encrypted. The whole
--     library credential is kept as jsonb: the verification library owns that
--     shape, and re-projecting it column by column would be a second copy of
--     its schema that could silently drift.
--
--   * passkey_ceremonies is the five-minute half-open state between "begin"
--     and "finish". It lives in the database, not in process memory, so a
--     ceremony survives an API replica change (ADR-021 runs several `api`
--     instances behind one address). Only the HASH of the ceremony token is
--     stored: a database dump must not hand out live login challenges.

-- +goose Up
CREATE TABLE passkey_credentials (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Human label ("YubiKey 5", "MacBook Touch ID"): the user must be able to
    -- tell which one to revoke when a device is lost.
    name text NOT NULL,
    -- The authenticator's credential id, unique ACROSS USERS: the login
    -- ceremony starts from the credential alone, before any user is known.
    credential_id bytea NOT NULL UNIQUE,
    -- The full webauthn.Credential (public key, sign count, flags, transports)
    -- as the verification library serializes it.
    credential jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);

CREATE INDEX passkey_credentials_user_idx ON passkey_credentials (user_id);

CREATE TABLE passkey_ceremonies (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- SHA-256 of the token the client echoes on finish; never the token itself.
    token_hash text NOT NULL UNIQUE,
    -- 'registration' or 'login': a registration ceremony must not be replayable
    -- as a login one, so the purpose is part of the lookup, not a hint.
    purpose text NOT NULL,
    -- Set for registration (the enrolling user is known); NULL for a
    -- discoverable login, where the authenticator names the user.
    user_id bigint REFERENCES users (id) ON DELETE CASCADE,
    -- The library's SessionData: challenge, allowed credentials, flags.
    data jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE passkey_ceremonies;
DROP TABLE passkey_credentials;
