-- ADR-042: applications gain the preview access wall — `none` (default,
-- unchanged), `basic_auth` (shared credentials) or `sso` (AkerDock session,
-- team membership). The enum `preview_protection` is reused as-is: the
-- semantics are identical and a second enum would only duplicate it.
-- `access_basic_auth_enc` holds the generated "user:password" under envelope
-- encryption (revealed to the team, bcrypt-hashed for the proxy).
--
-- The preview access-token table becomes the shared one: `preview_id` turns
-- nullable and an `application_id` column joins it, exactly one of the two
-- being set. Expand-only.

-- +goose Up
ALTER TABLE applications ADD COLUMN access_protection preview_protection NOT NULL DEFAULT 'none';
ALTER TABLE applications ADD COLUMN access_basic_auth_enc bytea;

ALTER TABLE preview_access_tokens ALTER COLUMN preview_id DROP NOT NULL;
ALTER TABLE preview_access_tokens ADD COLUMN application_id bigint REFERENCES applications (id) ON DELETE CASCADE;
ALTER TABLE preview_access_tokens ADD CONSTRAINT preview_access_tokens_one_target
    CHECK ((preview_id IS NULL) <> (application_id IS NULL));
CREATE INDEX preview_access_tokens_application_idx ON preview_access_tokens (application_id);

-- +goose Down
DROP INDEX preview_access_tokens_application_idx;
ALTER TABLE preview_access_tokens DROP CONSTRAINT preview_access_tokens_one_target;
ALTER TABLE preview_access_tokens DROP COLUMN application_id;
ALTER TABLE applications DROP COLUMN access_basic_auth_enc;
ALTER TABLE applications DROP COLUMN access_protection;
