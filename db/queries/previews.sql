-- PR previews (data-dictionary §8.9, §20.4).

-- name: UpsertPreview :one
-- New commit on the same PR reuses the identity — the instance redeploys,
-- it is never duplicated (§20.4). A destroyed preview row is revived when
-- the PR reopens.
INSERT INTO previews (application_id, provider, pr_id, source_branch, head_sha, is_fork)
VALUES ($1, $2, $3, sqlc.narg(source_branch), sqlc.narg(head_sha), $4)
ON CONFLICT (application_id, provider, pr_id) DO UPDATE SET
    source_branch = excluded.source_branch,
    head_sha = excluded.head_sha,
    is_fork = excluded.is_fork,
    status = CASE WHEN previews.status IN ('destroyed', 'failed') THEN 'queued'::preview_status ELSE previews.status END,
    destroyed_at = NULL,
    updated_at = now()
RETURNING *;

-- name: GetPreviewByID :one
SELECT * FROM previews WHERE id = $1;

-- name: GetPreviewByIdentity :one
SELECT * FROM previews
WHERE application_id = $1 AND provider = $2 AND pr_id = $3;

-- name: SetPreviewStatus :exec
UPDATE previews SET status = $2,
    cleanup_error = sqlc.narg(cleanup_error),
    destroyed_at = CASE WHEN $2 = 'destroyed'::preview_status THEN now() ELSE destroyed_at END,
    updated_at = now()
WHERE id = $1;

-- name: SetPreviewFqdn :exec
UPDATE previews SET fqdn = $2, updated_at = now() WHERE id = $1;

-- name: SetPreviewDeployed :exec
UPDATE previews SET status = 'active', last_deployed_at = now(), last_activity_at = now(), updated_at = now()
WHERE id = $1;

-- name: ApprovePreviewFork :execrows
UPDATE previews SET fork_approved_by = $2, fork_approved_at = now(), updated_at = now()
WHERE id = $1 AND is_fork AND fork_approved_at IS NULL;

-- name: CountLivePreviewsForApplication :one
-- The concurrency cap (§20.4.3) counts everything that consumes the server.
SELECT count(*) FROM previews
WHERE application_id = $1 AND status IN ('deploying', 'active');

-- name: ListPreviewsForApplication :many
SELECT * FROM previews
WHERE application_id = $1 AND status <> 'destroyed'
ORDER BY pr_id;

-- name: ListExpiredPreviews :many
-- TTL of inactivity (§20.4.3): based on the last deployment or activity.
SELECT p.* FROM previews p
JOIN applications a ON a.id = p.application_id
WHERE p.status = 'active' AND a.preview_ttl_minutes IS NOT NULL
  AND GREATEST(coalesce(p.last_activity_at, p.last_deployed_at), p.last_deployed_at)
      < now() - make_interval(mins => a.preview_ttl_minutes);

-- name: ListQueuedPreviews :many
-- Promoted by the scheduler when capacity frees up (§20.4.3).
SELECT * FROM previews WHERE status = 'queued' ORDER BY updated_at
LIMIT 50;

-- name: ListPreviewEnvVars :many
-- The DEDICATED preview variable set (INV-010): production secrets are never
-- copied implicitly.
SELECT * FROM environment_variables
WHERE resource_id = $1 AND is_preview = true
ORDER BY key;

-- name: GetPreviewByUUIDForTeam :one
-- Team ownership travels through the application chain (INV-002).
SELECT p.* FROM previews p
JOIN resources r ON r.id = p.application_id
WHERE p.uuid = $1 AND r.team_id = $2;

-- name: UpdateApplicationPreviewSettings :exec
UPDATE applications SET
    previews_enabled = COALESCE(sqlc.narg(previews_enabled), previews_enabled),
    preview_url_template = COALESCE(sqlc.narg(preview_url_template), preview_url_template),
    preview_max_concurrent = CASE WHEN sqlc.arg(set_max_concurrent)::boolean THEN sqlc.narg(preview_max_concurrent) ELSE preview_max_concurrent END,
    preview_ttl_minutes = CASE WHEN sqlc.arg(set_ttl)::boolean THEN sqlc.narg(preview_ttl_minutes) ELSE preview_ttl_minutes END,
    preview_protection = COALESCE(sqlc.narg(preview_protection), preview_protection),
    preview_fork_approval_enabled = COALESCE(sqlc.narg(preview_fork_approval_enabled), preview_fork_approval_enabled),
    preview_exclude_drafts = COALESCE(sqlc.narg(preview_exclude_drafts), preview_exclude_drafts)
WHERE id = $1;
