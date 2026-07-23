# Spécification — Contrat du module proxy

> Artefact §29.6 du PRD (`docs/PRD.md`). Le PRD est la source de vérité ; cette spécification précise le contrat du module proxy : représentation intermédiaire (IR), génération Traefik (P0) et Caddy (P2), priorités de routage, certificats, application atomique et fixtures de conformité. Lorsque le PRD est muet, la valeur retenue est marquée **(défaut proposé)**.
>
> Cohérence obligatoire : la mécanique de bascule zero-downtime (fichier dynamique par application, cible transitoire par IP du candidat, application atomique tmp+mv, vérification par l'API Traefik puis requête de fumée, rollback du fichier) est définie au §7 de `docs/specs/deployment-engine.md` — cette spécification en est le contrat côté proxy, elle ne la redéfinit pas. Décisions structurantes : §27.9/ADR-009 (IR commune, Traefik P0, Caddy P2, fixtures partagées), §27.1/ADR-001 (ports proxy configurables, control plane mono-port). Tables de référence : `domains` et `proxy_config_revisions` (`docs/specs/data-dictionary.md` §8.4, §11.1).

---

## 1. Vue d'ensemble

### 1.1 Rôle du provider proxy

Le module proxy est une **capacité remplaçable** (§16.1(5), §18.1) exposée aux services métier via un contrat unique. Un **provider** (Traefik en P0, Caddy en P2) implémente ce contrat ; la logique de routage — domaines, paths, priorités, middlewares, certificats — vit dans la **représentation intermédiaire** (§2), jamais dans le code spécifique à un proxy (ADR-009).

Opérations du contrat (sémantique normative, signatures Go non prescrites) :

| Opération | Rôle | Référence |
|---|---|---|
| `DeployProxy(server)` | Créer et démarrer le container proxy sur un serveur — uniquement si l'intention est `running` ; un serveur naît avec l'intention `stopped` et le premier démarrage passe par `StartProxy` (onboarding §20.1 étape 5) | §1.3 |
| `StartProxy` / `StopProxy` / `RestartProxy` | Cycle de vie depuis l'UI ; `StartProxy` converge config **et** container depuis zéro (c'est le premier démarrage nominal) ; l'arrêt coupe tout le trafic entrant du serveur (avertissement explicite, §4.1 PRD) | §1.3 |
| `UpgradeProxy(server, image)` | Recréation du container avec la nouvelle image épinglée ; notification « proxy obsolète » (§4.1, §11 PRD) | §1.4 |
| `GenerateStatic(ir) → fichiers` | Configuration statique du proxy (entrypoints, resolvers ACME) depuis l'IR serveur | §5.2 |
| `GenerateApp(ir, app, endpoint) → fichier` | Fichier de configuration dynamique d'une application ; `endpoint` = IP du candidat (forme transitoire) ou nom du container (forme stable) — deployment-engine §7.2 | §5.3 |
| `Validate(fichiers)` | Validation syntaxique et sémantique avant tout upload (schéma du format cible + règles §3) | §6.1 |
| `Apply(server, fichiers) → revision` | Application atomique (tmp + mv), enregistrement `proxy_config_revisions` avec checksum SHA-256 | §6 |
| `Verify(server, attentes)` | Vérification de prise en compte : API du proxy + requête de fumée | §6.3 |
| `Rollback(server, revision)` | Ré-application de la dernière révision `applied` | §6.4 |
| `RemoveApp(server, app_uuid)` | Retrait du routage d'une ressource (précède toute suppression de workload, §20.6) : suppression du fichier `<app_uuid>.yaml` + vérification | §6.5 |
| `Status(server)` | État observé du container proxy (`proxy_observed_status`), version d'image, dérive de checksum (§18.3) | §1.3 |

Le worker de déploiement **pilote** la bascule ; le proxy l'applique (deployment-engine §1.2). Le control plane n'est jamais dans le chemin des requêtes applicatives (INV-007).

### 1.2 Un proxy par serveur

- Chaque serveur a **son** proxy (§3.3 PRD), de type `proxy_type` (`traefik` | `caddy` | `none`, table `servers`). `none` = serveur sans trafic entrant routé (build server, par exemple).
- Le switch de type par serveur (§4.1 PRD) = régénération complète depuis l'IR pour le nouveau provider, déploiement du nouveau container, retrait de l'ancien. L'IR est inchangée : c'est précisément sa raison d'être.
- Le proxy est connecté (`docker network connect`, idempotent) à **chaque réseau de destination** hébergeant une ressource routée ; la connexion est vérifiée en `preparing`/`switching` avant toute bascule **(défaut proposé)**.

### 1.3 Le proxy est lui-même un container géré

Déploiement de référence (Traefik, P0) :

```sh
docker run -d \
  --name akerdock-proxy \
  --restart unless-stopped \
  --network AkerDock \
  -p <proxy_http_port>:<proxy_http_port> \
  -p <proxy_https_port>:<proxy_https_port> \
  [-p <tcp_port>:<tcp_port>]... \                    # routes TCP actives (§2.6, §5.6)
  -v /var/lib/akerdock/proxy/traefik.yaml:/etc/traefik/traefik.yaml:ro \
  -v /var/lib/akerdock/proxy/dynamic:/dynamic:ro \
  -v /var/lib/akerdock/proxy/acme.json:/acme/acme.json \
  -v /var/lib/akerdock/proxy/certs:/certs:ro \
  -v /var/lib/akerdock/proxy/auth:/auth:ro \
  --env-file /var/lib/akerdock/proxy/acme.env \        # credentials DNS-01, 0600, si DNS-01 configuré (§7.2)
  --label akerdock.managed=true \
  --label akerdock.type=proxy \
  --label akerdock.team_uuid=<team_uuid_du_serveur> \
  --health-cmd 'traefik healthcheck --ping' \
  --health-interval 5s --health-retries 3 \
  traefik:v3.5                                        # version épinglée par release AkerDock (défaut proposé)
```

Points normatifs :

- **Ports d'écoute configurables par serveur** (`proxy_http_port`/`proxy_https_port`, défauts 80/443, décision §27.1) : les entrypoints écoutent directement sur ces ports dans le container et sont publiés à l'identique (`8443:8443`, jamais `8443:443`) — les redirections HTTP→HTTPS émettent ainsi le bon port sans réécriture **(défaut proposé)**.
- L'API locale du proxy (Traefik `:8080`) n'est **jamais publiée sur l'hôte** : elle n'est accessible que par `docker exec` dans le container (vérification §6.3, conforme deployment-engine §7.2).
- Le socket Docker n'est **pas** monté dans le proxy : toute la configuration passe par les fichiers (pas de provider docker Traefik ; les labels de parité sont informatifs, §5.1).
- Arborescence sous `/var/lib/akerdock/proxy/` (extension de deployment-engine §5.1, **(défaut proposé)** — sauf `acme.json` et `acme.env`, dont les emplacements sont **normatifs**, §7.2/§7.5) :

