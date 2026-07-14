# ADR-015 — Fiabilité compose « by design » : zero-downtime et resource limits réellement appliqués

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.15, §15, §5.3, §5.5, §26.2, INV-005

## Contexte

Deux pièges structurels guettent les stacks Docker Compose (§15) : le zero-downtime y est souvent abandonné — chaque redéploiement coupe le service — et les resource limits déclarées dans le fichier ne sont pas réellement appliquées. Ces défauts touchent précisément le build pack le plus utilisé pour les services réels. Il faut décider s'ils sont acceptés ou traités dès la conception.

## Décision

Les deux limitations sont traitées **dès la conception** :

1. AkerDock **DOIT fournir le zero-downtime pour les services web des stacks compose** : bascule **par service** derrière le proxy (nouveau container du service démarré, health check satisfait, bascule du trafic, arrêt de l'ancien), au lieu d'un `down`/`up` global du stack. Cohérent avec INV-005 : une application saine reste routée tant que sa remplaçante n'a pas satisfait les conditions de bascule.
2. AkerDock **DOIT appliquer réellement les resource limits déclarées** aux ressources compose (memory/CPU, §5.3), vérifiables au niveau cgroups (preuve exigée dans la matrice §26.2 : « E2E rolling update stack compose + vérif cgroups »).

## Alternatives considérées

- **Parité stricte (reproduire les limitations)** : rejetée — ce sont des défauts reconnus de la référence, pas des comportements à préserver ; la parité visée porte sur les capacités, pas sur les bugs.
- **Zero-downtime compose via blue/green du stack entier** : rejeté — doubler tout le stack (bases comprises) est coûteux et dangereux pour les données ; la bascule par service derrière le proxy limite le doublement aux services web.
- **Corriger plus tard, après la parité** : rejeté — le PRD acte un traitement « by design » : rétrofiter le zero-downtime dans un moteur compose déjà écrit coûterait plus cher que de le concevoir d'emblée.

## Conséquences

- **Positives** : les stacks compose deviennent des citoyens de première classe — déploiements sans coupure, limites réellement opposables — au même niveau qu'une application à container unique.
- **Négatives** : le moteur de déploiement compose est nettement plus complexe qu'un `docker compose up` : orchestration par service, coexistence temporaire de deux versions d'un service sur le même réseau, génération de configuration proxy par service ; l'application des limits exige une transformation systématique du compose utilisateur.
- **Risques acceptés** : le zero-downtime par service ne s'applique qu'aux services web derrière le proxy — les services non exposés (workers, bases) suivent un remplacement classique ; certains stacks (état partagé, verrous applicatifs) ne tolèrent pas deux instances simultanées d'un même service, et ce cas devra rester désactivable par service.
