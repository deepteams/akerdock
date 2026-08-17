-- Models: first-class inference resources (ADR-080). The API key is
-- enveloped like a database credential; engine_flags is the ORDERED tier-2
-- jsonb list; the port and the server are immutable in v1 (a moved model is
-- a new model — the weights cache is server-scoped).

-- name: CreateModelRow :exec
INSERT INTO models (id, engine, model_id, served_model_name, quantization, max_model_len, tensor_parallel_size, memory_fraction, image, image_tag, engine_flags, api_key_enc, shm_size_mb, published_port, server_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15);

-- name: GetModelByUUID :one
SELECT sqlc.embed(r), sqlc.embed(m),
       e.uuid AS environment_uuid, p.uuid AS project_uuid, srv.uuid AS server_uuid,
       srv.host AS server_host, srv.name AS server_name, srv.gpu_name AS server_gpu_name
FROM resources r
JOIN models m ON m.id = r.id
JOIN environments e ON e.id = r.environment_id
JOIN projects p ON p.id = e.project_id
JOIN servers srv ON srv.id = m.server_id
WHERE r.uuid = $1 AND r.team_id = $2 AND r.deleted_at IS NULL AND r.resource_type = 'model';

-- name: GetModelByID :one
SELECT sqlc.embed(r), sqlc.embed(m)
FROM resources r
JOIN models m ON m.id = r.id
WHERE r.id = $1 AND r.deleted_at IS NULL AND r.resource_type = 'model';

-- name: ListModelsPage :many
-- Team-wide, not per environment: the Models section is a transverse view
-- (ADR-080 §6) — every model of the team with its server and GPU.
SELECT sqlc.embed(r), sqlc.embed(m),
       e.uuid AS environment_uuid, p.uuid AS project_uuid, srv.uuid AS server_uuid,
       srv.host AS server_host, srv.name AS server_name, srv.gpu_name AS server_gpu_name
FROM resources r
JOIN models m ON m.id = r.id
JOIN environments e ON e.id = r.environment_id
JOIN projects p ON p.id = e.project_id
JOIN servers srv ON srv.id = m.server_id
WHERE r.team_id = sqlc.arg(team_id) AND r.deleted_at IS NULL AND r.resource_type = 'model'
  AND (sqlc.arg(after_id)::bigint = 0 OR r.id < sqlc.arg(after_id))
ORDER BY r.id DESC
LIMIT sqlc.arg(page_limit);

-- name: UpdateModelRow :exec
UPDATE models SET
    model_id = $2, served_model_name = $3, quantization = $4, max_model_len = $5,
    tensor_parallel_size = $6, memory_fraction = $7, image = $8, image_tag = $9,
    engine_flags = $10, shm_size_mb = $11, updated_at = now()
WHERE id = $1;

-- name: NextFreeModelPort :one
-- Lowest free port in the models range for a server; the unique index stays
-- the authority against concurrent allocation (§22.3, databases precedent).
SELECT coalesce(max(published_port), 18000) + 1 AS port
FROM models WHERE server_id = $1;

-- name: ListRunningModelsOnServer :many
-- The soft start-guard of ADR-080 §5: the models on this server, other than
-- the one starting, that are running by intent or by observation — the set
-- the confirmation names before offering the one-click swap.
SELECT r.uuid, r.name, m.memory_fraction
FROM resources r
JOIN models m ON m.id = r.id
WHERE m.server_id = sqlc.arg(server_id) AND r.id <> sqlc.arg(model_id)
  AND r.deleted_at IS NULL
  AND (r.desired_status = 'running' OR r.observed_status IN ('starting', 'healthy'))
ORDER BY r.id;
