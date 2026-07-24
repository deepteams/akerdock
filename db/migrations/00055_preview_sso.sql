-- +goose Up
-- Preview protection by AkerDock authentication (ADR-030).
ALTER TYPE preview_protection ADD VALUE 'sso';

-- Access tokens of the sso mode: the browser holds the opaque value in a
-- preview-domain cookie, the base only ever stores its hash. Scoped to ONE
-- preview and ONE user, expiring, dying with the preview (cascade).
CREATE TABLE preview_access_tokens (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    token_hash text NOT NULL UNIQUE,
    preview_id bigint NOT NULL REFERENCES previews (id) ON DELETE CASCADE,
    user_id bigint REFERENCES users (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX preview_access_tokens_preview_idx ON preview_access_tokens (preview_id);

-- +goose Down
DROP TABLE preview_access_tokens;
-- The enum value stays: PostgreSQL cannot drop enum values, and an unused
-- value is harmless.
