-- +goose Up
-- (resource_id, mount_path) uniqueness holds for OPERATOR rows — one mount
-- per path in a single container. Compose mirror rows (is_generated) describe
-- several services: three services each mounting their own volume at /data is
-- valid compose, and the mirror must be able to say so.
ALTER TABLE persistent_storages DROP CONSTRAINT persistent_storages_resource_id_mount_path_key;
CREATE UNIQUE INDEX persistent_storages_operator_mount_key
    ON persistent_storages (resource_id, mount_path) WHERE NOT is_generated;

-- +goose Down
DROP INDEX persistent_storages_operator_mount_key;
ALTER TABLE persistent_storages ADD CONSTRAINT persistent_storages_resource_id_mount_path_key UNIQUE (resource_id, mount_path);
