-- Shared variables (§5.4, §3.1).

-- name: CreateSharedVariable :one
INSERT INTO shared_variables (uuid, team_id, scope, project_id, environment_id, server_id, key, value_enc, is_secret, created_by)
VALUES ($1, $2, $3, sqlc.narg(project_id), sqlc.narg(environment_id), sqlc.narg(server_id), $4, $5, $6, sqlc.narg(created_by))
RETURNING *;

-- name: GetSharedVariableByUUID :one
SELECT * FROM shared_variables WHERE uuid = $1 AND team_id = $2;

-- name: ListSharedVariablesPage :many
SELECT * FROM shared_variables
WHERE team_id = sqlc.arg(team_id)
  AND (sqlc.narg(scope)::shared_variable_scope IS NULL OR scope = sqlc.narg(scope)::shared_variable_scope)
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: UpdateSharedVariable :execrows
UPDATE shared_variables
SET value_enc = $2, is_secret = $3, updated_at = now()
WHERE id = $1;

-- name: DeleteSharedVariable :execrows
DELETE FROM shared_variables WHERE id = $1;

-- name: ListSharedVariablesForResource :many
-- Everything a deployment of this resource inherits (§5.4): the team's
-- variables, the ones of its project and environment, and the server-scoped
-- variables of its destination server.
SELECT sv.* FROM shared_variables sv
JOIN resources r ON r.id = sqlc.arg(resource_id)
JOIN environments e ON e.id = r.environment_id
JOIN destinations d ON d.id = r.destination_id
WHERE sv.team_id = r.team_id AND (
    sv.scope = 'team'
    OR (sv.scope = 'project' AND sv.project_id = e.project_id)
    OR (sv.scope = 'environment' AND sv.environment_id = r.environment_id)
    OR (sv.scope = 'server' AND sv.server_id = d.server_id))
ORDER BY sv.id;
