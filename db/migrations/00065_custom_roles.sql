-- ADR-038 (modèle de rôles) : rôles CUSTOM composés dans l'UI à partir des
-- permissions granulaires du catalogue. Un rôle custom appartient à une team,
-- porte un ensemble de permissions granulaires (jamais instance:*), et peut être
-- assigné à un membre via `team_memberships.custom_role_id`.
--
-- Choix de design : PAS de nouvelle valeur d'enum `team_role`. Quand
-- `custom_role_id` est renseigné, il OVERRIDE le rôle système de la colonne
-- `role` (qui reste une valeur valide servant de repli si le rôle custom est
-- supprimé — ON DELETE SET NULL). Cela évite le piège PostgreSQL « ALTER TYPE
-- ADD VALUE puis usage dans la même transaction » et garde la migration
-- expand-only (colonne nullable, aucune valeur d'enum ajoutée).

-- +goose Up
CREATE TABLE custom_roles (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    name text NOT NULL,
    description text,
    -- Permissions granulaires (domaine:action), déjà fermées sous prérequis et
    -- validées ⊆ composeur / jamais instance:* au moment de l'écriture.
    permissions text[] NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (team_id, name)
);

CREATE INDEX custom_roles_team_id_idx ON custom_roles (team_id);

-- Nullable : un membre sans rôle custom garde son rôle système. ON DELETE SET
-- NULL : supprimer un rôle custom retombe les membres sur leur rôle système.
ALTER TABLE team_memberships
    ADD COLUMN custom_role_id bigint REFERENCES custom_roles (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE team_memberships DROP COLUMN custom_role_id;
DROP TABLE custom_roles;
