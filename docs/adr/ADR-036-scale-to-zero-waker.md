# ADR-036 — Scale-to-zero par un waker en coupure

## Statut

Accepté — précise les **défauts proposés** de
[proxy-contract §8](../specs/proxy-contract.md) (Scale-to-zero, DEVRAIT), qu'il
verrouille ; complète [ADR-011](ADR-011-cycle-de-vie-des-previews.md) (cycle de
vie des previews) et [ADR-024](ADR-024-sse-et-websocket.md) sans les superséder.

## Contexte

Une preview de PR passe l'essentiel de son temps **inactive** : ouverte pour une
revue, consultée quelques minutes, puis oubliée jusqu'à la prochaine relecture ou
son TTL. Elle consomme pourtant CPU/RAM en continu (souvent un stack complet
nats/postgres/redis/app). Le scale-to-zero éteint la ressource après une période
d'inactivité et la rallume à la première requête — l'objectif structurant du flow
« CD par PR » : on peut garder beaucoup de previews vivantes sans les payer toutes
en permanence.

Deux invariants encadrent le mécanisme :

- **INV-007** — le control plane ne proxifie jamais le trafic applicatif. Le
  réveil doit donc se produire **côté serveur**, pas dans le control plane.
- **push §18.1** — le serveur ne contacte jamais le control plane ; c'est le
  control plane qui se connecte au serveur (SSH). Le réveil doit fonctionner
  **même control plane éteint**.

proxy-contract §8 propose (toutes clauses « défaut proposé ») un conteneur helper
`akerdock-waker` et un basculement de **deux variantes** du fichier dynamique
(`sleeping` → route vers le waker, `awake` → route directe), échangées par un
`mv -f` atomique au réveil. Ce défaut a deux angles morts pour notre choix de
mesure d'inactivité : une fois **réveillée**, la variante `awake` route
directement vers l'app, donc **le waker ne voit plus passer le trafic** et ne peut
pas dater la dernière requête ; mesurer l'inactivité imposerait alors de parser
les access logs du proxy.

## Décision

### 1. Le waker est un mode du binaire unique

Pas de second artefact. `akerdock waker` est une sous-commande du binaire
existant (ADR-021, full-Cobra ADR-033), déployée en conteneur helper avec la
**même image** épinglée par la release, labellisé `akerdock.type=helper` et
`akerdock.managed=true`, sur le réseau interne du serveur (**jamais publié**),
avec accès au socket Docker local. Son code est **borné** à démarrer des
conteneurs `akerdock.managed=true` : il ne crée, ne supprime, ni ne construit
rien.

### 2. Le waker reste en coupure et rapporte l'activité

On **écarte le basculement à deux variantes** au profit d'une **variante unique** :
pour une ressource `scale_to_zero`, le fichier dynamique route **toujours** vers le
waker (`http://akerdock-waker:8080`, middleware ajoutant l'en-tête
`X-AkerDock-Wake: <resource_uuid>`). Le waker est donc un reverse-proxy permanent
**en coupure** devant les ressources STZ :

- **endormie** (`sleeping`) : le conteneur cible est arrêté. À la première requête,
  le waker `docker start <uuid>` (idempotent, état `waking`), attend `healthy`
  (ou *running* stable 10 s à défaut de healthcheck), délai max **60 s**, puis
  **retient-et-relaie** la requête d'origine ;
- **réveillée** (`running`) : le waker relaie chaque requête vers le conteneur et
  **date la dernière activité** dans un fichier local
  (`/var/lib/akerdock/waker/<uuid>.activity`, timestamp Unix, réécriture atomique).

C'est ce fichier qui matérialise « le waker rapporte l'activité » : le control
plane le **lit via SSH** (jamais le serveur qui appelle le control plane —
push §18.1 préservé) lors de sa passe d'endormissement.

### 3. Endormissement piloté par le control plane

Une passe du scheduler (aux côtés du reaper TTL) sélectionne les ressources STZ
`running` dont la dernière activité (fichier waker lu par SSH) dépasse
`scale_to_zero_after_minutes` (défaut 30) et enfile un job qui : `docker stop`
le conteneur puis passe l'état à `sleeping`. Le fichier dynamique n'a **pas** à
changer (il pointe déjà vers le waker) — l'endormissement est un simple
`docker stop`, le réveil un `docker start`. Aucun basculement de fichier, aucun
parsing d'access logs.

### 4. Machine à états et limites

États : `sleeping → waking → running → (inactivité) → sleeping`. Ajout des états
`sleeping` et `waking` à l'énum `preview_status`. Limites (proxy-contract §8.3) :
réveil > 60 s → **504** ; corps de requête retenu ≤ **1 MiB**, au-delà **503
Retry-After: 5** ; WebSockets retenus pendant `waking` (une WS longue est un
mauvais candidat au STZ) ; opt-in **par ressource**, **previews d'abord**, jamais
implicite en production.

## Conséquences

- **Positives** : mesure d'inactivité **exacte** (par requête) sans parser de
  logs ; réveil fonctionnel control plane éteint (INV-007, push §18.1) ; zéro
  second artefact (ADR-021) ; endormir/réveiller = `docker stop`/`start`, sans
  toucher au fichier dynamique ni aux certificats.
- **Négatives / limites** : le waker est **en coupure permanente** des ressources
  STZ — un saut interne supplémentaire (local, réseau interne, coût négligeable)
  et un **SPOF pour les seules ressources STZ** si le waker tombe (mitigé :
  `restart: always`, previews-first, opt-in). C'est le prix assumé du choix
  « le waker rapporte l'activité » : la variante `awake` directe de §8.2 le
  rendrait aveugle au trafic établi.
- Le réveil de bout en bout (retenir-et-relayer, healthy, timeout) relève de la
  **validation E2E** (ADR-028) : les tests unitaires couvrent la décision de
  réveil, les limites et la génération du fichier dynamique ; le comportement live
  passe par le parcours DinD.

## Alternatives rejetées

- **Deux variantes + `mv -f` atomique (défaut §8.2)** : rend le waker aveugle au
  trafic une fois réveillé, donc impose de parser les access logs Traefik pour
  mesurer l'inactivité — un couplage au format de log et une source de vérité
  fragile, pour économiser un saut interne négligeable.
- **Image `akerdock-waker` dédiée (littéral §8.1)** : un second binaire à
  construire, publier, versionner et épingler, contre ADR-021 (livrable unique).
- **Le waker rapporte l'activité au control plane (HTTP)** : violerait push §18.1
  (le serveur ne contacte jamais le control plane) et casserait le réveil quand le
  control plane est éteint. Le fichier local lu par SSH conserve les deux
  invariants.
- **Mesure via métriques Sentinel** : approximative (un conteneur idle mais vivant
  consomme un peu de CPU/réseau) et dépendante de Sentinel activé ; le waker en
  coupure donne l'activité réelle.
