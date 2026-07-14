-- Adoption of existing Docker resources (PRD §20.7, ADR-013/ADR-023,
-- data-dictionary §11.x).
--
-- adoption_scans: one row per server scan. The candidates JSONB holds the
-- proposed mapping shown to the user — env variable NAMES only, never values
-- (INV-003); values are captured and envelope-encrypted at adoption time.
--
-- resources.adoption: while an adopted resource has not been normalized by
-- its first redeployment, this JSONB points at the real remote objects
-- (container name, compose project) so lifecycle/logs/terminal/deploy target
-- them instead of the uuid-derived names. Cleared by the normalizing deploy;
-- adopted_at stays as history.
--
-- persistent_storages.external_name: an adopted volume keeps its original
-- Docker name across redeployments — renaming it would silently remount an
-- empty volume, which is exactly the data loss §20.7 forbids.

-- +goose Up
CREATE TYPE adoption_scan_status AS ENUM ('pending', 'running', 'completed', 'failed');

CREATE TABLE adoption_scans (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    server_id bigint NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    status adoption_scan_status NOT NULL DEFAULT 'pending',
    error text,
    candidates jsonb,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);

CREATE INDEX adoption_scans_team_id_idx ON adoption_scans (team_id, id DESC);
CREATE INDEX adoption_scans_server_id_idx ON adoption_scans (server_id, id DESC);

ALTER TABLE resources ADD COLUMN adopted_at timestamptz;
ALTER TABLE resources ADD COLUMN adoption jsonb;

ALTER TABLE persistent_storages ADD COLUMN external_name text;

-- +goose Down
ALTER TABLE persistent_storages DROP COLUMN external_name;
ALTER TABLE resources DROP COLUMN adoption;
ALTER TABLE resources DROP COLUMN adopted_at;
DROP TABLE adoption_scans;
DROP TYPE adoption_scan_status;
