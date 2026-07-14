-- S3 storages (§7.2, data-dictionary §6.6). Credentials are envelope-encrypted
-- and never leave the instance (INV-003).

-- name: ListS3StoragesPage :many
SELECT * FROM s3_storages
WHERE team_id = $1 AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: GetS3StorageByUUID :one
SELECT * FROM s3_storages WHERE uuid = $1 AND team_id = $2;

-- name: GetS3StorageByID :one
SELECT * FROM s3_storages WHERE id = $1;

-- name: CreateS3Storage :one
INSERT INTO s3_storages (uuid, team_id, name, endpoint, region, bucket, path_prefix,
                         access_key_enc, secret_key_enc, is_usable, last_check_error)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: UpdateS3Storage :execrows
UPDATE s3_storages
SET name = $2, endpoint = $3, region = $4, bucket = $5, path_prefix = $6,
    access_key_enc = $7, secret_key_enc = $8, is_usable = $9, last_check_error = $10,
    updated_at = now(), version = version + 1
WHERE id = $1 AND version = sqlc.arg(expected_version);

-- name: SetS3StorageCheck :exec
UPDATE s3_storages
SET is_usable = $2, last_check_error = $3, updated_at = now()
WHERE id = $1;

-- name: DeleteS3Storage :execrows
DELETE FROM s3_storages WHERE id = $1;

-- name: CountBackupPlansUsingS3Storage :one
SELECT count(*) FROM database_backup_plans
WHERE s3_storage_id = $1 AND deleted_at IS NULL;

-- name: RotateS3StorageEnc :exec
UPDATE s3_storages SET access_key_enc = $2, secret_key_enc = $3 WHERE id = $1;

-- name: ListS3StoragesToRotate :many
-- Rows still encrypted with an older master key version (ADR-003, §23.2). The
-- key version is the first 4 bytes of the ciphertext.
SELECT id, uuid, access_key_enc, secret_key_enc FROM s3_storages
WHERE (get_byte(access_key_enc, 0) << 24 | get_byte(access_key_enc, 1) << 16 | get_byte(access_key_enc, 2) << 8 | get_byte(access_key_enc, 3)) <> sqlc.arg(active_version)::int
   OR (get_byte(secret_key_enc, 0) << 24 | get_byte(secret_key_enc, 1) << 16 | get_byte(secret_key_enc, 2) << 8 | get_byte(secret_key_enc, 3)) <> sqlc.arg(active_version)::int
ORDER BY id
LIMIT $1;