```text
/var/lib/akerdock/proxy/
├── traefik.yaml              # config statique générée (§5.2) — recréation du container à chaque changement
├── dynamic/                  # provider file watch: true
│   ├── 00-certificates.yaml  # certificats custom (§7.3) — nom réservé
│   ├── 00-control-plane.yaml # routage du FQDN de l'instance si ce serveur l'héberge (§14.2 PRD) — nom réservé
│   └── <app_uuid>.yaml       # UN fichier par application/preview/composant routé (§5.3)
│       .<app_uuid>.yaml.tmp      # fichier temporaire d'application atomique (éphémère)
│       .<app_uuid>.yaml.awake    # variante « éveillée » pré-générée (scale-to-zero, §8)
├── certs/                    # certificats custom déposés (§4.3 PRD)
├── acme.json                 # stockage ACME (0600) — emplacement NORMATIF (§7.5)
├── acme.env                  # credentials DNS-01 matérialisés (0600) — emplacement NORMATIF (§7.2)
└── auth/<app_uuid>.htpasswd  # fichiers d'utilisateurs basic auth (0600, §4.2)
```

Cycle de vie : `proxy_desired_state` (`running`/`stopped`) et `proxy_observed_status` (table `servers`) ; start/stop/restart depuis l'UI avec statut et logs consultables (§4.1 PRD). Stop = `docker stop` du container (les fichiers restent en place ; start = redémarrage sans régénération). Toute opération est auditée (§23.4).

### 1.4 Upgrade d'image

1. Notification « image du proxy obsolète » quand la version épinglée de la release AkerDock est plus récente que celle du container (§4.1, §11 PRD).
2. Upgrade = action explicite : `docker stop` (grace 10 s) → `docker rm` → `docker run` avec la nouvelle image, mêmes montages et ports. Interruption de trafic de quelques secondes, annoncée avant confirmation **(défaut proposé)**.
3. `acme.json`, certificats et fichiers dynamiques étant sur l'hôte, l'upgrade ne perd aucun état.
4. Post-upgrade : `Verify` global (§6.3) sur un échantillon de routes ; échec → recréation avec l'image précédente (l'image n'est purgée qu'après vérification réussie) **(défaut proposé)**.

---

## 2. Représentation intermédiaire (IR)

### 2.1 Principes

- L'IR est **la** source unique de génération : Traefik (P0) et Caddy (P2) en dérivent de façon déterministe (ADR-009). Aucun champ de l'IR n'est propre à un proxy, à l'exception de l'échappatoire `provider_raw` (§4.6).
- L'IR d'un serveur est construite par le control plane depuis PostgreSQL (`servers`, `applications`, `domains`, `service_components`, `databases`, snapshots de déploiement) — jamais éditée à la main.
- **Sérialisation canonique** : JSON, clés triées, pas de timestamp ni de champ non déterministe. Deux états métier identiques produisent une IR octet-à-octet identique — fondement du checksum et de la détection de dérive (§18.3).
- **Aucune valeur de secret** dans l'IR : uniquement des références (`secret://…`), résolues à la génération vers des fichiers `0600` sur le serveur (INV-003).
- Versionnée : `ir_version` entier, incrémenté à chaque changement incompatible ; les fixtures (§9) sont taguées par version.

### 2.2 Schéma (structure documentée)

```yaml
ir_version: 1
server:
  server_uuid: "6d0f…"
  proxy_type: traefik            # traefik | caddy
  http_port: 80                  # proxy_http_port (§27.1) — ex. 8080 derrière un proxy amont
  https_port: 443                # proxy_https_port (§27.1) — ex. 8443
  acme:                          # présent si au moins un domaine en ACME
    email: "ops@example.com"
    ca_url: "https://acme-v02.api.letsencrypt.org/directory"   # staging en test
    resolvers:
      - name: http01             # resolver par défaut (§7.1)
        challenge: http-01
      - name: dns01-cloudflare   # nom déterministe : dns01-<provider> (§7.2)
        challenge: dns-01
        provider: cloudflare     # identifiant provider Lego (§4.3 PRD)
        credentials_ref: "secret://team/<team_uuid>/dns/cloudflare"
apps: []                         # liste de RouteGroup (§2.3), triée par resource_uuid
tcp_routes: []                   # liste de TCPRoute (§2.6), triée par listen_port
certificates: []                 # liste de Certificate (§2.5), triée par premier domaine
```

### 2.3 `RouteGroup` — routage HTTP d'une ressource

Un `RouteGroup` par application, preview ou service component routé. Il correspond **exactement** au périmètre du fichier dynamique `<app_uuid>.yaml` (deployment-engine §6.1) : générer un RouteGroup = générer un fichier ; le supprimer = supprimer le fichier.

```yaml
resource_uuid: "9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01"
team_uuid: "…"
kind: application                # application | service_component | preview
routes:                          # une entrée par ligne de `domains` (fqdn, path) — §8.4 data dictionary
  - fqdn: "app.example.com"      # sans schéma, citext ; wildcard serveur déjà matérialisé (§3.4)
    path: "/"                    # path-based routing (§4.2 PRD)
    target_port: 3000            # `domains.target_port`, sinon ports_exposes par défaut
    strip_path: false            # true = retirer le préfixe avant transmission (défaut proposé : false)
    https:
      enabled: true
      force: true                # force_https par application (§4.3 PRD, défaut true)
      cert: { resolver: http01 } # { resolver: <nom> } XOR { ref: <certificats §2.5> }
    redirect_direction: both     # both | www | non_www — « Direction » (§4.2 PRD, §3.5)
    middlewares: [auth, noindex] # refs vers §2.4, ordre d'application défini au §4.7
service:                         # cible unique — la bascule remplace ce bloc (deployment-engine §7.2)
  endpoint_type: container_name  # container_name (forme stable) | container_ip (forme transitoire)
  endpoint: "9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01"   # ou "172.18.0.7" pendant switching
  scheme: http                   # http | https (backend TLS) — (défaut proposé : http)
middlewares: []                  # définitions locales (§2.4)
scale_to_zero: null              # bloc optionnel (§8), previews d'abord
```

Notes :

- `endpoint_type`/`endpoint` sont fournis par le worker (`GenerateApp(ir, app, endpoint)`) : **IP du candidat** pendant `switching`, **nom du container** (DNS Docker) en `finishing` — strictement conforme à deployment-engine §7.2 (étapes 1–2 et 7).
- Multi-domaines = plusieurs entrées `routes` (une ligne `domains` chacune) ; `domaine:port` = `target_port` explicite (§4.2 PRD).
- Un `kind: preview` porte par défaut les middlewares de protection (§4.5) et le jeu `scale_to_zero` si activé.

### 2.4 `Middleware` — définitions

```yaml
- name: auth                       # unique dans le RouteGroup ; nom généré : <app_uuid>-<name>
  type: basic_auth                 # §4.1
  users_ref: "secret://team/<team_uuid>/app/<app_uuid>/basic_auth"   # htpasswd (hashes bcrypt)
- name: limit
  type: rate_limit
  average: 100                     # req/s en moyenne
  burst: 200
  period: "1s"
- name: office-only
  type: ip_whitelist
  cidrs: ["203.0.113.0/24", "2001:db8::/48"]
- name: noindex
  type: custom_headers
  response: { X-Robots-Tag: "noindex" }
  request: {}                      # en-têtes ajoutés vers le backend
- name: gzip
  type: compression
- name: sablier                    # échappatoire, hors conformance (§4.6)
  type: provider_raw
  provider: traefik
  payload: { … }
```

