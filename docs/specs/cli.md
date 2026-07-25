# Spécification — CLI locale AkerDock (`akerdock`)

> Contrat du client en ligne de commande. Sources de vérité amont : PRD (`docs/PRD.md`)
> §12 (API/CLI), §5.7 (exploitation : logs, terminal), §27.18 (`akerdock up`, v2) ;
> ADR-018 (déploiement local, v2), ADR-021 (binaire unique), ADR-024 (temps réel :
> SSE + WebSocket), ADR-027 (chemins d'accès), ADR-031 (auth CLI), ADR-032 (tunnel TCP),
> ADR-033 (Cobra). Contrat d'API : `docs/specs/openapi-v1.yaml`. Autorisations :
> `docs/specs/rbac-matrix.md`. Menaces : `docs/specs/threat-model.md`.
>
> Périmètre : **v1 « debug »** — `login`, contextes, listing, logs (dont `-f`), shell,
> port-forward TCP, console de base typée. Le déploiement depuis le poste (`akerdock up`,
> ADR-018) et la gestion env/domaines/clés relèvent de **v2** et ne sont pas spécifiés ici.
>
> Les défauts non tranchés par le PRD sont marqués **(défaut proposé)**.

---

## 1. Périmètre et non-buts

**Buts v1.** Donner à un développeur, depuis son poste, un accès de debug à ses ressources
sans les exposer : s'authentifier (y compris en SSO/OIDC), lister les ressources, lire les
logs (snapshot et streaming), ouvrir un shell dans un conteneur, établir un tunnel TCP vers
un service (base, redis, …), et une console typée de confort.

**Non-buts v1.** Déploiement (`up`, rollback, `deploy`), gestion des variables d'env, des
domaines, des clés, des backups, des membres. La CLI **NE réimplémente jamais** de logique
métier : elle consomme l'API publique (§18.2 PRD), rien d'autre.

## 2. Invariant de transport

- La CLI **NE se connecte qu'au** FQDN du manager du contexte actif, **sur 443** (80
  uniquement pour un éventuel redirect→HTTPS). Aucune autre destination réseau.
- La CLI **n'ouvre aucun port** entrant ni loopback — uniquement des requêtes sortantes.
- `shell` et `port-forward` ouvrent un `wss://<manager>/…` sur 443 (en-têtes Upgrade
  WebSocket standard, comme le terminal web, qui traverse déjà proxies et load-balancers).
  Le tunnel vers le **serveur cible** est établi côté manager (SSH) — jamais côté client.
- Tout **DOIT** fonctionner à travers un proxy/LB intermédiaire ; les heartbeats (20 s)
  maintiennent les WebSockets ouvertes malgré les idle-timeouts des LB.

## 3. Commandes

`REF` désigne une ressource : `app/<nom|uuid>`, `db/<nom|uuid>`, `svc/<nom|uuid>`,
`preview/<pr|uuid>`. La team vient du contexte actif ; `--team`/`-a` l'emporte.

| Commande | Rôle |
|---|---|
| `akerdock serve all-in-one\|api\|worker\|scheduler` | Modes serveur (ADR-033). |
| `akerdock healthcheck` | Sonde de la healthcheck compose. |
| `akerdock login [--url URL] [--context NAME] [--scopes read,write] [--with-token] [--no-browser]` | Authentification (§5). |
| `akerdock logout [--context NAME] [--revoke]` | Efface le credential local ; `--revoke` supprime aussi le token côté serveur. |
| `akerdock context list \| current \| use NAME \| remove NAME` | Multi-instances. |
| `akerdock ls [apps\|databases\|services\|previews\|servers]` | Listing ; défaut : liste transverse des ressources. |
| `akerdock logs REF [--component C] [-n LINES] [-f] [--deployment [UUID]]` | Logs conteneur (snapshot ou `-f` streaming) ou logs d'un déploiement. |
| `akerdock shell REF [--component C]` | Shell interactif dans le conteneur (§6). |
| `akerdock port-forward REF [LOCAL:]REMOTE [--component C]` | Tunnel TCP (§7). |
| `akerdock db REF [--component C]` | Confort : ouvre un forward et lance le client local du moteur détecté (§8). |

**Flags globaux.** `--context NAME` ; `-o table|json` (`json` = objets API bruts, pour le
scripting) ; `--quiet`. `NO_COLOR` respecté. **Codes de sortie** : `0` succès, `1` erreur,
`2` usage.

## 4. Contextes et stockage

`~/.akerdock/` (répertoire `0700`) :
- `config.yaml` (`0600`) — `current_context` + `contexts: {nom → {url, fqdn, team_uuid}}`.
- `credentials.yaml` (`0600`) — `{contexte → token}`, séparé pour inspecter/partager la
  config sans exposer les tokens.

