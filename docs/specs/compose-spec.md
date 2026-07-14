# Spécification — Sous-ensemble Docker Compose et transformations

> Artefact §29.5 du PRD (`docs/PRD.md`). Le PRD et les specs existantes (`deployment-engine.md`, `data-dictionary.md`) sont la source de vérité ; cette spécification précise le build pack **Docker Compose** (§5.2 PRD) et les services one-click (§9 PRD) au niveau clé compose, transformation, variable et code d'erreur. Lorsque le PRD est muet, la valeur retenue est marquée **(défaut proposé)**.
>
> Périmètre : build pack `docker compose` des applications et stacks des services (one-click ou compose libre, table `services`/`service_components` du data dictionary). Les contrats partagés avec le moteur de déploiement (queue, verrous, machine à états, compensation) sont ceux de `deployment-engine.md` §5.7 ; la génération proxy détaillée relève du contrat proxy (§29.6, à venir).

---

## 1. Sous-ensemble Compose supporté

### 1.1 Version de référence

- Schéma de référence : la **Compose Specification** actuelle (spécification versionless maintenue par compose-spec.io), telle qu'implémentée par **Docker Compose v2** sur Docker Engine ≥ 24 (§22.4 PRD).
- La clé top-level `version:` est **obsolète** dans la Compose Specification : elle est **ignorée avec warning** (`compose_version_ignored`), quel que soit son contenu.
- Tout fichier est d'abord parsé et validé contre le schéma de la Compose Specification ; les règles ci-dessous s'appliquent **après** cette validation syntaxique.
- Quatre traitements possibles par clé : **supporté** (passé tel quel), **transformé** (réécrit par AkerDock, §2), **ignoré avec warning** (clé retirée, warning dans `details[]`), **rejeté avec erreur** (déploiement bloqué en validation, code stable §11). Une clé inconnue non listée est **ignorée avec warning** (`compose_key_ignored`) **(défaut proposé)** ; les clés `x-*` sont ignorées silencieusement conformément à la Compose Specification, sauf `x-akerdock` (§5).

### 1.2 Clés top-level

| Clé | Traitement | Détail |
|---|---|---|
| `services` | supporté | Cœur du modèle ; chaque service devient un `service_component` (data dictionary §9.2) |
| `networks` | transformé | Réseau isolé du stack imposé (§2.1) ; réseaux additionnels internes créés préfixés ; `external: true` soumis à politique (§1.4) |
| `volumes` | transformé | Noms préfixés par l'UUID du stack (§2.4) ; `external: true` soumis à politique |
| `configs` | supporté | Matérialisés en file mounts gérés (§8 PRD) ; `external: true` rejeté (`compose_external_object_rejected`) |
| `secrets` | transformé | `file:` matérialisé en file mount `0600` **(défaut proposé)** ; `external: true` (Swarm) rejeté (`compose_swarm_key_rejected`) |
| `name` | ignoré avec warning | Le nom de projet est imposé : UUID du stack (INV-011) |
| `version` | ignoré avec warning | Obsolète (cf. §1.1) |
| `include` | rejeté avec erreur | `compose_include_rejected` — surface path traversal ; un stack = un fichier |
| `x-akerdock` | supporté | Extensions AkerDock (§5) |
| autres `x-*` | ignoré (sans warning) | Conforme à la Compose Specification |

### 1.3 Clés `services.<name>.*` — supportées et transformées

| Clé | Traitement | Détail |
|---|---|---|
| `image` | supporté | Digest OCI résolu au pull (§18.3 PRD) |
| `build` (`context`, `dockerfile`, `args`, `target`, `additional_contexts`) | transformé | Contexte résolu dans le clone (`base_directory`), chemins validés anti-traversal (§23.3 PRD) ; build via le flux `building` du moteur (deployment-engine §5.3.2), labels §2.3 injectés ; `build.secrets` → build secrets BuildKit |
| `command`, `entrypoint` | supporté | — |
| `environment`, `env_file` | transformé | Fusion avec les variables du stack (§3) ; `env_file` résolu dans le clone, anti-traversal |
| `ports` | supporté | Mapping hôte = « Ports Mappings » (§5.3 PRD) ; rend le service **inéligible au zero-downtime** (§8.4) |
| `expose` | supporté | Documentation du port interne ; utilisé comme port par défaut du routage (§6) |
| `volumes` | transformé | Préfixage, extensions storage, validation des bind mounts (§2.4, §5) |
| `labels` | transformé | Fusion après les labels système ; préfixe `AkerDock.` réservé → `compose_reserved_label` (deployment-engine §6.2) |
| `container_name` | ignoré avec warning | `compose_container_name_ignored` — le nommage est imposé (§2.2, INV-011) |
| `hostname` | supporté | Ne change pas les alias DNS injectés (§2.1) |
| `networks` | transformé | Mappés sur le réseau du stack + réseaux additionnels préfixés ; `aliases` conservés |
| `depends_on` | transformé | Réécrit en plan d'ordonnancement (§2.6) |
| `healthcheck` | transformé | Mappé sur les flags `docker create` (§7) |
| `restart` | transformé | Défaut `unless-stopped` si absent **(défaut proposé)** ; `no` respecté (jobs one-shot, §7.3) |
| `deploy.resources.limits` / `deploy.resources.reservations` | transformé | **Réellement appliqués** via cgroups (décision §27.15, §8.5) |
| `mem_limit`, `mem_reservation`, `memswap_limit`, `cpus`, `cpu_shares`, `cpuset` (legacy) | transformé | Normalisés vers les mêmes flags que `deploy.resources` ; conflit entre les deux formes → `compose_conflicting_limits` |
| `stop_grace_period`, `stop_signal` | supporté | `stop_grace_period` → `--stop-timeout` |
| `user`, `working_dir`, `init`, `tty`, `stdin_open` | supporté | — |
| `read_only`, `tmpfs`, `shm_size` | supporté | — |
| `ulimits`, `group_add` | supporté | — |
| `dns`, `dns_search`, `extra_hosts` | supporté | — |
| `platform`, `pull_policy`, `profiles` | supporté | Services hors profils actifs non déployés |
| `logging` | supporté | Driver et options bornés par politique d'instance **(défaut proposé)** |
| `extends` | supporté | Résolu au parse ; `file:` limité au clone, anti-traversal |
| `env_file` externe au dépôt, chemin absolu | rejeté avec erreur | `compose_path_traversal` |

