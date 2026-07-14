-- Expiry alerting (§4.3, ADR-019). A certificate must be announced once per
-- threshold (J-30, then J-7), not on every scheduler pass: without a marker,
-- a 30-day window would produce one alert every 30 seconds for a month.

-- +goose Up
ALTER TABLE certificates
    -- The smallest threshold already announced, in days. NULL = nothing sent.
    ADD COLUMN expiry_alerted_threshold integer,
    ADD COLUMN expiry_alerted_at timestamptz;

-- +goose Down
ALTER TABLE certificates
    DROP COLUMN expiry_alerted_threshold,
    DROP COLUMN expiry_alerted_at;
