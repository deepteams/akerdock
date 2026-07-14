-- Incoming Git webhooks (§20.3, INV-009).

-- name: CreateWebhookEndpoint :one
INSERT INTO webhook_endpoints (uuid, application_id, provider, secret_enc)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetWebhookEndpointByUUID :one
-- Resolution at reception: the endpoint carries everything needed to verify
-- the signature and route the delivery.
SELECT e.*, r.team_id, r.uuid AS application_uuid
FROM webhook_endpoints e
JOIN applications a ON a.id = e.application_id
JOIN resources r ON r.id = a.id
WHERE e.uuid = $1 AND e.enabled AND r.deleted_at IS NULL;

-- name: GetWebhookEndpointForApplication :one
SELECT * FROM webhook_endpoints WHERE application_id = $1 AND provider = $2;

-- name: DeleteWebhookEndpoint :execrows
DELETE FROM webhook_endpoints WHERE id = $1;

-- name: CreateWebhookDelivery :one
-- ON CONFLICT DO NOTHING: a redelivery keeps the provider's delivery id, so the
-- unique constraint absorbs it — no second row, no second deployment.
INSERT INTO webhook_deliveries (provider, delivery_id, webhook_endpoint_id, event_type,
                                signature_valid, status, payload, team_id, application_id)
VALUES ($1, $2, sqlc.narg(webhook_endpoint_id), sqlc.narg(event_type),
        $3, $4, sqlc.narg(payload), sqlc.narg(team_id), sqlc.narg(application_id))
ON CONFLICT (provider, delivery_id) DO NOTHING
RETURNING *;

-- name: FinishWebhookDelivery :exec
UPDATE webhook_deliveries
SET status = $2, ignore_reason = sqlc.narg(ignore_reason), processed_at = now()
WHERE id = $1;

-- name: GetWebhookDeliveryByID :one
SELECT * FROM webhook_deliveries WHERE id = $1;

-- name: ListWebhookEndpointsToRotate :many
SELECT id, uuid, secret_enc FROM webhook_endpoints
WHERE (get_byte(secret_enc, 0) << 24 | get_byte(secret_enc, 1) << 16 | get_byte(secret_enc, 2) << 8 | get_byte(secret_enc, 3)) <> sqlc.arg(active_version)::int
ORDER BY id
LIMIT $1;

-- name: RotateWebhookEndpointEnc :exec
UPDATE webhook_endpoints SET secret_enc = $2 WHERE id = $1;

-- name: PurgeWebhookDeliveries :execrows
-- Retention bounds the dedup window: purging too aggressively would reopen the
-- replay window (INV-009), hence 30 days minimum.
DELETE FROM webhook_deliveries WHERE received_at < now() - interval '30 days';
