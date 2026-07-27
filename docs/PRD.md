# PRD — AkerDock

> Spécification produit d'AkerDock, PaaS self-hosted en Go. Ce document définit le produit, ses garanties et ses critères d'acceptation.

### Statut et convention de lecture

- **Deux natures de sections** : les sections 1 à 14 décrivent le **périmètre fonctionnel** du produit (ce qu'il fait) ; les sections 16 et suivantes en tirent des **exigences vérifiables** (comment on prouve qu'il le fait).
- **Mots normatifs** : **DOIT**, **NE DOIT PAS**, **DEVRAIT** et **PEUT** ont leur sens habituel de niveaux d'exigence.
- **Objectif** : des comportements, des garanties et des critères d'acceptation. Les choix d'implémentation restent substituables tant qu'ils sont respectés.
- **Traçabilité** : toute décision structurante est consignée dans un ADR (`docs/adr/`), et son état de livraison dans la matrice §26.

---

## 1. Vision produit

**AkerDock** est un PaaS self-hosted open source (licence Apache 2.0) : une alternative auto-hébergée à Heroku / Netlify / Vercel. L'utilisateur connecte ses propres serveurs (VPS, bare metal, Raspberry Pi…) via SSH et déploie applications, bases de données et services en containers Docker, avec reverse proxy, SSL automatique, backups et monitoring — sans vendor lock-in.

**Proposition de valeur :**
- Déployer n'importe quelle app (Git ou image Docker) sur n'importe quel serveur Linux en quelques clics.
- Aucune fonctionnalité paywallée : tout ce que fait le produit est dans le binaire que l'on héberge.
- Tout est du Docker standard : pas de format propriétaire, réversible à tout moment — les ressources restent exploitables sans AkerDock.

**Stack technique :** binaire Go unique (control plane, API, UI, workers), PostgreSQL comme seule dépendance externe (état **et** queue — ADR-002), UI Angular servie sur le port unique du control plane, pilotage des serveurs cibles via SSH, distribution en deux services compose (ADR-021).

---

## 2. Concepts et modèle de données

Hiérarchie organisationnelle :

```
Team → Project → Environment (production, staging…) → Resource
Resource = Application | Database | Service (one-click)
Resource ⟶ déployée sur → Server + Destination (réseau Docker cible)
```

- **Team** : périmètre d'isolation (serveurs, ressources, tokens API, notifications sont scopés par team). Multi-teams supporté.
- **Project** : regroupement logique ; contient des environments (défaut : `production`).
- **Environment** : jeu de ressources + variables partagées.
- **Server** : machine Linux accessible en SSH, avec son propre proxy.
- **Destination** : réseau Docker sur un serveur.
- Chaque ressource a un **UUID** utilisé comme nom de container/réseau/hostname interne.

---

## 3. Gestion des serveurs

