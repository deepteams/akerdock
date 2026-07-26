-- ADR-038 : une invitation peut porter un rôle custom (en plus des rôles système
-- admin/member/reviewer). Comme pour team_memberships, `custom_role_id` renseigné
-- override le rôle système à l'acceptation. Nullable, ON DELETE SET NULL :
-- supprimer un rôle custom retombe l'invitation sur son rôle système. Expand-only.

-- +goose Up
ALTER TABLE invitations
    ADD COLUMN custom_role_id bigint REFERENCES custom_roles (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE invitations DROP COLUMN custom_role_id;
