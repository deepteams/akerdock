-- An attached port-forward is activity (ADR-032 meeting ADR-036/ADR-037).
-- Scale-to-zero has exactly one activity source, the waker's per-resource file,
-- and the waker only writes it when it serves a PROXIED HTTP request. A tunnel
-- goes control plane → SSH → container IP and never touches the proxy, so a
-- developer connected to a container for an hour looked perfectly idle and the
-- scheduler stopped the very container they were working in.
--
-- Previews already carry last_activity_at; applications had nothing but
-- resources.updated_at — a column that only moves on a deploy or a config
-- change — so they get their own, read exactly like the preview one.
--
-- target_stopped is the other half. When the target IS gone (redeploy, manual
-- stop, crash), the forwarded TCP connection black-holes: the container's netns
-- is destroyed, so no RST and no FIN ever come back and the client retries
-- keepalives until the tunnel's own idle timeout. Ending the session with a
-- reason the CLI can print turns that silence into an error within one beat.

-- +goose Up
ALTER TABLE applications
    ADD COLUMN last_activity_at timestamptz;

-- Added last: PostgreSQL forbids USING a new enum value in the transaction that
-- adds it, and nothing above uses it (same precedent as 00079's grant_expired).
ALTER TYPE terminal_end_reason ADD VALUE IF NOT EXISTS 'target_stopped';

-- +goose Down
ALTER TABLE applications DROP COLUMN last_activity_at;
-- The enum value stays: PostgreSQL cannot drop one, and an unused value is
-- harmless (same stance as 00062).
