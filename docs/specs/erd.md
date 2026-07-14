# ERD — AkerDock

> Artefact §29.2 du PRD (`docs/PRD.md`). Diagrammes entité-relation du modèle décrit dans `docs/specs/data-dictionary.md` (54 tables), suivis de l'ownership team, des contraintes d'unicité, des indexes clés et de la stratégie de migrations. Le diagramme complet étant illisible d'un bloc, il est découpé par agrégat (§19.1) avec une vue d'ensemble.

Notation : `||` = exactement 1, `|o` = 0 ou 1, `o{` = 0..N. Les entités marquées « (réf.) » appartiennent à un autre agrégat et sont répétées pour le contexte.

---

## 1. Vue d'ensemble des agrégats

```mermaid
flowchart LR
    ID["Identité<br/>users, teams, sessions,<br/>memberships, invitations,<br/>mfa_factors, identities, api_tokens"]
    ORG["Organisation<br/>projects, environments,<br/>resources, tags"]
    INFRA["Infrastructure<br/>servers, destinations,<br/>private_keys, cloud/registry<br/>credentials, s3_storages,<br/>certificates"]
    SRC["Source<br/>git_sources, github_apps,<br/>repositories, webhook_endpoints,<br/>webhook_deliveries"]
    APP["Application<br/>applications, build/runtime_configs,<br/>domains, variables, storages,<br/>health_checks, previews"]
    SVC["Service / DB<br/>services, service_components,<br/>databases, credentials,<br/>backup_plans, backup_executions"]
    EXEC["Exécution<br/>deployments, steps, artifacts,<br/>scheduled_tasks, task_executions,<br/>terminal_sessions"]
    PLAT["Plateforme<br/>proxy_config_revisions, notifications,<br/>audit_events, outbox_events,<br/>feature_flags, instance_settings, jobs"]

    ID -->|"team_id partout (INV-001)"| ORG
    ID --> INFRA
    ID --> SRC
    ORG -->|"resources = union app/db/service"| APP
    ORG --> SVC
    INFRA -->|"destination, serveur, clés, storages"| ORG
    SRC -->|"source Git, webhooks"| APP
    APP -->|"déploiements, previews"| EXEC
    SVC -->|"backups, tâches"| EXEC
    EXEC --> PLAT
    INFRA --> PLAT
```

---

## 2. Agrégat Identité

```mermaid
erDiagram
    users ||--o{ identities : "comptes federes"
    users ||--o{ mfa_factors : "2FA TOTP"
    users ||--o{ sessions : "sessions navigateur"
    users ||--o{ team_memberships : "appartient"
    teams ||--o{ team_memberships : "membres + role"
    teams ||--o{ invitations : "invite par email"
    teams ||--o{ api_tokens : "tokens scopes team"
    teams |o--o{ sessions : "team active"
    users |o--o{ api_tokens : "createur"

    users {
        bigint id PK
        uuid uuid UK
        citext email UK
        text password_hash "Argon2id"
        boolean is_root
        timestamptz deleted_at "tombstone"
    }
    teams {
        bigint id PK
        uuid uuid UK
        text name
        integer version "verrou optimiste"
    }
    api_tokens {
        uuid uuid UK
        text token_prefix
        text token_hash UK "SHA-256"
        text_array permissions
        cidr_array ip_allowlist
        timestamptz expires_at
    }
    team_memberships {
        team_role role "owner-admin-member"
    }
```

## 3. Agrégat Organisation

```mermaid
erDiagram
    teams ||--o{ projects : "contient"
    projects ||--o{ environments : "1-N"
    environments ||--o{ resources : "1-N"
    teams ||--o{ resources : "team_id denormalise"
    destinations ||--o{ resources : "reseau Docker cible"
    teams ||--o{ tags : "etiquettes"
    resources ||--o{ resource_tags : ""
    tags ||--o{ resource_tags : "N-N"
    resources ||--o| applications : "extension 1-1"
    resources ||--o| databases : "extension 1-1"
    resources ||--o| services : "extension 1-1"

    projects {
        uuid uuid UK
        text slug "unique par team"
    }
    environments {
        uuid uuid UK
        text slug "unique par project"
    }
    resources {
        bigint id PK
        uuid uuid UK "base des noms Docker"
        resource_type resource_type "app-db-service"
        resource_desired_status desired_status
        resource_observed_status observed_status
        timestamptz observed_at "stale au-dela d un seuil"
        jsonb remnants "restes distants"
        timestamptz deleted_at "tombstone"
        integer version
    }
```

