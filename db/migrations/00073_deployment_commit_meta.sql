-- Métadonnées du commit déployé (source git) : auteur et sujet du dernier
-- commit, lus sur le build server après checkout. `commit_message` était déjà
-- exposé au contrat mais jamais stocké ; `commit_author` répond au besoin de
-- savoir « qui a poussé » le déploiement en cours (push/webhook, où
-- `triggered_by` est nul). Colonnes nullables, expand-only.

-- +goose Up
ALTER TABLE deployments ADD COLUMN commit_author text;
ALTER TABLE deployments ADD COLUMN commit_message text;

-- +goose Down
ALTER TABLE deployments DROP COLUMN commit_author;
ALTER TABLE deployments DROP COLUMN commit_message;
