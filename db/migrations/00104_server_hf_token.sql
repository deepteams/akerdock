-- A per-server Hugging Face token (ADR-081): enveloped like every stored
-- secret (ADR-003), write-only like a private key (ADR-075) — set, replaced
-- or cleared from the dashboard, never read back. For the engines of that
-- server it wins over the instance-wide AKERDOCK_HF_TOKEN fallback.

-- +goose Up
ALTER TABLE servers ADD COLUMN hf_token_enc bytea;

-- +goose Down
ALTER TABLE servers DROP COLUMN hf_token_enc;
