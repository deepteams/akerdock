-- Per-server agent tokens (ADR-040): observations-only credentials for the
-- server helper. One row per server; the plaintext survives encrypted so the
-- idempotent provisioning re-injects the same token at every ensure pass.

-- name: GetAgentTokenByServerID :one
SELECT * FROM agent_tokens WHERE server_id = $1;

-- name: GetAgentTokenByHash :one
SELECT * FROM agent_tokens WHERE token_hash = $1;

-- name: CreateAgentToken :one
-- The uuid is generated app-side: it is the envelope-encryption context of
-- token_enc, so a replaced row MUST carry the uuid its ciphertext was bound
-- to — hence uuid = excluded.uuid on conflict.
INSERT INTO agent_tokens (uuid, server_id, token_hash, token_enc)
VALUES ($1, $2, $3, $4)
ON CONFLICT (server_id) DO UPDATE SET
    uuid = excluded.uuid,
    token_hash = excluded.token_hash,
    token_enc = excluded.token_enc,
    rotated_at = now()
RETURNING *;

-- name: TouchAgentTokenSeen :exec
UPDATE agent_tokens SET last_seen_at = now() WHERE id = $1;
