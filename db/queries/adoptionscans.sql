-- Adoption scans (PRD §20.7, ADR-013/ADR-023).

-- name: CreateAdoptionScan :one
INSERT INTO adoption_scans (uuid, team_id, server_id, created_by)
VALUES ($1, $2, $3, sqlc.narg(created_by))
RETURNING *;

-- name: GetAdoptionScanByUUIDForTeam :one
SELECT sqlc.embed(sc), s.uuid AS server_uuid
FROM adoption_scans sc
JOIN servers s ON s.id = sc.server_id
WHERE sc.uuid = $1 AND sc.team_id = $2 AND s.deleted_at IS NULL;

-- name: GetAdoptionScanByID :one
SELECT * FROM adoption_scans WHERE id = $1;

-- name: ListAdoptionScansForServer :many
SELECT * FROM adoption_scans
WHERE server_id = $1
  AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id)::bigint)
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: SetAdoptionScanRunning :exec
UPDATE adoption_scans SET status = 'running' WHERE id = $1;

-- name: CompleteAdoptionScan :exec
UPDATE adoption_scans
SET status = 'completed', candidates = $2, completed_at = now()
WHERE id = $1;

-- name: FailAdoptionScan :exec
UPDATE adoption_scans
SET status = 'failed', error = $2, completed_at = now()
WHERE id = $1;

-- name: GetEnvironmentByUUIDForTeam :one
-- Adoption targets an environment across projects: team isolation only
-- (INV-002).
SELECT e.* FROM environments e
JOIN projects p ON p.id = e.project_id
WHERE e.uuid = $1 AND p.team_id = $2
  AND e.deleted_at IS NULL AND p.deleted_at IS NULL;

-- name: ListLiveResourceUUIDs :many
-- Scan exclusion (INV-015): "managed" means tracked by a live row, not just
-- labelled — a disowned resource keeps its labels but is adoptable again.
SELECT uuid FROM resources
WHERE uuid = ANY (sqlc.arg(uuids)::uuid[]) AND deleted_at IS NULL;

-- name: SetResourceAdoption :exec
-- Marks a resource as adopted and not yet normalized: the JSONB points at
-- the real remote objects (container name, compose project).
UPDATE resources
SET adopted_at = now(), adoption = $2, updated_at = now()
WHERE id = $1;

-- name: ClearResourceAdoption :exec
-- The normalizing deployment converged the remote objects onto the
-- uuid-derived names (§20.7): the pointer is obsolete, the history stays.
UPDATE resources
SET adoption = NULL, updated_at = now()
WHERE id = $1;
