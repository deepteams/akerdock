-- Deployments that rebuild nothing (§5.3, ADR-048).
--
-- Changing an environment variable and pressing Restart did nothing: a
-- container freezes its environment at creation time, so `docker restart`
-- hands the process back the values it already had. The only way to apply a
-- new variable was a full deployment — a clone and a build, for a change that
-- touches neither the source nor the image.
--
-- A skip_build deployment is the missing step in between: the pipeline runs
-- whole (fresh env file, create, health check, switchover), but the artifact
-- is the one already in place. `is_rollback` cannot carry this — it means
-- "an EARLIER image", and the history reads it that way.

-- +goose Up
ALTER TABLE deployments ADD COLUMN skip_build boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE deployments DROP COLUMN skip_build;
