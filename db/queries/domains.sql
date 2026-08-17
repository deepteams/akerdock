-- Domains (§4.2). Uniqueness (fqdn, path) is enforced by the table.

-- name: CreateDomain :one
INSERT INTO domains (uuid, application_id, fqdn, path, target_port, is_generated)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListDomainsForApplication :many
SELECT * FROM domains WHERE application_id = $1 ORDER BY fqdn, path;

-- name: CreateModelDomain :one
-- A model's public route (ADR-080 §1 / ADR-077). target_port stays NULL: a
-- model routes to its single engine container port, always.
INSERT INTO domains (uuid, model_id, fqdn, path, target_port, is_generated)
VALUES ($1, $2, $3, $4, NULL, false)
RETURNING *;

-- name: ListDomainsForModel :many
SELECT * FROM domains WHERE model_id = $1 ORDER BY fqdn, path;

-- name: DeleteDomainsForModel :exec
DELETE FROM domains WHERE model_id = $1;

-- name: CreateComponentDomain :one
-- Generated domain of a compose component (compose-spec §6): referencing
-- SERVICE_FQDN_<ID> in the file is a declaration of intent — the domain is
-- created from the server wildcard at first deployment.
INSERT INTO domains (uuid, service_component_id, fqdn, path, target_port, is_generated)
VALUES ($1, $2, $3, '/', sqlc.narg(target_port), true)
ON CONFLICT (fqdn, path) DO NOTHING
RETURNING *;
