-- Applications: union base resources + 1-1 extensions (§19.1).

-- name: CreateResource :one
INSERT INTO resources (uuid, team_id, environment_id, destination_id, resource_type, name, description)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: CreateApplicationRow :exec
INSERT INTO applications (id, git_repository_url, git_branch, base_directory, git_source_id, repository_id, watch_paths)
VALUES ($1, $2, $3, $4, sqlc.narg(git_source_id), sqlc.narg(repository_id), sqlc.narg(watch_paths));

-- name: UpdateApplicationGitSettings :exec
-- Git pipeline settings of a git application (PATCH /applications): a nil
-- argument leaves the column alone — partial like the rest of the update.
-- watch_paths carries a set flag because an explicit empty list must CLEAR
-- the column ("deploy on every push again"), not keep it.
UPDATE applications SET
    git_repository_url = COALESCE(sqlc.narg(git_repository_url), git_repository_url),
    git_branch = COALESCE(sqlc.narg(git_branch), git_branch),
    base_directory = COALESCE(sqlc.narg(base_directory), base_directory),
    watch_paths = CASE WHEN sqlc.arg(set_watch_paths)::boolean
                      THEN sqlc.narg(watch_paths)
                      ELSE watch_paths END,
    auto_deploy_enabled = COALESCE(sqlc.narg(auto_deploy_enabled), auto_deploy_enabled),
    updated_at = now()
WHERE id = $1;

-- name: UpdateBuildConfigGitPipeline :exec
-- Build-pack side of the same PATCH. publish_directory carries a set flag:
-- an explicit null means "no publish step anymore", a COALESCE would keep it.
UPDATE build_configs SET
    build_pack = COALESCE(sqlc.narg(build_pack), build_pack),
    dockerfile_path = COALESCE(sqlc.narg(dockerfile_path), dockerfile_path),
    publish_directory = CASE WHEN sqlc.arg(set_publish_directory)::boolean
                            THEN sqlc.narg(publish_directory)
                            ELSE publish_directory END,
    compose_file_path = COALESCE(sqlc.narg(compose_file_path), compose_file_path),
    raw_compose = COALESCE(sqlc.narg(raw_compose), raw_compose),
    updated_at = now()
WHERE application_id = $1;

-- name: CreateBuildConfig :exec
INSERT INTO build_configs (application_id, build_pack, image_name, image_tag, dockerfile_content, dockerfile_path, publish_directory, registry_credential_id, use_build_server, push_enabled, push_registry_credential_id, compose_file_path, raw_compose)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, sqlc.narg(compose_file_path), $12);

-- name: CreateRuntimeConfig :exec
INSERT INTO runtime_configs (application_id, ports_exposes, memory_limit, cpu_limit,
                             pre_deployment_command, post_deployment_command)
VALUES ($1, $2, $3, $4, sqlc.narg(pre_deployment_command), sqlc.narg(post_deployment_command));

-- name: GetApplicationByUUID :one
SELECT sqlc.embed(r), sqlc.embed(a), sqlc.embed(b), sqlc.embed(rt),
       e.uuid AS environment_uuid, p.uuid AS project_uuid,
       dst.uuid AS destination_uuid, srv.uuid AS server_uuid, srv.id AS server_row_id,
       pk.uuid AS private_key_uuid, rc.uuid AS registry_credential_uuid,
       prc.uuid AS push_registry_credential_uuid,
       (gs.api_token_enc IS NOT NULL)::boolean AS git_api_token_set,
       gs.api_url AS git_api_url,
       ga.uuid AS github_app_uuid
FROM resources r
JOIN applications a ON a.id = r.id
JOIN build_configs b ON b.application_id = a.id
JOIN runtime_configs rt ON rt.application_id = a.id
JOIN environments e ON e.id = r.environment_id
JOIN projects p ON p.id = e.project_id
JOIN destinations dst ON dst.id = r.destination_id
JOIN servers srv ON srv.id = dst.server_id
LEFT JOIN git_sources gs ON gs.id = a.git_source_id
LEFT JOIN github_apps ga ON ga.id = gs.github_app_id
LEFT JOIN private_keys pk ON pk.id = gs.private_key_id
LEFT JOIN registry_credentials rc ON rc.id = b.registry_credential_id
LEFT JOIN registry_credentials prc ON prc.id = b.push_registry_credential_id
WHERE r.uuid = $1 AND r.team_id = $2 AND r.deleted_at IS NULL AND r.resource_type = 'application';

