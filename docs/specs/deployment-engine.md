# Spécification — Moteur de déploiement

> Artefact §29.4 du PRD (`docs/PRD.md`). Le PRD est la source de vérité ; cette spécification le précise au niveau commande, répertoire, timeout, verrou et compensation. Lorsque le PRD est muet, la valeur retenue est marquée **(défaut proposé)**. Aucune décision d'implémentation Go n'est prise ici ; les contrats (queue, transport SSH, adaptateur runtime, proxy provider) sont ceux du §18.1.
>
> Périmètre : applications (build packs docker image, dockerfile en P0 ; nixpacks, railpack, static en P1). Le build pack **Docker Compose** est traité dans la spécification dédiée (§29.5, `docs/specs/compose-spec.md`) ; seuls ses points de contact avec le moteur (queue, verrous, états) sont mentionnés ici.

---

## 1. Vue d'ensemble

### 1.1 Du trigger au container en production

```text
Trigger (UI / API / webhook / CLI / schedule)
        │  validation, autorisation, snapshot de config, résolution du SHA
        ▼
API : INSERT Deployment (queued) + INSERT job + INSERT outbox  ── même transaction PostgreSQL
        │
        ▼
Queue PostgreSQL (FOR UPDATE SKIP LOCKED, lease + heartbeat)
        │
        ▼
Worker : machine à états (§4) — chaque étape = commandes SSH sur le serveur cible
        │
        ▼
Serveur cible : Docker Engine/BuildKit + proxy (Traefik) + arborescence /data/akerdock/
```

### 1.2 Acteurs

| Acteur | Responsabilité | Ce qu'il ne fait jamais |
|---|---|---|
| **API (control plane)** | Auth/policy, validation, snapshot versionné de la configuration (INV-014), résolution branche → SHA immuable, création transactionnelle `Deployment` + job + événement outbox, réponse `202` avec `deployment_uuid` | Exécuter une commande distante ; attendre la fin du déploiement |
| **Queue (PostgreSQL)** | Durabilité des jobs (INV-013), ordre par priorité/date, leases, retries, dead-letter | Logique métier |
| **Worker** | Acquisition des jobs, verrous et slots, exécution de la machine à états, streaming des logs, compensation, libération garantie des ressources | Servir du trafic HTTP utilisateur ; être source de vérité d'un état |
| **Serveur cible (via SSH)** | Exécution de git/docker/buildkit, hébergement des containers, du proxy et des fichiers sous `/data/akerdock/` | Contacter le control plane (architecture push, §18.1) |
| **Proxy (Traefik, P0)** | Routage du trafic, application de la représentation intermédiaire (§27.9) | Décider de la bascule (le worker pilote) |

### 1.3 Sources de vérité mobilisées (§18.3)

- Configuration désirée : snapshot PostgreSQL figé à l'enqueue — un déploiement rejoué utilise **son** snapshot, jamais la config courante.
- Code source : SHA résolu à l'enqueue, immuable (un push ultérieur = nouveau déploiement, éventuellement coalescé §3.4).
- Image : **digest OCI** résolu avant bascule ; le tag n'est jamais suffisant pour un rollback.
- État distant : Docker du serveur cible, interrogé par inspection — jamais supposé depuis la base (INV-004, §22.1).
- Routage : fichier de configuration dynamique du proxy sur le serveur, généré de façon déterministe depuis la représentation intermédiaire, validé et checksumé.

---

## 2. Queue et jobs

### 2.1 Schéma sémantique

Deux tables (noms indicatifs, le contrat sémantique est normatif) :

**`jobs`** — file durable générique (§21.3), requêtes critiques en SQL explicite (décision §27.25).

| Colonne | Type | Sémantique |
|---|---|---|
| `id` | uuid | Identifiant du job |
| `queue` | text | File logique : `deployments`, `server_ops`, `backups`, `maintenance`… (priorités séparées, §24.3) |
| `type` | text | `deployment.run`, `deployment.cancel_cleanup`… |
| `payload` | jsonb | Références uniquement : `deployment_uuid`, `application_uuid`, `server_uuid` — **jamais de secret** (INV-003) |
| `status` | enum | `scheduled → queued → leased → running → succeeded \| retry_wait \| cancelled \| dead_letter` (§21.3) |
| `priority` | int | Plus petit = plus prioritaire ; défaut `100` **(défaut proposé)** |
| `run_at` | timestamptz | Date d'éligibilité (backoff des retries) |
| `attempt` / `max_attempts` | int | Tentative courante / plafond (§2.4) |
| `leased_by` | text | Identité du worker (`hostname:pid:uuid`) |
| `lease_expires_at` | timestamptz | Expiration du lease |
| `heartbeat_at` | timestamptz | Dernier battement |
| `idempotency_key` | text unique | Déduplication d'enqueue (INV-004, §24.1) |
| `cancel_requested_at` | timestamptz | Annulation coopérative (§2.6) |
| `last_error` | jsonb | Classification de la dernière erreur (code, étape, redacted) |
| `created_at`, `started_at`, `finished_at` | timestamptz | Horodatage UTC (§22.3) |

**`deployments`** — historique métier (§19.1) : `uuid`, `application_uuid`, `server_uuid`, `destination_uuid`, `state` (machine §4), `commit_sha`, `config_snapshot_id`, `image_ref`, `image_digest`, `trigger` (`manual|api|webhook|preview|schedule|config_apply|cli_local` — vocabulaire canonique du data dictionary, le rollback étant porté par `is_rollback`), `webhook_delivery_id`, `forced` (build sans cache), `superseded_by` (coalescing §3.4), `attempt`, timestamps par étape, `finished_at`.

### 2.2 Enqueue transactionnel

Une seule transaction PostgreSQL contient : création du `Deployment` (état `queued`), snapshot de configuration, `INSERT jobs`, `INSERT outbox` (`deployment.queued.v1`), et la vérification du plafond de file (§3.2). Commit = le job existe et survivra à tout crash (INV-013). Après commit : `NOTIFY akerdock_jobs` pour réveiller les workers **(défaut proposé)**.

### 2.3 Acquisition, lease, heartbeat

Acquisition par un worker (sémantique exacte) :

```sql
SELECT id FROM jobs
WHERE queue = $1 AND status = 'queued' AND run_at <= now()
ORDER BY priority, run_at
FOR UPDATE SKIP LOCKED
LIMIT 1;
-- puis, même transaction :
UPDATE jobs SET status = 'leased', leased_by = $worker,
  lease_expires_at = now() + interval '90 seconds',
  heartbeat_at = now()
WHERE id = $id;
```

Les contraintes de slots et de verrous (§3) sont vérifiées **dans la même transaction** que l'acquisition ; si un slot manque, le job est laissé en `queued` (avec `run_at = now() + 5 s` pour éviter le busy-loop) et le worker passe au suivant.

