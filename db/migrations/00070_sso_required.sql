-- SSO obligatoire (ISO A.5.16 / SOC2 CC6.1) : une instance peut désactiver le
-- login par mot de passe, ne laissant que le SSO (OIDC). Pendant de mfa_required.
-- Garde-fous (côté code) : n'activable que si ≥1 provider OIDC est activé, et
-- l'utilisateur `is_root` reste exempté (porte de secours anti-lockout).
-- Expand-only (colonne NOT NULL avec défaut).

-- +goose Up
ALTER TABLE instance_settings ADD COLUMN password_login_disabled boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE instance_settings DROP COLUMN password_login_disabled;
