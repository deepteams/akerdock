-- Built-in MCP server (ADR-043): OAuth 2.1 for remote clients. Everything
-- here is read-only in effect — a grant only ever reads one team's inventory.

-- name: RegisterMcpOauthClient :one
INSERT INTO mcp_oauth_clients (client_id, client_name, redirect_uris)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetMcpOauthClient :one
SELECT * FROM mcp_oauth_clients WHERE client_id = $1;

-- name: CreateMcpOauthCode :exec
-- Single-use authorization code, PKCE challenge attached (ADR-043 §3).
INSERT INTO mcp_oauth_codes (code_hash, client_id, user_id, team_id, redirect_uri, code_challenge, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: TakeMcpOauthCode :one
-- Consumes the code: a replay finds nothing (DELETE … RETURNING).
DELETE FROM mcp_oauth_codes WHERE code_hash = $1 AND expires_at > now()
RETURNING *;

-- name: DeleteExpiredMcpOauthCodes :exec
DELETE FROM mcp_oauth_codes WHERE expires_at <= now();

-- name: CreateMcpAccessToken :one
INSERT INTO mcp_access_tokens (token_hash, client_id, client_name, user_id, team_id, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetMcpAccessTokenByHash :one
SELECT * FROM mcp_access_tokens
WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now();

-- name: TouchMcpAccessToken :exec
UPDATE mcp_access_tokens SET last_used_at = now() WHERE id = $1;

-- name: ListMcpAccessTokensForTeam :many
SELECT * FROM mcp_access_tokens
WHERE team_id = $1 AND revoked_at IS NULL AND expires_at > now()
ORDER BY created_at DESC
LIMIT 100;

-- name: RevokeMcpAccessToken :execrows
UPDATE mcp_access_tokens SET revoked_at = now()
WHERE uuid = $1 AND team_id = $2 AND revoked_at IS NULL;

-- name: SetInstanceMcpEnabled :exec
UPDATE instance_settings SET mcp_enabled = $1, updated_at = now() WHERE id = true;

-- name: SetInstanceMcpDcrEnabled :exec
UPDATE instance_settings SET mcp_dcr_enabled = $1, updated_at = now() WHERE id = true;
