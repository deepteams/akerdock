-- First proxy start is an operator's explicit action (§20.1 step 5).
--
-- New servers now start with proxy_desired_state = 'stopped': server
-- validation writes nothing and starts nothing proxy-wise until the operator
-- has reviewed the proxy settings (ports, wildcard domain, ACME email) and
-- pressed Start — which converges config + container from scratch.
--
-- Servers that never completed a validation (docker_version IS NULL) cannot
-- have a proxy container yet, so their recorded intent 'running' is a default
-- nobody chose: align them with the new rule. Servers already validated keep
-- their state — a running proxy an operator relies on must not flip.

-- +goose Up
ALTER TABLE servers ALTER COLUMN proxy_desired_state SET DEFAULT 'stopped';
UPDATE servers
SET proxy_desired_state = 'stopped'
WHERE proxy_desired_state = 'running'
  AND docker_version IS NULL;

-- +goose Down
ALTER TABLE servers ALTER COLUMN proxy_desired_state SET DEFAULT 'running';
