-- Passkeys (WebAuthn) for the dashboard login.

-- name: CreatePasskeyCredential :one
INSERT INTO passkey_credentials (user_id, name, credential_id, credential)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListPasskeysForUser :many
SELECT * FROM passkey_credentials WHERE user_id = $1 ORDER BY created_at;

-- name: GetPasskeyByCredentialID :one
-- The login ceremony starts from the credential: the authenticator presents a
-- credential id, and the user is whoever enrolled it. A deleted user's
-- passkeys must not open sessions, hence the join.
SELECT p.*, u.uuid AS user_uuid, u.email, u.name AS user_name
FROM passkey_credentials p
JOIN users u ON u.id = p.user_id
WHERE p.credential_id = $1 AND u.deleted_at IS NULL;

-- name: UpdatePasskeyCredential :exec
-- Called after every successful assertion: the sign counter moved, and the
-- clone-detection logic downstream depends on it being persisted.
UPDATE passkey_credentials SET credential = $2, last_used_at = now() WHERE id = $1;

-- name: DeletePasskeyForUser :execrows
-- Scoped by user: a session must never be able to delete someone else's key.
DELETE FROM passkey_credentials WHERE uuid = $1 AND user_id = $2;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1 AND deleted_at IS NULL;

-- name: CreatePasskeyCeremony :exec
INSERT INTO passkey_ceremonies (token_hash, purpose, user_id, data, expires_at)
VALUES ($1, $2, $3, $4, $5);

-- name: ConsumePasskeyCeremony :one
-- DELETE ... RETURNING makes the ceremony single-use by construction: two
-- concurrent finishes cannot both win, whatever the caller does.
DELETE FROM passkey_ceremonies
WHERE token_hash = $1 AND purpose = $2 AND expires_at > now()
RETURNING *;

-- name: PurgeExpiredPasskeyCeremonies :execrows
DELETE FROM passkey_ceremonies WHERE expires_at < now();
