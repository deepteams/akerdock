# ADR-032 — Tunnel TCP du CLI : WebSocket multiplexée (extension d'ADR-024)

- **Statut** : Accepté
- **Date** : 2026-07-25
- **Sections PRD liées** : §12 (CLI), §5.7 (exploitation), §24.4 (sessions temps réel), §27.1 (port unique)
- **Révise** : ADR-024 (élargit le périmètre du WebSocket) ; s'inscrit sous la clause d'ADR-027 (tout nouveau chemin d'accès exige un ADR)

## Contexte

Le CLI (ADR-031) doit permettre de **débugger une ressource sans l'exposer** : se connecter
à une base, un redis, un service compose ou une preview depuis le poste du développeur. Ces
services ne publient aucun port public (compose-spec §9 : sans domaine = privé, c'est
voulu). Le seul canal disponible du client vers eux est le control plane, sur 443.

ADR-024 a **réservé le WebSocket au terminal**, au motif qu'il était « le seul flux
bidirectionnel ». Un tunnel TCP est un second flux authentiquement bidirectionnel (octets
dans les deux sens) — le même critère, une seconde fois. Il faut donc un ADR qui étende
ADR-024 plutôt que le contredire. ADR-027 (retrait des tunnels Cloudflare) impose par
ailleurs que toute réintroduction d'un chemin d'accès reparte d'un ADR : c'est celui-ci. À
noter que ce tunnel n'est **pas une nouvelle exposition publique** — c'est un opérateur
authentifié qui atteint son propre workload à travers le port control plane déjà exposé.

## Décision

### Mint / redeem (motif terminal)

- **Mint (dans le contrat OpenAPI, `x-required-permission: write`)** :
  `POST /applications/{uuid}/port-forwards`, `POST /databases/{uuid}/port-forwards`,
  `POST /services/{uuid}/port-forwards`, `POST /applications/{a}/previews/{p}/port-forwards`
  (paramètre `component` optionnel, même sémantique que les sessions terminal). Corps :
  `{port: 1–65535}`. La **cible (conteneur, port) est figée et autorisée au mint**, une
  seule fois, auditée. Réponse `PortForwardSession` : `{uuid, token ("akdp_"+hex, usage
  unique, TTL 60 s, hash stocké), websocket_path:"/tunnel/ws", expires_at}`. Plafond par
  team `port_forward_limit` (défaut **10, proposé**) → `409` au-delà.
- **Redeem (hors contrat, comme `/terminal/ws`)** : `GET /tunnel/ws?token=akdp_…` — limiteur
  par IP, claim atomique à usage unique.
- **Exclu explicitement** : `/servers/{uuid}/port-forwards`. Un forward au niveau serveur est
  un `ssh -L` réinventé avec la clé de déploiement de la plateforme ; les opérateurs ont
  déjà SSH. Les cibles sont des ressources adossées à un conteneur, uniquement.

### Une WebSocket multiplexée par session

Une WS par connexion TCP forcerait soit un mint+écriture DB+audit **par connexion TCP**
(pathologique pour du trafic HTTP de dev qui ouvre des dizaines de connexions), soit un token
ré-utilisable (rompant l'invariant usage-unique de la maison). Donc **une session = un mint,
un audit d'ouverture, un audit de fermeture, une WS** ; les invariants du terminal (token
`akdp_` usage unique, idle 15 min, max 4 h, heartbeat 20 s, teardown garanti, audit
ouverture/fermeture) s'appliquent à la **session**. yamux complet est rejeté comme
dépendance : la WS fournit déjà des frontières de trames, une couche de multiplexage minimale
suffit.

### Protocole (sous-protocole `akerdock-tunnel-v1`)

- **Trames texte** = contrôle JSON. Client→serveur : `{"t":"open","id":N}` (sans adresse — la
  cible a été figée au mint ; le protocole est **sans adresse par conception**, ce qui
  ferme la porte à toute dérive de périmètre). Serveur : `{"t":"open_ok","id":N}` ou
  `{"t":"open_err","id":N,"code":"dial_failed|limit","msg":…}`. Les deux sens :
  `{"t":"eof","id":N}` (demi-fermeture TCP), `{"t":"close","id":N}`.
- **Trames binaires** = `[u32 big-endian id de stream][charge utile]`.
- Limites (défauts proposés) : **32** streams concurrents max par session ; buffer serveur en
  attente **1 MiB** par stream, puis fermeture du stream. **Pas de fenêtre de contrôle de
  flux en v1** — le head-of-line blocking entre streams d'une même session est une limite
  acceptée et documentée (outil de debug, typiquement 1–3 connexions concurrentes).

### Côté serveur

Un **canal SSH `direct-tcpip` par stream** sur la connexion SSH poolée existante vers le
serveur (`golang.org/x/crypto/ssh` multiplexe nativement les canaux — pas de nouvelle
connexion SSH par stream). Cible du dial : l'IP du conteneur sur son réseau Docker (joignable
depuis l'hôte), résolue par `docker inspect` à l'ouverture de la session. Contrainte honnête
à énoncer dans la spec : `internal/sshexec.Client` garde `*ssh.Client` privé et n'expose que
des méthodes orientées exec — il faut une petite extension `DialTCP(addr)` ; et **n'importe
quel port du conteneur cible est joignable depuis l'hôte** (Docker ne filtre pas
hôte→conteneur) — la **frontière d'autorisation est donc la ressource, pas le port**. La spec
le dit explicitement, plutôt que de feindre un contrôle par `EXPOSE` qui n'en est pas un.

## Alternatives considérées

- **Une WS par connexion TCP** : rejeté — mint/audit par connexion (ingérable) ou token
  ré-utilisable (rompt l'invariant usage-unique).
- **yamux / multiplexeur complet** : rejeté — dépendance lourde pour un besoin couvert par
  une couche minimale au-dessus des trames WS.
- **Tunnel SSH côté client** (le CLI ouvre lui-même le SSH vers le serveur) : rejeté — la clé
  de déploiement ne quitte jamais le control plane (ADR-001/ADR-003), et cela ouvrirait un
  accès réseau direct client→serveur, contraire à l'invariant de transport (ADR-031).

## Conséquences

- **Positives** : debug de toute ressource conteneur depuis le poste, sans exposition ni SSH
  direct ; réemploi intégral du motif terminal (token, cap, audit, teardown) ; un seul port,
  une seule pile d'auth.
- **Négatives** : head-of-line blocking entre streams d'une session (accepté) ; extension
  `DialTCP` à `internal/sshexec` ; une table technique de plus (`port_forward_sessions`).
- **Risques acceptés** : l'autorisation est au grain de la ressource, pas du port — un
  détenteur de `write` sur une ressource atteint tous les ports de ses conteneurs. C'est
  cohérent avec le terminal (`docker exec` donne déjà tout le conteneur).
