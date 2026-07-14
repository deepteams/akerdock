-- Compose stacks and their components (compose-spec.md, data-dictionary §9).

-- name: CreateServiceRow :exec
INSERT INTO services (id, compose_content, template_slug, template_version, template_repository, connect_to_predefined_network)
VALUES ($1, $2, sqlc.narg(template_slug), sqlc.narg(template_version), sqlc.narg(template_repository), $3);

-- name: GetServiceByID :one
SELECT * FROM services WHERE id = $1;

-- name: UpdateServiceCompose :execrows
UPDATE services SET compose_content = $2, connect_to_predefined_network = $3, updated_at = now()
WHERE id = $1;

-- name: UpsertServiceComponent :one
-- Component sync (data-dictionary §9.2): recreated/updated at every edit of
-- the compose file. The upsert keeps the row identity (uuid, backup plans,
-- domains) stable for components that survive the edit.
INSERT INTO service_components (resource_id, name, image, is_database, database_engine, exclude_from_hc, default_route_port)
VALUES ($1, $2, sqlc.narg(image), $3, sqlc.narg(database_engine), $4, sqlc.narg(default_route_port))
ON CONFLICT (resource_id, name) DO UPDATE SET
    image = excluded.image,
    is_database = excluded.is_database,
    database_engine = excluded.database_engine,
    exclude_from_hc = excluded.exclude_from_hc,
    default_route_port = excluded.default_route_port,
    updated_at = now()
RETURNING *;

-- name: DeleteVanishedServiceComponents :execrows
-- A service removed from the compose file loses its component row — and with
-- it, by CASCADE, its domains and backup plans (explicit in the API response).
DELETE FROM service_components
WHERE resource_id = $1 AND NOT (name = ANY (sqlc.arg(names)::text[]));

-- name: ListServiceComponents :many
SELECT * FROM service_components WHERE resource_id = $1 ORDER BY name;

-- name: GetServiceComponentByUUID :one
SELECT sc.* FROM service_components sc
JOIN resources r ON r.id = sc.resource_id
WHERE sc.uuid = $1 AND r.team_id = $2 AND r.deleted_at IS NULL;

-- name: SetServiceComponentObserved :exec
UPDATE service_components SET observed_status = $2, observed_at = now(), updated_at = now()
WHERE id = $1;

-- name: ListServiceComponentDomains :many
SELECT dom.* FROM domains dom
WHERE dom.service_component_id = $1
ORDER BY dom.fqdn, dom.path;

-- name: GetServiceStackByUUID :one
-- Full stack lookup (mirror of GetApplicationByUUID): the resource, the
-- compose extension, and the placement identities the API renders.
SELECT sqlc.embed(r), sqlc.embed(s),
       e.uuid AS environment_uuid, p.uuid AS project_uuid,
       dst.uuid AS destination_uuid, srv.uuid AS server_uuid, srv.id AS server_row_id
FROM resources r
JOIN services s ON s.id = r.id
JOIN environments e ON e.id = r.environment_id
JOIN projects p ON p.id = e.project_id
JOIN destinations dst ON dst.id = r.destination_id
JOIN servers srv ON srv.id = dst.server_id
WHERE r.uuid = $1 AND r.team_id = $2 AND r.deleted_at IS NULL AND r.resource_type = 'service';

-- name: ListServiceStacksPage :many
SELECT sqlc.embed(r), sqlc.embed(s),
       e.uuid AS environment_uuid, p.uuid AS project_uuid,
       dst.uuid AS destination_uuid, srv.uuid AS server_uuid
FROM resources r
JOIN services s ON s.id = r.id
JOIN environments e ON e.id = r.environment_id
JOIN projects p ON p.id = e.project_id
JOIN destinations dst ON dst.id = r.destination_id
JOIN servers srv ON srv.id = dst.server_id
WHERE r.team_id = $1 AND r.deleted_at IS NULL AND r.resource_type = 'service'
  AND r.id > sqlc.arg(after_id)
ORDER BY r.id
LIMIT sqlc.arg(page_limit);
