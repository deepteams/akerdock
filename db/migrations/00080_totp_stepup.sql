-- A TOTP step-up marker, distinct from the passkey one (ADR-045 §5).
--
-- `sessions.mfa_verified_at` means "recent PASSKEY re-authentication" and
-- nothing else: rbac-matrix §5 requires that exact ritual for the root
-- terminal, and the TOTP typed at login deliberately does not set it. Reusing
-- that column for a TOTP step-up would silently hand every TOTP-only user a
-- root shell — a security regression disguised as a refactor.
--
-- So the TOTP ceremony gets its own column. The root terminal keeps reading
-- `mfa_verified_at` and is untouched by construction; the access-grant flow
-- reads whichever column matches the factor the server picked for that user
-- (the passkey when one is enrolled, never a choice offered to the client).

-- +goose Up
ALTER TABLE sessions ADD COLUMN totp_verified_at timestamptz;

-- +goose Down
ALTER TABLE sessions DROP COLUMN totp_verified_at;
