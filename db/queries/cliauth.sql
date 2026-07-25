-- CLI login requests (ADR-031, data-dictionary §10.8).

-- name: CreateCliAuthCode :one
INSERT INTO cli_authorization_codes (
    request_id_hash, challenge, user_code, client_name, client_ip, expires_at
) VALUES ($1, $2, $3, sqlc.narg(client_name), sqlc.narg(client_ip), $4)
RETURNING *;

-- name: GetCliAuthCodeByRequestHash :one
SELECT * FROM cli_authorization_codes
WHERE request_id_hash = $1 AND expires_at > now();

-- name: ApproveCliAuthCode :execrows
-- Bind the pending request to the approving user/team and permissions.
UPDATE cli_authorization_codes
SET status = 'approved', user_id = $2, team_id = $3, permissions = $4
WHERE request_id_hash = $1 AND status = 'pending' AND expires_at > now();

-- name: ConsumeCliAuthCode :one
-- Single-use: the token exchange claims the approved request atomically.
UPDATE cli_authorization_codes
SET status = 'consumed'
WHERE request_id_hash = $1 AND status = 'approved' AND expires_at > now()
RETURNING *;

-- name: PurgeCliAuthCodes :execrows
DELETE FROM cli_authorization_codes WHERE expires_at <= now();
