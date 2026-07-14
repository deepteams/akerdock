-- Persistent storages (§8).

-- name: CreateStorage :one
INSERT INTO persistent_storages (uuid, resource_id, kind, name, host_path, mount_path)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: CreateAdoptedStorage :one
-- Adoption (§20.7): external_name keeps the original Docker volume name so
-- the normalizing redeployment remounts the SAME data (INV-008).
INSERT INTO persistent_storages (uuid, resource_id, kind, name, host_path, mount_path, external_name)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListStoragesForResource :many
SELECT * FROM persistent_storages WHERE resource_id = $1 ORDER BY mount_path;

-- name: GetStorageByUUID :one
SELECT * FROM persistent_storages WHERE uuid = $1 AND resource_id = $2;

-- name: DeleteStorage :execrows
DELETE FROM persistent_storages WHERE id = $1;