| Paramètre | Valeur | Note |
|---|---|---|
| Durée du lease | **90 s** | Renouvelé par heartbeat **(défaut proposé)** |
| Intervalle de heartbeat | **20 s** | `UPDATE jobs SET heartbeat_at = now(), lease_expires_at = now() + 90 s WHERE id = $1 AND leased_by = $worker` ; un échec de heartbeat (lease perdu) impose au worker d'**abandonner immédiatement** le job sans nouvelle mutation distante **(défaut proposé)** |
| Réveil | `LISTEN akerdock_jobs` + polling de secours toutes les **5 s** **(défaut proposé)** |
| Scan des leases expirés | Toutes les **30 s** : `status IN ('leased','running') AND lease_expires_at < now()` → remise en `queued` avec `attempt` inchangé et marqueur `recovered = true` **(défaut proposé)** |

### 2.4 Retry, backoff, dead-letter

Classification obligatoire de chaque échec (§22.1) :

- **Erreur transitoire d'infrastructure** (SSH injoignable, timeout réseau, registry 5xx, disque distant momentanément saturé) → retry automatique.
- **Erreur déterministe** (échec de build, healthcheck jamais sain, commande pre/post en échec, config invalide, `${VAR:?}` vide) → **aucun retry automatique** ; le déploiement passe en `failed`, retry manuel possible (§21.1 : `failed → retrying → preparing`, nouvelle tentative explicitement liée).

| Paramètre | Valeur |
|---|---|
| `max_attempts` (jobs `deployment.run`) | **3** (1 exécution + 2 retries automatiques d'infra) **(défaut proposé)** |
| Backoff | `delay = min(30 s × 2^(attempt−1), 15 min)` **(défaut proposé)** |
| Jitter | Full jitter : `run_at = now() + random(0, delay)` (bornes : 0 s – 15 min) **(défaut proposé)** |
| Dead-letter | `attempt ≥ max_attempts` → `status = 'dead_letter'`, `Deployment.state = failed`, événement `deployment.failed.v1`, notification, entrée dashboard « actions prioritaires ». Rejeu depuis la dead-letter = action manuelle auditée qui crée une **nouvelle tentative liée** |

### 2.5 Reprise après crash (INV-004, INV-013, §22.1)

Un job récupéré après expiration de lease n'est **jamais rejoué à l'aveugle**. Le worker repreneur exécute d'abord une **inspection distante** puis décide reprendre / compenser / terminer :

1. Lire `Deployment.state` et les métadonnées d'étape en base (dernier checkpoint committé).
2. Inspecter le serveur : `docker image inspect` de l'image attendue (label `akerdock.deployment_uuid`), `docker container inspect <uuid>-next` et `<uuid>`, checksum du fichier proxy, présence du répertoire de clone.
3. Appliquer la règle de reprise de l'état concerné (colonne « crash pendant l'état » du tableau §4).

Règle générale : tout effet distant est **idempotent ou détectable** — les objets créés portent le label `akerdock.deployment_uuid=<uuid>` qui permet de savoir si l'étape a déjà produit son effet avant de la rejouer.

### 2.6 Annulation coopérative

