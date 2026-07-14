-- Projects and environments (§2, §19.2). All access is scoped by the
-- authenticated team (INV-001); slugs are internal, derived from names.

-- name: CreateProject :one
INSERT INTO projects (team_id, name, slug, description)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetProjectByUUID :one
SELECT * FROM projects
WHERE uuid = $1 AND team_id = $2 AND deleted_at IS NULL;

-- name: ListProjectsPage :many
SELECT * FROM projects
WHERE team_id = sqlc.arg(team_id) AND deleted_at IS NULL
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: UpdateProject :execrows
UPDATE projects
SET name = $2, slug = $3, description = $4, updated_at = now(), version = version + 1
WHERE id = $1 AND version = sqlc.arg(expected_version) AND deleted_at IS NULL;

-- name: SoftDeleteProject :execrows
UPDATE projects SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: SoftDeleteProjectEnvironments :exec
UPDATE environments SET deleted_at = now(), updated_at = now()
WHERE project_id = $1 AND deleted_at IS NULL;

-- name: CreateEnvironment :one
INSERT INTO environments (project_id, name, slug, description)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetEnvironmentByUUID :one
SELECT * FROM environments
WHERE uuid = $1 AND project_id = $2 AND deleted_at IS NULL;

-- name: ListEnvironmentsPage :many
SELECT * FROM environments
WHERE project_id = sqlc.arg(project_id) AND deleted_at IS NULL
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListEnvironmentsSummary :many
SELECT * FROM environments
WHERE project_id = $1 AND deleted_at IS NULL
ORDER BY id;

-- name: UpdateEnvironment :execrows
UPDATE environments
SET name = $2, slug = $3, description = $4, updated_at = now(), version = version + 1
WHERE id = $1 AND version = sqlc.arg(expected_version) AND deleted_at IS NULL;

-- name: SoftDeleteEnvironment :execrows
UPDATE environments SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: CountResourcesByEnvironment :many
-- Live resources per environment, in one round trip: a per-row count would fan
-- out into one query per environment on every project listing.
SELECT environment_id, count(*) AS resources
FROM resources
WHERE environment_id = ANY(sqlc.arg(environment_ids)::bigint[]) AND deleted_at IS NULL
GROUP BY environment_id;
