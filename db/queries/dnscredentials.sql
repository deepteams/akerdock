-- DNS-01 credentials (proxy-contract §7.2). The config is never selected back
-- out to the API: it is decrypted only to be materialized on the server.

-- name: CreateDNSCredential :one
INSERT INTO cloud_credentials (uuid, team_id, name, provider, config_enc, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetDNSCredentialByUUID :one
SELECT * FROM cloud_credentials WHERE uuid = $1 AND team_id = $2 AND deleted_at IS NULL;

-- name: GetDNSCredentialByID :one
SELECT * FROM cloud_credentials WHERE id = $1 AND deleted_at IS NULL;

-- name: ListDNSCredentialsPage :many
SELECT * FROM cloud_credentials
WHERE team_id = sqlc.arg(team_id) AND deleted_at IS NULL
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: SoftDeleteDNSCredential :execrows
UPDATE cloud_credentials SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: CountDNSCredentialUsage :one
SELECT count(*) FROM servers WHERE dns_credential_id = $1 AND deleted_at IS NULL;
