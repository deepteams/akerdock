-- Notifications (§11, ADR-019).

-- name: ListNotificationChannelsPage :many
SELECT * FROM notification_channels
WHERE team_id = $1 AND (sqlc.arg(after_id)::bigint = 0 OR id < sqlc.arg(after_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- name: GetNotificationChannelByUUID :one
SELECT * FROM notification_channels WHERE uuid = $1 AND team_id = $2;

-- name: GetNotificationChannelByID :one
SELECT * FROM notification_channels WHERE id = $1;

-- name: CreateNotificationChannel :one
INSERT INTO notification_channels (uuid, team_id, kind, name, config_enc, enabled)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateNotificationChannel :execrows
UPDATE notification_channels
SET name = $2, config_enc = $3, enabled = $4, updated_at = now(), version = version + 1
WHERE id = $1 AND version = sqlc.arg(expected_version);

-- name: DeleteNotificationChannel :execrows
DELETE FROM notification_channels WHERE id = $1;

-- name: ListNotificationRules :many
SELECT * FROM notification_rules WHERE channel_id = $1 ORDER BY id;

-- name: GetNotificationRuleByUUID :one
SELECT r.* FROM notification_rules r WHERE r.uuid = $1 AND r.channel_id = $2;

-- name: CreateNotificationRule :one
INSERT INTO notification_rules (uuid, channel_id, event_type, enabled, project_id, environment_id,
                                min_severity, debounce_seconds, quiet_hours_start, quiet_hours_end,
                                digest_enabled, digest_interval_minutes)
VALUES ($1, $2, $3, $4, sqlc.narg(project_id), sqlc.narg(environment_id),
        $5, $6, sqlc.narg(quiet_hours_start), sqlc.narg(quiet_hours_end), $7, $8)
RETURNING *;

-- name: DeleteNotificationRule :execrows
DELETE FROM notification_rules WHERE id = $1;

-- name: ListNotificationChannelsToRotate :many
SELECT id, uuid, config_enc FROM notification_channels
WHERE (get_byte(config_enc, 0) << 24 | get_byte(config_enc, 1) << 16 | get_byte(config_enc, 2) << 8 | get_byte(config_enc, 3)) <> sqlc.arg(active_version)::int
ORDER BY id
LIMIT $1;

-- name: RotateNotificationChannelEnc :exec
UPDATE notification_channels SET config_enc = $2 WHERE id = $1;

-- --- dispatcher --------------------------------------------------------------

-- name: GetNotificationCursor :one
SELECT last_outbox_event_id FROM notification_cursor WHERE id;

-- name: SetNotificationCursor :exec
UPDATE notification_cursor SET last_outbox_event_id = $1 WHERE id;

-- name: ListOutboxEventsAfter :many
-- Every event, whatever its team: the rules decide who hears about it.
SELECT * FROM outbox_events
WHERE id > $1
ORDER BY id
LIMIT $2;

-- name: MatchNotificationRules :many
-- The rules that hear this event type, for this team, honouring the
-- project/environment scoping (NULL = the whole team).
SELECT r.*, c.kind, c.team_id FROM notification_rules r
JOIN notification_channels c ON c.id = r.channel_id
JOIN teams t ON t.id = c.team_id
WHERE r.enabled AND c.enabled
  AND r.event_type = sqlc.arg(event_type)
  AND t.uuid = sqlc.arg(team_uuid)
  AND (r.project_id IS NULL OR r.project_id = sqlc.narg(project_id))
  AND (r.environment_id IS NULL OR r.environment_id = sqlc.narg(environment_id))
ORDER BY r.id;

-- name: CreateNotificationDelivery :one
-- ON CONFLICT DO NOTHING: re-reading an outbox event must never notify twice.
INSERT INTO notification_deliveries (rule_id, channel_id, outbox_event_id)
VALUES ($1, $2, $3)
ON CONFLICT (rule_id, outbox_event_id) DO NOTHING
RETURNING *;

-- name: LastSentDelivery :one
SELECT sent_at FROM notification_deliveries
WHERE rule_id = $1 AND status = 'sent' AND sent_at IS NOT NULL
ORDER BY sent_at DESC
LIMIT 1;

-- name: FinishNotificationDelivery :exec
UPDATE notification_deliveries
SET status = $2, attempts = attempts + 1, last_error = $3, suppressed_reason = $4,
    sent_at = CASE WHEN $2 = 'sent' THEN now() ELSE sent_at END
WHERE id = $1;

-- name: CountSuppressedSince :one
-- How many events this rule swallowed since its last send — an aggregated
-- alert must be able to say "and 12 others" rather than hide them (ADR-019).
SELECT count(*) FROM notification_deliveries
WHERE rule_id = $1 AND status = 'suppressed' AND created_at > coalesce(sqlc.narg(since)::timestamptz, '-infinity');

-- name: ResolveProjectEnvironmentOfResource :one
-- Which project/environment a resource belongs to, for rule scoping.
SELECT e.id AS environment_id, p.id AS project_id FROM resources r
JOIN environments e ON e.id = r.environment_id
JOIN projects p ON p.id = e.project_id
WHERE r.uuid = $1;

-- name: GetProjectByID :one
SELECT * FROM projects WHERE id = $1 AND deleted_at IS NULL;

-- name: GetEnvironmentByID :one
SELECT * FROM environments WHERE id = $1 AND deleted_at IS NULL;

-- --- deferred digest (ADR-019 §4) --------------------------------------------

-- name: ListDigestRulesDue :many
-- Digest rules whose window has elapsed and that have something to say. A rule
-- with nothing pending is not woken up: an empty digest is noise.
SELECT r.*, c.kind, c.team_id FROM notification_rules r
JOIN notification_channels c ON c.id = r.channel_id
WHERE r.enabled AND c.enabled AND r.digest_enabled
  AND EXISTS (
    SELECT 1 FROM notification_deliveries d
    WHERE d.rule_id = r.id AND d.status = 'pending'
      AND d.created_at < now() - make_interval(mins => r.digest_interval_minutes)
  )
ORDER BY r.id;

-- name: ListPendingDigestDeliveries :many
-- What the digest of this rule stands for.
SELECT d.id, d.outbox_event_id, e.event_type, e.resource_uuid, e.occurred_at
FROM notification_deliveries d
JOIN outbox_events e ON e.id = d.outbox_event_id
WHERE d.rule_id = $1 AND d.status = 'pending'
ORDER BY d.id;

-- name: MarkDigestDeliveriesSent :exec
UPDATE notification_deliveries
SET status = 'sent', attempts = attempts + 1, sent_at = now()
WHERE id = ANY(sqlc.arg(delivery_ids)::bigint[]);

-- name: MarkDigestDeliveriesFailed :exec
UPDATE notification_deliveries
SET status = 'failed', attempts = attempts + 1, last_error = sqlc.narg(last_error)
WHERE id = ANY(sqlc.arg(delivery_ids)::bigint[]);

-- name: SetRuleDigestFlushed :exec
UPDATE notification_rules SET last_digest_at = now() WHERE id = $1;
