-- ADR-049: narrowly scoped unauthenticated routes through an otherwise
-- protected resource. Applications carry routes directly; Compose services
-- carry them on their mirrored component because domains are per component.
--
-- Inline Compose stacks gain the same wall as applications. Access tokens gain
-- a resource_id while application_id stays during the rolling-upgrade window:
-- an old replica can still validate grants minted for applications.

-- +goose Up
ALTER TABLE applications
    ADD COLUMN access_public_routes jsonb NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE services
    ADD COLUMN access_protection preview_protection NOT NULL DEFAULT 'none',
    ADD COLUMN access_basic_auth_enc bytea;

ALTER TABLE service_components
    ADD COLUMN access_public_routes jsonb NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE preview_access_tokens
    DROP CONSTRAINT preview_access_tokens_one_target;
ALTER TABLE preview_access_tokens
    ADD COLUMN resource_id bigint REFERENCES resources (id) ON DELETE CASCADE;
UPDATE preview_access_tokens
SET resource_id = application_id
WHERE application_id IS NOT NULL;
ALTER TABLE preview_access_tokens
    ADD CONSTRAINT preview_access_tokens_one_target CHECK (
        (preview_id IS NOT NULL AND application_id IS NULL AND resource_id IS NULL)
        OR
        (
            preview_id IS NULL
            AND (application_id IS NOT NULL OR resource_id IS NOT NULL)
            AND (
                application_id IS NULL
                OR resource_id IS NULL
                OR application_id = resource_id
            )
        )
    );
CREATE INDEX preview_access_tokens_resource_idx
    ON preview_access_tokens (resource_id);

-- +goose Down
DROP INDEX preview_access_tokens_resource_idx;
ALTER TABLE preview_access_tokens
    DROP CONSTRAINT preview_access_tokens_one_target;
DELETE FROM preview_access_tokens
WHERE preview_id IS NULL AND application_id IS NULL;
ALTER TABLE preview_access_tokens
    DROP COLUMN resource_id;
ALTER TABLE preview_access_tokens
    ADD CONSTRAINT preview_access_tokens_one_target
        CHECK ((preview_id IS NULL) <> (application_id IS NULL));

ALTER TABLE service_components DROP COLUMN access_public_routes;
ALTER TABLE services
    DROP COLUMN access_basic_auth_enc,
    DROP COLUMN access_protection;
ALTER TABLE applications DROP COLUMN access_public_routes;
