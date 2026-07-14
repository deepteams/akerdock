-- Browser sessions (PRD §698: CSRF, Secure/HttpOnly/SameSite cookies, session
-- rotation after login, invalidation on logout).
--
-- The `sessions` table already exists (00001). What is missing is what makes a
-- login endpoint safe to expose:
--
--   * a CSRF secret bound to the session. A session cookie is sent by the
--     browser on every request, including one triggered by another site — so a
--     cookie alone authenticates the BROWSER, not the intent. SameSite=Lax
--     blocks the obvious cross-site POST, but it is a defence the browser
--     grants us, not one we enforce: the CSRF token is ours.
--
--   * brute-force resistance on the password. Without it, a login endpoint is a
--     password oracle rated at whatever the rate limiter allows.

-- +goose Up
ALTER TABLE sessions
    -- Random per session, mirrored into a readable cookie; the client echoes it
    -- in a header on every mutation. An attacker on another origin can make the
    -- browser send the session cookie, but cannot READ this value to echo it.
    ADD COLUMN csrf_token text;

-- users.failed_login_count and users.locked_until already exist (00001): the
-- schema anticipated the lockout, nothing consumed it. The login endpoint below
-- finally does.

CREATE INDEX sessions_user_active_idx ON sessions (user_id)
    WHERE revoked_at IS NULL;

-- +goose Down
DROP INDEX sessions_user_active_idx;
ALTER TABLE sessions DROP COLUMN csrf_token;