### 3.1 Connexion et validation
- Tout serveur Linux joignable en SSH peut être ajouté (VPS, EC2, Raspberry Pi, machine locale). Architectures **AMD64 et ARM64**.
- Authentification **exclusivement par clé SSH** (sans passphrase, sans 2FA) ; les clés privées sont stockées chiffrées dans l'instance (« Private Keys »).
- Utilisateur root par défaut ; **utilisateur non-root expérimental** (exige `sudo NOPASSWD: ALL`).
- **Docker Engine ≥ 24** requis (snap non supporté).
- Bouton « Validate Server & Install Docker Engine » : vérifie la connectivité SSH, installe les dépendances (curl, wget, git, jq), installe/configure Docker, exécute des health checks (3 retries par étape).
- Timeout de connexion SSH configurable par serveur ; nom d'utilisateur SSH acceptant notamment les points.
- Serveur **localhost** pré-enregistré (la machine hébergeant l'instance), utilisable mais déconseillé pour la production.
- Variables d'environnement **partagées au niveau serveur**, héritables par les ressources qui y sont déployées.

### 3.2 Maintenance système et provisioning cloud (retiré — ADR-027)

Le server patching (APT/DNF/Zypper depuis le dashboard), les cloud provider tokens et le provisioning Hetzner sont **retirés du périmètre produit** (ADR-027, réévaluable sur demande avérée). La numérotation de section est conservée pour la stabilité des renvois. Sans rapport et toujours au périmètre : Hetzner/Cloudflare comme providers **DNS-01** (§4.3) et Hetzner comme provider **S3** (§7.2).

### 3.3 Multi-serveurs
- Chaque serveur est indépendant avec son propre proxy ; le trafic applicatif va directement au serveur cible (jamais via l'instance de contrôle). L'instance ne fait que UI + déploiements SSH + health monitoring.
- **Déploiement multi-serveurs d'une même app** (HA, expérimental) : même architecture requise + registry Docker externe (build → push → pull) ; load balancer externe à la charge de l'utilisateur.

### 3.4 Build servers
- Serveur dédié à la compilation (flag « Build Server ») pour décharger les serveurs de production.
- Prérequis : Docker Engine, accès au code source, **push obligatoire vers un container registry**, architecture identique aux serveurs de déploiement.
- Activation par application (« Use a Build Server? »). Sélection aléatoire si plusieurs build servers. Un build server ne peut pas héberger d'applications.

### 3.5 Docker Swarm (expérimental, déprécié)
- Swarm Manager (obligatoire) + workers ; registry externe obligatoire ; minimum recommandé 3 nœuds ; stockage persistant multi-nœuds non résolu. Non production-ready et annoncé comme déprécié pour la génération suivante.

### 3.6 Cloudflare Tunnels (retiré — ADR-027)
Retiré du périmètre produit (ADR-027, réévaluable sur demande avérée). La numérotation de section est conservée pour la stabilité des renvois. Cloudflare comme provider **DNS-01** (§4.3) n'est pas concerné.

### 3.7 Nettoyage disque automatisé
- « Automated Docker Cleanup » par serveur : déclenchement par **seuil d'usage disque** (%) et/ou **cron planifié** ; options opt-in pour purger volumes et réseaux inutilisés.
- Ne cible que les ressources gérées (containers stoppés, images inutilisées, build cache, anciennes helper images) ; jamais pendant un déploiement en cours.

### 3.8 Monitoring serveur — agent Sentinel (expérimental)
- Agent léger en Go déployé en container : CPU/RAM serveur et par container (~10 s), disque (~60 s) ; architecture **push** vers l'instance (endpoint + token) ; rétention et fréquence configurables ; API REST locale (`localhost:8888`).
- Graphiques historiques dans l'UI (serveur et par ressource). Limitation : pas de métriques pour les stacks Docker Compose / services one-click.

---

## 4. Proxy, domaines et SSL

### 4.1 Reverse proxy
- **Traefik** (défaut) et **Caddy** (expérimental) ; switch possible par serveur à tout moment (régénération des labels).
- Configuration **automatique** : la plateforme génère le routage des containers — plusieurs apps par serveur sans gestion manuelle des ports.
- Config proxy éditable par serveur dans l'UI + fichiers de dynamic config Traefik (`/var/lib/akerdock/proxy/dynamic`).
- **Cycle de vie du proxy** : start / stop / restart du proxy par serveur depuis l'UI, statut visible, logs du proxy consultables ; l'arrêt du proxy coupe tout le trafic entrant du serveur (avertissement explicite) ; notification si l'image du proxy est obsolète (cf. §11).
- Capacités via labels/middlewares : Basic Auth, rate limiting, IP whitelisting, custom headers, load balancing, dashboard Traefik.

### 4.2 Domaines
- Formats supportés par application : FQDN simple, **multi-domaines** (virgules), **domaine:port** (routage vers un port interne précis), **path-based routing** (priorité au path le plus spécifique).
- **Wildcard domain par serveur** : les nouvelles apps reçoivent `<uuid>.example.com` automatiquement (fallback : domaines **sslip.io**).
- Redirection **www/non-www** native (« Direction » : both / to-www / to-non-www).
- Validation DNS via 1.1.1.1 (DNS de validation customisable).

### 4.3 SSL/TLS
- Certificats **Let's Encrypt automatiques** (émission + renouvellement, HTTP-01 par défaut) ; fallback certificat self-signed si l'émission échoue.
- **Certificats wildcard** via DNS-01 challenge (providers DNS supportés par Lego : Cloudflare, Route 53, OVH, Hetzner…).
- **Certificats custom** : dépôt dans `/var/lib/akerdock/proxy/certs` + dynamic config.
- Option **Force HTTPS** par application.

---

## 5. Applications

### 5.1 Sources de déploiement
| Source | Description |
|---|---|
| Public Git Repository | URL HTTPS d'un repo public (GitHub, GitLab, Bitbucket, Gitea, autres) |
| Private Repo — GitHub App | Intégration officielle : discovery des repos, auto-deploy on push, preview deployments, commentaires de statut sur PR ; GitHub Enterprise supporté |
| Private Repo — Deploy Key | Clé SSH (générée ou importée) déposée comme deploy key ; GitHub/GitLab/Bitbucket/Gitea ; auto-deploy via webhooks manuels |
| Dockerfile | Dockerfile inline ou du repo |
| Docker Compose | Fichier compose du repo comme définition multi-services |
| Docker Image | Image pré-construite depuis un registry (Docker Hub, GHCR, GitLab Registry, custom) ; registries privés via `docker login` sur le serveur |

- Fonctions Git annexes : sélection de branche, **base directory** (monorepos), git **submodules**, git **LFS**, shallow clone.
- Pattern CI externe : build en GitHub Actions → push registry → appel du deploy webhook AkerDock (pull + redeploy sans rebuild).

### 5.2 Build packs
| Build pack | Rôle |
|---|---|
| **Nixpacks** (défaut) | Auto-détection langage/framework, génération du Dockerfile ; override install/build/start ; `nixpacks.toml` ; mode static (Nginx, publish directory) |
| **Railpack** (bêta) | Successeur de Nixpacks : images plus petites, meilleur caching BuildKit ; supporte déploiements réguliers, preview et static |
| **Static** | Fichiers pré-buildés servis par **Nginx** (config nginx éditable) ; option SPA |
| **Dockerfile** | Contrôle total ; build args auto-injectés (désactivable) ; `SOURCE_COMMIT` opt-in |
| **Docker Compose** | Le fichier compose est la source de vérité (env, storage, network) ; domaine par service ; réseau bridge isolé par UUID ; extensions `x-akerdock` (`is_directory`, `content`, `exclude_from_hc`) ; mode « raw compose » avancé |

- **Push post-build vers registry** (champs image + tag) : requis pour Swarm et build servers ; tag custom + tag SHA du commit.

### 5.3 Configuration d'une application
- **Ports** : « Ports Exposes » (port interne utilisé par le proxy, optionnel pour une application sans trafic entrant) et « Ports Mappings » (mapping hôte optionnel, hors proxy, protocoles TCP/UDP/SCTP et binding IP supportés).
- **Health checks** : path, port, méthode, interval/timeout/retries/start period (requiert curl/wget dans le container) ; `HEALTHCHECK` Dockerfile prioritaire ; conditionne le routage Traefik et les rolling updates.
- **Resource limits** : memory limit/reservation/swap/swappiness, CPU limit/sets/shares.
- **Custom Docker options** : options `docker run` arbitraires (`--cap-add`, `--gpus`, `--ulimit`…).
- **Custom labels** : labels containers éditables (labels proxy régénérables) ; labels système injectés (`akerdock.managed=true`, `akerdock.resource_uuid`, `akerdock.type`).
- **Commandes pre/post-deployment** : pre = dans le container existant avant déploiement ; post = dans le nouveau container après (échec post = déploiement échoué, sans rollback auto).
- **Arrêt et redémarrage** : délai de grâce d'arrêt (`stop grace period`) et plafond de boucles de redémarrage configurables.
- **Stockage persistant** : cf. §8.

### 5.4 Variables d'environnement
- Flags **build-time** (`ARG` / `--env-file`, stockées hors image) et **runtime** (`.env` + `env_file`) par variable.
- **Docker Build Secrets** (BuildKit `--secret`) en option pour ne pas fuiter les secrets dans les metadata d'image.
- Types spéciaux : **multiline** (clés, certificats), **literal** (pas d'interpolation), **locked** (masquée, non rééditable).
- Deux vues : Normal (cartes) et **Developer** (éditeur `.env` bulk).
- **Shared variables** hiérarchiques : `{{team.VAR}}`, `{{project.VAR}}`, `{{environment.VAR}}`, complétées par les variables partagées du serveur cible.
- Variables prédéfinies : `AKERDOCK_FQDN`, `AKERDOCK_URL`, `AKERDOCK_BRANCH`, `AKERDOCK_RESOURCE_UUID`, `AKERDOCK_CONTAINER_NAME`, `SOURCE_COMMIT`, `PORT`, `HOST`, `AKERDOCK_PR_ID` (ADR-022).
- **Magic variables** (compose/services) : `SERVICE_<TYPE>_<ID>` — `URL`, `FQDN`, `USER`, `PASSWORD(_64)`, `PASSWORDWITHSYMBOLS(_64)`, `BASE64_32/64/128`, `REALBASE64_*`, `HEX_*`. Générées par la plateforme, persistantes entre redéploiements, partagées entre services du stack, éditables en UI.
- Variables requises : syntaxe `${VAR:?}` (bloque le déploiement si vide).

### 5.5 Cycle de déploiement
- **Auto-deploy on push** : GitHub App ou webhooks manuels (GitHub/GitLab/Bitbucket/Gitea, avec secret et validation de signature).
- Les commits contenant les marqueurs `[skip ci]` ou `[skip cd]` ne déclenchent pas d'auto-déploiement.
- **Watch paths** : patterns de chemins par application limitant l'auto-deploy aux pushes modifiant certains fichiers (essentiel en monorepo) ; limitation connue : non appliqués aux preview deployments (toute PR ouverte/mise à jour déploie).
- Toggle **« Auto Deploy »** désactivable par application : les événements webhook sont alors ignorés pour cette ressource (le deploy webhook manuel/API reste utilisable).
- **Deploy webhook / API** : `GET|POST /api/v1/deploy?uuid=…&force=…` (multi-uuid, deploy par tag, force = build sans cache), auth Bearer.
- **File de déploiement par serveur** : `concurrent_builds` (défaut 2) + `deployment_queue_limit` (défaut 25) ; vue des déploiements en cours/en attente ; annulation.
- **Zero-downtime / rolling update** : nouveau container démarré à côté de l'ancien → health check OK → bascule du trafic → arrêt de l'ancien. Conditions : health check passant, noms de containers par défaut, pas de Docker Compose, pas de port mapping hôte.
- **Rollback** : vers une image locale précédente encore présente sur le serveur.
- **Historique** : liste des déploiements (queued / in progress / finished / failed), build logs en temps réel, annulation.
- **Diff de configuration** : mémorisation et présentation des changements de configuration applicative inclus dans un redéploiement, en plus du SHA Git.

### 5.6 Preview deployments (déploiements de PR / MR)

Environnement éphémère déployé automatiquement **pour chaque pull request** (GitHub) ou merge request (GitLab).

- **Prérequis** : intégration **GitHub App** (recommandée) ou webhooks manuels ; **wildcard DNS** (`A` record `*.domaine` vers l'IP du serveur).
- **Déclenchement** : ouverture d'une PR → build + déploiement d'une instance séparée ; **redeploy automatique à chaque nouveau commit** de la PR. Côté GitLab : event « Merge request » du webhook manuel. Les PR déjà ouvertes avant l'activation de la feature nécessitent un déploiement manuel depuis le dashboard.
- **URL par PR** : template configurable, ex. `{{pr_id}}.{{domain}}` ; placeholder `{{random}}` pour un sous-domaine aléatoire à chaque déploiement.
- **Variables d'environnement séparées** : jeu de variables dédié aux previews — les secrets de production ne fuient jamais vers les PR (y compris celles de contributeurs externes). Variables prédéfinies injectées : `AKERDOCK_PR_ID` (numéro de la PR), `AKERDOCK_URL` / `SERVICE_URL_<ID>` (URL de la preview, y compris pour les stacks compose).
- **Feedback sur la PR** (GitHub App) : commentaires de statut automatiques avec le lien de la preview à chaque déploiement.
- **Scoped deployments** : par défaut, seules les PR des members/collaborators/contributors du repo déclenchent une preview ; toggle opt-in pour autoriser les PR publiques (projets open source).
- Les pull requests provenant d'un fork sont ignorées par défaut afin de ne pas exposer les secrets et capacités du runner à du code non fiable.
- **Cleanup automatique** : l'environnement de preview est détruit à la fermeture ou au merge de la PR.
- Suppression manuelle possible, y compris via API par identifiant de PR.
- **Limitations** : pas de plafond natif sur le nombre de previews simultanées ; supporte les build packs réguliers, Railpack et static.

### 5.7 Exploitation
- **Lifecycle** : Deploy / Redeploy, Start, Stop, Restart, force rebuild sans cache — en UI et API.
- **Logs runtime** : streaming des logs containers par ressource (et par service d'un stack), nombre de lignes configurable.
- Recherche, sections repliables et téléchargement des logs ; timestamps alignés sur le fuseau du serveur cible ; rendu HTML neutralisé.
- **Terminal web** (xterm.js) : shell dans tout container ou serveur géré, via WebSocket → SSH ; reconnexion, scrollback.
- **Scheduled tasks** : crons par application/service (nom, commande, expression cron ou alias `daily`/`hourly`/…, container cible dans un stack) ; exécution par `docker exec` ; historique des exécutions + notifications.
- **Statut** : état des containers (running/exited, healthy/unhealthy) au niveau app et par service.
- **Clonage de ressource** : duplication d'une ressource vers un autre projet, environnement ou serveur/destination — copie de la configuration (source, variables, storage déclaré), **pas des données de volumes** ; déplacement possible entre environnements ; pas de transfert inter-team (frontière de sécurité, demande communautaire récurrente).

---

## 6. Bases de données managées

### 6.1 Moteurs one-click
**PostgreSQL, MySQL, MariaDB, MongoDB, Redis, KeyDB, Dragonfly, ClickHouse.** (Tout autre moteur reste déployable en image/compose, sans les fonctions managées.)

### 6.2 Fonctions communes
- **Credentials auto-générés** (mots de passe 64 caractères) ; champs adaptés par moteur.
- **Internal URL** (hostname = UUID de la ressource sur le réseau Docker) et **External URL** (si accès public activé).
- **Accès public** : ports mapping Docker (permanent, restart requis) ou **proxy TCP Nginx dynamique** (« Accessible over the internet », port public modifiable sans redémarrer la base, timeout configurable, défaut 3600 s).
- **Configs custom** par moteur : `postgres_conf` + `initdb args` + init scripts (`/docker-entrypoint-initdb.d/`), `mysql_conf`, `mariadb_conf`, `mongo_conf`, `redis_conf`, `keydb_conf` (pas de config custom pour Dragonfly/ClickHouse).
- **Image/tag libre**, custom docker run options, resource limits, health checks configurables, log drain, volumes et file mounts.
- **Lifecycle** : start/stop/restart + statut, compteurs de restart, `last_online_at`.

### 6.3 SSL bases de données
- « Enable SSL » + mode par moteur (PostgreSQL : allow/prefer/require/verify-ca/verify-full ; MySQL/MongoDB : prefer→verify-full ; MariaDB/Redis/KeyDB/Dragonfly : on/off ; ClickHouse : non supporté).
- **CA gérée par la plateforme** (visualisation/régénération dans l'UI), montable dans les containers clients ; CA custom possible.

---

## 7. Backups

### 7.1 Backups de bases planifiés
- Moteurs supportés : **PostgreSQL (`pg_dump`/`pg_dumpall`), MySQL (`mysqldump`), MariaDB (`mariadb-dump`), MongoDB (`mongodump --gzip`)** ; option « dump all databases » ; sélection de bases (liste), exclusion de collections (MongoDB).
- **Les bases internes des services one-click sont aussi backupables** (détection par image : postgres/mysql/mariadb/mongo).
- **L'instance elle-même** backupe sa propre base PostgreSQL par le même mécanisme.
- **Planification** : expressions cron + alias (`every_minute`, `hourly`, `daily`, `weekly`, `monthly`, `yearly`) ; timeout configurable (défaut 3600 s) ; bouton « Backup Now ».

### 7.2 Destinations et rétention
- **Local** (`/var/lib/akerdock/backups/...`) et/ou **S3** (upload via client MinIO `mc`) ; option « S3 only » (suppression du fichier local après upload).
- **Rétention séparée local / S3**, trois règles cumulatives : nombre max de backups, ancienneté max (jours), taille totale max (GB) ; 0 = illimité.
- Notifications succès/échec (y compris « succès local mais échec S3 »).

### 7.3 Restore
- **Import Backups** par instance de base : upload direct (drag & drop), fichier déjà sur le serveur, ou depuis un S3 configuré ; commandes de restore par défaut personnalisables (`pg_restore`, `mysql`, `mariadb`, `mongorestore`).
- Chaque exécution de backup est tracée (statut, fichier, taille, upload S3) ; download / delete depuis l'UI.

### 7.4 S3 Storages (ressource de configuration)
- Endpoint, bucket, région, access key / secret (chiffrés en base), path-style ; **vérification obligatoire** (`ListObjectsV2`) avant usage ; flag d'utilisabilité + alerte si le storage devient inutilisable.
- Providers testés : AWS S3, Cloudflare R2, DigitalOcean Spaces, MinIO, Backblaze B2, Scaleway, Hetzner, Wasabi, Supabase Storage…

### 7.5 Backup/restore de l'instance
- Tout l'état = `/var/lib/akerdock` (config, clés SSH, proxy) + base PostgreSQL interne ; backup planifié de la base avec upload S3 ; procédure de restore documentée (clé maître, clés SSH, `pg_restore`).

---

## 8. Stockage persistant

- **Volume Docker nommé** : nom + mount path ; le nom est préfixé par l'UUID de la ressource (anti-collision).
- **Bind mount** (répertoire hôte → container).
- **File mount** : fichier individuel dont le **contenu est éditable dans l'UI** (chown/chmod, conversion fichier↔répertoire, rechargement du contenu depuis le serveur).
- Extensions compose : `is_directory: true` (création du répertoire hôte), `content: |` (création du fichier avec interpolation des variables d'env) ; `configs` top-level supporté.
- Avertissement produit : partage d'un volume entre containers déconseillé (locking).

---

## 9. Services one-click

- **Catalogue de 280+ services one-click** docker-compose (la documentation d'introduction conserve parfois la mention plus prudente « 200+ ») : WordPress, Ghost, Directus, Strapi (CMS) ; Plausible, PostHog, Umami, Metabase (analytics) ; **Supabase, Appwrite**, PocketBase, GitLab, Gitea (dev) ; **n8n**, ActivePieces (automation) ; **MinIO**, Nextcloud (storage) ; Ollama, Open WebUI, Langfuse, Qdrant, Weaviate (IA) ; Authentik, Keycloak, Vaultwarden (sécurité) ; Grafana, Uptime Kuma (monitoring) ; Elasticsearch, Meilisearch, Typesense (search) ; Immich, Jellyfin, Cal.com, Odoo, Home Assistant…
- **Anatomie d'un template** : fichier compose standard + métadonnées en commentaires (`documentation`, `slogan`, `category`, `tags`, `logo`, `port`) ; compilés en un JSON de catalogue livré avec les releases (rafraîchissable depuis GitHub). Critère d'admission : repo ≥ 1000 stars.
- **Magic variables** (`SERVICE_FQDN_*`, `SERVICE_PASSWORD_*`, etc.) pour l'auto-configuration : URLs, credentials générés et partagés entre les services du stack.
- **Gestion d'un service déployé** : Deploy / Stop / Restart / « Pull latest images & restart » ; restart **par sous-container** ; éditeur du compose dans l'UI ; domaine par sous-service ; env vars ; storage/file storage ; scheduled tasks ; **backups des bases internes** ; logs par container ; `exclude_from_hc` pour les jobs one-shot.
- **Réseau** : stack isolé dans un réseau nommé par UUID ; « Connect to Predefined Network » pour la communication inter-stacks ; services sans domaine/port = privés (DNS interne).

---

## 10. Organisation, auth et sécurité

### 10.1 Équipes et rôles
- Multi-teams ; membres invités par email ou créés par un admin.
- Rôles : **root/owner d'instance** (premier utilisateur : accès global, updates, settings), puis **admin** et **member** par team. Granularité limitée (pas de RBAC par projet/ressource — demande communautaire récurrente).
- **Suppression d'utilisateur** : procédure documentée (self-service ou par le root) ; les teams dont il est seul membre et leurs ressources doivent être traitées explicitement avant suppression — jamais de cascade silencieuse.

### 10.2 Authentification
- Email/mot de passe, inscription publique désactivable, **2FA TOTP**.
- **Réinitialisation de mot de passe** par email (« forgot password ») : requiert l'email transactionnel de l'instance configuré (cf. §14.2) ; sinon reset manuel par le root.
- **OAuth dashboard** : Azure, Bitbucket, GitHub, GitLab et Google.
- **SSO OpenID Connect** : configuration d'un IdP OIDC générique (Okta documenté ; IdP compatibles possibles selon leur conformité). SAML natif non documenté.
- Bootstrap non interactif du premier root user par variables d'environnement, avec validation stricte de l'email, du nom et du mot de passe.

### 10.3 API tokens
- API désactivée par défaut (activation dans les settings) ; tokens à **permissions granulaires** : `read`, `read:sensitive`, `write`, `deploy`, `root` ; hashés SHA-256, affichés une seule fois, expiration, **IP allowlist (CIDR)**, scopés par team ; rate limit 200 req/min.

### 10.4 Surface réseau
- Ports plateforme : 8000 (dashboard), 6001 (WebSocket), 6002 (terminal), 22 (SSH), 80/443 (proxy). 8000/6001/6002 fermables derrière un domaine proxifié.
- Avertissement produit : Docker bypasse UFW (préférer le firewall du cloud provider) ; le hardening OS reste à la charge de l'utilisateur.
- Accès terminal aux serveurs et containers contrôlable au niveau instance/team et réservé aux rôles autorisés ; toute session doit être authentifiée, auditée et bornée à la team active.

---

## 11. Notifications

- **Canaux** : Email (SMTP ou Resend), Discord, Telegram, Slack (compatible Mattermost), Pushover, webhooks custom.
- **Événements** (activables individuellement **par canal**) : déploiement réussi/échoué, changement de statut de container (app arrêtée/unhealthy), **preview créée / mise à jour / bientôt expirée / détruite** (`application.preview.created|updated|expiring|deleted.v1`), backup réussi/échoué, scheduled task réussie/échouée, statut du Docker cleanup, seuil d'usage disque, **serveur injoignable / de nouveau joignable**, mises à jour disponibles, proxy obsolète.

---

## 12. API, CLI et automatisation

- **REST API** `/api/v1` (OpenAPI 3.1, Bearer) : CRUD applications, databases (+ backups), services, servers (+ validation, domaines et ressources), projects/environments, teams, GitHub Apps, private keys, variables d'env (dont bulk), deployments (trigger/liste/logs/cancel), deploy par UUID/tag ; liste transverse des ressources ; endpoints système (healthcheck non authentifié, version, enable/disable de l'API).
- **Webhooks entrants** : endpoints dédiés GitHub/GitLab/Bitbucket/Gitea (signature vérifiée, auto-deploy, previews) + deploy webhook générique par ressource pour CI custom.
- **CLI officielle** (Go, binaire unique, Cobra — ADR-033) : multi-instances (contextes), gestion servers/projects/resources/deployments (streaming des logs), domaines, clés, databases et backups. **v1 « debug »** (spec `docs/specs/cli.md`, ADR-031/032) : `login` par navigateur sans ouvrir de port (poll+code+PKCE, SSO compris), listing, logs (snapshot et `-f`), shell dans un container, **port-forward TCP** vers une ressource sans l'exposer, console typée ; le client ne parle qu'au manager sur 80/443 et traverse proxy/LB. Le déploiement depuis le poste (`akerdock up`, §27.18, ADR-018) relève de v2.
- **Serveur MCP intégré** : activation au niveau instance, transport Streamable HTTP sur `/mcp`, authentification par token API, scoping par team et 10 outils read-only (`overview`, list/get servers, projects, applications, databases et services), pagination 50 par défaut/100 maximum. Les opérations d'écriture ne font pas partie de v4.1.2.
- **Terraform** : providers communautaires uniquement (pas d'officiel).

---

## 13. Observabilité

- Build logs temps réel ; logs applicatifs par container ; terminal web.
- **Log drains** par serveur puis par ressource : Axiom, New Relic, config **Fluent Bit custom**.
- Métriques CPU/RAM serveur + containers (Sentinel) avec historique.
- Canal d'**audit structuré** pour les requêtes API et événements webhook ; corrélation avec acteur/token, team, cible, résultat et identifiant de requête, sans journaliser les secrets. Ce goulot d'audit est aussi le point d'instrumentation OTLP : chaque action émet un compteur `akerdock.actions.total{action, actor, result}` et un span-event sur la trace active — traces, métriques et logs étant activables/désactivables signal par signal dans la config d'instance (§14.2, ADR-008). Les jobs (déploiements, backups, cleanup, sync Git, notifications…) portent chacun leur span + métrique de durée/issue, et chaque requête API sa propre span serveur.
- Health checks applicatifs ; surveillance de joignabilité des serveurs avec notifications ; alertes disque.
- Pas d'APM. L'**uptime monitoring intégré** est décidé par ADR-017 : checks HTTP/TCP simples exécutés hors du workload, historique et alerting via les canaux existants — le périmètre s'arrête au up/down et à la latence (Uptime Kuma & co restent disponibles en one-click pour les besoins avancés).

---

## 14. La plateforme elle-même

### 14.1 Installation
- Script d'installation (`install.sh`) : vérifie les prérequis (Docker ≥ 24, Compose v2), génère la clé maître et le `.env`, construit l'image et démarre la stack via Docker Compose. Dashboard et API sur le **port unique** du control plane (ADR-021).
- OS : Debian/Ubuntu (LTS pour le script), RHEL-like, SLES, Arch, Alpine, Raspberry Pi OS 64-bit. **Minimum : 2 vCPU, 2 GB RAM, 30 GB disque.**
- Paramètres avancés : plage CIDR Docker custom, registry d'installation custom, `docker-compose.custom.yml` persistant à travers les upgrades.

### 14.2 Paramètres d'instance
- **FQDN de l'instance** : dashboard servi derrière le proxy avec certificat automatique, permettant de fermer les ports directs 8000/6001/6002.
- **Timezone de l'instance** configurable (affichage et crons de maintenance de la plateforme).
- Inscription publique on/off, API on/off (cf. §10), serveur DNS de validation custom (cf. §4.2).
- **Email transactionnel de l'instance** (SMTP ou Resend) : invitations, réinitialisation de mot de passe, email de test ; les teams peuvent réutiliser cette configuration système pour leurs notifications au lieu d'un SMTP propre.
- **Export OTLP distant** (ADR-008/§27.8) : endpoint, protocole (HTTP/gRPC), en-têtes d'auth (chiffrés au repos) et choix des signaux (traces, métriques, logs) vers un collector OpenTelemetry ; configuré ici, chiffré, appliqué au prochain redémarrage du binaire. À défaut, repli sur les variables `OTEL_*`.
- **Onboarding guidé** au premier démarrage : création du root user, première team, premier serveur (localhost ou distant) et première ressource.

### 14.3 Mises à jour
- **Auto-update** (vérification périodique du CDN, désactivable), update semi-automatique (bouton, réservé au root user), ou manuelle (script).
- Cron d'auto-update configurable ; upgrade/downgrade vers une version explicite ; procédure séparée d'upgrade/rollback du PostgreSQL interne.
- Désinstallation documentée et destructive uniquement après confirmation ; les migrations de ressources/volumes entre serveurs sont des opérations explicites, distinctes du backup du control plane.

### 14.4 Modèle économique
- **Self-hosted : gratuit et complet.** Aucune feature paywallée, aucune édition « entreprise » : ce que fait le produit est dans le binaire que l'on héberge (licence Apache 2.0, ADR-020).
- Un éventuel control plane managé resterait un **service d'hébergement** du même binaire, sans capacité réservée — c'est un non-objectif du périmètre courant (§16.2).

---

## 15. Pièges structurels traités par conception

Les limitations qui font échouer un PaaS de containers en production sont connues. Elles sont traitées **par conception** dans AkerDock, et chacune est prouvée par un test :

- **Zero-downtime des stacks compose** : bascule par service derrière le proxy (ADR-015) — pas seulement pour les applications à container unique.
- **Resource limits réellement appliquées** aux ressources compose (cgroups vérifiés en E2E), jamais déclaratives sans effet.
- **Rollback par artifact vérifié** (digest OCI, ADR-006) — jamais « l'image encore présente localement, si elle y est ».
- **Plafond et TTL des previews** (§20.4.3) : une PR ouverte ne peut pas consommer un serveur sans borne.
- **Watch paths appliqués aux previews** aussi (§20.4.5) : en monorepo, seule l'application affectée redéploie.
- **RBAC fin** par projet et environnement (ADR-007), pas un simple couple admin/member.
- **Restore drills** (ADR-014) : un backup qui n'a jamais été restauré n'est pas un backup.
- **Notifications routées et agrégées** (ADR-019) : un serveur qui flappe ne produit pas des dizaines d'alertes.

---

## 16. Objectifs produit de AkerDock

> Les sections 16 à 28 transforment le périmètre fonctionnel (§1–14) en **exigences vérifiables** : chacune doit pouvoir être prouvée par un test, une fixture ou un runbook.

### 16.1 Objectifs

1. Permettre à une équipe de déployer et exploiter une application containerisée sur un serveur vierge en moins de 15 minutes, sans écrire de pipeline CI/CD.
2. Offrir un control plane self-hosted qui ne se trouve jamais dans le chemin des requêtes applicatives.
3. Garantir que toutes les ressources restent des objets Docker, Compose, réseau, volume et fichiers standards, administrables hors de AkerDock.
4. Livrer un cœur fonctionnel complet et prouvé (§26) avant d'ajouter de la surface : une capacité n'est « livrée » qu'avec sa documentation, ses migrations, son audit et son test.
5. Concevoir les modules comme des capacités remplaçables : moteur de build, scheduler, proxy, secret store, transport distant, métriques et catalogue de services.
6. Rester léger et simple à opérer : un binaire Go unique + PostgreSQL (pas de Redis ni de runtime applicatif), un seul port exposé pour le control plane, et un dashboard qui reste réactif sur un VPS modeste (2 vCPU / 2 GB). Cette empreinte est un engagement produit, mesuré en CI, pas un effet de bord.

### 16.2 Non-objectifs initiaux

- Devenir un orchestrateur généraliste équivalent à Kubernetes.
- Fournir du stockage distribué ou un load balancer global propriétaire.
- Offrir du billing, du support commercial ou l'exploitation d'un service managé.
- Réimplémenter Nixpacks, Railpack, Docker, BuildKit, Traefik ou Caddy ; AkerDock les orchestre.
- Importer le schéma interne d'une autre plateforme. Le chemin d'entrée est l'**adoption de ressources Docker existantes** (§20.7, ADR-013) : ce qui tourne déjà est repris tel quel, sans dépendre du format de qui l'a créé.

### 16.3 Acteurs

| Acteur | Besoin principal | Droits attendus |
|---|---|---|
| Root de l'instance | Installer, mettre à jour, sécuriser et diagnostiquer le control plane | Toutes teams et réglages instance |
| Owner/Admin de team | Administrer membres, serveurs, sources, secrets et ressources de sa team | Lecture/écriture/deploy dans sa team |
| Member/Développeur | Configurer et déployer les ressources autorisées | Selon politique de team, jamais inter-team |
| Opérateur/SRE | Observer, redémarrer, rollback, backup/restore, terminal | Accès opérationnel explicite et audité |
| Pipeline CI | Déclencher un déploiement et en lire le résultat | Token `deploy` minimal |
| Intégration read-only/MCP | Inventorier l'infrastructure | Token `read`, secrets masqués |
| Serveur cible | Exécuter builds, workloads, proxy et agents | Confiance limitée à son périmètre serveur |
| Fournisseur Git/Cloud/S3 | Émettre des événements ou exécuter une action demandée | Credentials minimaux, rotation possible |

### 16.4 Indicateurs de succès proposés

- Taux de déploiements réussis hors erreur applicative ≥ 99 %.
- Aucun chevauchement inter-team dans les tests d'autorisation et d'isolation.
- Reprise d'un worker après crash sans double bascule de trafic ni perte d'un job accepté.
- 95e percentile de réponse API de lecture < 300 ms hors SSH/fournisseurs externes, à 50 utilisateurs concurrents.
- Événement webhook accepté en < 500 ms puis traité asynchronement.
- RPO du control plane ≤ 24 h avec backup quotidien ; RTO documenté ≤ 2 h sur une installation standard.
- Toutes les opérations destructives, d'accès secret et de terminal produisent un audit exploitable.

## 17. Invariants fonctionnels obligatoires

Chaque invariant doit posséder au moins un test automatisé de niveau API ou intégration.

| ID | Exigence |
|---|---|
| INV-001 | Toute ressource appartient à exactement une team, directement ou par sa chaîne Project → Environment. |
| INV-002 | Une requête ne peut référencer une clé, source, destination, storage, serveur ou ressource d'une autre team, même avec un UUID valide. |
| INV-003 | Un secret n'est jamais renvoyé sans permission `read:sensitive`, ni écrit dans les logs, événements ou messages d'erreur. |
| INV-004 | Une opération distante est idempotente ou porte une clé d'idempotence et un mécanisme de détection/réconciliation. |
| INV-005 | Une application saine existante reste routée tant que sa remplaçante n'a pas satisfait les conditions de bascule. |
| INV-006 | L'échec d'un déploiement ne supprime ni volume persistant ni dernier container sain. |
| INV-007 | Le control plane ne proxyfie pas le trafic applicatif ; sa panne n'arrête pas les workloads déjà actifs. |
| INV-008 | La suppression d'un objet logique exige la vérification de ses dépendances et sépare clairement « retirer de AkerDock » de « supprimer les données ». |
| INV-009 | Un webhook est authentifié, associé exactement au bon dépôt et dédupliqué avant de déclencher un déploiement. |
| INV-010 | Une PR non fiable ou issue d'un fork n'obtient aucun secret de production et ne déclenche rien sans politique explicite. |
| INV-011 | Les noms Docker générés sont déterministes, non conflictuels et rattachables à un UUID interne stable. |
| INV-012 | Toute commande shell construite depuis une entrée utilisateur est passée comme arguments typés ou échappée avec une bibliothèque centralisée et testée. |
| INV-013 | Un job accepté survit au redémarrage du processus et ne reste pas indéfiniment `in_progress` sans heartbeat/lease. |
| INV-014 | Les changements de configuration sont versionnés suffisamment pour expliquer et reproduire chaque déploiement. |
| INV-015 | Les ressources découvertes sur un serveur sont distinguées des ressources gérées ; le cleanup ne détruit jamais un objet non géré ou persistant. |

## 18. Frontières du système et architecture cible en Go

### 18.1 Composants logiques

```text
Navigateur / CLI / API / MCP / Webhooks
                  │
          API + Auth + Policy
                  │
        Services métier / Postgres
          │          │          │
      Job queue   Event bus   Realtime hub
          │                     │
       Workers ───────────── logs/états
          │
  SSH/agent ou provider API
          │
Serveurs cibles : Docker/BuildKit + Proxy + Sentinel
```

- **API/control plane** : HTTP, UI, auth, validation, politiques, persistance, OpenAPI et MCP.
- **Workers** : déploiements, validation serveur, backups, tâches planifiées, cleanup, notifications, synchronisation Git et maintenance.
- **Realtime hub** : progression des jobs, logs de build/runtime et terminal. Il ne constitue pas la source de vérité.
- **PostgreSQL** : configuration, états désirés/observés, historique, audits, leases et outbox.
- **Queue** : queue durable en PostgreSQL (décision §27.2). L'interface reste abstraite dans le code, mais aucun bus externe n'est planifié.
- **Transport distant** : interface abstraite avec implémentation SSH initiale. Un agent sortant optionnel pourra être ajouté sans modifier les services métier.
- **Runtime cible** : Docker Engine/Compose/BuildKit (décision §27.4 : Docker standalone confirmé, Kubernetes écarté). Tous les appels passent par un adaptateur runtime unique, instrumenté et sécurisé — c'est ce contrat qui permettrait d'évaluer un autre orchestrateur plus tard sans toucher aux services métier.
- **Proxy provider** : contrat commun Traefik/Caddy ; génération de configuration, validation, application atomique et rollback.

### 18.2 Recommandation de packaging

- Démarrer en **monolithe modulaire Go** avec binaires ou modes `api`, `worker`, `scheduler` et `all-in-one` issus du même dépôt.
- Interdire les dépendances circulaires entre domaines ; exposer des interfaces pour Git, SSH, Docker, proxy, registry, secret store, object storage et notification.
- Utiliser PostgreSQL comme source de vérité et le pattern **transactional outbox** pour publier les événements après commit.
- Prévoir le multi-instance dès le schéma : jobs avec lease, verrous distribués par ressource/serveur, migrations compatibles rolling upgrade.
- Garder l'UI découplée de l'orchestration : toute action produit importante doit avoir un contrat API stable.

### 18.3 Sources de vérité

| Donnée | Source autoritative | Réconciliation |
|---|---|---|
| Configuration désirée | PostgreSQL | Version + diff par mutation |
| État container/réseau/volume | Docker du serveur cible | Polling + événement/agent |
| Code source | Fournisseur Git au SHA résolu | SHA immuable conservé avec le déploiement |
| Image déployée | Digest OCI, pas seulement le tag | Résolution du digest avant bascule |
| Secrets | Secret store chiffré | Référence/version, jamais valeur dans événements |
| Routage | Fichier/labels proxy sur le serveur | Génération déterministe + validation + checksum |
| Job | Queue durable + historique PostgreSQL | Lease, heartbeat, retry et dead-letter |

## 19. Modèle de données logique

### 19.1 Entités principales

| Agrégat | Entités et relations clés |
|---|---|
| Identité | `User`, `Identity`, `MFAFactor`, `Session`, `Team`, `TeamMembership`, `Invitation`, `APIToken` |
| Organisation | `Project` 1—N `Environment`; `Environment` 1—N `Resource`; `Tag` N—N ressources |
| Infrastructure | `Server`, `Destination`, `PrivateKey`, `CloudCredential`, `RegistryCredential`, `S3Storage` |
| Source | `GitSource`, `GitHubApp`, `Repository`, `WebhookEndpoint`, `WebhookDelivery` |
| Application | `Application`, `BuildConfig`, `RuntimeConfig`, `Domain`, `EnvironmentVariable`, `PersistentStorage`, `HealthCheck` |
| Service/DB | `Service`, `ServiceComponent`, `Database`, `DatabaseCredential`, `DatabaseBackupPlan`, `BackupExecution` |
| Exécution | `Deployment`, `DeploymentStep`, `DeploymentArtifact`, `ScheduledTask`, `TaskExecution`, `TerminalSession` |
| Plateforme | `ProxyConfigRevision`, `NotificationChannel`, `NotificationRule`, `AuditEvent`, `OutboxEvent`, `FeatureFlag` |

`Resource` est une union logique (`Application | Database | Service`) avec les champs communs : UUID, team, environnement, destination, nom, description, statut désiré/observé, timestamps et politique de suppression.

### 19.2 Contraintes et cycle de vie

- UUID publics aléatoires, non séquentiels ; identifiants internes séparés si nécessaire.
- Unicité des slugs Project/Environment dans leur parent et des noms Docker dans leur destination.
- Suppression Project/Environment interdite tant qu'elle contient des ressources, sauf opération cascade explicitement prévisualisée et confirmée.
- Suppression d'une clé, source, destination ou storage interdite tant qu'elle est référencée.
- Secrets chiffrés par enveloppe avec version de clé ; rotation sans réécriture bloquante de toute la base.
- `created_at`, `updated_at`, `deleted_at`, `created_by`, `updated_by` et numéro de version optimiste sur les agrégats mutables.
- Historique de déploiement, audit et exécutions de backup soumis à une rétention configurable ; aucune cascade accidentelle depuis un utilisateur supprimé.
- Les statuts observés ont un `observed_at` : au-delà d'un seuil, l'UI indique « inconnu/stale », jamais un faux `running`.

## 20. Workflows critiques et critères d'acceptation

### 20.1 Onboarding d'un serveur

1. L'admin choisit une team, une clé et saisit host, port, user et timeout.
2. L'API valide syntaxe, unicité et appartenance des références, puis crée le serveur en `pending`.
3. Un worker teste host key/politique SSH, connexion, sudo, OS/architecture, espace disque, Docker/Compose et ports.
4. Si autorisé, le worker installe ou met à niveau les prérequis et crée le réseau/dossiers/helper containers.
5. Il déploie et vérifie proxy et Sentinel selon les options, puis passe le serveur à `ready`. Le proxy n'est concerné que si son intention est `running` : un serveur est créé avec l'intention `stopped`, et le **premier démarrage du proxy est un acte explicite de l'opérateur** (revue des réglages — ports, wildcard, email ACME — puis Start), jamais un effet de bord de la validation.
6. Chaque étape est rejouable, loguée, annulable entre deux mutations et assortie d'une instruction de remédiation.

**Acceptation** : mauvais host key, clé d'une autre team, Docker Snap, architecture inconnue, sudo interactif, disque insuffisant et timeout produisent chacun une erreur distincte sans serveur faussement `ready`.

### 20.2 Création et déploiement d'une application Git

1. Sélection Project/Environment/Destination, source, dépôt, branche et build pack.
2. Validation de l'accès Git et résolution de la branche en SHA immuable.
3. Snapshot versionné de la configuration et création du `Deployment` en `queued`.
4. Acquisition d'un verrou applicatif et d'un slot de build serveur.
5. Clone isolé, submodules/LFS, génération du plan de build, injection contrôlée des variables et secrets BuildKit.
6. Build avec logs structurés ; production d'une image identifiée par digest ; push registry si requis.
7. Préparation du container candidat, volumes, réseau, labels et configuration proxy non active.
8. Démarrage, health checks et post-command ; bascule atomique du trafic ; arrêt gracieux de l'ancien container.
9. Publication des statuts, notification, conservation de l'artifact de rollback et nettoyage asynchrone.

**Échec/compensation** : avant bascule, supprimer uniquement le candidat et conserver l'ancien ; après bascule, tenter un rollback automatique seulement si la politique l'autorise et si l'ancien artifact est vérifié. Toujours libérer lease et slot.

### 20.3 Auto-déploiement par webhook

1. Vérifier limite de taille, IP si configurée, signature HMAC et horodatage.
2. Persister la livraison et répondre rapidement `2xx` ; dédupliquer par provider + delivery ID.
3. Associer exactement provider/installation/repository/branch ou PR à une ressource de la même team.
4. Appliquer politique de contributeur/fork, marqueurs skip et filtres de chemins.
5. Coalescer les pushes rapides : un SHA obsolète en file PEUT être remplacé par le plus récent avant le début du build.
6. Déclencher le workflow de déploiement avec référence à la livraison d'origine.

### 20.4 Preview de pull request

Socle (parité) :

- Créer une identité de preview déterministe `(application_uuid, provider, pr_id)` et une URL sans collision.
- Utiliser un jeu de variables dédié ; aucune copie implicite des secrets de production.
- Redéployer au nouveau SHA, conserver seulement la rétention définie et détruire containers/routage à fermeture/merge.
- Si le cleanup échoue, marquer `cleanup_failed`, notifier et réessayer ; ne jamais recycler l'identité d'une PR pour une autre application.

Les exigences suivantes font partie du **périmètre prioritaire** de la feature (décision §27.11, suivi §26) — elles sont livrées avec elle, pas en extension ultérieure :

1. **Docker Compose en preview** : le build pack compose DOIT être supporté — stack éphémère complet par PR (réseau isolé, volumes propres, magic variables résolues par instance de preview), détruit intégralement au cleanup.
2. **Données éphémères** : une preview PEUT provisionner ses bases éphémères, initialisées par script de seed ou par clone d'un snapshot de référence ; NE DOIT JAMAIS partager implicitement une base avec la production ou une autre preview.
3. **Cycle de vie et coûts** : plafond de previews simultanées par application et par serveur (file d'attente au-delà), **TTL d'inactivité** avec destruction automatique, resource limits distincts pour les previews, pool de serveurs de preview dédié optionnel ; le proxy DEVRAIT supporter le **scale-to-zero** (arrêt du container idle, réveil à la première requête).
4. **Protection d'accès par défaut** : toute URL de preview est protégée (basic auth ou lien signé) et sert `X-Robots-Tag: noindex` ; l'exposition publique est un choix explicite par application.
5. **Monorepo** : les watch paths s'appliquent aussi aux previews — seule une application affectée par les fichiers modifiés de la PR est (re)déployée.
6. **Intégration Git riche** : commit statuses/checks (pending/success/failure) utilisables comme condition de merge, API Deployments GitHub (« View deployment »), **commentaire unique mis à jour en place** (pas un commentaire par déploiement), et parité de feedback pour GitLab/Gitea, pas seulement GitHub App.
7. **Contrôles de déclenchement — options activables par application** (désactivées par défaut, le comportement de parité restant le défaut) : opt-in par label de PR, commandes en commentaire (`/deploy`, `/destroy`), exclusion des draft PRs, annulation automatique du build de preview rendu obsolète par un nouveau commit. Chaque contrôle est activable individuellement.
8. **Forks sur approbation** : une PR de fork PEUT obtenir une preview après approbation manuelle d'un mainteneur — builder isolé, aucun secret injecté ; sans approbation, elle reste ignorée (INV-010).

### 20.5 Backup et restore d'une base

- Verrouiller une exécution par plan, vérifier l'espace temporaire et la destination S3 avant lancement.
- Exécuter l'outil adapté dans un environnement contrôlé, capturer exit code, taille, checksum et version moteur.
- Uploader par flux, vérifier l'objet distant, puis appliquer rétention locale/S3 sans supprimer le dernier backup valide.
- Le restore est une opération séparée, confirmée, avec test préalable du format et journal complet. Un restore vers une base non vide exige une confirmation renforcée.
- **Acceptation** : un succès local + échec S3 est un statut partiel explicite, pas un succès global.

Exigences complémentaires (décision §27.14) :

- **Backup des volumes applicatifs** : plans de backup sur les volumes et bind mounts des applications et services — pas seulement les bases — chiffrés et dédupliqués (outil type restic), avec option de quiesce/stop par ressource pour la cohérence, et la même planification, rétention locale/S3 et notifications que les backups de bases.
- **Moteurs additionnels** : Redis (snapshot RDB) et ClickHouse couverts nativement, levant la limitation de parité (§15).
- **Restore drills** : test de restauration automatique périodique dans un environnement jetable — restauration réelle + vérification d'intégrité (checksum, comptage) — avec alerte si un plan de backup s'avère non restaurable. Un backup jamais restauré n'est pas considéré comme fiable.

### 20.6 Suppression d'une ressource

1. Afficher une prévisualisation : containers, réseaux, domaines, tâches, backups et volumes affectés.
2. Demander distinctement si les volumes/données persistantes doivent être conservés.
3. Créer un job de suppression idempotent ; retirer d'abord le routage, puis workloads, objets éphémères et enfin l'objet logique.
4. En cas d'échec partiel, conserver un tombstone réconciliable et proposer retry/forget ; ne pas perdre la liste des restes distants.

### 20.7 Adoption d'une ressource existante (décision §27.13)

1. Scanner un serveur : inventaire des containers et stacks compose **non gérés** (s'appuie sur INV-015).
2. Proposer un mapping vers le modèle AkerDock : application ou service, réseaux, volumes, variables, ports et domaines détectés par inspection et labels.
3. Prévisualiser : ce qui sera géré, ce qui sera modifié (labels/metadata ajoutés), ce qui n'est pas adoptable et pourquoi.
4. Adopter **sans redéploiement** : AkerDock prend le contrôle sans redémarrer le workload lorsque c'est possible ; le premier redéploiement normalise complètement la ressource.
5. Opération réversible : « désadopter » rend la ressource à son état non géré sans la détruire.

**Acceptation** : adopter un stack compose multi-services avec volumes puis le redéployer sans perte de données ; une ressource non représentable dans le modèle est signalée avec le motif, jamais adoptée partiellement en silence.

### 20.8 Déploiement coordonné d'un environnement (décision §27.16)

- Un environnement peut être déployé **comme une unité** : graphe de dépendances explicite entre ressources, ordre topologique, parallélisme au sein d'un même niveau.
- **Hooks de migration** : job one-shot exécuté après build et avant bascule (ex. migration de schéma) ; l'échec du hook empêche toute bascule dans l'environnement.
- Mode atomique par niveau (optionnel) : la bascule de trafic attend que toutes les ressources du niveau soient saines.
- **Rollback automatique sur santé dégradée** (politique opt-in par application) : après bascule, fenêtre d'observation (bake time) sur les health checks ; en cas de dégradation, rollback vers l'artifact précédent vérifié, notifié et audité.
- Échec partiel : état de l'environnement explicite (ressources déployées / non déployées / en échec), reprise possible au point d'échec — jamais de demi-bascule silencieuse.

## 21. Machines à états

### 21.1 Déploiement

```text
queued → preparing → cloning → building → pushing? → starting
   └──────────────────────────────────────────────→ cancelled
starting → healthchecking → switching → finishing → succeeded
    └──────────────→ failed ←──────────────────────────┘
failed → retrying → preparing
```

- `cancelled`, `failed` et `succeeded` sont terminaux pour une tentative.
- `queued → superseded` : un déploiement encore en file peut être remplacé par un plus récent de la même application (coalescing §20.3.5) ; `superseded` est terminal, assimilé à `cancelled`, avec lien vers le déploiement remplaçant.
- Un retry crée une tentative liée ou incrémente explicitement `attempt`; il ne réécrit pas silencieusement l'historique.
- `switching` est protégé par un verrou exclusif par application/destination.

### 21.2 Ressource et serveur

```text
Ressource désirée : stopped ↔ running → deleting → deleted
Ressource observée : unknown | starting | healthy | unhealthy | exited | missing
Serveur : pending → validating → ready ↔ unreachable → maintenance → deleting
```

État désiré et état observé sont stockés séparément. La réconciliation converge vers l'état désiré mais suspend les actions destructives lorsque l'observation est trop ancienne.

### 21.3 Job générique

```text
scheduled → queued → leased → running → succeeded
                       │          ├→ retry_wait → queued
                       │          ├→ cancelled
                       └───────────└→ dead_letter
```

Chaque lease a une expiration et un heartbeat. Après crash, un autre worker reprend uniquement après expiration et vérifie l'effet déjà produit avant de rejouer.

## 22. Exigences non fonctionnelles proposées

### 22.1 Disponibilité et résilience

- Les workloads et proxies continuent de fonctionner sans control plane.
- L'API et les workers supportent au minimum deux instances derrière un load balancer, sans session locale obligatoire.
- Tous les appels SSH, Git, registry, S3 et provider ont timeout, cancellation, classification d'erreur et retry borné avec jitter.
- Un circuit breaker empêche une panne fournisseur de saturer les workers.
- Les jobs de déploiement et restore ne sont jamais rejoués à l'aveugle ; leur reprise commence par une inspection distante.
- Une procédure documentée restaure PostgreSQL, clés de chiffrement, clés SSH, configurations proxy et fichiers nécessaires.

### 22.2 Performance et capacité de référence

Pour la première version stable, sur 4 vCPU/8 Go et PostgreSQL correctement dimensionné :

- 100 serveurs, 2 000 ressources et 100 000 déploiements historiques par instance.
- 50 builds simultanés distribués ; limite configurable par serveur et par team.
- 1 000 livraisons webhook/minute en burst, avec mise en file sans perte.
- 500 flux realtime concurrents et 50 sessions terminal simultanées.
- Pagination obligatoire pour toute collection ; aucun endpoint liste ne charge une relation non bornée.
- Backpressure sur logs : buffer borné, reprise par curseur, signal explicite si des lignes ont été abandonnées.

Ces nombres sont des objectifs de test, pas des limites de licence. Ils doivent être révisés après benchmarks.

### 22.3 Durabilité et cohérence

- Transactions ACID pour mutations métier et outbox ; migrations versionnées et restaurables.
- Sauvegarde du control plane chiffrable, checksumée et testée périodiquement par restore automatisé.
- Cohérence éventuelle admise pour statuts et métriques ; forte cohérence exigée pour autorisation, réservation de nom/port, secret et bascule de trafic.
- Verrou optimiste sur éditions UI/API afin d'éviter l'écrasement silencieux d'une configuration concurrente.
- Tous les timestamps internes sont UTC ; affichage dans le fuseau utilisateur/serveur avec indication explicite.

### 22.4 Compatibilité

- Serveurs Linux AMD64/ARM64 avec Docker Engine ≥ 24 et Compose v2.
- PostgreSQL interne sur une plage de versions explicitement testée ; upgrade majeur guidé.
- Navigateurs evergreen ; UI responsive jusque mobile pour consultation, actions d'urgence et terminal.
- API versionnée ; compatibilité descendante sur une version mineure, dépréciation annoncée avant suppression.
- Export JSON/YAML des configurations non secrètes et export chiffré optionnel des secrets pour éviter le lock-in ; l'export fait partie du contrat de configuration déclarative (§24.5).

### 22.5 Accessibilité et ergonomie

- Parcours clavier, focus visible, labels de formulaires, contraste WCAG 2.1 AA et annonces live pour progression/erreurs.
- Toute action longue devient un job visible avec étapes, durée, logs, annulation possible et remédiation.
- Confirmation renforcée pour suppression de données, restore, rotation de CA et terminal root.
- Les valeurs générées (UUID, domaine, URLs, credentials affichables) ont une action de copie et un contexte clair.

## 23. Sécurité et modèle de menace

### 23.1 Confiance et isolation

- Le control plane, ses administrateurs root et toute personne ayant un terminal root sont hautement privilégiés.
- Un serveur cible compromis ne doit pas donner accès aux autres serveurs : clés/credentials séparables et secrets distribués au strict besoin.
- Une team est une frontière de sécurité. Tous les repositories/queries/services reçoivent le `team_id` depuis le contexte authentifié, jamais depuis un paramètre client non vérifié.
- Les builders exécutent du code non fiable. Ils doivent être isolés des credentials du control plane, du socket Docker global lorsque possible et du réseau interne sensible.
- Les previews publiques utilisent des builders dédiés ou une politique d'isolation renforcée ; aucun secret de production par défaut.

### 23.2 Secrets et cryptographie

- Chiffrement au repos authentifié (AEAD) avec clé maître externe ou fichier root-only ; versionnement et rotation.
- Mots de passe hashés avec Argon2id ; tokens API stockés sous forme de hash irréversible avec préfixe d'identification.
- Secrets masqués dans UI/API/logs/audit ; révélation explicite seulement si le produit l'autorise et si `read:sensitive` est présent.
- Clés SSH sans passphrase acceptées pour compatibilité, mais fichiers `0600`, répertoire `0700`, sélection par team et rotation assistée.
- Webhook secrets, OAuth client secrets, credentials DNS-01, registry/S3 credentials et CA privées suivent le même secret store.

### 23.3 Contrôles applicatifs

- CSRF pour sessions navigateur, cookies Secure/HttpOnly/SameSite, rotation de session après login/élévation, invalidation à logout/changement de rôle.
- 2FA TOTP avec codes de récupération ; anti-bruteforce et délai progressif sur login.
- OIDC : validation stricte issuer, audience, nonce, PKCE et email normalisé ; liaison de compte explicite contre la prise de contrôle par collision d'email.
- SSRF : allow/deny policy sur URLs Git, registry, S3, webhook et proxy ; blocage metadata cloud/link-local par défaut.
- Validation centralisée des images, branches, chemins, domaines, CIDR, ports, cron et options Docker.
- Protection path traversal/symlink lors des file mounts, archives, clones et uploads de backup.
- Limite taille/type sur uploads et affichage UI des file mounts (5 MiB maximum pour édition inline par parité v4.1.x).
- Neutralisation ANSI/HTML dans logs et limitation des séquences terminal côté affichage.

### 23.4 Audit minimal

Journaliser : login/logout/échecs, MFA, membres/rôles, création/révocation de tokens, accès sensible, mutation de secret, terminal, changements serveur/proxy, déploiement/rollback, backup/restore, suppression, settings instance et appels webhook/API mutateurs.

Chaque événement contient `event_id`, date UTC, acteur/type/token, team, action, ressource/type/UUID, résultat, IP, user-agent, request/correlation ID et diff redacted. L'audit est append-only, paginé, filtrable, exportable et soumis à rétention.

### 23.5 Tests de sécurité obligatoires

- Matrice inter-team sur chaque endpoint et relation indirecte.
- Fuzzing des parseurs Compose, env, cron, domaines, ports et custom Docker options.
- Tests d'injection shell sur toute commande distante.
- Scénarios webhook : replay, mauvaise signature, repo au nom préfixe, fork, payload volumineux et événements désordonnés.
- Scénarios de concurrence : double deploy, delete pendant deploy, rotation de clé pendant job, double restore.
- SAST, dependency/container scanning, SBOM et images signées pour les releases AkerDock.

## 24. Contrats API, événements et jobs

### 24.1 REST

- OpenAPI est un artifact versionné et testé en CI ; l'API est sous `/api/v1`.
- Erreurs au format stable : `code`, `message` générique, `details` validés, `request_id`; aucune stack ni commande sensible.
- Pagination par curseur recommandée pour historiques/logs ; pagination page/per-page acceptée pour compatibilité MCP.
- `Idempotency-Key` supporté sur créations, deploy, backup et restore.
- ETag/version optimiste sur PATCH sensibles ; réponse `409` avec version courante en cas de conflit.
- Les actions longues répondent `202` avec `job_uuid` et URL de suivi.
- Les permissions sont évaluées à l'action, pas seulement au groupe de routes : `read`, `read:sensitive`, `write`, `deploy`, `root`.

### 24.2 Événements internes

Envelope minimal :

```json
{
  "id": "uuid",
  "type": "deployment.succeeded.v1",
  "occurred_at": "RFC3339Nano",
  "team_uuid": "uuid",
  "resource_uuid": "uuid",
  "actor": {"type": "user|token|system", "uuid": "uuid"},
  "correlation_id": "uuid",
  "payload": {}
}
```

- Version dans le type ; consommateurs idempotents ; ordre garanti seulement par clé d'agrégat si nécessaire.
- Outbox publiée après commit, inbox/déduplication chez les consommateurs à effets externes.
- Les payloads contiennent des références et métadonnées redacted, jamais les valeurs de secrets.

### 24.3 Scheduling

- Cron interprété dans un timezone explicite, avec prochaine exécution prévisualisée.
- Politique de chevauchement par tâche : `forbid` par défaut, `allow` ou `replace` optionnel.
- Politique de missed run après indisponibilité : `skip` par défaut ou `catch_up_one`; jamais une rafale illimitée.
- Backups, cleanup et tâches utilisateur utilisent le même scheduler mais des files/priorités séparées.

### 24.4 Realtime et terminal

- Transport : **SSE** avec reprise par `Last-Event-ID` pour logs, statuts et progression ; **WebSocket réservé au terminal** (décision §27.24).
- Flux logs/statuts reprenables par curseur et protégés par la même policy que l'endpoint REST équivalent.
- Token realtime court, mono-usage ou borné à la ressource ; révocation à la fermeture de session.
- Terminal via PTY avec resize, heartbeat, idle timeout, durée maximum configurable et kill garanti à la déconnexion/expiration.
- L'ouverture et la fermeture sont auditées ; les frappes ne sont pas enregistrées par défaut pour éviter de collecter des secrets, sauf mode réglementaire explicite.

### 24.5 Configuration déclarative — config as code (décision §27.12)

- Toute la configuration non secrète d'une team (projets, environnements, ressources, domaines, variables non secrètes, plans de backup, tâches planifiées) est **exportable en YAML** stable, versionnable en Git.
- **Apply idempotent** : soumettre ce YAML fait converger l'état — création, mise à jour, suppression uniquement sur demande explicite ; un mode **dry-run** produit le diff complet avant application ; les conflits sont détectés par version optimiste (§24.1).
- Les secrets sont **référencés** (nom + version), jamais inline dans l'export ; leurs valeurs passent exclusivement par les endpoints dédiés.
- Le format est un contrat versionné (schéma publié), soumis à la même politique de compatibilité que l'API (§22.4).
- Un **provider Terraform/OpenTofu officiel** est construit sur l'API et couvre au minimum le périmètre P0/P1.
- Un apply est audité comme toute mutation et exécuté comme un job visible avec étapes et annulation (§22.5).

## 25. Dashboard web et exigences UX

### 25.1 Exigences par parcours

| Parcours | Exigences minimales |
|---|---|
| Onboarding | Assistant premier démarrage : root user, première team, premier serveur (localhost ou distant) et première ressource, avec sortie possible à chaque étape |
| Dashboard | État global, serveurs injoignables, déploiements actifs/échoués, alertes disque/backup/update et actions prioritaires |
| Création ressource | Sélecteur Project/Environment/Destination, source/build pack, résumé avant création, validation inline et valeurs par défaut sûres |
| Détail application | État désiré/observé, domaine, source/SHA, config, env, storage, health, déploiements, logs, terminal, tâches et actions lifecycle |
| Déploiement | Timeline des étapes, log stream, config diff, auteur/déclencheur, SHA/digest, durée, cancel/retry/rollback selon état |
| Serveur | Reachability, Docker/proxy/Sentinel, ressources, destinations, disque/CPU/RAM, cleanup, logs et terminal |
| Base | URLs interne/externe, credentials masqués, SSL, config, volumes, health, backups/restores et avertissements de données |
| Service Compose | Éditeur validé, diff, liste des composants, domaines/env/storage/health/logs par composant |
| Sécurité | Membres/rôles, invitations, tokens, sessions, MFA/SSO, clés/credentials et audit |

Les formulaires distinguent systématiquement : valeur enregistrée, valeur héritée, valeur générée, secret verrouillé et changement non encore déployé.

### 25.2 Stack et architecture du dashboard

- Le dashboard est une **SPA Angular** (dernière version LTS), TypeScript en mode strict, composants **standalone** et signals ; pas de SSR requis (outil d'administration authentifié).
- L'UI consomme **exclusivement l'API publique** (§24) et les flux realtime — aucune route privée non documentée ; toute capacité visible dans l'UI est donc scriptable via API/CLI (cohérent avec §18.2).
- **Distribution** : assets statiques compilés, embarqués et servis par le binaire Go du control plane ; aucun runtime Node en production ; l'UI et l'API partagent le port unique du control plane (§27.1).
- **Lazy loading par domaine fonctionnel** (serveurs, projets, ressources, sécurité, settings) ; budget de performance défini et suivi en CI (taille des bundles, temps de chargement initial).
- Génération du client API et de ses types depuis l'artefact OpenAPI (§24.1) pour empêcher toute dérive UI/API.
- **i18n dès le premier composant** : UI en anglais (langue par défaut), aucune chaîne en dur — clés de traduction partout ; le français arrive comme seconde locale sans refactoring.

### 25.3 Design system et composants

- **Bibliothèque de composants interne, minimaliste** : boutons, formulaires, tables, badges d'état, timeline de déploiement, log viewer, éditeurs (env, compose), modales de confirmation — **pas de kit UI tiers lourd** (Material & co) ; dépendances tierces limitées aux besoins spécialisés (xterm.js pour le terminal, éditeur de code, graphiques de métriques).
- **Design system « clean » documenté et versionné** : design tokens (couleurs, typographie, espacements, rayons, élévations), thèmes **clair et sombre**, densité adaptée à un outil d'exploitation (tables et listes compactes, information dense sans surcharge décorative).
- Thème par défaut : **suivi du système** (`prefers-color-scheme`) avec toggle manuel persisté par utilisateur ; couleur d'accent de marque : **teal/cyan**, choisie pour ne pas entrer en collision avec les couleurs sémantiques d'état (succès, alerte, danger).
- **États visuels normalisés** dans tout le produit : mêmes couleurs/badges pour running, starting, healthy, unhealthy, exited, failed, queued, stale/inconnu — un état donné se lit de la même façon sur le dashboard, une ressource, un déploiement ou un job.
- Iconographie et vocabulaire cohérents ; toute action destructive suit le même pattern de confirmation (§22.5).
- Catalogue de composants consultable (type Storybook ou équivalent) servant de référence unique ; un composant ne rentre dans l'UI que s'il est dans le catalogue.
- Le design system respecte les exigences d'accessibilité du §22.5 (clavier, focus, contraste WCAG 2.1 AA) dès la conception des composants, pas en retrofit.

## 26. Stratégie de livraison et grille de suivi

### 26.1 Niveaux

- **P0 — Fondation** : auth/team, projets/environnements, serveurs SSH, Docker standalone, applications Dockerfile/image, variables, volumes, Traefik/HTTPS, queue, logs et lifecycle.
- **P1 — PaaS utilisable** : GitHub/GitLab/webhooks, Nixpacks/Railpack, zero-downtime, rollback, databases, backups S3, notifications, scheduled tasks et API publique.
- **P2 — Périmètre large** : Compose/services, catalogue one-click, previews, build servers, Caddy, Sentinel, log drains, terminal, OAuth/OIDC, shared vars et cleanup.
- **P3 — Périphérie/expérimental** : multi-server d'une app, MCP, DNS-01 avancé et Swarm déprécié derrière feature flag. (Cloudflare tunnels, cloud provisioning et patching : retirés — ADR-027.)

Chaque phase est utilisable seule. Une feature ne passe pas à « complète » sans documentation, migrations, métriques, audit, tests d'autorisation et scénario de reprise.

### 26.2 Grille de suivi

La colonne « Sections » renvoie aux exigences de ce document qui définissent la capacité.

| Capacité | Sections | Priorité | Statut | Preuve attendue |
|---|---|---:|---|---|
| Team isolation/auth/tokens | §10, §23 | P0 | À faire | Tests inter-team + API |
| Server onboarding/SSH | §3, §20.1 | P0 | À faire | Tests module du validateur + parcours E2E unique ; VM/ARM64 manuels (§27.26) |
| Deploy image/Dockerfile | §5, §20.2 | P0 | À faire | Tests unitaires du moteur/reprise + parcours E2E unique |
| Proxy/domaines/ACME | §4 | P0 | À faire | Fixtures de conformité + routage réel dans le parcours E2E unique |
| Git/build packs/webhooks | §5.1–5.6 | P1 | À faire | Tests de protocole/module par provider et build pack |
| Databases/backups/restore | §6–7 | P1 | À faire | Tests module par moteur supporté |
| Compose/services/templates | §5.2, §9 | P2 | À faire | Conformance fixtures Compose |
| Previews PR enrichies (compose, données éphémères, seed par clone de volume — ADR-029, TTL/caps, protection, checks, forks approuvés) | §5.6, §20.4, §27.11 | P2 | Conforme | Tests protocole multi-providers + sécurité fork/accès + tests module du seed |
| Scale-to-zero previews **et applications** (endort/réveille via un waker en coupure) | §20.4.3, proxy-contract §8 | P3 | En cours | Tests module waker (réveil, limites 503/504, single-flight, uptime non-activité) + génération du fichier dynamique + décision d'endormissement ; réveil de bout en bout au parcours E2E (ADR-036, ADR-037) |
| Backups volumes + Redis/ClickHouse + restore drills | §20.5, §27.14 | P1 | À faire | Tests module backup/restore + drill automatisé |
| Upgrade majeur PostgreSQL de l'instance (in-place opt-in, backup-first) | §14.3, §22.4 | P2 | Conforme | `scripts/pg-upgrade.sh` (détection de version, copie du volume, `pgautoupgrade` one-shot, vérif health) + garde-fou `install.sh` + runbook §C (ADR-039) |
| Config as code + Terraform officiel | §24.5, §27.12 | P2 | À faire | Round-trip export→apply + tests provider |
| Adoption de ressources existantes | §20.7, §27.13 | P2 | Conforme | Tests module scan/réconciliation sans perte |
| Déploiement coordonné + auto-rollback | §20.8, §27.16 | P2 | À faire | Tests unitaires graphe, hooks et rollback |
| Fiabilité compose (zero-downtime, limits) | §27.15 | P2 | À faire | Tests de commandes/états + fixtures cgroups ciblées |
| Uptime monitoring intégré | §27.17 | P2 | Conforme | Tests module des checks, seuils et alerting |
| CLI locale (debug : login navigateur, contextes, ls/logs/shell/port-forward) | §12, §5.7 | P2 | À faire | Tests module (login poll+PKCE, mux tunnel, REF/contextes) + validation manuelle shell/forward (ADR-031/032/033) |
| CLI deploy local (`akerdock up`) | §12, §27.18 | P2 | À faire | Tests module du push local + validation manuelle ciblée |
| Notifications : routage/agrégation | §11, §27.19 | P2 | À faire | Tests flapping/débounce + heures calmes |
| Observabilité/terminal | §3.8, §5.7, §13 | P2 | À faire | Charge + auth + reconnect |
| Multi-serveurs HA d'une même app | §3.3, §27.4 | P3 | À faire | Spike + validation manuelle (Swarm non réimplémenté, ADR-004) |
| Tunnels Cloudflare / provisioning cloud / server patching | §3.2, §3.6 | — | Abandonné | ADR-027 (réévaluable sur demande avérée) |

Le statut autorisé est `À faire | En cours | Partiel | Conforme | Divergence documentée | Abandonné`. Une preuve renvoie vers tests, captures, benchmark ou ADR.

### 26.3 Definition of Done d'une capacité

1. Comportements nominaux et erreurs documentés.
2. API et modèle d'autorisation spécifiés.
3. Tests unitaires et module au niveau propriétaire ; le produit conserve un
   parcours E2E représentatif unique (ADR-028).
4. Idempotence, retry, cancellation et reprise après crash testés si action longue.
5. Logs, métriques, trace/correlation ID, audit et notifications pertinents présents.
6. Migration up/down ou procédure de rollback de release.
7. Documentation opérateur et utilisateur.
8. Entrée de matrice de parité mise à jour avec preuve.

## 27. Points de divergence à décider par ADR

Ces sujets sont tracés ici avec leur statut. Les 26 points ci-dessous sont tous tranchés (orientations produit actées le 11 juillet 2026) et formalisés en ADR dans `docs/adr/` (§29.12) ; toute révision d'une décision passe par un nouvel ADR qui supersede l'ancien.

1. **SSH push vs agent pull** : SSH offre la parité initiale ; un agent sortant réduit les ports entrants et permet événements Docker, mais introduit versionnement, enrollment et upgrade d'agent.
   **Décision** : SSH d'abord, agent sortant comme cible pour réduire la surface entrante. Exigences associées sur les ports : le proxy écoute sur **80/443 par défaut** mais ses ports d'écoute **DOIVENT être configurables par serveur** (ex. 8080/8443 lorsqu'un reverse proxy amont détient déjà 80/443) ; le control plane est exposé sur **un seul port**, derrière son propre domaine/DNS — un port, un certificat, une règle de firewall.
2. **Queue PostgreSQL vs Redis/NATS** : PostgreSQL simplifie le self-hosting ; un bus séparé améliore le débit mais augmente l'exploitation.
   **Décision** : queue durable **PostgreSQL** retenue. L'interface queue reste abstraite dans le code, mais aucun bus externe (Redis/NATS) n'est planifié.
3. **Secrets en base vs Vault/SOPS/KMS** : commencer par chiffrement enveloppe, exposer ensuite une interface de secret store externe.
   **Décision** : chiffrement enveloppe AEAD (AES-256-GCM) dans PostgreSQL, clé maître dans un fichier root-only ou une variable d'environnement, versionnement de clé et rotation (§23.2). Interface `SecretStore` interne dès le début, mais une seule implémentation livrée ; Vault/KMS uniquement sur demande validée.
4. **Docker standalone vs orchestrateur** : stabiliser standalone ; traiter Swarm comme compatibilité dépréciée et évaluer Nomad/Kubernetes uniquement sur besoin validé.
   **Décision** : **Docker standalone confirmé comme runtime** (Engine/Compose/BuildKit). Kubernetes — y compris sous forme « embarquée et transparente » — est écarté : il contredit la proposition de valeur (VPS modestes dès 2 GB, objets Docker standards réversibles §16.1(3), catalogue de templates compose) et l'abstraction fuirait au premier incident (pods, PVC, ingress) face à des utilisateurs qui ont précisément choisi la plateforme pour ne pas apprendre Kubernetes. Swarm n'est pas réimplémenté (compatibilité dépréciée au mieux, derrière feature flag P3). Un orchestrateur ne sera réévalué que sur besoin utilisateur validé, via le contrat d'adaptateur runtime (§18.1), et sans jamais être imposé aux installations existantes. L'ADR consignera ce rejet et ses motifs.
5. **Build local vs builders isolés rootless** : la parité Docker socket est rapide ; la cible sécurité devrait isoler les builds non fiables (BuildKit rootless/VM/microVM).
   **Décision** : builds via le BuildKit du Docker du serveur en P0/P1 (parité) ; builders **BuildKit rootless dédiés obligatoires pour le code non fiable** au plus tard avec les previews de forks approuvés (§20.4.8). Le contrat d'adaptateur build est écrit dès P0 pour que la bascule ne touche pas le moteur de déploiement.
6. **Rollback local vs registry immuable** : préférer les digests OCI conservés en registry pour un rollback reproductible, avec fallback local.
   **Décision** : si un registry est configuré, chaque déploiement est pushé et référencé par **digest OCI** (rollback reproductible vers toute version conservée) ; sans registry, rétention locale des N dernières images, explicitement protégées du cleanup automatique (INV-015).
7. **RBAC** : un couple admin/member est trop grossier dès qu'une équipe partage un environnement de production ; AkerDock introduit des rôles et permissions par projet et environnement.
   **Décision** : RBAC fin retenu, modèle **permissions à la carte** — chaque action produit est une permission granulaire ; un rôle est un ensemble nommé de permissions, attribuable au niveau team, projet ou environnement. **Rôles système prédéfinis** fournis : owner, admin, developer et **viewer (read-only)** ; rôles custom composables par les admins de team. À spécifier dans la matrice RBAC/permissions (§29.7).
8. **Métriques push vs pull/OpenTelemetry** : conserver un agent léger mais standardiser OTLP/Prometheus pour éviter un protocole propriétaire.
   **Décision** : **OTLP partout** — agent serveur, control plane et workers émettent métriques/traces/logs en OpenTelemetry, avec exposition Prometheus ; aucun protocole propriétaire.
9. **Proxy labels vs configuration déclarative** : supporter les labels de parité, générer une représentation intermédiaire commune afin de tester Traefik et Caddy de façon identique.
   **Décision** : validé — représentation intermédiaire commune, labels supportés pour la parité, fixtures de conformité partagées Traefik/Caddy (§29.6). Séquencement : **Traefik seul en P0** ; Caddy arrive en P2 via la représentation intermédiaire, dont les fixtures existent dès P0.
10. **Catalogue one-click** : importer les templates compatibles sous respect des licences, mais versionner, valider et signer le catalogue indépendamment du binaire.
    **Décision** : catalogue = **dépôt de templates dédié** maintenu par le projet (versionné, validé, signé, rafraîchissable indépendamment du binaire) **+ dépôts de templates utilisateur** — chaque team peut enregistrer un ou plusieurs repositories Git (publics ou privés, via les clés/credentials existants) contenant ses propres templates, avec validation à l'import et resynchronisation à la demande.
11. **Previews riches** : un preview minimal (un container + une URL publique) est en dessous du standard du domaine — environnements compose complets, données éphémères, TTL, protection d'accès, checks Git.
    **Décision** : la feature preview est livrée d'emblée enrichie, tout le périmètre du §20.4 étant prioritaire — compose éphémère, données éphémères, TTL/plafonds/scale-to-zero, protection d'accès par défaut, watch paths en preview, checks Git, forks sur approbation ; les contrôles de déclenchement (labels, commandes en commentaire, exclusion des drafts, annulation des builds obsolètes) sont des **options activables par application**, désactivées par défaut.
12. **Configuration as code** : une plateforme pilotée uniquement par l'UI n'est pas reproductible ; le YAML exporté et un provider Terraform officiel le rendent possible.
    **Décision** : export YAML complet + apply idempotent avec dry-run/diff, et provider Terraform/OpenTofu officiel construit sur l'API. Exigences au §24.5.
13. **Adoption de ressources existantes** : aucune plateforme du segment ne sait prendre le contrôle d'un container ou d'un stack compose déjà déployé.
    **Décision** : adoption sans redéploiement, prévisualisée et réversible — c'est aussi le chemin d'entrée depuis n'importe quelle plateforme, puisqu'elle ne suppose que du Docker standard. Workflow au §20.7.
14. **Backups au-delà des bases** : sauvegarder quatre moteurs SQL et ignorer les volumes applicatifs laisse la moitié de l'état sur la table — et un restore jamais rejoué n'est pas un backup.
    **Décision** : backup de volumes chiffré/dédupliqué, moteurs Redis et ClickHouse, et restore drills automatiques. Exigences au §20.5.
15. **Fiabilité compose « by design »** : les pièges structurels du §15 (zero-downtime des stacks, resource limits sans effet) sont traités dès la conception.
    **Décision** : AkerDock DOIT fournir le zero-downtime pour les services web des stacks compose (bascule par service derrière le proxy) et DOIT appliquer réellement les resource limits déclarées aux ressources compose.
16. **Déploiement coordonné d'un environnement** : déployer ressource par ressource, sans ordre ni hooks, casse tout environnement où une migration doit précéder une application.
    **Décision** : environnement déployable comme une unité — graphe de dépendances, hooks de migration avant bascule, mode atomique par niveau, rollback automatique opt-in sur santé dégradée. Workflow au §20.8.
17. **Uptime monitoring intégré** : sans check externe, la plateforme ne sait pas si ce qu'elle déploie répond réellement depuis Internet.
    **Décision** : checks HTTP/TCP simples intégrés — cible, intervalle, seuils d'échec, exécutés hors du workload surveillé — avec alerting via les canaux de notification existants (§11) et historique de disponibilité par ressource. Pas d'APM : le périmètre s'arrête au up/down et à la latence.
18. **Déploiement depuis le poste (`akerdock up`)** : exiger un dépôt Git accessible ou une image publiée ferme la boucle de feedback la plus courte, celle du développeur.
    **Décision** : la CLI PEUT pousser un contexte local — détection du build pack, création de l'application si besoin, build et déploiement — pour le prototypage avant branchement d'un provider Git. Un déploiement de source locale est marqué comme tel dans l'historique (pas de SHA Git, digest du contexte à la place) et n'active jamais d'auto-deploy.
19. **Routage et agrégation des notifications** : un message par événement rend le canal inutilisable (un serveur qui flappe = des dizaines d'alertes, donc plus personne ne lit).
    **Décision** : règles de routage par projet/environnement/sévérité vers les canaux, **agrégation/débounce** des événements répétitifs (flapping), heures calmes configurables et résumé différé des événements non critiques.
20. **Licence du projet**.
    **Décision** : **Apache 2.0** — même licence que la référence, adoption et contributions maximales, clause brevets incluse. Le fossé concurrentiel est le produit, pas la licence ; le risque « fork cloud par un tiers » est accepté.
21. **Distribution de l'instance** (concrétise §16.1(6)).
    **Décision** : **docker-compose minimal à 2 services** — l'image AkerDock (binaire Go statique dans une image distroless, modes `all-in-one`/`api`/`worker` §18.2) + PostgreSQL. Un seul `docker compose up`, upgrade par changement de tag, backups PostgreSQL standards, un seul port exposé (§27.1).
22. **Nommage des variables prédéfinies**.
    **Décision** : préfixe **`AKERDOCK_*` uniquement** (`AKERDOCK_FQDN`, `AKERDOCK_URL`, `AKERDOCK_BRANCH`, `AKERDOCK_PR_ID`…), **sans alias d'aucune autre plateforme** : identité propre, pas de dette de nommage, un seul nom par variable — deux noms pour une même valeur, c'est une divergence qui attend son heure. La syntaxe des magic variables `SERVICE_<TYPE>_<ID>` est conservée : elle est fonctionnelle, pas liée à une marque.
23. **Migration depuis une autre plateforme**.
    **Décision** : **aucun assistant d'import propriétaire** — l'adoption générique de ressources (§20.7) EST le chemin d'entrée : AkerDock reprend les containers, stacks, volumes et réseaux Docker standards déjà en place, sans rien connaître du schéma interne de l'outil qui les a créés. Un import lié à un format tiers deviendrait une dette à maintenir à chaque version de ce tiers.
24. **Transport temps réel**.
    **Décision** : **SSE** (Server-Sent Events) pour logs, statuts et progression de jobs — reconnexion native, reprise par curseur `Last-Event-ID`, compatible proxies d'entreprise ; **WebSocket réservé au terminal** (seul flux bidirectionnel). Tout passe par le port unique du control plane.
25. **Socle technique Go/API**.
    **Décision** : accès PostgreSQL via **pgx + sqlc** (SQL explicite, types vérifiés à la compilation — indispensable pour les requêtes critiques de queue/leases/outbox), migrations SQL versionnées ; API **spec-first** avec router **chi** + **oapi-codegen** : les handlers Go et le client TypeScript de l'UI (§25.2) sont générés depuis le même artefact OpenAPI (§24.1).
26. **Stratégie de tests E2E**.
    **Décision révisée par ADR-028** : exactement **un parcours E2E produit**, en **Docker-in-Docker uniquement**, après fusion sur `main`, à la demande et avant release ; aucun E2E sur les pull requests et aucun catalogue nightly. Les règles déterministes sont prouvées en tests unitaires/module. **Risque résiduel accepté et documenté** : systemd, reboots réels, firewalls, disques pleins et ARM64 ne sont pas couverts par l'automatisation — ces classes sont validées manuellement de façon ponctuelle.

## 28. Maintenance de ce document

Ce PRD est la source de vérité produit : une exigence qui n'y figure pas n'est pas une exigence. Toute évolution du périmètre s'y écrit **avant** le code (workflow spec-first, CONTRIBUTING.md), et toute décision structurante donne lieu à un ADR (`docs/adr/`). La grille §26.2 porte l'état de livraison et la preuve attendue de chaque capacité.

---

## 29. Artefacts requis avant une implémentation complète

Ce PRD définit le produit et ses garanties, mais ne remplace pas à lui seul les spécifications d'ingénierie. Pour pouvoir reconstruire la plateforme sans décisions implicites dispersées dans le code, les livrables suivants sont obligatoires :

1. **Glossaire et data dictionary** : tous les champs, types, contraintes, valeurs par défaut, données sensibles et règles de suppression. — **Livré** : `docs/specs/data-dictionary.md`
2. **ERD versionné** : cardinalités, ownership team, indexes, contraintes d'unicité et stratégie de migrations. — **Livré** : `docs/specs/erd.md`
3. **OpenAPI v1** : schémas, erreurs, permissions, pagination, idempotence et exemples pour chaque endpoint. — **Livré** (périmètre P0 + cœur P1) : `docs/specs/openapi-v1.yaml`
4. **Spécification du moteur de déploiement** : plan par build pack, commandes exactes, répertoires distants, labels, noms, timeouts, locks, retry et compensation. — **Livré** : `docs/specs/deployment-engine.md`
5. **Spécification Compose** : sous-ensemble supporté, transformations, magic variables, réseaux, volumes, health checks et cas rejetés. — **Livré** : `docs/specs/compose-spec.md`
6. **Contrat proxy** : représentation intermédiaire, génération Traefik/Caddy, priorités de routes, certificats, reload atomique et fixtures de conformité. — **Livré** : `docs/specs/proxy-contract.md`
7. **Threat model STRIDE** et matrice RBAC/permissions par action et type de ressource. — **Livré** : `docs/specs/threat-model.md` + `docs/specs/rbac-matrix.md`
8. **Protocoles Git/webhook** par fournisseur avec signatures, événements, permissions d'installation et scénarios de preview. — **Livré** : `docs/specs/git-webhook-protocols.md`
9. **Plan de tests** : couverture prioritairement unitaire/module et un parcours produit E2E unique en Docker-in-Docker (décision §27.26, ADR-028) ; la matrice OS/architecture reste validée manuellement. — **Livré** : `docs/specs/e2e-test-plan.md`
10. **Runbooks opérateur** : install/upgrade/downgrade, rotation de clés, panne PostgreSQL/queue, serveur compromis, restore, cleanup bloqué et récupération d'un déploiement orphelin. — **Livré** : `docs/runbooks/` (11 runbooks + index)
11. **Inventaire licences/SBOM** : dépendances, images helper, templates one-click, logos et conditions de redistribution. — **Livré** : `docs/specs/licensing-sbom.md`
12. **ADRs initiaux et révisions** : choix listés au §27, avec décision, alternatives et conséquences. — **Livré** : `docs/adr/` (ADR-001 à ADR-028 + index)
13. **Design system et catalogue de composants** (§25.3) : tokens, thèmes clair/sombre, états visuels normalisés et composants documentés, versionnés avec l'UI Angular. — **Livré** : `docs/specs/design-system.md`

L'implémentation peut démarrer par un vertical slice P0 (auth team → ajout d'un serveur → déploiement d'une image → domaine HTTPS → logs → suppression sûre), pendant que ces artefacts sont détaillés itérativement. La parité complète ne doit toutefois pas être déclarée avant leur couverture.
