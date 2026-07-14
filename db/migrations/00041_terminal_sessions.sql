-- Web terminal sessions (data-dictionary §10.6, PRD §5.7/§24.4, ADR-024).
--
-- One row per terminal session (xterm.js -> WebSocket -> SSH/PTY). The row
-- is created when the one-time attach token is issued; the WebSocket upgrade
-- claims the token exactly once (claimed_at). Only the token hash is stored
-- (§23.2). Keystrokes are never recorded (§24.4); open and close are audited.
-- Deleted targets go SET NULL — target_name keeps the label as a snapshot.

-- +goose Up
CREATE TYPE terminal_target AS ENUM ('server', 'container');
CREATE TYPE terminal_end_reason AS ENUM ('user_close', 'idle_timeout', 'max_duration', 'disconnect', 'revoked');

CREATE TABLE terminal_sessions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id bigint REFERENCES users (id) ON DELETE SET NULL,
    target_kind terminal_target NOT NULL,
    server_id bigint REFERENCES servers (id) ON DELETE SET NULL,
    resource_id bigint REFERENCES resources (id) ON DELETE SET NULL,
    target_name text NOT NULL,
    client_ip inet,
    token_hash text NOT NULL UNIQUE,
    token_expires_at timestamptz NOT NULL,
    claimed_at timestamptz,
    started_at timestamptz NOT NULL DEFAULT now(),
    ended_at timestamptz,
    end_reason terminal_end_reason,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX terminal_sessions_team_id_idx ON terminal_sessions (team_id);

-- +goose Down
DROP TABLE terminal_sessions;
DROP TYPE terminal_end_reason;
DROP TYPE terminal_target;
