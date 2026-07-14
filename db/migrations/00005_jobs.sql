-- Durable PostgreSQL job queue (ADR-002, data-dictionary §11.8, state
-- machine §21.3). resource_id has no FK yet: the resources table lands
-- with the application endpoints, which will add the constraint.
-- steps/result/retry_of_id come from the OpenAPI Job schema and were
-- missing from the data dictionary (amended).

-- +goose Up
CREATE TYPE job_status AS ENUM ('scheduled', 'queued', 'leased', 'running', 'retry_wait', 'succeeded', 'cancelled', 'dead_letter');

CREATE TABLE jobs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    queue text NOT NULL DEFAULT 'default',
    job_type text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}',
    status job_status NOT NULL DEFAULT 'queued',
    priority integer NOT NULL DEFAULT 0,
    run_at timestamptz NOT NULL DEFAULT now(),
    attempt integer NOT NULL DEFAULT 0,
    max_attempts integer NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    idempotency_key text,
    lock_key text,
    leased_by text,
    lease_expires_at timestamptz,
    heartbeat_at timestamptz,
    cancel_requested_at timestamptz,
    last_error text,
    steps jsonb NOT NULL DEFAULT '[]',
    result jsonb,
    retry_of_id bigint REFERENCES jobs (id) ON DELETE SET NULL,
    team_id bigint REFERENCES teams (id) ON DELETE SET NULL,
    resource_id bigint,
    correlation_id uuid,
    dead_lettered_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX jobs_idempotency_key_key ON jobs (idempotency_key) WHERE idempotency_key IS NOT NULL;
-- Mutual exclusion per resource/server without a separate lock table (ERD §12).
CREATE UNIQUE INDEX jobs_lock_key_active_key ON jobs (lock_key) WHERE status IN ('leased', 'running');
-- Dequeue: the partial index only holds eligible jobs, sorted in
-- consumption order for FOR UPDATE SKIP LOCKED (ERD §12).
CREATE INDEX jobs_dequeue_idx ON jobs (queue, priority DESC, run_at, id) WHERE status = 'queued';
-- Lease reaper: crash recovery (INV-013).
CREATE INDEX jobs_lease_expiry_idx ON jobs (lease_expires_at) WHERE status IN ('leased', 'running');
CREATE INDEX jobs_team_id_id_idx ON jobs (team_id, id DESC);

-- +goose Down
DROP TABLE jobs;
DROP TYPE job_status;