-- name: ListApplicationsPage :many
SELECT sqlc.embed(r), sqlc.embed(a), sqlc.embed(b), sqlc.embed(rt),
       e.uuid AS environment_uuid, p.uuid AS project_uuid,
       dst.uuid AS destination_uuid, srv.uuid AS server_uuid, srv.id AS server_row_id,
       pk.uuid AS private_key_uuid, rc.uuid AS registry_credential_uuid,
       prc.uuid AS push_registry_credential_uuid,
       (gs.api_token_enc IS NOT NULL)::boolean AS git_api_token_set,
       gs.api_url AS git_api_url,
       ga.uuid AS github_app_uuid
FROM resources r
JOIN applications a ON a.id = r.id
JOIN build_configs b ON b.application_id = a.id
JOIN runtime_configs rt ON rt.application_id = a.id
LEFT JOIN git_sources gs ON gs.id = a.git_source_id
LEFT JOIN github_apps ga ON ga.id = gs.github_app_id
LEFT JOIN private_keys pk ON pk.id = gs.private_key_id
LEFT JOIN registry_credentials rc ON rc.id = b.registry_credential_id
LEFT JOIN registry_credentials prc ON prc.id = b.push_registry_credential_id
JOIN environments e ON e.id = r.environment_id
JOIN projects p ON p.id = e.project_id
JOIN destinations dst ON dst.id = r.destination_id
JOIN servers srv ON srv.id = dst.server_id
WHERE r.team_id = sqlc.arg(team_id) AND r.deleted_at IS NULL AND r.resource_type = 'application'
  AND (sqlc.arg(after_id)::bigint = 0 OR r.id < sqlc.arg(after_id))
ORDER BY r.id DESC
LIMIT sqlc.arg(page_limit);

-- name: GetApplicationByID :one
SELECT sqlc.embed(r), sqlc.embed(a), sqlc.embed(b), sqlc.embed(rt)
FROM resources r
JOIN applications a ON a.id = r.id
JOIN build_configs b ON b.application_id = a.id
JOIN runtime_configs rt ON rt.application_id = a.id
WHERE r.id = $1 AND r.deleted_at IS NULL AND r.resource_type = 'application';

-- name: GetResourceByID :one
SELECT * FROM resources WHERE id = $1 AND deleted_at IS NULL;

-- name: SetResourceDesiredStatus :exec
UPDATE resources SET desired_status = $2, updated_at = now() WHERE id = $1;

-- name: SetResourceObservedStatus :exec
UPDATE resources SET observed_status = $2, observed_at = now(),
    last_online_at = CASE WHEN $2 = 'healthy'::resource_observed_status THEN now() ELSE last_online_at END,
    updated_at = now()
WHERE id = $1;

-- name: SoftDeleteResource :execrows
UPDATE resources SET deleted_at = now(), desired_status = 'deleted', updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: CountResourcesInEnvironment :one
SELECT count(*) FROM resources WHERE environment_id = $1 AND deleted_at IS NULL;

-- name: CountResourcesInProject :one
SELECT count(*) FROM resources r
JOIN environments e ON e.id = r.environment_id
WHERE e.project_id = $1 AND r.deleted_at IS NULL;

-- name: CountResourcesOnServer :one
SELECT count(*) FROM resources r
JOIN destinations d ON d.id = r.destination_id
WHERE d.server_id = $1 AND r.deleted_at IS NULL;

-- name: GetDefaultDestination :one
SELECT * FROM destinations WHERE server_id = $1 AND is_default;

-- name: CreateDestination :one
INSERT INTO destinations (uuid, server_id, name, network, is_default)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetDestinationByID :one
SELECT * FROM destinations WHERE id = $1;

-- name: UpdateResourceMeta :execrows
UPDATE resources SET name = $2, description = $3, updated_at = now(), version = version + 1
WHERE id = $1 AND version = sqlc.arg(expected_version) AND deleted_at IS NULL;

-- name: UpdateBuildConfigSource :exec
UPDATE build_configs SET image_name = $2, image_tag = $3, dockerfile_content = $4,
    -- `set_registry_credential` distinguishes "leave it alone" from "clear it":
    -- an explicit null means "this image is public now", and a COALESCE would
    -- silently keep pulling with a credential the operator meant to remove.
    registry_credential_id = CASE WHEN sqlc.arg(set_registry_credential)::boolean
                                 THEN sqlc.narg(registry_credential_id)
                                 ELSE registry_credential_id END,
    use_build_server = COALESCE(sqlc.narg(use_build_server), use_build_server),
    push_enabled = COALESCE(sqlc.narg(push_enabled), push_enabled),
    push_registry_credential_id = CASE WHEN sqlc.arg(set_push_registry_credential)::boolean
                                      THEN sqlc.narg(push_registry_credential_id)
                                      ELSE push_registry_credential_id END,
    updated_at = now()
