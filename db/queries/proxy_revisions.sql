-- Proxy config revisions (§6.2–§6.4).

-- name: CreateProxyRevision :one
INSERT INTO proxy_config_revisions (server_id, revision, scope, proxy_type, checksum_sha256, content)
VALUES ($1,
        (SELECT coalesce(max(revision), 0) + 1 FROM proxy_config_revisions WHERE server_id = $1),
        $2, $3, $4, $5)
RETURNING *;

-- name: MarkProxyRevisionApplied :exec
UPDATE proxy_config_revisions SET status = 'applied', applied_at = now() WHERE id = $1;

-- name: MarkProxyRevisionFailed :exec
UPDATE proxy_config_revisions SET status = 'failed', error = $2 WHERE id = $1;

-- name: MarkProxyRevisionRolledBack :exec
UPDATE proxy_config_revisions SET status = 'rolled_back' WHERE id = $1;

-- name: GetLastAppliedProxyRevision :one
SELECT * FROM proxy_config_revisions
WHERE server_id = $1 AND scope = $2 AND status = 'applied'
ORDER BY id DESC
LIMIT 1;

-- name: ListServersWithProxy :many
-- Servers whose routing the reconciler converges. A proxy the operator
-- DELIBERATELY stopped (§3) is excluded: re-applying its files would be
-- harmless, but the drift loop exists to repair accidents — and an intent is
-- not an accident.
SELECT * FROM servers
WHERE deleted_at IS NULL AND status = 'ready' AND proxy_type <> 'none'
  AND proxy_desired_state = 'running';

-- name: ListAppliedProxyRevisions :many
-- The current expected state of each scope on a server: its last applied
-- revision (drift reconciliation, §6.2.4).
SELECT DISTINCT ON (scope) * FROM proxy_config_revisions
WHERE server_id = $1 AND status = 'applied'
ORDER BY scope, id DESC;