## 4. Agrégat Infrastructure

```mermaid
erDiagram
    teams ||--o{ servers : "possede"
    teams ||--o{ private_keys : "possede"
    teams ||--o{ cloud_credentials : "possede"
    teams ||--o{ registry_credentials : "possede"
    teams ||--o{ s3_storages : "possede"
    private_keys ||--o{ servers : "acces SSH (RESTRICT)"
    servers ||--o{ destinations : "reseaux Docker"
    servers ||--o{ certificates : "reflet observe (18.3)"
    cloud_credentials |o--o{ servers : "credential DNS-01 (RESTRICT)"
    cloud_credentials |o--o{ certificates : "credential DNS-01 (RESTRICT)"

    servers {
        uuid uuid UK
        text host
        server_status status "machine a etats 21.2"
        proxy_type proxy_type "traefik-caddy-none"
        integer proxy_http_port "defaut 80, configurable"
        integer concurrent_builds "defaut 2"
        boolean is_build_server
        bytea ca_key_enc "chiffre"
        bytea log_drain_config_enc "chiffre"
    }
    private_keys {
        uuid uuid UK
        text fingerprint_sha256
        bytea private_key_enc "chiffre enveloppe"
    }
    s3_storages {
        uuid uuid UK
        bytea access_key_enc "chiffre"
        bytea secret_key_enc "chiffre"
        boolean is_usable "verification obligatoire"
    }
    destinations {
        uuid uuid UK
        text network "unique par serveur"
    }
    certificates {
        uuid uuid UK
        certificate_kind kind "acme-custom-self_signed"
        citext main_domain "UNIQUE(server,kind,main)"
        timestamptz not_after "index - expiration"
        certificate_status status
        timestamptz observed_at "reflet observe"
    }
```

## 5. Agrégat Source

```mermaid
erDiagram
    teams ||--o{ git_sources : "possede"
    teams ||--o{ github_apps : "possede"
    github_apps |o--o{ git_sources : "kind = github_app"
    private_keys |o--o{ git_sources : "deploy key"
    git_sources ||--o{ repositories : "discovery"
    git_sources |o--o{ applications : "source (RESTRICT)"
    repositories |o--o{ applications : "association exacte INV-009"
    applications ||--o{ webhook_endpoints : "webhooks entrants"
    webhook_endpoints |o--o{ webhook_deliveries : "livraisons"
    github_apps |o--o{ webhook_deliveries : "livraisons app"
    webhook_deliveries |o--o{ deployments : "declenche"

    git_sources {
        uuid uuid UK
        git_source_kind kind "public-deploy_key-github_app"
        git_provider provider
    }
    github_apps {
        uuid uuid UK
        bigint app_id
        bytea app_private_key_enc "chiffre"
        bytea webhook_secret_enc "chiffre"
    }
    webhook_deliveries {
        text delivery_id "UNIQUE(provider, delivery_id)"
        boolean signature_valid
        webhook_delivery_status status
        text ignore_reason
    }
```

## 6. Agrégat Application

```mermaid
erDiagram
    resources ||--o| applications : "extension 1-1"
    applications ||--|| build_configs : "1-1"
    applications ||--|| runtime_configs : "1-1"
    applications ||--o{ domains : "FQDN + path + port"
    service_components ||--o{ domains : "domaine par sous-service"
    resources ||--o{ environment_variables : "variables + jeu preview"
    resources ||--o{ persistent_storages : "volume-bind-file"
    resources ||--o| health_checks : "0-1"
    applications ||--o{ previews : "1 par PR"
    users |o--o{ previews : "approbation fork"
    registry_credentials |o--o{ build_configs : "pull / push"
    teams ||--o{ shared_variables : "scope team"
    projects |o--o{ shared_variables : "scope project"
    environments |o--o{ shared_variables : "scope environment"
    servers |o--o{ shared_variables : "scope server"

    applications {
        bigint id PK "FK resources.id"
        text git_branch
        text watch_paths
        boolean previews_enabled
        text preview_url_template
        preview_protection preview_protection "basic_auth par defaut"
        integer preview_ttl_minutes
    }
    build_configs {
        build_pack build_pack "nixpacks par defaut"
        boolean use_build_server
        boolean use_build_secrets
    }
    environment_variables {
        text key "UNIQUE(resource,key,is_preview)"
        bytea value_enc "chiffre enveloppe"
        boolean is_secret
        boolean is_build_time
        boolean is_preview
        boolean is_generated "magic variables"
    }
    previews {
        uuid uuid UK
        integer pr_id "UNIQUE(app,provider,pr_id)"
        preview_status status "inclut cleanup_failed"
        boolean is_fork
        timestamptz last_activity_at "TTL"
    }
    domains {
        citext fqdn "UNIQUE(fqdn,path)"
        text path
        integer target_port
    }
```

