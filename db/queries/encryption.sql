-- Envelope-encryption inventory (ADR-003, data-dictionary §12). The first
-- 4 bytes of every *_enc column carry the key version that encrypted it.

-- name: EncryptionKeyVersionHistogram :many
SELECT 'private_keys.private_key_enc' AS column_name,
       (get_byte(private_key_enc, 0) << 24 | get_byte(private_key_enc, 1) << 16 | get_byte(private_key_enc, 2) << 8 | get_byte(private_key_enc, 3)) AS key_version,
       count(*) AS rows
FROM private_keys GROUP BY 1, 2
UNION ALL
SELECT 'environment_variables.value_enc',
       (get_byte(value_enc, 0) << 24 | get_byte(value_enc, 1) << 16 | get_byte(value_enc, 2) << 8 | get_byte(value_enc, 3)),
       count(*)
FROM environment_variables GROUP BY 1, 2
UNION ALL
SELECT 'database_credentials.password_enc',
       (get_byte(password_enc, 0) << 24 | get_byte(password_enc, 1) << 16 | get_byte(password_enc, 2) << 8 | get_byte(password_enc, 3)),
       count(*)
FROM database_credentials GROUP BY 1, 2
UNION ALL
SELECT 's3_storages.access_key_enc',
       (get_byte(access_key_enc, 0) << 24 | get_byte(access_key_enc, 1) << 16 | get_byte(access_key_enc, 2) << 8 | get_byte(access_key_enc, 3)),
       count(*)
FROM s3_storages GROUP BY 1, 2
UNION ALL
SELECT 's3_storages.secret_key_enc',
       (get_byte(secret_key_enc, 0) << 24 | get_byte(secret_key_enc, 1) << 16 | get_byte(secret_key_enc, 2) << 8 | get_byte(secret_key_enc, 3)),
       count(*)
FROM s3_storages GROUP BY 1, 2
UNION ALL
SELECT 'notification_channels.config_enc',
       (get_byte(config_enc, 0) << 24 | get_byte(config_enc, 1) << 16 | get_byte(config_enc, 2) << 8 | get_byte(config_enc, 3)),
       count(*)
FROM notification_channels GROUP BY 1, 2
UNION ALL
SELECT 'webhook_endpoints.secret_enc',
       (get_byte(secret_enc, 0) << 24 | get_byte(secret_enc, 1) << 16 | get_byte(secret_enc, 2) << 8 | get_byte(secret_enc, 3)),
       count(*)
FROM webhook_endpoints GROUP BY 1, 2
ORDER BY 2, 1;

-- name: ListPrivateKeysToRotate :many
SELECT id, uuid, private_key_enc FROM private_keys
WHERE (get_byte(private_key_enc, 0) << 24 | get_byte(private_key_enc, 1) << 16 | get_byte(private_key_enc, 2) << 8 | get_byte(private_key_enc, 3)) <> sqlc.arg(active_version)::int
ORDER BY id
LIMIT $1;

-- name: RotatePrivateKeyEnc :exec
UPDATE private_keys SET private_key_enc = $2, updated_at = now() WHERE id = $1;

-- name: ListEnvVarsToRotate :many
SELECT id, uuid, value_enc FROM environment_variables
WHERE (get_byte(value_enc, 0) << 24 | get_byte(value_enc, 1) << 16 | get_byte(value_enc, 2) << 8 | get_byte(value_enc, 3)) <> sqlc.arg(active_version)::int
ORDER BY id
LIMIT $1;

-- name: RotateEnvVarEnc :exec
UPDATE environment_variables SET value_enc = $2, updated_at = now() WHERE id = $1;

-- name: ListDatabaseCredentialsToRotate :many
SELECT id, uuid, password_enc FROM database_credentials
WHERE (get_byte(password_enc, 0) << 24 | get_byte(password_enc, 1) << 16 | get_byte(password_enc, 2) << 8 | get_byte(password_enc, 3)) <> sqlc.arg(active_version)::int
ORDER BY id
LIMIT $1;

-- name: RotateDatabaseCredentialEnc :exec
UPDATE database_credentials SET password_enc = $2, updated_at = now() WHERE id = $1;
