# Data dictionary — AkerDock

> Artefact §29.1 du PRD (`docs/PRD.md`). Couvre toutes les entités du modèle de données logique (§19.1), les contraintes du §19.2, les machines à états du §21, les exigences de sécurité du §23.2 et les tables techniques (queue §21.3, outbox §18.2/§24.2, audit §23.4). Le PRD est la source de vérité ; toute divergence est signalée explicitement.

Conventions de nommage : noms de tables et de colonnes en anglais `snake_case`, tables au pluriel. Le document lui-même est en français.

---

## 1. Glossaire

| Terme | Définition |
|---|---|
| **Team** | Périmètre d'isolation et frontière de sécurité (§2, §23.1). Toute ressource, clé, token ou notification appartient à exactement une team (INV-001) ; aucun accès inter-team n'est possible (INV-002). |
| **Project** | Regroupement logique au sein d'une team ; contient des environments (défaut : `production`). |
| **Environment** | Jeu de ressources et de variables partagées au sein d'un project (production, staging…). Peut être déployé comme une unité (§20.8). |
| **Resource** | Union logique `Application \| Database \| Service` (§19.1) : champs communs (UUID, team, environnement, destination, statuts désiré/observé, politique de suppression). |
| **Application** | Ressource construite depuis un dépôt Git, un Dockerfile, un compose ou une image, déployée en container(s) derrière le proxy (§5). |
| **Database** | Base de données managée one-click (PostgreSQL, MySQL, MariaDB, MongoDB, Redis, KeyDB, Dragonfly, ClickHouse — §6) avec credentials générés, SSL et backups. |
| **Service** | Stack Docker Compose multi-containers issu du catalogue one-click ou d'un compose utilisateur (§9), composé de service components. |
| **Service component** | Sous-container d'un service (un service du fichier compose) : statut, domaine, logs et restart individuels (§9). |
| **Server** | Machine Linux pilotée en SSH, hébergeant Docker, un proxy et éventuellement l'agent Sentinel (§3). Machine à états §21.2. |
| **Destination** | Réseau Docker cible sur un serveur ; une ressource est déployée sur exactement une destination (§2). |
| **Private key** | Clé SSH privée stockée chiffrée, scopée par team, utilisée pour les serveurs et les deploy keys Git (§3.1, §23.2). |
| **Git source** | Connexion à un fournisseur Git : dépôt public, deploy key ou GitHub App (§5.1). |
| **Build pack** | Stratégie de build d'une application : Nixpacks, Railpack, Static, Dockerfile, Docker Compose ou image pré-construite (§5.2). |
| **Deployment** | Exécution versionnée du pipeline de déploiement d'une ressource (machine à états §21.1), avec SHA, digest OCI, snapshot de configuration (INV-014) et logs par étape. |
| **Preview** | Environnement éphémère déployé pour une PR/MR : identité déterministe `(application, provider, pr_id)`, URL dédiée, variables séparées, TTL et cleanup (§5.6, §20.4). |
| **Magic variable** | Variable `SERVICE_<TYPE>_<ID>` générée par la plateforme (URL, FQDN, mots de passe…), persistante entre redéploiements et partagée entre les services d'un stack (§5.4, §9). |
| **Variable partagée** | Variable héritée `{{team.VAR}}` / `{{project.VAR}}` / `{{environment.VAR}}` ou variable de serveur, interpolée dans les ressources (§5.4, §3.1). |
| **Persistent storage** | Stockage d'une ressource : volume Docker nommé, bind mount ou file mount à contenu éditable (§8). |
| **Backup plan / execution** | Planification cron d'un backup de base (locale, S3, rétention §7) et trace de chaque exécution (statut, taille, checksum). |
| **S3 storage** | Configuration d'un stockage objet compatible S3 (credentials chiffrés, vérification obligatoire — §7.4). |
| **Proxy config revision** | Révision versionnée et checksummée de la configuration proxy générée pour un serveur, appliquée atomiquement avec rollback (§18.1, §18.3). |
| **Job / Lease** | Unité de travail durable dans la queue PostgreSQL : lease avec expiration, heartbeat, retry borné et dead-letter (§21.3, INV-013). |
| **Outbox** | Table d'événements internes publiés après commit (pattern transactional outbox, §18.2, §24.2) ; garantit la cohérence entre mutation et événement. |
| **Audit event** | Enregistrement append-only d'une action sensible (login, secret, terminal, déploiement, suppression… — §23.4), sans jamais contenir de valeur secrète (INV-003). |
| **API token** | Token Bearer à permissions granulaires (`read`, `read:sensitive`, `write`, `deploy`, `root`), hashé SHA-256, avec préfixe d'identification, expiration et IP allowlist (§10.3). |
| **Webhook delivery** | Livraison entrante d'un fournisseur Git : authentifiée, associée exactement au bon dépôt et dédupliquée par `(provider, delivery_id)` avant tout déclenchement (INV-009, §20.3). |
| **Statut désiré / observé** | Double statut de toute ressource : l'intention stockée en base vs l'état constaté sur le serveur, avec `observed_at` (au-delà d'un seuil : « inconnu/stale », jamais un faux `running` — §19.2, §21.2). |

---

## 2. Conventions transverses (§19.2, §27.25)

Sauf mention contraire dans une table, les règles suivantes s'appliquent partout :

1. **Identifiants** : `id bigint GENERATED ALWAYS AS IDENTITY` est la clé primaire interne (jointures, jamais exposée) ; `uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE` est l'identifiant public aléatoire, non séquentiel, utilisé dans l'API et comme base des noms Docker (INV-011). Les tables d'association pures (ex. `resource_tags`, `team_memberships`) peuvent omettre `uuid`.
2. **Ownership team (INV-001)** : chaque table porte `team_id` directement, ou y remonte par une chaîne parent documentée dans la section « Ownership » de `docs/specs/erd.md`. Le `team_id` provient toujours du contexte authentifié (§23.1).
3. **Timestamps** : `created_at timestamptz NOT NULL DEFAULT now()` et `updated_at timestamptz NOT NULL DEFAULT now()` partout ; tous les timestamps sont UTC (§22.3). `deleted_at timestamptz NULL` pour le soft delete/tombstone quand la rétention ou la réconciliation l'exige.
4. **Traçabilité** : `created_by` / `updated_by` (`bigint NULL REFERENCES users(id) ON DELETE SET NULL`) sur les agrégats mutables. Aucune cascade depuis un utilisateur supprimé (§19.2) : les utilisateurs sont soft-deleted et l'audit conserve des snapshots.
5. **Verrou optimiste** : colonne `version integer NOT NULL DEFAULT 1`, incrémentée à chaque mutation, sur les agrégats éditables en UI/API (§22.3, §24.1 : réponse `409` en cas de conflit). Les tables enfant 1—1 d'un agrégat (ex. `build_configs`) sont verrouillées via la `version` de leur racine.
6. **Statuts** : état désiré et état observé dans des colonnes séparées ; tout statut observé est accompagné de `observed_at` (§19.2). Les statuts des machines à états du §21 sont des **enums PostgreSQL** (voir §3 ci-dessous) ; les enums ne sont étendus que par `ALTER TYPE … ADD VALUE` (additif, compatible rolling upgrade).
7. **Chiffrement enveloppe (décision §27.3, §23.2)** : toute colonne `*_enc` est un `bytea` au format `key_version (4 octets big-endian) || nonce (12 octets) || ciphertext AES-256-GCM (tag inclus)`, avec comme AAD `nom_table || nom_colonne || uuid de la ligne` (empêche le rejeu d'un ciphertext d'une ligne vers une autre). La rotation de clé maître réécrit les lignes paresseusement par `key_version`, sans blocage global (§19.2). Les mots de passe utilisateur sont hashés **Argon2id**, les tokens (API, session, invitation, Sentinel) sont hashés **SHA-256** avec préfixe d'identification — jamais chiffrés, car jamais restitués.
8. **Suppression** : trois régimes, indiqués par table — **RESTRICT** (interdite tant que référencée : clés, sources, destinations, storages, serveurs — §19.2) ; **CASCADE explicite** (uniquement après prévisualisation et confirmation applicative — §20.6, la FK `ON DELETE CASCADE` ne sert que de filet une fois la décision prise) ; **tombstone** (`deleted_at` + restes distants réconciliables — §20.6.4). « Retirer de AkerDock » est toujours distinct de « supprimer les données » (INV-008).
9. **Rétention** : historiques (déploiements, exécutions de backup/tâches, audit, livraisons webhook, jobs terminés) purgés par un job de rétention configurable (§19.2, §22.2) ; jamais par cascade accidentelle.

Extensions PostgreSQL requises : `citext` (emails), `pgcrypto` (`gen_random_uuid()` natif ≥ PG13, extension conservée pour compat). Contrainte `UNIQUE NULLS NOT DISTINCT` : requiert PostgreSQL ≥ 15 (plage de versions testée, §22.4).

---

## 3. Types énumérés PostgreSQL

| Type | Valeurs | Référence PRD |
|---|---|---|
| `team_role` | `owner`, `admin`, `member` | §10.1 |
| `oauth_provider` | `github`, `gitlab`, `google`, `azure`, `bitbucket`, `oidc` | §10.2 |
| `mfa_type` | `totp` | §10.2 |
| `resource_type` | `application`, `database`, `service` | §19.1 |
| `resource_desired_status` | `stopped`, `running`, `deleting`, `deleted` | §21.2 |
| `resource_observed_status` | `unknown`, `starting`, `healthy`, `unhealthy`, `exited`, `missing` | §21.2 |
| `server_status` | `pending`, `validating`, `ready`, `unreachable`, `maintenance`, `deleting` | §21.2 |
| `proxy_type` | `traefik`, `caddy`, `none` | §4.1, §27.9 |
| `proxy_desired_state` | `running`, `stopped` | §4.1 |
| `proxy_revision_status` | `generated`, `applied`, `failed`, `rolled_back` | §18.1 |
| `certificate_kind` | `acme_http01`, `acme_dns01`, `custom`, `self_signed` | §4.3, §6.3 (proxy-contract §7) |
| `certificate_status` | `pending`, `issued`, `renewing`, `failed`, `expired`, `revoked` | §4.3 (proxy-contract §7.4–7.5) |
| `git_provider` | `github`, `gitlab`, `bitbucket`, `gitea`, `other` | §5.1 |
| `git_source_kind` | `public`, `deploy_key`, `github_app` | §5.1 |
| `webhook_provider` | `github`, `gitlab`, `bitbucket`, `gitea`, `generic` | §12 |
| `webhook_delivery_status` | `received`, `accepted`, `ignored`, `duplicate`, `failed` | §20.3 |
| `build_pack` | `nixpacks`, `railpack`, `static`, `dockerfile`, `compose`, `image` | §5.2 |
| `redirect_direction` | `both`, `www`, `non_www` | §4.2 |
| `storage_kind` | `volume`, `bind`, `file` | §8 |
| `preview_status` | `queued`, `deploying`, `active`, `failed`, `destroying`, `cleanup_failed`, `destroyed` | §20.4 |
| `preview_protection` | `none`, `basic_auth`, `signed_link` | §20.4.4 |
| `db_engine` | `postgresql`, `mysql`, `mariadb`, `mongodb`, `redis`, `keydb`, `dragonfly`, `clickhouse` | §6.1 |
| `public_access_mode` | `port_mapping`, `tcp_proxy` | §6.2 |
| `backup_execution_status` | `running`, `succeeded`, `partial`, `failed` | §20.5 |
| `deployment_status` | `queued`, `preparing`, `cloning`, `building`, `pushing`, `starting`, `healthchecking`, `switching`, `finishing`, `succeeded`, `failed`, `cancelled`, `retrying`, `superseded` | §21.1 (`superseded` : coalescing §20.3.5, terminal assimilé à `cancelled`) |
| `deployment_step_status` | `pending`, `running`, `succeeded`, `failed`, `skipped`, `cancelled` | §20.2 |
| `deployment_trigger` | `manual`, `webhook`, `api`, `preview`, `schedule`, `config_apply`, `cli_local` | §5.5, §24.5, §27.18 |
| `artifact_kind` | `local_image`, `registry_image` | §27.6 |
| `overlap_policy` | `forbid`, `allow`, `replace` | §24.3 |
| `missed_run_policy` | `skip`, `catch_up_one` | §24.3 |
| `task_execution_status` | `running`, `succeeded`, `failed`, `skipped` | §5.7 |
| `terminal_target` | `server`, `container` | §5.7 |
| `adoption_scan_status` | `pending`, `running`, `completed`, `failed` | §20.7 |
| `uptime_check_kind` | `http`, `tcp` | ADR-017 |
| `uptime_status` | `unknown`, `up`, `down` | ADR-017 |
| `terminal_end_reason` | `user_close`, `idle_timeout`, `max_duration`, `disconnect`, `revoked` | §24.4 |
| `notification_channel_kind` | `smtp`, `resend`, `discord`, `telegram`, `slack`, `pushover`, `webhook` | §11 |
| `notification_severity` | `info`, `warning`, `critical` | §27.19 |
| `actor_kind` | `user`, `token`, `system` | §24.2 |
| `audit_result` | `success`, `failure`, `denied` | §23.4 |
| `job_status` | `scheduled`, `queued`, `leased`, `running`, `retry_wait`, `succeeded`, `cancelled`, `dead_letter` | §21.3 |
| `shared_variable_scope` | `team`, `project`, `environment`, `server` | §5.4 |
| `log_drain_kind` | `none`, `axiom`, `new_relic`, `fluentbit` | §13 |

---

## 4. Agrégat Identité

### 4.1 `users`

Utilisateur du control plane. Suppression : **tombstone** (`deleted_at`) — jamais de cascade vers teams ou ressources (§10.1, §19.2) ; les teams orphelines sont traitées explicitement avant suppression.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | Identifiant interne. |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | Identifiant public. |
| `email` | `citext` | non | — | UNIQUE partiel `WHERE deleted_at IS NULL` | non | Email de connexion, normalisé (§23.3). |
| `name` | `text` | non | — | — | non | Nom affiché. |
| `password_hash` | `text` | oui | — | — | non (hash Argon2id) | NULL si compte OAuth/OIDC uniquement. |
| `is_root` | `boolean` | non | `false` | index partiel `WHERE is_root` | non | Root d'instance (premier utilisateur, §10.1) ; bootstrap possible par variables d'env (§10.2). |
| `email_verified_at` | `timestamptz` | oui | — | — | non | Vérification d'email. |
| `failed_login_count` | `integer` | non | `0` | — | non | Anti-bruteforce (§23.3). |
| `locked_until` | `timestamptz` | oui | — | — | non | Verrouillage progressif après échecs de login. |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `deleted_at` | `timestamptz` | oui | — | — | non | Tombstone. |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 4.2 `identities`

Identité fédérée (OAuth dashboard, SSO OIDC — §10.2). Liaison de compte explicite contre la collision d'email (§23.3). Suppression : **CASCADE** avec l'utilisateur (délestage du tombstone : conservées tant que l'utilisateur est soft-deleted).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `user_id` | `bigint` | non | — | FK `users(id)` ON DELETE CASCADE, index | non | Compte lié. |
| `provider` | `oauth_provider` | non | — | UNIQUE `(provider, provider_subject)` | non | Fournisseur d'identité. |
| `provider_subject` | `text` | non | — | (cf. ci-dessus) | non | `sub` OIDC / ID du compte fournisseur. |
| `email` | `citext` | oui | — | — | non | Email rapporté par le fournisseur (informatif). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 4.3 `mfa_factors`

Facteur 2FA TOTP avec codes de récupération (§10.2, §23.3). Suppression : **CASCADE** avec l'utilisateur ; désactivation auditée.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `user_id` | `bigint` | non | — | FK `users(id)` ON DELETE CASCADE | non | — |
| `type` | `mfa_type` | non | `'totp'` | UNIQUE `(user_id, type)` | non | Type de facteur. |
| `secret_enc` | `bytea` | non | — | — | **oui** | Secret TOTP, chiffré enveloppe. |
| `recovery_code_hashes` | `text[]` | non | `'{}'` | — | non (hash SHA-256) | Codes de récupération hashés, consommés un par un. |
| `confirmed_at` | `timestamptz` | oui | — | — | non | NULL tant que le facteur n'est pas validé par un premier code. |
| `last_used_at` | `timestamptz` | oui | — | — | non | Anti-rejeu du même pas TOTP. |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 4.4 `sessions`

Session navigateur (cookies Secure/HttpOnly/SameSite, rotation après login/élévation — §23.3). Suppression : **purge physique** après expiration/révocation (rétention courte).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `user_id` | `bigint` | non | — | FK `users(id)` ON DELETE CASCADE, index | non | — |
| `token_hash` | `text` | non | — | UNIQUE | non (hash SHA-256) | Hash du token de session ; le token clair n'est jamais stocké. |
| `current_team_id` | `bigint` | oui | — | FK `teams(id)` ON DELETE SET NULL | non | Team active de la session (§10.4 : sessions bornées à la team active). |
| `mfa_verified_at` | `timestamptz` | oui | — | — | non | Étape 2FA franchie. |
| `ip` | `inet` | oui | — | — | non | IP de création. |
| `user_agent` | `text` | oui | — | — | non | — |
| `last_seen_at` | `timestamptz` | non | `now()` | — | non | Activité (idle timeout). |
| `expires_at` | `timestamptz` | non | — | index | non | Expiration absolue. |
| `revoked_at` | `timestamptz` | oui | — | — | non | Invalidation à logout/changement de rôle (§23.3). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

### 4.5 `teams`

Frontière d'isolation (§2, §23.1). Suppression : **RESTRICT** tant qu'il existe des projets, serveurs, clés, storages ou tokens ; procédure explicite, jamais de cascade silencieuse (§10.1).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `name` | `text` | non | — | — | non | Nom affiché. |
| `description` | `text` | oui | — | — | non | — |
| `personal` | `boolean` | non | `false` | — | non | Team personnelle créée automatiquement avec l'utilisateur (§10.1 ; exposée par le schéma OpenAPI `Team`). |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `deleted_at` | `timestamptz` | oui | — | — | non | Tombstone. |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 4.6 `team_memberships`

Appartenance et rôle d'un utilisateur dans une team (§10.1 ; RBAC fin par projet/environnement à venir — décision §27.7, matrice §29.7). Suppression : **CASCADE** avec la team ou l'utilisateur (le retrait d'un membre est audité).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE CASCADE ; UNIQUE `(team_id, user_id)` | non | — |
| `user_id` | `bigint` | non | — | FK `users(id)` ON DELETE CASCADE, index | non | — |
| `role` | `team_role` | non | `'member'` | — | non | `owner` / `admin` / `member`. |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | Changement de rôle. |

### 4.7 `invitations`

Invitation d'un membre par email (§10.1). Suppression : **purge** après expiration/acceptation (rétention courte).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE CASCADE | non | — |
| `email` | `citext` | non | — | UNIQUE partiel `(team_id, email) WHERE accepted_at IS NULL AND revoked_at IS NULL` | non | Destinataire. |
| `role` | `team_role` | non | `'member'` | CHECK `role <> 'owner'` | non | Rôle proposé. |
| `token_hash` | `text` | non | — | UNIQUE | non (hash SHA-256) | Hash du lien d'invitation. |
| `invited_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `expires_at` | `timestamptz` | non | — | — | non | — |
| `accepted_at` | `timestamptz` | oui | — | — | non | — |
| `revoked_at` | `timestamptz` | oui | — | — | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

### 4.8 `api_tokens`

Token API à permissions granulaires (§10.3, §23.2). Affiché une seule fois ; stocké en hash SHA-256 irréversible avec préfixe d'identification. Suppression : révocation (`revoked_at`) puis purge selon rétention ; l'audit conserve un snapshot de l'acteur.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE CASCADE, index `(team_id, id DESC)` | non | Scope team du token (§10.3). |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `name` | `text` | non | — | — | non | Libellé. |
| `token_prefix` | `text` | non | — | index | non | Préfixe public d'identification (§23.2), ex. `akd_a1b2c3`. |
| `token_hash` | `text` | non | — | UNIQUE | non (hash SHA-256) | Hash du token complet ; jamais chiffré car jamais restitué. |
| `permissions` | `text[]` | non | `'{read}'` | CHECK ⊆ `{read, read:sensitive, write, deploy, root}` | non | Permissions évaluées à l'action (§24.1). |
| `ip_allowlist` | `cidr[]` | oui | — | — | non | Restriction CIDR (§10.3). |
| `expires_at` | `timestamptz` | oui | — | — | non | NULL = sans expiration. |
| `last_used_at` | `timestamptz` | oui | — | — | non | Mis à jour de façon paresseuse (pas à chaque requête). |
| `revoked_at` | `timestamptz` | oui | — | — | non | Révocation. |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

---

## 5. Agrégat Organisation

### 5.1 `projects`

Regroupement logique dans une team (§2). Suppression : **interdite tant que des environments contiennent des ressources** ; cascade explicite prévisualisée et confirmée sinon (§19.2, §20.6).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE RESTRICT, index `(team_id, id DESC)` | non | Ownership direct (INV-001). |
| `name` | `text` | non | — | — | non | Nom affiché. |
| `slug` | `text` | non | — | UNIQUE partiel `(team_id, slug) WHERE deleted_at IS NULL` ; CHECK format slug | non | Unicité dans le parent (§19.2). |
| `description` | `text` | oui | — | — | non | — |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `deleted_at` | `timestamptz` | oui | — | — | non | Tombstone. |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 5.2 `environments`

Jeu de ressources d'un project (défaut `production`) ; déployable comme une unité (§20.8). Suppression : identique à `projects` (interdite si non vide, cascade prévisualisée sinon).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `project_id` | `bigint` | non | — | FK `projects(id)` ON DELETE RESTRICT, index | non | Ownership par chaîne (INV-001). |
| `name` | `text` | non | — | — | non | Ex. « Production ». |
| `slug` | `text` | non | — | UNIQUE partiel `(project_id, slug) WHERE deleted_at IS NULL` ; CHECK format slug | non | Unicité dans le parent (§19.2). |
| `description` | `text` | oui | — | — | non | — |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `deleted_at` | `timestamptz` | oui | — | — | non | Tombstone. |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 5.3 `resources`

Table de base de l'union logique `Application | Database | Service` (§19.1) : porte tous les champs communs. Les tables `applications`, `databases` et `services` sont des extensions 1—1 (héritage par classe : `PK = FK resources(id)`). `team_id` est **dénormalisé** depuis la chaîne environment → project → team (cohérence garantie par trigger) pour permettre les vérifications INV-002 et les listes paginées par team sans double jointure. Suppression : **tombstone** + job de suppression idempotent (§20.6) — routage retiré d'abord, puis workloads, puis objet logique ; les restes distants sont conservés dans `remnants`.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | Base des noms Docker déterministes (INV-011) et hostname interne (§2). |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE RESTRICT, index `(team_id, id DESC)` | non | Dénormalisé ; cohérent avec `environment_id` (trigger). |
| `environment_id` | `bigint` | non | — | FK `environments(id)` ON DELETE RESTRICT, index | non | Parent organisationnel. |
| `destination_id` | `bigint` | non | — | FK `destinations(id)` ON DELETE RESTRICT, index | non | Réseau Docker cible (§2). Multi-serveur HA (§3.3, P3) : table d'extension future, hors périmètre. |
| `resource_type` | `resource_type` | non | — | — | non | Discriminant de l'union ; cohérence avec la table d'extension garantie par trigger. |
| `name` | `text` | non | — | UNIQUE partiel `(environment_id, name) WHERE deleted_at IS NULL` | non | Nom affiché, unique dans l'environnement. |
| `description` | `text` | oui | — | — | non | — |
| `desired_status` | `resource_desired_status` | non | `'stopped'` | — | non | Intention (§21.2). |
| `observed_status` | `resource_observed_status` | non | `'unknown'` | — | non | État constaté (§21.2). |
| `observed_at` | `timestamptz` | oui | — | — | non | Fraîcheur de l'observation ; au-delà d'un seuil, l'UI affiche « stale » (§19.2). |
| `last_online_at` | `timestamptz` | oui | — | — | non | Dernière observation `healthy`/`running` (§6.2). |
| `remnants` | `jsonb` | oui | — | — | non | Restes distants après échec de suppression, pour retry/forget (§20.6.4). |
| `adopted_at` | `timestamptz` | oui | — | — | non | Ressource entrée par adoption (§20.7, ADR-013) ; conservé après normalisation (historique, et politique compose `AllowExternalObjects`). |
| `adoption` | `jsonb` | oui | — | — | non | Pointeur vers les objets distants d'origine (`container_name`, `compose_project`, `scan_uuid`) **tant que la ressource n'est pas normalisée** ; le premier déploiement réussi l'efface. Lifecycle, logs, terminal et moteur ciblent ces noms tant qu'il est présent. |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `deleted_at` | `timestamptz` | oui | — | — | non | Tombstone (INV-008). |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste de tout l'agrégat (config incluse — INV-014). |

### 5.4 `tags`

Étiquette libre, N—N avec les ressources (§19.1) ; utilisée notamment pour le deploy par tag (§5.5). Suppression : libre (**CASCADE** sur `resource_tags`).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE CASCADE | non | — |
| `name` | `citext` | non | — | UNIQUE `(team_id, name)` | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

### 5.5 `resource_tags`

Association N—N ressource ↔ tag. Suppression : **CASCADE** des deux côtés.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `resource_id` | `bigint` | non | — | PK composite, FK `resources(id)` ON DELETE CASCADE | non | — |
| `tag_id` | `bigint` | non | — | PK composite, FK `tags(id)` ON DELETE CASCADE, index | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

---

## 6. Agrégat Infrastructure

### 6.1 `servers`

Machine Linux pilotée en SSH (§3), machine à états §21.2. Suppression : **RESTRICT** tant que des destinations/ressources y sont rattachées ; « retirer de AkerDock » toujours distinct de « détruire le VPS fournisseur » (§3.2, INV-008) ; tombstone (`deleted_at`) pendant le retrait.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE RESTRICT, index `(team_id, id DESC)` | non | Ownership direct (INV-001). |
| `name` | `text` | non | — | UNIQUE partiel `(team_id, name) WHERE deleted_at IS NULL` | non | — |
| `description` | `text` | oui | — | — | non | — |
| `host` | `text` | non | — | — | non | IP ou FQDN SSH. |
| `port` | `integer` | non | `22` | CHECK `1..65535` | non | Port SSH. |
| `ssh_user` | `text` | non | `'root'` | — | non | Points acceptés dans le nom (§3.1) ; non-root expérimental. |
| `use_sudo` | `boolean` | non | `false` | — | non | Utilisateur non-root avec `sudo NOPASSWD: ALL` (§3.1). |
| `ssh_timeout_seconds` | `integer` | non | `30` | CHECK `> 0` | non | Timeout de connexion configurable (§3.1). |
| `private_key_id` | `bigint` | non | — | FK `private_keys(id)` ON DELETE RESTRICT | non | Clé SSH d'accès (même team, INV-002). |
| `status` | `server_status` | non | `'pending'` | index partiel `WHERE status <> 'ready'` | non | Machine à états §21.2. |
| `observed_at` | `timestamptz` | oui | — | — | non | Dernière vérification de joignabilité/faits. |
| `unreachable_since` | `timestamptz` | oui | — | — | non | Début d'injoignabilité (notification §11). |
| `os_name` | `text` | oui | — | — | non | Observé lors de la validation (§20.1). |
| `architecture` | `text` | oui | — | CHECK `IN ('amd64','arm64')` | non | Observée ; conditionne build servers (§3.4). |
| `docker_version` | `text` | oui | — | — | non | Observée ; Docker ≥ 24 requis, snap refusé (§3.1). |
| `is_localhost` | `boolean` | non | `false` | — | non | Serveur hébergeant l'instance (§3.1). |
| `is_build_server` | `boolean` | non | `false` | — | non | Serveur dédié au build, n'héberge pas d'applications (§3.4). |
| `wildcard_domain` | `text` | oui | — | — | non | Génération `<uuid>.example.com` ; fallback sslip.io (§4.2). |
| `proxy_type` | `proxy_type` | non | `'traefik'` | — | non | Traefik (défaut) / Caddy (P2) / none (§4.1, §27.9). |
| `proxy_desired_state` | `proxy_desired_state` | non | `'running'` | — | non | Start/stop du proxy (§4.1). |
| `proxy_observed_status` | `resource_observed_status` | non | `'unknown'` | — | non | Statut observé du container proxy. |
| `proxy_http_port` | `integer` | non | `80` | CHECK `1..65535` | non | Configurable par serveur (décision §27.1). |
| `proxy_https_port` | `integer` | non | `443` | CHECK `1..65535` | non | Configurable par serveur (décision §27.1). |
| `concurrent_builds` | `integer` | non | `2` | CHECK `> 0` | non | Slots de build simultanés (§5.5). |
| `deployment_queue_limit` | `integer` | non | `25` | CHECK `> 0` | non | Taille max de file par serveur (§5.5). |
| `cleanup_enabled` | `boolean` | non | `false` | — | non | Automated Docker Cleanup (§3.7). |
| `cleanup_disk_threshold_pct` | `integer` | oui | — | CHECK `1..100` | non | Seuil d'usage disque déclencheur. |
| `cleanup_cron` | `text` | oui | — | — | non | Planification cron du cleanup. |
| `cleanup_prune_volumes` | `boolean` | non | `false` | — | non | Opt-in volumes inutilisés (§3.7). |
| `cleanup_prune_networks` | `boolean` | non | `false` | — | non | Opt-in réseaux **gérés** inutilisés (§3.7, INV-015). |
| `cleanup_next_run_at` | `timestamptz` | oui | — | — | non | Fenêtre cron du cleanup (§3.7), possédée par le scheduler — mêmes règles que `database_backup_plans.next_run_at`. |
| `cleanup_last_run_at` | `timestamptz` | oui | — | — | non | Dernier cleanup effectivement exécuté (cron, seuil ou manuel). |
| `sentinel_enabled` | `boolean` | non | `false` | — | non | Agent de métriques (§3.8, OTLP §27.8). |
| `sentinel_token_hash` | `text` | oui | — | — | non (hash SHA-256) | Token push de l'agent ; vérifié par hash, jamais restitué. |
| `sentinel_push_interval_seconds` | `integer` | non | `10` | CHECK `> 0` | non | Fréquence configurable (§3.8). |
| `sentinel_metrics_retention_days` | `integer` | non | `7` | CHECK `> 0` | non | Rétention configurable (§3.8). |
| `log_drain_kind` | `log_drain_kind` | non | `'none'` | — | non | Log drain par serveur (§13). |
| `log_drain_config_enc` | `bytea` | oui | — | — | **oui** | Config du drain (tokens Axiom/New Relic, config Fluent Bit), chiffrée. |
| `ca_cert` | `text` | oui | — | — | non | Certificat CA plateforme pour SSL des bases (§6.3), montable côté clients. |
| `ca_key_enc` | `bytea` | oui | — | — | **oui** | Clé privée de la CA, chiffrée ; régénérable depuis l'UI (§6.3). |
| `cloud_credential_id` | `bigint` | oui | — | FK `cloud_credentials(id)` ON DELETE SET NULL | non | Provenance si provisionné via un fournisseur cloud (§3.2). |
| `cloud_external_id` | `text` | oui | — | — | non | ID du VPS chez le fournisseur (destruction = action distincte, §3.2). |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `deleted_at` | `timestamptz` | oui | — | — | non | Tombstone. |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

> Les métriques historiques de Sentinel (CPU/RAM/disque, §3.8) ne sont pas modélisées ici : elles transitent en OTLP (décision §27.8) vers un stockage de séries temporelles hors du modèle relationnel.

### 6.2 `destinations`

Réseau Docker cible sur un serveur (§2). Suppression : **RESTRICT** tant que des ressources la référencent (§19.2).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `server_id` | `bigint` | non | — | FK `servers(id)` ON DELETE RESTRICT, index | non | Ownership par chaîne server → team. |
| `name` | `text` | non | — | — | non | Nom affiché. |
| `network` | `text` | non | — | UNIQUE `(server_id, network)` | non | Nom du réseau Docker (unicité des noms Docker par destination, §19.2). |
| `is_default` | `boolean` | non | `false` | UNIQUE partiel `(server_id) WHERE is_default` | non | Destination proposée par défaut sur le serveur. |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 6.3 `private_keys`

Clé SSH privée, chiffrée au repos (§3.1, §23.2) ; fichiers distants `0600`/répertoire `0700`. Suppression : **RESTRICT** tant que référencée par un serveur ou une git source (§19.2) ; rotation assistée. Exception à l'ownership direct : la **clé d'instance** (instance-config §6.2), générée au premier démarrage avant toute team, porte `is_instance = true` et `team_id NULL` — contrainte `CHECK (team_id IS NOT NULL OR is_instance)` et unicité partielle (une seule clé d'instance) ; elle n'apparaît dans aucune liste scopée par team.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | oui¹ | — | FK `teams(id)` ON DELETE RESTRICT, index ; CHECK `(team_id IS NOT NULL OR is_instance)` | non | Sélection par team (§23.2). ¹NULL uniquement pour la clé d'instance. |
| `is_instance` | `boolean` | non | `false` | UNIQUE partiel `WHERE is_instance` | non | Clé SSH d'instance générée au premier démarrage (instance-config §6.2). |
| `name` | `text` | non | — | — | non | — |
| `description` | `text` | oui | — | — | non | — |
| `fingerprint_sha256` | `text` | non | — | UNIQUE `(team_id, fingerprint_sha256)` | non | Empreinte publique ; anti-doublon. |
| `public_key` | `text` | non | — | — | non | Clé publique (affichable, copiable — §22.5). |
| `private_key_enc` | `bytea` | non | — | — | **oui** | Clé privée PEM/OpenSSH, chiffrée enveloppe ; sans passphrase (§3.1) ; restitution soumise à `read:sensitive` (INV-003). |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 6.4 `cloud_credentials`

Credential **DNS-01** d'une team (proxy-contract §7.2, PRD §4.3, amendement n°21) : le jeu de variables d'environnement attendu par Lego pour un provider DNS (`CF_DNS_API_TOKEN`, `AWS_ACCESS_KEY_ID`, …), chiffré enveloppe, matérialisé sur le serveur en `acme.env` (0600) et injecté par `--env-file` — jamais dans un fichier de config généré, jamais en argv (INV-003/INV-012). Historique : la table décrivait initialement les tokens de **provisioning cloud**, retiré du périmètre (ADR-027) — cette entrée documente la table telle que la migration 00035 l'a réellement créée. Suppression : tombstone ; **RESTRICT** tant que référencée par un serveur (`servers.dns_credential_id`) ou un certificat.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE RESTRICT, index | non | — |
| `name` | `text` | non | — | UNIQUE `(team_id, name)` | non | — |
| `provider` | `text` | non | — | grammaire fermée (INV-012) | non | Identifiant **Lego** (`cloudflare`, `route53`, `ovh`, `hetzner`, …) ; devient le nom du resolver `dns01-<provider>`, donc atteint un fichier de config. |
| `config_enc` | `bytea` | non | — | — | **oui** | Variables d'environnement Lego, chiffrées enveloppe. |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `deleted_at` | `timestamptz` | oui | — | — | non | Tombstone. |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 6.5 `registry_credentials`

Credentials d'un container registry privé (`docker login`, push post-build — §5.1, §5.2). Suppression : **RESTRICT** tant que référencés par une config de build ou un artifact.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE RESTRICT, index | non | — |
| `name` | `text` | non | — | UNIQUE `(team_id, name)` | non | — |
| `registry_url` | `text` | non | — | — | non | Ex. `ghcr.io`, `registry.gitlab.com` ; validé par la policy SSRF (§23.3). |
| `username` | `text` | non | — | — | non | — |
| `password_enc` | `bytea` | non | — | — | **oui** | Mot de passe / token registry, chiffré enveloppe. |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 6.6 `s3_storages`

Stockage objet compatible S3 pour les backups (§7.4). Vérification `ListObjectsV2` obligatoire avant usage ; flag d'utilisabilité + alerte si dégradé. Suppression : **RESTRICT** tant que référencé par un plan de backup (§19.2).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE RESTRICT, index | non | — |
| `name` | `text` | non | — | UNIQUE `(team_id, name)` | non | — |
| `endpoint` | `text` | non | — | — | non | URL du endpoint (policy SSRF §23.3). |
| `bucket` | `text` | non | — | — | non | — |
| `region` | `text` | oui | — | — | non | — |
| `path_style` | `boolean` | non | `true` | — | non | Path-style vs virtual-host (§7.4). |
| `access_key_enc` | `bytea` | non | — | — | **oui** | Access key, chiffrée enveloppe (§7.4 « chiffrés en base »). |
| `secret_key_enc` | `bytea` | non | — | — | **oui** | Secret key, chiffrée enveloppe. |
| `is_usable` | `boolean` | non | `false` | — | non | Passe à `true` après vérification réussie ; alerte si redevient `false` (§7.4). |
| `last_verified_at` | `timestamptz` | oui | — | — | non | — |
| `unusable_since` | `timestamptz` | oui | — | — | non | Début d'indisponibilité constatée. |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 6.7 `certificates`

**Reflet observé** du sous-système certificats d'un serveur (§4.3, §6.3 ; proxy-contract §7). L'état réel vit sur le serveur — `acme.json` (`/var/lib/akerdock/proxy/acme.json`) et les fichiers de `/var/lib/akerdock/proxy/certs/` — cette table n'est **jamais** une source de vérité : elle est mise à jour par le worker après chaque application de configuration proxy et par la réconciliation périodique (§18.3), à des fins d'inventaire et d'alerte d'expiration (J-30/J-7, proxy-contract §7.3/§7.6). La CA gérée par plateforme pour le SSL des bases (§6.3) n'est pas dupliquée ici : elle reste portée par `servers.ca_cert` / `servers.ca_key_enc` (§6.1). Le matériel de clé privée ne quitte jamais le serveur (jamais en base, proxy-contract §7.3). Suppression : **CASCADE** avec le serveur ; une ligne dont le certificat a disparu du serveur est supprimée par la synchronisation (reflet, pas de tombstone ni de verrou optimiste — table non éditable en UI/API).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `server_id` | `bigint` | non | — | FK `servers(id)` ON DELETE CASCADE, index | non | Ownership par chaîne server → team. |
| `kind` | `certificate_kind` | non | — | UNIQUE `(server_id, kind, main_domain)` ; CHECK : `acme_dns01` ⇒ `dns_provider` non NULL | non | Mode d'obtention (§4.3) ; `self_signed` = fallback servi en attendant une émission valide (proxy-contract §7.4). |
| `main_domain` | `citext` | non | — | (cf. `kind`) | non | Domaine principal couvert (CN / premier SAN). |
| `sans` | `citext[]` | non | `'{}'` | — | non | Domaines alternatifs couverts, wildcards inclus (`*.preview.example.com` ⇒ DNS-01 obligatoire, proxy-contract §7.2). |
| `issuer` | `text` | oui | — | — | non | Émetteur observé (ex. `Let's Encrypt R11`) ; NULL tant que rien n'est émis. |
| `not_before` | `timestamptz` | oui | — | — | non | Début de validité observé. |
| `not_after` | `timestamptz` | oui | — | index | non | **Expiration observée — donnée clé du monitoring** : alerte à J-30/J-7 et filtre `expiring_within_days` de l'API. |
| `status` | `certificate_status` | non | `'pending'` | — | non | `pending` = émission en cours (fallback self-signed servi) ; `failed` = échec d'émission/renouvellement (§7.5 proxy-contract). |
| `last_error` | `text` | oui | — | — | non | Dernière erreur d'émission/renouvellement (cause extraite des logs proxy : challenge, rate limit, CAA…) ; jamais de secret (INV-003). |
| `dns_provider` | `text` | oui | — | — | non | Identifiant provider **Lego** (`cloudflare`, `route53`…) pour `acme_dns01` (proxy-contract §7.2). |
| `dns_credential_id` | `bigint` | oui | — | FK `cloud_credentials(id)` ON DELETE RESTRICT | non | Credential DNS-01 (même team, INV-002). Le secret vit dans `cloud_credentials.config_enc` (§6.4) — **aucune colonne secrète ici**. Matérialisé en `/var/lib/akerdock/proxy/acme.env` (0600) à la génération. |
| `cert_path` | `text` | oui | — | — | non | Chemin distant du certificat sur le serveur (`/var/lib/akerdock/proxy/certs/…` pour `custom`) ; NULL pour ACME (matériel dans `acme.json`). |
| `key_path` | `text` | oui | — | — | non | Chemin distant de la clé privée (0600, `custom`) ; le matériel n'est **jamais** rapatrié en base. |
| `observed_at` | `timestamptz` | oui | — | — | non | Fraîcheur du reflet (§19.2) ; au-delà d'un seuil, l'UI affiche « stale », jamais un faux `issued`. |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | Dernière synchronisation. |

### 6.8 `adoption_scans`

Un scan d'adoption (§20.7, ADR-013/ADR-023) : inventaire des containers et stacks compose **non gérés** d'un serveur, avec le mapping proposé vers le modèle AkerDock. `candidates` porte les candidats tels que servis par l'API (adoptables et non adoptables, avec motifs), **plus** deux champs internes au scan (`compose_content` réécrit, `compose_working_dir`) que le handler ne réémet jamais. Les **noms** de variables d'environnement y figurent, **jamais les valeurs** (INV-003) : celles-ci sont capturées et chiffrées enveloppe au moment de l'adoption. Suppression : **CASCADE** avec le serveur — un scan est un instantané, pas une ressource.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE CASCADE, index `(team_id, id DESC)` | non | Isolation team (INV-002). |
| `server_id` | `bigint` | non | — | FK `servers(id)` ON DELETE CASCADE, index `(server_id, id DESC)` | non | Serveur scanné. |
| `status` | `adoption_scan_status` | non | `'pending'` | enum `pending/running/completed/failed` | non | — |
| `error` | `text` | oui | — | — | non | Cause quand `failed`. |
| `candidates` | `jsonb` | oui | — | — | non | Candidats au format API + champs internes (voir ci-dessus). Rempli quand `completed`. |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `completed_at` | `timestamptz` | oui | — | — | non | — |

### 6.9 `uptime_checks`

Un check d'uptime (ADR-017) : sonde HTTP/TCP exécutée **depuis le control plane** — hors du workload surveillé —, verdict à seuils (le basculement après N résultats consécutifs EST l'anti-flapping ; le notifier ne voit que les transitions). La fenêtre (`next_run_at`) est possédée par le scheduler ; la granularité effective est bornée par son tick. Suppression : tombstone ; l'historique suit en CASCADE.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE CASCADE, index `(team_id, id DESC)` | non | Isolation team (INV-002). |
| `resource_id` | `bigint` | oui | — | FK `resources(id)` ON DELETE CASCADE | non | Lien optionnel : l'historique « par ressource » d'ADR-017. |
| `name` | `text` | non | — | UNIQUE `(team_id, name)` | non | — |
| `kind` | `uptime_check_kind` | non | — | — | non | `http` (URL, up = réponse < 400) ou `tcp` (host:port, up = connexion). |
| `target` | `text` | non | — | — | non | URL ou `host:port`, validé à l'entrée. |
| `interval_seconds` | `integer` | non | `60` | CHECK `>= 10` | non | — |
| `timeout_seconds` | `integer` | non | `10` | CHECK `1..60` | non | — |
| `failure_threshold` | `integer` | non | `3` | CHECK `> 0` | non | Échecs consécutifs avant `down`. |
| `success_threshold` | `integer` | non | `2` | CHECK `> 0` | non | Succès consécutifs avant retour `up` (depuis `unknown`, un seul suffit). |
| `enabled` | `boolean` | non | `true` | — | non | — |
| `status` | `uptime_status` | non | `'unknown'` | — | non | Verdict courant — écrit par le prober seul. |
| `status_since` | `timestamptz` | oui | — | — | non | Début du verdict courant. |
| `consecutive_failures` | `integer` | non | `0` | — | non | Compteur de la machine à états. |
| `consecutive_successes` | `integer` | non | `0` | — | non | — |
| `last_checked_at` | `timestamptz` | oui | — | — | non | — |
| `last_latency_ms` | `integer` | oui | — | — | non | — |
| `last_error` | `text` | oui | — | — | non | Dernier motif d'échec. |
| `next_run_at` | `timestamptz` | oui | — | — | non | Fenêtre du prober ; NULL = à resemer (création ou édition). |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `deleted_at` | `timestamptz` | oui | — | — | non | Tombstone. |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste (les écritures du prober ne le bumpent jamais). |

### 6.10 `uptime_check_results`

L'historique brut des sondes, borné par rétention (30 jours) — le verdict courant vit sur le check. Suppression : **CASCADE** avec le check.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `check_id` | `bigint` | non | — | FK `uptime_checks(id)` ON DELETE CASCADE, index `(check_id, id DESC)` | non | — |
| `checked_at` | `timestamptz` | non | `now()` | — | non | — |
| `ok` | `boolean` | non | — | — | non | — |
| `latency_ms` | `integer` | oui | — | — | non | — |
| `status_code` | `integer` | oui | — | — | non | HTTP seulement. |
| `error` | `text` | oui | — | — | non | Motif d'échec de la sonde. |

---

## 7. Agrégat Source

### 7.1 `git_sources`

Connexion à un fournisseur Git (§5.1) : dépôt public, deploy key (clé SSH) ou GitHub App. Suppression : **RESTRICT** tant que référencée par une application (§19.2).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE RESTRICT, index | non | — |
| `name` | `text` | non | — | UNIQUE `(team_id, name)` | non | — |
| `kind` | `git_source_kind` | non | — | CHECK cohérence : `deploy_key` ⇒ `private_key_id` non NULL ; `github_app` ⇒ `github_app_id` non NULL | non | Type de connexion (§5.1). |
| `provider` | `git_provider` | non | — | — | non | GitHub, GitLab, Bitbucket, Gitea, autre. |
| `api_url` | `text` | oui | — | — | non | Endpoint API (self-hosted / GitHub Enterprise, §5.1). |
| `html_url` | `text` | oui | — | — | non | Base des URLs web du fournisseur. |
| `api_token_enc` | `bytea` | oui | — | chiffrement enveloppe (§23.2) | **oui** | Token API du provider (protocols §3-§6) : feedback de preview dégradé et vérification des droits des commandes sans GitHub App. Write-only (INV-003). |
| `private_key_id` | `bigint` | oui | — | FK `private_keys(id)` ON DELETE RESTRICT | non | Deploy key (même team, INV-002). |
| `github_app_id` | `bigint` | oui | — | FK `github_apps(id)` ON DELETE RESTRICT | non | Intégration GitHub App. |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 7.2 `github_apps`

GitHub App enregistrée : discovery des repos, auto-deploy, previews, commentaires de PR (§5.1) ; GitHub Enterprise supporté. Suppression : **RESTRICT** tant que référencée par une git source.

> **Amendement (migration 00039)** : le manifest flow (git-webhook-protocols §2.1) crée la ligne en **brouillon** — `app_id`, `client_id` et les secrets chiffrés sont donc **nullables** jusqu'à la conversion (l'unicité `(team_id, app_id)` est un index partiel `WHERE app_id IS NOT NULL`). Colonnes ajoutées : `slug` (URL d'installation), `manifest_state_hash` / `manifest_state_expires_at` (state anti-CSRF du callback, haché, usage unique, 10 min).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE RESTRICT, index | non | — |
| `name` | `text` | non | — | — | non | Nom de l'app GitHub. |
| `app_id` | `bigint` | non | — | UNIQUE `(team_id, app_id)` | non | ID de l'application chez GitHub. |
| `installation_id` | `bigint` | oui | — | — | non | NULL tant que l'app n'est pas installée sur un compte/org. |
| `client_id` | `text` | oui | — | — | non | OAuth client ID de l'app. |
| `client_secret_enc` | `bytea` | oui | — | — | **oui** | OAuth client secret, chiffré enveloppe (§23.2). |
| `webhook_secret_enc` | `bytea` | oui | — | — | **oui** | Secret HMAC des webhooks de l'app, chiffré (INV-009). |
| `app_private_key_enc` | `bytea` | non | — | — | **oui** | Clé privée RSA (PEM) de l'app, chiffrée ; signe les JWT d'installation. |
| `api_url` | `text` | non | `'https://api.github.com'` | — | non | GitHub Enterprise : URL custom. |
| `html_url` | `text` | non | `'https://github.com'` | — | non | — |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 7.3 `repositories`

Cache des dépôts découverts via une source (discovery GitHub App, §5.1) ; sert à l'association exacte webhook → ressource (INV-009, §20.3). Suppression : **CASCADE** avec la source (cache resynchronisable) ; les applications y pointent en `SET NULL`.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `git_source_id` | `bigint` | non | — | FK `git_sources(id)` ON DELETE CASCADE, index | non | Ownership par chaîne source → team. |
| `external_id` | `text` | non | — | UNIQUE `(git_source_id, external_id)` | non | ID du repo chez le fournisseur (stable au renommage). |
| `full_name` | `text` | non | — | index | non | `owner/name` ; comparaison exacte, jamais par préfixe (§23.5). |
| `default_branch` | `text` | oui | — | — | non | — |
| `html_url` | `text` | oui | — | — | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | Dernière resynchronisation. |

### 7.4 `webhook_endpoints`

Configuration d'un webhook entrant par application : provider Git (webhooks manuels §5.1/§5.5) ou `generic` (deploy webhook CI custom §12). Suppression : **CASCADE** avec l'application.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | Entre dans l'URL de réception. |
| `application_id` | `bigint` | non | — | FK `applications(id)` ON DELETE CASCADE ; UNIQUE `(application_id, provider)` | non | Ressource cible. |
| `provider` | `webhook_provider` | non | — | — | non | GitHub / GitLab / Bitbucket / Gitea / générique. |
| `secret_enc` | `bytea` | non | — | — | **oui** | Secret HMAC de validation de signature (§5.5, §23.2), chiffré enveloppe. |
| `enabled` | `boolean` | non | `true` | — | non | — |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 7.5 `webhook_deliveries`

Livraison webhook entrante, persistée avant réponse `2xx` puis traitée asynchronement (§20.3) ; déduplication par `(provider, delivery_id)` (INV-009). Suppression : **purge** par rétention (1 000 livraisons/min en burst, §22.2).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `provider` | `webhook_provider` | non | — | UNIQUE `(provider, delivery_id)` | non | — |
| `delivery_id` | `text` | non | — | (cf. ci-dessus) | non | ID de livraison du fournisseur (ex. `X-GitHub-Delivery`) ; généré si absent (generic). |
| `webhook_endpoint_id` | `bigint` | oui | — | FK `webhook_endpoints(id)` ON DELETE SET NULL | non | Endpoint destinataire (webhooks manuels/génériques). |
| `github_app_id` | `bigint` | oui | — | FK `github_apps(id)` ON DELETE SET NULL | non | Livraisons reçues au niveau GitHub App. |
| `event_type` | `text` | oui | — | — | non | Ex. `push`, `pull_request`, `merge_request`. |
| `signature_valid` | `boolean` | non | `false` | — | non | Résultat de la vérification HMAC/horodatage (§20.3). |
| `status` | `webhook_delivery_status` | non | `'received'` | index partiel `WHERE status = 'received'` | non | Cycle accepté/ignoré/dupliqué/échoué. |
| `ignore_reason` | `text` | oui | — | — | non | Ex. `skip_ci`, `fork_untrusted` (INV-010), `watch_paths`, `auto_deploy_disabled`. |
| `payload` | `jsonb` | oui | — | — | non | Payload tronqué à la limite de taille (§20.3) ; jamais de secret. |
| `team_id` | `bigint` | oui | — | FK `teams(id)` ON DELETE SET NULL, index | non | Résolue lors de l'association à une ressource. |
| `application_id` | `bigint` | oui | — | FK `applications(id)` ON DELETE SET NULL | non | Ressource associée (exactement une, même team — §20.3). |
| `received_at` | `timestamptz` | non | `now()` | index | non | Réception (< 500 ms avant `2xx`, §16.4). |
| `processed_at` | `timestamptz` | oui | — | — | non | Fin du traitement asynchrone. |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

---

## 8. Agrégat Application

### 8.1 `applications`

Extension 1—1 de `resources` (`resource_type = 'application'`) : identité de la source et politiques de déploiement/preview (§5). Suppression : **CASCADE** technique avec la ligne `resources` (la décision passe par le workflow tombstone de la ressource, §20.6). Champs communs (statuts, team, destination, version…) : voir `resources` (§5.3).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | — | PK, FK `resources(id)` ON DELETE CASCADE | non | Héritage par classe. |
| `git_source_id` | `bigint` | oui | — | FK `git_sources(id)` ON DELETE RESTRICT | non | NULL pour les apps « image » ou Dockerfile inline. |
| `repository_id` | `bigint` | oui | — | FK `repositories(id)` ON DELETE SET NULL | non | Repo découvert (GitHub App) ; association webhook exacte (INV-009). |
| `git_repository_url` | `text` | oui | — | — | non | URL HTTPS d'un repo public (§5.1) ; policy SSRF (§23.3). |
| `git_branch` | `text` | oui | — | — | non | Branche suivie. |
| `base_directory` | `text` | non | `'/'` | — | non | Racine de build (monorepos, §5.1). |
| `enable_submodules` | `boolean` | non | `false` | — | non | §5.1. |
| `enable_lfs` | `boolean` | non | `false` | — | non | §5.1. |
| `enable_shallow_clone` | `boolean` | non | `false` | — | non | §5.1. |
| `auto_deploy_enabled` | `boolean` | non | `true` | — | non | Toggle « Auto Deploy » : événements webhook ignorés si `false` (§5.5). |
| `watch_paths` | `text` | oui | — | — | non | Patterns (un par ligne) limitant l'auto-deploy en monorepo (§5.5) ; appliqués aussi aux previews (§20.4.5). |
| `previews_enabled` | `boolean` | non | `false` | — | non | Preview par PR/MR (§5.6). |
| `preview_url_template` | `text` | non | `'{{pr_id}}.{{domain}}'` | — | non | Placeholders `{{pr_id}}`, `{{domain}}`, `{{random}}` (§5.6). |
| `preview_public_prs_enabled` | `boolean` | non | `false` | — | non | Opt-in PR publiques (§5.6) ; forks ignorés par défaut (INV-010). |
| `preview_fork_approval_enabled` | `boolean` | non | `false` | — | non | Forks sur approbation d'un mainteneur, builder isolé, zéro secret (§20.4.8). |
| `preview_max_concurrent` | `integer` | oui | — | CHECK `> 0` | non | Plafond de previews simultanées ; NULL = défaut instance (§20.4.3). |
| `preview_ttl_minutes` | `integer` | oui | — | CHECK `> 0` | non | TTL d'inactivité avant destruction automatique (§20.4.3). |
| `preview_protection` | `preview_protection` | non | `'basic_auth'` | — | non | Protégée par défaut + `X-Robots-Tag: noindex` ; `none` = choix explicite (§20.4.4). |
| `preview_require_label` | `text` | oui | — | — | non | Opt-in par label de PR (§20.4.7) ; NULL = désactivé. |
| `preview_comment_commands_enabled` | `boolean` | non | `false` | — | non | Commandes `/deploy`, `/destroy` en commentaire (§20.4.7). |
| `preview_exclude_drafts` | `boolean` | non | `false` | — | non | §20.4.7. |
| `preview_deploy_on_open` | `boolean` | non | `true` | — | non | Auto-déploiement à l'ouverture d'une PR ; `false` = premier déploiement manuel (UI ou `/deploy`), §20.4.7. |
| `preview_cancel_obsolete_builds` | `boolean` | non | `false` | — | non | Annulation du build de preview rendu obsolète (§20.4.7). |
| `rollback_on_degraded_health` | `boolean` | non | `false` | — | non | Rollback auto opt-in après bascule (§20.8). |
| `bake_time_seconds` | `integer` | oui | — | CHECK `> 0` | non | Fenêtre d'observation post-bascule (§20.8). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 8.2 `build_configs`

Configuration de build d'une application (§5.2), 1—1. Versionnée via `resources.version` et snapshotée dans chaque déploiement (INV-014). Suppression : **CASCADE** avec l'application.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `application_id` | `bigint` | non | — | UNIQUE, FK `applications(id)` ON DELETE CASCADE | non | — |
| `build_pack` | `build_pack` | non | `'nixpacks'` | — | non | Nixpacks / Railpack (bêta) / Static / Dockerfile / Compose / Image (§5.2). |
| `install_command` | `text` | oui | — | — | non | Override Nixpacks/Railpack (§5.2). |
| `build_command` | `text` | oui | — | — | non | Override. |
| `start_command` | `text` | oui | — | — | non | Override. |
| `publish_directory` | `text` | oui | — | — | non | Mode static : répertoire publié (§5.2). |
| `is_spa` | `boolean` | non | `false` | — | non | Option SPA du pack static. |
| `custom_nginx_config` | `text` | oui | — | — | non | Config Nginx éditable (pack static). |
| `dockerfile_path` | `text` | oui | — | — | non | Chemin du Dockerfile dans le repo. |
| `dockerfile_content` | `text` | oui | — | — | non | Dockerfile inline (§5.1). |
| `auto_inject_build_args` | `boolean` | non | `true` | — | non | Build args auto-injectés, désactivable (§5.2). |
| `inject_source_commit` | `boolean` | non | `false` | — | non | `SOURCE_COMMIT` opt-in (§5.2). |
| `compose_file_path` | `text` | oui | — | — | non | Chemin du fichier compose dans le repo (§5.2). |
| `raw_compose` | `boolean` | non | `false` | — | non | Mode « raw compose » avancé (§5.2). |
| `image_name` | `text` | oui | — | — | non | Source « Docker Image » : image pré-construite (§5.1) ; validée (§23.3). |
| `image_tag` | `text` | oui | — | — | non | — |
| `registry_credential_id` | `bigint` | oui | — | FK `registry_credentials(id)` ON DELETE RESTRICT | non | Pull depuis un registry privé. |
| `push_enabled` | `boolean` | non | `false` | — | non | Push post-build (requis build servers, §3.4/§5.2). |
| `push_image_name` | `text` | oui | — | — | non | Image cible du push. |
| `push_image_tag` | `text` | oui | — | — | non | Tag custom (§5.2). |
| `push_tag_with_commit_sha` | `boolean` | non | `false` | — | non | Tag SHA du commit (§5.2). |
| `push_registry_credential_id` | `bigint` | oui | — | FK `registry_credentials(id)` ON DELETE RESTRICT | non | — |
| `use_build_server` | `boolean` | non | `false` | — | non | « Use a Build Server? » (§3.4). |
| `use_build_secrets` | `boolean` | non | `false` | — | non | Docker Build Secrets BuildKit `--secret` (§5.4). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 8.3 `runtime_configs`

Configuration d'exécution d'une application (§5.3), 1—1. Suppression : **CASCADE** avec l'application.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `application_id` | `bigint` | non | — | UNIQUE, FK `applications(id)` ON DELETE CASCADE | non | — |
| `ports_exposes` | `text` | oui | — | — | non | Port(s) interne(s) pour le proxy ; optionnel sans trafic entrant (§5.3). |
| `ports_mappings` | `jsonb` | oui | — | — | non | Mappings hôte `[{host_ip, host_port, container_port, protocol}]`, TCP/UDP/SCTP (§5.3) ; désactive le rolling update (§15). |
| `custom_docker_options` | `text` | oui | — | — | non | Options `docker run` validées (`--cap-add`, `--gpus`… — §5.3, §23.3, INV-012). |
| `custom_labels` | `text` | oui | — | — | non | Labels containers éditables ; labels système `AkerDock.*` injectés en sus (§5.3). |
| `pre_deployment_command` | `text` | oui | — | — | non | Exécutée dans le container existant avant déploiement (§5.3). |
| `post_deployment_command` | `text` | oui | — | — | non | Exécutée dans le nouveau container ; échec = déploiement échoué (§5.3). |
| `stop_grace_period_seconds` | `integer` | non | `10` | CHECK `>= 0` | non | Délai de grâce d'arrêt (§5.3). |
| `restart_limit` | `integer` | oui | — | CHECK `> 0` | non | Plafond de boucles de redémarrage (§5.3). |
| `memory_limit` | `text` | oui | — | — | non | Ex. `512m` (§5.3) ; appliqué aussi aux stacks compose (décision §27.15). |
| `memory_reservation` | `text` | oui | — | — | non | — |
| `memory_swap` | `text` | oui | — | — | non | — |
| `memory_swappiness` | `integer` | oui | — | CHECK `0..100` | non | — |
| `cpu_limit` | `numeric(6,2)` | oui | — | CHECK `> 0` | non | — |
| `cpu_sets` | `text` | oui | — | — | non | Ex. `0-2`. |
| `cpu_shares` | `integer` | oui | — | CHECK `> 0` | non | — |
| `force_https` | `boolean` | non | `true` | — | non | Redirection HTTPS par application (§4.3). |
| `redirect_direction` | `redirect_direction` | non | `'both'` | — | non | www / non-www (§4.2). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 8.4 `domains`

Domaine routé par le proxy : FQDN, port interne cible et path (§4.2). Attaché à une application **ou** à un service component (domaine par sous-service, §9). Suppression : **CASCADE** avec son propriétaire (le retrait de routage précède la suppression du workload, §20.6).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `application_id` | `bigint` | oui | — | FK `applications(id)` ON DELETE CASCADE ; CHECK exactement un propriétaire non NULL | non | — |
| `service_component_id` | `bigint` | oui | — | FK `service_components(id)` ON DELETE CASCADE | non | — |
| `fqdn` | `citext` | non | — | UNIQUE `(fqdn, path)` ; CHECK format domaine (§23.3) | non | Sans schéma ; multi-domaines = plusieurs lignes (§4.2). |
| `path` | `text` | non | `'/'` | (cf. `fqdn`) | non | Path-based routing, priorité au plus spécifique (§4.2). |
| `target_port` | `integer` | oui | — | CHECK `1..65535` | non | Syntaxe `domaine:port` → port interne précis (§4.2) ; NULL = port exposé par défaut. |
| `is_generated` | `boolean` | non | `false` | — | non | Issu du wildcard serveur `<uuid>.example.com` / sslip.io (§4.2). |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

> La contrainte `UNIQUE (fqdn, path)` est **globale à l'instance** : plus stricte que le strict nécessaire (deux serveurs pourraient servir le même FQDN), mais elle élimine toute ambiguïté de routage et les collisions inter-team sur un même serveur (INV-002). Le certificat, l'émission ACME et son état vivent dans les révisions proxy (`proxy_config_revisions`, §11.1) et sur le serveur, pas en base.

### 8.5 `environment_variables`

Variable d'environnement d'une ressource (application, database, service — §5.4), y compris le jeu séparé preview (§5.6) et les magic variables générées (§5.4, §9). Toutes les **valeurs sont chiffrées** enveloppe, `is_secret` ne pilotant que le masquage/`read:sensitive` (INV-003) — évite toute erreur de classification. Suppression : **CASCADE** avec la ressource.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `resource_id` | `bigint` | non | — | FK `resources(id)` ON DELETE CASCADE ; UNIQUE `(resource_id, key, is_preview)` | non | Ressource propriétaire. |
| `key` | `text` | non | — | CHECK format `[A-Za-z_][A-Za-z0-9_]*` | non | Nom de la variable. |
| `value_enc` | `bytea` | non | — | — | **oui** | Valeur chiffrée enveloppe (secrète ou non). |
| `is_secret` | `boolean` | non | `false` | — | non | Masquée en UI/API sans `read:sensitive` (INV-003). |
| `is_build_time` | `boolean` | non | `false` | — | non | Disponible au build (`ARG`/BuildKit), stockée hors image (§5.4). |
| `is_literal` | `boolean` | non | `false` | — | non | Pas d'interpolation (§5.4). |
| `is_multiline` | `boolean` | non | `false` | — | non | Clés, certificats (§5.4). |
| `is_locked` | `boolean` | non | `false` | — | non | Masquée et non rééditable (§5.4). |
| `is_preview` | `boolean` | non | `false` | — | non | Jeu dédié aux previews — jamais de fuite des secrets de production (§5.6, INV-010). |
| `is_generated` | `boolean` | non | `false` | — | non | Magic variable `SERVICE_<TYPE>_<ID>` : générée, persistante entre redéploiements, éditable (§5.4). |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 8.6 `shared_variables`

Variable partagée hiérarchique `{{team.VAR}}` / `{{project.VAR}}` / `{{environment.VAR}}` et variables de serveur héritées (§5.4, §3.1). `team_id` toujours renseigné (INV-001), le scope précisant le niveau. Suppression : **CASCADE** avec son scope.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE CASCADE, index | non | Toujours présent, quel que soit le scope. |
| `scope` | `shared_variable_scope` | non | — | CHECK : `project` ⇒ `project_id` non NULL ; `environment` ⇒ `environment_id` non NULL ; `server` ⇒ `server_id` non NULL ; `team` ⇒ tous NULL | non | Niveau d'héritage. |
| `project_id` | `bigint` | oui | — | FK `projects(id)` ON DELETE CASCADE | non | — |
| `environment_id` | `bigint` | oui | — | FK `environments(id)` ON DELETE CASCADE | non | — |
| `server_id` | `bigint` | oui | — | FK `servers(id)` ON DELETE CASCADE | non | Variables partagées au niveau serveur (§3.1). |
| `key` | `text` | non | — | UNIQUE partiel par scope : `(team_id, key) WHERE scope='team'`, `(project_id, key) WHERE scope='project'`, `(environment_id, key) WHERE scope='environment'`, `(server_id, key) WHERE scope='server'` ; CHECK format | non | — |
| `value_enc` | `bytea` | non | — | — | **oui** | Valeur chiffrée enveloppe. |
| `is_secret` | `boolean` | non | `false` | — | non | Masquage (INV-003). |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 8.7 `persistent_storages`

Stockage persistant d'une ressource : volume nommé, bind mount ou file mount à contenu éditable (§8). Suppression : la ligne suit la ressource (**CASCADE**) ; les **données distantes** suivent le choix explicite « conserver / supprimer » du workflow §20.6 (INV-006, INV-008).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `resource_id` | `bigint` | non | — | FK `resources(id)` ON DELETE CASCADE ; UNIQUE `(resource_id, mount_path)` | non | — |
| `kind` | `storage_kind` | non | — | CHECK cohérence : `volume` ⇒ `name` non NULL ; `bind`/`file` ⇒ `host_path` non NULL | non | volume / bind / file (§8). |
| `name` | `text` | oui | — | — | non | Nom du volume, préfixé par l'UUID de la ressource (anti-collision, §8, INV-011). |
| `external_name` | `text` | oui | — | — | non | Volume **adopté** (§20.7) : nom Docker d'origine, monté tel quel — le préfixer remonterait un volume vide (INV-008). Jamais monté dans une preview (INV-010). |
| `host_path` | `text` | oui | — | CHECK anti path traversal (§23.3) | non | Chemin hôte (bind/file). |
| `mount_path` | `text` | non | — | — | non | Chemin dans le container. |
| `content` | `text` | oui | — | CHECK `length(content) <= 5*1024*1024` | non | Contenu du file mount, éditable en UI (≤ 5 MiB, §23.3) ; rechargement depuis le serveur (§8). |
| `is_directory` | `boolean` | non | `false` | — | non | Extension compose `is_directory: true` (§8) ; conversion fichier ↔ répertoire. |
| `file_mode` | `text` | oui | — | CHECK format octal | non | chmod (§8). |
| `owner_uid` | `integer` | oui | — | — | non | chown (§8). |
| `group_gid` | `integer` | oui | — | — | non | chown (§8). |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 8.8 `health_checks`

Health check applicatif (§5.3) : conditionne le routage et le rolling update (INV-005) ; le `HEALTHCHECK` Dockerfile reste prioritaire. Une ligne par ressource (applications et bases, §6.2). Suppression : **CASCADE** avec la ressource.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `resource_id` | `bigint` | non | — | UNIQUE, FK `resources(id)` ON DELETE CASCADE | non | — |
| `enabled` | `boolean` | non | `false` | — | non | — |
| `method` | `text` | non | `'GET'` | — | non | Méthode HTTP (§5.3). |
| `path` | `text` | non | `'/'` | — | non | — |
| `port` | `integer` | oui | — | CHECK `1..65535` | non | NULL = port exposé. |
| `interval_seconds` | `integer` | non | `30` | CHECK `> 0` | non | — |
| `timeout_seconds` | `integer` | non | `5` | CHECK `> 0` | non | — |
| `retries` | `integer` | non | `3` | CHECK `> 0` | non | — |
| `start_period_seconds` | `integer` | non | `5` | CHECK `>= 0` | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 8.9 `previews`

Environnement éphémère de PR/MR (§5.6, §20.4). Identité déterministe `(application, provider, pr_id)`, jamais recyclée pour une autre application. Suppression : cycle `destroying → destroyed` (ou `cleanup_failed` + retry, §20.4) ; purge des lignes `destroyed` selon rétention.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | Base des noms Docker de l'instance de preview (INV-011). |
| `application_id` | `bigint` | non | — | FK `applications(id)` ON DELETE CASCADE ; UNIQUE `(application_id, provider, pr_id)` | non | Identité déterministe (§20.4). |
| `provider` | `git_provider` | non | — | (cf. ci-dessus) | non | GitHub PR / GitLab MR / Gitea (§20.4.6). |
| `pr_id` | `integer` | non | — | (cf. ci-dessus) | non | Numéro de PR/MR (`AKERDOCK_PR_ID`, §27.22). |
| `source_branch` | `text` | oui | — | — | non | Branche source de la PR. |
| `head_sha` | `text` | oui | — | — | non | Dernier SHA déployé (redeploy à chaque commit, §5.6). |
| `is_fork` | `boolean` | non | `false` | — | non | PR issue d'un fork (INV-010). |
| `repo_reference` | `text` | oui | — | — | non | Référence du dépôt chez le provider pour le feedback (§20.4.6) : project id GitLab, `owner/repo` Gitea/GitHub. Capturée de la livraison authentifiée ; NULL sur le chemin GitHub App (cache `repositories`). |
| `fork_approved_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | Approbation mainteneur (§20.4.8) ; NULL = non approuvée. |
| `fork_approved_at` | `timestamptz` | oui | — | — | non | — |
| `fqdn` | `citext` | oui | — | — | non | URL de la preview (template `{{pr_id}}.{{domain}}`, §5.6). |
| `status` | `preview_status` | non | `'queued'` | index partiel `WHERE status NOT IN ('destroyed')` | non | Inclut `cleanup_failed` : notifié et réessayé (§20.4). |
| `cleanup_error` | `text` | oui | — | — | non | Dernière erreur de cleanup. |
| `last_deployed_at` | `timestamptz` | oui | — | — | non | — |
| `last_activity_at` | `timestamptz` | oui | — | index | non | Dernière requête reçue : TTL d'inactivité et scale-to-zero (§20.4.3). |
| `destroyed_at` | `timestamptz` | oui | — | — | non | Fermeture/merge de la PR ou TTL (§5.6). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

> Les variables du jeu preview sont dans `environment_variables.is_preview` (partagées par toutes les previews d'une application, parité §5.6) ; les commentaires/checks Git (§20.4.6 : commentaire unique mis à jour en place) sont des identifiants externes conservés dans le `payload` des jobs/événements, pas des colonnes dédiées.

---

## 9. Agrégat Service / Database

### 9.1 `services`

Extension 1—1 de `resources` (`resource_type = 'service'`) : stack Docker Compose one-click ou utilisateur (§9). Le fichier compose est la source de vérité (§5.2). Suppression : **CASCADE** technique avec `resources` (workflow §20.6).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | — | PK, FK `resources(id)` ON DELETE CASCADE | non | Héritage par classe. |
| `compose_content` | `text` | non | — | — | non | Fichier compose éditable en UI (§9) ; extensions `is_directory`, `content`, `exclude_from_hc` (§5.2). |
| `template_slug` | `text` | oui | — | — | non | Slug du template one-click d'origine (§9) ; NULL = compose libre. |
| `template_version` | `text` | oui | — | — | non | Version du catalogue à l'instanciation (catalogue versionné/signé, §27.10). |
| `template_repository` | `text` | oui | — | — | non | Dépôt de templates d'origine (officiel ou dépôt de team, §27.10). |
| `connect_to_predefined_network` | `boolean` | non | `false` | — | non | Communication inter-stacks (§9). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 9.2 `service_components`

Sous-container d'un stack (un service du compose) : statut, image, domaine, logs et restart individuels (§9). Recréés par synchronisation à chaque édition du compose. Suppression : **CASCADE** avec la ressource.

> **Amendement (migration 00038)** : la FK porte sur `resources(id)` et non `services(id)` — une **application en build pack compose** (§5.2 PRD, « domaine par service ») porte aussi des composants, et les deux extensions partagent l'identité `resources`. Pour un stack `service`, la valeur est identique (`services.id = resources.id`, héritage par classe).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `resource_id` | `bigint` | non | — | FK `resources(id)` ON DELETE CASCADE ; UNIQUE `(resource_id, name)` | non | Stack `service` ou application en build pack compose. |
| `name` | `text` | non | — | (cf. ci-dessus) | non | Nom du service dans le compose. |
| `image` | `text` | oui | — | — | non | Image résolue (informatif). |
| `is_database` | `boolean` | non | `false` | — | non | Détection par image postgres/mysql/mariadb/mongo → backupable (§7.1). |
| `database_engine` | `db_engine` | oui | — | — | non | Moteur détecté si `is_database`. |
| `exclude_from_hc` | `boolean` | non | `false` | — | non | Jobs one-shot exclus du health check du stack (§9). |
| `default_route_port` | `integer` | oui | — | CHECK 1–65535 | non | Port de routage par défaut (compose-spec §6) : premier `expose`, résolu à la validation. |
| `observed_status` | `resource_observed_status` | non | `'unknown'` | — | non | Statut par sous-container (§5.7, §9). |
| `observed_at` | `timestamptz` | oui | — | — | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 9.3 `databases`

Extension 1—1 de `resources` (`resource_type = 'database'`) : base managée one-click (§6). `server_id` est dénormalisé depuis la destination (trigger) pour porter la contrainte d'unicité du port public par serveur (forte cohérence exigée sur la réservation de port, §22.3). Suppression : **CASCADE** technique avec `resources` (workflow §20.6 — question distincte sur les volumes de données).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | — | PK, FK `resources(id)` ON DELETE CASCADE | non | Héritage par classe. |
| `engine` | `db_engine` | non | — | — | non | PostgreSQL, MySQL, MariaDB, MongoDB, Redis, KeyDB, Dragonfly, ClickHouse (§6.1). |
| `image` | `text` | oui | — | — | non | Image/tag libre (§6.2) ; NULL = image par défaut du moteur. |
| `image_tag` | `text` | oui | — | — | non | — |
| `custom_config` | `text` | oui | — | — | non | `postgres_conf` / `mysql_conf` / `redis_conf`… ; refusé pour Dragonfly/ClickHouse (§6.2). |
| `initdb_args` | `text` | oui | — | — | non | PostgreSQL : arguments `initdb` (§6.2) ; init scripts via file mounts (§8). |
| `server_id` | `bigint` | non | — | FK `servers(id)` ON DELETE RESTRICT ; UNIQUE partiel `(server_id, public_port) WHERE is_public` | non | Dénormalisé depuis `resources.destination_id` (trigger). |
| `is_public` | `boolean` | non | `false` | — | non | Accès public activé (§6.2). |
| `public_access_mode` | `public_access_mode` | oui | — | CHECK : `is_public` ⇒ non NULL | non | Port mapping Docker (restart requis) ou proxy TCP Nginx dynamique (§6.2). |
| `public_port` | `integer` | oui | — | CHECK `1..65535` ; `is_public` ⇒ non NULL | non | Port public, modifiable sans redémarrage en mode `tcp_proxy` (§6.2). |
| `tcp_proxy_timeout_seconds` | `integer` | non | `3600` | CHECK `> 0` | non | Timeout du proxy TCP (§6.2). |
| `ssl_enabled` | `boolean` | non | `false` | — | non | « Enable SSL » (§6.3) ; non supporté ClickHouse (validation applicative). |
| `ssl_mode` | `text` | oui | — | CHECK `IN ('allow','prefer','require','verify-ca','verify-full','on','off')` | non | Modes par moteur (§6.3) ; validation croisée moteur ↔ mode en applicatif. |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 9.4 `database_credentials`

Credentials générés (mots de passe 64 caractères) ou fournis, par base (§6.2) ; champs adaptés par moteur (utilisateur applicatif, superutilisateur…). Suppression : **CASCADE** avec la base.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `database_id` | `bigint` | non | — | FK `databases(id)` ON DELETE CASCADE ; UNIQUE `(database_id, username)` | non | — |
| `username` | `text` | non | — | (cf. ci-dessus) | non | Vide autorisé pour Redis (`default`). |
| `password_enc` | `bytea` | non | — | — | **oui** | Mot de passe, chiffré enveloppe ; entre dans les URLs interne/externe reconstruites à la volée (§6.2), jamais stockées assemblées. |
| `database_name` | `text` | oui | — | — | non | Base initiale (moteurs SQL/Mongo). |
| `is_admin` | `boolean` | non | `false` | — | non | Compte superutilisateur (ex. `root` MySQL) vs compte applicatif. |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 9.5 `database_backup_plans`

Plan de backup planifié (§7.1–7.2) : cible une base managée, une base interne de service (détectée par image) ou la base de l'instance elle-même. Suppression : soft delete (`deleted_at`) ; l'historique d'exécutions suit sa propre rétention — le dernier backup valide n'est jamais supprimé par la rétention (§20.5).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `database_id` | `bigint` | oui | — | FK `databases(id)` ON DELETE CASCADE ; CHECK exactement une cible : `database_id` ⊕ `service_component_id` ⊕ `is_instance_backup` | non | Base managée. |
| `service_component_id` | `bigint` | oui | — | FK `service_components(id)` ON DELETE CASCADE | non | Base interne d'un stack compose (§7.1, compose-spec §10) — PostgreSQL seul en v1, refusé en `422` sinon. |
| `is_instance_backup` | `boolean` | non | `false` | — | non | Backup de la base PostgreSQL de AkerDock lui-même (§7.1, §7.5). |
| `enabled` | `boolean` | non | `true` | — | non | — |
| `cron_expression` | `text` | non | — | CHECK validation cron (§23.3) | non | Expression cron ou alias `daily`/`hourly`/… (§7.1). |
| `timezone` | `text` | non | `'UTC'` | — | non | Cron interprété dans un fuseau explicite (§24.3). |
| `dump_all` | `boolean` | non | `false` | — | non | « Dump all databases » (§7.1). |
| `included_databases` | `text[]` | oui | — | — | non | Sélection de bases (§7.1). |
| `excluded_collections` | `text[]` | oui | — | — | non | MongoDB : collections exclues (§7.1). |
| `timeout_seconds` | `integer` | non | `3600` | CHECK `> 0` | non | §7.1. |
| `s3_storage_id` | `bigint` | oui | — | FK `s3_storages(id)` ON DELETE RESTRICT | non | Destination S3 (même team, INV-002) ; NULL = local uniquement. |
| `s3_only` | `boolean` | non | `false` | — | non | Suppression du fichier local après upload (§7.2). |
| `retention_local_max_count` | `integer` | non | `0` | CHECK `>= 0` | non | 0 = illimité (§7.2). |
| `retention_local_max_days` | `integer` | non | `0` | CHECK `>= 0` | non | — |
| `retention_local_max_size_gb` | `numeric(10,2)` | non | `0` | CHECK `>= 0` | non | — |
| `retention_s3_max_count` | `integer` | non | `0` | CHECK `>= 0` | non | Rétention S3 séparée (§7.2). |
| `retention_s3_max_days` | `integer` | non | `0` | CHECK `>= 0` | non | — |
| `retention_s3_max_size_gb` | `numeric(10,2)` | non | `0` | CHECK `>= 0` | non | — |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `deleted_at` | `timestamptz` | oui | — | — | non | Soft delete. |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

> Les backups de **volumes applicatifs** (décision §27.14) réutiliseront ce plan via une table d'extension dédiée (cible = ressource + liste de volumes, outil restic) ; hors périmètre du présent dictionnaire car absents du §19.1.

### 9.6 `backup_executions`

Trace de chaque exécution de backup (§7.3, §20.5) : statut, fichier, taille, checksum, upload S3. `partial` = succès local mais échec S3 (statut explicite, §20.5). Suppression : **purge** par la rétention du plan, sans jamais supprimer le dernier backup valide.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `backup_plan_id` | `bigint` | non | — | FK `database_backup_plans(id)` ON DELETE CASCADE, index `(backup_plan_id, created_at DESC)` | non | — |
| `job_id` | `bigint` | oui | — | FK `jobs(id)` ON DELETE SET NULL | non | Job d'exécution (lease/heartbeat, §21.3). |
| `status` | `backup_execution_status` | non | `'running'` | — | non | `running` / `succeeded` / `partial` / `failed`. |
| `filename` | `text` | oui | — | — | non | Chemin local `/var/lib/akerdock/backups/...` (§7.2). |
| `size_bytes` | `bigint` | oui | — | — | non | — |
| `checksum_sha256` | `text` | oui | — | — | non | Intégrité, vérifiée au restore et lors des drills (§20.5). |
| `engine_version` | `text` | oui | — | — | non | Version du moteur au moment du dump (§20.5). |
| `uploaded_to_s3` | `boolean` | non | `false` | — | non | Objet distant vérifié après upload (§20.5). |
| `s3_upload_error` | `text` | oui | — | — | non | Détail d'un statut `partial`. |
| `local_deleted_at` | `timestamptz` | oui | — | — | non | Fichier local supprimé (rétention ou `s3_only`). |
| `error_message` | `text` | oui | — | — | non | Erreur générique (jamais de secret, INV-003). |
| `started_at` | `timestamptz` | oui | — | — | non | — |
| `finished_at` | `timestamptz` | oui | — | — | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

---

## 10. Agrégat Exécution

### 10.1 `deployments`

Exécution du pipeline de déploiement d'une ressource — machine à états §21.1. Chaque déploiement conserve SHA immuable, digest OCI et snapshot de configuration (§18.3, INV-014). Suppression : **purge** par rétention (100 000 en historique, §22.2) ; **CASCADE** technique si le tombstone de la ressource est purgé.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | `job_uuid` renvoyé par les actions `202` (§24.1). |
| `resource_id` | `bigint` | non | — | FK `resources(id)` ON DELETE CASCADE, index `(resource_id, id DESC)` | non | Application ou service déployé. |
| `preview_id` | `bigint` | oui | — | FK `previews(id)` ON DELETE SET NULL | non | Déploiement de preview (§20.4). |
| `status` | `deployment_status` | non | `'queued'` | index partiel `(server_id, created_at) WHERE status NOT IN ('succeeded','failed','cancelled')` | non | Machine à états §21.1 ; `switching` sous verrou exclusif application/destination. |
| `attempt` | `integer` | non | `1` | — | non | Incrément explicite au retry, historique préservé (§21.1). |
| `retry_of_id` | `bigint` | oui | — | FK `deployments(id)` ON DELETE SET NULL | non | Tentative liée (§21.1). |
| `superseded_by_id` | `bigint` | oui | — | FK `deployments(id)` ON DELETE SET NULL | non | Déploiement plus récent ayant remplacé celui-ci en file (coalescing §20.3.5) ; renseigné quand `status = 'superseded'`. |
| `is_rollback` | `boolean` | non | `false` | — | non | Rollback : redéploiement d'un artifact vérifié sans rebuild (§27.6). |
| `trigger` | `deployment_trigger` | non | — | — | non | manual / webhook / api / preview / schedule / config_apply / cli_local. |
| `triggered_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | Acteur humain. |
| `api_token_id` | `bigint` | oui | — | FK `api_tokens(id)` ON DELETE SET NULL | non | Déclenchement par token (`deploy`, §10.3). |
| `webhook_delivery_id` | `bigint` | oui | — | FK `webhook_deliveries(id)` ON DELETE SET NULL | non | Référence à la livraison d'origine (§20.3.6). |
| `git_branch` | `text` | oui | — | — | non | Branche résolue. |
| `commit_sha` | `text` | oui | — | — | non | SHA immuable résolu avant build (§18.3, §20.2) ; NULL si source locale ou image. |
| `is_local_source` | `boolean` | non | `false` | — | non | `akerdock up` : source poussée depuis le poste (§27.18) ; n'active jamais l'auto-deploy. |
| `context_digest` | `text` | oui | — | — | non | Digest du contexte local à la place du SHA (§27.18). |
| `force_rebuild` | `boolean` | non | `false` | — | non | Build sans cache (§5.5). |
| `image_name` | `text` | oui | — | — | non | Image produite/déployée. |
| `image_tag` | `text` | oui | — | — | non | — |
| `image_digest` | `text` | oui | — | — | non | Digest OCI résolu avant bascule (§18.3, §27.6). |
| `config_snapshot` | `jsonb` | oui | — | — | non | Snapshot versionné de la configuration, secrets **référencés** (nom + version), jamais en clair (INV-003, INV-014). |
| `config_diff` | `jsonb` | oui | — | — | non | Diff de configuration présenté avec le redéploiement (§5.5), redacted. |
| `error_message` | `text` | oui | — | — | non | Résumé d'échec exposé par le schéma OpenAPI `Deployment` — sans secret ni commande sensible (§24.1). |
| `server_id` | `bigint` | non | — | FK `servers(id)` ON DELETE RESTRICT | non | Serveur cible (dénormalisé pour la file par serveur, §5.5). |
| `build_server_id` | `bigint` | oui | — | FK `servers(id)` ON DELETE SET NULL | non | Build server utilisé (sélection aléatoire, §3.4). |
| `queued_at` | `timestamptz` | non | `now()` | — | non | — |
| `started_at` | `timestamptz` | oui | — | — | non | — |
| `finished_at` | `timestamptz` | oui | — | — | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 10.2 `deployment_steps`

Étape du pipeline (§20.2) : timeline UI, logs de build structurés, exit codes. Suppression : **CASCADE** avec le déploiement.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `deployment_id` | `bigint` | non | — | FK `deployments(id)` ON DELETE CASCADE ; UNIQUE `(deployment_id, seq)` | non | — |
| `seq` | `integer` | non | — | (cf. ci-dessus) | non | Ordre d'exécution. |
| `name` | `text` | non | — | — | non | Ex. `clone`, `build`, `push`, `healthcheck`, `switch`. |
| `status` | `deployment_step_status` | non | `'pending'` | — | non | — |
| `exit_code` | `integer` | oui | — | — | non | Code de sortie de la commande distante. |
| `log` | `text` | oui | — | — | non | Logs de l'étape, ANSI/HTML neutralisés (§23.3), secrets redacted (INV-003), tronqués/compressés au-delà d'un seuil ; le streaming realtime passe par SSE, pas par cette colonne (§24.4). |
| `started_at` | `timestamptz` | oui | — | — | non | — |
| `finished_at` | `timestamptz` | oui | — | — | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

### 10.3 `deployment_artifacts`

Artifact produit par un déploiement : image locale ou poussée en registry, candidate au rollback (§5.5, §27.6). Protégée du cleanup automatique (INV-015). Suppression : **CASCADE** avec le déploiement ; le nettoyage des images distantes est un job explicite.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `deployment_id` | `bigint` | non | — | FK `deployments(id)` ON DELETE CASCADE, index | non | — |
| `kind` | `artifact_kind` | non | — | — | non | `local_image` (rétention des N dernières) ou `registry_image` (digest immuable, §27.6). |
| `image_name` | `text` | non | — | — | non | — |
| `image_tag` | `text` | oui | — | — | non | — |
| `image_digest` | `text` | oui | — | — | non | Digest OCI ; requis pour `registry_image` (rollback reproductible). |
| `server_id` | `bigint` | oui | — | FK `servers(id)` ON DELETE CASCADE | non | Serveur où réside l'image locale. |
| `registry_credential_id` | `bigint` | oui | — | FK `registry_credentials(id)` ON DELETE SET NULL | non | Registry de conservation. |
| `protected_from_cleanup` | `boolean` | non | `true` | — | non | Jamais purgée par l'Automated Cleanup (INV-015, §27.6). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

### 10.4 `scheduled_tasks`

Cron par application/service (§5.7) : commande exécutée par `docker exec` dans le container cible. Suppression : **CASCADE** avec la ressource (historique purgé par rétention).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `resource_id` | `bigint` | non | — | FK `resources(id)` ON DELETE CASCADE ; UNIQUE `(resource_id, name)` | non | — |
| `name` | `text` | non | — | (cf. ci-dessus) | non | — |
| `command` | `text` | non | — | — | non | Commande passée en arguments typés/échappés (INV-012). |
| `cron_expression` | `text` | non | — | CHECK validation cron | non | Expression ou alias `daily`/`hourly`/… (§5.7). |
| `timezone` | `text` | non | `'UTC'` | — | non | Fuseau explicite, prochaine exécution prévisualisée (§24.3). |
| `container_name` | `text` | oui | — | — | non | Container cible dans un stack (§5.7). |
| `enabled` | `boolean` | non | `true` | — | non | — |
| `overlap_policy` | `overlap_policy` | non | `'forbid'` | — | non | §24.3. |
| `missed_run_policy` | `missed_run_policy` | non | `'skip'` | — | non | Jamais de rafale illimitée (§24.3). |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 10.5 `task_executions`

Historique des exécutions de tâches planifiées (§5.7), avec notifications succès/échec (§11). Suppression : **purge** par rétention.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `scheduled_task_id` | `bigint` | non | — | FK `scheduled_tasks(id)` ON DELETE CASCADE, index `(scheduled_task_id, created_at DESC)` | non | — |
| `job_id` | `bigint` | oui | — | FK `jobs(id)` ON DELETE SET NULL | non | — |
| `status` | `task_execution_status` | non | `'running'` | — | non | `skipped` = politique de chevauchement/missed run (§24.3). |
| `exit_code` | `integer` | oui | — | — | non | — |
| `output` | `text` | oui | — | — | non | Sortie tronquée, neutralisée (§23.3). |
| `started_at` | `timestamptz` | oui | — | — | non | — |
| `finished_at` | `timestamptz` | oui | — | — | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

### 10.6 `terminal_sessions`

Session terminal web (xterm.js → WebSocket → SSH/PTY, §5.7, §24.4). Ouverture et fermeture auditées ; les frappes ne sont **pas** enregistrées par défaut (§24.4). L'attache WebSocket se fait par un **token court à usage unique** (§24.4) dont seul le hash est stocké (§23.2) — la ligne est créée à l'émission du token, la session démarre à la consommation. Suppression : **purge** par rétention ; les cibles supprimées passent en `SET NULL`, le libellé est conservé en snapshot.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE CASCADE, index | non | Session bornée à la team active (§10.4). |
| `user_id` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `target_kind` | `terminal_target` | non | — | — | non | Serveur ou container (§5.7). |
| `server_id` | `bigint` | oui | — | FK `servers(id)` ON DELETE SET NULL | non | — |
| `resource_id` | `bigint` | oui | — | FK `resources(id)` ON DELETE SET NULL | non | Ressource du container ciblé. |
| `target_name` | `text` | non | — | — | non | Snapshot du nom de la cible (survit aux suppressions). |
| `client_ip` | `inet` | oui | — | — | non | — |
| `token_hash` | `text` | non | — | UNIQUE | non (hash SHA-256) | Hash du token d'attache WebSocket ; le token clair n'est jamais stocké (§23.2). |
| `token_expires_at` | `timestamptz` | non | — | — | non | Expiration du token d'attache (courte, §24.4). |
| `claimed_at` | `timestamptz` | oui | — | — | non | Consommation du token par l'upgrade WebSocket — usage unique (§24.4). |
| `started_at` | `timestamptz` | non | `now()` | — | non | — |
| `ended_at` | `timestamptz` | oui | — | — | non | Kill garanti à la déconnexion/expiration (§24.4). |
| `end_reason` | `terminal_end_reason` | oui | — | — | non | user_close / idle_timeout / max_duration / disconnect / revoked. |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

---

### 10.7 `port_forward_sessions`

Session de tunnel TCP du CLI (ADR-032) : WebSocket multiplexée → canal SSH `direct-tcpip`
vers un container. Même contrat que `terminal_sessions` — token d'attache à usage unique
hashé (§23.2), ouverture/fermeture auditées, purge par rétention, cibles en `SET NULL` avec
libellé snapshot. La cible (container, port) est **figée à la création**.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE CASCADE, index | non | Bornée à la team active. |
| `user_id` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `server_id` | `bigint` | oui | — | FK `servers(id)` ON DELETE SET NULL | non | Serveur atteint par SSH. |
| `resource_id` | `bigint` | oui | — | FK `resources(id)` ON DELETE SET NULL | non | Ressource ciblée. |
| `target_name` | `text` | non | — | — | non | Snapshot `<container>:<port>` (survit aux suppressions). |
| `target_port` | `integer` | non | — | CHECK 1–65535 | non | Port interne figé à la création. |
| `client_ip` | `inet` | oui | — | — | non | — |
| `token_hash` | `text` | non | — | UNIQUE | non (hash SHA-256) | Hash du token d'attache ; le token clair n'est jamais stocké (§23.2). |
| `token_expires_at` | `timestamptz` | non | — | — | non | Expiration du token d'attache (courte). |
| `claimed_at` | `timestamptz` | oui | — | — | non | Consommation par l'upgrade WebSocket — usage unique. |
| `started_at` | `timestamptz` | non | `now()` | — | non | — |
| `ended_at` | `timestamptz` | oui | — | — | non | Teardown garanti à la déconnexion/expiration. |
| `end_reason` | `terminal_end_reason` | oui | — | — | non | Réutilise l'enum : user_close / idle_timeout / max_duration / disconnect / revoked. |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

---

### 10.8 `cli_authorization_codes`

Demandes d'authentification du CLI en cours (ADR-031, flux poll+code+PKCE). Éphémères
(TTL 10 min), purgées après consommation ou expiration. Seuls des **hash** sont stockés ;
ni le `verifier`, ni le token frappé n'y figurent.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `request_id_hash` | `text` | non | — | UNIQUE | non (hash SHA-256) | Hash de l'identifiant de demande porté par la CLI. |
| `challenge` | `text` | non | — | — | non | `base64url(SHA-256(verifier))` (PKCE) — public par conception. |
| `user_code` | `text` | non | — | — | non | Code court confronté par l'utilisateur (anti-phishing). |
| `status` | `text` | non | `'pending'` | — | non | pending / approved / consumed. |
| `user_id` | `bigint` | oui | — | FK `users(id)` ON DELETE CASCADE | non | Rempli à l'approbation. |
| `team_id` | `bigint` | oui | — | FK `teams(id)` ON DELETE CASCADE | non | Team choisie à l'approbation. |
| `permissions` | `text[]` | oui | — | — | non | Permissions approuvées (⊆ session). |
| `client_name` | `text` | oui | — | — | non | `<user>@<host>` fourni par la CLI (rendu inerte à l'affichage). |
| `client_ip` | `inet` | oui | — | — | non | — |
| `expires_at` | `timestamptz` | non | — | — | non | TTL court (défaut 10 min). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

---

## 11. Agrégat Plateforme et tables techniques

### 11.1 `proxy_config_revisions`

Révision de configuration proxy générée pour un serveur : génération déterministe, validation, application atomique, rollback (§18.1) ; réconciliation par checksum (§18.3). Suppression : **purge** en conservant les N dernières révisions par serveur.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `server_id` | `bigint` | non | — | FK `servers(id)` ON DELETE CASCADE ; UNIQUE `(server_id, revision)` | non | — |
| `revision` | `integer` | non | — | (cf. ci-dessus) | non | Numéro croissant par serveur. |
| `proxy_type` | `proxy_type` | non | — | — | non | Représentation générée pour Traefik ou Caddy depuis l'IR commune (§27.9). |
| `checksum_sha256` | `text` | non | — | — | non | Comparé au fichier distant pour détecter la dérive (§18.3). |
| `content` | `text` | non | — | — | non | Configuration générée (labels + dynamic config) ; ne contient aucun secret — clés privées TLS uniquement sur le serveur (§4.3). |
| `status` | `proxy_revision_status` | non | `'generated'` | — | non | generated → applied / failed / rolled_back. |
| `error` | `text` | oui | — | — | non | Cause d'échec d'application. |
| `applied_at` | `timestamptz` | oui | — | — | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

### 11.2 `notification_channels`

Canal de notification d'une team (§11). La configuration (URLs de webhook, tokens de bot, mot de passe SMTP…) est chiffrée en bloc. Suppression : **CASCADE** avec la team ; libre sinon (les règles suivent).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `team_id` | `bigint` | non | — | FK `teams(id)` ON DELETE CASCADE, index | non | — |
| `kind` | `notification_channel_kind` | non | — | — | non | smtp / resend / discord / telegram / slack (Mattermost compatible) / pushover / webhook (§11). |
| `name` | `text` | non | — | UNIQUE `(team_id, name)` | non | — |
| `config_enc` | `bytea` | non | — | — | **oui** | Configuration JSON sérialisée puis chiffrée enveloppe (tokens, URLs, credentials SMTP). |
| `use_instance_email` | `boolean` | non | `false` | — | non | Réutilise l'email transactionnel de l'instance (§14.2). |
| `enabled` | `boolean` | non | `true` | — | non | — |
| `created_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 11.3 `notification_rules`

Événement activé par canal (§11), enrichi du routage/agrégation de la décision §27.19 (scoping projet/environnement, sévérité, débounce, heures calmes, résumé différé). Suppression : **CASCADE** avec le canal.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | — |
| `channel_id` | `bigint` | non | — | FK `notification_channels(id)` ON DELETE CASCADE ; UNIQUE NULLS NOT DISTINCT `(channel_id, event_type, project_id, environment_id)` | non | — |
| `event_type` | `text` | non | — | (cf. ci-dessus) | non | Nomenclature des événements (§11, §24.2) : `deployment.failed`, `server.unreachable`, `backup.failed`… |
| `enabled` | `boolean` | non | `true` | — | non | Activable individuellement par canal (§11). |
| `project_id` | `bigint` | oui | — | FK `projects(id)` ON DELETE CASCADE | non | Routage par projet (§27.19) ; NULL = toute la team. |
| `environment_id` | `bigint` | oui | — | FK `environments(id)` ON DELETE CASCADE | non | Routage par environnement. |
| `min_severity` | `notification_severity` | non | `'info'` | — | non | Seuil de sévérité (§27.19). |
| `debounce_seconds` | `integer` | non | `0` | CHECK `>= 0` | non | Agrégation anti-flapping (§27.19). |
| `quiet_hours_start` | `time` | oui | — | — | non | Heures calmes (§27.19). |
| `quiet_hours_end` | `time` | oui | — | — | non | — |
| `digest_enabled` | `boolean` | non | `false` | — | non | Résumé différé des événements non critiques (§27.19). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 11.4 `audit_events`

Journal d'audit **append-only** (§23.4) : aucune mise à jour ni suppression unitaire ; pagination, filtrage, export ; purge par rétention (partitionnement mensuel recommandé). Volontairement **sans FK** vers `users`/`teams`/`api_tokens` : l'événement snapshot l'acteur et survit à toute suppression (§19.2 « aucune cascade accidentelle »).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | `event_id` (§23.4). |
| `occurred_at` | `timestamptz` | non | `now()` | index BRIN ; index `(team_id, occurred_at DESC)` | non | Date UTC. |
| `team_id` | `bigint` | oui | — | (sans FK) | non | Team concernée ; NULL pour les événements instance (login, settings). |
| `actor_kind` | `actor_kind` | non | — | — | non | user / token / system (§24.2). |
| `actor_uuid` | `uuid` | oui | — | — | non | UUID de l'utilisateur ou du token (snapshot). |
| `actor_display` | `text` | oui | — | — | non | Nom/préfixe de token au moment de l'action. |
| `action` | `text` | non | — | index | non | Ex. `secret.reveal`, `terminal.open`, `deployment.rollback`, `server.delete` (liste §23.4). |
| `target_kind` | `text` | oui | — | — | non | Type de la cible (ex. `application`). |
| `target_uuid` | `uuid` | oui | — | index | non | UUID de la cible. |
| `result` | `audit_result` | non | — | — | non | success / failure / denied. |
| `ip` | `inet` | oui | — | — | non | — |
| `user_agent` | `text` | oui | — | — | non | — |
| `request_id` | `uuid` | oui | — | — | non | Corrélation avec les logs API (§13, §23.4). |
| `correlation_id` | `uuid` | oui | — | — | non | Chaîne d'événements (§24.2). |
| `diff_redacted` | `jsonb` | oui | — | — | non | Diff avant/après, secrets systématiquement redacted (INV-003). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

### 11.5 `outbox_events`

Transactional outbox (§18.2) : l'événement est écrit dans la même transaction que la mutation, publié après commit (§24.2). Le `bigint` séquentiel donne l'ordre de publication. Suppression : **purge** des événements publiés après une rétention courte.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | Ordre de commit/publication. |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | `id` de l'envelope (§24.2) ; clé de déduplication des consommateurs (inbox). |
| `event_type` | `text` | non | — | — | non | Versionné dans le type : `deployment.succeeded.v1` (§24.2). |
| `occurred_at` | `timestamptz` | non | `now()` | — | non | RFC3339Nano dans l'envelope. |
| `team_uuid` | `uuid` | oui | — | — | non | Référence par UUID public (pas de FK : l'événement est un fait immuable). |
| `resource_uuid` | `uuid` | oui | — | — | non | — |
| `actor` | `jsonb` | oui | — | — | non | `{type, uuid}` (§24.2). |
| `correlation_id` | `uuid` | oui | — | — | non | — |
| `aggregate_key` | `text` | oui | — | index | non | Ordre garanti par clé d'agrégat si nécessaire (§24.2). |
| `payload` | `jsonb` | non | `'{}'` | — | non | Références et métadonnées redacted, **jamais** de valeur de secret (§24.2, INV-003). |
| `published_at` | `timestamptz` | oui | — | index partiel `(id) WHERE published_at IS NULL` | non | NULL = à publier. |
| `publish_attempts` | `integer` | non | `0` | — | non | Retry borné avec jitter (§22.1). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |

### 11.6 `feature_flags`

Activation de capacités expérimentales/dépréciées (ex. Swarm P3 derrière flag, §26.1) au niveau instance (`team_id` NULL) ou par team. Suppression : libre.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `key` | `text` | non | — | UNIQUE NULLS NOT DISTINCT `(key, team_id)` | non | Ex. `swarm`, `caddy_proxy`, `mcp_server`. |
| `team_id` | `bigint` | oui | — | FK `teams(id)` ON DELETE CASCADE | non | NULL = valeur d'instance ; ligne team = override. |
| `enabled` | `boolean` | non | `false` | — | non | — |
| `description` | `text` | oui | — | — | non | — |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

### 11.7 `instance_settings`

Réglages d'instance (§14.2) — **ajout au §19.1**, impliqué par les features FQDN d'instance, email transactionnel, DNS de validation, auto-update et onboarding. Table **singleton** (une seule ligne, `CHECK (id = 1)`). Suppression : interdite.

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `smallint` | non | `1` | PK, CHECK `(id = 1)` | non | Singleton. |
| `fqdn` | `text` | oui | — | — | non | FQDN du dashboard derrière le proxy (§14.2, port unique §27.1). |
| `timezone` | `text` | non | `'UTC'` | — | non | Affichage et crons de maintenance (§14.2). |
| `registration_enabled` | `boolean` | non | `false` | — | non | Inscription publique on/off (§10.2, §14.2) — fermée par défaut. |
| `api_enabled` | `boolean` | non | `false` | — | non | API désactivée par défaut (§10.3). |
| `dns_validation_server` | `text` | non | `'1.1.1.1'` | — | non | DNS de validation custom (§4.2, §14.2). |
| `transactional_email_config_enc` | `bytea` | oui | — | — | **oui** | SMTP/Resend de l'instance (invitations, reset — §14.2), chiffré enveloppe. |
| `otlp_config_enc` | `bytea` | oui | — | — | **oui** | Export OTLP distant (endpoint, protocole, en-têtes d'auth, signaux — §14.2, ADR-008), chiffré enveloppe ; lu au boot, appliqué au prochain redémarrage. |
| `auto_update_enabled` | `boolean` | non | `true` | — | non | Vérification périodique, désactivable (§14.3). |
| `auto_update_cron` | `text` | oui | — | CHECK validation cron | non | Cron d'auto-update configurable (§14.3). |
| `onboarding_completed_at` | `timestamptz` | oui | — | — | non | Assistant premier démarrage (§14.2, §25.1). |
| `updated_by` | `bigint` | oui | — | FK `users(id)` ON DELETE SET NULL | non | Réservé au root (§10.1). |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |
| `version` | `integer` | non | `1` | — | non | Verrou optimiste. |

### 11.8 `jobs`

Queue durable PostgreSQL (décision §27.2), machine à états §21.3 : lease avec expiration, heartbeat, retry borné, dead-letter (INV-013). Consommation par `SELECT … FOR UPDATE SKIP LOCKED`. Files/priorités séparées par `queue` (backups, cleanup, tâches utilisateur — §24.3). Suppression : **purge** des jobs terminés par rétention ; les `dead_letter` sont conservés jusqu'à intervention (retry/forget).

| Colonne | Type PostgreSQL | Null | Défaut | Contraintes | Sensible | Description |
|---|---|---|---|---|---|---|
| `id` | `bigint` | non | identity | PK | non | — |
| `uuid` | `uuid` | non | `gen_random_uuid()` | UNIQUE | non | Suivi API (`202` + URL de suivi, §24.1). |
| `queue` | `text` | non | `'default'` | index composite (voir ci-dessous) | non | File logique : `deploy`, `backup`, `cleanup`, `notify`, `maintenance`… |
| `job_type` | `text` | non | — | — | non | Ex. `deployment.run`, `server.validate`, `backup.execute`, `resource.delete`. |
| `payload` | `jsonb` | non | `'{}'` | — | non | Références (UUIDs) et paramètres ; **jamais** de secret en clair (INV-003). |
| `status` | `job_status` | non | `'queued'` | index partiel `(queue, priority DESC, run_at, id) WHERE status = 'queued'` | non | Machine à états §21.3 (`scheduled` = déclenchement différé). |
| `priority` | `integer` | non | `0` | — | non | Priorité intra-file (§24.3). |
| `run_at` | `timestamptz` | non | `now()` | — | non | Ne pas exécuter avant (retry backoff avec jitter, §22.1). |
| `attempt` | `integer` | non | `0` | — | non | Tentatives effectuées. |
| `max_attempts` | `integer` | non | `5` | CHECK `> 0` | non | Au-delà → `dead_letter` (§21.3). |
| `idempotency_key` | `text` | oui | — | UNIQUE | non | Clé d'idempotence des opérations distantes (INV-004, `Idempotency-Key` §24.1). |
| `lock_key` | `text` | oui | — | UNIQUE partiel `WHERE status IN ('leased','running')` | non | Verrou d'exclusivité par ressource/serveur (ex. `switch:app:<uuid>`, §21.1, §18.2). |
| `leased_by` | `text` | oui | — | — | non | Identifiant du worker détenteur. |
| `lease_expires_at` | `timestamptz` | oui | — | index partiel `WHERE status IN ('leased','running')` | non | Reprise par un autre worker **après expiration uniquement**, avec inspection préalable de l'effet produit (§21.3, §22.1). |
| `heartbeat_at` | `timestamptz` | oui | — | — | non | Prolonge le lease (INV-013). |
| `cancel_requested_at` | `timestamptz` | oui | — | — | non | Annulation coopérative : le worker vérifie ce champ à chaque checkpoint entre deux étapes (spec deployment-engine §2.6). |
| `last_error` | `text` | oui | — | — | non | Classification d'erreur (§22.1), redacted. |
| `steps` | `jsonb` | non | `'[]'` | — | non | Étapes visibles du job (schéma OpenAPI `JobStep` : name, status, message, started_at, finished_at — §20.1). |
| `result` | `jsonb` | oui | — | — | non | Résultat structuré en cas de succès (schéma OpenAPI `Job.result`) ; jamais de secret (INV-003). |
| `retry_of_id` | `bigint` | oui | — | FK `jobs(id)` ON DELETE SET NULL | non | Job d'origine si nouvelle tentative liée créée par un retry dead-letter (deployment-engine §2.4). |
| `team_id` | `bigint` | oui | — | FK `teams(id)` ON DELETE SET NULL | non | Limites de concurrence par team (§22.2). |
| `resource_id` | `bigint` | oui | — | FK `resources(id)` ON DELETE SET NULL | non | Cible principale, pour l'UI « jobs de la ressource ». |
| `correlation_id` | `uuid` | oui | — | — | non | Traçabilité bout en bout (§13, §24.2). |
| `dead_lettered_at` | `timestamptz` | oui | — | — | non | — |
| `finished_at` | `timestamptz` | oui | — | — | non | — |
| `created_at` | `timestamptz` | non | `now()` | — | non | — |
| `updated_at` | `timestamptz` | non | `now()` | — | non | — |

---

## 12. Récapitulatif des données sensibles (chiffrement enveloppe §23.2, §27.3)

| Table.colonne | Contenu |
|---|---|
| `private_keys.private_key_enc` | Clés SSH privées |
| `mfa_factors.secret_enc` | Secrets TOTP |
| `cloud_credentials.token_enc` | Tokens fournisseur cloud |
| `registry_credentials.password_enc` | Credentials registry |
| `s3_storages.access_key_enc`, `s3_storages.secret_key_enc` | Credentials S3 |
| `github_apps.client_secret_enc`, `webhook_secret_enc`, `app_private_key_enc` | Secrets GitHub App |
| `webhook_endpoints.secret_enc` | Secrets HMAC des webhooks entrants |
| `environment_variables.value_enc`, `shared_variables.value_enc` | Valeurs de variables (secrètes ou non) |
| `database_credentials.password_enc` | Mots de passe des bases managées |
| `servers.ca_key_enc` | Clé privée de la CA SSL bases |
| `servers.log_drain_config_enc` | Tokens des log drains |
| `notification_channels.config_enc` | Tokens/credentials des canaux |
| `instance_settings.transactional_email_config_enc` | SMTP/Resend de l'instance |
| `instance_settings.otlp_config_enc` | Endpoint + en-têtes d'auth de l'export OTLP |

Hashés (irréversibles, jamais chiffrés car jamais restitués) : `users.password_hash` (Argon2id), `api_tokens.token_hash`, `sessions.token_hash`, `invitations.token_hash`, `servers.sentinel_token_hash`, `mfa_factors.recovery_code_hashes` (SHA-256, avec préfixe d'identification pour les tokens API — §23.2).

**Total : 54 tables** (8 Identité, 5 Organisation, 7 Infrastructure, 5 Source, 9 Application, 6 Service/DB, 6 Exécution, 7 Plateforme dont `instance_settings` ajoutée, 1 queue technique). Toutes les entités du §19.1 sont couvertes ; les ajouts au-delà de la liste (§19.1) sont : `resource_tags` (matérialisation du N—N `Tag`), `shared_variables` (§5.4), `previews` (§5.6/§20.4), `instance_settings` (§14.2), `certificates` (reflet observé du sous-système certificats, §4.3/§6.3, proxy-contract §7) — chacun impliqué par une feature explicite du PRD.
