-- Preview route templates (§20.4, ADR-035): the single preview_url_template
-- becomes a table of {host, port} rows. Stored as JSONB on the application;
-- empty/NULL keeps the historical single-template behaviour (no backfill).
--
-- random_slug is the stable per-preview value behind the {{random}} placeholder
-- — generated once so every route and redeploy resolves to the same hostname
-- (no certificate churn).

-- +goose Up
ALTER TABLE applications
    ADD COLUMN preview_url_templates jsonb;

ALTER TABLE previews
    ADD COLUMN random_slug text;

-- +goose Down
ALTER TABLE previews DROP COLUMN random_slug;
ALTER TABLE applications DROP COLUMN preview_url_templates;
