# ADR-003 — Secrets : chiffrement enveloppe AEAD en base, interface SecretStore interne

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.3, §19.2, §23.2, INV-003

## Contexte

La plateforme stocke de nombreux secrets : clés SSH privées, variables d'environnement, webhook secrets, credentials registry/S3/cloud, OAuth client secrets, CA privées. Les chiffrer avec une clé applicative unique et globale est simple, mais ne permet ni rotation ni compartimentage. Un secret store externe (Vault, SOPS, KMS) offrirait une meilleure séparation, mais imposerait un composant supplémentaire à chaque installation self-hosted. Il faut définir le niveau de protection au repos et le point d'extension futur.

## Décision

**Chiffrement enveloppe AEAD (AES-256-GCM) dans PostgreSQL** :

- La clé maître réside dans un **fichier root-only** ou une **variable d'environnement**, externe à la base (§23.2).
- **Versionnement de clé et rotation** pris en charge dès le début : chaque secret porte la version de la clé qui l'a chiffré, et la rotation s'effectue sans réécriture bloquante de toute la base (§19.2).
- Une interface **`SecretStore` interne existe dès le début**, mais **une seule implémentation est livrée** (chiffrement enveloppe en base). Vault/KMS ne seront envisagés que **sur demande utilisateur validée**.

Les règles d'usage du §23.2 s'appliquent : secrets masqués dans UI/API/logs/audit, révélation uniquement avec la permission `read:sensitive` (INV-003), mots de passe hashés en Argon2id, tokens API hashés irréversiblement.

## Alternatives considérées

- **Vault/KMS dès le départ** : rejeté — composant lourd à opérer pour l'utilisateur cible (VPS modeste), contraire à l'objectif de simplicité d'exploitation ; reste possible plus tard via l'interface `SecretStore`.
- **SOPS/fichiers chiffrés hors base** : rejeté — sépare les secrets de leur cycle de vie transactionnel (versions, audit, suppression) et complique backup/restore du control plane.
- **Chiffrement au niveau disque uniquement (LUKS/at-rest DB)** : rejeté — ne protège ni contre un dump SQL exfiltré ni contre un accès applicatif trop large, et n'offre ni versionnement ni rotation par secret.

## Conséquences

- **Positives** : aucune dépendance externe ; backup/restore de la base emporte les secrets (chiffrés) ; rotation de clé possible sans indisponibilité ; point d'extension propre si un jour Vault/KMS est demandé.
- **Négatives** : la clé maître devient un point critique — sa perte rend tous les secrets irrécupérables ; sa gestion (fichier root-only, permissions, sauvegarde séparée de la base) doit être documentée dans les runbooks (§29.10).
- **Risques acceptés** : un attaquant qui obtient à la fois un dump de la base et la clé maître (compromission du control plane) lit tous les secrets — c'est cohérent avec le modèle de menace §23.1, où le control plane est hautement privilégié ; pas d'intégration HSM/KMS tant qu'aucune demande validée n'existe.
