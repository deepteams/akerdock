-- OAuth/OIDC dashboard login (PRD §10.2, §23.3): provider credentials,
-- in-flight login states, and the identities an account is linked to.

-- name: ListOauthProviderConfigs :many
SELECT * FROM oauth_provider_configs ORDER BY provider;

-- name: ListEnabledOauthProviderConfigs :many
-- What the sign-in page shows: enabled providers only, and nothing secret.
SELECT provider, display_name FROM oauth_provider_configs
WHERE enabled ORDER BY provider;

-- name: GetOauthProviderConfig :one
SELECT * FROM oauth_provider_configs WHERE provider = $1;

-- name: UpsertOauthProviderConfig :one
-- Full replacement, secret included: the API never reads the secret back, so
-- there is nothing to "keep" on update — the caller re-provides it.
INSERT INTO oauth_provider_configs
    (uuid, provider, display_name, client_id, client_secret_enc, issuer_url, enabled)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (provider) DO UPDATE
SET uuid = EXCLUDED.uuid,
    display_name = EXCLUDED.display_name,
    client_id = EXCLUDED.client_id,
    client_secret_enc = EXCLUDED.client_secret_enc,
    issuer_url = EXCLUDED.issuer_url,
    enabled = EXCLUDED.enabled,
    updated_at = now()
RETURNING *;

-- name: DeleteOauthProviderConfig :execrows
DELETE FROM oauth_provider_configs WHERE provider = $1;

-- name: CreateOauthLoginState :exec
INSERT INTO oauth_login_states (state_hash, provider, purpose, user_id, pkce_verifier, nonce, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ConsumeOauthLoginState :one
-- DELETE ... RETURNING: the state is single-use by construction — a replayed
-- callback finds nothing, whatever the caller does. Provider and purpose are
-- part of the lookup, not hints: a GitHub login state must not complete a
-- Google callback, nor a login state a link.
DELETE FROM oauth_login_states
WHERE state_hash = $1 AND provider = $2 AND purpose = $3 AND expires_at > now()
RETURNING *;

-- name: PurgeExpiredOauthLoginStates :execrows
DELETE FROM oauth_login_states WHERE expires_at < now();

-- name: GetIdentity :one
SELECT * FROM identities WHERE provider = $1 AND provider_subject = $2;

-- name: CreateIdentity :one
INSERT INTO identities (user_id, provider, provider_subject, email)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListIdentitiesForUser :many
SELECT * FROM identities WHERE user_id = $1 ORDER BY provider;

-- name: DeleteIdentityForUser :execrows
-- Scoped by user: a session must never unlink someone else's identity.
DELETE FROM identities WHERE uuid = $1 AND user_id = $2;

-- name: CountCredentialsForUser :one
-- How many ways this user can still sign in: a password, federated
-- identities, passkeys. Unlinking the LAST one would lock the account out
-- silently — the caller refuses when this reaches one.
SELECT (CASE WHEN u.password_hash IS NOT NULL THEN 1 ELSE 0 END)
     + (SELECT count(*) FROM identities i WHERE i.user_id = u.id)
     + (SELECT count(*) FROM passkey_credentials p WHERE p.user_id = u.id)
  AS credentials
FROM users u WHERE u.id = $1;

-- name: GetUserByEmailIncludingDeleted :one
-- The collision check of §23.3 must also see soft-deleted accounts: a
-- tombstoned email must not be resurrectable by whoever registers it at an
-- identity provider.
SELECT * FROM users WHERE email = $1;
