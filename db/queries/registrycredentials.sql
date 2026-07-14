-- Private registry credentials (data-dictionary §6.5). The password is never
-- selected back out to the API: only the deployment engine decrypts it, and
-- only to feed the stdin of `docker login` (INV-003).

-- name: CreateRegistryCredential :one
INSERT INTO registry_credentials (uuid, team_id, name, registry_url, username, password_enc, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetRegistryCredentialByUUID :one
SELECT * FROM registry_credentials
WHERE uuid = $1 AND team_id = $2 AND deleted_at IS NULL;

-- name: GetRegistryCredentialByID :one
SELECT * FROM registry_credentials WHERE id = $1 AND deleted_at IS NULL;

-- name: ListRegistryCredentialsPage :many
SELECT * FROM registry_credentials
WHERE team_id = sqlc.arg(team_id) AND deleted_at IS NULL
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: UpdateRegistryCredential :execrows
UPDATE registry_credentials
SET name = COALESCE(sqlc.narg(name), name),
    registry_url = COALESCE(sqlc.narg(registry_url), registry_url),
    username = COALESCE(sqlc.narg(username), username),
    password_enc = COALESCE(sqlc.narg(password_enc), password_enc),
    updated_by = sqlc.narg(updated_by),
    updated_at = now(),
    version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(expected_version) AND deleted_at IS NULL;

-- name: SoftDeleteRegistryCredential :execrows
UPDATE registry_credentials SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: CountRegistryCredentialUsage :one
-- A credential still referenced by a build config or by a rollback artifact
-- cannot be deleted: the deployment that depends on it would stop being able
-- to pull its own image (§19.2).
SELECT (SELECT count(*) FROM build_configs b
        WHERE b.registry_credential_id = sqlc.arg(credential_id)
           OR b.push_registry_credential_id = sqlc.arg(credential_id))
     + (SELECT count(*) FROM deployment_artifacts a
        WHERE a.registry_credential_id = sqlc.arg(credential_id)) AS usage_count;
