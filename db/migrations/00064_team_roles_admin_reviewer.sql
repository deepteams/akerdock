-- ADR-038 (modèle de rôles) : rôles de team = admin / member / reviewer (+ custom
-- plus tard). Deux changements ici :
--
--   1. `owner` est FUSIONNÉ dans `admin` : un seul rôle haut de team. La valeur
--      d'enum `owner` ne peut pas être retirée (PostgreSQL ne sait pas supprimer
--      une valeur d'enum) et reste donc dans le type, mais plus aucune ligne ne
--      la porte après le backfill. Le code mappe `owner` sur le set de `admin`
--      par sécurité si une ligne traînait.
--   2. `reviewer` est ajouté : il ne voit que les PR previews (previews:read),
--      rien d'autre.
--
-- On ajoute la valeur d'enum PUIS on backfille vers `admin` (valeur déjà
-- existante) — on n'UTILISE jamais `reviewer` dans cette transaction, ce qui
-- respecte la contrainte PostgreSQL sur ALTER TYPE ADD VALUE (même schéma que
-- 00062). Migration expand-only : aucun DROP/RENAME.

-- +goose Up
ALTER TYPE team_role ADD VALUE IF NOT EXISTS 'reviewer';

UPDATE team_memberships SET role = 'admin' WHERE role = 'owner';

-- +goose Down
-- Rétablit les anciens owners : dans le modèle pré-ADR-038, le créateur/haut rôle
-- de team était `owner`. On ne peut pas distinguer un ancien owner d'un admin
-- créé après coup, donc ce down est best-effort (il repromeut tous les admins en
-- owner). La valeur d'enum `reviewer` ne peut pas être retirée ; les lignes qui
-- l'utilisent devraient être réassignées à la main avant un rollback complet.
UPDATE team_memberships SET role = 'owner' WHERE role = 'admin';
