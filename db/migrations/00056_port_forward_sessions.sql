-- CLI TCP tunnel sessions (data-dictionary §10.7, ADR-032). Same lifecycle as
-- terminal_sessions: the row is created when the one-time attach token is
-- issued, the WebSocket upgrade claims it exactly once (claimed_at), only the
-- token hash is stored (§23.2), open and close are audited. The target
-- (container, port) is fixed at creation. Reuses terminal_end_reason.

-- +goose Up
CREATE TABLE port_forward_sessions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id bigint REFERENCES users (id) ON DELETE SET NULL,
    server_id bigint REFERENCES servers (id) ON DELETE SET NULL,
    resource_id bigint REFERENCES resources (id) ON DELETE SET NULL,
    preview_id bigint REFERENCES previews (id) ON DELETE SET NULL,
    target_name text NOT NULL,
    target_component text,
    target_port integer NOT NULL CHECK (target_port BETWEEN 1 AND 65535),
    client_ip inet,
    token_hash text NOT NULL UNIQUE,
    token_expires_at timestamptz NOT NULL,
    claimed_at timestamptz,
    started_at timestamptz NOT NULL DEFAULT now(),
    ended_at timestamptz,
    end_reason terminal_end_reason,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX port_forward_sessions_team_id_idx ON port_forward_sessions (team_id);

-- +goose Down
DROP TABLE port_forward_sessions;