### 2.5 `Certificate` — certificats

```yaml
- id: "wildcard-preview"           # référencé par routes[].https.cert.ref
  type: acme                       # acme | custom | none
  resolver: dns01-cloudflare       # pour acme
  main: "preview.example.com"
  sans: ["*.preview.example.com"]  # wildcard ⇒ resolver DNS-01 obligatoire (§7.2)
- id: "corp-example"
  type: custom                     # fichiers déposés dans /var/lib/akerdock/proxy/certs (§7.3)
  cert_file: "certs/corp.example.com.pem"
  key_file: "certs/corp.example.com.key"
```

`type: none` = HTTP seul (route sans `https.enabled`). Le fallback self-signed n'apparaît pas dans l'IR : c'est un comportement garanti du provider (§7.4).

### 2.6 `TCPRoute` — proxy TCP des bases (§6.2 PRD)

Mode `public_access_mode = tcp_proxy` de la table `databases` : exposition publique d'une base sans port mapping sur son container (elle ne redémarre jamais pour un changement de port public).

```yaml
- resource_uuid: "4e7a…"           # UUID de la base
  team_uuid: "…"
  listen_port: 15432               # databases.public_port — modifiable sans toucher à la base
  target: { container: "4e7a…", port: 5432 }
  idle_timeout_seconds: 3600       # databases.tcp_proxy_timeout_seconds — best-effort par provider (§5.6)
  tls_passthrough: true            # le TLS de la base (§6.3 PRD) traverse tel quel (défaut proposé)
```

Contrainte : `listen_port` unique par serveur, disjoint de `http_port`/`https_port` (validé côté API). L'ensemble des `listen_port` actifs détermine les ports publiés du container proxy : le **modifier** déclenche une révision de configuration statique + recréation du container proxy (§5.6) — la base, elle, ne redémarre pas (parité §6.2) ; le **retargeting** (changer la base ou son port interne) est purement dynamique, sans redémarrage d'aucune sorte **(défaut proposé)**.

---

## 3. Règles de priorité de routage

Le comportement doit être **identique quel que soit le provider** : les priorités sont donc calculées dans l'IR, jamais déléguées aux heuristiques du proxy (Traefik trie par longueur de règle : proche mais non contractuel).

### 3.1 Formule de priorité (défaut proposé)

Pour chaque route HTTP :

```text
priority = 1000 × segments(path) + len(path)
```

où `segments(path)` = nombre de segments non vides (`/` → 0 ; `/api` → 1 ; `/api/v2` → 2) et `len(path)` = longueur du path en caractères (`/` → 1). Plus la valeur est haute, plus la route est prioritaire.

- **Path le plus spécifique d'abord** (§4.2 PRD) : `/api/v2` (2007) > `/api` (1004) > `/` (1) — quel que soit l'ordre de déclaration.
- Le FQDN n'entre pas dans la formule : deux FQDN différents ne sont jamais en concurrence (matching exact sur l'hôte), et deux routes de même `(fqdn, path)` sont **impossibles** (§3.3).
- Les routeurs de redirection (www/non-www, §3.5) héritent de la priorité de la route qu'ils desservent.

### 3.2 `domaine:port`

La syntaxe `domaine:port` (§4.2 PRD) cible un **port interne** du container (`domains.target_port`), pas un port d'écoute du proxy. Elle ne crée pas d'entrypoint : la route écoute toujours sur `http_port`/`https_port` et transmet vers `endpoint:target_port`.

### 3.3 Collisions interdites

- Unicité `(fqdn, path)` **globale à l'instance** — contrainte `UNIQUE` de la table `domains` (§8.4), qui élimine collisions inter-team et ambiguïtés (INV-002). Rejet à la validation API, avant toute IR.
- Défense en profondeur : `Validate` (§6.1) échoue si une IR contient deux routes `(fqdn, path)` identiques sur un serveur — une IR en collision n'est jamais appliquée.
- `listen_port` TCP : unicité par serveur + disjonction avec les ports HTTP/HTTPS (§2.6).

### 3.4 Wildcard `<uuid>.domaine` et previews

- Le **wildcard domain par serveur** (`servers.wildcard_domain`, fallback sslip.io, §4.2 PRD) est résolu **à la création de la ressource** : le control plane matérialise un FQDN exact (`<uuid>.example.com`, `is_generated = true` dans `domains`). L'IR et les proxies ne voient jamais de règle d'hôte wildcard — uniquement des hôtes exacts.
- Les URLs de preview (template `{{pr_id}}.{{domain}}`, `{{random}}`, §5.6 PRD) suivent le même chemin : FQDN exact par preview, `RouteGroup` propre (`kind: preview`, identité `(application_uuid, provider, pr_id)`), donc fichier dynamique propre et bascule indépendante de la production.
- Seuls les **certificats** peuvent être wildcard (`sans: ["*.…"]`, DNS-01, §7.2) : un certificat wildcard couvre N hôtes exacts.

### 3.5 Redirections www/non-www (« Direction »)

Champ `redirect_direction` (`both` | `www` | `non_www`, défaut `both` — data dictionary §8.3) :

| Valeur | Comportement généré |
|---|---|
| `both` | Le FQDN déclaré et sa contrepartie (`www.` ajouté ou retiré) servent tous deux l'application (deux routeurs vers le même service) |
| `www` | La contrepartie apex redirige `308` vers `www.<fqdn>` (schéma et path préservés) |
| `non_www` | `www.<fqdn>` redirige `308` vers l'apex |

