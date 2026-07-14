-- Instance settings mutations (§14.2).

-- name: SetApiEnabled :one
UPDATE instance_settings
SET api_enabled = $1, updated_at = now(), version = version + 1
WHERE id = 1
RETURNING *;