## 7. Agrégat Service / Database

```mermaid
erDiagram
    resources ||--o| services : "extension 1-1"
    services ||--o{ service_components : "sous-containers"
    resources ||--o| databases : "extension 1-1"
    servers ||--o{ databases : "port public unique (denormalise)"
    databases ||--o{ database_credentials : "credentials generes"
    databases |o--o{ database_backup_plans : "cible"
    service_components |o--o{ database_backup_plans : "base interne de service"
    database_backup_plans ||--o{ backup_executions : "historique"
    s3_storages |o--o{ database_backup_plans : "destination S3 (RESTRICT)"
    jobs |o--o| backup_executions : "execute"

    services {
        bigint id PK "FK resources.id"
        text compose_content "source de verite"
        text template_slug "catalogue one-click"
    }
    service_components {
        uuid uuid UK
        text name "UNIQUE(service,name)"
        boolean is_database "backupable"
        boolean exclude_from_hc
        resource_observed_status observed_status
    }
    databases {
        db_engine engine "8 moteurs"
        boolean is_public
        integer public_port "UNIQUE(server,port)"
        boolean ssl_enabled
    }
    database_credentials {
        text username
        bytea password_enc "chiffre enveloppe"
    }
    backup_executions {
        backup_execution_status status "partial = succes local echec S3"
        text checksum_sha256
        boolean uploaded_to_s3
    }
```

## 8. Agrégat Exécution

```mermaid
erDiagram
    resources ||--o{ deployments : "historique"
    previews |o--o{ deployments : "deploiement de preview"
    servers ||--o{ deployments : "cible + file par serveur"
    servers |o--o{ deployments : "build server"
    webhook_deliveries |o--o{ deployments : "origine"
    api_tokens |o--o{ deployments : "declenche par token"
    deployments |o--o{ deployments : "retry_of"
    deployments ||--o{ deployment_steps : "timeline + logs"
    deployments ||--o{ deployment_artifacts : "images rollback"
    resources ||--o{ scheduled_tasks : "crons"
    scheduled_tasks ||--o{ task_executions : "historique"
    jobs |o--o| task_executions : "execute"
    teams ||--o{ terminal_sessions : "auditees"
    users |o--o{ terminal_sessions : ""
    servers |o--o{ terminal_sessions : "cible"
    resources |o--o{ terminal_sessions : "container cible"

    deployments {
        uuid uuid UK
        deployment_status status "machine a etats 21.1"
        integer attempt
        deployment_trigger trigger
        text commit_sha "SHA immuable"
        text image_digest "digest OCI"
        jsonb config_snapshot "INV-014, redacted"
    }
    deployment_steps {
        integer seq "UNIQUE(deployment,seq)"
        deployment_step_status status
        text log "neutralise, redacted"
    }
    deployment_artifacts {
        artifact_kind kind "local_image-registry_image"
        boolean protected_from_cleanup "INV-015"
    }
    jobs {
        uuid uuid UK
        text queue
        job_status status "machine a etats 21.3"
        text lock_key "UNIQUE si leased-running"
        text idempotency_key UK
        timestamptz lease_expires_at
        timestamptz heartbeat_at
    }
```

## 9. Agrégat Plateforme

```mermaid
erDiagram
    servers ||--o{ proxy_config_revisions : "revisions checksummees"
    teams ||--o{ notification_channels : "canaux"
    notification_channels ||--o{ notification_rules : "evenement par canal"
    projects |o--o{ notification_rules : "routage"
    environments |o--o{ notification_rules : "routage"
    teams |o--o{ feature_flags : "override par team"
    teams |o--o{ jobs : "limites par team"
    resources |o--o{ jobs : "cible"

    proxy_config_revisions {
        integer revision "UNIQUE(server,revision)"
        text checksum_sha256
        proxy_revision_status status
    }
    notification_channels {
        notification_channel_kind kind
        bytea config_enc "chiffre enveloppe"
    }
    audit_events {
        uuid uuid UK "append-only, SANS FK"
        bigint team_id "snapshot"
        actor_kind actor_kind
        uuid actor_uuid "snapshot"
        text action
        audit_result result
    }
    outbox_events {
        bigint id PK "ordre de publication"
        uuid uuid UK "dedup consommateurs"
        text event_type "versionne .v1"
        uuid team_uuid "reference, pas FK"
        timestamptz published_at "NULL = a publier"
    }
    instance_settings {
        smallint id PK "singleton CHECK id=1"
        boolean api_enabled "false par defaut"
        bytea transactional_email_config_enc "chiffre"
    }
```