La contrepartie n'est générée que si elle n'entre pas en collision avec une ligne `domains` existante (l'unicité §3.3 prime ; en cas de conflit, la redirection est omise et un avertissement de validation est remonté) **(défaut proposé)**. Les redirections HTTPS et www sont composées dans l'ordre du §4.7 (HTTPS d'abord : une seule redirection visible par requête au plus, vers l'URL finale).

---

## 4. Middlewares mappés

### 4.1 Basic auth

- IR `type: basic_auth`, `users_ref` vers le secret store. À la génération, la valeur (format htpasswd, hashes bcrypt) est écrite en `/var/lib/akerdock/proxy/auth/<app_uuid>.htpasswd` (0600, SFTP — INV-003/012) et le middleware référence ce fichier. Les hashes ne transitent **pas** dans `proxy_config_revisions.content` (qui ne contient aucun secret, §11.1 data dictionary) **(défaut proposé)**.
- Traefik : `basicAuth.usersFile: /auth/<app_uuid>.htpasswd`.

### 4.2 Rate limiting

- IR `type: rate_limit` (`average`, `burst`, `period`). Traefik : `rateLimit`. Sémantique contractuelle : limitation par IP source **(défaut proposé)** ; au-delà → `429`.

### 4.3 IP whitelist

- IR `type: ip_whitelist` (`cidrs`, IPv4/IPv6, validés centralement §23.3). Traefik v3 : `ipAllowList`. Hors liste → `403`.

### 4.4 Custom headers et compression

- `type: custom_headers` : en-têtes de réponse et/ou de requête (`headers.customResponseHeaders` / `customRequestHeaders`). Les noms `X-Forwarded-*` sont réservés au proxy et rejetés en validation **(défaut proposé)**.
- `type: compression` : Traefik `compress` (gzip/brotli négociés).

### 4.5 Protection des previews (§20.4.4)

Pour `kind: preview`, le control plane injecte par défaut (selon `preview_protection`, défaut `basic_auth`) :

1. un middleware `basic_auth` (credentials générés par preview, jeu de variables preview) — ou la validation de **lien signé** en P2 ;
2. un middleware `custom_headers` avec `X-Robots-Tag: noindex` — présent **même si** `preview_protection = none` (l'exposition publique reste non indexée) **(défaut proposé)**.

`preview_protection = none` est un choix explicite par application (§20.4.4).

### 4.6 Extensibilité

- Ajouter un type de middleware = étendre l'IR (nouveau `type`, bump éventuel de `ir_version`) **et** fournir son mapping pour chaque provider **et** ses fixtures (§9). Un type sans mapping complet est refusé.
- Échappatoire `type: provider_raw` (ADR-009, « extensions explicites ») : payload passé tel quel au provider nommé, **exclu des fixtures de conformité**, marqué dans l'UI comme non portable — un switch de proxy le signale et l'ignore.

### 4.7 Ordre d'application (déterministe, défaut proposé)

```text
force-https → redirection www/non-www → ip_whitelist → rate_limit → basic_auth → custom_headers → compression → provider_raw
```

Cet ordre est un invariant contractuel testé par fixtures : un client hors whitelist reçoit `403` avant tout challenge d'authentification ; les redirections précèdent tout le reste.

---

## 5. Génération Traefik (P0)

### 5.1 File provider — le fichier fait foi

Conforme à deployment-engine §7.1 : le routage est matérialisé en **un fichier de configuration dynamique par application** — `/var/lib/akerdock/proxy/dynamic/<app_uuid>.yaml` — monté dans le container (provider `file`, `watch: true`). Les **labels de parité** Traefik sont posés sur le container final en `finishing` (deployment-engine §7.2 étape 7) à des fins de diagnostic et de compatibilité d'usage, mais ne sont **jamais lus** par le proxy (pas de provider docker) : le fichier fait foi, c'est lui qui rend la bascule atomique et vérifiable.

Conventions de nommage dans les fichiers générés (déterministes, INV-011) :

| Objet Traefik | Nom |
|---|---|
| Routeur HTTP | `<app_uuid>-r<n>` (HTTPS) / `<app_uuid>-r<n>-web` (HTTP) — `n` = index de la route triée par `(fqdn, path)` |
| Routeur de redirection www | `<app_uuid>-r<n>-www` |
| Service | `<app_uuid>` (un seul service par RouteGroup) |
| Middleware | `<app_uuid>-<name>` (+ middlewares implicites `<app_uuid>-https-redirect`, `<app_uuid>-www-redirect`) |
| Routeur/service TCP | `<db_uuid>-tcp` |

En-tête obligatoire de chaque fichier généré : `# generated by AkerDock — revision <n> — DO NOT EDIT` ; toute édition manuelle est détectée par checksum (§6.2, §18.3).

### 5.2 Configuration statique du proxy

`/var/lib/akerdock/proxy/traefik.yaml`, générée par `GenerateStatic(ir)`. Tout changement (ports, resolvers, entrypoints TCP) crée une révision et **recrée le container** (§1.4) ; les changements de routage passent exclusivement par les fichiers dynamiques (hot reload).

```yaml
# generated by AkerDock — revision 12 — DO NOT EDIT
api:
  dashboard: false
  insecure: true          # API sur :8080, port NON publié sur l'hôte — accessible uniquement
                          # via docker exec (vérification §6.3)
ping: {}                  # healthcheck du container (§1.3)

entryPoints:
  web:
    address: ":80"        # = servers.proxy_http_port (§27.1) — ":8080" si 8080 configuré
  websecure:
    address: ":443"       # = servers.proxy_https_port — ":8443" si 8443 configuré
  tcp-15432:              # un entrypoint par TCPRoute active (§2.6)
    address: ":15432/tcp"

providers:
  file:
    directory: /dynamic
    watch: true

certificatesResolvers:
  http01:                                  # défaut (§7.1)
    acme:
      email: "ops@example.com"
      storage: /acme/acme.json
      httpChallenge:
        entryPoint: web
  dns01-cloudflare:                        # généré si un certificat le référence (§7.2)
    acme:
      email: "ops@example.com"
      storage: /acme/acme.json
      dnsChallenge:
        provider: cloudflare               # provider Lego ; credentials via env (§7.2)
        resolvers: ["1.1.1.1:53"]          # dns_validation_server de l'instance (§4.2, §14.2 PRD)

log:
  level: INFO
accessLog: {}
```

> HTTP-01 exige que Let's Encrypt atteigne le serveur sur le port **80 public**. Si `proxy_http_port ≠ 80` (proxy amont détenant 80/443, §27.1), l'amont doit relayer `/.well-known/acme-challenge/` vers le port configuré, sinon utiliser DNS-01 — contrainte documentée à la validation du domaine **(défaut proposé)**.

### 5.3 Exemple 1 — application simple en HTTPS

Application `9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01`, `app.example.com`, port interne 3000, `force_https = true`, `redirect_direction = both` sans contrepartie déclarable (pas de `www` généré ici pour la concision). Forme **stable** (`finishing`) ; pendant `switching`, seule l'URL du service diffère (`http://172.18.0.7:3000`, IP du candidat — deployment-engine §7.2).

```yaml
# /var/lib/akerdock/proxy/dynamic/9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01.yaml
# generated by AkerDock — revision 41 — DO NOT EDIT
http:
  routers:
    9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01-r0-web:
      entryPoints: [web]
      rule: Host(`app.example.com`) && PathPrefix(`/`)
      priority: 1
      middlewares: [9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01-https-redirect]
      service: 9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01
    9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01-r0:
      entryPoints: [websecure]
      rule: Host(`app.example.com`) && PathPrefix(`/`)
      priority: 1
      service: 9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01
      tls:
        certResolver: http01
  middlewares:
    9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01-https-redirect:
      redirectScheme:
        scheme: https
        permanent: true
  services:
    9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01:
      loadBalancer:
        servers:
          - url: "http://9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01:3000"   # DNS Docker par nom de container
```

### 5.4 Exemple 2 — multi-domaines, path routing et redirection non-www

Application `b2d15c78-90ab-4cde-8123-456789abcdef` : `shop.example` → port 3000, `shop.example/api` → port 8081 (`domaine:port`), `redirect_direction = non_www`.

```yaml
# /var/lib/akerdock/proxy/dynamic/b2d15c78-90ab-4cde-8123-456789abcdef.yaml
# generated by AkerDock — revision 42 — DO NOT EDIT
http:
  routers:
    b2d15c78-90ab-4cde-8123-456789abcdef-r0:
      entryPoints: [websecure]
      rule: Host(`shop.example`) && PathPrefix(`/`)
      priority: 1                                     # segments=0, len=1 (§3.1)
      service: b2d15c78-90ab-4cde-8123-456789abcdef
      tls: { certResolver: http01 }
    b2d15c78-90ab-4cde-8123-456789abcdef-r1:
      entryPoints: [websecure]
      rule: Host(`shop.example`) && PathPrefix(`/api`)
      priority: 1004                                  # segments=1, len=4 → prime sur "/"
      service: b2d15c78-90ab-4cde-8123-456789abcdef-api
      tls: { certResolver: http01 }
    b2d15c78-90ab-4cde-8123-456789abcdef-r0-www:      # www.shop.example → shop.example (308)
      entryPoints: [websecure]
      rule: Host(`www.shop.example`)
      priority: 1
      middlewares: [b2d15c78-90ab-4cde-8123-456789abcdef-www-redirect]
      service: noop@internal
      tls: { certResolver: http01 }
    b2d15c78-90ab-4cde-8123-456789abcdef-r0-web:      # HTTP : redirection HTTPS pour les deux hôtes
      entryPoints: [web]
      rule: Host(`shop.example`) || Host(`www.shop.example`)
      priority: 1
      middlewares: [b2d15c78-90ab-4cde-8123-456789abcdef-https-redirect]
      service: noop@internal
  middlewares:
    b2d15c78-90ab-4cde-8123-456789abcdef-https-redirect:
      redirectScheme: { scheme: https, permanent: true }
    b2d15c78-90ab-4cde-8123-456789abcdef-www-redirect:
      redirectRegex:
        regex: "^https?://www\\.shop\\.example/(.*)"
        replacement: "https://shop.example/${1}"
        permanent: true
  services:
    b2d15c78-90ab-4cde-8123-456789abcdef:
      loadBalancer:
        servers:
          - url: "http://b2d15c78-90ab-4cde-8123-456789abcdef:3000"
    b2d15c78-90ab-4cde-8123-456789abcdef-api:
      loadBalancer:
        servers:
          - url: "http://b2d15c78-90ab-4cde-8123-456789abcdef:8081"
```

### 5.5 Exemple 3 — preview protégée (basic auth + noindex + wildcard DNS-01)

Preview PR #123 de l'application ci-dessus, container `b2d15c78-…-pr-123`, FQDN `123.preview.example.com`, certificat wildcard `*.preview.example.com` (resolver `dns01-cloudflare`).

```yaml
# /var/lib/akerdock/proxy/dynamic/b2d15c78-90ab-4cde-8123-456789abcdef-pr-123.yaml
# generated by AkerDock — revision 43 — DO NOT EDIT
http:
  routers:
    b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-r0:
      entryPoints: [websecure]
      rule: Host(`123.preview.example.com`) && PathPrefix(`/`)
      priority: 1
      middlewares:
        - b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-auth      # ip_whitelist → rate_limit → auth → headers (§4.7)
        - b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-noindex
      service: b2d15c78-90ab-4cde-8123-456789abcdef-pr-123
      tls:
        certResolver: dns01-cloudflare
        domains:
          - main: "preview.example.com"
            sans: ["*.preview.example.com"]
    b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-r0-web:
      entryPoints: [web]
      rule: Host(`123.preview.example.com`)
      priority: 1
      middlewares: [b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-https-redirect]
      service: noop@internal
  middlewares:
    b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-https-redirect:
      redirectScheme: { scheme: https, permanent: true }
    b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-auth:
      basicAuth:
        usersFile: /auth/b2d15c78-90ab-4cde-8123-456789abcdef-pr-123.htpasswd
    b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-noindex:
      headers:
        customResponseHeaders:
          X-Robots-Tag: "noindex"
  services:
    b2d15c78-90ab-4cde-8123-456789abcdef-pr-123:
      loadBalancer:
        servers:
          - url: "http://b2d15c78-90ab-4cde-8123-456789abcdef-pr-123:3000"
```

### 5.6 Exemple 4 — route TCP vers une base de données

PostgreSQL `4e7a9b0c-1122-4334-8556-778899aabbcc`, port interne 5432, `public_port = 15432` (`public_access_mode = tcp_proxy`, §6.2 PRD). Prérequis statique : entrypoint `tcp-15432` (§5.2) + port publié sur le container proxy.

```yaml
# /var/lib/akerdock/proxy/dynamic/4e7a9b0c-1122-4334-8556-778899aabbcc.yaml
# generated by AkerDock — revision 44 — DO NOT EDIT
tcp:
  routers:
    4e7a9b0c-1122-4334-8556-778899aabbcc-tcp:
      entryPoints: [tcp-15432]
      rule: HostSNI(`*`)                 # passthrough TCP brut ; le TLS de la base (§6.3 PRD) traverse tel quel
      service: 4e7a9b0c-1122-4334-8556-778899aabbcc-tcp
  services:
    4e7a9b0c-1122-4334-8556-778899aabbcc-tcp:
      loadBalancer:
        servers:
          - address: "4e7a9b0c-1122-4334-8556-778899aabbcc:5432"
```

- Changer la **cible** (autre base, autre port interne) : fichier dynamique seul, à chaud.
- Changer le **port public** : révision statique + recréation du proxy (§2.6) — la base ne redémarre pas ; l'UI annonce l'interruption de quelques secondes du proxy.
- `idle_timeout_seconds` : appliqué si le provider le supporte ; Traefik v3 n'a pas de timeout d'inactivité par routeur TCP — la valeur y est sans effet (divergence documentée, testée en fixture comme « non garanti ») ; Caddy (layer4) l'applique. Le timeout par défaut de 3600 s reste porté par l'IR pour les providers capables.

### 5.7 Fichier réservé `00-control-plane.yaml` — FQDN de l'instance

Quand le serveur héberge l'instance (`servers.is_localhost`) **et** qu'un FQDN d'instance est configuré (`instance_settings.fqdn`, §14.2 PRD), le bootstrap du proxy converge le fichier réservé `00-control-plane.yaml` : le dashboard est servi derrière le proxy avec certificat automatique (`certResolver: http01`), redirection HTTPS forcée, route unique `Host(<fqdn>) && PathPrefix(/)` vers le control plane.

- Scope de révision : `00-control-plane` — mêmes mécanismes que les applications (révision checksummée §6.2.4, application atomique §6.2, vérification §6.3, réconciliation de drift §18.3). Le nom est réservé : les scopes applicatifs sont des UUIDs, aucune collision possible.
- Cible : `http://host.docker.internal:<AKERDOCK_PORT>` — le container proxy est lancé avec `--add-host=host.docker.internal:host-gateway` (le control plane est un service compose sur un autre réseau Docker, injoignable par nom de container). Le port est celui publié par le compose de l'instance (instance-config §4).
- Convergence au bootstrap uniquement (start/validation/recréation du proxy), idempotente : aucune révision n'est créée si le contenu désiré est identique à la dernière révision appliquée. FQDN retiré ou serveur qui n'héberge plus l'instance → retrait du routage (révision vide, §6.5).
- Retrait du FQDN ou changement de valeur : pris en compte au prochain bootstrap du proxy (redémarrage depuis la page serveur), pas à chaud — le réglage est rare et le redémarrage coûte quelques secondes.

---

## 6. Application atomique et vérification

Contrat d'application d'un fichier dynamique — identique que l'appelant soit le worker de déploiement (`switching`/`finishing`, deployment-engine §7.2 étapes 3–4 et 7) ou une mutation de configuration (domaine ajouté, middleware modifié, retrait de routage) :

### 6.1 Validation avant upload

`Validate` s'exécute côté control plane, avant toute mutation distante : schéma du format cible (YAML Traefik / JSON Caddy), unicité `(fqdn, path)` dans l'IR (§3.3), références de middlewares/certificats résolues, ports dans `1..65535`. Une IR invalide ne produit **aucune** révision.

### 6.2 Écriture atomique et checksum

1. Génération déterministe du contenu depuis l'IR ; calcul du **SHA-256**.
2. `INSERT proxy_config_revisions` (`server_id`, `revision` = n+1, `proxy_type`, `content`, `checksum_sha256`, `status = 'generated'`) — le `content` ne contient jamais de secret (§11.1 data dictionary ; les htpasswd passent par fichiers séparés, §4.1).
3. Upload SFTP vers `/var/lib/akerdock/proxy/dynamic/.<app_uuid>.yaml.tmp` puis `mv -f` vers `<app_uuid>.yaml` — rename atomique sur le même système de fichiers : Traefik ne voit jamais un fichier partiel.
4. Réconciliation de dérive (§18.3) : périodiquement et avant chaque bascule, le checksum du fichier distant est comparé à la dernière révision `applied` ; une dérive (édition manuelle) est signalée et corrigée par ré-application **(défaut proposé)**.

### 6.3 Attente de prise en compte et vérification

Conforme deployment-engine §7.2 étape 4 :

1. **API Traefik** : polling toutes les 1 s, max **30 s**, via `docker exec akerdock-proxy wget -qO- http://127.0.0.1:8080/api/http/services` (routes HTTP) et `/api/http/routers`, ou `/api/tcp/routers` + `/api/tcp/services` (routes TCP) — jusqu'à voir le routeur et l'endpoint attendus (URL exacte du service, y compris l'IP du candidat pendant `switching`).
2. **Requête de fumée** à travers le proxy, depuis le serveur lui-même :
   `curl -fsS -o /dev/null --max-time 5 --resolve <fqdn>:<proxy_port>:127.0.0.1 http://<fqdn><health_path>` (et variante `https://` avec `-k` si le certificat n'est pas encore émis — le self-signed de fallback répond, §7.4) **(défaut proposé)**.
   Pour une route TCP : test de connexion `nc -z 127.0.0.1 <listen_port>` **(défaut proposé)**.
3. Succès → `UPDATE proxy_config_revisions SET status = 'applied', applied_at = now()`.

### 6.4 Rollback du fichier en cas d'échec

Échec de l'étape 6.3 (timeout API ou fumée en échec) :

1. `status = 'failed'` + `error` sur la révision.
2. Ré-application du contenu de la **dernière révision `applied`** du même périmètre, par le même mécanisme tmp + mv + vérification ; la révision fautive passe à `rolled_back` une fois l'ancienne configuration re-vérifiée.
3. Dans le contexte d'une bascule de déploiement : c'est la compensation **C2** (deployment-engine §9.1) — le fichier re-pointe l'ancien container, qui n'a jamais cessé de tourner (INV-005) ; une re-tentative locale immédiate est permise avant C2 (deployment-engine §9.2).
4. Si la ré-application échoue elle-même (proxy down, disque plein) : le serveur passe en anomalie de routage — alerte, entrée « actions prioritaires », réconciliation par job dédié ; jamais de suppression du dernier fichier connu bon.

### 6.5 Retrait de routage

`RemoveApp` : suppression de `<app_uuid>.yaml` (+ `.awake`, htpasswd associés), vérification par l'API que routeurs et services de l'application ont disparu, révision dédiée tracée. Le retrait du routage **précède** toujours l'arrêt du workload (§20.6).

---

## 7. Certificats

### 7.1 ACME HTTP-01 (défaut)

- Émission et renouvellement automatiques Let's Encrypt (§4.3 PRD) via le resolver `http01` (§5.2). Le renouvellement est géré par le proxy lui-même (Traefik renouvelle à ~30 jours de l'échéance) — aucune action du control plane.
- Précondition validée à l'ajout du domaine : résolution DNS du FQDN vers le serveur via le DNS de validation de l'instance (`dns_validation_server`, défaut `1.1.1.1`, §4.2 PRD) ; avertissement bloquant sinon **(défaut proposé : avertissement non bloquant, l'utilisateur peut forcer — le DNS peut converger après coup)**.

### 7.2 DNS-01 (wildcard)

- Obligatoire pour tout **certificat** wildcard (§4.3 PRD). Un resolver `dns01-<provider>` par provider DNS utilisé sur le serveur, `provider` = identifiant **Lego** (cloudflare, route53, ovh, hetzner, …).
- **Un `wildcard_domain` sans credential DNS-01 est accepté** (amendement de spec) : le domaine ne sert alors que de **gabarit de nommage** — chaque hôte attribué sous le wildcard reçoit son **propre certificat individuel via HTTP-01** (§7.1, `certResolver: http01` par routeur). Contreparties assumées, à afficher à l'opérateur : chaque hôte doit être joignable publiquement sur le port HTTP du serveur (l'ACME HTTP-01 l'exige), et les limites d'émission de la CA s'appliquent **par hôte** (~50 certificats/domaine enregistré/semaine chez Let's Encrypt) — un usage intensif des previews peut les épuiser, là où un certificat wildcard n'en consomme qu'un.
- **Credentials** : stockés dans le secret store (chiffrement enveloppe, §23.2 — table `cloud_credentials`, référencée par `certificates.dns_credential_id`, data dictionary §6.7), référencés par `credentials_ref` dans l'IR ; matérialisés à la génération en `/var/lib/akerdock/proxy/acme.env` (**emplacement normatif**, 0600, SFTP) sous les noms de variables attendus par Lego (ex. `CF_DNS_API_TOKEN=…`), injectés au container proxy par `--env-file` (§1.3). Jamais dans `proxy_config_revisions.content`, jamais dans argv (INV-003/012). Rotation d'un credential = régénération du fichier + recréation du container.
- Un certificat wildcard est demandé via `tls.domains` sur un routeur qui le référence (exemple §5.5).

### 7.3 Certificats custom

- Dépôt (UI/API) des fichiers PEM dans `/var/lib/akerdock/proxy/certs/` (0600 ; la clé privée ne quitte jamais le serveur — pas en base, §11.1 data dictionary), déclarés dans le fichier dynamique réservé :

```yaml
# /var/lib/akerdock/proxy/dynamic/00-certificates.yaml
# generated by AkerDock — revision 45 — DO NOT EDIT
tls:
  certificates:
    - certFile: /certs/corp.example.com.pem
      keyFile: /certs/corp.example.com.key
```

- Traefik sélectionne le certificat par SNI ; une route dont le `Certificate` est `type: custom` n'a pas de `certResolver` (elle porte `tls: {}` simple). Expiration surveillée par le control plane (lecture du PEM) : alerte à J-30/J-7 **(défaut proposé)**.

### 7.4 Fallback self-signed

- Comportement contractuel (§4.3 PRD) : tant qu'aucun certificat valide n'est disponible (émission ACME en cours ou en échec), le proxy sert son **certificat par défaut auto-signé** — Traefik le fait nativement — plutôt que de refuser la connexion. Le trafic HTTP et la route restent fonctionnels ; la fixture correspondante (§9) valide « TLS répond toujours, même sans certificat émis ».

### 7.5 Stockage et alertes

- Stockage ACME : `/var/lib/akerdock/proxy/acme.json` (**emplacement normatif**, 0600), inclus dans le périmètre de backup de l'instance/du serveur (§7.5 PRD) — sa perte n'est pas grave (ré-émission) mais coûte du rate limit Let's Encrypt.
- **Alerte d'échec d'émission** : le control plane surveille l'émission après application d'une route ACME — présence du certificat dans `acme.json` (ou API Traefik) sous **10 min (défaut proposé)** ; sinon, événement `proxy.certificate_issue_failed.v1` (outbox §24.2) → notification (§11 PRD) avec la cause extraite des logs du proxy (échec de challenge, rate limit, CAA…). Le fallback self-signed (§7.4) reste servi entre-temps.
- Rate limits Let's Encrypt : l'instance utilise le CA de staging dans les E2E DinD (§27.26) ; `ca_url` est configurable dans l'IR (§2.2).

### 7.6 Synchronisation vers la table `certificates`

Le control plane maintient un **reflet observé** de l'état des certificats de chaque serveur dans la table `certificates` (data dictionary §6.7) : après chaque `Apply` touchant une route TLS et lors de la réconciliation périodique (§18.3), le worker lit `acme.json` et les PEM de `certs/` par SFTP (métadonnées uniquement — le matériel de clé privée n'est jamais rapatrié) et met à jour domaines couverts, émetteur, `not_before`/`not_after`, statut (`pending`/`issued`/`renewing`/`failed`/`expired`/`revoked`), dernière erreur et `observed_at`. Ce reflet alimente l'inventaire API (`GET /servers/{uuid}/certificates`, filtre `expiring_within_days`), l'alerte d'expiration à J-30/J-7 (§7.3) et l'alerte d'échec d'émission (§7.5) ; il n'est **jamais** une source de vérité — l'état réel reste `acme.json` et les fichiers du serveur. `POST /certificates/{uuid}/renew` force une ré-émission via un job audité (sauvegarde puis retrait ciblé de l'entrée d'`acme.json`, redémarrage du proxy — cf. runbook certificats), suivie d'une resynchronisation du reflet.

---

## 8. Scale-to-zero (DEVRAIT, §20.4.3) — mécanisme proposé

Tout ce chapitre est **(défaut proposé)** : le PRD exige seulement que le proxy DEVRAIT supporter le scale-to-zero (arrêt du container idle, réveil à la première requête), previews d'abord.

### 8.1 Composant « waker » local au serveur

Un helper container `akerdock-waker` (`akerdock.type=helper`, image du projet, épinglée par release) est déployé sur les serveurs où au moins une ressource a `scale_to_zero` activé. Il écoute sur le réseau interne (jamais publié sur l'hôte) et dispose du socket Docker local, limité par son code au démarrage de containers portant `akerdock.managed=true`.

Pourquoi pas le control plane : INV-007 (le control plane ne proxyfie pas le trafic applicatif) et architecture push (§18.1 — le serveur ne contacte jamais le control plane). Le réveil fonctionne donc même control plane éteint ; le control plane est informé a posteriori par réconciliation d'état observé (§18.3, §21.2).

### 8.2 Endormissement et réveil

**Endormissement** (control plane, sur TTL d'inactivité §20.4.3 — mesuré sur les access logs du proxy ou les métriques Sentinel) :

1. Générer **deux** variantes du fichier dynamique : la variante « sleeping » (service du RouteGroup → `http://akerdock-waker:8080`, en-tête de requête `X-AkerDock-Wake: <app_uuid>` ajouté par middleware) et la variante « awake » normale, déposée en `/var/lib/akerdock/proxy/dynamic/.<app_uuid>.yaml.awake`.
2. Appliquer la variante sleeping (§6), puis `docker stop` du container. État désiré : `sleeping`.

**Réveil** (waker, à la première requête) :

1. Requête entrante → `docker start <app_uuid>` (idempotent) ; état `waking`.
2. Attente du healthcheck (`healthy`, ou `running` stable 10 s sans healthcheck), timeout **60 s (défaut proposé)**.
3. `mv -f .<app_uuid>.yaml.awake <app_uuid>.yaml` (bascule atomique, même mécanisme que §6.2 — le waker ne génère rien, il échange des fichiers pré-générés par le control plane).
4. **Rejeu** : la requête retenue est transmise au container (hold-and-forward) ; les requêtes suivantes empruntent le chemin normal dès la prise en compte du fichier par Traefik. Pour un client navigateur (`Accept: text/html`) le waker PEUT servir une page d'attente auto-rafraîchissante à la place du hold.

```text
États : sleeping ──(1ʳᵉ requête)──▶ waking ──(healthy + swap fichier)──▶ running
             ▲                                                             │
             └───────────────(TTL d'inactivité, control plane)◀────────────┘
```

### 8.3 Limites assumées

- **Timeout de réveil** : au-delà de 60 s (cold start long), `504` + page d'erreur explicite ; l'app reste `waking` jusqu'à résolution ou retour en `sleeping` par le control plane.
- **Corps de requête** : hold-and-forward borné à **1 MiB (défaut proposé)** ; au-delà, `503` + `Retry-After: 5` (le client rejoue).
- **WebSockets** : un upgrade pendant `waking` est retenu puis proxifié une fois le container sain, dans la limite du timeout ; les WS de longue durée empêchent par ailleurs la détection d'inactivité — une ressource à WS persistants est un mauvais candidat au scale-to-zero (documenté).
- Le premier octet de réponse subit la latence complète du démarrage ; le scale-to-zero est activé par ressource, previews d'abord, jamais implicitement en production.

---

## 9. Fixtures de conformité

Les fixtures (ADR-009, existantes **dès P0**) garantissent que Caddy (P2) reproduira exactement le comportement de Traefik : **mêmes fixtures, deux providers**.

### 9.1 Format

```text
tests/proxy-conformance/
├── cases/<nom_du_cas>/
│   ├── ir.json                  # IR complète du serveur (entrée unique, ir_version épinglée)
│   ├── expected/traefik/        # sortie attendue de GenerateStatic + GenerateApp
│   │   ├── traefik.yaml
│   │   └── dynamic/<app_uuid>.yaml…
│   ├── expected/caddy/          # renseigné en P2 (absent = cas non encore porté, CI le signale)
│   └── assertions.yaml          # assertions comportementales HTTP/TCP (§9.2)
└── harness/                     # backend echo + client d'assertions (DinD, §27.26)
```

Deux niveaux de test, tous deux obligatoires pour qu'un provider soit conforme :

1. **Golden files** : `Generate*(ir.json)` doit produire octet-à-octet `expected/<provider>/` (déterminisme, base du checksum).
2. **Assertions comportementales** : le proxy est lancé en Docker-in-Docker (§27.26) avec la config générée et un backend echo (répond ses en-têtes reçus + `X-Backend: <nom>`) ; chaque assertion est une requête réelle. C'est ce niveau qui rend les providers interchangeables : deux configs différentes, un seul comportement.

### 9.2 Format des assertions

```yaml
# assertions.yaml — cas "multi-domaines + path"
- name: "path le plus spécifique d'abord"
  request: { method: GET, url: "https://shop.example/api/users", insecure_tls: true }
  expect:  { status: 200, backend: "api-8081" }
- name: "redirection non-www"
  request: { method: GET, url: "https://www.shop.example/x?y=1", follow_redirects: false, insecure_tls: true }
  expect:  { status: 308, headers: { Location: "https://shop.example/x?y=1" } }
- name: "force HTTPS"
  request: { method: GET, url: "http://shop.example/", follow_redirects: false }
  expect:  { status: 308, headers: { Location: "https://shop.example/" } }
- name: "preview : non authentifié refusé, jamais indexé"
  request: { method: GET, url: "https://123.preview.example.com/", insecure_tls: true }
  expect:  { status: 401 }
- name: "preview : authentifié, X-Robots-Tag présent"
  request: { method: GET, url: "https://123.preview.example.com/", basic_auth: "preview:s3cret", insecure_tls: true }
  expect:  { status: 200, headers: { X-Robots-Tag: "noindex" }, backend: "pr-123" }
- name: "TCP base de données"
  tcp:     { connect: "127.0.0.1:15432" }
  expect:  { connected: true, backend: "pg-echo" }
```

`insecure_tls: true` accepte le self-signed de fallback (les fixtures n'émettent pas de vrais certificats ; un cas dédié utilise Pebble/staging pour le flux ACME complet). Les URLs utilisent les ports du cas (`http_port`/`https_port` de l'IR) — le cas « ports non standard 8080/8443 » fait partie du jeu minimal.

### 9.3 Jeu de cas minimal (P0)

app simple HTTP ; app HTTPS force ; multi-domaines + paths + `domaine:port` ; redirections www/non-www (3 directions) ; preview protégée (auth + noindex) ; wildcard matérialisé `<uuid>.domaine` ; middlewares (rate limit 429, ip_whitelist 403 avant auth — ordre §4.7) ; certificat custom ; fallback self-signed ; TCP base ; ports proxy 8080/8443 ; bascule (deux `ir.json` séquentiels : IP transitoire puis nom stable — vérifie qu'aucune requête n'échoue pendant le swap) ; retrait de routage (404 après `RemoveApp`).

---

## 10. Mapping Caddy (P2, esquisse)

Objectif : démontrer que l'IR est suffisante — pas une spécification complète (elle accompagnera la livraison P2, validée par les fixtures §9).

| Élément IR | Traefik | Caddy |
|---|---|---|
| Route `(fqdn, path)` | router `Host && PathPrefix` + priority | bloc de site `fqdn` + matcher `path /api/*` ; l'ordre des matchers est **émis trié** par la priorité §3.1 (Caddy évalue dans l'ordre de la config JSON) |
| Service (endpoint IP ou nom) | `loadBalancer.servers[].url` | `reverse_proxy <endpoint>:<port>` |
| force HTTPS | routeur web + `redirectScheme` | natif (auto-HTTPS redirect) ; désactivé par site si `https.enabled: false` (`auto_https off` ciblé) |
| Redirection www | `redirectRegex` | `redir` dans un site dédié à la contrepartie |
| basic_auth | `basicAuth.usersFile` | `basic_auth` (bcrypt) |
| rate_limit | `rateLimit` | module `rate_limit` (extension embarquée dans l'image Caddy AkerDock) |
| ip_whitelist | `ipAllowList` | matcher `remote_ip` + `abort`/`respond 403` |
| custom_headers / compression | `headers` / `compress` | `header` / `encode gzip br` |
| ACME HTTP-01 / DNS-01 | certResolvers | natif / modules DNS Caddy (credentials via env, même fichier `acme.env`) |
| Certificat custom | `tls.certificates` | `tls /certs/x.pem /certs/x.key` |
| Fallback self-signed | certificat par défaut Traefik | `tls internal` en fallback |
| Route TCP | routeur TCP + entrypoint | module `layer4` (image AkerDock) — supporte `idle_timeout` (§5.6) |
| Ports 80/443 configurables | adresses d'entrypoints | `http_port` / `https_port` globaux |

Esquisse pour l'exemple §5.3 (Caddyfile ; la génération réelle émettra du JSON Caddy, plus déterministe) :

```caddyfile
{
  http_port 80
  https_port 443
  email ops@example.com
}
app.example.com {
  reverse_proxy 9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01:3000
}
```

Différence opérationnelle assumée : Caddy ne surveille pas un répertoire de fichiers — l'application atomique (§6) passe par son **admin API** locale (`POST /load`, transactionnel, rollback natif en cas de config invalide), appelée via `docker exec` comme l'API Traefik ; le contrat §6 (génération → checksum → apply → verify → rollback, un périmètre par application) est inchangé, seul le transport d'application diffère. Les fichiers par application restent la matérialisation sur disque (assemblés avant `POST /load`), conservant le diagnostic et la réconciliation par checksum.

---

## 11. Traçabilité PRD

| Section de cette spec | Sections PRD / specs |
|---|---|
| 1 | §4.1, §16.1(5), §18.1, §20.1, §20.6, §27.1, ADR-001, deployment-engine §1.2 |
| 2 | §4.2–4.3, §6.2, §18.3, §27.9/ADR-009, data dictionary §8.3–8.4, §9.1 (`databases`), deployment-engine §6.1, §7.2 |
| 3 | §4.2, §5.6, §17 (INV-002/011), data dictionary §8.4 |
| 4 | §4.1, §20.4.4, §23.2–23.3, ADR-009 |
| 5 | §4.1–4.3, §6.2, §14.2, §27.1, deployment-engine §5.1, §6.1, §7.1–7.2 |
| 6 | §17 (INV-005), §18.3, §20.6, data dictionary §11.1, deployment-engine §7.2, §9 |
| 7 | §4.3, §7.5, §11, §14.2, §23.2, §24.2, data dictionary §6.7 (`certificates`) |
| 8 | §17 (INV-007), §18.1, §20.4.3, §21.2 |
| 9 | §26.1, §27.9/ADR-009, §27.26 |
| 10 | §4.1, §26.1 (P2), §27.9/ADR-009 |