WHERE application_id = $1;

-- name: UpdateRuntimeSettings :exec
UPDATE runtime_configs SET ports_exposes = $2, memory_limit = $3,
    pre_deployment_command = sqlc.narg(pre_deployment_command),
    post_deployment_command = sqlc.narg(post_deployment_command),
    updated_at = now()
WHERE application_id = $1;

-- name: DeleteDomainsForApplication :exec
DELETE FROM domains WHERE application_id = $1;

-- name: DeleteComponentDomainsForResource :exec
-- The compose components' domains (compose-spec §6). Deleted with the
-- application: the (fqdn, path) uniqueness is GLOBAL and hard (INV-002) — a
-- surviving row locks the URL against any future application, forever.
DELETE FROM domains WHERE service_component_id IN (
    SELECT id FROM service_components WHERE resource_id = $1
);

-- Tags (§5.4): used by the deploy webhook (?tag=).

-- name: UpsertTag :one
INSERT INTO tags (team_id, name) VALUES ($1, $2)
ON CONFLICT (team_id, name) DO UPDATE SET name = EXCLUDED.name
RETURNING *;

-- name: TagResource :exec
INSERT INTO resource_tags (resource_id, tag_id) VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: ListTagsForResource :many
SELECT t.name FROM tags t
JOIN resource_tags rt ON rt.tag_id = t.id
WHERE rt.resource_id = $1
ORDER BY t.name;

-- name: ListApplicationsByTags :many
SELECT DISTINCT r.uuid FROM resources r
JOIN resource_tags rt ON rt.resource_id = r.id
JOIN tags t ON t.id = rt.tag_id
WHERE r.team_id = $1 AND r.deleted_at IS NULL AND r.resource_type = 'application'
  AND t.name = ANY(sqlc.arg(tag_names)::citext[])
ORDER BY r.uuid;

-- name: ClearResourceTags :exec
DELETE FROM resource_tags WHERE resource_id = $1;

-- name: SetResourceRemnants :exec
-- What a failed deletion left behind on the server (§20.6.4). Recorded so the
-- operator can retry, or forget with an explicit acknowledgement — a forget
-- never cleans anything up remotely, it only stops pretending the job matters.
UPDATE resources SET remnants = $2, updated_at = now() WHERE id = $1;

-- name: GetResourceRemnants :one
SELECT remnants FROM resources WHERE id = $1;

-- name: UpdateApplicationScaleToZero :exec
-- Scale-to-zero of the application itself (ADR-037), separate from previews.
UPDATE applications SET
    scale_to_zero = COALESCE(sqlc.narg(scale_to_zero), scale_to_zero),
    scale_to_zero_after_minutes = COALESCE(sqlc.narg(scale_to_zero_after_minutes), scale_to_zero_after_minutes)
WHERE id = $1;

-- name: ListApplicationsToSleep :many
-- Awake applications that opted into scale-to-zero and are meant to run: the
-- scheduler reads each one's waker activity file over SSH and sleeps the ones
-- idle past their window (ADR-037). A manually stopped app (desired_status !=
-- running) is never touched.
SELECT r.id, r.uuid, r.updated_at, a.scale_to_zero_after_minutes
FROM applications a
JOIN resources r ON r.id = a.id
WHERE a.scale_to_zero = true AND a.scale_slept_at IS NULL
  AND r.desired_status = 'running' AND r.deleted_at IS NULL;

-- name: ListSleepingApplications :many
-- Applications currently asleep (ADR-037): the scheduler flips them back to
-- awake when the waker has served them again (fresh activity after slept_at).
SELECT r.id, r.uuid, a.scale_slept_at
FROM applications a
JOIN resources r ON r.id = a.id
WHERE a.scale_slept_at IS NOT NULL AND r.deleted_at IS NULL;

-- name: SetApplicationSlept :exec
UPDATE applications SET scale_slept_at = now() WHERE id = $1;

-- name: SetApplicationAwake :exec
UPDATE applications SET scale_slept_at = NULL WHERE id = $1;

-- name: WakeSleptApplicationForServer :execrows
-- Agent ingestion (ADR-040): flip a slept scale-to-zero application back to
-- awake, only when its resource lives on the given server.
UPDATE applications a SET scale_slept_at = NULL
FROM resources r
JOIN destinations d ON d.id = r.destination_id
WHERE a.id = r.id AND r.uuid = $1 AND d.server_id = $2 AND a.scale_slept_at IS NOT NULL;
