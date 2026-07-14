# ADR-007 — RBAC fin par projet et par environnement

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.7, §10.1, §15, §16.3, §23.1, §29.7

## Contexte

Un modèle de rôles grossier — owner d'instance, puis admin et member par team — donne à un simple member le pouvoir de toucher la production de tous les projets de sa team (§15). Or AkerDock pose la team comme frontière de sécurité (§23.1) et décrit des acteurs aux besoins distincts (développeur, opérateur/SRE, pipeline CI, intégration read-only — §16.3). Il faut décider du grain du contrôle d'accès.

## Décision

**RBAC fin retenu** : des rôles et permissions **par projet et par environnement**, au-delà de la parité admin/member. Un développeur peut par exemple être autorisé à déployer sur `staging` d'un projet sans pouvoir toucher `production`, ni les autres projets de la team.

Le détail (actions × types de ressources × rôles) est à spécifier dans la **matrice RBAC/permissions** (§29.7), qui est un artefact obligatoire avant implémentation complète. Les permissions API existantes (`read`, `read:sensitive`, `write`, `deploy`, `root` — §10.3, §24.1) restent le socle d'évaluation à l'action.

## Alternatives considérées

- **Parité stricte admin/member** : rejetée — reproduit une limitation connue et critiquée de la référence (§15) alors que le modèle team/projet/environnement de AkerDock permet mieux dès le départ.
- **ACL par ressource individuelle** : rejetée — granularité maximale mais complexité d'administration et d'audit disproportionnée ; le grain projet/environnement couvre les cas réels (protéger `production`).
- **Politiques externes (OPA & co)** : rejetées — dépendance et complexité d'exploitation injustifiées pour un self-hosted ; le modèle interne suffit.

## Conséquences

- **Positives** : moindre privilège réel (protéger la production des members), déblocage d'un point de friction connu de la référence, alignement avec les acteurs du §16.3 et l'audit (§23.4).
- **Négatives** : toute la couche d'autorisation doit évaluer projet et environnement en plus de la team — chaque endpoint et chaque relation indirecte doivent être couverts par la matrice de tests inter-team et inter-rôle (§23.5) ; l'UI doit refléter des droits partiels (actions grisées, listes filtrées).
- **Risques acceptés** : complexité de spécification reportée sur la matrice §29.7, qui devient bloquante avant l'implémentation ; risque de sur-granularité si l'on n'arbitre pas fermement le grain (projet/environnement, pas ressource).
