-- ADR-044: dynamic client registration becomes an explicit instance opt-in.
-- CIMD (a client_id that is an https URL, resolved by fetching its metadata
-- document) is the default and needs no stored client; DCR stays available
-- for clients that do not implement it yet, off unless an instance root says
-- otherwise. Expand-only.

-- +goose Up
ALTER TABLE instance_settings ADD COLUMN mcp_dcr_enabled boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE instance_settings DROP COLUMN mcp_dcr_enabled;
