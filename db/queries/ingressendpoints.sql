-- Ingress endpoints and their attach sessions (ADR-060). The mint/claim
-- discipline mirrors portforwardsessions.sql, but the claim happens agent-side
-- (the control plane only records what the agent reports), so there is no
-- ClaimIngressSession here: the row is stamped from observations.

-- name: CreateIngressEndpoint :one
INSERT INTO ingress_endpoints (
    team_id, name, description, fqdn, server_id, access, basic_auth_hash, created_by
) VALUES ($1, $2, sqlc.narg(description), $3, $4, $5, sqlc.narg(basic_auth_hash), sqlc.narg(created_by))
RETURNING *;

-- name: GetIngressEndpointByUUID :one
SELECT * FROM ingress_endpoints WHERE uuid = $1 AND team_id = $2;

-- name: GetIngressEndpointByID :one
SELECT * FROM ingress_endpoints WHERE id = $1;

-- name: GetIngressEndpointByUUIDGlobal :one
-- Unscoped by design: the SSO wall's forward-auth is called by Traefik, which
-- has no team — the reference travels in the middleware address (ADR-030).
SELECT * FROM ingress_endpoints WHERE uuid = $1;

-- name: GetIngressEndpointByFQDN :one
-- The authorize step resolves the redirect host to a declared endpoint — the
-- sole anti-open-redirect rule, same as GetResourceByRoutedHost.
SELECT * FROM ingress_endpoints WHERE fqdn = $1;

-- name: CreateIngressAccessToken :exec
INSERT INTO preview_access_tokens (token_hash, ingress_endpoint_id, expires_at)
VALUES ($1, $2, $3);

-- name: ListIngressEndpoints :many
SELECT e.*, s.uuid AS server_uuid, s.name AS server_name
FROM ingress_endpoints e
JOIN servers s ON s.id = e.server_id
WHERE e.team_id = $1
ORDER BY lower(e.name);

-- name: UpdateIngressEndpoint :one
-- The FQDN and the server are immutable after declaration: both are baked into
-- the issued certificate and the deposited router. Renaming the URL is a
-- delete + declare, which is what it costs everywhere else in the product.
UPDATE ingress_endpoints
SET name = $3,
    description = sqlc.narg(description),
    access = $4,
    basic_auth_hash = sqlc.narg(basic_auth_hash),
    updated_by = sqlc.narg(updated_by),
    updated_at = now(),
    version = version + 1
WHERE uuid = $1 AND team_id = $2
RETURNING *;

-- name: DeleteIngressEndpoint :one
DELETE FROM ingress_endpoints WHERE uuid = $1 AND team_id = $2 RETURNING *;

-- name: CreateIngressDomain :one
INSERT INTO domains (uuid, ingress_endpoint_id, fqdn, path, is_generated)
VALUES ($1, $2, $3, '/', false)
RETURNING *;

-- name: CreateIngressSession :one
INSERT INTO ingress_tunnel_sessions (
    team_id, endpoint_id, user_id, client_ip, token_hash, token_expires_at
) VALUES ($1, $2, sqlc.narg(user_id), sqlc.narg(client_ip), $3, $4)
RETURNING *;

-- name: GetOpenIngressSessionByUUID :one
SELECT * FROM ingress_tunnel_sessions WHERE uuid = $1 AND ended_at IS NULL;

-- name: CountOpenIngressSessions :one
SELECT count(*) FROM ingress_tunnel_sessions
WHERE team_id = $1 AND ended_at IS NULL;

-- name: GetOpenIngressSessionForEndpoint :one
-- The occupancy probe (ADR-060 §6): the partial unique index makes at most one
-- row match. "Open" means not ended — an unclaimed, unexpired mint occupies.
SELECT s.*, u.email AS user_email
FROM ingress_tunnel_sessions s
LEFT JOIN users u ON u.id = s.user_id
WHERE s.endpoint_id = $1 AND s.ended_at IS NULL;

-- name: MarkIngressSessionClaimed :execrows
-- Stamped from the agent's claim observation, never from an HTTP redeem: the
-- socket lives agent-side. Idempotent against replayed observations.
UPDATE ingress_tunnel_sessions
SET claimed_at = now(), started_at = now(), last_seen_at = now()
WHERE uuid = $1 AND claimed_at IS NULL AND ended_at IS NULL;

-- name: TouchIngressSession :execrows
-- Agent-reported liveness. Zero rows means the durable session was finalized
-- (operator close, sweep) and the caller must cut the socket.
UPDATE ingress_tunnel_sessions
SET last_seen_at = now()
WHERE uuid = $1 AND claimed_at IS NOT NULL AND ended_at IS NULL;

-- name: EndIngressSession :execrows
UPDATE ingress_tunnel_sessions
SET ended_at = now(), end_reason = $2
WHERE id = $1 AND ended_at IS NULL;

-- name: EndIngressSessionByUUID :one
UPDATE ingress_tunnel_sessions
SET ended_at = now(), end_reason = $3
WHERE uuid = $1 AND team_id = $2 AND ended_at IS NULL
RETURNING *;

-- name: ListIngressSessionsPage :many
SELECT s.*, u.email AS user_email, e.uuid AS endpoint_uuid, e.name AS endpoint_name, e.fqdn AS endpoint_fqdn
FROM ingress_tunnel_sessions s
LEFT JOIN users u ON u.id = s.user_id
LEFT JOIN ingress_endpoints e ON e.id = s.endpoint_id
WHERE s.team_id = $1
  AND s.id < sqlc.arg(before_id)::bigint
  AND (sqlc.narg(endpoint_id)::bigint IS NULL OR s.endpoint_id = sqlc.narg(endpoint_id))
  AND (NOT sqlc.arg(active_only)::boolean
       OR (s.ended_at IS NULL
           AND ((s.claimed_at IS NULL AND s.token_expires_at > now())
                OR (s.claimed_at IS NOT NULL
                    AND (s.last_seen_at IS NULL OR s.last_seen_at > now() - interval '90 seconds')))))
ORDER BY s.id DESC
LIMIT $2;

-- name: SweepIngressSessions :many
-- Leader-side finalization of rows whose socket can no longer answer for
-- itself: an unclaimed token past its TTL, a claimed session whose agent went
-- silent (no report for 90 s), or one past the 12 h ceiling (ADR-060 §6).
-- The derived reason keeps the audit line and the CLI message coherent.
UPDATE ingress_tunnel_sessions
SET ended_at = now(),
    end_reason = CASE
        WHEN claimed_at IS NULL THEN 'user_close'::terminal_end_reason
        WHEN started_at <= now() - interval '12 hours' THEN 'max_duration'::terminal_end_reason
        ELSE 'disconnect'::terminal_end_reason
    END
WHERE ended_at IS NULL
  AND (
    (claimed_at IS NULL AND token_expires_at <= now())
    OR (claimed_at IS NOT NULL AND started_at <= now() - interval '12 hours')
    OR (claimed_at IS NOT NULL AND last_seen_at IS NOT NULL AND last_seen_at <= now() - interval '90 seconds')
  )
RETURNING *;

-- name: PurgeIngressSessions :execrows
DELETE FROM ingress_tunnel_sessions
WHERE ended_at IS NOT NULL AND ended_at < now() - interval '30 days';
