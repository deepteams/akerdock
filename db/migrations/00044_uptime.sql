-- Integrated uptime monitoring (ADR-017, PRD §27.17): simple HTTP/TCP checks
-- run OUTSIDE the monitored workload (from the control plane), with an
-- availability history per check and alerting through the existing
-- notification channels.
--
-- The check row carries its own scheduling window (next_run_at, owned by the
-- scheduler) and its state machine: status flips only after N consecutive
-- results (failure/success thresholds) — the anti-flapping lives in the
-- state, not in the notifier.

-- +goose Up
CREATE TYPE uptime_check_kind AS ENUM ('http', 'tcp');
CREATE TYPE uptime_status AS ENUM ('unknown', 'up', 'down');

CREATE TABLE uptime_checks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    -- Optional link to a resource: the availability history "per resource"
    -- of ADR-017. The check survives nothing — it follows the resource.
    resource_id bigint REFERENCES resources (id) ON DELETE CASCADE,
    name text NOT NULL,
    kind uptime_check_kind NOT NULL,
    -- http: a URL; tcp: host:port. Probed from the control plane.
    target text NOT NULL,
    interval_seconds integer NOT NULL DEFAULT 60 CHECK (interval_seconds >= 10),
    timeout_seconds integer NOT NULL DEFAULT 10 CHECK (timeout_seconds BETWEEN 1 AND 60),
    failure_threshold integer NOT NULL DEFAULT 3 CHECK (failure_threshold > 0),
    success_threshold integer NOT NULL DEFAULT 2 CHECK (success_threshold > 0),
    enabled boolean NOT NULL DEFAULT true,
    status uptime_status NOT NULL DEFAULT 'unknown',
    status_since timestamptz,
    consecutive_failures integer NOT NULL DEFAULT 0,
    consecutive_successes integer NOT NULL DEFAULT 0,
    last_checked_at timestamptz,
    last_latency_ms integer,
    last_error text,
    next_run_at timestamptz,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    version integer NOT NULL DEFAULT 1,
    UNIQUE (team_id, name)
);

CREATE INDEX uptime_checks_team_id_idx ON uptime_checks (team_id, id DESC);

CREATE TABLE uptime_check_results (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    check_id bigint NOT NULL REFERENCES uptime_checks (id) ON DELETE CASCADE,
    checked_at timestamptz NOT NULL DEFAULT now(),
    ok boolean NOT NULL,
    latency_ms integer,
    status_code integer,
    error text
);

CREATE INDEX uptime_check_results_check_id_idx ON uptime_check_results (check_id, id DESC);

-- +goose Down
DROP TABLE uptime_check_results;
DROP TABLE uptime_checks;
DROP TYPE uptime_status;
DROP TYPE uptime_check_kind;
