-- SCIM 2.0 (ISO A.5.16/A.5.18, SOC2 CC6.1-6.3) : provisioning + déprovisioning
-- automatique des comptes depuis l'IdP. Un token SCIM est scopé à UNE team
-- (décision ADR-038 bis) : l'IdP crée/désactive les membres de cette team.
-- `external_id` sur team_memberships retient l'identifiant IdP pour matcher
-- idempotemment un utilisateur déjà provisionné. Expand-only.

-- +goose Up
CREATE TABLE scim_tokens (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    name text NOT NULL,
    token_hash text NOT NULL UNIQUE,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    last_used_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX scim_tokens_team_id_idx ON scim_tokens (team_id);

ALTER TABLE team_memberships ADD COLUMN external_id text;

-- +goose Down
ALTER TABLE team_memberships DROP COLUMN external_id;
DROP TABLE scim_tokens;
