-- Audit log (§23.4): strictly append-only — no UPDATE or single DELETE
-- query must ever exist against this table.

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (team_id, actor_kind, actor_uuid, actor_display, action, target_kind, target_uuid, result, ip, user_agent, request_id, correlation_id, diff_redacted)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, sqlc.narg(diff_redacted));

-- name: InsertOutboxEvent :exec
INSERT INTO outbox_events (uuid, event_type, team_uuid, resource_uuid, actor, aggregate_key, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: CountAuditEvents :one
SELECT count(*) FROM audit_events;

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
