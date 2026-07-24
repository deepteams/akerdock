-- Environment variables (§5.4): the production set (is_preview = false)
-- for the v1 endpoints; the preview set lands with previews.

-- name: CreateEnvVar :one
INSERT INTO environment_variables (uuid, resource_id, key, value_enc, is_build_time, is_literal, is_multiline, is_locked, is_secret, is_preview)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetEnvVarByUUID :one
SELECT * FROM environment_variables WHERE uuid = $1 AND resource_id = $2;

-- name: GetEnvVarByKey :one
SELECT * FROM environment_variables
WHERE resource_id = $1 AND key = $2 AND is_preview = false;

-- name: ListEnvVarsPage :many
-- The production set by default; the dedicated preview set on demand (§5.6):
-- the platform-generated preview credentials live there, and an operator who
-- cannot read them cannot open their own protected preview.
SELECT * FROM environment_variables
WHERE resource_id = sqlc.arg(resource_id) AND is_preview = sqlc.arg(is_preview)
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListEnvVarsForDeploy :many
SELECT * FROM environment_variables
WHERE resource_id = $1 AND is_preview = false
ORDER BY key;

-- name: UpdateEnvVar :one
UPDATE environment_variables
SET value_enc = $2, is_build_time = $3, is_literal = $4, is_multiline = $5, is_locked = $6, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteEnvVar :execrows
DELETE FROM environment_variables WHERE id = $1;

-- name: DeleteEnvVarsNotInKeys :exec
DELETE FROM environment_variables
WHERE resource_id = $1 AND is_preview = false AND NOT is_locked
  AND key <> ALL(sqlc.arg(keys)::text[]);

-- name: CreateGeneratedEnvVar :execrows
-- Magic variables (compose-spec §4.3): written at first use, is_generated,
-- never regenerated while the row exists — the conflict target guarantees it.
INSERT INTO environment_variables (uuid, resource_id, key, value_enc, is_secret, is_generated)
VALUES ($1, $2, $3, $4, $5, true)
ON CONFLICT (resource_id, key, is_preview) DO NOTHING;

-- name: CreateGeneratedPreviewEnvVar :execrows
-- Generated secret of the PREVIEW variable set (§20.4.4) — e.g. the basic
-- auth credential. Same one-shot semantics as the magic variables.
INSERT INTO environment_variables (uuid, resource_id, key, value_enc, is_secret, is_generated, is_preview)
VALUES ($1, $2, $3, $4, true, true, true)
ON CONFLICT (resource_id, key, is_preview) DO NOTHING;
