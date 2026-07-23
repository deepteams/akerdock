-- Deployments (state machine §21.1). Transitions are committed before the
-- next remote action (write-ahead, deployment-engine §4).

-- name: CreateDeployment :one
INSERT INTO deployments (uuid, resource_id, trigger, api_token_id, force_rebuild, image_name, image_tag, server_id, config_snapshot, preview_id, commit_sha)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, sqlc.narg(preview_id), sqlc.narg(commit_sha))
RETURNING *;

-- name: GetDeploymentByID :one
SELECT * FROM deployments WHERE id = $1;

-- name: GetDeploymentByUUIDForTeam :one
SELECT sqlc.embed(d), r.uuid AS resource_uuid FROM deployments d
JOIN resources r ON r.id = d.resource_id
WHERE d.uuid = $1 AND r.team_id = $2;

-- name: ListDeploymentsForResource :many
SELECT * FROM deployments
WHERE resource_id = sqlc.arg(resource_id)
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: SetDeploymentStatus :exec
UPDATE deployments SET status = $2,
    started_at = CASE WHEN started_at IS NULL AND $2 <> 'queued'::deployment_status THEN now() ELSE started_at END,
    finished_at = CASE WHEN $2 IN ('succeeded'::deployment_status, 'failed'::deployment_status, 'cancelled'::deployment_status) THEN now() ELSE finished_at END,
    updated_at = now()
WHERE id = $1;

-- name: SetDeploymentError :exec
UPDATE deployments SET error_message = $2, updated_at = now() WHERE id = $1;

-- name: SetDeploymentImageDigest :exec
UPDATE deployments SET image_digest = $2, updated_at = now() WHERE id = $1;

-- name: SetDeploymentImage :exec
UPDATE deployments SET image_name = $2, image_tag = $3, updated_at = now() WHERE id = $1;

-- name: CountActiveDeploymentsForServer :one
SELECT count(*) FROM deployments
WHERE server_id = $1 AND status NOT IN ('succeeded', 'failed', 'cancelled', 'superseded');

-- name: CreateDeploymentStep :one
INSERT INTO deployment_steps (deployment_id, seq, name, status, started_at)
VALUES ($1, $2, $3, 'running', now())
RETURNING id;

-- name: FinishDeploymentStep :exec
UPDATE deployment_steps SET status = $2, exit_code = $3, log = $4, finished_at = now()
WHERE id = $1;

-- name: SetDeploymentStepLog :exec
-- Live output of a RUNNING step (docker build, container start): the SSE log
-- stream polls the steps every second, so refreshing the log as the command
-- runs is what turns "step build: started … (silence)" into a console.
UPDATE deployment_steps SET log = $2 WHERE id = $1;

-- name: ListDeploymentSteps :many
SELECT * FROM deployment_steps WHERE deployment_id = $1 ORDER BY seq;

-- name: CancelQueuedDeployment :execrows
UPDATE deployments SET status = 'cancelled', finished_at = now(), updated_at = now()
WHERE id = $1 AND status = 'queued';

-- name: SetDeploymentCommit :exec
UPDATE deployments SET commit_sha = $2, git_branch = $3, updated_at = now() WHERE id = $1;

-- Rollback artifacts (ADR-006, §10.3).

-- name: CreateDeploymentArtifact :exec
INSERT INTO deployment_artifacts (deployment_id, kind, image_name, image_tag, image_digest, server_id)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetArtifactForDeployment :one
SELECT a.* FROM deployment_artifacts a
JOIN deployments d ON d.id = a.deployment_id
WHERE d.uuid = $1 AND d.resource_id = $2;

-- name: GetArtifactByDigest :one
SELECT a.* FROM deployment_artifacts a
JOIN deployments d ON d.id = a.deployment_id
WHERE a.image_digest = $1 AND d.resource_id = $2
ORDER BY a.id DESC
LIMIT 1;

-- name: GetPreviousArtifact :one
-- The most recent artifact of a successful deployment other than the one
-- currently serving traffic (the last succeeded deployment).
SELECT a.* FROM deployment_artifacts a
JOIN deployments d ON d.id = a.deployment_id
WHERE d.resource_id = $1 AND d.status = 'succeeded'
  AND d.id < (SELECT max(id) FROM deployments WHERE resource_id = $1 AND status = 'succeeded')
ORDER BY a.id DESC
LIMIT 1;

-- name: CreateRollbackDeployment :one
INSERT INTO deployments (uuid, resource_id, trigger, api_token_id, is_rollback, image_name, image_tag, image_digest, server_id, config_snapshot)
VALUES ($1, $2, $3, $4, true, $5, $6, $7, $8, $9)
RETURNING *;

-- Coalescing (§3.4): a queued webhook deployment for the same application
-- is superseded by a newer one; an already leased/running deployment is
-- never coalesced.
-- name: SupersedeQueuedDeployments :many
UPDATE deployments SET status = 'superseded', superseded_by_id = $2,
    finished_at = now(), updated_at = now()
WHERE resource_id = $1 AND status = 'queued' AND trigger = 'webhook' AND id <> $2
RETURNING id;

-- name: CancelJobsForDeployments :exec
UPDATE jobs SET status = 'cancelled', finished_at = now(), updated_at = now()
WHERE job_type = 'deployment.run' AND status = 'queued'
  AND (payload->>'deployment_id')::bigint = ANY(sqlc.arg(deployment_ids)::bigint[]);

-- name: MaxDeploymentStepSeq :one
-- A resumed deployment continues the step numbering of the attempt that
-- crashed: restarting at 1 would collide with the steps already recorded, and
-- would also erase the history of what the dead worker had done.
SELECT coalesce(max(seq), 0)::int AS max_seq FROM deployment_steps WHERE deployment_id = $1;

-- name: SetDeploymentBuildServer :exec
UPDATE deployments SET build_server_id = $2 WHERE id = $1;
