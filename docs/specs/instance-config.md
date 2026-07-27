# Spécification — Configuration de l'instance AkerDock

> Contrat de configuration du control plane AkerDock. Sources de vérité amont : PRD (`docs/PRD.md`) §10.2, §14.1–14.3, §18.2, §22.1, §23.2, §27.1, §27.3, §27.21 ; ADR-003 (chiffrement enveloppe, clé maître versionnée) ; ADR-008 (OTLP partout) ; ADR-021 (distribution compose 2 services) ; data dictionary §2.7 (format `*_enc`) et §11.7 (`instance_settings`). Cette spécification **rend normatifs** les défauts posés en « (défaut proposé) » par les runbooks [install.md](../runbooks/install.md), [key-rotation.md](../runbooks/key-rotation.md), [upgrade-downgrade.md](../runbooks/upgrade-downgrade.md) et [control-plane-restore.md](../runbooks/control-plane-restore.md) ; les runbooks référencent désormais les sections correspondantes.
>
> Périmètre : configuration du **processus AkerDock** (l'instance/control plane) et de sa distribution compose. L'arborescence des **serveurs cibles** est hors périmètre — elle est définie par la spec deployment-engine §5.1. Les réglages persistés modifiables à chaud (FQDN, email transactionnel, API on/off…) relèvent de la table `instance_settings` (data dictionary §11.7), pas de ce document — sauf leur amorçage au premier démarrage (§6).
>
> Écarts assumés vis-à-vis des extraits des runbooks (justifiés en note) : identifiants techniques en minuscules (`akerdock` comme nom de service compose, user et base PostgreSQL — §4, note N1) ; volumes nommés plutôt que bind mounts pour l'état des services (§4, note N2).

---

## 1. Vue d'ensemble

### 1.1 Sources de configuration et précédence

Trois sources, par ordre de précédence décroissant :

1. **Variables d'environnement** `AKERDOCK_*` (plus les variables standard `OTEL_*`, §2.4) — source recommandée, c'est celle que la distribution compose (§4) utilise ;
2. **Fichier de configuration optionnel** (YAML), chemin donné par `AKERDOCK_CONFIG_FILE` — clés en `snake_case` sans le préfixe (ex. `port: 8080`, `log_level: debug`). Aucun fichier n'est cherché par défaut : sans `AKERDOCK_CONFIG_FILE`, cette source n'existe pas ;
3. **Défauts compilés** dans le binaire (colonne « Défaut » du §2).

La configuration est lue **une seule fois au démarrage** : toute modification exige un redémarrage du processus. Les réglages modifiables à chaud vivent dans `instance_settings` et dans l'UI/API — jamais dans les deux à la fois : quand une valeur existe côté base, l'environnement ne sert qu'à l'amorçage du premier démarrage (§6.2).

### 1.2 Principe : zéro configuration obligatoire hors DB et clé maître

Une instance démarre avec exactement **deux éléments fournis par l'opérateur** :

- `AKERDOCK_DATABASE_URL` — l'accès PostgreSQL (ADR-002, ADR-021) ;
- `AKERDOCK_MASTER_KEY_FILE` (ou, déconseillé, `AKERDOCK_MASTER_KEY`) — la clé maître de chiffrement (ADR-003).

Tout le reste a un défaut sûr et documenté. Corollaires : aucun défaut ne peut être « deviné » silencieusement (pas de génération automatique de clé maître, pas de base embarquée) ; et l'absence de l'un des deux éléments est une **erreur fatale au démarrage** avec message explicite (§6.4, §7) — jamais un mode dégradé.

---

## 2. Variables d'environnement normatives

### 2.1 Variables lues par le binaire

| Nom | Requis | Défaut | Description | Sensible |
|---|---|---|---|---|
| `AKERDOCK_DATABASE_URL` | **oui** | — | DSN PostgreSQL (`postgres://user:pass@host:5432/db?sslmode=…`). PostgreSQL ≥ 15 (data dictionary §2). | **oui** (contient le mot de passe) |
| `AKERDOCK_MASTER_KEY_FILE` | **oui**¹ | — | Chemin du fichier de clé maître multi-versions (§3). Dans la distribution compose : `/run/secrets/master.key`. | non (le chemin ; le contenu l'est) |
| `AKERDOCK_MASTER_KEY` | non¹ (**déconseillé**) | — | Alternative : contenu du fichier de clé directement en variable (même format qu'au §3, lignes séparées par `\n`). Émet un avertissement au démarrage (§7.2) : l'environnement d'un processus est plus exposé qu'un fichier `0600` (inspection `/proc`, `docker inspect`, logs d'orchestrateur). | **oui** |
| `AKERDOCK_MODE` | non | `all-in-one` | Mode du monolithe modulaire (§18.2 PRD) : `all-in-one` \| `api` \| `worker` \| `scheduler`. Le premier argument de la ligne de commande (ex. `command: ["all-in-one"]`) a précédence sur la variable. Plusieurs `api`/`worker` peuvent coexister (§22.1) ; plusieurs `scheduler` sont sûrs (élection par verrou advisory PostgreSQL). | non |
| `AKERDOCK_PORT` | non | `8080` | **Port unique** du control plane (§27.1 PRD, ADR-021) : UI, API, SSE, WebSocket terminal et `/api/v1/health` — rien d'autre n'écoute. Dans la distribution compose, c'est aussi le port publié (§4). | non |
| `AKERDOCK_INSTANCE_FQDN` | non | — | FQDN de l'instance (§14.2 PRD). Sert uniquement à amorcer `instance_settings.fqdn` au premier démarrage (§6.2) ; ensuite la valeur en base fait foi (modifiable dans l'UI). | non |
| `AKERDOCK_INSTANCE_PORT` | non | = `AKERDOCK_PORT` | Port auquel l'instance est joignable **sur son hôte** — la cible de la route proxy `00-control-plane` (proxy-contract §5.7). Diffère de `AKERDOCK_PORT` sous la distribution compose : le mapping publie `${AKERDOCK_PORT}:8080` et le processus écoute toujours 8080 dans le container, donc le compose transmet le port publié via cette variable (§4). Un binaire lancé directement sur l'hôte n'a pas à la définir. | non |
| `AKERDOCK_ROOT_EMAIL` | non² | — | Bootstrap non interactif du premier root user (§10.2 PRD). Email validé strictement. | non |
| `AKERDOCK_ROOT_NAME` | non² | — | Nom du root user. Non vide après trim, ≤ 255 caractères. | non |
| `AKERDOCK_ROOT_PASSWORD` | non² | — | Mot de passe du root user, validation stricte (≥ 12 caractères — §10.2 PRD) ; hashé Argon2id, jamais journalisé. À retirer de l'environnement après le premier démarrage (§6.3, §7.2). | **oui** |
| `AKERDOCK_TIMEZONE` | non | `UTC` | Timezone IANA (ex. `Europe/Paris`). Amorce `instance_settings.timezone` au premier démarrage (défaut aligné sur le data dictionary §11.7) ; ensuite la valeur en base fait foi. Les timestamps stockés et journalisés restent en UTC. | non |
| `AKERDOCK_LOCALHOST_HOST` | non | `host.docker.internal` | Adresse par laquelle le processus joint la machine hôte en SSH pour le serveur `localhost` pré-enregistré (§6.2). Lue à l'amorçage seulement ; ensuite la fiche serveur en base fait foi (modifiable via `PATCH /servers/{uuid}`). Dans la distribution compose, le nom est résolu par `extra_hosts: host-gateway` (§4.1). | non |
| `AKERDOCK_GITHUB_CA_FILE` | non | — | Certificat(s) CA additionnel(s) (PEM) pour joindre un GitHub Enterprise Server à CA privée (git-webhook-protocols §2.6). Ajouté aux racines système pour les appels à l'API GitHub uniquement. | non |
| `AKERDOCK_LOCALHOST_USER` | non | `root` | Utilisateur SSH du serveur `localhost` pré-enregistré (§6.2). `install.sh` y place l'utilisateur qui exécute l'installation. Lue à l'amorçage seulement. | non |
| `AKERDOCK_LOG_LEVEL` | non | `info` | `debug` \| `info` \| `warn` \| `error`. | non |
| `AKERDOCK_LOG_FORMAT` | non | `json` | `json` (production, une ligne par événement) \| `text` (lisible, développement). | non |
| `AKERDOCK_DATA_DIR` | non | `/var/lib/akerdock` | Répertoire de données du processus (§5.2). Dans la distribution compose : volume nommé `akerdock_data`. Créé au démarrage s'il n'existe pas ; non inscriptible = erreur fatale. | non |
| `AKERDOCK_WORKER_CONCURRENCY` | non | `10` | Nombre maximal de jobs exécutés en parallèle **par processus** en mode `worker` ou `all-in-one` (entier ≥ 1). Défaut calibré sur le gabarit minimal 2 vCPU / 2 GB (§14.1 PRD) ; les plafonds par serveur et par team (§22.2 PRD) s'appliquent en plus, côté queue. | non |
| `AKERDOCK_SHUTDOWN_TIMEOUT` | non | `30s` | Délai de drain à l'arrêt gracieux (§6.5) : durée Go (`30s`, `2m`). Doit rester inférieur au `stop_grace_period` du compose (40 s, §4) et à l'expiration de lease des jobs (90 s, deployment-engine §2.5). | non |
| `AKERDOCK_TERMINAL_IDLE_TIMEOUT` | non | `15m` | Inactivité (aucune frappe) au-delà de laquelle une session terminal web est fermée (§24.4 PRD, ADR-024) : durée Go. La sortie du terminal ne compte pas comme activité — un spinner ne maintient pas un shell root oublié. | non |
| `AKERDOCK_TERMINAL_MAX_DURATION` | non | `4h` | Durée maximum d'une session terminal web, quelle que soit l'activité (§24.4 PRD) : durée Go. La fermeture est garantie (kill du PTY distant) et journalisée avec sa raison. | non |
| `AKERDOCK_CONFIG_FILE` | non | — | Chemin d'un fichier de configuration YAML optionnel (§1.1). Fichier illisible ou invalide = erreur fatale. | non |

¹ Exactement une des deux sources de clé maître doit être fournie. Les deux à la fois = erreur fatale (ambigu) ; aucune = erreur fatale (§6.4).
² Les trois variables `AKERDOCK_ROOT_*` forment un trio tout-ou-rien : en fournir une seule ou deux = erreur fatale. Elles ne sont **lues que si aucun utilisateur n'existe** en base, et consommées une seule fois (§6.3).

### 2.2 Variables consommées par le compose (pas par le binaire)

Ces variables vivent dans `/var/lib/akerdock/.env` et sont interpolées par Docker Compose (§4) ; le binaire ne les lit pas :

| Nom | Requis | Défaut | Description | Sensible |
|---|---|---|---|---|
| `AKERDOCK_TAG` | **oui** | — | Tag d'image explicite (`v1.0.0`) — jamais `latest` (runbook upgrade-downgrade). Le compose refuse de démarrer sans (`:?`). | non |
| `POSTGRES_PASSWORD` | **oui** | — | Mot de passe du PostgreSQL interne (généré à l'installation : `openssl rand -hex 24`). | **oui** |

`AKERDOCK_PORT` et les `AKERDOCK_ROOT_*`/`AKERDOCK_INSTANCE_FQDN` peuvent aussi figurer dans `.env` : le compose les transmet au service (§4).

### 2.3 Règles de nommage

Préfixe **`AKERDOCK_*` uniquement**, sans alias sous une autre marque (décision §27.22, ADR-022). Ces variables d'instance sont un espace de noms distinct des variables prédéfinies injectées dans les workloads (`AKERDOCK_FQDN`, `AKERDOCK_URL`, `AKERDOCK_BRANCH`, `AKERDOCK_PR_ID`… — deployment-engine §5.2) : aucune variable de la table §2.1 n'est injectée dans un container déployé.

### 2.4 Télémétrie : variables standard OpenTelemetry (ADR-008)

Conformément à « OTLP partout, aucun protocole propriétaire », l'export de télémétrie se configure par les variables **standard** du SDK OpenTelemetry — c'est l'unique exception assumée au préfixe `AKERDOCK_*` (une variable propriétaire dupliquant `OTEL_EXPORTER_OTLP_ENDPOINT` recréerait le protocole maison qu'ADR-008 rejette) :

| Nom | Requis | Défaut | Description | Sensible |
|---|---|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | non | — (export désactivé) | Endpoint OTLP (gRPC 4317 ou HTTP 4318) pour métriques, traces et logs du control plane et des workers. Absente = aucune tentative d'export, aucun warning répété. | non |
| `OTEL_SERVICE_NAME` | non | `akerdock` | Nom de service émis. | non |
| Autres `OTEL_*` | non | défauts SDK | Toutes les variables standard du SDK OTel Go sont honorées telles quelles (`OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER`…). | selon variable (`…_HEADERS` peut porter un token : **oui**) |

---

## 3. Fichier de clé maître multi-versions (normatif — ADR-003)

### 3.1 Format

Fichier texte UTF-8, **une ligne par version de clé** :

```text
<version>:<clé en base64 standard, 32 octets décodés>
```

- `<version>` : entier décimal **de 1 à 4294967295** (uint32 — exactement l'espace du préfixe `key_version` sur 4 octets big-endian des colonnes `*_enc`, data dictionary §2.7), sans zéros de tête. Chaque version est **unique** dans le fichier ;
- `<clé>` : base64 standard (RFC 4648, avec padding) décodant **exactement 32 octets** (AES-256-GCM, §27.3 PRD) ;
- séparateur : `:`, sans espaces autour ; fin de ligne `\n` ;
- lignes vides et lignes commençant par `#` : ignorées (commentaires d'opérateur autorisés) ;
- toute autre ligne, une version dupliquée, une base64 invalide ou une clé d'une autre longueur : fichier **invalide** → refus de démarrer (§6.4).

La **version active** — celle qui chiffre toute nouvelle écriture — est la **version de numéro le plus élevé** présente dans le fichier. Aucun marqueur explicite : c'est le comportement que le runbook [key-rotation.md](../runbooks/key-rotation.md) rend opérationnel (ajouter une ligne suffit à activer la nouvelle clé au rechargement).

### 3.2 Exemple complet

```text
# /var/lib/akerdock/keys/master.key — 0600 root:root
# v1 : installation 2026-07-11 ; v2 : rotation planifiée 2027-01-15
1:m4C9Zk0vG8kQ2m1H0cVvXHkq3D3jUj0F3q5m8Q2xX9s=
2:Zk3q8W1mB7hT4nJ6cR9vY0dL2aP5sG8uK1oE4wI7xN0=
```

Ici la version active est **2** ; la version 1 reste présente pour déchiffrer les données qui la référencent encore.

### 3.3 Permissions et emplacement

- Hôte : `/var/lib/akerdock/keys/master.key`, propriétaire `root:root`, mode **`0600`**, répertoire `keys/` en `0700` (§23.2 PRD) ;
- Container : monté **en lecture seule** sur `/run/secrets/master.key` (§4) ;
- Au démarrage, le binaire vérifie les permissions du fichier : lisible ou inscriptible par « other » = **erreur fatale** ; tout autre écart avec `0600` = avertissement (§7.2).

### 3.4 Règles de gestion des versions

1. **Ajout d'une version = rotation** : ajouter une ligne de version strictement supérieure aux existantes en fait la version active au prochain démarrage/rechargement (`docker compose up -d akerdock`). Les versions n'ont pas besoin d'être contiguës ;
2. **Ne jamais supprimer une version tant que des données chiffrées la référencent** : chaque valeur `*_enc` en base commence par la `key_version` (4 octets big-endian) qui l'a chiffrée (data dictionary §2.7) — retirer une version encore référencée rend ces ciphertexts définitivement illisibles. La procédure de vérification (histogramme SQL des versions colonne par colonne, sur les 16 colonnes chiffrées du data dictionary §12) est dans [key-rotation.md](../runbooks/key-rotation.md) ;
3. **Démarrage** : si une opération de déchiffrement rencontre une `key_version` absente du fichier, l'erreur est explicite (version manquante nommée) et l'opération échoue — l'instance ne masque jamais une clé manquante (§6.4). Le re-chiffrement vers la version active est paresseux, sans réécriture bloquante (§19.2 PRD, ADR-003) ;
4. **Sauvegarde** : le fichier (toutes versions) est copié hors machine, séparément des dumps de base (§23.1 PRD) — voir [install.md](../runbooks/install.md) étape 2 et [control-plane-restore.md](../runbooks/control-plane-restore.md) (« les 3 pièces »).

---

## 4. Distribution compose de référence (normatif — ADR-021)

### 4.1 `docker-compose.yml`

Deux services, un seul port publié, volumes nommés, healthchecks sur les deux services, tags épinglés. Fichier de référence, livré avec chaque release à l'emplacement `/var/lib/akerdock/docker-compose.yml` :

```yaml
name: akerdock

services:
  akerdock:
    image: ghcr.io/deepteams/akerdock:${AKERDOCK_TAG:?tag d'image explicite requis (jamais latest)}
    command: ["all-in-one"]              # modes all-in-one|api|worker|scheduler (§18.2 PRD, §2.1)
    restart: unless-stopped
    ports:
      - "${AKERDOCK_PORT:-8080}:8080"    # l'unique port publié du control plane (§27.1)
    environment:
      AKERDOCK_DATABASE_URL: postgres://akerdock:${POSTGRES_PASSWORD}@postgres:5432/akerdock?sslmode=disable
      AKERDOCK_MASTER_KEY_FILE: /run/secrets/master.key
      AKERDOCK_INSTANCE_FQDN: ${AKERDOCK_INSTANCE_FQDN:-}
      # Bootstrap du premier root user (§10.2 PRD) — lu uniquement si aucun utilisateur n'existe (§6.3)
      AKERDOCK_ROOT_EMAIL: ${AKERDOCK_ROOT_EMAIL:-}
      AKERDOCK_ROOT_NAME: ${AKERDOCK_ROOT_NAME:-}
      AKERDOCK_ROOT_PASSWORD: ${AKERDOCK_ROOT_PASSWORD:-}
      # Serveur localhost pré-enregistré (§6.2) — lu à l'amorçage seulement
      AKERDOCK_LOCALHOST_USER: ${AKERDOCK_LOCALHOST_USER:-}
    volumes:
      - ./keys/master.key:/run/secrets/master.key:ro
      - akerdock_data:/var/lib/akerdock
    networks: [akerdock]
    extra_hosts:
      # Fait résoudre host.docker.internal vers la passerelle du réseau compose
      # sur Linux (natif sur Docker Desktop) : c'est l'adresse SSH du serveur
      # localhost pré-enregistré (§6.2).
      - "host.docker.internal:host-gateway"
    depends_on:
      postgres:
        condition: service_healthy
    healthcheck:
      # Image distroless sans shell : le binaire embarque une sous-commande `healthcheck`
      # qui interroge http://127.0.0.1:8080/api/v1/health et sort en 0/1 (§6.6).
      test: ["CMD", "/akerdock", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 30s
    stop_grace_period: 40s               # > AKERDOCK_SHUTDOWN_TIMEOUT (30 s, §6.5)

  postgres:
    image: postgres:17                   # tag exact épinglé par les notes de release (≥ 15, data dictionary §2)
    restart: unless-stopped
    environment:
      POSTGRES_USER: akerdock
      POSTGRES_DB: akerdock
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?requis (openssl rand -hex 24)}
    volumes:
      - akerdock_pgdata:/var/lib/postgresql/data
      - ./backups:/backups               # dumps locaux visibles sur l'hôte (§5.1)
    networks: [akerdock]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U akerdock -d akerdock"]
      interval: 5s
      timeout: 3s
      retries: 10

networks:
  akerdock:
    name: akerdock

volumes:
  akerdock_data:
  akerdock_pgdata:
```

Propriétés garanties (ADR-021) : un seul `docker compose up -d` installe ; l'upgrade est un changement de `AKERDOCK_TAG` ([upgrade-downgrade.md](../runbooks/upgrade-downgrade.md)) ; PostgreSQL n'est **pas** publié sur l'hôte ; `sslmode=disable` est acceptable uniquement parce que le trafic reste sur le réseau compose privé `akerdock` — toute base externe exige `sslmode=verify-full` (avertissement §7.2 sinon).

> **Note N1 — identifiants en minuscules.** Le nom de service compose, l'utilisateur et la base PostgreSQL sont `akerdock` (minuscules). Les extraits du runbook [install.md](../runbooks/install.md) écrits avant cette spec utilisent la casse `AkerDock` : c'est la présente spec qui fait foi — les identifiants techniques sont en minuscules (un rôle PostgreSQL en casse mixte créé via `POSTGRES_USER` exigerait des identifiants cités dans chaque commande `psql`/`pg_dump`, source d'erreurs opérateur).
>
> **Note N2 — volumes nommés (écart justifié avec install.md).** Le runbook d'installation proposait des bind mounts (`./postgres`, implicitement l'état applicatif sur l'hôte). Cette spec retient les **volumes nommés** `akerdock_pgdata` et `akerdock_data` : gestion des UID/permissions par Docker (l'image distroless tourne non-root, l'image postgres avec son propre UID — les bind mounts imposent des chown manuels), sémantique de sauvegarde claire (l'état restaurable passe par `pg_dump`/`pg_restore`, jamais par une copie de fichiers — ADR-021 « backups PostgreSQL standards »), et volume déplaçable indépendamment du répertoire compose. Les éléments que l'opérateur doit toucher ou exfiltrer restent des fichiers de l'hôte sous `/var/lib/akerdock/` (compose, `.env`, `keys/master.key`, `backups/` — §5.1) : la procédure « 3 pièces » de [control-plane-restore.md](../runbooks/control-plane-restore.md) est inchangée, aucun volume nommé n'a besoin d'être sauvegardé (§5.3).

### 4.2 `docker-compose.override.yml` — customisations persistantes

Les personnalisations locales vont dans `/var/lib/akerdock/docker-compose.override.yml`, chargé automatiquement par Compose v2 et **jamais touché par les upgrades** (parité avec le `docker-compose.custom.yml` de la référence, §14.1 PRD) : les releases ne remplacent que `docker-compose.yml`. Exemple documenté :

```yaml
# /var/lib/akerdock/docker-compose.override.yml — personnalisations locales, survivent aux upgrades
services:
  akerdock:
    environment:
      AKERDOCK_LOG_LEVEL: debug
      AKERDOCK_WORKER_CONCURRENCY: "20"
      OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector.interne:4317
    deploy:
      resources:
        limits:
          memory: 2g

networks:
  akerdock:
    ipam:
      config:
        - subnet: 172.30.0.0/24        # plage CIDR Docker custom (§14.1 PRD)
```

Règles : l'override ne doit **ni** changer les images/tags (l'upgrade passe par `AKERDOCK_TAG`), **ni** publier de port supplémentaire (§27.1), **ni** retirer les healthchecks. Un override qui casse ces invariants est hors support.

---

## 5. Arborescence `/var/lib/akerdock/` de l'instance

Distincte de l'arborescence `/var/lib/akerdock/` **des serveurs cibles** (applications, proxy, sources — deployment-engine §5.1, normative là-bas) : même nom de racine (parité §7.5 PRD, « tout l'état = un répertoire »), contenus différents. Quand l'instance gère aussi son propre hôte comme serveur cible (`localhost`), les deux arborescences coexistent sous la même racine sans collision de sous-répertoires.

### 5.1 Sur l'hôte de l'instance

```text
/var/lib/akerdock/                          # racine, 0750, root
├── docker-compose.yml                   # fichier de référence de la release (§4.1), remplacé à l'upgrade
├── docker-compose.override.yml          # optionnel — customisations persistantes (§4.2)
├── .env                                 # 0600 — AKERDOCK_TAG, POSTGRES_PASSWORD, AKERDOCK_PORT, AKERDOCK_ROOT_*…
├── keys/                                # 0700
│   └── master.key                       # 0600 root:root — clé maître multi-versions (§3)
└── backups/                             # dumps locaux (pré-upgrade, « Backup Now ») — monté dans postgres (/backups)
```

C'est exactement le périmètre à exfiltrer hors machine : `docker-compose.yml` + override + `.env` + `keys/master.key` (+ les dumps, dont la copie de référence vit sur S3 via le plan `is_instance_backup`) — les « 3 pièces » de [control-plane-restore.md](../runbooks/control-plane-restore.md).

### 5.2 Dans le volume `akerdock_data` (monté sur `AKERDOCK_DATA_DIR`, défaut `/var/lib/akerdock` dans le container)

```text
/var/lib/akerdock/                          # dans le container = volume nommé akerdock_data
├── ssh/
│   └── instance_ed25519.pub             # copie de la clé publique d'instance (§6.2) — la privée est en base, chiffrée
└── tmp/                                 # espace temporaire du processus (téléchargements, staging), purgé au démarrage
```

### 5.3 Invariant : rien d'irrécupérable hors base + clé maître

Le volume `akerdock_data` ne contient **aucun état irrécupérable** : tout ce qui s'y trouve est régénérable depuis PostgreSQL + `master.key` (la clé SSH privée d'instance est stockée chiffrée dans `private_keys`, pas dans le volume). Conséquence : ni `akerdock_data` ni `akerdock_pgdata` (couvert par `pg_dump`) n'entrent dans le plan de sauvegarde — le contrat de restore reste « dump + `master.key` + compose/`.env` » (§22.1 PRD).

---

## 6. Cycle de vie

### 6.1 Séquence de démarrage (tous démarrages)

Ordre normatif, chaque étape bloquante :

1. **Chargement et validation de la configuration** (§7) — toute erreur est fatale, listée exhaustivement ;
2. **Connexion PostgreSQL** — retry avec backoff pendant 30 s au plus (couvre la fenêtre `depends_on: service_healthy`), puis erreur fatale ;
3. **Migrations SQL versionnées, automatiques** (ADR-025) — appliquées au boot par le binaire, avant toute écoute réseau ; conçues compatibles rolling upgrade (§18.2 PRD) pour les modes multi-instance. Log explicite `migrations applied` (repère utilisé par [upgrade-downgrade.md](../runbooks/upgrade-downgrade.md)). Une migration en échec = arrêt immédiat, base laissée dans l'état de la dernière migration complète (chaque migration est transactionnelle) ;
4. **Chargement de la clé maître** (§3) — parsing strict, contrôle des permissions, auto-test chiffrer/déchiffrer avec la version active ; tout échec = refus de démarrer (§6.4) ;
5. **Amorçages de premier démarrage** (§6.2) et **bootstrap du root user** (§6.3), le cas échéant ;
6. **Écoute** sur `0.0.0.0:AKERDOCK_PORT` et démarrage des composants du mode (`api` : HTTP seul ; `worker` : consommation de la queue ; `scheduler` : crons sous verrou advisory ; `all-in-one` : les trois). En modes `worker`/`scheduler` purs, le port ne sert que `/api/v1/health`.

### 6.2 Premier démarrage (idempotent, rejouable)

Détecté par l'état de la base, pas par un fichier marqueur — chaque action est individuellement idempotente :

- **Singleton `instance_settings`** créé s'il n'existe pas, amorcé avec `AKERDOCK_INSTANCE_FQDN` (si présente) et `AKERDOCK_TIMEZONE`. Aux démarrages suivants, ces variables **n'écrasent jamais** la base ; une divergence produit un avertissement (§7.2) ;
- **Clé SSH d'instance** : génération d'une paire ed25519 sans passphrase si aucune clé d'instance n'existe — privée chiffrée en base (`private_keys`, chiffrement enveloppe §2.7 du data dictionary), publique recopiée en `AKERDOCK_DATA_DIR/ssh/instance_ed25519.pub` pour que l'opérateur puisse la déposer sur le serveur `localhost` ou un premier serveur cible ;
- **Bootstrap root user** (§6.3) ;
- **Serveur `localhost` pré-enregistré** (PRD §3) : dès qu'une team existe (celle du root — au même démarrage si le trio `AKERDOCK_ROOT_*` est fourni, sinon à un démarrage ultérieur), une fiche serveur nommée `localhost` (`is_localhost = true`, statut `pending`) est créée dans la team la plus ancienne, pointant `AKERDOCK_LOCALHOST_HOST`:22 avec l'utilisateur `AKERDOCK_LOCALHOST_USER` et la clé SSH d'instance. Amorçage **une seule fois** dans la vie de l'instance (`instance_settings.localhost_seeded`) : si l'opérateur supprime ce serveur, il n'est jamais recréé — l'amorçage ne recrée pas ce que l'opérateur a détruit. Tant que ce serveur n'a **jamais** été validé, le scheduler retente sa validation à chaque tick de maintenance, pendant 24 h après sa création : dès que la clé publique d'instance est autorisée sur l'hôte — ce que `install.sh` fait automatiquement pour l'utilisateur qui installe — il passe `ready` sans action de l'opérateur. Passé ce délai, ou après une première validation réussie, son cycle de vie redevient entièrement celui d'un serveur ordinaire (validation manuelle PRD §20.1).

### 6.3 Bootstrap du premier root user (§10.2 PRD)

- Les variables `AKERDOCK_ROOT_EMAIL` / `AKERDOCK_ROOT_NAME` / `AKERDOCK_ROOT_PASSWORD` ne sont **lues que si aucun utilisateur n'existe** en base ; elles sont **consommées une seule fois** — dès qu'un utilisateur existe, elles sont ignorées (avertissement si encore présentes, §7.2) ;
- Validation **stricte et bloquante** : email syntaxiquement valide et normalisé, nom non vide (§2.1), mot de passe ≥ 12 caractères ; tout échec = **erreur fatale** au démarrage (jamais de root créé « au rabais », jamais de démarrage sans root alors que le trio était fourni) ;
- Le mot de passe est hashé Argon2id (§23.2 PRD) et n'apparaît dans aucun log ni événement d'audit ; la création du root est auditée (§23.4) ;
- Alternative sans variables : l'onboarding guidé de l'UI crée le root au premier accès (§14.2 PRD) ;
- Après le premier démarrage réussi, retirer `AKERDOCK_ROOT_PASSWORD` de `.env` ([install.md](../runbooks/install.md) étape 4).

### 6.4 Clé maître absente ou invalide : refus de démarrer

Aucun mode dégradé silencieux, dans tous les cas ci-dessous l'instance **refuse de démarrer** avec un message actionnable nommant le problème exact :

| Situation | Message (contenu minimal) |
|---|---|
| Ni `AKERDOCK_MASTER_KEY_FILE` ni `AKERDOCK_MASTER_KEY` | variable manquante + renvoi vers [install.md](../runbooks/install.md) étape 2 |
| Les deux fournies | conflit de sources, en retirer une |
| Fichier absent / illisible | chemin exact + permissions attendues |
| Format invalide (§3.1) | numéro de ligne fautive + règle violée (sans jamais journaliser le contenu de la ligne) |
| Permissions trop ouvertes (lisible/inscriptible par « other ») | mode actuel vs `0600` attendu |
| Auto-test AEAD en échec | version active concernée |

À chaud, une `key_version` référencée en base mais absente du fichier produit une erreur explicite par opération (§3.4.3) et un compteur d'erreurs OTLP — jamais une valeur vide ou un secret « sauté ».

### 6.5 Arrêt gracieux

Sur `SIGTERM`/`SIGINT` : arrêt de l'écoute des nouvelles requêtes et du leasing de nouveaux jobs, drain des jobs en cours pendant au plus `AKERDOCK_SHUTDOWN_TIMEOUT` (défaut 30 s), heartbeats maintenus pendant le drain, puis arrêt. Les jobs non terminés sont repris après expiration de leur lease (90 s) par inspection distante, jamais rejoués à l'aveugle (§22.1 PRD, deployment-engine §2.5). Le `stop_grace_period: 40s` du compose (§4.1) laisse le drain se terminer avant le `SIGKILL` de Docker.

### 6.6 Healthcheck

- Endpoint HTTP : **`GET /api/v1/health`**, non authentifié, disponible même API désactivée (OpenAPI `/health`), sur le port unique. `200` = processus vivant, base joignable, clé maître chargée ;
- Sous-commande **`/akerdock healthcheck`** : requête locale sur `/api/v1/health`, code de sortie 0/1 — c'est le healthcheck du compose (l'image distroless n'ayant ni shell ni curl, ADR-021).

---

## 7. Validation au démarrage

### 7.1 Erreurs fatales — collecte exhaustive, jamais de démarrage partiel

Toutes les vérifications de configuration sont exécutées **avant** d'ouvrir le port et de toucher la queue ; les erreurs sont **toutes collectées puis listées ensemble** (l'opérateur corrige tout en un cycle), et le processus sort en code `1` :

```text
FATAL configuration invalide (3 erreurs) :
  - AKERDOCK_DATABASE_URL : absente (requise — DSN PostgreSQL, spec instance-config §2)
  - AKERDOCK_MODE : valeur "workers" invalide (attendu : all-in-one|api|worker|scheduler)
  - AKERDOCK_ROOT_EMAIL : fournie sans AKERDOCK_ROOT_NAME/AKERDOCK_ROOT_PASSWORD (trio tout-ou-rien)
```

Sont fatals notamment : variable requise absente (§1.2) ; valeur hors domaine (`AKERDOCK_MODE`, `AKERDOCK_LOG_LEVEL`, `AKERDOCK_LOG_FORMAT`) ; `AKERDOCK_PORT` hors `1–65535` ; DSN non parsable ; timezone IANA inconnue ; durée/entier non parsable (`AKERDOCK_SHUTDOWN_TIMEOUT`, `AKERDOCK_WORKER_CONCURRENCY < 1`) ; trio `AKERDOCK_ROOT_*` incomplet ou invalide (§6.3) ; conflits et invalidités de clé maître (§6.4) ; `AKERDOCK_CONFIG_FILE` illisible ou YAML invalide ; `AKERDOCK_DATA_DIR` non inscriptible. Les messages d'erreur ne reproduisent **jamais** la valeur d'une variable sensible (§2, colonne « sensible »).

### 7.2 Avertissements (démarrage autorisé, log `warn` au boot)

- `AKERDOCK_MASTER_KEY` utilisée au lieu d'un fichier (§2.1) ;
- variables `AKERDOCK_ROOT_*` encore présentes alors que des utilisateurs existent — recommander leur retrait ([install.md](../runbooks/install.md)) ;
- permissions de `master.key` différentes de `0600` sans être ouvertes à « other » (§3.3) ;
- `AKERDOCK_INSTANCE_FQDN`/`AKERDOCK_TIMEZONE` divergeant de la valeur en base après le premier démarrage (la base fait foi, §6.2) ;
- `sslmode=disable` dans `AKERDOCK_DATABASE_URL` vers un hôte hors du réseau compose (§4.1) ;
- variable d'environnement `AKERDOCK_*` inconnue (détection de faute de frappe : `AKERDOCK_LOGLEVEL` → suggestion `AKERDOCK_LOG_LEVEL`).

Chaque avertissement est émis une fois au démarrage, en clair dans les logs — jamais répété en boucle, jamais silencieux.

---

## Traçabilité

| Section | Rend normatif | Référencé par |
|---|---|---|
| §2 (port 8080, noms de variables) | §27.1 PRD, ADR-022 | install.md (prérequis réseau, `.env`) |
| §3 (format `version:base64`, active = plus haute) | ADR-003, §23.2/§27.3 PRD, data dictionary §2.7 | install.md étape 2, key-rotation.md §A |
| §4 (compose 2 services) | ADR-021, §27.21/§14.1 PRD | install.md étape 3 |
| §6 (migrations auto au boot, bootstrap root une seule fois) | ADR-025, §10.2/§18.2 PRD | install.md étape 4, upgrade-downgrade.md §A |
| §5–§6 (arborescence instance, `/api/v1/health`, arrêt gracieux) | §7.5/§22.1 PRD, OpenAPI `/health` | control-plane-restore.md, install.md (vérification) |
