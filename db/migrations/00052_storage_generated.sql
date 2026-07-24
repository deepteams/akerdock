-- +goose Up
-- Compose stacks declare their volumes in the FILE (compose-spec §2.4): the
-- deployment syncs them here so the Storages tab shows what actually exists.
-- is_generated separates these mirrored rows from operator-created ones —
-- they are rewritten on every deployment and not editable.
ALTER TABLE persistent_storages ADD COLUMN is_generated boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE persistent_storages DROP COLUMN is_generated;
