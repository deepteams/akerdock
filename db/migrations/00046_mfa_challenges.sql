-- MFA login challenges (PRD §10.2: 2FA TOTP; §23.3: recovery codes and
-- anti-bruteforce).
--
-- The mfa_factors table has existed since 00001; what was missing is the
-- half-open state of a two-step login: the password was right, the TOTP code
-- has not been presented yet. That state must NOT be a session — a session
-- row, however flagged, is one forgotten WHERE clause away from being
-- accepted. It is its own short-lived row instead, exactly like
-- passkey_ceremonies (00036) and for the same reason: it lives in the
-- database so the second step can land on another API replica (ADR-021),
-- and only the HASH of the challenge token is stored — a database dump must
-- not hand out half-completed logins.

-- +goose Up
CREATE TABLE mfa_challenges (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- SHA-256 of the token the client echoes with its TOTP code.
    token_hash text NOT NULL UNIQUE,
    -- The user whose password was already verified. The challenge dies with
    -- the account.
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE mfa_challenges;
