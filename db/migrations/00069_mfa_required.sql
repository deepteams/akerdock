-- MFA imposable (ISO A.8.5 / SOC2 CC6.1) : quand `mfa_required` est activé pour
-- l'instance, un utilisateur sans facteur MFA confirmé qui se connecte ouvre une
-- session marquée `mfa_pending` — bloquée sur toute l'API tant qu'il n'a pas
-- enrôlé un facteur (enrôlement forcé). La confirmation TOTP lève le flag.
-- Expand-only (colonnes NOT NULL avec défaut).

-- +goose Up
ALTER TABLE instance_settings ADD COLUMN mfa_required boolean NOT NULL DEFAULT false;
ALTER TABLE sessions ADD COLUMN mfa_pending boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE sessions DROP COLUMN mfa_pending;
ALTER TABLE instance_settings DROP COLUMN mfa_required;
