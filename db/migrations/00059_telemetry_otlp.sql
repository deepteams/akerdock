-- Remote telemetry export (§14.2, ADR-008/§27.8).
--
-- otlp_config_enc: the OTLP exporter configuration (endpoint, protocol, auth
-- headers, which signals), envelope-encrypted like the transactional email
-- config — the headers can carry a bearer token, so the whole blob is a secret
-- (INV-003). Read once at boot; a change takes effect at the next restart.

-- +goose Up
ALTER TABLE instance_settings
    ADD COLUMN otlp_config_enc bytea;

-- +goose Down
ALTER TABLE instance_settings DROP COLUMN otlp_config_enc;
