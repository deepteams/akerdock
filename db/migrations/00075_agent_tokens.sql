-- ADR-040 phase 1: per-server agent tokens. The server helper (waker mode)
-- pushes outbound observations to the control plane; it authenticates with a
-- token scoped to exactly one server, injected at container (re)creation by
-- the SSH provisioning. `token_hash` authenticates ingestion (SHA-256, like
-- api_tokens); `token_enc` keeps the plaintext under envelope encryption so
-- the idempotent ensure command can re-inject the SAME token on every pass
-- without a rotation. `last_seen_at` surfaces a silent agent (egress blocked,
-- old image) — a signal, never an outage. Expand-only.

-- +goose Up
CREATE TABLE agent_tokens (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    server_id bigint NOT NULL UNIQUE REFERENCES servers (id) ON DELETE CASCADE,
    token_hash text NOT NULL UNIQUE,
    token_enc bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    rotated_at timestamptz,
    last_seen_at timestamptz
);

-- +goose Down
DROP TABLE agent_tokens;
