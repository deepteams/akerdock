# Matrice RBAC / Permissions — AkerDock (artefact §29.7)

> ⚠️ **Modèle de rôles mis à jour par [ADR-038](../adr/ADR-038-roles-model.md)**
> (supersede la partie rôles d'ADR-007). Rôles de team = **`admin` / `member` /
> `reviewer`** + **rôles custom** ; `owner` est fusionné dans `admin` ; le **root
> est réservé à l'instance** (`users.is_root`, hors modèle de team). ADR-038 acte
> aussi que les **permissions granulaires `domaine:action` de ce document
> deviennent l'unité d'évaluation réelle** (aujourd'hui l'enforcement est coarse
> et le granulaire n'est que documentaire) : chaque opération portera un
> `x-required-permission` granulaire, avec une **table de prérequis** (§3 ADR-038)
> et fermeture transitive. Les colonnes `owner / developer / viewer` ci-dessous
> sont remplacées par `admin / member / reviewer` (+ custom) et **régénérées** à
> l'implémentation.

> Document de spécification d'autorisation (artefact §29.7 du PRD, `docs/PRD.md`).
> Décision de référence : **ADR-007 / §27.7** — RBAC fin, modèle **permissions à la carte** :
> chaque action produit est une permission granulaire `domaine:action` ; un rôle est un
> ensemble nommé de permissions, attribuable au niveau **team, projet ou environnement**
> (le scope le plus spécifique gagne). Rôles système immuables : **owner, admin, developer,
> viewer** (read-only strict) ; rôles custom composables par les admins de team.
>
> Cohérence : les permissions de token API §10.3 (`read`, `read:sensitive`, `write`,
> `deploy`, `root`) restent le socle d'évaluation à l'action (§24.1) et sont **mappées**
> sur ce modèle granulaire (§4 + §7). Les `x-required-permission` de l'OpenAPI deviennent
> une projection de ces permissions granulaires (table de correspondance §7).
>
> Les défauts proposés au-delà de la parité sont marqués **(défaut proposé)**.

---

## 1. Modèle

### 1.1 Permissions granulaires

Une permission est nommée `domaine:action`. Elle représente **une capacité produit atomique**.
Elle est **positive uniquement** (pas de permission négative) : l'absence d'une permission vaut
**deny implicite** (§3.4).

Familles de domaines :

| Domaine | Portée |
|---|---|
| `team` | Administration de la team, membres, invitations |
| `roles` | Rôles custom et attributions |
| `tokens` | Tokens API |
| `projects` / `environments` | Hiérarchie organisationnelle |
| `resources` | Vue transverse et adoption de ressources |
| `applications` | Applications (config, lifecycle, déploiement) |
| `databases` | Bases managées |
| `services` | Services one-click / stacks compose |
| `secrets` | Variables d'environnement et valeurs sensibles |
| `servers` | Serveurs cibles, proxy, maintenance |
| `certificates` | Certificats TLS des serveurs (reflet observé, expiration, renouvellement) |
| `keys` | Clés SSH privées |
| `sources` | Sources Git / GitHub Apps / webhooks |
| `registries` | Credentials de registry |
| `cloud` | Credentials DNS-01 des certificats wildcard (§4.3 ; le provisioning cloud est retiré — ADR-027) |
| `storages` | S3 storages |
| `backups` | Plans de backup, exécutions, restore |
| `deployments` | Historique, logs, annulation |
| `jobs` | Jobs asynchrones (queue durable) et dead-letter (retry/forget) |
| `previews` | Déploiements de PR / forks |
| `templates` | Catalogue de templates de team |
| `terminal` | Sessions terminal |
| `logs` | Logs runtime, drains |
| `metrics` | Métriques, uptime |
| `notifications` | Canaux et règles |
| `audit` | Journal d'audit |
| `config` | Config-as-code (export/apply) |
| `instance` | Réglages instance (root uniquement) |

### 1.2 Liste complète des permissions (72)

> Convention : `read`/`view`/`list` = lecture non sensible ; `read:sensitive` = révélation de
> secret (INV-003) ; `manage`/`create`/`update`/`delete` = mutation ; `deploy`/actions
> = lifecycle. Chaque permission map vers une permission de token §10.3 (colonne « token »).

#### Identité, team, RBAC

| # | Permission | Description | Token map |
|---|---|---|---|
| 1 | `team:read` | Voir la team, ses réglages non sensibles | `read` |
| 2 | `team:manage` | Modifier la team, réglages, suppression | `write` |
| 3 | `members:read` | Lister les membres et leurs rôles | `read` |
| 4 | `members:manage` | Inviter, retirer, changer le rôle d'un membre | `write` |
| 5 | `invitations:manage` | Créer/révoquer des invitations | `write` |
| 6 | `roles:read` | Voir les rôles et attributions | `read` |
| 7 | `roles:manage` | Créer/éditer/supprimer des rôles custom et les attribuer | `write` |
| 8 | `tokens:read` | Lister les tokens API (métadonnées, jamais la valeur) | `read` |
| 9 | `tokens:create` | Créer un token API (garde anti-élévation §4) | `write` |
| 10 | `tokens:revoke` | Révoquer un token API | `write` |

#### Organisation

| # | Permission | Description | Token map |
|---|---|---|---|
| 11 | `projects:read` | Lister/voir les projets | `read` |
| 12 | `projects:manage` | Créer/éditer/supprimer des projets | `write` |
| 13 | `environments:read` | Lister/voir les environnements | `read` |
| 14 | `environments:manage` | Créer/éditer/supprimer des environnements | `write` |
| 15 | `resources:read` | Vue transverse des ressources | `read` |
| 16 | `resources:adopt` | Adopter/désadopter une ressource existante (§20.7) | `write` |
| 17 | `environments:deploy` | Déploiement coordonné d'un environnement (§20.8) | `deploy` |

#### Applications

| # | Permission | Description | Token map |
|---|---|---|---|
| 18 | `applications:read` | Lister/voir config d'application (secrets masqués) | `read` |
| 19 | `applications:create` | Créer une application | `write` |
| 20 | `applications:update` | Modifier la configuration (source, ports, limits, options…) | `write` |
| 21 | `applications:delete` | Supprimer une application | `write` |
| 22 | `applications:deploy` | Déclencher deploy/redeploy/rollback | `deploy` |
| 23 | `applications:lifecycle` | Start/stop/restart | `deploy` |
| 24 | `applications:exec` | Scheduled tasks / commandes pre-post (exec) | `deploy` |

#### Bases de données

| # | Permission | Description | Token map |
|---|---|---|---|
| 25 | `databases:read` | Lister/voir les bases (credentials masqués) | `read` |
| 26 | `databases:create` | Créer une base managée | `write` |
| 27 | `databases:update` | Modifier config/SSL/limits d'une base | `write` |
| 28 | `databases:delete` | Supprimer une base | `write` |
| 29 | `databases:lifecycle` | Start/stop/restart d'une base | `deploy` |
| 30 | `databases:credentials` | Révéler les credentials/URL de connexion (`read:sensitive`) | `read:sensitive` |

#### Services / Compose

| # | Permission | Description | Token map |
|---|---|---|---|
| 31 | `services:read` | Voir services/stacks compose | `read` |
| 32 | `services:manage` | Créer/éditer/supprimer un service, éditer le compose | `write` |
| 33 | `services:deploy` | Deploy/pull latest/lifecycle par sous-container | `deploy` |

#### Secrets / variables

| # | Permission | Description | Token map |
|---|---|---|---|
| 34 | `secrets:read` | Lister les clés de variables (valeurs masquées) | `read` |
| 35 | `secrets:reveal` | Révéler les valeurs de variables/secrets (INV-003) | `read:sensitive` |
| 36 | `secrets:write` | Créer/éditer/supprimer des variables (dont bulk) | `write` |

#### Infrastructure : serveurs, clés, proxy

| # | Permission | Description | Token map |
|---|---|---|---|
| 37 | `servers:read` | Lister/voir serveurs, ressources, domaines | `read` |
| 38 | `servers:manage` | Créer/éditer/retirer un serveur, validation/install | `write` |
| 39 | `servers:maintain` | Cleanup, cycle de vie du proxy | `write` |
| 40 | `servers:proxy` | Éditer la config proxy, régénérer labels, rotation CA | `write` |
| 41 | `keys:read` | Lister les clés SSH (métadonnées) | `read` |
| 42 | `keys:reveal` | Révéler le matériel de clé privée (`read:sensitive`) | `read:sensitive` |
| 43 | `keys:manage` | Créer/éditer/supprimer/rotation des clés SSH | `write` |
| 68 | `certificates:read` | Inventaire des certificats d'un serveur (domaines, expiration, statut — reflet observé) | `read` |
| 69 | `certificates:renew` | Forcer le renouvellement/la ré-émission d'un certificat (202 + job, audité) | `write` |

#### Sources, registries, cloud, storages

| # | Permission | Description | Token map |
|---|---|---|---|
| 44 | `sources:read` | Voir sources Git / GitHub Apps / webhooks | `read` |
| 45 | `sources:manage` | Configurer sources Git, GitHub Apps, webhooks | `write` |
| 46 | `registries:manage` | Gérer les credentials de registry | `write` |
| 47 | `cloud:read` | Voir les cloud provider tokens (métadonnées) | `read` |
| 48 | `cloud:manage` | Gérer les credentials DNS-01 (§4.3, proxy-contract §7.2) | `write` |
| 49 | `storages:manage` | Gérer les S3 storages (CRUD, vérification) | `write` |

#### Backups

| # | Permission | Description | Token map |
|---|---|---|---|
| 50 | `backups:read` | Voir plans et exécutions de backup | `read` |
| 51 | `backups:manage` | Créer/éditer/supprimer des plans, Backup Now | `write` |
| 52 | `backups:restore` | Restaurer une base/volume depuis un backup | `write` |

#### Exécution, observabilité, terminal

| # | Permission | Description | Token map |
|---|---|---|---|
| 53 | `deployments:read` | Historique, détail, logs de build (SSE) | `read` |
| 54 | `deployments:cancel` | Annuler un déploiement en cours | `deploy` |
| 70 | `jobs:manage` | Retry/forget des jobs en dead-letter (action manuelle auditée, §21.3, deployment-engine §2.4) | `write` |
| 55 | `previews:manage` | Gérer previews, approuver une PR de fork (§20.4.8) | `write` |
| 56 | `templates:manage` | Enregistrer/synchroniser des repos de templates (§27.10) | `write` |
| 57 | `terminal:open` | Ouvrir un terminal container/serveur (non-root) | `write` |
| 58 | `terminal:root` | Ouvrir un terminal **root** (double contrôle §5) | `write` |
| 72 | `port-forwards:open` | Ouvrir un tunnel TCP vers un container d'une ressource (CLI, ADR-032) — frontière au grain de la ressource | `write` |
| 59 | `logs:read` | Logs runtime des containers | `read` |
| 60 | `logs:manage` | Configurer les log drains | `write` |
| 61 | `metrics:read` | Métriques serveur/ressource, uptime | `read` |
| 62 | `notifications:manage` | Canaux et règles de notification | `write` |
| 63 | `audit:read` | Consulter le journal d'audit | `read` |
| 64 | `config:export` | Exporter la config-as-code (YAML) | `read` |
| 65 | `config:apply` | Apply idempotent de la config-as-code (§24.5) | `write` |

#### Instance (root d'instance uniquement — hors scope team)

| # | Permission | Description | Token map |
|---|---|---|---|
| 66 | `instance:manage` | Réglages instance, activer/désactiver l'API, updates | `root` |
| 67 | `instance:audit` | Audit global inter-team | `root` |
| 71 | `instance:encryption` | État du chiffrement au repos et rotation forcée de la clé maître (re-chiffrement — ADR-003) | `root` |

> **Total : 72 permissions granulaires** (dont 3 exclusivement `instance:*` réservées au root
> d'instance, hors modèle de rôle de team). Le socle « produit team » couvre 68 permissions,
> soit dans la fourchette cible §29.7 (~40-60) élargie pour couvrir tout le périmètre du PRD.

---

## 2. Matrice permissions × rôles système

> Légende : ● = accordée ; ○ = non accordée. Les 4 rôles système sont **immuables** (§3.4).
> `owner` et `admin` diffèrent uniquement sur l'administration de la team elle-même
> (suppression de team, gestion des rôles, retrait de l'owner). `viewer` est **read-only strict** :
> **aucune mutation, aucun secret** (INV-003).

| Permission | owner | admin | developer | viewer |
|---|:---:|:---:|:---:|:---:|
| team:read | ● | ● | ● | ● |
| team:manage | ● | ○ | ○ | ○ |
| members:read | ● | ● | ● | ● |
| members:manage | ● | ● | ○ | ○ |
| invitations:manage | ● | ● | ○ | ○ |
| roles:read | ● | ● | ● | ● |
| roles:manage | ● | ● | ○ | ○ |
| tokens:read | ● | ● | ● | ○ |
| tokens:create | ● | ● | ○ | ○ |
| tokens:revoke | ● | ● | ○ | ○ |
| projects:read | ● | ● | ● | ● |
| projects:manage | ● | ● | ● | ○ |
| environments:read | ● | ● | ● | ● |
| environments:manage | ● | ● | ● | ○ |
| resources:read | ● | ● | ● | ● |
| resources:adopt | ● | ● | ● | ○ |
| environments:deploy | ● | ● | ● | ○ |
| applications:read | ● | ● | ● | ● |
| applications:create | ● | ● | ● | ○ |
| applications:update | ● | ● | ● | ○ |
| applications:delete | ● | ● | ● | ○ |
| applications:deploy | ● | ● | ● | ○ |
| applications:lifecycle | ● | ● | ● | ○ |
| applications:exec | ● | ● | ● | ○ |
| databases:read | ● | ● | ● | ● |
| databases:create | ● | ● | ● | ○ |
| databases:update | ● | ● | ● | ○ |
| databases:delete | ● | ● | ● | ○ |
| databases:lifecycle | ● | ● | ● | ○ |
| databases:credentials | ● | ● | ● | ○ |
| services:read | ● | ● | ● | ● |
| services:manage | ● | ● | ● | ○ |
| services:deploy | ● | ● | ● | ○ |
| secrets:read | ● | ● | ● | ● |
| secrets:reveal | ● | ● | ● | ○ |
| secrets:write | ● | ● | ● | ○ |
| servers:read | ● | ● | ● | ● |
| servers:manage | ● | ● | ○ | ○ |
| servers:maintain | ● | ● | ○ | ○ |
| servers:proxy | ● | ● | ○ | ○ |
| certificates:read | ● | ● | ● | ● |
| certificates:renew | ● | ● | ○ | ○ |
| keys:read | ● | ● | ● | ● |
| keys:reveal | ● | ● | ○ | ○ |
| keys:manage | ● | ● | ○ | ○ |
| sources:read | ● | ● | ● | ● |
| sources:manage | ● | ● | ● | ○ |
| registries:manage | ● | ● | ● | ○ |
| cloud:read | ● | ● | ○ | ○ |
| cloud:manage | ● | ● | ○ | ○ |
| storages:manage | ● | ● | ● | ○ |
| backups:read | ● | ● | ● | ● |
| backups:manage | ● | ● | ● | ○ |
| backups:restore | ● | ● | ● | ○ |
| deployments:read | ● | ● | ● | ● |
| deployments:cancel | ● | ● | ● | ○ |
| jobs:manage | ● | ● | ○ | ○ |
| previews:manage | ● | ● | ● | ○ |
| templates:manage | ● | ● | ● | ○ |
| terminal:open | ● | ● | ● | ○ |
| terminal:root | ● | ● | ○ | ○ |
| port-forwards:open | ● | ● | ● | ○ |
| logs:read | ● | ● | ● | ● |
| logs:manage | ● | ● | ● | ○ |
| metrics:read | ● | ● | ● | ● |
| notifications:manage | ● | ● | ● | ○ |
| audit:read | ● | ● | ● | ○ |
| config:export | ● | ● | ● | ● |
| config:apply | ● | ● | ● | ○ |

> Notes de conception :
> - **developer** = l'acteur « Member/Développeur » et « Opérateur/SRE » (§16.3) : plein pouvoir
>   applicatif (créer/déployer/backup/restore/terminal non-root) mais **pas** d'administration
>   d'infrastructure sensible (serveurs, clés SSH, cloud, terminal root, gestion de membres/rôles).
>   Un déploiement plus fin (ex. « deploy sur staging mais pas production ») se fait par
>   **attribution de rôle scopée à l'environnement** (§3.1) — le rôle système `developer` attribué
>   à `env=staging` seulement.
> - **viewer** = « Intégration read-only/MCP » et audit : aucune mutation, `secrets:reveal` refusé,
>   `databases:credentials`/`keys:reveal` refusés (INV-003). `config:export` autorisé car il ne
>   contient jamais de secret inline (§24.5).
> - **certificates:read** est accordée à `viewer` : l'inventaire d'expiration (domaines,
>   `not_after`, statut) ne contient aucun secret — le matériel de clé privée ne quitte jamais
>   le serveur — et le monitoring read-only est précisément le cas d'usage viewer/MCP,
>   cohérent avec `servers:read` et `metrics:read` (INV-003 respecté). **certificates:renew**
>   est alignée sur `servers:maintain` (admin+) : un renouvellement forcé touche l'infra du
>   serveur (édition d'`acme.json`, redémarrage du proxy) et consomme du quota Let's Encrypt.
> - **jobs:manage** (retry/forget dead-letter) est réservée à admin+ : le forget peut abandonner
>   une suppression laissant des restes distants (§20.6.4) ; le rejeu par canal métier (deploy,
>   backup, validation serveur) reste accessible au developer via ses permissions existantes.
> - Les permissions `instance:*` (66, 67, 71) ne figurent pas dans les rôles de team : elles
>   sont portées exclusivement par le **root d'instance** (§3.5).

---

## 3. Règles de résolution

### 3.1 Attribution et scopes

Un rôle est attribuable à trois niveaux, du plus général au plus spécifique :

```
team  ⊃  project  ⊃  environment
```

Une attribution = `(sujet, rôle, scope)` où `sujet ∈ {membre, rôle custom}` et
`scope ∈ {team_uuid, project_uuid, environment_uuid}`.

### 3.2 Héritage (le plus spécifique gagne — override, pas intersection)

- Une attribution au niveau **team** s'applique à tous ses projets et environnements.
- Une attribution au niveau **projet** s'applique à tous ses environnements.
- Une attribution au niveau **environnement** ne s'applique qu'à cet environnement.
- **Le scope le plus spécifique gagne** : si un membre est `viewer` au niveau team mais
  `developer` sur `project=X`, il est développeur sur X et lecteur ailleurs.
- L'override est **par ensemble d'attributions**, résolu à l'action pour la ressource ciblée :
  on retient l'attribution la plus spécifique couvrant le scope de la ressource.

### 3.3 Cumul multi-rôles (union)

- Un membre peut porter plusieurs rôles (système et/ou custom) **au même scope** : l'ensemble
  effectif de permissions à ce scope est l'**union** de leurs permissions.
- Entre scopes différents, on applique d'abord la règle du plus spécifique (§3.2), puis l'union
  au sein du scope retenu.
- Formellement, pour une action sur une ressource `r` :
  `perms(sujet, r) = ⋃ { rôle.permissions | attribution(sujet, rôle, scope) ∧ scope = plus_spécifique_couvrant(r) }`.

### 3.4 Deny implicite et immuabilité

- **Deny par défaut** : une permission absente de l'ensemble effectif est refusée. Aucune
  permission négative n'existe (pas d'exception à composer).
- **Rôles système immuables** : `owner`, `admin`, `developer`, `viewer` ne sont ni éditables ni
  supprimables. Un admin qui veut dévier crée un **rôle custom** (composable, §1).
- Réponse d'un refus : `not_found` pour une ressource d'une autre team (pas d'oracle, INV-002) ;
  `403 forbidden` pour un refus de permission intra-team.

### 3.5 Cas du root d'instance

- Le **root d'instance** (`users.is_root`) est hors du modèle de rôle de team : il détient
  implicitement toutes les permissions sur toutes les teams **plus** `instance:*` (§10.1).
- Il n'est jamais membre implicite d'une team pour l'audit : ses actions inter-team sont tracées
  avec `actor.type=user` + flag root (§23.4).
- Un token créé par le root est **scopé à une team** comme tout token (§10.3) ; le root ne peut
  pas créer un token « global » — un token `root` reste borné à sa team (voir §4).

---

## 4. Tokens API — mapping et garde anti-élévation

### 4.1 Rappel du modèle de token (§10.3)

Un token API porte un sous-ensemble de `{read, read:sensitive, write, deploy, root}`, est scopé
à **une team**, hashé SHA-256, avec IP allowlist et expiration (`api_tokens`, §10.3).

### 4.2 Permissions effectives d'un token = intersection

> **Décision (arbitrage du point OpenAPI)** : un token n'accorde jamais plus que son créateur.

```
perms_effectives(token) = perms_token(token)  ∩  perms_RBAC(créateur, réévaluées à l'usage)
```

- `perms_token` : projection des scopes `{read, read:sensitive, write, deploy, root}` du token
  vers les permissions granulaires (via la colonne « token map » du §1 et la table §7).
- `perms_RBAC(créateur)` : les permissions RBAC du créateur **dans la team du token**, au scope
  concerné, **réévaluées à chaque requête** (pas figées à la création).
- Conséquence : si le créateur perd un droit (rétrogradation de rôle), le token perd ce droit à
  la requête suivante, sans révocation explicite.

### 4.3 Garde anti-élévation à la création (`tokens:create`)

- La création de token exige la permission dédiée **`tokens:create`** (incluse dans `admin` et
  `owner`, absente de `developer` et `viewer` — cf. §2). C'est l'arbitrage du point soulevé par
  l'OpenAPI (`createApiToken`, `x-required-permission: write` + garde) : `write` seul ne suffit
  pas conceptuellement ; la capacité est portée par `tokens:create`, réservée à admin+.
- **Anti-élévation obligatoire** : le créateur ne peut octroyer au token que des permissions
  qu'il **possède lui-même** au scope visé. Toute demande d'un scope de token dont la projection
  dépasse `perms_RBAC(créateur)` → `403` (cohérent avec la description OpenAPI de `createApiToken`).
- Un token `root` ne peut être créé que par un porteur de `instance:manage` (root d'instance) ou,
  par extension, un owner/admin pour un token `root` **borné à sa team** — jamais un token à
  privilèges instance.

### 4.4 Révocation automatique sur perte de droits **(défaut proposé)**

- **(Défaut proposé)** : lorsqu'un créateur perd la permission `tokens:create` (rétrogradation,
  retrait de la team, suppression du compte), ses tokens actifs sont **automatiquement révoqués**
  (`revoked_at` renseigné, événement d'audit).
- Rationale : la réévaluation à l'usage (§4.2) neutralise déjà l'élévation, mais la révocation
  explicite ferme la fenêtre où un token « survivrait » à son créateur et clarifie l'audit.
- Alternative conservée si le défaut est refusé : conserver le token mais le réduire à
  l'intersection (§4.2), avec alerte à l'admin. À trancher en ADR si divergence.

---

## 5. Actions sensibles à double contrôle

> Ces actions exigent : **permission** + **confirmation renforcée** (§22.5) + **audit** (§23.4).
> La confirmation renforcée est une ré-authentification/step-up ou une saisie de confirmation
> explicite selon la criticité.

| Action | Permission requise | Contrôle supplémentaire | Réf. |
|---|---|---|---|
| Ouvrir un terminal **root** (shell serveur) | `terminal:root` | Step-up : **ré-authentification par passkey** récente pour une session navigateur (`403 stepup_required` sinon), permission `root` pour un token API — un token ne peut pas se ré-authentifier. Plus audit ouverture/fermeture + idle/kill | §24.4, §23.4, §10.4 |
| **Restore sur une base non vide** | `backups:restore` | Confirmation renforcée explicite + test de format préalable + journal complet | §20.5, §22.5 |
| Suppression **avec volumes/données** | `applications:delete` / `databases:delete` / `services:manage` | Prévisualisation des objets affectés + question distincte « conserver les volumes ? » + confirmation | §20.6, §22.5, INV-008 |
| **Rotation de CA** de bases | `servers:proxy` | Confirmation renforcée + audit | §6.3, §22.5, §23.4 |
| Suppression d'une team / cascade projet-environnement | `team:manage` / `projects:manage` | Prévisualisation cascade + confirmation ; RESTRICT tant que dépendances | §19.2, §10.1, INV-008 |
| Création d'un token `root`/`deploy` élevé | `tokens:create` | Garde anti-élévation (§4.3) + audit création/révocation | §10.3, §23.4 |
| **Rotation forcée de la clé maître** (re-chiffrement actif) | `instance:encryption` | Confirmation renforcée + `Idempotency-Key` + audit ; réservé au root d'instance | §23.2, ADR-003 |
| **Forget** d'un job en dead-letter avec restes distants | `jobs:manage` | Corps `acknowledge_remnants=true` obligatoire (sinon `409 remnants_present`) + audit | §20.6.4, §21.3 |

Toutes ces actions émettent un événement d'audit avec acteur/token, cible, résultat et diff
redacted (§23.4).

---

## 6. Tests d'autorisation exigés

> Chaque invariant d'autorisation possède au moins un test API/intégration (§17). La matrice
> ci-dessous est la base de la suite de tests de sécurité (§23.5).

### 6.1 Matrice inter-team (INV-002)
- Pour **chaque endpoint** et chaque relation indirecte : un acteur de la team A tentant
  d'accéder à un UUID de la team B reçoit `not_found` (jamais `403` révélant l'existence, jamais
  de fuite).
- Couvre : serveurs, clés, sources, destinations, storages, ressources, tokens, backups, previews.

### 6.2 Scope hopping (héritage §3.2)
- Un `developer` scopé à `env=staging` ne peut agir sur `env=production` du même projet.
- Un rôle scopé à `project=X` ne fuit pas vers `project=Y`.
- Vérifier que le plus spécifique gagne (override) et que l'union multi-rôles ne franchit pas les
  frontières de scope (§3.3).

### 6.3 Élévation par token (§4)
- Un créateur `developer` ne peut pas créer de token portant `write`/`deploy` dépassant ses droits
  → `403` (anti-élévation §4.3).
- Un token dont le créateur est rétrogradé perd le droit correspondant à la requête suivante
  (réévaluation §4.2) ; **(défaut proposé)** vérifier la révocation automatique (§4.4).
- Un `write` sans `tokens:create` ne peut pas créer de token → `403` (§4.3).

### 6.4 Chaque rôle système × chaque famille d'endpoints
- `owner`, `admin`, `developer`, `viewer` testés sur chaque famille (applications, databases,
  services, secrets, servers, keys, backups, terminal, deployments, cloud, config).
- **viewer** : toute mutation → `403` ; `secrets:reveal`, `keys:reveal`,
  `databases:credentials`, `terminal:*` → `403` (read-only strict, INV-003).
- **developer** : `servers:manage`, `keys:manage`, `cloud:*`, `terminal:root`,
  `members:manage`, `roles:manage`, `tokens:create` → `403`.

### 6.5 Actions sensibles (double contrôle §5)
- Terminal root, restore sur base non vide, suppression avec volumes, rotation CA : vérifier
  qu'en l'absence de confirmation renforcée l'action est refusée même si la permission est
  présente.

### 6.6 Cohérence avec l'audit
- Chaque action d'autorisation refusée/accordée sur une action sensible produit une entrée
  d'audit exploitable (§23.4).

---

## 7. Table de correspondance OpenAPI `x-required-permission` → permissions granulaires

> Les `x-required-permission` de `docs/specs/openapi-v1.yaml` restent le socle d'évaluation à
> l'action (§24.1). Cette table les projette sur les permissions granulaires : le contrôle
> d'accès effectif = permission granulaire ci-dessous, **et** le token doit porter le scope
> `x-required-permission` indiqué (les deux conditions, cohérent avec §4.2 intersection).

| operationId (OpenAPI) | x-required-permission | Permission(s) granulaire(s) |
|---|---|---|
| getHealth | (aucune) | — (public) |
| getVersion | read | `team:read` |
| enableApi / disableApi | root | `instance:manage` |
| listTeams / getTeam | read | `team:read` |
| listTeamMembers | read | `members:read` |
| listTeamInvitations | read | `members:read` |
| createTeamInvitation / revokeTeamInvitation | write | `invitations:manage` |
| listApiTokens | read | `tokens:read` |
| **createApiToken** | write | **`tokens:create`** (+ garde anti-élévation §4.3) |
| revokeApiToken | write | `tokens:revoke` |
| listProjects / getProject | read | `projects:read` |
| createProject / updateProject / deleteProject | write | `projects:manage` |
| listEnvironments / getEnvironment | read | `environments:read` |
| createEnvironment / updateEnvironment / deleteEnvironment | write | `environments:manage` |
| listPrivateKeys / getPrivateKey | read | `keys:read` (+ `keys:reveal` si matériel de clé demandé) |
| createPrivateKey / updatePrivateKey / deletePrivateKey | write | `keys:manage` |
| listServers / getServer / listServerResources / listServerDomains | read | `servers:read` |
| createServer / updateServer / deleteServer | write | `servers:manage` |
| validateServer | write | `servers:manage` |
| listApplications / getApplication | read | `applications:read` |
| createApplication | write | `applications:create` |
| updateApplication | write | `applications:update` |
| deleteApplication | write | `applications:delete` (double contrôle si volumes §5) |
| listApplicationEnvs | read | `secrets:read` (+ `secrets:reveal` pour les valeurs) |
| createApplicationEnv / updateApplicationEnv / deleteApplicationEnv / replaceApplicationEnvs | write | `secrets:write` |
| startApplication / stopApplication / restartApplication | deploy | `applications:lifecycle` |
| deployApplication / rollbackApplication | deploy | `applications:deploy` |
| listApplicationDeployments / getDeployment / getDeploymentLogs | read | `deployments:read` |
| cancelDeployment | deploy | `deployments:cancel` |
| webhookDeploy / webhookDeployPost | deploy | `applications:deploy` (token `deploy`) |
| listDatabases / getDatabase | read | `databases:read` (+ `databases:credentials` pour URLs/creds) |
| createPostgresqlDatabase | write | `databases:create` |
| updateDatabase | write | `databases:update` |
| deleteDatabase | write | `databases:delete` (double contrôle si volumes §5) |
| startDatabase / stopDatabase / restartDatabase | deploy | `databases:lifecycle` |
| listBackupPlans / getBackupPlan / listBackupExecutions | read | `backups:read` |
| createBackupPlan / updateBackupPlan / deleteBackupPlan / executeBackupPlan | write | `backups:manage` |
| **restoreBackupExecution** | write | **`backups:restore`** (double contrôle si base non vide §5) |
| getJob | read | permission de lecture de la ressource sous-jacente (contextuelle) |
| listJobs | read | permission de lecture de la ressource sous-jacente (contextuelle, comme getJob) |
| retryJob / forgetJob | write | `jobs:manage` (forget avec restes distants : double contrôle §5) |
| listServerCertificates / getCertificate | read | `certificates:read` |
| renewCertificate | write | `certificates:renew` |
| getEncryptionStatus / rotateEncryption | root | `instance:encryption` (rotation : double contrôle §5) |

> Observations transmises pour révision de l'OpenAPI :
> - `createApiToken` devrait exposer `x-required-permission: write` **plus** une extension
>   `x-required-grant: tokens:create` (ou équivalent) pour matérialiser la garde §4.3.
> - `restoreBackupExecution` et les `delete*` avec volumes devraient porter un marqueur
>   `x-sensitive-action: true` pour signaler le double contrôle §5.
> - `listPrivateKeys`/`listApplicationEnvs`/`getDatabase` : la révélation du matériel sensible
>   dépend de `read:sensitive` (INV-003) ; la permission granulaire `*:reveal` / `*:credentials`
>   conditionne les champs, cohérent avec `is_redacted`.

---

## 8. Synthèse

- **71 permissions granulaires** définies (`domaine:action`), dont 68 pour le modèle de rôle de
  team et 3 exclusivement `instance:*` (root d'instance).
- **4 rôles système immuables** : owner, admin, developer, viewer (read-only strict) + rôles
  custom composables par les admins de team (ADR-007 / §27.7).
- Attribution scopée team/projet/environnement, **le plus spécifique gagne** ; cumul multi-rôles
  par **union** ; **deny implicite**.
- Tokens API = **intersection** (perms token ∩ perms RBAC du créateur réévaluées à l'usage) ;
  création via `tokens:create` (admin+) avec **garde anti-élévation obligatoire** ; **révocation
  automatique sur perte de droits (défaut proposé)**.
- Actions sensibles (terminal root, restore base non vide, suppression avec volumes, rotation CA)
  = permission + confirmation renforcée + audit.
