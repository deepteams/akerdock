-- The team a user acts in, remembered ACROSS sessions (PRD §37 — multi-team).
--
-- `sessions.current_team_id` already carries the acting team, but it dies with
-- the session: a member of two teams who switches, signs out and signs back in
-- lands on their oldest team again — the exact symptom that makes a team
-- switcher feel broken. So the choice is persisted on the user, and a new
-- session opens on it.
--
-- ON DELETE SET NULL, not CASCADE: losing the remembered team must never delete
-- the user. A NULL simply means "no preference yet" and the login falls back to
-- the oldest membership.

-- +goose Up
ALTER TABLE users ADD COLUMN last_team_id bigint REFERENCES teams (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE users DROP COLUMN last_team_id;