> `audit_events` et `outbox_events` n'ont volontairement **aucune FK** : ce sont des faits immuables qui référencent par UUID public et survivent à la suppression de leurs sujets (§19.2, §23.4, §24.2).

---

## 10. Ownership et isolation team (INV-001, INV-002)

Chaque table remonte à exactement une team, directement ou par chaîne parent. Le `team_id` utilisé dans les requêtes provient **toujours** du contexte authentifié (§23.1), jamais d'un paramètre client.

| Chemin vers la team | Tables |
|---|---|
| `team_id` direct | `projects`, `resources` (dénormalisé + trigger de cohérence), `servers`, `private_keys`, `cloud_credentials`, `registry_credentials`, `s3_storages`, `git_sources`, `github_apps`, `api_tokens`, `tags`, `shared_variables`, `notification_channels`, `terminal_sessions`, `invitations`, `team_memberships` |
| Via 1 parent | `environments` → `projects` ; `destinations` → `servers` ; `certificates` → `servers` ; `repositories` → `git_sources` ; `notification_rules` → `notification_channels` ; `proxy_config_revisions` → `servers` ; `applications`/`databases`/`services` → `resources` |
| Via 2+ parents | `build_configs`, `runtime_configs`, `webhook_endpoints`, `previews` → `applications` → `resources` ; `environment_variables`, `persistent_storages`, `health_checks`, `scheduled_tasks`, `deployments` → `resources` ; `service_components` → `services` → `resources` ; `database_credentials`, `database_backup_plans` → `databases` → `resources` ; `deployment_steps`, `deployment_artifacts` → `deployments` ; `backup_executions` → `database_backup_plans` ; `task_executions` → `scheduled_tasks` ; `domains` → `applications` ou `service_components` ; `resource_tags` → `resources` |
| Hors team (instance) | `users`, `identities`, `mfa_factors`, `sessions` (scopées utilisateur), `instance_settings`, `feature_flags` (`team_id` NULL = instance) |
| `team_id` nullable (résolu ou snapshot) | `webhook_deliveries` (résolue à l'association §20.3), `jobs` (jobs de maintenance sans team), `audit_events` / `outbox_events` (snapshot sans FK) |

Application de l'isolation : toute requête d'accès joint la chaîne d'ownership jusqu'à `team_id` et le compare au contexte ; un UUID valide d'une autre team produit un 404 indistinguable d'un inexistant (INV-002). La dénormalisation de `team_id` sur `resources` permet ce contrôle et la pagination sans double jointure ; sa cohérence avec `environment_id` est garantie par trigger.

---

## 11. Contraintes d'unicité

| Table | Contrainte | Justification |
|---|---|---|
| toutes | `UNIQUE (uuid)` | Identifiant public (§19.2). |
| `users` | `UNIQUE (email) WHERE deleted_at IS NULL` | Réutilisation possible après tombstone. |
| `identities` | `UNIQUE (provider, provider_subject)` | Une identité fédérée = un compte (§23.3). |
| `mfa_factors` | `UNIQUE (user_id, type)` | Un facteur TOTP par utilisateur. |
| `sessions`, `api_tokens`, `invitations` | `UNIQUE (token_hash)` | Lookup O(1) par hash. |
| `team_memberships` | `UNIQUE (team_id, user_id)` | Une appartenance par couple. |
| `invitations` | `UNIQUE (team_id, email) WHERE accepted_at IS NULL AND revoked_at IS NULL` | Une invitation active à la fois. |
| `projects` | `UNIQUE (team_id, slug) WHERE deleted_at IS NULL` | Slugs uniques dans le parent (§19.2). |
| `environments` | `UNIQUE (project_id, slug) WHERE deleted_at IS NULL` | Idem. |
| `resources` | `UNIQUE (environment_id, name) WHERE deleted_at IS NULL` | Nom unique par environnement. |
| `tags` | `UNIQUE (team_id, name)` | — |
| `servers` | `UNIQUE (team_id, name) WHERE deleted_at IS NULL` | — |
| `destinations` | `UNIQUE (server_id, network)` ; `UNIQUE (server_id) WHERE is_default` | Noms Docker uniques par destination (§19.2) ; une destination par défaut. |
| `certificates` | `UNIQUE (server_id, kind, main_domain)` | Un reflet observé par certificat servi sur le serveur. |
| `private_keys` | `UNIQUE (team_id, fingerprint_sha256)` | Anti-doublon de clé. |
| `cloud_credentials`, `registry_credentials`, `s3_storages`, `git_sources`, `notification_channels` | `UNIQUE (team_id, name)` | Nommage stable par team. |
| `github_apps` | `UNIQUE (team_id, app_id)` | Une app GitHub enregistrée une fois par team. |
| `repositories` | `UNIQUE (git_source_id, external_id)` | Association webhook exacte (INV-009). |
| `webhook_endpoints` | `UNIQUE (application_id, provider)` | Un endpoint par provider et par app. |
| `webhook_deliveries` | `UNIQUE (provider, delivery_id)` | **Déduplication des webhooks (INV-009)** — l'insert en conflit marque `duplicate`. |
| `domains` | `UNIQUE (fqdn, path)` | Routage sans ambiguïté, pas de collision inter-team. |
| `environment_variables` | `UNIQUE (resource_id, key, is_preview)` | Jeu production et jeu preview disjoints (§5.6). |
| `shared_variables` | `UNIQUE` partiel par scope (`team`/`project`/`environment`/`server` + `key`) | Une valeur par clé et par niveau (§5.4). |
| `persistent_storages` | `UNIQUE (resource_id, mount_path)` | Un montage par chemin. |
| `health_checks` | `UNIQUE (resource_id)` | 1—1. |
| `previews` | `UNIQUE (application_id, provider, pr_id)` | **Identité de preview déterministe, jamais recyclée (§20.4)**. |
| `service_components` | `UNIQUE (service_id, name)` | Noms compose uniques dans le stack. |
| `databases` | `UNIQUE (server_id, public_port) WHERE is_public` | **Réservation de port public — forte cohérence (§22.3)**. |
| `database_credentials` | `UNIQUE (database_id, username)` | — |
| `deployment_steps` | `UNIQUE (deployment_id, seq)` | Timeline ordonnée. |
| `scheduled_tasks` | `UNIQUE (resource_id, name)` | — |
| `proxy_config_revisions` | `UNIQUE (server_id, revision)` | Révisions monotones par serveur. |
| `notification_rules` | `UNIQUE NULLS NOT DISTINCT (channel_id, event_type, project_id, environment_id)` | Une règle par événement et par scope. |
| `feature_flags` | `UNIQUE NULLS NOT DISTINCT (key, team_id)` | Instance + override par team. |
| `build_configs`, `runtime_configs` | `UNIQUE (application_id)` | 1—1. |
| `jobs` | `UNIQUE (idempotency_key)` ; `UNIQUE (lock_key) WHERE status IN ('leased','running')` | **Idempotence (INV-004)** ; **verrou exclusif par ressource/serveur (§21.1 `switching`)**. |
| `instance_settings` | `CHECK (id = 1)` | Singleton. |

---

## 12. Indexes clés (justifiés par les requêtes)

| Index | Requête servie |
|---|---|
| `(team_id, id DESC)` sur `projects`, `servers`, `resources`, `api_tokens`, `private_keys`, `git_sources`, `s3_storages`, `notification_channels`… | Listes paginées par team (pagination par curseur `id`, §24.1 ; P95 < 300 ms à 50 utilisateurs, §16.4 ; 2 000 ressources/instance, §22.2). |
| `jobs (queue, priority DESC, run_at, id) WHERE status = 'queued'` | **Dequeue `SELECT … FOR UPDATE SKIP LOCKED`** : le partiel ne contient que les jobs éligibles, le tri correspond à l'ordre de consommation (§21.3, §27.2). |
| `jobs (lease_expires_at) WHERE status IN ('leased','running')` | Récupération des leases expirés par le reaper — reprise après crash (INV-013, §21.3). |
| `jobs UNIQUE (lock_key) WHERE status IN ('leased','running')` | Exclusion mutuelle deploy/switch par application/destination (§21.1) sans table de verrous séparée. |
| `webhook_deliveries UNIQUE (provider, delivery_id)` | **Déduplication à l'insert** : un replay provider devient un `ON CONFLICT` → `duplicate`, en O(1), avant tout traitement (INV-009, 1 000 livraisons/min §22.2). |
| `webhook_deliveries (received_at)`, `(status) WHERE status = 'received'` | File de traitement asynchrone (< 500 ms d'ack, §16.4) et purge de rétention. |
| `deployments (resource_id, id DESC)` | Historique paginé par ressource (100 000 déploiements, §22.2). |
| `deployments (server_id, created_at) WHERE status NOT IN ('succeeded','failed','cancelled')` | File par serveur : `concurrent_builds` / `deployment_queue_limit` (§5.5), vue « en cours/en attente ». |
| `outbox_events (id) WHERE published_at IS NULL` | Publieur outbox : lecture séquentielle des non-publiés dans l'ordre de commit (§18.2, §24.2). |
| `audit_events (team_id, occurred_at DESC)` + BRIN `(occurred_at)` | Audit paginé/filtré par team (§23.4) ; BRIN quasi gratuit sur table append-only pour la purge par rétention. |
| `resources (environment_id)`, `(destination_id)` | Prévisualisation de suppression (§20.6.1) et vérifications RESTRICT (INV-008). |
| `environment_variables (resource_id)`, `persistent_storages (resource_id)`, `domains (application_id)` / `(service_component_id)` | Chargement d'un agrégat application sans relation non bornée (§22.2). |
| `previews (application_id)`, `(last_activity_at) WHERE status = 'active'` | Plafond de previews simultanées et balayage TTL/scale-to-zero (§20.4.3). |
| `backup_executions (backup_plan_id, created_at DESC)`, `task_executions (scheduled_task_id, created_at DESC)` | Historiques paginés + application de la rétention (§7.2, §19.2). |
| `certificates (not_after)` | Alerte d'expiration J-30/J-7 (proxy-contract §7.3) et filtre `expiring_within_days` de l'API certificats. |
| `sessions (user_id)`, `(expires_at)` | Révocation par utilisateur ; purge des sessions expirées. |
| `api_tokens (token_prefix)` | Pré-filtrage du lookup Bearer avant comparaison de hash (§10.3). |

---

## 13. Stratégie de migrations (décision §27.25, §18.2)

- **Outil** : [goose](https://github.com/pressly/goose) (SQL-first, embarquable dans le binaire Go via `embed.FS`, exécuté au démarrage ou par sous-commande `AkerDock migrate`). Migrations **SQL pures versionnées** `NNNN_description.up.sql` / `NNNN_description.down.sql`, numérotation séquentielle, appliquées en transaction quand PostgreSQL le permet.
- **sqlc** : après chaque migration, `sqlc generate` régénère les types Go ; la CI échoue si le code généré n'est pas committé — le schéma, les requêtes et le code ne peuvent pas diverger (§27.25).
- **Down obligatoire** : chaque migration a un down testé en CI (up → down → up). Le down sert au développement et à la procédure de rollback de release (§26.3.6) ; en production, un downgrade suit la procédure documentée (§14.3), jamais un down automatique sur données réelles.
- **Compatibilité rolling upgrade (§18.2)** : deux versions consécutives du binaire doivent coexister sur le même schéma (multi-instance, §22.1). Pattern **expand / contract** :
  1. *Expand* (release N) : ajouts uniquement — nouvelles tables/colonnes nullable ou avec défaut, nouveaux index en `CREATE INDEX CONCURRENTLY` (hors transaction), doubles écritures si renommage.
  2. *Migrate* : backfill par lots (jamais de `UPDATE` massif verrouillant), idempotent et reprenable.
  3. *Contract* (release N+1 au plus tôt) : suppression des colonnes/chemins legacy une fois qu'aucune instance N-1 ne tourne.
- **Enums** : extension uniquement par `ALTER TYPE … ADD VALUE` (additif, sans réécriture) ; jamais de suppression de valeur — une valeur retirée du produit reste dans le type et est refusée par validation applicative. Renommer un statut = nouvelle valeur + migration de données + dépréciation.
- **Interdits en migration ordinaire** : changement de type verrouillant, `NOT NULL` sans défaut sur table volumineuse, suppression de colonne encore lue par la release précédente, index non-`CONCURRENTLY` sur table chaude.
- **Chiffrement** : la rotation de clé maître (§23.2, §27.3) n'est **pas** une migration de schéma — c'est un job applicatif qui réécrit les `*_enc` par `key_version`, par lots, sans blocage.
- **Vérifications CI** : migration up/down sur base vide + sur un dump de fixtures ; test de démarrage de la release N-1 sur le schéma N (garde rolling upgrade) ; `EXPLAIN` de non-régression sur les requêtes critiques (dequeue jobs, dédup webhooks, listes paginées).
