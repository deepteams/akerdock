-- +goose Up
-- A terminal can open inside a PREVIEW instance (§20.4, §5.7): the container
-- names derive from the preview uuid (INV-011), not the resource's — the
-- session must record which preview it targets. ON DELETE SET NULL: a
-- destroyed preview invalidates the target, and connect says so explicitly.
ALTER TABLE terminal_sessions ADD COLUMN preview_id bigint REFERENCES previews (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE terminal_sessions DROP COLUMN preview_id;
