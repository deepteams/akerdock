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
                         access_key_enc, secret_key_enc, is_usable, last_check_error, sse_algorithm)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: UpdateS3Storage :execrows
UPDATE s3_storages
SET name = $2, endpoint = $3, region = $4, bucket = $5, path_prefix = $6,
    access_key_enc = $7, secret_key_enc = $8, is_usable = $9, last_check_error = $10,
    sse_algorithm = sqlc.arg(sse_algorithm),
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
