-- +goose Up
-- Per-PR variable overrides (§20.4): the shared preview set stays the base,
-- and a row carrying a preview_id applies to THAT instance only, overriding
-- the set's key. ON DELETE CASCADE: an override has no life beyond its PR.
ALTER TABLE environment_variables ADD COLUMN preview_id bigint REFERENCES previews (id) ON DELETE CASCADE;

-- The old uniqueness knew two sets (production, previews). With overrides it
-- becomes: one row per key in each of production, the shared preview set,
-- and every single preview.
ALTER TABLE environment_variables DROP CONSTRAINT environment_variables_resource_id_key_is_preview_key;
CREATE UNIQUE INDEX environment_variables_scope_key
    ON environment_variables (resource_id, key, is_preview, COALESCE(preview_id, 0));

-- +goose Down
DROP INDEX environment_variables_scope_key;
ALTER TABLE environment_variables DROP COLUMN preview_id;
ALTER TABLE environment_variables ADD CONSTRAINT environment_variables_resource_id_key_is_preview_key UNIQUE (resource_id, key, is_preview);
