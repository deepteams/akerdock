-- Application health checks (data-dictionary §8.8): they gate routing and
-- rolling updates (INV-005). A Dockerfile HEALTHCHECK stays authoritative
-- over this configuration (§5.3).

-- +goose Up
CREATE TABLE health_checks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    resource_id bigint NOT NULL UNIQUE REFERENCES resources (id) ON DELETE CASCADE,
    enabled boolean NOT NULL DEFAULT false,
    method text NOT NULL DEFAULT 'GET',
    path text NOT NULL DEFAULT '/',
    port integer CHECK (port BETWEEN 1 AND 65535),
    interval_seconds integer NOT NULL DEFAULT 30 CHECK (interval_seconds > 0),
    timeout_seconds integer NOT NULL DEFAULT 5 CHECK (timeout_seconds > 0),
    retries integer NOT NULL DEFAULT 3 CHECK (retries > 0),
    start_period_seconds integer NOT NULL DEFAULT 5 CHECK (start_period_seconds >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE health_checks;
