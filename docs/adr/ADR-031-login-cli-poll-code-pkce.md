# ADR-031 — Authentification du CLI local : poll + code de confirmation + PKCE

- **Statut** : Accepté
- **Date** : 2026-07-25
- **Sections PRD liées** : §12 (CLI officielle), §10.2/§10.3 (auth, tokens), §23 (sécurité), §5.7 (exploitation)

## Contexte

Le PRD (§12) prévoit une CLI officielle. AkerDock sait déjà authentifier n'importe qui via
le panel (mot de passe, passkey, OIDC/Google), et une **session navigateur peut déjà créer
des tokens API** (`POST /teams/{uuid}/tokens`, garde anti-élévation). Mais il n'existe
**aucun flux d'authentification pour un client hors navigateur** : `/auth/oauth/*` est un
flux redirect+cookie strictement navigateur, qui produit une session, pas un token.

Deux contraintes structurantes de l'environnement cible pèsent sur le choix :

1. **Le client ne parle qu'au manager, sur 80/443, et n'ouvre aucun port.** Le poste peut
   être derrière un NAT, une session SSH distante, un conteneur, un pare-feu d'entreprise.
   Toute communication doit sortir en HTTPS vers le FQDN de l'instance et **traverser un
   proxy ou un load-balancer** intermédiaire.
2. **L'authentification doit fonctionner pour un utilisateur SSO/OIDC**, sans lui demander
   de contourner sa méthode de login habituelle.

## Décision

Le CLI s'authentifie par un flux **poll + code de confirmation lié par PKCE**, inspiré de
`gh auth login`. Il n'ouvre **aucun port** (que des requêtes sortantes), et le credential
final est un **token API `akd_` normal** — nommé, listé et révocable comme les autres.

### Séquence

1. Le CLI génère `verifier` (32 octets aléatoires, base64url) et
   `challenge = base64url(SHA-256(verifier))`, puis `POST /auth/cli/start {challenge, name}`
   (non authentifié, limiteur par IP). Réponse : `{request_id, user_code, verify_url,
   interval, expires_in}`. `user_code` = 6–8 caractères lisibles ; TTL **10 min (défaut
   proposé)**.
