-- Persistent storages (§8).

-- name: CreateStorage :one
INSERT INTO persistent_storages (uuid, resource_id, kind, name, host_path, mount_path)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListStoragesForResource :many
SELECT * FROM persistent_storages WHERE resource_id = $1 ORDER BY mount_path;

-- name: GetStorageByUUID :one
SELECT * FROM persistent_storages WHERE uuid = $1 AND resource_id = $2;

-- name: DeleteStorage :execrows
DELETE FROM persistent_storages WHERE id = $1;
