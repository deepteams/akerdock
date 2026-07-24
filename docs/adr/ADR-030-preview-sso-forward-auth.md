# ADR-030 — Protection des previews par authentification AkerDock (SSO)

- **Statut** : Accepté
- **Date** : 2026-07-24
- **Sections PRD liées** : §20.4.4 (protection des previews), §10.2 (authentification), INV-010
- **Révise** : rien — ajoute un mode de protection à côté de `basic_auth` et `none`

## Contexte

Le basic auth protège les previews mais son ergonomie est mauvaise (boîte de
dialogue navigateur, re-prompts quand l'application derrière émet ses propres
401, un seul mot de passe partagé par l'équipe) et son niveau de garantie est
inférieur à l'authentification du panel — qui sait déjà faire email/mot de
passe, passkeys, MFA et OIDC/Google. L'équipe qui review une PR est par
définition celle qui a accès au panel : la preview doit pouvoir déléguer son
contrôle d'accès à AkerDock.

## Décision

Nouveau mode `preview_protection: sso` :

1. **Traefik `forwardAuth`** sur chaque routeur HTTPS de la preview, vers
   `https://<fqdn-instance>/webhooks/previews/forward-auth` — le control
   plane décide requête par requête. Le mode exige un FQDN d'instance
   configuré ; sans lui le déploiement de la preview échoue avec la cause.
2. **Cookie d'accès par preview** : le forward-auth accepte un cookie
   `akerdock_preview` porté par le domaine de la preview — un token opaque
   dont seul le hash est stocké (`preview_access_tokens`), lié à LA preview,
   à l'utilisateur qui l'a obtenu, et expirant (12 h). Sa révocation suit la
   preview (cascade).
3. **Bootstrap par redirection** : sans cookie, le forward-auth redirige le
   navigateur vers `/webhooks/previews/authorize?redirect=<url>` sur le
   panel. Là, la SESSION AkerDock fait foi — quelle que soit la méthode de
   login (mot de passe, passkey, OIDC). L'accès est accordé si l'utilisateur
   appartient à la team de l'application (isolation INV-001) ; un token est
   émis, audité, et le navigateur est renvoyé sur
   `https://<host-preview>/.akerdock/preview-callback?token=…&next=<chemin>` —
   un **routeur dédié** du fichier de routage de la preview (priorité
   maximale, sans middleware d'auth) proxifie ce chemin côté serveur vers le
   control plane (`passHostHeader: false`), qui pose le cookie et redirige
   vers `next` (contraint à un chemin local). Le token voyage dans l'URL de
   la requête : les query strings survivent à tous les sauts de proxy, les
   en-têtes `X-Forwarded-*` non (purgés par les entrypoints intermédiaires
   comme non fiables) — c'est ce qui rend le flux robuste aux topologies en
   épingle (l'auth de l'instance repassant par son propre proxy).
   L'identité de la preview voyage de même dans l'**adresse** du middleware
   forward-auth (`?preview=<uuid>`), jamais déduite d'un `X-Forwarded-Host`.
4. Le host de la preview est résolu côté serveur (fqdn exact ou
   `<service>-<fqdn>` des stacks compose) — le paramètre `redirect` n'est
   jamais suivi vers un host qui n'est pas celui d'une preview connue
   (anti open redirect).

`basic_auth` reste le défaut : il fonctionne sans FQDN d'instance et pour des
invités hors team. `signed_link` reste réservé (valeur d'enum existante, non
implémentée).

## Alternatives considérées

- **Élargir le cookie de session au domaine parent** : simple mais élargit la
  surface du cookie du PANEL à tous les sous-domaines — dont les previews qui
  exécutent du code de PR. Rejeté : le cookie de preview est un jeton dédié,
  à portée et durée limitées, sans aucun pouvoir sur l'API.
- **Basic auth par utilisateur** : garde tous les défauts d'ergonomie.
- **OAuth2-proxy externe** : un composant de plus à opérer — contraire à
  ADR-025 (PostgreSQL seule dépendance).

## Service workers de l'application

Une PWA installe un service worker qui possède l'origine de la preview. Un
worker **correct** est transparent pour ce flux : une réponse redirigée
(`opaqueredirect`) transmise telle quelle à une navigation est suivie
nativement par le navigateur, et la danse de login traverse le worker sans
friction. Un worker qui met en cache ou retraite les réponses redirigées de
navigation se bloque, lui, derrière **n'importe quel** émetteur de 302
(OAuth, CDN, load balancer) — pas seulement ce flux ; le corriger relève de
l'application (règles : ne jamais mettre en cache une réponse
`redirected`/non-200 comme shell ; idéalement exclure `/.akerdock/**`).

**Décision négative documentée** : la plateforme n'envoie JAMAIS
`Clear-Site-Data` pour évincer un worker défaillant. Testé et rejeté :
Chrome le refuse sur les requêtes non credentialisées (manifest, no-cors),
et sur une navigation transitant par un worker il ordonne l'éviction du
worker en train de traiter la réponse — un blocage mesuré d'environ 10 s
par requête, pire que le mal.

## Conséquences

- Table `preview_access_tokens` (hash seul), deux routes navigateur hors
  bearer (`/webhooks/previews/*`), un lookup DB par requête de preview en
  mode sso — accepté : trafic de review, et le hash est indexé.
- L'application derrière peut renvoyer ses propres 401 sans déclencher de
  boîte de dialogue : plus aucun `WWW-Authenticate` côté proxy.
