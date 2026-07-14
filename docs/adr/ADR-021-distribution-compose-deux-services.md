# ADR-021 — Distribution de l'instance : docker-compose minimal à 2 services

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.21, §16.1(6), §18.2, §14.1, §27.1

## Contexte

AkerDock s'engage sur une empreinte légère : un binaire Go unique + PostgreSQL, sans Redis ni runtime applicatif, un seul port exposé, sur un gabarit de 2 vCPU / 2 GB (§14.1, §16.1(6)). Reste à traduire cet engagement en **format de distribution** — c'est lui que l'opérateur installe, met à jour et sauvegarde.

## Décision

**docker-compose minimal à 2 services** :

1. **l'image AkerDock** : binaire Go statique dans une image **distroless**, avec les modes `all-in-one`/`api`/`worker` du monolithe modulaire (§18.2) ;
2. **PostgreSQL**.

Propriétés garanties : **un seul `docker compose up`** pour installer ; **upgrade par changement de tag** ; **backups PostgreSQL standards** (aucun autre état à sauvegarder que la base et la configuration) ; **un seul port exposé** pour le control plane (§27.1).

## Alternatives considérées

- **Stack multi-containers (app + cache + service temps réel + helpers)** : rejetée — contredit l'engagement d'empreinte ; chaque container supplémentaire est un composant à installer, superviser, sauvegarder et mettre à jour.
- **Binaire nu (systemd) sans Docker** : rejeté comme mode principal — reporte l'installation de PostgreSQL et la gestion des upgrades sur l'utilisateur ; Docker Compose est déjà un prérequis conceptuel du produit.
- **PostgreSQL embarqué dans l'image AkerDock (ou SQLite)** : rejeté — coupler base et application casse les upgrades indépendants et les backups standards ; SQLite est incompatible avec le multi-instance (§18.2) et le rôle central de PostgreSQL (queue, leases, outbox — ADR-002).

## Conséquences

- **Positives** : installation et modèle mental minimaux (2 services) ; upgrade/downgrade triviaux par tag ; surface d'attaque réduite (distroless : pas de shell ni de paquets) ; backup de l'instance = backup PostgreSQL standard ; cohérence directe avec ADR-002 (pas de Redis) et ADR-001/024 (port unique).
- **Négatives** : distroless complique le debug in situ (pas de shell dans le container) — les diagnostics passent par les logs, l'API et les outils externes ; l'utilisateur gère lui-même les upgrades majeurs de PostgreSQL (procédure à documenter, §29.10).
- **Risques acceptés** : le mode `all-in-one` fait cohabiter API et workers dans un même processus — les limites de cette cohabitation (isolation des pannes, dimensionnement) sont assumées pour les petites installations, le passage aux modes séparés `api`/`worker` restant le chemin de croissance.
