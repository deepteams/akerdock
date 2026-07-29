-- Audit log (§23.4): strictly append-only — no UPDATE or single DELETE
-- query must ever exist against this table.

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (team_id, actor_kind, actor_uuid, actor_display, action, target_kind, target_uuid, target_name, result, ip, user_agent, request_id, correlation_id, diff_redacted)
VALUES ($1, $2, $3, $4, $5, $6, $7, sqlc.narg(target_name), $8, $9, $10, $11, $12, sqlc.narg(diff_redacted));

-- name: ResolveAuditTargetName :one
-- The display name of what an action touched, read at the moment it is audited
-- so the trail keeps the name the resource had THEN (see 00084).
--
-- One statement rather than fifteen: each branch is gated by the target kind, so
-- PostgreSQL discards the others on a constant one-time filter and only the
-- matching table is probed, by its unique index on uuid. Best-effort by
-- construction — an unknown kind or a row already gone simply yields nothing,
-- and the trail keeps the uuid it already has.
SELECT name FROM (
    SELECT resources.name FROM resources
        WHERE resources.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text IN ('application', 'database', 'service', 'resource', 'preview')
    UNION ALL SELECT servers.name FROM servers
        WHERE servers.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'server'
    UNION ALL SELECT projects.name FROM projects
        WHERE projects.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'project'
    UNION ALL SELECT environments.name FROM environments
        WHERE environments.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'environment'
    UNION ALL SELECT teams.name FROM teams
        WHERE teams.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'team'
    UNION ALL SELECT users.name FROM users
        WHERE users.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'user'
    UNION ALL SELECT notification_channels.name FROM notification_channels
        WHERE notification_channels.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'notification_channel'
    UNION ALL SELECT scheduled_tasks.name FROM scheduled_tasks
        WHERE scheduled_tasks.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'scheduled_task'
    UNION ALL SELECT custom_roles.name FROM custom_roles
        WHERE custom_roles.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'role'
    UNION ALL SELECT registry_credentials.name FROM registry_credentials
        WHERE registry_credentials.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'registry_credential'
    UNION ALL SELECT cloud_credentials.name FROM cloud_credentials
        WHERE cloud_credentials.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'dns_credential'
    UNION ALL SELECT github_apps.name FROM github_apps
        WHERE github_apps.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'github_app'
    UNION ALL SELECT uptime_checks.name FROM uptime_checks
        WHERE uptime_checks.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'uptime_check'
    UNION ALL SELECT api_tokens.name FROM api_tokens
        WHERE api_tokens.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'api_token'
    UNION ALL SELECT scim_tokens.name FROM scim_tokens
        WHERE scim_tokens.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'scim_token'
    UNION ALL SELECT external_endpoints.name FROM external_endpoints
        WHERE external_endpoints.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'external_endpoint'
    UNION ALL SELECT service_components.name FROM service_components
        WHERE service_components.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'service_component'
    UNION ALL SELECT private_keys.name FROM private_keys
        WHERE private_keys.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'private_key'
    UNION ALL SELECT git_sources.name FROM git_sources
        WHERE git_sources.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text IN ('git_source', 'source')
    UNION ALL SELECT s3_storages.name FROM s3_storages
        WHERE s3_storages.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text IN ('s3_storage', 'storage')
    -- An invitation has no name; the invited address is what a reader needs.
    UNION ALL SELECT invitations.email::text AS name FROM invitations
        WHERE invitations.uuid = sqlc.arg(target_uuid) AND sqlc.arg(target_kind)::text = 'invitation'
) AS candidates
LIMIT 1;

-- name: InsertOutboxEvent :exec
INSERT INTO outbox_events (uuid, event_type, team_uuid, resource_uuid, actor, aggregate_key, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: CountAuditEvents :one
SELECT count(*) FROM audit_events;

-- name: ListInstanceAuditEventsPage :many
-- Instance-wide audit (reserved to the instance root): every team AND the
-- system/instance actions that have no team_id (encryption rotation, instance
-- settings…), which no team-scoped view can show. Same optional filters.
SELECT * FROM audit_events
WHERE (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
  AND (sqlc.narg(action)::text IS NULL OR action = sqlc.narg(action))
  AND (sqlc.narg(result)::audit_result IS NULL OR result = sqlc.narg(result))
  AND (sqlc.narg(actor_uuid)::uuid IS NULL OR actor_uuid = sqlc.narg(actor_uuid))
  AND (sqlc.narg(from_time)::timestamptz IS NULL OR occurred_at >= sqlc.narg(from_time))
  AND (sqlc.narg(to_time)::timestamptz IS NULL OR occurred_at <= sqlc.narg(to_time))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: PurgeAuditEvents :one
-- Retention purge (§23.4): removes audit rows older than retention_days. Goes
-- through the SQL function (not a direct DELETE) — the function is the only
-- sanctioned path past the append-only trigger, and it caps the deletion to
-- aged-out rows. retention_days <= 0 keeps everything. Returns rows removed.
SELECT purge_audit_events(sqlc.arg(retention_days)::integer);

-- name: ListAuditEventsPage :many
-- Read side of the audit trail (§23.4: paginé, filtrable, exportable). A SELECT
-- does not violate the append-only rule. Team-scoped; optional filters on action,
-- result, actor, target and an occurred_at window; cursor by descending id.
SELECT * FROM audit_events
WHERE team_id = sqlc.arg(team_id)
  AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
  AND (sqlc.narg(action)::text IS NULL OR action = sqlc.narg(action))
  -- A feature's trail spans several action names (open, close, grant, revoke),
  -- so the prefix filter is a list: one story rather than four partial lists.
  AND (sqlc.narg(action_prefixes)::text[] IS NULL
       OR action LIKE ANY (SELECT p || '%' FROM unnest(sqlc.narg(action_prefixes)::text[]) AS p))
  AND (sqlc.narg(result)::audit_result IS NULL OR result = sqlc.narg(result))
  AND (sqlc.narg(actor_uuid)::uuid IS NULL OR actor_uuid = sqlc.narg(actor_uuid))
  AND (sqlc.narg(target_uuid)::uuid IS NULL OR target_uuid = sqlc.narg(target_uuid))
  AND (sqlc.narg(from_time)::timestamptz IS NULL OR occurred_at >= sqlc.narg(from_time))
  AND (sqlc.narg(to_time)::timestamptz IS NULL OR occurred_at <= sqlc.narg(to_time))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- Outbox publisher (§18.2, §24.2): events are published in commit order.

-- name: ClaimUnpublishedOutboxEvents :many
UPDATE outbox_events SET published_at = now(), publish_attempts = publish_attempts + 1
WHERE id IN (
    SELECT id FROM outbox_events
    WHERE published_at IS NULL
    ORDER BY id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: ListOutboxEventsForTeamAfter :many
SELECT * FROM outbox_events
WHERE team_uuid = $1 AND id > $2 AND published_at IS NOT NULL
ORDER BY id
LIMIT $3;

-- name: PurgePublishedOutboxEvents :execrows
-- Never purge past the notification cursor: an event the dispatcher has not
-- read yet would be a notification silently lost (the 7-day window makes this
-- unlikely, not impossible — a long outage is exactly when alerts matter).
DELETE FROM outbox_events
WHERE published_at IS NOT NULL AND published_at < now() - interval '7 days'
  AND id <= (SELECT last_outbox_event_id FROM notification_cursor);
