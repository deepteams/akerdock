-- Managed databases (§6). Passwords are envelope encrypted; connection
-- URLs are rebuilt on the fly, never stored assembled (§6.2).

-- name: CreateDatabaseRow :exec
INSERT INTO databases (id, engine, image, image_tag, custom_config, initdb_args, server_id, is_public, public_access_mode, public_port, ssl_enabled, ssl_mode)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- name: CreateDatabaseCredential :one
INSERT INTO database_credentials (uuid, database_id, username, password_enc, db_name)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetDatabaseByUUID :one
SELECT sqlc.embed(r), sqlc.embed(d), sqlc.embed(c),
       e.uuid AS environment_uuid, p.uuid AS project_uuid, srv.uuid AS server_uuid,
       srv.host AS server_host
FROM resources r
JOIN databases d ON d.id = r.id
JOIN database_credentials c ON c.database_id = d.id
JOIN environments e ON e.id = r.environment_id
JOIN projects p ON p.id = e.project_id
JOIN servers srv ON srv.id = d.server_id
WHERE r.uuid = $1 AND r.team_id = $2 AND r.deleted_at IS NULL AND r.resource_type = 'database';

-- name: GetDatabaseByID :one
SELECT sqlc.embed(r), sqlc.embed(d), sqlc.embed(c)
FROM resources r
JOIN databases d ON d.id = r.id
JOIN database_credentials c ON c.database_id = d.id
WHERE r.id = $1 AND r.deleted_at IS NULL AND r.resource_type = 'database';

-- name: ListDatabasesPage :many
SELECT sqlc.embed(r), sqlc.embed(d), sqlc.embed(c),
       e.uuid AS environment_uuid, p.uuid AS project_uuid, srv.uuid AS server_uuid,
       srv.host AS server_host
FROM resources r
JOIN databases d ON d.id = r.id
JOIN database_credentials c ON c.database_id = d.id
JOIN environments e ON e.id = r.environment_id
JOIN projects p ON p.id = e.project_id
JOIN servers srv ON srv.id = d.server_id
WHERE r.team_id = sqlc.arg(team_id) AND r.deleted_at IS NULL AND r.resource_type = 'database'
  AND (sqlc.arg(after_id)::bigint = 0 OR r.id < sqlc.arg(after_id))
ORDER BY r.id DESC
LIMIT sqlc.arg(page_limit);

-- name: UpdateDatabaseRow :exec
UPDATE databases SET image = $2, image_tag = $3, custom_config = $4,
    is_public = $5, public_access_mode = $6, public_port = $7, ssl_mode = $8, updated_at = now()
WHERE id = $1;

-- name: UpdateDatabasePassword :exec
UPDATE database_credentials SET password_enc = $2, updated_at = now() WHERE database_id = $1;

-- name: NextFreePublicPort :one
-- Lowest free port in the dynamic range for a server (§6.2); the unique
-- index remains the authority against concurrent allocation.
SELECT coalesce(max(public_port), 15431) + 1 AS port
FROM databases WHERE server_id = $1 AND is_public;

-- name: ListTCPProxyPorts :many
-- Every public port routed through the proxy on a server. It is the set the
-- static config must declare: Traefik cannot add a listener at runtime.
SELECT d.public_port FROM databases d
JOIN resources r ON r.id = d.id
WHERE d.server_id = $1 AND d.is_public AND d.public_access_mode = 'tcp_proxy'
  AND d.public_port IS NOT NULL AND r.deleted_at IS NULL
ORDER BY d.public_port;
