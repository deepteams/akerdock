-- Deferred digest (ADR-019 §4): non-critical events are grouped and sent
-- later, instead of one message each. `digest_enabled` was stored but nothing
-- consumed it — the interval and the last flush are what make it actionable.

-- +goose Up
ALTER TABLE notification_rules
    ADD COLUMN digest_interval_minutes integer NOT NULL DEFAULT 60
        CHECK (digest_interval_minutes > 0),
    ADD COLUMN last_digest_at timestamptz;

-- The digest flush looks for deliveries left pending by a digest rule.
CREATE INDEX notification_deliveries_digest_idx
    ON notification_deliveries (rule_id, created_at)
    WHERE status = 'pending';

-- +goose Down
DROP INDEX notification_deliveries_digest_idx;
ALTER TABLE notification_rules
    DROP COLUMN digest_interval_minutes,
    DROP COLUMN last_digest_at;