Un contexte = une instance + une team active. `login` crée ou met à jour le contexte courant.
Le trousseau OS est un **DEVRAIT (v1.x)** (voir ADR-031 pour l'écart assumé).

## 5. Login (ADR-031)

Flux **poll + code de confirmation lié par PKCE** — aucun port ouvert, tout en sortie HTTPS.

1. La CLI génère `verifier` + `challenge = SHA-256(verifier)`, `POST /auth/cli/start
   {challenge, name}` → `{request_id, user_code, verify_url, interval, expires_in}`.
2. Affiche `user_code` + l'URL, ouvre le navigateur sur `/cli/authorize?request_id=…`
   (ou imprime si `--no-browser`).
3. Consentement navigateur : login (mot de passe/passkey/OIDC), team, permissions, **et
   confrontation du `user_code`** ; approbation → `POST /auth/cli/approve` (session + CSRF).
4. La CLI poll `POST /auth/cli/token {request_id, verifier}` → à l'approbation, token `akd_`
   (TTL 30 j, nom `cli — <user>@<host>`) écrit en `0600`.

**Exigences** (détail normatif dans ADR-031) : codes usage-unique et hashés ; `verifier`
jamais transmis au navigateur ; `SHA-256(verifier) == challenge` vérifié à l'échange ;
approbation POST+CSRF ; correspondance du `user_code` exigée ; permissions ⊆ session ;
défaut `read,write`, jamais `root`/`deploy`/`read:sensitive` par défaut ; tout audité.
Repli `--with-token` pour les machines sans navigateur.

## 6. Shell

Réemploi **intégral** des sessions terminal existantes (§5.7, §24.4, ADR-024) : `POST
/applications/{uuid}/terminal-sessions` (+ `component`) frappe un token d'attache à usage
unique, la CLI ouvre `wss://<manager>/terminal/ws?token=…&cols=&rows=`, met le TTY local en
mode raw et pont le flux binaire ↔ PTY, en transmettant les changements de taille de
fenêtre. Idle timeout, durée max, heartbeat et kill garanti s'appliquent inchangés. La CLI
**NE définit ni ne spécifie** de nouveau protocole ici.

## 7. Port-forward (ADR-032)

`akerdock port-forward db/varuna 15432:5432` établit un tunnel du `127.0.0.1:15432` **local
au processus CLI** (écoute loopback du CLI, hors de l'invariant §2 qui ne concerne que les
connexions réseau sortantes) vers le port `5432` du conteneur cible, via le manager.

- **Mint** : `POST /{applications|databases|services}/{uuid}/port-forwards` (+ previews),
  `x-required-permission: write`, corps `{port}`, cible figée et autorisée au mint, plafond
  `port_forward_limit` (défaut **10**) → `PortForwardSession{uuid, token akdp_, websocket_path
  "/tunnel/ws", expires_at}`.
- **Redeem** : `GET /tunnel/ws?token=akdp_…` (hors contrat), sous-protocole
  `akerdock-tunnel-v1` : une WS multiplexée par session, trames texte de contrôle
  (`open`/`open_ok`/`open_err`/`eof`/`close` par `id`) + trames binaires `[u32 id][charge]`.
- **Limites** : 32 streams/session, buffer 1 MiB/stream, idle 15 min, max 4 h, heartbeat
  20 s, teardown garanti, ouverture/fermeture auditées.
- **Frontière d'autorisation = la ressource, pas le port** : la cible est un conteneur
  autorisé au mint ; tout port de ce conteneur est atteignable (Docker ne filtre pas
  hôte→conteneur), au même titre que `shell` donne tout le conteneur. Énoncé, non masqué.
- **Serveurs exclus** : pas de `port-forward` au niveau serveur (= `ssh -L`).

## 8. Console typée (`akerdock db`)

Confort au-dessus du §7. `akerdock db
REF` détecte le moteur de la ressource (postgres / mysql / redis / mongo), ouvre un
port-forward éphémère et lance le client local correspondant (`psql`, `mysql`, `redis-cli`,
`mongosh`) préconfiguré avec les identifiants de la ressource. Si le client local est absent,
la CLI imprime la commande de connexion et laisse le forward ouvert. La CLI **NE stocke ni ne
relaie** de mot de passe en clair au-delà du lancement du processus enfant.

## 9. Sécurité (delta au threat-model)

- **T — interception loopback** : neutralisée par PKCE (le `verifier` ne quitte pas la CLI),
  cf. ADR-031.
- **S — phishing de la page de consentement** : la confrontation du `user_code` (généré par
  la CLI, affiché des deux côtés) casse le vecteur device-flow classique.
- **E — tunnel/shell vers un conteneur non autorisé** : autorisation au mint (`write` sur la
  ressource), team-scoping (INV-001), cible figée à la création de session.
- **Repudiation** : `start`/`approve`/`token`, ouverture/fermeture de shell et de
  port-forward, création et révocation de token — tous audités (§23.4).
- **Stockage** : token `akd_` au repos en `0600`, TTL 30 j, révocable (écart keychain assumé,
  ADR-031).

## 10. Audit et observabilité

Chaque action à effet distant est auditée avec acteur/token, IP, horodatage (§23.4) : login
(succès/échec), ouverture/fermeture de session terminal et de port-forward, révocation.
Les frappes du shell et les octets du tunnel **NE sont jamais** journalisés (§24.4).

## 11. Tests

Conformément à la pyramide de tests (ADR-026/028, plan de tests §2) : la logique déterministe
est prouvée en **tests unitaires/module** — parsing des `REF`, résolution de contexte, machine
d'état du login (start/poll/approve/échange, vérification PKCE), multiplexage du tunnel
(framing `open`/`eof`/`close`, cap de streams, buffer). Le shell et le port-forward de bout en
bout sont validés **manuellement** ponctuellement ; le parcours E2E produit unique
(Docker-in-Docker) n'est **pas** étendu pour la CLI (ADR-028).
