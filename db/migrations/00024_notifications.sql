-- Notifications (data-dictionary §11.2/§11.3, ADR-019). Channels carry their
-- credentials encrypted in one blob; rules route an event type to a channel,
-- scoped by project/environment and filtered by severity.
--
-- notification_deliveries is not in the dictionary: ADR-019 requires
-- debouncing and a delivery history, and neither is derivable from the two
-- tables above. It is what makes the dispatcher idempotent (one delivery per
-- rule × event, enforced by a UNIQUE) and what a debounce window is measured
-- against.

-- +goose Up
CREATE TYPE notification_channel_kind AS ENUM ('smtp', 'resend', 'discord', 'telegram', 'slack', 'pushover', 'webhook');
CREATE TYPE notification_severity AS ENUM ('info', 'warning', 'critical');
CREATE TYPE notification_delivery_status AS ENUM ('pending', 'sent', 'failed', 'suppressed');

CREATE TABLE notification_channels (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    kind notification_channel_kind NOT NULL,
    name text NOT NULL,
    -- JSON configuration (webhook URL, bot token, SMTP credentials…),
    -- envelope-encrypted as one blob (§23.2).
    config_enc bytea NOT NULL,
    use_instance_email boolean NOT NULL DEFAULT false,
    enabled boolean NOT NULL DEFAULT true,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    version integer NOT NULL DEFAULT 1,
    UNIQUE (team_id, name)
);

CREATE INDEX notification_channels_team_id_idx ON notification_channels (team_id);

CREATE TABLE notification_rules (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    channel_id bigint NOT NULL REFERENCES notification_channels (id) ON DELETE CASCADE,
    event_type text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    project_id bigint REFERENCES projects (id) ON DELETE CASCADE,
    environment_id bigint REFERENCES environments (id) ON DELETE CASCADE,
    min_severity notification_severity NOT NULL DEFAULT 'info',
    debounce_seconds integer NOT NULL DEFAULT 0 CHECK (debounce_seconds >= 0),
    quiet_hours_start time,
    quiet_hours_end time,
    digest_enabled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- NULLS NOT DISTINCT: a team-wide rule (project_id NULL) must collide with
    -- another team-wide rule for the same event, not be silently duplicated.
    UNIQUE NULLS NOT DISTINCT (channel_id, event_type, project_id, environment_id)
);

CREATE INDEX notification_rules_event_type_idx ON notification_rules (event_type) WHERE enabled;

CREATE TABLE notification_deliveries (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    rule_id bigint NOT NULL REFERENCES notification_rules (id) ON DELETE CASCADE,
    channel_id bigint NOT NULL REFERENCES notification_channels (id) ON DELETE CASCADE,
    outbox_event_id bigint NOT NULL REFERENCES outbox_events (id) ON DELETE CASCADE,
    status notification_delivery_status NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    last_error text,
    -- The reason a delivery was suppressed (debounce, quiet hours): the
    -- operator must be able to see that an event was matched and deliberately
    -- not sent, rather than silently lost.
    suppressed_reason text,
    sent_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- One delivery per rule and event: the dispatcher can re-read the same
    -- outbox event without ever notifying twice.
    UNIQUE (rule_id, outbox_event_id)
);

CREATE INDEX notification_deliveries_pending_idx ON notification_deliveries (id) WHERE status = 'pending';
CREATE INDEX notification_deliveries_rule_sent_idx ON notification_deliveries (rule_id, sent_at DESC);

-- The dispatcher's cursor over the outbox. A single row: notifications are
-- one consumer among others (SSE reads the same table independently), so the
-- outbox itself carries no per-consumer state.
CREATE TABLE notification_cursor (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    last_outbox_event_id bigint NOT NULL DEFAULT 0
);
INSERT INTO notification_cursor (id, last_outbox_event_id) VALUES (true, 0);

-- +goose Down
DROP TABLE notification_cursor;
DROP TABLE notification_deliveries;
DROP TABLE notification_rules;
DROP TABLE notification_channels;
DROP TYPE notification_delivery_status;
DROP TYPE notification_severity;
DROP TYPE notification_channel_kind;