- L'annulation (UI/API, §5.5) écrit `cancel_requested_at` et publie `NOTIFY`.
- Le worker vérifie le drapeau à **chaque checkpoint** (= chaque transition d'état de la machine §4) et **avant toute commande distante longue** (clone, build, pull, push).
- Pour interrompre une commande en cours : fermeture du canal SSH avec envoi de signal, les commandes longues étant lancées via `timeout -k 10 <secs> <cmd>` côté distant pour garantir la terminaison **(défaut proposé)**.
- **Barrière d'annulation** : à partir de l'entrée en `switching`, l'annulation est refusée (la bascule est atomique : elle se termine ou est compensée, jamais interrompue).
- Après annulation : compensation identique à un échec au même point (§9), état terminal `cancelled`, libération des verrous/slots, nettoyage du candidat.

---

## 3. Verrous et contrôle de concurrence

Tous les verrous sont matérialisés en PostgreSQL (verrous multi-instance, §18.2). Libération **garantie** : chaque acquisition est enregistrée avec le `job_id` détenteur ; la fin du job (succès, échec, panique, annulation) passe par un point de sortie unique qui libère verrous et slots (sémantique defer/finally) ; de plus, le scan des leases expirés (§2.3) libère les verrous des jobs morts.

### 3.1 Verrou exclusif par (application, destination)

- **Un seul déploiement actif** (états `preparing` → `finishing`) par couple `(application_uuid, destination_uuid)` : les autres attendent en `queued`. Les previews de PR ont leur propre identité (§20.4) donc leur propre verrou.
- L'état `switching` est en outre protégé par ce même verrou de façon **stricte** (§21.1) : aucune reprise après crash ne peut re-basculer tant que l'inspection n'a pas déterminé l'issue de la bascule précédente (pas de double bascule, §16.4).
- Implémentation sémantique : ligne `resource_locks(application_uuid, destination_uuid, holder_job_id, acquired_at)` avec contrainte d'unicité, prise dans la transaction d'acquisition du job.

### 3.2 Slots et plafonds par serveur (§5.5)

| Paramètre | Défaut | Sémantique |
|---|---|---|
| `concurrent_builds` | **2** | Nombre max de jobs de déploiement en exécution simultanée par serveur (compte de `jobs` en `leased/running` ciblant le serveur, vérifié à l'acquisition). Un déploiement sans build (docker image, rollback) consomme aussi un slot : il exécute pull/start/bascule sur le même Docker **(défaut proposé : slot unique, pas de file séparée)** |
| `deployment_queue_limit` | **25** | Nombre max de déploiements `queued` par serveur ; au-delà, l'enqueue est **refusé** à l'API avec `429` et code d'erreur stable (`deployment_queue_full`) **(défaut proposé : refus plutôt que blocage)** |

Les deux valeurs sont configurables par serveur ; la limite par team (§22.2) s'applique en plus, avec la même règle.

### 3.3 Ordre dans la file

FIFO par `(priority, run_at)` au sein d'un serveur. Un redéploiement manuel « urgent » PEUT recevoir `priority = 50` **(défaut proposé)**. La vue « déploiements en cours/en attente » (§5.5) lit directement `deployments` + `jobs`.

### 3.4 Coalescing des pushes (§20.3.5)

À l'enqueue d'un déploiement déclenché par webhook pour `(application, branche)` :

1. Chercher un déploiement existant `queued` (job non encore `leased`) pour la même application et la même branche, issu d'un webhook.
2. S'il existe avec un SHA plus ancien : le marquer `superseded` (état terminal assimilé à `cancelled`, `superseded_by = <nouveau uuid>`), annuler son job, créer le nouveau déploiement au SHA récent. La livraison webhook d'origine reste tracée et pointe vers le déploiement qui l'a remplacée.
3. Un déploiement déjà `leased`/en cours n'est **jamais** coalescé : il se termine, le nouveau attend le verrou §3.1.

Fenêtre de coalescing = tant que le job est en `queued` ; aucune temporisation artificielle **(défaut proposé)**.

---

## 4. Machine à états du déploiement (§21.1)

```text
queued → preparing → cloning → building → pushing? → starting
   └──────────────────────────────────────────────→ cancelled
starting → healthchecking → switching → finishing → succeeded
    └──────────────→ failed ←──────────────────────────┘
failed → retrying → preparing
```

Chaque transition est committée en base **avant** l'action distante de l'état suivant (write-ahead : l'état en base dit « ce qui a pu commencer », l'inspection distante dit « ce qui a réellement eu lieu »). Chaque transition publie un événement outbox (§12). `cancelled`, `failed`, `succeeded` sont terminaux pour une tentative.

Les build packs sans étape de build (docker image, rollback) traversent `cloning`/`building`/`pushing` en no-op (transition immédiate, tracée). Détail des actions par build pack au §5.

| État | Préconditions | Actions exactes | Effets de bord distants | Timeout | Crash pendant l'état (règle de reprise) | Transition d'échec |
|---|---|---|---|---|---|---|
| **queued** | Deployment + job committés ; plafond §3.2 respecté | Attente d'acquisition ; cible du coalescing §3.4 | Aucun | Aucun (borné par `deployment_queue_limit`) | Rien à reprendre (aucun effet) | Annulation → `cancelled` |
| **preparing** | Verrou §3.1 acquis, slot §3.2 acquis, serveur `ready` | Charger le snapshot de config ; connexion SSH (test `docker info`) ; vérifier espace disque (`df -P /data/akerdock`, seuil min **2 GiB libres (défaut proposé)**) ; créer l'arborescence (§5.1) ; générer et téléverser `build.env`, `runtime.env`, `secrets/` (§5.7) ; exécuter la **pre-deployment command** (§10) ; vérifier le réseau de destination (`docker network inspect`, créer si absent) | Répertoires + fichiers env (0600) ; réseau destination | SSH connect : **10 s** (configurable par serveur, §3.1 PRD) ; état complet : **120 s (défaut proposé)** | Idempotent : tout rejouer (mkdir -p, ré-upload des fichiers, re-exécution de la pre-command — elle DOIT être idempotente, documenté §10) | `failed` ; compensation C1 (§9) |
| **cloning** | Source Git ; credentials valides | Commandes §5.3.1 : clone shallow au SHA exact, submodules/LFS si activés | Répertoire `source/<deployment_uuid>/` | **600 s (défaut proposé)**, configurable par application | Répertoire potentiellement partiel → `rm -rf` du répertoire de ce deployment puis re-clone (idempotent par destruction) | `failed` ; C1 |
| **building** | Source présente ; plan de build généré | Commandes §5.3.2/§5.4/§5.5/§5.6 selon build pack ; logs streamés (§12.2) ; `--no-cache` si `forced` | Image locale `akerdock/<app_uuid>:<sha12>` avec labels §6 | **3600 s (défaut proposé)**, configurable par application | `docker image inspect akerdock/<app_uuid>:<sha12>` + label `akerdock.deployment_uuid` : si présente et complète → passer à l'état suivant ; sinon relancer le build (le cache BuildKit rend le rejeu peu coûteux) | `failed` (déterministe : pas de retry auto) ; C1 |
| **pushing** *(optionnel)* | Registry configuré (décision §27.6) ou exigé (build server, multi-serveurs) | `docker tag` + `docker push` (§5.3.3) ; **résolution du digest OCI** et enregistrement dans `DeploymentArtifact` | Image dans le registry | **900 s (défaut proposé)** | Push idempotent (layers dédupliqués) → rejouer ; re-résoudre le digest | `failed` si registry obligatoire ; **sinon** dégradation en mode rétention locale + avertissement **(défaut proposé)** ; C1 |
| **starting** | Image présente (locale ou pullée) ; digest résolu | Créer volumes manquants (§6.3) ; `docker create` du candidat `<uuid>-next` + `docker start` (§5.3.4) ; en mode non-rolling (§7.4) : arrêt de l'ancien d'abord | Container candidat ; volumes | Création + démarrage : **60 s (défaut proposé)** | `docker container inspect <uuid>-next` : absent → recréer ; présent `created` → start ; présent `running` → continuer ; présent `exited` → supprimer et recréer | `failed` ; C2 (§9) |
| **healthchecking** | Candidat `running` | Polling `docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' <uuid>-next` toutes les **2 s (défaut proposé)** jusqu'à `healthy` ; sans healthcheck défini : app **inéligible au rolling** (§7.3), en mode non-rolling attendre `running` stable **10 s (défaut proposé)** ; puis **post-deployment command** dans le candidat (§10) | Aucun (lecture + exec) | `start_period + (interval + timeout) × retries + 30 s` de marge (défauts §11 → ~135 s) | Relire l'état de santé : `healthy` → continuer ; `unhealthy`/disparu → `failed` + C2 ; en cours → reprendre le polling | `failed` (déterministe) ; C2 ; les logs du candidat (`docker logs --tail 200 <uuid>-next`) sont capturés dans le log de build avant suppression **(défaut proposé)** |
| **switching** | Candidat `healthy` + post-command OK ; verrou §3.1 strict ; **barrière d'annulation active** | Algorithme §7.2 : génération de la représentation intermédiaire → fichier dynamique Traefik → application atomique → vérification ; puis arrêt gracieux de l'ancien, `docker rename` du candidat | Fichier proxy modifié ; ancien container arrêté/supprimé ; candidat renommé `<uuid>` | **120 s (défaut proposé)** hors stop grace ; + `stop_grace_period` | **Cas critique.** Inspection : (a) fichier proxy pointe encore l'ancien → candidat toujours sain ? oui : rejouer la bascule ; non : `failed` + C2 ; (b) fichier pointe le candidat, ancien encore présent → reprendre à l'arrêt de l'ancien ; (c) ancien absent, rename non fait → reprendre au rename ; jamais de seconde bascule sans cette inspection (INV-004/005) | Avant application proxy : `failed` + C2. Après application vérifiée : la bascule a eu lieu → poursuivre vers `finishing` si possible, sinon `failed` + C3 (§9) |
| **finishing** | Trafic basculé, container final nommé `<uuid>` | Mettre à jour la config proxy vers la forme stable (§7.2 étape 7) ; enregistrer `DeploymentArtifact` (digest, tags conservés) ; marquer les images de rollback protégées (§8.2) ; planifier le nettoyage asynchrone (sources anciennes, images hors rétention — jamais inline) ; mettre à jour l'état observé de la ressource | Fichier proxy stabilisé ; métadonnées | **60 s (défaut proposé)** | Toutes les actions sont idempotentes → rejouer intégralement | Échec ici = déploiement **réussi avec avertissement** (`succeeded` + événement `deployment.finishing_degraded.v1`) : le trafic est déjà sur la nouvelle version, ne jamais la casser pour un nettoyage **(défaut proposé)** |
| **succeeded** | — | Événement + notification ; libération verrou/slot | — | — | — | — |
| **failed** | — | Compensation exécutée (§9) ; événement + notification ; libération verrou/slot ; classification de l'erreur (§2.4) | — | — | Compensation elle-même idempotente et rejouable | — |
| **cancelled** | Annulation avant la barrière | Compensation au point courant (§9) ; libération verrou/slot | — | — | Idem `failed` | — |
| **retrying** | Action manuelle sur `failed`, ou retry auto d'infra | Nouvelle tentative liée (`attempt + 1`, historique conservé §21.1) → `preparing` avec le **même snapshot et le même SHA** | — | — | — | — |

---

## 5. Plan d'exécution par build pack

### 5.1 Arborescence distante (normative)

```text
/data/akerdock/                              # racine, 0750, propriétaire = user SSH AkerDock
├── applications/<app_uuid>/
│   ├── source/<deployment_uuid>/             # clone jetable par déploiement, purgé en finishing (rétention : dernier + courant)
│   ├── env/
│   │   ├── build.env                         # variables build-time non secrètes (0600)
│   │   ├── runtime.env                       # variables runtime, --env-file du container (0600)
│   │   └── secrets/<VAR_NAME>                # un fichier par build secret BuildKit (0600)
│   └── keys/deploy_key                       # clé de déploiement Git éphémère si nécessaire (0600, supprimée après clone)
├── proxy/
│   ├── dynamic/<app_uuid>.yaml               # config dynamique Traefik par application (§7)
│   └── certs/                                # certificats custom (§4.3 PRD)
├── backups/                                  # hors périmètre de cette spec (§20.5)
└── tmp/                                      # espace temporaire, purgé par le cleanup
```

**(défaut proposé)** pour l'ensemble de l'arborescence : tout l'état d'un serveur cible vit sous `/data/akerdock`, ce qui rend l'inventaire, le backup et le nettoyage évidents (§14.1 PRD). Tous les fichiers contenant des valeurs de variables sont en mode `0600`, répertoires `0700`.

### 5.2 Variables : build-time vs runtime (§5.4 PRD, INV-003, INV-012)

| Catégorie | Matérialisation | Consommation |
|---|---|---|
| Runtime | `env/runtime.env` (`KEY=value`, multiline via quoting) | `docker create --env-file …` — jamais de `-e KEY=value` sur la ligne de commande |
| Build-time non secrète | `env/build.env` | `--build-arg KEY` **sans valeur dans argv** : la valeur est lue depuis l'environnement du processus `docker`, exporté depuis `build.env` (via `set -a; . build.env; set +a` dans la session distante) — rien de sensible dans `ps` (INV-012) |
| Build secret (opt-in BuildKit) | Un fichier par secret : `env/secrets/<NAME>` | `--secret id=<NAME>,src=…/env/secrets/<NAME>` ; consommé dans le Dockerfile par `RUN --mount=type=secret,id=<NAME>` ; absent des layers et de l'historique d'image |
| Prédéfinies (décision §27.22) | Injectées dans `runtime.env` : `AKERDOCK_FQDN`, `AKERDOCK_URL`, `AKERDOCK_BRANCH`, `AKERDOCK_RESOURCE_UUID`, `AKERDOCK_CONTAINER_NAME`, `PORT`, `HOST`, `AKERDOCK_PR_ID` (previews) ; `SOURCE_COMMIT` en build arg **opt-in** (§5.2 PRD) | Comme les catégories ci-dessus |

Règles :
- Interpolation des shared variables (`{{team.VAR}}`…) et vérification des variables requises `${VAR:?}` **côté control plane, avant enqueue** — un manquement bloque le déploiement en validation, pas à mi-build.
- Transfert des fichiers par **SFTP** (contenu jamais dans argv ni dans un heredoc de commande loggée), mode `0600` posé à l'upload.
- Les fichiers `env/` sont réécrits à chaque déploiement depuis le snapshot ; un déploiement rejoué reproduit exactement les mêmes fichiers (INV-014).

### 5.3 Build pack `dockerfile` (P0)

#### 5.3.1 Clone (état `cloning`)

Clone shallow **au SHA exact** (un `git clone --depth 1 -b <branche>` suivrait la tête mouvante — interdit) :

```sh
mkdir -p /data/akerdock/applications/<app_uuid>/source/<deployment_uuid>
cd /data/akerdock/applications/<app_uuid>/source/<deployment_uuid>
git init -q
git remote add origin <repo_url_sans_credentials>
git fetch -q --depth 1 origin <commit_sha>
git checkout -q --detach FETCH_HEAD
# si submodules activés :
git submodule update --init --recursive --depth 1
# si LFS activé :
git lfs install --local && git lfs pull
```

Authentification (INV-003, INV-012) — jamais de credential dans l'URL ni dans argv :
- **Deploy key SSH** : clé téléversée en `keys/deploy_key` (0600), `GIT_SSH_COMMAND="ssh -i /data/akerdock/applications/<app_uuid>/keys/deploy_key -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"` posé dans l'environnement de la session ; fichier supprimé en fin d'état.
- **Token HTTPS (GitHub App / PAT)** : `git config credential.helper 'store --file=/data/akerdock/applications/<app_uuid>/keys/git_credentials'` avec fichier téléversé par SFTP (0600), supprimé en fin d'état **(défaut proposé)**.

Le **base directory** (monorepo) ne change pas le clone ; il change le contexte de build (`<clone>/<base_directory>`).

#### 5.3.2 Build (état `building`)

```sh
cd /data/akerdock/applications/<app_uuid>/source/<deployment_uuid>/<base_directory>
set -a; . /data/akerdock/applications/<app_uuid>/env/build.env; set +a
DOCKER_BUILDKIT=1 docker build \
  --file <dockerfile_location>            # défaut : ./Dockerfile
  --progress plain \
  --tag akerdock/<app_uuid>:<sha12> \
  --label akerdock.managed=true \
  --label akerdock.resource_uuid=<app_uuid> \
  --label akerdock.team_uuid=<team_uuid> \
  --label akerdock.deployment_uuid=<deployment_uuid> \
  --label akerdock.commit_sha=<commit_sha> \
  --build-arg AKERDOCK_FQDN --build-arg AKERDOCK_BRANCH … \   # build args auto-injectés, désactivables (§5.2 PRD) ; valeurs via env, pas argv
  [--build-arg SOURCE_COMMIT]             # opt-in
  [--secret id=<NAME>,src=/data/akerdock/applications/<app_uuid>/env/secrets/<NAME>]…
  [--no-cache]                            # si forced (deploy webhook force=true, §5.5 PRD)
  .
```

`<sha12>` = 12 premiers caractères hexadécimaux du commit SHA **(défaut proposé)**. Builds exécutés via le BuildKit du Docker du serveur en P0/P1 (décision §27.5) ; le contrat d'adaptateur build est défini dès P0 pour la bascule ultérieure vers des builders rootless — cette spec n'utilise que des opérations exprimables dans les deux modes.

#### 5.3.3 Push (état `pushing`, si registry configuré — décision §27.6)

```sh
# login : mot de passe via stdin, jamais argv
printf '%s' "$REGISTRY_PASSWORD" | docker login <registry_host> --username <user> --password-stdin
docker tag akerdock/<app_uuid>:<sha12> <registry>/<image>:<sha12>
[docker tag akerdock/<app_uuid>:<sha12> <registry>/<image>:<tag_custom>]   # §5.2 PRD
docker push <registry>/<image>:<sha12>
# résolution du digest OCI (source de vérité de l'artifact, §18.3) :
docker image inspect --format '{{index .RepoDigests 0}}' <registry>/<image>:<sha12>
```

Le digest (`<registry>/<image>@sha256:…`) est enregistré dans `DeploymentArtifact` avant toute bascule.

#### 5.3.4 Création et démarrage du candidat (état `starting`)

```sh
# volumes déclarés (§6.3) — création idempotente :
docker volume create --label akerdock.managed=true --label akerdock.resource_uuid=<app_uuid> \
  --label akerdock.team_uuid=<team_uuid> <app_uuid>_<volume_name>

docker create \
  --name <app_uuid>-next \
  --network <destination_network> \
  --env-file /data/akerdock/applications/<app_uuid>/env/runtime.env \
  --restart unless-stopped \
  --stop-timeout <stop_grace_period> \
  --label akerdock.managed=true \
  --label akerdock.resource_uuid=<app_uuid> \
  --label akerdock.type=application \
  --label akerdock.team_uuid=<team_uuid> \
  --label akerdock.deployment_uuid=<deployment_uuid> \
  [--health-cmd '<cmd>' --health-interval <i>s --health-timeout <t>s --health-retries <r> --health-start-period <s>s] \  # seulement si pas de HEALTHCHECK Dockerfile (prioritaire, §5.3 PRD)
  [-v <app_uuid>_<volume_name>:<mount_path>]… [-v <host_path>:<mount_path>]… \
  [--memory … --memory-reservation … --memory-swap … --cpus … --cpuset-cpus … --cpu-shares …] \
  [-p <ip:host:container[/proto]>]…       # Ports Mappings — rend l'app inéligible au rolling (§7.3)
  [<custom_docker_options>]               # validées/échappées centralement (INV-012, §23.3)
  <image_ref>                             # digest si registry (<registry>/<image>@sha256:…), tag local sinon
docker start <app_uuid>-next
```

Healthcheck HTTP généré quand path/port sont configurés sans `HEALTHCHECK` Dockerfile :
`--health-cmd 'curl -fsS -X <method> http://127.0.0.1:<port><path> || wget -q -O /dev/null http://127.0.0.1:<port><path>'` (requiert curl ou wget dans l'image, §5.3 PRD ; l'absence des deux = état `unhealthy` documenté avec remédiation).

En mode **non-rolling** (§7.4), l'ancien container est arrêté et supprimé avant `docker create`, et le container est créé directement sous le nom `<app_uuid>`.

### 5.4 Build pack `docker image` (P0)

Pas de source Git : `cloning`/`building` sont des no-ops.

```sh
[printf '%s' "$REGISTRY_PASSWORD" | docker login <registry_host> --username <user> --password-stdin]
docker pull <image>:<tag>
docker image inspect --format '{{index .RepoDigests 0}}' <image>:<tag>   # digest OCI figé pour ce déploiement
```

Timeout du pull : **900 s (défaut proposé)**. La suite (starting → finishing) est identique à §5.3.4, avec `<image>@sha256:…` comme référence. Le pattern « CI externe → deploy webhook » (§5.1 PRD) aboutit ici : pull + redeploy sans rebuild.

### 5.5 Build packs `nixpacks` et `railpack` (P1)

Les deux produisent un Dockerfile/plan, puis rejoignent **exactement** le flux `dockerfile` (§5.3.2 → §5.3.4). Le binaire de build pack est provisionné sur le serveur lors de l'onboarding (§20.1) à une version épinglée par release AkerDock **(défaut proposé)**.

**Nixpacks** — génération du plan puis du contexte, dans l'état `building` :

```sh
cd <clone>/<base_directory>
nixpacks plan . --format json > /data/akerdock/applications/<app_uuid>/source/<deployment_uuid>/.nixpacks-plan.json   # tracé dans les logs de build
nixpacks build . --name akerdock/<app_uuid>:<sha12> \
  --label akerdock.managed=true --label akerdock.resource_uuid=<app_uuid> \
  --label akerdock.deployment_uuid=<deployment_uuid> --label akerdock.commit_sha=<commit_sha> \
  [--install-cmd '<override>'] [--build-cmd '<override>'] [--start-cmd '<override>'] \
  [--config nixpacks.toml] \
  [--no-cache]
```

Les variables build-time sont fournies via l'environnement du processus (`set -a; . build.env; set +a`), Nixpacks propageant l'environnement au build ; pas de valeur en argv. Mode static Nixpacks (publish directory + Nginx) : traité comme §5.6 avec le répertoire produit par le build.

**Railpack (bêta)** — même contrat :

```sh
cd <clone>/<base_directory>
railpack build . --name akerdock/<app_uuid>:<sha12> [--env-file …/build.env] [--no-cache]
```

Les flags exacts de Railpack sont à figer lors de l'implémentation P1 contre la version épinglée ; l'exigence normative est : image taggée `akerdock/<app_uuid>:<sha12>`, labels §6, secrets jamais dans argv ni dans l'image.

### 5.6 Build pack `static` (P1)

1. Clone (§5.3.1).
2. Génération sur le serveur de deux fichiers dans `source/<deployment_uuid>/.akerdock/` : `nginx.conf` (éditable par l'utilisateur dans l'UI, avec option SPA `try_files $uri $uri/ /index.html`) et un Dockerfile minimal :

```dockerfile
FROM nginx:alpine
COPY .akerdock/nginx.conf /etc/nginx/conf.d/default.conf
COPY <publish_directory>/ /usr/share/nginx/html/
```

3. `docker build` identique à §5.3.2 (tag, labels), port interne `80`, puis flux standard.

### 5.7 Build pack `docker compose`

Hors périmètre : voir la spécification Compose (§29.5, à venir). Contrats partagés avec le moteur : mêmes queue/verrous/slots (§2–3), même machine à états (les états `starting/healthchecking/switching` opèrent par service), zero-downtime par service derrière le proxy exigé par la décision §27.15, réseau isolé nommé par UUID, extensions `is_directory`/`content`/`exclude_from_hc`.

---

## 6. Nommage et labels (INV-011, §8, décision §27.22)

### 6.1 Noms

| Objet | Nom | Note |
|---|---|---|
| Container applicatif | `<app_uuid>` | Le nom est aussi le hostname DNS interne (§2 PRD) |
| Container candidat (rolling) | `<app_uuid>-next` | Existe uniquement entre `starting` et la fin de `switching` ; renommé `<app_uuid>` après bascule |
| Container de preview | `<app_uuid>-pr-<pr_id>` | Identité `(application_uuid, provider, pr_id)` (§20.4) **(défaut proposé)** |
| Image locale | `akerdock/<app_uuid>:<sha12>` | + tags registry §5.3.3 |
| Volume | `<app_uuid>_<volume_name>` | Préfixe UUID anti-collision (§8 PRD) |
| Réseau de destination | nom de la `Destination` (UUID pour les réseaux créés par AkerDock) | Stacks compose : réseau propre nommé par UUID (§9 PRD) |
| Fichier proxy | `/data/akerdock/proxy/dynamic/<app_uuid>.yaml` | Un fichier par application → application/suppression atomique par ressource |

Tous les noms sont déterministes, dérivés d'UUID stables, sans entrée utilisateur libre (INV-011). Les custom container names restent possibles (§5.3 PRD) mais rendent l'application inéligible au rolling (§7.3).

### 6.2 Labels système

Posés sur **containers, images, volumes et réseaux** gérés :

| Label | Valeur | Rôle |
|---|---|---|
| `akerdock.managed` | `true` | Frontière géré / non géré (INV-015) : le cleanup et l'adoption s'appuient dessus |
| `akerdock.resource_uuid` | UUID de la ressource | Rattachement au modèle |
| `akerdock.type` | `application` \| `database` \| `service` \| `proxy` \| `helper` | Typage |
| `akerdock.team_uuid` | UUID de la team | Isolation, audit |
| `akerdock.deployment_uuid` | UUID du déploiement | Idempotence des reprises (§2.5) — containers et images |
| `akerdock.commit_sha` | SHA complet | Traçabilité — images |
| `akerdock.retain` | `true` | Protection explicite du cleanup pour les images de rollback (§8.2) **(défaut proposé)** |

Les custom labels utilisateur (§5.3 PRD) sont ajoutés après les labels système et ne peuvent pas les écraser (préfixe `akerdock.` réservé, rejeté en validation).

---

## 7. Zero-downtime (rolling update)

### 7.1 Représentation du routage

Le routage est généré depuis la **représentation intermédiaire** commune (décision §27.9) et matérialisé en **fichier de configuration dynamique Traefik** par application (`/data/akerdock/proxy/dynamic/<app_uuid>.yaml`), monté dans le container Traefik (provider `file` avec `watch: true`). Les labels de routage restent posés sur les containers pour la parité et le diagnostic, mais **le fichier fait foi** — c'est lui qui permet une bascule atomique et vérifiable (checksum, §18.3) **(défaut proposé, conforme à « fichier/labels proxy » §18.3)**.

### 7.2 Algorithme de bascule (états `switching` puis `finishing`)

Précondition : candidat `<uuid>-next` `healthy`, post-command OK, verrou strict §3.1, barrière d'annulation.

1. **Résoudre l'endpoint du candidat** : `docker inspect --format '{{(index .NetworkSettings.Networks "<destination_network>").IPAddress}}' <uuid>-next`. L'IP est stable pour la durée de vie du container : elle sert de cible **transitoire** (le nom `<uuid>-next` disparaîtra au rename, l'IP non).
2. **Générer** la config dynamique depuis la représentation intermédiaire : routers (domaines, path-based avec priorité au path le plus spécifique, redirection www, middlewares), service → `url: http://<ip_next>:<ports_exposes>`.
3. **Appliquer atomiquement** : upload SFTP vers `/data/akerdock/proxy/dynamic/.<app_uuid>.yaml.tmp` puis `mv -f` (rename atomique sur le même système de fichiers) ; enregistrer le checksum SHA-256 en base.
4. **Vérifier** : polling (toutes les 1 s, max **30 s (défaut proposé)**) de l'API locale Traefik (`wget -qO- http://127.0.0.1:8080/api/http/services` exécuté dans le container proxy) jusqu'à voir le nouvel endpoint ; puis requête de fumée à travers le proxy : `curl -fsS -o /dev/null --max-time 5 --resolve <fqdn>:<proxy_port>:127.0.0.1 http://<fqdn><health_path>` **(défaut proposé)**. Échec de vérification → re-pointer le fichier sur l'ancien container (compensation C2) → `failed`. L'ancien container n'a jamais cessé de tourner (INV-005).
5. **Arrêt gracieux de l'ancien** : `docker stop -t <stop_grace_period> <uuid>` (SIGTERM, puis SIGKILL après le délai) puis `docker rm <uuid>`. L'image de l'ancien n'est **pas** supprimée (rollback §8, INV-006).
6. **Renommer** : `docker rename <uuid>-next <uuid>`. Le trafic continue de passer par l'IP (étape 2) : aucune fenêtre d'indisponibilité pendant le rename.
7. **Stabiliser** (état `finishing`) : régénérer le fichier avec `url: http://<uuid>:<ports_exposes>` (résolution DNS Docker par nom, robuste aux redémarrages du container qui changeraient l'IP), appliquer (étapes 3–4). Poser les labels de routage de parité sur le container final.

Chaque étape est individuellement idempotente ou détectable, ce qui fonde les règles de reprise du §4 (`switching`).

### 7.3 Conditions d'éligibilité (§5.5, §15 PRD)

Rolling update **uniquement si** : health check configuré et fonctionnel, nom de container par défaut (pas de custom name), pas de Docker Compose (P0/P1 — levé par §27.15 dans la spec Compose), **pas de port mapping hôte** (« Ports Mappings » : deux containers ne peuvent pas binder le même port hôte).

### 7.4 Fallback stop-then-start

Pour les applications inéligibles : à l'entrée de `starting`, exécuter `docker stop -t <stop_grace_period> <uuid> && docker rm <uuid>`, créer le nouveau container directement nommé `<uuid>`, démarrer, attendre `running` (+ santé si un healthcheck existe), appliquer la config proxy (mêmes étapes 2–4 et 7, sans transition d'IP). Interruption de service assumée = durée stop + start ; l'UI l'affiche comme telle. En cas d'échec du nouveau container, l'ancien a déjà été supprimé : la compensation propose le **redéploiement du dernier artifact vérifié** (§8) — c'est précisément pour cela que ses images sont protégées (INV-006 : l'échec ne supprime jamais le dernier artifact sain).

---

## 8. Rollback (décision §27.6)

### 8.1 Principe

Un rollback est un **redéploiement d'un artifact vérifié, sans rebuild** : nouvelle entrée `Deployment` (trigger `rollback`, lien vers le déploiement d'origine et son snapshot de config — INV-014), machine à états avec `cloning/building` en no-op, entrée en `starting` seulement après vérification de l'artifact.

### 8.2 Résolution de l'artifact

| Contexte | Vérification | Source |
|---|---|---|
| Registry configuré | `docker pull <registry>/<image>@sha256:<digest>` (le digest garantit l'immutabilité, indépendamment des tags déplacés) | `DeploymentArtifact.image_digest` |
| Sans registry (fallback local) | `docker image inspect akerdock/<app_uuid>:<sha12>` — présence + correspondance du label `akerdock.deployment_uuid` | Rétention locale |

Rétention locale : les images des **N = 3 derniers déploiements réussis (défaut proposé, configurable par application)** portent `akerdock.retain=true` et sont enregistrées comme protégées ; le cleanup automatique (§3.7 PRD) les exclut (INV-015). En `finishing`, l'image sortant de la fenêtre perd sa protection.

Si l'artifact demandé est introuvable/invérifiable, le rollback est **refusé à la validation** avec la liste des artifacts effectivement disponibles — jamais d'échec à mi-parcours pour cette cause.

### 8.3 Rollback automatique (opt-in, §20.8)

Politique par application, désactivée par défaut. Après `succeeded` : fenêtre d'observation (**bake time, défaut 300 s (défaut proposé)**) sur le health check du container promu. Dégradation (`unhealthy` ou sortie du container) pendant la fenêtre → déclenchement automatique d'un rollback vers l'artifact précédent vérifié, notifié et audité. Le rollback automatique ne s'applique qu'une fois par déploiement (pas de boucle ping-pong) **(défaut proposé)**.

---

## 9. Compensation et échecs

### 9.1 Politiques de compensation

| ID | Nom | Actions |
|---|---|---|
| **C1** | Avant tout container candidat | Supprimer le clone du déploiement (`rm -rf source/<deployment_uuid>`) ; conserver l'image buildée (utile au diagnostic et au retry, purgée par le cleanup si non promue **(défaut proposé)**) ; l'ancien container et son routage sont intacts (INV-005/006) |
| **C2** | Candidat créé, bascule non effective | Capturer `docker logs --tail 200 <uuid>-next` dans le log de build ; `docker stop -t 10 <uuid>-next && docker rm <uuid>-next` ; si le fichier proxy a été modifié sans vérification concluante : le régénérer vers l'ancien container et vérifier (étapes 3–4 du §7.2) ; **ne jamais toucher** à l'ancien container, à ses volumes, ni aux images protégées (INV-006) |
| **C3** | Après bascule vérifiée | Aucune dé-bascule implicite : le nouveau container reste en production ; si la politique de rollback automatique (§8.3) est active et l'artifact précédent vérifié → rollback auto ; sinon `failed` avec remédiation manuelle proposée (bouton rollback) — §20.2 |

### 9.2 Tableau étape × échec → action

| État au moment de l'échec | Exemples d'échec | Compensation | Retry auto ? |
|---|---|---|---|
| `preparing` | SSH KO, disque plein, pre-command en échec | C1 (rien à nettoyer sauf fichiers env, laissés en place — réécrits au prochain run) | SSH/réseau : oui ; pre-command/disque : non |
| `cloning` | Auth Git, SHA introuvable, timeout | C1 | Timeout/réseau : oui ; auth/SHA : non |
| `building` | Erreur de compilation, `${VAR:?}` (défense en profondeur), OOM du build | C1 | Non (déterministe) |
| `pushing` | Registry injoignable, 401 | C1 ; si registry optionnel : dégradation en rétention locale + avertissement | 5xx/réseau : oui ; 401 : non |
| `starting` | Image corrompue, port hôte occupé, options Docker invalides | C2 | Non |
| `healthchecking` | Jamais `healthy`, container exited, post-command en échec | C2 | Non |
| `switching` avant application proxy vérifiée | Génération IR invalide, upload KO, vérification Traefik KO | C2 | Erreur d'application/vérif : une re-tentative locale immédiate puis C2 **(défaut proposé)** |
| `switching` après application vérifiée | `docker stop`/`rm`/`rename` KO | Poursuivre : re-tenter, sinon `failed` + C3 (le trafic est déjà correct ; l'ancien container arrêté-non-supprimé est signalé pour nettoyage réconciliable, §20.6) | Oui (idempotent) |
| `finishing` | Nettoyage/labels/protection KO | Aucune : `succeeded` dégradé + tâche de réconciliation asynchrone | Oui (job séparé) |
| N'importe quel état, annulation | `cancel_requested_at` | Compensation de l'état courant (C1 ou C2) ; refusée après la barrière (§2.6) | — |

### 9.3 Libération garantie

Verrou (§3.1) et slot (§3.2) sont libérés dans **tous** les chemins de sortie (succès, échec, annulation, dead-letter) par le point de sortie unique du job ; en cas de mort du worker, la libération est effectuée par le scan des leases expirés (§2.3) après inspection de reprise. La compensation elle-même est un ensemble d'opérations idempotentes : un crash pendant la compensation aboutit à sa reprise, pas à son abandon.

---

## 10. Pre/post-deployment commands (§5.3 PRD)

| | Pre-deployment | Post-deployment |
|---|---|---|
| **Où** | `docker exec <uuid> sh -c '<commande>'` dans le **container existant** (l'ancien) | `docker exec <uuid>-next sh -c '<commande>'` dans le **nouveau container** (candidat) |
| **Quand** | Fin de l'état `preparing`, avant tout clone/build | Fin de l'état `healthchecking`, après `healthy`, **avant** `switching` |
| **Si aucun container existant** (premier déploiement, ou app arrêtée) | Étape sautée, tracée dans le log (`skipped: no running container`) **(défaut proposé)** | N/A (le candidat existe toujours à ce stade) |
| **Timeout** | **600 s (défaut proposé, configurable par application)** | **600 s (défaut proposé, configurable par application)** |
| **Effet d'un échec** (exit code ≠ 0 ou timeout) | Déploiement `failed` avant toute mutation de build — l'existant n'est pas touché | Déploiement `failed`, compensation C2 (candidat supprimé), **pas de bascule, pas de rollback automatique** (§5.3 PRD : « échec post = déploiement échoué, sans rollback auto ») — l'ancien container reste routé (INV-005) |
| **Logs** | stdout/stderr intégrés au log de build, secrets non interpolés dans la ligne de commande loggée | idem |

Les commandes sont fournies par l'utilisateur : documentées comme devant être **idempotentes** (elles peuvent être rejouées lors d'une reprise après crash, §4). Elles s'exécutent avec l'environnement du container (les variables runtime y sont déjà) ; aucune variable n'est ajoutée en argv.

---

## 11. Timeouts, intervalles et retries — récapitulatif

Sauf mention contraire, chaque valeur est **(défaut proposé)** ; « Configurable » indique le niveau de surcharge prévu. Toutes les opérations distantes ont timeout + cancellation + classification + retry borné avec jitter (§22.1).

| Paramètre | Défaut | Configurable | Référence |
|---|---|---|---|
| SSH connect timeout | 10 s | Par serveur (PRD §3.1) | §4 `preparing` |
| SSH keepalive (ServerAlive) | 15 s, 3 échecs max | Instance | — |
| Inactivité d'une commande SSH (sans sortie) | 300 s | Instance | Détection de commande gelée |
| Espace disque minimal avant build | 2 GiB | Par serveur | §4 `preparing` |
| État `preparing` (total) | 120 s | Non | §4 |
| Git clone (+ submodules/LFS) | 600 s | Par application | §4 `cloning` |
| Build | 3600 s | Par application | §4 `building` |
| Pull d'image | 900 s | Par application | §5.4 |
| Push registry | 900 s | Par application | §4 `pushing` |
| Création + démarrage du container | 60 s | Non | §4 `starting` |
| Health check — interval / timeout / retries / start period | 5 s / 5 s / 10 / 5 s | Par application (PRD §5.3) | §4 `healthchecking` |
| Polling de l'état de santé | 2 s | Non | §4 |
| Budget total healthchecking | start_period + (interval+timeout)×retries + 30 s | Dérivé | §4 |
| Stabilité `running` sans healthcheck (mode non-rolling) | 10 s | Non | §4 |
| Pre/post-deployment command | 600 s | Par application | §10 |
| Vérification proxy après application | 30 s (polling 1 s) | Non | §7.2 |
| État `switching` (hors stop grace) | 120 s | Non | §4 |
| Stop grace period | 30 s | Par application (PRD §5.3) | §7.2 |
| État `finishing` | 60 s | Non | §4 |
| Lease de job | 90 s | Instance | §2.3 |
| Heartbeat | 20 s | Instance | §2.3 |
| Scan des leases expirés | 30 s | Instance | §2.3 |
| Polling de secours de la queue | 5 s | Instance | §2.3 |
| Backoff retry (base / facteur / max / jitter) | 30 s / ×2 / 15 min / full | Instance | §2.4 |
| `max_attempts` (deployment.run) | 3 | Instance | §2.4 |
| `concurrent_builds` | 2 (PRD §5.5) | Par serveur | §3.2 |
| `deployment_queue_limit` | 25 (PRD §5.5) | Par serveur | §3.2 |
| Rétention d'images de rollback (sans registry) | 3 images | Par application | §8.2 |
| Bake time (rollback auto opt-in) | 300 s | Par application | §8.3 |
| Rétention des clones sources | courant + précédent | Instance | §5.1 |

---

## 12. Observabilité

### 12.1 Événements (outbox transactionnelle, §18.2, §24.2)

Chaque transition d'état publie un événement dans la même transaction que la transition (envelope §24.2, versionnée, sans secret) :

| Événement | Émis à |
|---|---|
| `deployment.queued.v1` | Enqueue (§2.2) |
| `deployment.started.v1` | `queued → preparing` |
| `deployment.step_changed.v1` | Chaque transition intermédiaire (`payload.from`, `payload.to`, `payload.attempt`) |
| `deployment.superseded.v1` | Coalescing (§3.4) |
| `deployment.cancel_requested.v1` / `deployment.cancelled.v1` | Annulation (§2.6) |
| `deployment.succeeded.v1` | Terminal (payload : `commit_sha`, `image_digest`, durée par étape) |
| `deployment.failed.v1` | Terminal (payload : étape d'échec, classification, `attempt`, dead-letter éventuel) |
| `deployment.finishing_degraded.v1` | Succès avec nettoyage dégradé (§4) |
| `deployment.rollback_triggered.v1` | Rollback manuel ou automatique (§8) |

Consommateurs : realtime hub (progression UI), notifications (§11 PRD — routage/agrégation §27.19), audit, webhooks sortants futurs. Consommateurs idempotents, déduplication par `id`.

### 12.2 Logs de build (streaming)

- Le worker capture stdout/stderr de chaque commande distante en flux, ligne par ligne : horodatage UTC, état de la machine, numéro de séquence monotone.
- Persistance en base (table `deployment_logs`, append-only, curseur = numéro de séquence) et diffusion **SSE** avec reprise par `Last-Event-ID` (décision §27.24).
- Backpressure : buffer borné, reprise par curseur, signal explicite `lines_dropped` si des lignes ont été abandonnées (§22.2).
- Redaction avant persistance : les valeurs de secrets connues du snapshot sont masquées (`***`) dans chaque ligne ; séquences ANSI/HTML neutralisées à l'affichage (§23.3) ; INV-003.
- Rétention des logs alignée sur la rétention de l'historique de déploiement (§19.2).

### 12.3 Audit et corrélation

- Entrées d'audit (§23.4) pour : déclenchement (acteur/token, trigger, `Idempotency-Key`), annulation, retry manuel, rollback (manuel et automatique), rejeu depuis la dead-letter.
- `correlation_id` unique propagé : requête API → job → événements → logs → notifications ; le `webhook_delivery_id` relie un auto-deploy à sa livraison d'origine (§20.3).
- Métriques (OTLP, décision §27.8) : durée par état, taille de file par serveur, taux de succès (§16.4 : ≥ 99 % hors erreur applicative), leases expirés, retries, coalescings, dead-letters, durée de bascule proxy.
- Le diff de configuration inclus dans un redéploiement (§5.5 PRD) est attaché au `Deployment` (snapshot versionné, INV-014).

---

## 13. Traçabilité PRD

| Section de cette spec | Sections PRD |
|---|---|
| 1 | §5.5, §18.1–18.3, §20.2 |
| 2 | §17 (INV-004/013), §21.3, §22.1, §27.2, §27.25 |
| 3 | §5.5, §20.3.5, §21.1 |
| 4 | §20.2, §21.1, §22.1 |
| 5 | §5.1–5.4, §8, §17 (INV-003/012), §27.5, §27.22 |
| 6 | §2, §8, §17 (INV-011/015), §27.22 |
| 7 | §4.1, §5.5, §15, §17 (INV-005), §18.3, §27.9, §27.15 |
| 8 | §5.5, §15, §17 (INV-006), §20.8, §27.6 |
| 9 | §17 (INV-005/006), §20.2, §20.6, §20.8 |
| 10 | §5.3 |
| 11 | §3.1, §5.3, §5.5, §22.1 |
| 12 | §13, §18.2, §22.2, §23.3–23.4, §24.2, §27.8, §27.19, §27.24 |
