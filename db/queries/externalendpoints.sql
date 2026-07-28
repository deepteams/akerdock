-- External endpoints and their access grants (ADR-045): declared bastion
-- targets, and the bounded re-authenticated windows during which a user may
-- mint tunnels to them.

-- name: CreateExternalEndpoint :one
INSERT INTO external_endpoints (
    team_id, name, description, host, port, server_id,
    project_id, environment_id, criticality, max_grant_minutes, created_by
) VALUES (
    $1, $2, sqlc.narg(description), $3, $4, $5,
    sqlc.narg(project_id), sqlc.narg(environment_id), $6, $7, sqlc.narg(created_by)
)
RETURNING *;

-- name: UpdateExternalEndpoint :one
UPDATE external_endpoints
SET name = $2,
    description = sqlc.narg(description),
    host = $3,
    port = $4,
    server_id = $5,
    project_id = sqlc.narg(project_id),
    environment_id = sqlc.narg(environment_id),
    criticality = $6,
    max_grant_minutes = $7,
    updated_by = sqlc.narg(updated_by),
    updated_at = now(),
    version = version + 1
WHERE id = $1
RETURNING *;

-- name: DeleteExternalEndpoint :execrows
DELETE FROM external_endpoints WHERE id = $1 AND team_id = $2;

-- name: GetExternalEndpointByUUID :one
SELECT * FROM external_endpoints WHERE uuid = $1 AND team_id = $2;

-- name: GetExternalEndpointByID :one
SELECT * FROM external_endpoints WHERE id = $1;

-- name: ListExternalEndpointsPage :many
SELECT * FROM external_endpoints
WHERE team_id = $1 AND id > sqlc.arg(after_id)::bigint
ORDER BY id
LIMIT sqlc.arg(page_limit)::int;

-- name: CreateExternalEndpointGrant :one
-- A grant is only ever created behind a fresh second factor; `factor` records
-- which one was consumed, and `renewed_from` chains a renewal to the grant it
-- extended so a long chain stays visible in the audit trail.
INSERT INTO external_endpoint_grants (
    endpoint_id, user_id, reason, factor, granted_by, renewed_from, expires_at
) VALUES (
    $1, $2, $3, $4, sqlc.narg(granted_by), sqlc.narg(renewed_from), $5
)
RETURNING *;

-- name: GetLiveExternalEndpointGrant :one
-- The hot path on every mint: the caller's own live grant on this endpoint.
-- Revoked and expired rows are invisible here, which is what makes revocation
-- and expiry take effect without a sweep.
SELECT * FROM external_endpoint_grants
WHERE endpoint_id = $1
  AND user_id = $2
  AND revoked_at IS NULL
  AND expires_at > now()
ORDER BY expires_at DESC
LIMIT 1;

-- name: GetExternalEndpointGrantByUUID :one
SELECT * FROM external_endpoint_grants WHERE uuid = $1;

-- name: ExtendExternalEndpointGrant :one
-- Renewal pushes back an existing window in place. Guarded on the row still
-- being live: a grant that expired between the ceremony and this statement is
-- not renewable, it is a new request (ADR-045 §5).
UPDATE external_endpoint_grants
SET expires_at = $2, reason = $3, factor = $4, requested_at = now()
WHERE id = $1 AND revoked_at IS NULL AND expires_at > now()
RETURNING *;

-- name: RevokeExternalEndpointGrant :one
UPDATE external_endpoint_grants
SET revoked_at = now(), revoked_by = sqlc.narg(revoked_by)
WHERE uuid = $1 AND revoked_at IS NULL
RETURNING *;

-- name: ListExternalEndpointGrantsPage :many
-- Newest first: the audit question is almost always "who has access right
-- now", so the cursor walks ids downwards.
SELECT g.*, u.email AS user_email
FROM external_endpoint_grants g
JOIN users u ON u.id = g.user_id
WHERE g.endpoint_id = $1 AND g.id < sqlc.arg(before_id)::bigint
ORDER BY g.id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: CreateEndpointPortForwardSession :one
-- The endpoint variant of CreatePortForwardSession: no resource, no preview,
-- and an explicit `authorized_until` — the instant the session is actually cut,
-- which the CLI is told at open and reminded of before it lands.
INSERT INTO port_forward_sessions (
    team_id, user_id, server_id, external_endpoint_id, grant_id, target_name,
    target_port, client_ip, token_hash, token_expires_at, authorized_until
) VALUES (
    $1, sqlc.narg(user_id), $2, $3, sqlc.narg(grant_id), $4,
    $5, sqlc.narg(client_ip), $6, $7, sqlc.narg(authorized_until)
)
RETURNING *;

-- name: ListLivePortForwardSessionsByGrant :many
-- Revoking a grant tears down the sessions it opened; this is that set.
SELECT * FROM port_forward_sessions
WHERE grant_id = $1 AND ended_at IS NULL;

-- name: SetSessionTotpVerified :exec
-- TOTP step-up (ADR-045 §5). Deliberately a SEPARATE column from the passkey
-- marker: `mfa_verified_at` means "recent passkey", the root terminal requires
-- that ritual, and letting a TOTP set it would hand every TOTP-only user a root
-- shell.
UPDATE sessions SET totp_verified_at = now() WHERE id = $1;

-- name: SetPortForwardAuthorizedUntil :exec
-- A renewed grant pushes back the deadline of the sessions it opened, so a
-- transfer in flight survives instead of restarting from zero (ADR-045 §5).
UPDATE port_forward_sessions
SET authorized_until = $2
WHERE id = $1 AND ended_at IS NULL;
