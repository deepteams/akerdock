-- Platform aggregate: append-only audit log (§23.4) and transactional
-- outbox (§18.2, §24.2). Both are deliberately FK-free: they snapshot
-- their subjects by UUID and survive any deletion (§19.2).

-- +goose Up
CREATE TYPE actor_kind AS ENUM ('user', 'token', 'system');
CREATE TYPE audit_result AS ENUM ('success', 'failure', 'denied');

CREATE TABLE audit_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    team_id bigint,
    actor_kind actor_kind NOT NULL,
    actor_uuid uuid,
    actor_display text,
    action text NOT NULL,
    target_kind text,
    target_uuid uuid,
    result audit_result NOT NULL,
    ip inet,
    user_agent text,
    request_id uuid,
    correlation_id uuid,
    diff_redacted jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_team_time_idx ON audit_events (team_id, occurred_at DESC);
CREATE INDEX audit_events_occurred_brin ON audit_events USING brin (occurred_at);
CREATE INDEX audit_events_action_idx ON audit_events (action);
CREATE INDEX audit_events_target_idx ON audit_events (target_uuid);

CREATE TABLE outbox_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    event_type text NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    team_uuid uuid,
    resource_uuid uuid,
    actor jsonb,
    correlation_id uuid,
    aggregate_key text,
    payload jsonb NOT NULL DEFAULT '{}',
    published_at timestamptz,
    publish_attempts integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX outbox_events_unpublished_idx ON outbox_events (id) WHERE published_at IS NULL;
CREATE INDEX outbox_events_aggregate_idx ON outbox_events (aggregate_key);

-- +goose Down
DROP TABLE outbox_events;
DROP TABLE audit_events;
DROP TYPE audit_result;
DROP TYPE actor_kind;
