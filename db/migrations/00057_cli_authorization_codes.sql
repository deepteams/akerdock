-- CLI login requests (data-dictionary §10.8, ADR-031). Ephemeral: created by
-- /auth/cli/start, bound to a user/team by /auth/cli/approve, consumed by
-- /auth/cli/token. Only hashes are stored; neither the verifier nor the minted
-- token is kept here. Purged after consumption or expiry.

-- +goose Up
CREATE TABLE cli_authorization_codes (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id_hash text NOT NULL UNIQUE,
    challenge text NOT NULL,
    user_code text NOT NULL,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'consumed')),
    user_id bigint REFERENCES users (id) ON DELETE CASCADE,
    team_id bigint REFERENCES teams (id) ON DELETE CASCADE,
    permissions text[],
    client_name text,
    client_ip inet,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE cli_authorization_codes;
