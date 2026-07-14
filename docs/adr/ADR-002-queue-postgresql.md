# ADR-002 — Queue durable PostgreSQL, sans bus externe

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.2, §16.1(6), §18.1, §18.2, §18.3, §21.3

## Contexte

L'objectif produit est un control plane léger à opérer : un binaire Go unique + PostgreSQL, sans Redis ni runtime applicatif (§16.1(6)). La solution courante — un Redis à côté de la base pour les queues et le cache — ajoute un second système d'état à installer, sauvegarder et superviser. Un bus séparé (Redis/NATS) améliorerait le débit brut, mais ajouterait un composant à installer, sauvegarder, superviser et mettre à jour pour chaque installation self-hosted. Il faut choisir le support de la queue de jobs durable (déploiements, backups, validations serveur, etc.).

## Décision

La queue durable est implémentée en **PostgreSQL**. PostgreSQL est la source de vérité unique : configuration, états, historique, audits, **leases et outbox** (§18.1). Les jobs suivent la machine à états générique du §21.3 (lease avec expiration et heartbeat, retry, dead-letter), et le pattern **transactional outbox** publie les événements après commit (§18.2).

L'**interface queue reste abstraite dans le code**, mais **aucun bus externe (Redis/NATS) n'est planifié** : une seule implémentation est livrée et maintenue.

## Alternatives considérées

- **Redis (parité avec la référence)** : rejeté — composant supplémentaire à opérer et à sauvegarder, contraire à l'engagement d'empreinte « binaire Go + PostgreSQL uniquement ».
- **NATS/JetStream** : rejeté — meilleur débit et sémantique de streaming, mais complexité d'exploitation injustifiée pour les volumes cibles (§22.2), qui restent atteignables avec PostgreSQL.
- **Queue en mémoire du processus Go** : rejetée — violerait INV-013 (un job accepté doit survivre au redémarrage du processus) et interdirait le multi-instance (§18.2).

## Conséquences

- **Positives** : un seul système d'état à installer, sauvegarder et restaurer ; transactions ACID entre mutation métier, enfilage du job et outbox (pas de fenêtre d'incohérence) ; self-hosting simplifié conformément à l'engagement produit.
- **Négatives** : débit inférieur à un bus dédié ; les requêtes de queue/leases (SELECT … FOR UPDATE SKIP LOCKED et similaires) deviennent des chemins critiques à écrire et indexer soigneusement (d'où pgx + sqlc, cf. ADR-025) ; la charge de la queue et celle du métier se partagent la même base.
- **Risques acceptés** : si les objectifs de capacité (§22.2 : 1 000 livraisons webhook/minute en burst, 50 builds simultanés) devenaient insuffisants, la migration vers un bus externe serait un chantier notable — atténué par l'interface queue abstraite, mais aucun travail spéculatif n'est engagé.
