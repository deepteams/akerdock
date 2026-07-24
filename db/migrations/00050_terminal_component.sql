-- +goose Up
-- A compose stack has no container of its own (compose-spec §2.2): a terminal
-- into it must name the SERVICE whose container to exec into. The component
-- is resolved and validated at session creation, then read back at connect
-- time — the container name becomes `<resource_uuid>-<component>`.
ALTER TABLE terminal_sessions ADD COLUMN target_component text;

-- +goose Down
ALTER TABLE terminal_sessions DROP COLUMN target_component;