### 1.4 Clés soumises à politique (refusées par défaut)

Ces clés élèvent les privilèges du container sur le serveur. Elles sont **refusées par défaut** (`compose_privileged_denied`) et activables par une **politique explicite par serveur** réservée aux admins **(défaut proposé)** — cohérent avec la validation centralisée des options Docker (§23.3 PRD, INV-012).

| Clé | Politique |
|---|---|
| `privileged: true` | Refusé par défaut |
| `cap_add` | Allowlist par défaut : `NET_BIND_SERVICE`, `CHOWN`, `SETUID`, `SETGID` **(défaut proposé)** ; au-delà, politique serveur |
| `cap_drop` | Toujours autorisé (réduction de privilèges) |
| `devices` | Refusé par défaut |
| `security_opt` | Refusé par défaut, sauf `no-new-privileges:true` toujours autorisé **(défaut proposé)** |
| `sysctls` | Allowlist réseau (`net.*` non privilégiés) **(défaut proposé)** ; reste sur politique |
| bind mount hors racines autorisées (dont `/var/run/docker.sock`, `/`, `/etc`, `/data/akerdock`) | Refusé par défaut (`compose_bind_mount_denied`) ; racines autorisées configurables par serveur **(défaut proposé)** |
| `networks.*.external: true`, `volumes.*.external: true` | Refusé par défaut (`compose_external_object_rejected`) ; autorisable par politique (objets non gérés, INV-015 : jamais touchés par le cleanup) |

### 1.5 Clés rejetées avec erreur

| Clé | Code | Motif |
|---|---|---|
| `deploy.replicas`, `deploy.mode`, `deploy.placement`, `deploy.update_config`, `deploy.rollback_config`, `deploy.endpoint_mode`, `deploy.labels` | `compose_swarm_key_rejected` | Sémantique Swarm — non réimplémenté (décision §27.4, ADR-004) |
| `network_mode: host` | `compose_network_mode_host_rejected` | Casse l'isolation réseau par stack et le routage proxy |
| `network_mode: service:*` / `container:*` | `compose_network_mode_rejected` | Incompatible avec le remplacement de containers (zero-downtime) |
| `pid: host`, `ipc: host`, `userns_mode: host`, `cgroup_parent`, `cgroup: host` | `compose_host_namespace_rejected` | Évasion d'isolation |
| `external_links` | `compose_swarm_key_rejected` | Légacy, contourne le modèle de destinations |
| `secrets` top-level `external: true` | `compose_swarm_key_rejected` | Secrets Swarm |
| `scale` | `compose_swarm_key_rejected` | Une instance par service (multi-instances = P3, §3.3 PRD) |
| `credential_spec`, `isolation` | `compose_platform_unsupported` | Windows uniquement |

`links` (légacy) est **ignoré avec warning** (`compose_key_ignored`) : le DNS du réseau isolé couvre le besoin.

---

## 2. Transformations appliquées

Toutes les transformations sont **déterministes** : le même compose + le même snapshot de configuration produisent exactement le même plan (INV-011, INV-014). Le compose transformé (forme canonique) est tracé dans les logs de déploiement.

### 2.1 Réseau

