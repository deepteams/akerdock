-- Servers (§3, state machine §21.2).

-- name: CreateServer :one
INSERT INTO servers (team_id, name, description, host, port, ssh_user, ssh_timeout_seconds, private_key_id, is_build_server, wildcard_domain, proxy_type, proxy_http_port, proxy_https_port, dns_credential_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING *;

-- name: GetServerByUUID :one
SELECT * FROM servers WHERE uuid = $1 AND team_id = $2 AND deleted_at IS NULL;

-- name: GetServerByID :one
SELECT * FROM servers WHERE id = $1 AND deleted_at IS NULL;

-- name: ListServersPage :many
SELECT * FROM servers
WHERE team_id = sqlc.arg(team_id) AND deleted_at IS NULL
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListReadyServers :many
-- Every server the agent must run on (ADR-052): Docker operations flow
-- through the command channel, so the helper is ensured on ALL ready servers
-- — build servers and database-only hosts included, proxy or not.
SELECT * FROM servers WHERE deleted_at IS NULL AND status = 'ready';

-- name: UpdateServer :execrows
UPDATE servers SET
    name = $2, description = $3, host = $4, port = $5, ssh_user = $6,
    ssh_timeout_seconds = $7, private_key_id = $8, is_build_server = $9,
    wildcard_domain = $10, proxy_type = $11, proxy_http_port = $12,
    proxy_https_port = $13, status = $14, dns_credential_id = sqlc.narg(dns_credential_id),
    cleanup_enabled = sqlc.arg(cleanup_enabled),
    cleanup_cron = sqlc.narg(cleanup_cron),
    cleanup_disk_threshold_pct = sqlc.narg(cleanup_disk_threshold_pct),
    cleanup_prune_volumes = sqlc.arg(cleanup_prune_volumes),
    cleanup_prune_networks = sqlc.arg(cleanup_prune_networks),
    -- The schedule may have changed: the scheduler recomputes the window.
    cleanup_next_run_at = NULL,
    updated_at = now(), version = version + 1
WHERE id = $1 AND version = sqlc.arg(expected_version) AND deleted_at IS NULL;

-- name: ListCleanupSchedulableServers :many
-- Cleanup-enabled, ready servers (§3.7). The scheduler owns the cron window
-- (cleanup_next_run_at) exactly like the backup plans.
SELECT * FROM servers
WHERE cleanup_enabled AND deleted_at IS NULL AND status = 'ready'
ORDER BY id;

-- name: SetServerCleanupSchedule :exec
-- Scheduler-owned: never bumps `version` (not a user edit).
UPDATE servers
SET cleanup_next_run_at = sqlc.narg(next_run_at),
    cleanup_last_run_at = coalesce(sqlc.narg(last_run_at), cleanup_last_run_at)
WHERE id = $1;

-- name: CanStartServerCleanup :one
-- Reader/writer exclusion for §3.7. The cleanup job is already `running` when
-- it reaches this query. Locking the server row makes this check atomic with
-- StartDeploymentUnlessCleanupRunning: either the cleanup observes an active
-- deployment and defers, or a queued deployment observes the running cleanup
-- and waits before its first mutation. Queued deployments do not block a
-- cleanup because they have not touched the server yet.
WITH locked_server AS MATERIALIZED (
    SELECT servers.id FROM servers
    WHERE servers.id = sqlc.arg(server_id)
    FOR UPDATE
)
SELECT NOT EXISTS (
    SELECT 1 FROM deployments d
    WHERE (d.server_id = ls.id OR d.build_server_id = ls.id)
      AND d.status NOT IN ('queued', 'succeeded', 'failed', 'cancelled', 'superseded')
) AS can_start
FROM locked_server ls;

-- name: SoftDeleteServer :execrows
UPDATE servers SET deleted_at = now(), status = 'deleting', updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: SetServerStatus :exec
UPDATE servers SET status = $2, observed_at = now(),
    unreachable_since = CASE WHEN $2 = 'unreachable'::server_status THEN coalesce(unreachable_since, now()) ELSE NULL END,
    updated_at = now()
WHERE id = $1;

-- name: PinServerHostKey :exec
-- Trust-on-first-use (§20.1): the fingerprint is written only when none is
-- pinned yet. Overwriting it here would defeat the whole point — a changed key
-- must fail the connection, not silently re-pin itself.
UPDATE servers SET host_key_fingerprint = $2, updated_at = now()
WHERE id = $1 AND host_key_fingerprint IS NULL;

-- name: RepinServerHostKey :exec
-- Deliberate re-pin, on an explicit re-validation of a rebuilt server.
UPDATE servers SET host_key_fingerprint = $2, updated_at = now() WHERE id = $1;

-- name: RecordServerFacts :exec
UPDATE servers SET os_name = $2, architecture = $3, docker_version = $4,
    status = $5, observed_at = now(),
    unreachable_since = CASE WHEN $5 = 'unreachable'::server_status THEN coalesce(unreachable_since, now()) ELSE NULL END,
    updated_at = now()
WHERE id = $1;

-- name: CountServersUsingPrivateKey :one
SELECT count(*) FROM servers WHERE private_key_id = $1 AND deleted_at IS NULL;

-- Server inventory (§3): only managed resources appear here (INV-015).

-- name: ListServerResourcesPage :many
SELECT r.uuid, r.name, r.resource_type, r.observed_status,
       e.uuid AS environment_uuid, p.uuid AS project_uuid, r.id
FROM resources r
JOIN destinations d ON d.id = r.destination_id
JOIN environments e ON e.id = r.environment_id
JOIN projects p ON p.id = e.project_id
WHERE d.server_id = sqlc.arg(server_id) AND r.deleted_at IS NULL
  AND (sqlc.arg(after_id)::bigint = 0 OR r.id < sqlc.arg(after_id))
ORDER BY r.id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListServerDomains :many
SELECT r.uuid AS resource_uuid, r.resource_type, dom.fqdn, dom.path, dom.target_port
FROM domains dom
JOIN applications a ON a.id = dom.application_id
JOIN resources r ON r.id = a.id
JOIN destinations d ON d.id = r.destination_id
WHERE d.server_id = $1 AND r.deleted_at IS NULL
ORDER BY r.uuid, dom.fqdn, dom.path;

-- name: ListReadyBuildServers :many
-- The build servers of a team that can actually take a build. A build server
-- that is not ready is not a build server: dispatching to it would fail the
-- deployment for a reason that has nothing to do with the application.
SELECT * FROM servers
WHERE team_id = $1 AND is_build_server AND status = 'ready' AND deleted_at IS NULL
ORDER BY id;

-- name: SetServerCA :exec
UPDATE servers SET ca_cert = $2, ca_key_enc = $3, updated_at = now() WHERE id = $1;

-- name: ListUnvalidatedLocalhostServers :many
-- The seeded localhost server, while it has never passed a validation
-- (instance-config §6.2): the scheduler retries it until the instance key is
-- authorized on the host. Bounded to 24h after creation so a host that will
-- never run SSH does not accumulate a failed job every tick forever — past
-- the window, validation stays a click away in the UI.
SELECT * FROM servers
WHERE is_localhost AND deleted_at IS NULL
  AND docker_version IS NULL
  AND status IN ('pending', 'unreachable')
  AND created_at > now() - interval '24 hours';

-- name: SetProxyDesiredState :exec
-- The operator's intent on the proxy (§3): an explicit stop must survive the
-- drift reconciliation — a proxy someone deliberately stopped is not drift.
UPDATE servers SET proxy_desired_state = $2, updated_at = now() WHERE id = $1;

-- name: SetProxyObservedStatus :exec
UPDATE servers SET proxy_observed_status = $2, updated_at = now() WHERE id = $1;