2. Le CLI **affiche** `user_code` et l'URL, puis ouvre le navigateur sur
   `https://<instance>/cli/authorize?request_id=…` (ou imprime l'URL si `--no-browser`).
3. Page de consentement du SPA : login si nécessaire (mot de passe/passkey/OIDC), sélecteur
   de team, permissions demandées affichées, **et le `user_code` que l'utilisateur confronte
   à celui de son terminal**. Approbation → `POST /auth/cli/approve {request_id, team_uuid,
   permissions}` — **session + double-submit CSRF** ; les permissions demandées **DOIVENT**
   être un sous-ensemble de celles de la session (garde anti-élévation existante).
4. Le CLI **poll** `POST /auth/cli/token {request_id, verifier}` (non authentifié, limiteur
   par IP, backoff `interval`) : `{status:"pending"}` tant que non approuvé ; une fois
   approuvé, claim atomique (usage unique) + vérification `SHA-256(verifier) == challenge` →
   token `akd_` frappé via le **chemin de création de token existant** avec l'Identity de la
   session comme créateur (l'invariant « un appelant ne peut accorder une permission qu'il ne
   détient pas » s'applique inchangé). Le token est écrit dans
   `~/.akerdock/credentials.yaml` (0600).

### Endpoints (hors contrat OpenAPI, montés à côté de `/auth`, comme `/terminal/ws`)

- `GET /cli/authorize` — route du SPA (page de consentement), pas un endpoint d'API.
- `POST /auth/cli/start` — non authentifié, limiteur IP. Crée la demande.
- `POST /auth/cli/approve` — **session + CSRF**. Lie la demande à un utilisateur/team et des
  permissions.
- `POST /auth/cli/token` — non authentifié, limiteur IP. Échange final.

### Exigences normatives

- Le `request_id` et les codes **DOIVENT** être à usage unique, à TTL court, et stockés
  **hashés** (SHA-256), jamais en clair (§23.2).
- Le `verifier` **NE DOIT JAMAIS** transiter par le navigateur : la possession du seul
  `request_id` (visible dans l'URL, l'historique, les logs de proxy) **NE DOIT PAS** suffire
  à obtenir un token — l'échange **DOIT** vérifier `SHA-256(verifier) == challenge`.
- L'approbation **DOIT** être un POST explicite depuis la page de consentement, protégé par
  la session **et** le double-submit CSRF ; jamais un effet de bord d'un GET.
- La page de consentement **DOIT** afficher le `user_code` et exiger que l'utilisateur
  confirme sa correspondance avec le terminal ; elle **DOIT** afficher les permissions et la
  team, et **DOIT** rendre inerte le `name` (chaîne contrôlée par le client).
- `root`, `deploy` et `read:sensitive` **NE DOIVENT PAS** être demandés par défaut ; le jeu
  par défaut est `read,write` (`write` est requis pour frapper les sessions terminal et les
  port-forwards). La page **PEUT** laisser retirer des permissions, jamais en ajouter.
- Le token frappé **DOIT** porter `expires_at` (TTL **30 jours, défaut proposé**) et un nom
  reconnaissable `cli — <user>@<host>`. Pas de refresh en v1 : un `login` (un passage
  navigateur) re-frappe.
- `start`, `approve`, `token` (succès **et** échec) et la création du token **DOIVENT** être
  audités (§23.4).
- Repli headless : `akerdock login --with-token` (coller un `akd_` existant) **DOIT** exister.

### Invariant de transport (posé ici, appliqué par toute la spec CLI)

- Le CLI **NE se connecte qu'au** FQDN du manager, **sur 443** (80 uniquement pour un
  éventuel redirect→HTTPS) ; aucune autre destination.
- Le CLI **n'ouvre aucun port** entrant ni loopback.
- `shell` et `port-forward` passent en `wss://<manager>/…` sur 443, avec les en-têtes Upgrade
  WebSocket standard (mêmes que le terminal, qui traverse déjà les proxies) ; le tunnel vers
  le serveur cible est fait **côté manager** (SSH), invisible du client.

### Stockage local

`~/.akerdock/` (répertoire `0700`) :
- `config.yaml` (`0600`) — contextes `{nom → {url, fqdn, team_uuid}}` + `current_context`.
- `credentials.yaml` (`0600`) — `{contexte → token}`, séparé pour que la config soit
  inspectable sans exposer les tokens.

Le trousseau natif de l'OS est un **DEVRAIT (v1.x)**. Écart assumé vis-à-vis du threat-model
(qui le pose en « défaut proposé ») : un trousseau cross-platform dans un binaire Go statique
(contrainte ADR-021, image distroless) est un coût de dépendance réel ; le risque est atténué
par le mode `0600`, le TTL 30 jours, la révocabilité et la visibilité de `last_used_at`.

## Alternatives considérées

- **Callback loopback (`127.0.0.1:<port>`, style gcloud/gh browser)** : rejeté — ouvre un
  port local (viole la contrainte réseau) et casse en SSH distant / conteneur / poste
  verrouillé.
- **OAuth Device Authorization Grant nu** (code tapé, pas de PKCE) : rejeté — phishable par
  construction (un attaquant envoie son `user_code` à la victime). Notre variante neutralise
  ce vecteur : c'est le CLI qui génère la demande et affiche le code à confronter.
- **CLI comme client OIDC (drive le flux OIDC lui-même)** : rejeté — ne marche pas pour les
  instances en mot de passe/passkey, et ferait du CLI un second relying party à configurer.
- **Token collé uniquement** : conservé comme repli `--with-token`, mais insuffisant comme
  défaut (mauvaise UX, pas de SSO intégré).

## Conséquences

- **Positives** : un seul type de credential (token `akd_`), aucun port ouvert, traversée
  proxy/LB native, SSO gratuit (l'approbation se fait dans le navigateur).
- **Négatives** : trois endpoints hors contrat de plus (spécifiés dans `docs/specs/cli.md`) ;
  un lookup DB par poll ; deux nouvelles tables techniques (`cli_authorization_codes`).
- **Risques acceptés** : token au repos dans un fichier `0600` plutôt qu'un trousseau OS en
  v1 (voir ci-dessus).
