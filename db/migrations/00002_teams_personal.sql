-- The OpenAPI Team schema exposes a `personal` flag (team created
-- automatically with its user, PRD §2/§10.1) that was missing from the
-- data dictionary. Additive change, rolling-upgrade safe (expand step).

-- +goose Up
ALTER TABLE teams ADD COLUMN personal boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE teams DROP COLUMN personal;
