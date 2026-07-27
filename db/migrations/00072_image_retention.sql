-- Rétention locale des images de déploiement (ADR-006, §29.4) : sans registry,
-- les N dernières images sont conservées pour le rollback et protégées du
-- cleanup automatique (§3.7, INV-015) ; au-delà de N, elles sont récupérées
-- après un déploiement réussi. N est réglable ici (défaut 5, minimum 1 pour
-- toujours protéger l'image en service). Le compte s'applique par application
-- et, à l'identique, par preview (au merge/close de la PR, tout est supprimé).
-- Expand-only (colonne NOT NULL avec défaut).

-- +goose Up
ALTER TABLE instance_settings
  ADD COLUMN image_retention_count integer NOT NULL DEFAULT 5
    CONSTRAINT image_retention_count_min CHECK (image_retention_count >= 1);

-- +goose Down
ALTER TABLE instance_settings DROP COLUMN image_retention_count;
