-- HTTP idempotency keys (§24.1): replaying the same key with the same body
-- returns the original response; the same key with a different body is a
-- conflict. Kept at least 24 h, then purged by retention.

-- +goose Up
CREATE TABLE idempotency_keys (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    key text NOT NULL,
    -- The endpoint the key was used on: the same key on another endpoint is
    -- a distinct operation.
    endpoint text NOT NULL,
    request_hash text NOT NULL,
    status_code integer,
    response_body jsonb,
    completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (team_id, key, endpoint)
);

CREATE INDEX idempotency_keys_created_at_idx ON idempotency_keys (created_at);

-- +goose Down
DROP TABLE idempotency_keys;
