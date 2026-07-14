-- Instance SSH key support (instance-config §6.2). The data dictionary had
-- private_keys.team_id NOT NULL, but the instance key is generated at first
-- boot, before any team exists. Resolution (documented in data-dictionary
-- §6.3): team_id becomes nullable for the single instance key only, flagged
-- is_instance — every other key still requires a team (INV-001).

-- +goose Up
ALTER TABLE private_keys ADD COLUMN is_instance boolean NOT NULL DEFAULT false;
ALTER TABLE private_keys ALTER COLUMN team_id DROP NOT NULL;
ALTER TABLE private_keys ADD CONSTRAINT private_keys_team_or_instance
    CHECK (team_id IS NOT NULL OR is_instance);
CREATE UNIQUE INDEX private_keys_instance_key ON private_keys (is_instance) WHERE is_instance;

-- +goose Down
DROP INDEX private_keys_instance_key;
ALTER TABLE private_keys DROP CONSTRAINT private_keys_team_or_instance;
DELETE FROM private_keys WHERE team_id IS NULL;
ALTER TABLE private_keys ALTER COLUMN team_id SET NOT NULL;
ALTER TABLE private_keys DROP COLUMN is_instance;
