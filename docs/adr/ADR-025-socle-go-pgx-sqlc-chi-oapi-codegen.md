# ADR-025 — Socle technique Go/API : pgx + sqlc, migrations SQL versionnées, chi + oapi-codegen spec-first

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.25, §24.1, §25.2, §18.2, ADR-002

## Contexte

Le control plane Go a besoin d'un socle d'accès aux données et d'une chaîne API. Les chemins critiques — queue, leases, outbox en PostgreSQL (ADR-002) — reposent sur du SQL précis (verrous, `SKIP LOCKED`, transactions) qu'un ORM masque ou dégrade. Côté API, le PRD exige qu'OpenAPI soit un artefact versionné et testé en CI (§24.1) et que le client TypeScript de l'UI soit généré depuis le même artefact (§25.2) pour empêcher toute dérive UI/API. Il faut fixer ces choix structurants avant la première ligne de code.

## Décision

- **Accès PostgreSQL via pgx + sqlc** : SQL explicite, types vérifiés à la compilation — indispensable pour les requêtes critiques de **queue/leases/outbox** ; **migrations SQL versionnées**.
- **API spec-first** avec le router **chi** et **oapi-codegen** : les **handlers Go** et le **client TypeScript de l'UI** (§25.2) sont générés depuis le **même artefact OpenAPI** (§24.1).

## Alternatives considérées

- **ORM (GORM/ent)** : rejeté — abstraction inadaptée aux requêtes critiques de queue/leases/outbox (verrous, `FOR UPDATE SKIP LOCKED`), coût caché en performance et en contrôle ; sqlc donne la sûreté de types sans masquer le SQL.
- **Code-first (OpenAPI généré depuis le code Go)** : rejeté — la spec devient un sous-produit au lieu d'un contrat ; le spec-first garantit que handlers Go et client TypeScript dérivent du même artefact, sans dérive possible.
- **Frameworks web lourds (gin, echo) ou gRPC-gateway** : rejetés — chi est un router minimal compatible `net/http` standard, suffisant et sans lock-in ; gRPC ajouterait une couche de traduction pour une API REST publique contractuelle.

## Conséquences

- **Positives** : requêtes critiques auditables et optimisables en SQL natif, vérifiées à la compilation ; contrat API unique dont dérivent serveur et client (aucune dérive UI/API possible) ; dépendances minimales et standards de l'écosystème Go ; migrations SQL explicites compatibles rolling upgrade (§18.2).
- **Négatives** : sqlc impose d'écrire tout le SQL à la main (plus verbeux qu'un ORM pour le CRUD simple) et une étape de génération de code dans le build ; le spec-first exige de maintenir l'artefact OpenAPI en amont de chaque évolution d'endpoint, avec la discipline CI correspondante (§24.1).
- **Risques acceptés** : couplage assumé à PostgreSQL (aucune portabilité multi-SGBD — cohérent avec ADR-002 et ADR-021) ; dépendance à des générateurs tiers (sqlc, oapi-codegen) dont les évolutions doivent être suivies ; les erreurs de conception de la spec OpenAPI se propagent mécaniquement au serveur et au client.
