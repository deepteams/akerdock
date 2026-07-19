-- Instance settings mutations (§14.2).

-- name: SetApiEnabled :one
UPDATE instance_settings
SET api_enabled = $1, updated_at = now(), version = version + 1
WHERE id = 1
RETURNING *;

-- name: SetInstanceIdentity :one
-- FQDN + contact ACME (§14.2) : la base fait foi après le premier démarrage,
-- c'est donc ici — et nulle part ailleurs — qu'ils se modifient.
UPDATE instance_settings
SET fqdn = $1, acme_email = $2, updated_at = now(), version = version + 1
WHERE id = 1
RETURNING *;
