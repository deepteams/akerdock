-- GitLab/Gitea preview parity (§20.4.6-§20.4.7, protocols §4/§6, amendment 31).
--
-- api_token_enc: provider API token, envelope-encrypted, write-only through
-- the API (INV-003). It funds the degraded feedback path (commit statuses,
-- upserted comment) and the server-side rights check of comment commands for
-- sources that have no GitHub App.
--
-- repo_reference: where the preview phones home — GitLab project id or
-- Gitea/GitHub full name, captured from the authenticated MR/PR delivery at
-- upsert time. The GitHub App path keeps using the repositories cache and
-- leaves it NULL.

-- +goose Up
ALTER TABLE git_sources
    ADD COLUMN api_token_enc bytea;

ALTER TABLE previews
    ADD COLUMN repo_reference text;

-- +goose Down
ALTER TABLE previews DROP COLUMN repo_reference;
ALTER TABLE git_sources DROP COLUMN api_token_enc;
