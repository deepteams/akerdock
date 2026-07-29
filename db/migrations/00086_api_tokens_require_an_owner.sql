-- +goose Up

-- A token is held by a PERSON. `api_tokens.created_by` is not bookkeeping: the
-- middleware intersects a token's permissions with its creator's on every
-- request (rbac-matrix §4.2), it is how deprovisioning revokes what someone
-- still holds, and it is how a CLI token is tied back to the human an access
-- grant was issued to (ADR-045 §5). A token with no creator escapes all three:
-- nothing narrows it when its holder is demoted, nothing revokes it when they
-- leave, and no grant is ever spendable through it — it simply refuses to open
-- a `sensitive` tunnel, with no way out but re-issuing it.
--
-- The column existed from the first migration but was written by nobody until
-- the creator was recorded at both mint sites, so every token predating that
-- carries NULL. They are retired here rather than left to fail in ways that
-- read as platform bugs. Revoked, not deleted: what existed stays legible in
-- the audit trail, which is the whole posture of the retention work.
--
-- Operational note: every API token minted before this migration stops
-- authenticating. `akerdock login` re-issues a CLI token; a CI token is
-- re-created from Team settings.
UPDATE api_tokens SET revoked_at = now(), updated_at = now()
WHERE created_by IS NULL AND revoked_at IS NULL;

-- And the invariant, so the situation cannot return: a LIVE token names its
-- owner. History keeps its NULLs — a revoked row is a record, not a credential.
--
-- The users FK is ON DELETE SET NULL, and no code path deletes a user today
-- (deprovisioning removes the membership and revokes the tokens, which is the
-- model). Whoever adds one will hit this constraint rather than silently
-- orphan live credentials: revoke first, then delete.
ALTER TABLE api_tokens
    ADD CONSTRAINT api_tokens_live_have_an_owner
    CHECK (created_by IS NOT NULL OR revoked_at IS NOT NULL);

-- +goose Down
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_live_have_an_owner;
