-- Audit log (§23.4): strictly append-only — no UPDATE or single DELETE
-- query must ever exist against this table.

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (team_id, actor_kind, actor_uuid, actor_display, action, target_kind, target_uuid, result, ip, user_agent, request_id, diff_redacted)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, sqlc.narg(diff_redacted));

-- name: InsertOutboxEvent :exec
INSERT INTO outbox_events (uuid, event_type, team_uuid, resource_uuid, actor, aggregate_key, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: CountAuditEvents :one
SELECT count(*) FROM audit_events;

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