- Chaque stack reçoit un **réseau bridge isolé nommé par l'UUID du stack** (`resources.uuid`, §9 PRD) : `docker network create --label <labels §2.3> <stack_uuid>` — création idempotente.
- Tous les services du stack y sont attachés avec **deux alias DNS** : le nom du service compose (`<service>` — les références inter-services du fichier fonctionnent sans réécriture) et `<stack_uuid>-<service>` **(défaut proposé)**.
- **Connexion au réseau prédéfini** (`services.connect_to_predefined_network`, §9 PRD) : chaque service est en plus attaché au réseau de la `Destination`, avec pour **seul alias `<stack_uuid>-<service>`** (jamais l'alias court : deux stacks avec un service `db` entreraient en collision) **(défaut proposé)**.
- Les réseaux additionnels déclarés dans le fichier deviennent des réseaux internes au stack, nommés `<stack_uuid>_<network_name>`.

### 2.2 Nommage des containers

| Objet | Nom |
|---|---|
| Container d'un service | `<stack_uuid>-<service>` |
| Candidat zero-downtime (§8) | `<stack_uuid>-<service>-next` |
| Container de preview (§20.4 PRD) | `<preview_uuid>-<service>` — `previews.uuid` est la base des noms Docker de l'instance de preview (data dictionary §8.9) |
| Image buildée localement | `AkerDock/<stack_uuid>-<service>:<sha12>` **(défaut proposé)**, labels §2.3 |

`<service>` est le nom du service compose, validé `[a-z0-9][a-z0-9_.-]*` (`compose_invalid_service_name` sinon). `container_name` utilisateur est ignoré (§1.3) : les noms restent déterministes et dérivés d'UUID stables (INV-011).

### 2.3 Labels système injectés

Alignés **exactement** sur la table deployment-engine §6.2, avec un label supplémentaire `akerdock.component`. Posés sur containers, images, volumes et réseaux du stack :

| Label | Valeur | Rôle |
|---|---|---|
| `akerdock.managed` | `true` | Frontière géré / non géré (INV-015) |
| `akerdock.resource_uuid` | UUID du stack (la ressource `service`) | Rattachement au modèle |
| `akerdock.type` | `service` (`application` si build pack compose d'une application) | Typage |
| `akerdock.team_uuid` | UUID de la team | Isolation, audit |
| `akerdock.deployment_uuid` | UUID du déploiement | Idempotence des reprises (deployment-engine §2.5) — containers et images |
| `akerdock.commit_sha` | SHA complet | Traçabilité — images buildées depuis Git |
| `akerdock.retain` | `true` | Protection cleanup des images de rollback (deployment-engine §8.2) |
| `akerdock.component` | Nom du service compose (= `service_components.name`) | Rattachement au sous-container ; absent des objets partagés du stack (réseau) |

Les labels utilisateur (`services.<name>.labels`) sont ajoutés **après** et ne peuvent pas écraser le préfixe `AkerDock.` (réservé, `compose_reserved_label`).

### 2.4 Volumes

- Volume nommé `<vol>` → **`<stack_uuid>_<vol>`** (anti-collision, §8 PRD), créé avec les labels §2.3 : `docker volume create --label … <stack_uuid>_<vol>`. Les références dans tous les services sont réécrites de façon cohérente (un volume partagé entre services du stack reste partagé).
- Bind mounts : chemins relatifs résolus depuis le clone (build pack compose d'une application) ou depuis `/data/akerdock/services/<stack_uuid>/` **(défaut proposé)** ; chemins absolus soumis à la politique §1.4 ; anti path traversal (§23.3 PRD).
- File mounts et extensions storage : §5.
- Chaque volume/bind/file déclaré est synchronisé dans `persistent_storages` (data dictionary §8.7) pour l'UI et le workflow de suppression (§20.6 PRD).

### 2.5 Politique de restart

- `restart` absent → **`unless-stopped`** injecté **(défaut proposé)** (parité avec le container applicatif, deployment-engine §5.3.4).
- `restart: no` conservé : c'est le marqueur des jobs one-shot (migrations, seeds) — voir `exclude_from_hc` (§7.3).
- `restart: always` et `on-failure[:n]` conservés tels quels.

### 2.6 Réécriture des `depends_on`

`depends_on` n'est pas passé à Docker (les containers sont créés individuellement par le moteur) : il est compilé en **plan d'ordonnancement** :

- Forme courte → `condition: service_started` ; forme longue conservée (`service_started`, `service_healthy`, `service_completed_successfully`).
- Ordre de démarrage = **tri topologique** ; cycle → `compose_dependency_cycle`.
- `service_healthy` exige un healthcheck sur la dépendance (`compose_dependency_needs_healthcheck` sinon).
- `service_completed_successfully` : la dépendance doit être un job one-shot (`restart: no`) ; le démarrage des dépendants attend son exit code 0.
- Lors d'un redéploiement zero-downtime (§8), `depends_on` ordonne le **remplacement** des services mais ne redémarre jamais un dépendant dont l'image et la configuration n'ont pas changé **(défaut proposé)**.

### 2.7 Variables d'environnement injectées

Chaque container reçoit via `--env-file` (jamais en argv, INV-012 ; fichiers `env/` par stack, deployment-engine §5.1–5.2) : les variables du stack résolues (§3), les magic variables (§4) et les prédéfinies `AKERDOCK_*` (décision §27.22) pertinentes : `AKERDOCK_RESOURCE_UUID`, `AKERDOCK_CONTAINER_NAME` (= `<stack_uuid>-<service>`), `AKERDOCK_FQDN`/`AKERDOCK_URL` (composants avec domaine), `AKERDOCK_BRANCH`/`SOURCE_COMMIT` (compose depuis Git), `AKERDOCK_PR_ID` (previews).

---

## 3. Variables et interpolation

### 3.1 Syntaxe d'interpolation

Interpolation conforme à la Compose Specification, appliquée **côté control plane avant enqueue** (deployment-engine §5.2 : un manquement bloque en validation, pas à mi-build) :

| Syntaxe | Comportement |
|---|---|
| `${VAR}` / `$VAR` | Valeur résolue ; vide si indéfinie, avec warning `compose_variable_undefined` **(défaut proposé)** |
| `${VAR:-def}` / `${VAR-def}` | Défaut si vide-ou-indéfinie / si indéfinie |
| `${VAR:?msg}` / `${VAR:?}` | **Variable requise** : bloque le déploiement si vide ou indéfinie (`compose_required_variable_missing`, §5.4 PRD) |
| `${VAR:+alt}` | Valeur alternative si définie |
| `$$` | `$` littéral (pas d'interpolation) |

### 3.2 Ordre de résolution d'un nom de variable

Du plus spécifique au plus général — la première définition gagne :

1. **Variables du stack** (`environment_variables` de la ressource, jeu preview `is_preview` en contexte preview — jamais les secrets de production en preview, INV-010) ; les magic variables (§4) en font partie (`is_generated`).
2. **Variables partagées de l'environnement** (scope `environment`).
3. **Variables partagées du serveur** cible (scope `server`, §3.1 PRD) **(défaut proposé pour la position : entre environnement et projet)**.
4. **Variables partagées du projet** (scope `project`).
5. **Variables partagées de la team** (scope `team`).

En complément, la syntaxe explicite **`{{team.VAR}}` / `{{project.VAR}}` / `{{environment.VAR}}`** (§5.4 PRD) référence directement un scope précis à l'intérieur d'une **valeur** de variable du stack ; elle est résolue au même moment, court-circuite l'ordre ci-dessus, et une référence introuvable est une erreur (`compose_shared_variable_missing`) **(défaut proposé)**.

### 3.3 Types de variables

Portés par `environment_variables` (data dictionary §8.5) :

| Type | Effet dans le pipeline compose |
|---|---|
| `is_multiline` | Valeur écrite avec quoting sûr dans le `env-file` (clés, certificats) |
| `is_literal` | **Aucune interpolation** de la valeur : `${…}` et `{{…}}` y sont traités comme du texte |
| `is_locked` | Masquée et non rééditable en UI ; utilisée normalement à l'exécution |
| `is_build_time` | Disponible au build (`build.args` / BuildKit), stockée hors image |
| `is_secret` | Masquage UI/API sans `read:sensitive` ; redaction dans les logs (INV-003) |

---

## 4. Magic variables `SERVICE_<TYPE>_<ID>` — spécification complète

Syntaxe conservée telle quelle (décision §27.22, ADR-022 : fonctionnelle, non liée à la marque).

### 4.1 Résolution de l'identifiant `<ID>`

- `<ID>` est un token `[A-Z0-9_]+`. Convention : nom du service compose en majuscules, caractères non alphanumériques remplacés par `_` (ex. service `open-webui` → `OPEN_WEBUI`).
- **Le même `<ID>` désigne la même valeur dans tout le stack** : deux services référençant `SERVICE_PASSWORD_DB` reçoivent la même valeur (§5.4, §9 PRD).
- Pour `URL` et `FQDN`, `<ID>` doit correspondre à un composant du stack (nom normalisé) ; sinon `compose_magic_variable_unknown_component`.

### 4.2 Types et règles de génération exactes

Tous les alphabets et longueurs sont **(défaut proposé)**, alignés sur le comportement observé de la référence ; générateur : CSPRNG.

| Type | Valeur générée | Alphabet | Longueur |
|---|---|---|---|
| `SERVICE_FQDN_<ID>` | FQDN du composant `<ID>`, **sans schéma** (ex. `app.example.com`) ; généré depuis le wildcard serveur / sslip.io si aucun domaine (§4.2 PRD) | — | — |
| `SERVICE_FQDN_<ID>_<PORT>` | Comme `FQDN`, en associant le routage du domaine au port interne `<PORT>` (équivalent `domaine:port`, §4.2 PRD) | — | — |
| `SERVICE_URL_<ID>` (+ variante `_<PORT>`) | URL complète avec schéma (`https://` si TLS actif, sinon `http://`) | — | — |
| `SERVICE_USER_<ID>` | Nom d'utilisateur aléatoire | `[a-z]` premier caractère, puis `[a-z0-9]` | 16 |
| `SERVICE_PASSWORD_<ID>` | Mot de passe sans symboles | `[A-Za-z0-9]` | 32 |
| `SERVICE_PASSWORD_64_<ID>` | Idem | `[A-Za-z0-9]` | 64 |
| `SERVICE_PASSWORDWITHSYMBOLS_<ID>` | Mot de passe avec symboles | `[A-Za-z0-9]` + `!@#$%^&*()-_=+[]{}<>~` | 32 |
| `SERVICE_PASSWORDWITHSYMBOLS_64_<ID>` | Idem | idem | 64 |
| `SERVICE_BASE64_32_<ID>` / `_64_` / `_128_` | Chaîne aléatoire « base64-like » (pas un encodage) | `[A-Za-z0-9]` | 32 / 64 / 128 caractères |
| `SERVICE_REALBASE64_32_<ID>` / `_64_` / `_128_` | **Vrai encodage base64** de N octets aléatoires : `base64(random_bytes(N))`, padding conservé | Base64 standard | N = 32 / 64 / 128 octets |
| `SERVICE_HEX_32_<ID>` / `_64_` / `_128_` | Encodage hexadécimal de N/2 octets aléatoires → **N caractères hex** | `[0-9a-f]` | 32 / 64 / 128 caractères |

`SERVICE_<TYPE>` sans longueur explicite là où une longueur existe (`PASSWORD`, `PASSWORDWITHSYMBOLS`) = variante courte (32). Un type inconnu → `compose_magic_variable_invalid_type`.

### 4.3 Cycle de vie

- **Génération** : à la première sauvegarde/déploiement du compose qui référence la variable. Écrite dans `environment_variables` avec `is_generated = true`, `is_secret = true` pour les types credentials **(défaut proposé)**.
- **Persistance** : la valeur est stable entre redéploiements (§5.4 PRD) — jamais régénérée tant que la ligne existe.
- **Partage** : portée = le stack (la ressource) ; tous les services y accèdent par le même nom.
- **Édition UI** : éditable comme toute variable (§5.4 PRD) ; l'édition ne casse pas le lien `is_generated` (la variable n'est pas régénérée ensuite) **(défaut proposé)**.
- **Suppression du compose** : une magic variable qui n'est plus référencée est conservée mais signalée en UI comme orpheline ; suppression manuelle explicite **(défaut proposé)** — jamais de perte silencieuse d'un credential encore utilisé par des données existantes.
- **Preview (§20.4 PRD)** : chaque preview génère **sa propre instance** de chaque magic variable (nouvelle valeur par identité de preview, stockée dans le jeu `is_preview` rattaché à la preview) ; `SERVICE_FQDN_*`/`SERVICE_URL_*` résolvent vers l'URL de la preview (`SERVICE_URL_<ID>` = URL de preview, §5.6 PRD). Destruction de la preview = destruction de ses instances de variables.

---

## 5. Extensions AkerDock (`x-akerdock`)

Namespace propre `x-akerdock` **(défaut proposé)**, conforme au mécanisme d'extensions de la Compose Specification (les clés `x-*` sont légales à tout niveau). Les extensions écrites **hors** de ce namespace (clés non préfixées au niveau d'un service, comme en produisent certaines plateformes tierces) ne sont **pas** interprétées : elles suivent la règle générale du §1.3 — clé inconnue, retirée avec warning `compose_key_ignored`. Une clé qui n'a pas d'effet doit le dire ; deux orthographes pour une même extension seraient une divergence qui attend son heure. La réécriture vers `x-akerdock` est faite **à l'import** dans le dépôt de templates (ADR-010, ADR-022), une fois, pas à chaque déploiement.

### 5.1 Extensions supportées

| Besoin (§5.2, §8 PRD) | Clé `x-akerdock` (défaut proposé) | Niveau | Effet |
|---|---|---|---|
| `is_directory: true` (entrée de volume) | `x-akerdock.is_directory: true` | Entrée de `volumes` (forme longue) | Création du répertoire hôte avant montage (`persistent_storages.is_directory`) |
| `content: \|` (entrée de volume) | `x-akerdock.content: \|` | Entrée de `volumes` (forme longue, `type: bind` fichier) | Création du fichier hôte avec **interpolation des variables** (§3) ; contenu éditable en UI (≤ 5 MiB, §23.3 PRD) ; `mode`/`uid`/`gid` optionnels via `x-akerdock.file_mode`, `x-akerdock.owner_uid`, `x-akerdock.group_gid` |
| `exclude_from_hc: true` (service) | `x-akerdock.exclude_from_hc: true` | `services.<name>` | Exclusion du health check agrégé du stack (§7.3, `service_components.exclude_from_hc`) |
| Stack ne tolérant pas deux instances simultanées | `x-akerdock.zero_downtime: false` | `services.<name>` | Désactive le remplacement à deux instances pour ce service (ADR-015) |
| Métadonnées de template en commentaires | `x-akerdock.template` (top-level) | Top-level | §12 |

Exemple :

```yaml
services:
  app:
    image: ghcr.io/example/app:1.4
    volumes:
      - type: bind
        source: ./config/app.ini
        target: /etc/app/app.ini
        x-akerdock:
          content: |
            [server]
            url = ${SERVICE_URL_APP}
      - type: bind
        source: ./data
        target: /var/lib/app
        x-akerdock:
          is_directory: true
  migrate:
    image: ghcr.io/example/app:1.4
    command: ["app", "migrate"]
    restart: "no"
    x-akerdock:
      exclude_from_hc: true
```

`x-akerdock.content` et `x-akerdock.is_directory` sont mutuellement exclusifs sur une même entrée (`compose_storage_extension_conflict`).

---

## 6. Domaines et routage

- **Un domaine par service** : chaque `service_component` peut porter zéro, un ou plusieurs domaines (`domains.service_component_id`, data dictionary §8.4) — FQDN + path + `target_port` optionnel (`domaine:port`, §4.2 PRD).
- **Port par service** : le port cible du routage est, dans l'ordre : `target_port` du domaine → premier `expose` du service → port du template (`x-akerdock.template.port`, §12) → erreur `compose_routable_port_unresolved` si le service a un domaine mais aucun port déterminable **(défaut proposé)**.
- **Services sans domaine = privés** : aucune configuration proxy générée, aucun port publié ; joignables uniquement par DNS interne du réseau du stack (alias §2.1) — parité §9 PRD.
- Génération : chaque composant avec domaine produit une entrée dans la **représentation intermédiaire proxy** (décision §27.9) ; matérialisation Traefik/Caddy, priorités path-based, redirection www, certificats : voir le **contrat proxy (§29.6, à venir)**. Fichier dynamique par ressource : `/data/akerdock/proxy/dynamic/<stack_uuid>.yaml`, sections par composant **(défaut proposé)**.
- `SERVICE_FQDN_<ID>` référencé dans le compose vaut déclaration d'intention de domaine : si le composant n'a pas de domaine configuré, un domaine est généré depuis le wildcard serveur (fallback sslip.io, §4.2 PRD) au premier déploiement.

---

## 7. Health checks et `exclude_from_hc`

### 7.1 Mapping compose ↔ AkerDock

Le fichier compose est la source de vérité (§5.2 PRD) : `services.<name>.healthcheck` prime.

| Clé compose | Flag `docker create` | Défaut si healthcheck présent sans la clé |
|---|---|---|
| `test` | `--health-cmd` | — (requis) |
| `interval` | `--health-interval` | `30s` (spec Compose) |
| `timeout` | `--health-timeout` | `30s` |
| `retries` | `--health-retries` | `3` |
| `start_period` | `--health-start-period` | `0s` |
| `start_interval` | `--health-start-interval` | `5s` |
| `disable: true` | `--no-healthcheck` | — |

Ordre de priorité par service : `healthcheck` compose > `HEALTHCHECK` de l'image > health check UI de la ressource (appliqué au composant routé, généré en `--health-cmd` HTTP comme deployment-engine §5.3.4) **(défaut proposé)**. Un service web sans aucun des trois est inéligible au zero-downtime (§8.4).

### 7.2 Statut agrégé du stack

Statut observé par composant (`service_components.observed_status`) ; le statut du stack est l'agrégat des composants **non exclus** : `healthy` si tous sains, `unhealthy` si au moins un dégradé, etc. **(défaut proposé)**.

### 7.3 Jobs one-shot et `exclude_from_hc`

- `x-akerdock.exclude_from_hc: true` exclut le composant : du statut agrégé, de la barrière `healthchecking` du déploiement, et des notifications de changement de statut (§9 PRD).
- Un service `restart: "no"` **sans** `exclude_from_hc` déclenche le warning `compose_oneshot_without_exclude` suggérant l'extension **(défaut proposé)**.
- Pendant un déploiement, un job one-shot exclu est lancé selon l'ordre `depends_on` ; `service_completed_successfully` reste vérifiable (§2.6) ; son exit ≠ 0 fait échouer le déploiement (`failed`, classification déterministe).

---

## 8. Zero-downtime compose (décision §27.15, ADR-015)

### 8.1 Classification des services

- **Service web** : au moins un domaine (§6). Remplacement **à deux instances avec bascule proxy par service**.
- **Service non-web** (workers, bases, caches) : remplacement **recreate** (stop-then-start, deployment-engine §7.4), sans coupure du routage des services web.

### 8.2 Algorithme par déploiement de stack

Même queue, verrous, slots et machine à états que le moteur (deployment-engine §2–4) — verrou §3.1 au niveau du stack ; les états `starting`/`healthchecking`/`switching` opèrent **par service** :

1. **Plan** : parse, transformations (§2), diff par service (image, config, volumes) ; un service inchangé n'est pas remplacé **(défaut proposé)**.
2. **Build/pull** de toutes les images du stack (états `cloning`/`building`/`pushing` mutualisés), digests résolus avant toute mutation.
3. Parcours en **ordre topologique** (§2.6). Pour chaque **service non-web** modifié : `docker stop -t <grace>` + `rm` de `<stack_uuid>-<service>`, création du nouveau sous le même nom, attente `running`/`healthy` selon healthcheck.
4. Pour chaque **service web** éligible : création du candidat **`<stack_uuid>-<service>-next`** sur les mêmes réseaux ; attente `healthy` ; **bascule proxy du seul routage de ce composant** (algorithme deployment-engine §7.2 : IR → fichier dynamique → application atomique → vérification → arrêt gracieux de l'ancien → `docker rename` → stabilisation par nom DNS) ; barrière d'annulation active par bascule.
5. **Jobs one-shot** : exécutés à leur position topologique (§7.3).
6. `finishing` : labels de parité, protection des images de rollback, synchronisation `service_components`, nettoyage asynchrone.

**Échec en cours de plan** : le service en échec suit la compensation C2 (candidat supprimé, ancien intact et routé — INV-005/006) ; les services **déjà basculés restent en place** (pas de dé-bascule implicite, C3) ; le déploiement est `failed` avec le détail par composant — état partiel explicite, reprise possible (§20.8 PRD).

### 8.3 Coexistence temporaire

Pendant la bascule d'un service web, deux instances coexistent sur le réseau du stack ; l'alias DNS court `<service>` pointe l'ancien container jusqu'au rename **(défaut proposé)** — les autres services ne voient jamais le candidat avant promotion. Les stacks ne tolérant aucune coexistence désactivent le mécanisme par service : `x-akerdock.zero_downtime: false` (§5.1, risque accepté ADR-015).

### 8.4 Conditions d'inéligibilité (par service)

Un service web est traité en recreate (avec interruption assumée, affichée comme telle) si :

- aucun healthcheck résolu (§7.1) ;
- port mapping hôte (`ports`) — deux instances ne peuvent pas binder le même port ;
- `x-akerdock.zero_downtime: false` ;
- mode raw compose (§9) ;
- volume nommé monté en écriture partagé avec sa propre instance ne tolérant pas deux writers — non détectable automatiquement : documenté, à couvrir par `zero_downtime: false` (avertissement produit §8 PRD).

### 8.5 Resource limits réellement appliquées (décision §27.15)

`deploy.resources` et les clés legacy (§1.3) sont traduites en flags du `docker create` de **chaque** container du stack — jamais ignorées :

| Clé compose | Flag |
|---|---|
| `deploy.resources.limits.memory` / `mem_limit` | `--memory` |
| `deploy.resources.reservations.memory` / `mem_reservation` | `--memory-reservation` |
| `memswap_limit` | `--memory-swap` |
| `deploy.resources.limits.cpus` / `cpus` | `--cpus` |
| `cpu_shares` | `--cpu-shares` |
| `cpuset` | `--cpuset-cpus` |
| `deploy.resources.limits.pids` | `--pids-limit` |

Preuve exigée : vérification cgroup (`docker inspect` + `/sys/fs/cgroup`) dans les E2E (§26.2 PRD).

---

## 9. Mode raw compose

Mode avancé opt-in par ressource (§5.2 PRD) : le fichier est appliqué au plus près de la sémantique `docker compose up`.

**Reste actif (non désactivable)** :

- labels de gestion §2.3 sur tous les objets créés (INV-015 : sans eux, cleanup et adoption sont aveugles) ;
- réseau isolé du stack + nom de projet imposé par UUID (INV-011) ;
- interpolation des variables et magic variables (§3–4) ;
- clés **rejetées** §1.5 et politique §1.4 (frontières de sécurité, non négociables) ;
- resource limits appliquées (§8.5).

**Désactivé** :

- renommage des containers (§2.2) : noms Compose standards `<stack_uuid>-<service>-1` via le project name ;
- préfixage/réécriture des volumes et réseaux additionnels (§2.4) hors labels ;
- injection de la politique de restart (§2.5) et réécriture `depends_on` (§2.6 — sémantique compose native) ;
- extensions storage et gestion des domaines par composant (l'utilisateur gère ses propres labels proxy) ;
- **zero-downtime** (§8) : tout redéploiement est un `down`/`up` du stack, interruption assumée et affichée ;
- synchronisation fine `service_components` (statuts limités à running/exited) **(défaut proposé)**.

---

## 10. Backups des bases internes (§7.1 PRD)

- À chaque synchronisation du compose, chaque service est classé par **détection d'image** : le nom d'image (basename, registre et namespace ignorés) est comparé à `postgres`/`postgresql`, `mysql`, `mariadb`, `mongo`/`mongodb`, y compris les variantes courantes `bitnami/postgresql`, `pgvector/pgvector`, `supabase/postgres`, `percona`, `mongodb/mongodb-community-server` **(défaut proposé, liste maintenue avec le catalogue)**.
- Résultat porté par `service_components.is_database` + `database_engine` (data dictionary §9.2) : le composant devient cible valide d'un `database_backup_plan` (`service_component_id`, §9.5) avec les mêmes moteurs/outils que les bases managées (`pg_dump`, `mysqldump`, `mariadb-dump`, `mongodump --gzip`).
- Les credentials sont lus depuis les variables résolues du composant (dont magic variables `SERVICE_USER_*`/`SERVICE_PASSWORD_*`) **(défaut proposé)** ; jamais loggés (INV-003).
- Une image non reconnue reste backupable via les mécanismes hors périmètre (volumes, §27.14).

---

## 11. Validation — erreurs et warnings à codes stables

Codes stables consommables par l'API dans `details[]` (§24.1 PRD : `code`, `message`, `details`). Sévérité `error` = déploiement/sauvegarde bloqué ; `warning` = accepté, tracé et affiché. Liste normative (extensible par version d'API) :

| Code | Sévérité | Cas |
|---|---|---|
| `compose_parse_error` | error | YAML invalide ou non conforme au schéma Compose Specification (position incluse) |
| `compose_version_ignored` | warning | Clé `version:` présente (§1.1) |
| `compose_key_ignored` | warning | Clé non supportée retirée (§1.2–1.3, `links`…) |
| `compose_container_name_ignored` | warning | `container_name` retiré (§2.2) |
| `compose_swarm_key_rejected` | error | Clé Swarm (§1.5) |
| `compose_network_mode_host_rejected` | error | `network_mode: host` |
| `compose_network_mode_rejected` | error | `network_mode: service:*` / `container:*` |
| `compose_host_namespace_rejected` | error | `pid`/`ipc`/`userns_mode`/`cgroup` host, `cgroup_parent` |
| `compose_privileged_denied` | error | `privileged`, `cap_add`, `devices`, `security_opt`, `sysctls` hors politique serveur (§1.4) |
| `compose_bind_mount_denied` | error | Bind mount hors racines autorisées (dont `docker.sock`) |
| `compose_external_object_rejected` | error | `external: true` (réseau/volume/config) hors politique |
| `compose_include_rejected` | error | Clé `include` |
| `compose_platform_unsupported` | error | `credential_spec`, `isolation` |
| `compose_invalid_service_name` | error | Nom de service hors `[a-z0-9][a-z0-9_.-]*` |
| `compose_reserved_label` | error | Label utilisateur préfixé `AkerDock.` |
| `compose_path_traversal` | error | `env_file`, `build.context`, `extends.file`, bind relatif sortant du clone (§23.3 PRD) |
| `compose_conflicting_limits` | error | `deploy.resources` et clés legacy contradictoires (§1.3) |
| `compose_dependency_cycle` | error | Cycle dans `depends_on` |
| `compose_dependency_needs_healthcheck` | error | `service_healthy` vers un service sans healthcheck |
| `compose_required_variable_missing` | error | `${VAR:?}` vide ou indéfinie (§3.1) |
| `compose_variable_undefined` | warning | `${VAR}` indéfinie sans défaut |
| `compose_shared_variable_missing` | error | `{{team.VAR}}`/`{{project.VAR}}`/`{{environment.VAR}}` introuvable |
| `compose_magic_variable_invalid_type` | error | `SERVICE_<TYPE>_…` type inconnu (§4.2) |
| `compose_magic_variable_unknown_component` | error | `SERVICE_FQDN/URL_<ID>` sans composant correspondant |
| `compose_storage_extension_conflict` | error | `content` + `is_directory` sur la même entrée (§5.1) |
| `compose_routable_port_unresolved` | error | Domaine sans port cible déterminable (§6) |
| `compose_domain_conflict` | error | Violation `UNIQUE (fqdn, path)` globale (data dictionary §8.4) |
| `compose_oneshot_without_exclude` | warning | `restart: no` sans `exclude_from_hc` (§7.3) |
| `compose_zero_downtime_ineligible` | warning | Service web traité en recreate, motif inclus (§8.4) |
| `compose_file_content_too_large` | error | `x-akerdock.content` > 5 MiB (§23.3 PRD) |

Chaque entrée `details[]` porte : `code`, `severity`, `service` (nom du service concerné, si applicable), `path` (chemin YAML, ex. `services.app.deploy.replicas`), `message` (générique, jamais de secret — INV-003).

---

## 12. Templates one-click (§9, §27.10, ADR-010)

### 12.1 Anatomie d'un template AkerDock

Un template = un fichier compose valide au sens de cette spec + un bloc de métadonnées top-level **(défaut proposé)** :

```yaml
x-akerdock:
  template:
    slug: umami            # requis, unique dans le dépôt, [a-z0-9-]+
    name: Umami            # requis, nom affiché
    documentation: https://umami.is/docs   # requis (§9 PRD)
    slogan: Simple, privacy-focused analytics   # requis
    category: analytics    # requis, vocabulaire contrôlé du catalogue
    tags: [analytics, privacy]   # optionnel
    logo: svgs/umami.svg   # requis, chemin relatif au dépôt (SVG/PNG, ≤ 128 KiB (défaut proposé))
    port: 3000             # requis si un service doit être exposé : port par défaut du routage (§6)
    min_akerdock_version: "1.2"   # optionnel
services:
  umami: …
```

Les métadonnées portées en **commentaires** (`# documentation:`, `# slogan:`, `# category:`, `# tags:`, `# logo:`, `# port:`), format répandu dans l'écosystème des catalogues compose, sont reconnues à l'import et réécrites vers `x-akerdock.template` — même pipeline que la réécriture des variables prédéfinies d'une autre plateforme (ADR-022) **(défaut proposé)**.

### 12.2 Compilation du catalogue en JSON signé (§27.10)

- Le dépôt officiel est compilé en un **artefact JSON de catalogue** : `{ schema_version, generated_at, source_commit, templates: [{ slug, version (checksum SHA-256 du compose canonique), metadata, compose (contenu canonique), logo_data_uri }] }` **(défaut proposé)**.
- L'artefact est **signé** (signature détachée **Ed25519 (défaut proposé)**) ; la clé publique du projet est embarquée dans le binaire ; l'instance **vérifie la signature avant tout rafraîchissement** du catalogue — un catalogue non vérifiable est refusé et l'ancien reste en service.
- Rafraîchissable indépendamment des releases du binaire (ADR-010) ; `template_slug`/`template_version`/`template_repository` sont figés sur la ressource à l'instanciation (data dictionary §9.1).
- CI du dépôt officiel : chaque template passe la validation §12.3 + un déploiement de fumée **(défaut proposé)**.

### 12.3 Validation à l'import d'un dépôt de templates utilisateur

Dépôts Git de team (publics/privés via les clés/credentials existants, INV-002), **validation à l'import et à chaque resynchronisation** ; les templates utilisateur ne sont **pas signés** par le projet (responsabilité de la team, risque accepté ADR-010). Règles, chacune rapportée par template avec les codes §11 plus :

| Règle | Code | Sévérité |
|---|---|---|
| Compose conforme à la présente spec (§1, §11) — un template rejeté n'entre pas au catalogue de la team, les autres oui | (codes §11) | error |
| Métadonnées requises présentes et valides (`slug`, `name`, `documentation`, `slogan`, `category`, `logo` ; `port` si service exposé) | `template_metadata_missing` | error |
| `slug` unique dans le dépôt | `template_slug_conflict` | error |
| Logo présent, format SVG/PNG, taille ≤ 128 KiB | `template_logo_invalid` | error |
| Variables prédéfinies d'une autre plateforme détectées (préfixe étranger) | `template_foreign_variables` | error à l'import officiel (réécriture obligatoire vers `AKERDOCK_*`, ADR-022) ; warning en dépôt utilisateur **(défaut proposé)** |
| Magic variables cohérentes (§4 : types valides, `FQDN`/`URL` pointant un service du template) | `compose_magic_variable_*` | error |
| Image sans tag ou `:latest` | `template_unpinned_image` | warning **(défaut proposé)** |
| Clés soumises à politique (§1.4) utilisées | `template_requires_policy` | warning (signalé avant instanciation : le déploiement échouera si la politique serveur ne l'autorise pas) |

L'instanciation d'un template crée une ressource `service` ordinaire : le compose devient `services.compose_content`, éditable en UI, entièrement soumis aux sections 1 à 11.

---

## 13. Traçabilité PRD

| Section de cette spec | Sections PRD / specs |
|---|---|
| 1 | §5.2, §22.4, §23.3, §27.4 (ADR-004), INV-012, INV-015 |
| 2 | §2, §5.3, §8, §9, INV-011, INV-014 ; deployment-engine §5.1–5.3, §6 |
| 3 | §5.4, §3.1, §5.6, INV-003, INV-010 ; data-dictionary §8.5–8.6 ; deployment-engine §5.2 |
| 4 | §5.4, §9, §20.4, §27.22 (ADR-022) ; data-dictionary §8.5, §8.9 |
| 5 | §5.2, §8, §27.10/§27.22 (ADR-010/022) ; data-dictionary §8.7, §9.2 |
| 6 | §4.2, §9, §27.9 (ADR-009, contrat proxy §29.6) ; data-dictionary §8.4 |
| 7 | §5.3, §9 ; data-dictionary §8.8, §9.2 ; deployment-engine §5.3.4 |
| 8 | §15, §27.15 (ADR-015), §20.8, INV-005/006 ; deployment-engine §2–4, §7 |
| 9 | §5.2, INV-011, INV-015 |
| 10 | §7.1 ; data-dictionary §9.2, §9.5 |
| 11 | §24.1, §23.3, INV-003 |
| 12 | §9, §27.10 (ADR-010), §29.11 ; data-dictionary §9.1 |
