-- ADR-058: a session can be put in "view as <role>" mode — its permissions are
-- intersected with the simulated role's, so an admin sees exactly what a member
-- or a reviewer sees, 403s included, without creating a second account.
--
-- The state lives on the SESSION, not on the user: it must die with the browser
-- session, never follow the account into an API token or another device. It is
-- a restriction only — the intersection cannot grant anything the session did
-- not already hold, which is why no elevation is reachable through this column.
--
-- Exactly one of the two columns is set: a system role by name, or a custom
-- role by id (custom roles override system ones, ADR-038).

-- +goose Up
ALTER TABLE sessions
    ADD COLUMN view_as_role team_role,
    ADD COLUMN view_as_custom_role_id bigint REFERENCES custom_roles (id) ON DELETE SET NULL,
    ADD CONSTRAINT sessions_view_as_one_source
        CHECK (view_as_role IS NULL OR view_as_custom_role_id IS NULL);

-- +goose Down
ALTER TABLE sessions
    DROP CONSTRAINT sessions_view_as_one_source,
    DROP COLUMN view_as_custom_role_id,
    DROP COLUMN view_as_role;
