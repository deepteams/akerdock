
-- name: SetTransactionalEmailConfig :exec
UPDATE instance_settings SET transactional_email_config_enc = $1, updated_at = now() WHERE id = 1;

-- name: SetOtlpConfig :exec
UPDATE instance_settings SET otlp_config_enc = $1, updated_at = now() WHERE id = 1;
