-- Queries used by the startup sequence (instance-config §6).

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: CreateUser :one
INSERT INTO users (email, name, password_hash, is_root)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetInstanceSettings :one
SELECT * FROM instance_settings WHERE id = 1;

-- name: InsertInstanceSettingsIfAbsent :execrows
INSERT INTO instance_settings (id, fqdn, timezone, acme_email)
VALUES (1, $1, $2, sqlc.narg(acme_email))
ON CONFLICT (id) DO NOTHING;

-- name: SetAcmeEmailIfAbsent :execrows
-- Seeds the ACME contact on an instance that predates the setting. The database
-- stays authoritative: an existing value is never overwritten by the variable.
UPDATE instance_settings SET acme_email = $1, updated_at = now()
WHERE id = 1 AND acme_email IS NULL;

-- name: GetOldestTeamID :one
-- The localhost server lands in the first team of the instance — the root
-- user's, by construction (§6.2).
SELECT id FROM teams ORDER BY id LIMIT 1;

-- name: CreateLocalhostServerIfAbsent :execrows
-- ON CONFLICT: the operator may already have a server named "localhost" in
-- that team — theirs wins, the seed backs off silently.
INSERT INTO servers (team_id, name, description, host, port, ssh_user, private_key_id, is_localhost)
VALUES ($1, 'localhost', 'The machine hosting this AkerDock instance, seeded at first start (instance-config §6.2). Validated automatically once the instance public key is authorized on this host.', $2, 22, $3, $4, true)
ON CONFLICT DO NOTHING;

-- name: SetLocalhostSeeded :execrows
UPDATE instance_settings SET localhost_seeded = true, updated_at = now()
WHERE id = 1 AND NOT localhost_seeded;
