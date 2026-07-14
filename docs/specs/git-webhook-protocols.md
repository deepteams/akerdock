# Spécification — Protocoles Git et webhooks par fournisseur

> Artefact §29.8 du PRD (`docs/PRD.md`). Le PRD est la source de vérité ; cette spécification précise, fournisseur par fournisseur, les protocoles d'intégration Git : réception et validation des webhooks (INV-009), politique fork/contributeur (INV-010), authentification vers les APIs, événements consommés et feedback riche des previews (§20.4.6–20.4.8). Lorsque le PRD est muet, la valeur retenue est marquée **(défaut proposé)**. Les noms de headers, d'événements et d'endpoints sont ceux des APIs réelles des fournisseurs ; les points incertains sont marqués **(à vérifier)**.
>
> Documents liés : `docs/specs/deployment-engine.md` (§3.4 coalescing, état `superseded`), `docs/specs/data-dictionary.md` (§7 : `git_sources`, `github_apps`, `repositories`, `webhook_endpoints`, `webhook_deliveries` ; §8.9 : `previews`).

---

## 1. Modèle commun de réception

### 1.1 URLs de réception (défaut proposé)

Tous les webhooks entrants passent par le port unique du control plane (§27.1), hors préfixe `/api/v1` car ils ne sont pas authentifiés par token Bearer mais par signature :

| Voie | URL | Authentification |
|---|---|---|
| GitHub App (niveau app, tous les repos de l'installation) | `POST /webhooks/github/apps/{github_app_uuid}` | `X-Hub-Signature-256` (secret de l'app) |
| Webhook manuel par application | `POST /webhooks/{provider}/{endpoint_uuid}` avec `provider ∈ github\|gitlab\|bitbucket\|gitea` | Signature/token du provider (secret de `webhook_endpoints`) |
| Deploy webhook générique (CI custom, §12) | `GET\|POST /api/v1/deploy` | Bearer, permission `deploy` (voir §7) |
| Callback du manifest flow GitHub | `GET /webhooks/github/manifest/callback` | `state` signé à usage unique (voir §2.1) |

L'`endpoint_uuid` (resp. `github_app_uuid`) est un UUID aléatoire non séquentiel (§19.2 PRD) : il identifie la cible sans la révéler et évite les URLs devinables. Un UUID inconnu répond `404` sans corps détaillé.

### 1.2 Pipeline de réception (§20.3)

Le traitement synchrone (avant réponse HTTP) est minimal — objectif < 500 ms (§16.4) :

```text
1. Limite de taille          → 413 si dépassée
2. IP allowlist (optionnelle) → 403 si hors plage
3. Résolution de l'endpoint   → 404 si uuid inconnu ou endpoint disabled
4. Vérification de signature  → 401 si absente/invalide (comparaison temps constant)
5. Parse JSON                 → 400 si non parsable
6. Persistance de la livraison (webhook_deliveries, status = received)
   + déduplication (provider, delivery_id)
7. Réponse 200 immédiate ; enqueue d'un job webhook.process
──────────────────────────── traitement asynchrone ────────────────────────────
8. Association exacte livraison → application (INV-009)
9. Politiques : auto-deploy, fork/contributeur (INV-010), [skip ci]/[skip cd], watch paths
10. Déclenchement : déploiement (coalescing §1.9) / preview / cleanup / commande
11. Mise à jour du statut : accepted | ignored (+ ignore_reason) | duplicate | failed
```

Détails normatifs :

- **Limite de taille** : corps refusé au-delà de **2 MiB** avec `413` **(défaut proposé)** — les payloads push/PR réels restent très en deçà (GitHub plafonne lui-même ses payloads à 25 MB et abandonne la livraison au-delà). La signature est vérifiée sur le **corps brut complet** ; la colonne `payload` de `webhook_deliveries` est ensuite persistée tronquée à **512 KiB** avec marqueur de troncature **(défaut proposé)**, jamais de secret dedans.
- **IP allowlist** : liste CIDR optionnelle par endpoint et/ou par instance **(défaut proposé : désactivée)**. Elle est un complément, jamais un substitut à la signature (les plages des providers changent ; GitHub publie les siennes via `GET /meta`, Atlassian les siennes pour Bitbucket Cloud).
- **Signature** : algorithme par provider (voir sections dédiées). La comparaison est en **temps constant** dans tous les cas (bibliothèque standard, jamais `==` sur chaînes). Une signature invalide est persistée (`signature_valid = false`, `status = failed`) pour l'audit (§23.4) puis répond `401` — elle ne déclenche **rien** (INV-009).
- **Horodatage** : aucun provider Git ne signe d'horodatage (contrairement à Stripe/Slack) ; la protection anti-rejeu repose donc entièrement sur la déduplication persistée `(provider, delivery_id)` (contrainte UNIQUE de `webhook_deliveries`). La rétention des livraisons (purge §22.2) borne la fenêtre de dédup ; une purge trop agressive rouvrirait la fenêtre de rejeu — rétention minimale **30 jours (défaut proposé)**.
- **Réponse** : `200` avec corps minimal `{"received": true}` dès que la livraison est persistée, y compris si le traitement asynchrone l'ignorera ensuite. Motif : GitLab désactive automatiquement un webhook qui échoue en rafale, GitHub marque le hook en erreur ; les décisions métier (skip, fork, watch paths) ne sont pas des erreurs de livraison.

### 1.3 Codes de réponse

| Code | Cas | Corps |
|---|---|---|
| `200` | Livraison persistée (traitée, ignorée ou dupliquée ensuite) | `{"received": true}` |
| `400` | JSON non parsable, payload tronqué par le réseau (échoue de toute façon la signature), event obligatoire absent | générique |
| `401` | Signature/token absent ou invalide | générique, sans indice sur le secret |
| `403` | IP hors allowlist | générique |
| `404` | `endpoint_uuid`/`github_app_uuid` inconnu ou désactivé | générique |
| `413` | Corps > limite de taille | générique |

Aucun corps d'erreur ne distingue « secret faux » de « secret absent », ni « endpoint inexistant » de « endpoint d'une autre team » (INV-002).

### 1.4 Déduplication

- Clé : `(provider, delivery_id)` — `X-GitHub-Delivery`, `X-Gitlab-Event-UUID`, `X-Request-UUID` (Bitbucket), `X-Gitea-Delivery` ; UUID généré côté AkerDock pour `generic`.
- Une livraison en doublon est persistée avec `status = duplicate` **(défaut proposé : ligne rejetée par la contrainte UNIQUE, comptée en métrique et loguée avec référence à l'originale, sans seconde ligne)** et répond `200`. Elle ne déclenche jamais un second déploiement (INV-009).
- Les **redeliveries manuelles** (bouton « Redeliver » chez GitHub) conservent le même GUID de livraison **(à vérifier)** : elles sont donc absorbées par la dédup. Les retries automatiques de Bitbucket incrémentent `X-Attempt-Number` en conservant le même `X-Request-UUID` **(à vérifier)** : absorbés aussi.

### 1.5 Association exacte livraison → ressource (INV-009)

L'association se fait **par identifiant, jamais par nom, jamais par préfixe** (§23.5 : scénario « repo au nom préfixe ») :

| Voie | Clés d'association |
|---|---|
| GitHub App | `github_app_uuid` de l'URL → `github_apps` ; `installation.id` du payload = `github_apps.installation_id` ; `repository.id` = `repositories.external_id` → applications liées via `applications.repository_id` |
| Webhook manuel | `endpoint_uuid` de l'URL → `webhook_endpoints.application_id` (une application exactement) ; contrôle de cohérence : le repo du payload correspond au repo configuré de l'application (`ignored`/`failed` sinon, jamais de déploiement « au plus proche ») |
| Générique | UUIDs passés explicitement, résolus dans la team du token |

Puis filtrage par **branche** (`ref == refs/heads/<git_branch>` pour un push) ou par **PR/MR** (previews). La `team_id` de la livraison est celle de la ressource associée ; une livraison qui ne résout aucune ressource est `ignored` (`ignore_reason = no_target`) **(défaut proposé)**. Il est impossible qu'une livraison déclenche une ressource d'une autre team : la chaîne `github_app`/`webhook_endpoint` → application porte l'ownership (INV-001/INV-002).

Si plusieurs applications de la même team suivent le même repo/branche (cas légitime, ex. monorepo), la livraison est associée à **chacune** et chaque application applique ses propres politiques (watch paths notamment) ; `webhook_deliveries.application_id` référence la première et le détail par application est porté par les événements/jobs **(défaut proposé)**.

### 1.6 Politique fork et contributeur (INV-010)

Évaluée pour tout événement PR/MR, dans cet ordre :

1. **Détection fork** : le repo source de la PR diffère du repo cible (comparaison par **ID de repo**, pas par nom). `previews.is_fork = true`.
2. **PR de fork, `preview_fork_approval_enabled = false`** : ignorée (`ignore_reason = fork_untrusted`). Aucun build, aucun secret, aucun commentaire.
3. **PR de fork, approbation activée (§20.4.8)** : ignorée tant qu'un mainteneur autorisé n'a pas approuvé (voir §2.7). Après approbation : build sur **builder isolé** (décision §27.5), **aucune variable marquée secrète injectée** — y compris celles du jeu preview ; seules les variables preview non sensibles sont fournies **(défaut proposé)**.
4. **PR interne, `preview_public_prs_enabled = false`** (défaut) : seuls les auteurs membres/collaborateurs/contributeurs du repo déclenchent (§5.6). La qualité d'auteur est vérifiée **côté serveur via l'API du provider** quand des droits sont nécessaires (commandes, approbation) ; le champ déclaratif du payload (`author_association` GitHub) ne suffit que pour le filtre de parité **(défaut proposé)**.
5. **PR interne, PR publiques activées** : toute PR du repo déclenche.

### 1.7 Marqueurs `[skip ci]` / `[skip cd]` (§5.5)

- Cherchés dans le **message du commit de tête** de la livraison (`head_commit.message` pour un push ; message du commit head de la PR pour un synchronize) **(défaut proposé)**.
- Comparaison insensible à la casse, marqueurs exacts `[skip ci]` et `[skip cd]` **(défaut proposé)** ; pas d'alias `[ci skip]` en v1 **(défaut proposé)**.
- Effet : `ignored` (`ignore_reason = skip_ci`). Le deploy webhook générique et le déploiement manuel restent utilisables (le marqueur ne bloque que l'auto-deploy).

### 1.8 Watch paths (§5.5)

- Patterns glob (syntaxe doublestar `**` **(défaut proposé)**), un par ligne (`applications.watch_paths`), évalués contre l'union des fichiers `added ∪ modified ∪ removed` de tous les commits de la livraison.
- Les payloads push **plafonnent la liste des commits à 20** chez GitHub et GitLab : si `total_commits_count > 20`, ou en cas de **force push** (le chaînage `before → after` ne couvre pas la livraison), le worker interroge l'API de comparaison du provider (`GET /repos/{owner}/{repo}/compare/{before}...{after}` GitHub, `GET /projects/:id/repository/compare` GitLab) pour obtenir la liste complète **(défaut proposé)** ; à défaut d'API disponible, la livraison est traitée comme « match » (fail-open : mieux vaut un déploiement de trop qu'un déploiement manquant) **(défaut proposé)**.
- Les watch paths s'appliquent **aussi aux previews** (§20.4.5, §15) — la liste des fichiers d'une PR est obtenue via l'API de diff (`GET /repos/{owner}/{repo}/compare/{base}...{head}` GitHub, `GET /projects/:id/merge_requests/:iid/diffs` GitLab, diffstat Bitbucket, `GET /repos/{owner}/{repo}/pulls/{index}/files` Gitea).
- Aucun match → `ignored` (`ignore_reason = watch_paths`).

### 1.9 Coalescing et état `superseded`

Le coalescing est spécifié dans `deployment-engine.md` §3.4 ; rappel du contrat côté webhook :

- À l'enqueue d'un déploiement webhook pour `(application, branche)`, un déploiement encore `queued` (job non `leased`) issu d'un webhook avec un SHA plus ancien est marqué `superseded` (terminal, assimilé à `cancelled`, `superseded_by` renseigné).
- La livraison d'origine du déploiement remplacé reste `accepted` et pointe vers le déploiement qui l'a remplacée — la traçabilité `webhook_delivery_id` n'est jamais perdue.
- Un déploiement `leased`/en cours n'est **jamais** coalescé.
- Pour les previews, si `preview_cancel_obsolete_builds = true` (§20.4.7), un build de preview **en cours** rendu obsolète par un nouveau commit de la même PR est annulé (annulation coopérative) — c'est l'extension opt-in du coalescing au-delà de l'état `queued`.

### 1.10 Ordonnancement des livraisons

Les providers ne garantissent pas l'ordre. Garde-fou **(défaut proposé)** : une livraison push dont le `after` est déjà connu comme `before` d'une livraison **acceptée plus récemment** pour la même `(application, branche)` est ignorée (`ignore_reason = out_of_order`). Combiné au coalescing et à la sérialisation par verrou applicatif (deployment-engine §3.1), le pire cas résiduel est un déploiement intermédiaire superflu, jamais une régression silencieuse du SHA déployé après un déploiement plus récent réussi — le SHA déployé est toujours le `after` de sa livraison, résolu à l'enqueue, jamais re-résolu.

### 1.11 Observabilité

Chaque livraison produit : entrée d'audit (§23.4 : appels webhook), métriques OTLP (compteurs par provider × statut × ignore_reason, latence réception → `2xx`, latence réception → fin de traitement), et le `webhook_delivery_id` est propagé comme corrélation jusqu'au déploiement, ses logs et ses notifications (deployment-engine §9).

---

## 2. GitHub — GitHub App (voie recommandée)

Une GitHub App par team (table `github_apps`), créée par manifest flow, portant : le webhook au niveau app (un seul endpoint pour tous les repos installés), l'authentification API (JWT → installation token) et les permissions fines. C'est la seule voie offrant le feedback riche complet (§20.4.6) : Checks, Deployments, commentaire upserté, commandes.

### 2.1 Création par manifest flow (aller-retour complet)

Le manifest flow évite toute saisie manuelle (app ID, clé privée, secret) : l'instance génère l'App, GitHub renvoie les credentials.

1. **Initiation (dashboard)** : l'utilisateur choisit la team, le compte cible (personnel ou organisation) et, pour GitHub Enterprise Server, l'URL de base. AkerDock crée la ligne `github_apps` en **brouillon** (uuid généré, credentials NULL) et un jeton `state` signé, à usage unique, expirant après **10 minutes (défaut proposé)** — anti-CSRF du callback.
2. **Soumission du manifest** : le dashboard rend un formulaire auto-soumis en `POST` vers `https://github.com/settings/apps/new?state={state}` (compte personnel) ou `https://github.com/organizations/{org}/settings/apps/new?state={state}` (organisation) — sur GHES, même chemin sur le `html_url` de l'instance. Champ de formulaire unique `manifest` contenant le JSON :

```json
{
  "name": "akerdock-<instance>-<suffixe>",
  "url": "https://<fqdn-instance>",
  "hook_attributes": { "url": "https://<fqdn-instance>/webhooks/github/apps/<uuid>", "active": true },
  "redirect_url": "https://<fqdn-instance>/webhooks/github/manifest/callback",
  "setup_url": "https://<fqdn-instance>/…/github-apps/<uuid>/setup",
  "public": false,
  "default_events": ["push", "pull_request", "installation_repositories", "issue_comment"],
  "default_permissions": {
    "contents": "read",
    "metadata": "read",
    "pull_requests": "write",
    "checks": "write",
    "deployments": "write",
    "issues": "read"
  }
}
```

   (`issues: read` uniquement pour recevoir `issue_comment` — voir §2.3 ; l'événement `installation` est envoyé d'office à toute App, sans souscription explicite **(à vérifier)**.)
3. **Confirmation chez GitHub** : l'utilisateur voit la page de création pré-remplie, peut ajuster le nom (unicité globale des noms d'App), et valide.
4. **Callback** : GitHub redirige vers `redirect_url?code={code}&state={state}`. AkerDock vérifie le `state` (signature, expiration, usage unique, correspondance avec le brouillon).
5. **Conversion** : `POST {api_url}/app-manifests/{code}/conversions` — **sans authentification**, le `code` est à usage unique et expire après **une heure** (côté GitHub). Réponse : `id` (app ID), `slug`, `client_id`, `client_secret`, `webhook_secret`, `pem` (clé privée RSA), `html_url`. AkerDock persiste : `app_id`, `client_id`, et chiffre enveloppe `client_secret_enc`, `webhook_secret_enc`, `app_private_key_enc` (§23.2). La réponse de conversion n'est jamais loguée (INV-003).
6. **Installation** : AkerDock redirige l'utilisateur vers `https://github.com/apps/{slug}/installations/new` ; l'utilisateur choisit le compte et le périmètre (« All repositories » ou sélection).
7. **Retour** : deux signaux redondants — le webhook `installation` (`action = created`, `installation.id`, liste des repos) reçu sur l'endpoint de l'app, et la redirection navigateur vers `setup_url?installation_id={id}&setup_action=install`. Le premier des deux renseigne `github_apps.installation_id` ; l'UI confirme.
8. **Discovery** : `GET /installation/repositories` (paginée, token d'installation) alimente le cache `repositories` (`external_id` = ID GitHub du repo, stable au renommage).

Contrainte de schéma : `github_apps.installation_id` est scalaire — **une installation par enregistrement d'App**. Installer la même App sur un second compte/organisation nécessite un second enregistrement (nouveau manifest flow) **(défaut proposé, aligné sur le data dictionary)**.

### 2.2 Authentification : JWT App → installation access token

Deux niveaux, conformes au modèle GitHub Apps :

1. **JWT d'App** — signé **RS256** avec la clé privée PEM. Claims : `iss` = `app_id` (le `client_id` est aussi accepté par GitHub), `iat` = maintenant − 60 s (tolérance d'horloge), `exp` ≤ `iat` + 10 min (maximum GitHub) — retenu : **`iat` + 9 min (défaut proposé)**. Utilisé uniquement contre les endpoints `/app/*`.
2. **Installation access token** — `POST {api_url}/app/installations/{installation_id}/access_tokens` avec `Authorization: Bearer <jwt>`, `Accept: application/vnd.github+json`, `X-GitHub-Api-Version: 2022-11-28`. Réponse : token `ghs_…`, `expires_at` = **+1 heure**. Le corps de la requête PEUT restreindre le token (`repositories`, `permissions`) : AkerDock demande un token **restreint au(x) repo(s) cible(s)** de l'opération quand il les connaît **(défaut proposé)** — moindre privilège si le token fuit dans un log de build.
3. **Cache et renouvellement** : cache mémoire par `(installation_id, périmètre)` ; renouvellement anticipé quand il reste **< 5 minutes** de validité **(défaut proposé)** ; un `401` sur un appel API invalide l'entrée de cache et force un renouvellement (une seule fois, puis erreur classifiée). Les tokens d'installation ne sont **jamais persistés** en base ni écrits dans un log (INV-003).
4. **Clone Git** : `https://x-access-token:{installation_token}@github.com/{owner}/{repo}.git` (ou le host GHES). Le token est passé au processus git sans apparaître dans la ligne de commande persistée (INV-012, deployment-engine).

### 2.3 Permissions minimales demandées

| Permission | Niveau | Justification |
|---|---|---|
| `contents` | read | Clone du code (via `x-access-token`), lecture des commits et API compare (`GET /repos/{o}/{r}/compare/{base}...{head}`) pour watch paths et payloads tronqués. Jamais `write` : AkerDock ne pousse rien. |
| `metadata` | read | Permission plancher obligatoire de toute App ; discovery des repos (`GET /installation/repositories`), résolution branches/SHA. |
| `pull_requests` | write | Commentaire unique upserté sur la PR (`POST/PATCH` issue comments d'une PR — couvert par cette permission pour les PRs), lecture des PRs et de leurs fichiers, réactions d'accusé de réception des commandes. |
| `checks` | write | Checks API (`POST/PATCH /repos/{o}/{r}/check-runs`) — réservée aux Apps ; statut de preview utilisable comme condition de merge (§20.4.6). |
| `deployments` | write | Deployments API (`POST /repos/{o}/{r}/deployments` + statuses) — bouton « View deployment » sur la PR. |
| `issues` | read | Uniquement pour recevoir l'événement `issue_comment` (commandes `/deploy`, `/destroy`) ; la souscription à cet événement exige la permission Issues ou Pull requests **(à vérifier — si Pull requests suffit, retirer `issues` du manifest)**. |

Aucune permission organisation, aucun accès `secrets`, `actions`, `administration`. Toute élévation ultérieure passe par `new_permissions_accepted` (événement `installation`) et une action explicite de l'utilisateur GitHub.

### 2.4 Événements webhook consommés

Souscrits dans le manifest ; tout autre `X-GitHub-Event` reçu est persisté puis `ignored` (`ignore_reason = event_not_handled`) **(défaut proposé)**.

| `X-GitHub-Event` | Actions traitées | Effet |
|---|---|---|
| `push` | — | Auto-deploy de la branche suivie (pipeline §1.2) ; `ref`, `before`, `after`, `commits[]` (plafonné à 20 — §1.8), `head_commit` |
| `pull_request` | `opened`, `synchronize`, `reopened` | Création/redeploy de preview (§20.4) ; head SHA = `pull_request.head.sha` ; fork si `pull_request.head.repo.id ≠ pull_request.base.repo.id` |
| `pull_request` | `closed` | Cleanup de la preview (merge ou fermeture — `pull_request.merged` distingue) ; annulation des builds de preview en cours pour cette PR |
| `pull_request` | `ready_for_review`, `converted_to_draft` | Si `preview_exclude_drafts` : sortie de draft → deploy ; passage en draft → pas de nouveau deploy **(défaut proposé : la preview existante n'est pas détruite)** |
| `pull_request` | `labeled`, `unlabeled` | Si `preview_require_label` : label ajouté → deploy, retiré → destruction de la preview **(défaut proposé)** ; sert aussi à l'approbation de fork par label (§2.7) |
| `issue_comment` | `created` | Commandes `/deploy`, `/destroy` (si activées) et approbation de fork par commentaire ; uniquement si `issue.pull_request` est présent |
| `installation` | `created`, `deleted`, `suspend`, `unsuspend`, `new_permissions_accepted` | Cycle de vie : renseigne/invalide `installation_id` ; `deleted`/`suspend` → source marquée dégradée + notification (§11) |
| `installation_repositories` | `added`, `removed` | Resynchronisation du cache `repositories` ; un repo retiré casse l'association des applications liées → notification |

Les actions `edited`, `assigned`, `review_requested`, etc. sont ignorées sans traitement.

### 2.5 Signature `X-Hub-Signature-256`

- Header `X-Hub-Signature-256: sha256=<hex>` — HMAC-SHA256 du **corps brut** avec le `webhook_secret` de l'App ; comparaison en temps constant. Le header legacy `X-Hub-Signature` (SHA-1) est ignoré.
- Autres headers exploités : `X-GitHub-Delivery` (GUID → `delivery_id`), `X-GitHub-Event`, `X-GitHub-Hook-Installation-Target-ID` (doit correspondre à `app_id` — contrôle de cohérence supplémentaire **(défaut proposé)**), `Content-Type: application/json` (le mode `application/x-www-form-urlencoded` n'est pas accepté pour l'App — le manifest configure JSON).
- Secret absent en base (App brouillon) → `401` systématique.

### 2.6 GitHub Enterprise Server

- `github_apps.api_url` (ex. `https://ghe.example.com/api/v3`) et `html_url` remplacent les défauts github.com ; manifest flow, conversion, JWT, tokens d'installation et webhooks fonctionnent à l'identique sur GHES (versions minimales supportées **(à vérifier)**).
- Les URLs d'API construites le sont **toujours** depuis `api_url` — jamais de `api.github.com` en dur ; policy SSRF (§23.3) appliquée à `api_url`/`html_url` à l'enregistrement.

### 2.7 Feedback riche des previews (§20.4.6–20.4.8)

Principe transversal : le feedback est **best-effort** — un échec d'appel de feedback (check, deployment, commentaire) est logué, retenté avec backoff, notifié s'il persiste, mais **n'échoue jamais le déploiement** de la preview **(défaut proposé)**.

**a) Checks API** — un check run par preview et par SHA :

- Création dès l'acceptation de la livraison : `POST /repos/{owner}/{repo}/check-runs` avec `name: "AkerDock / preview / <application_name>"` **(défaut proposé)**, `head_sha`, `status: "queued"`, `details_url` (page du déploiement dans le dashboard), `external_id` = `deployment_uuid`.
- Transitions : `PATCH /repos/{owner}/{repo}/check-runs/{check_run_id}` — `status: "in_progress"` au début du build, puis `status: "completed"` avec `conclusion: "success"` (avec `output.summary` contenant l'URL de preview) ou `"failure"` ; build annulé/supersédé → `conclusion: "cancelled"` ; livraison ignorée par politique → `conclusion: "skipped"` **(défaut proposé)**.
- Le check est utilisable comme required status check de branch protection (condition de merge, §20.4.6).

**b) Deployments API** — matérialise « View deployment » sur la PR :

- `POST /repos/{owner}/{repo}/deployments` avec `ref` = head SHA, `environment: "preview/pr-<pr_id>"` **(défaut proposé)**, `transient_environment: true`, `production_environment: false`, `auto_merge: false`, `required_contexts: []` (sinon GitHub refuse le deployment si des checks sont pendants — y compris le nôtre).
- Statuts : `POST /repos/{owner}/{repo}/deployments/{deployment_id}/statuses` avec `state` ∈ `in_progress` → `success` (+ `environment_url` = URL de la preview, `log_url` = page AkerDock) ou `failure` ; à la destruction de la preview : `state: "inactive"` (retire « View deployment »).

**c) Commentaire unique upserté** — jamais un commentaire par déploiement :

- Marqueur HTML invisible en tête du corps : `<!-- AkerDock:preview:<application_uuid>:<pr_id> -->` **(défaut proposé)**.
- Upsert : `GET /repos/{owner}/{repo}/issues/{pr_number}/comments` (paginée), recherche du marqueur ; trouvé → `PATCH /repos/{owner}/{repo}/issues/comments/{comment_id}`, sinon → `POST /repos/{owner}/{repo}/issues/{pr_number}/comments`. L'ID du commentaire est mémorisé dans le payload des jobs/événements (data dictionary §8.9, pas de colonne dédiée) pour éviter la relecture ; la recherche par marqueur reste le fallback si l'ID mémorisé a disparu (commentaire supprimé à la main).
- Contenu : statut courant, URL de preview (avec mention de la protection d'accès §20.4.4), SHA déployé, horodatage, lien vers les logs. Une application ayant plusieurs previews actives sur le même repo garde un commentaire par PR, pas par déploiement.

**d) Commandes en commentaire (opt-in, `preview_comment_commands_enabled`)** :

- Événement `issue_comment` / `created`, sur une PR uniquement (`issue.pull_request` présent), corps dont la **première ligne** est exactement `/deploy` ou `/destroy` (trim, insensible à la casse) **(défaut proposé)**.
- **Vérification des droits de l'auteur, côté serveur** : `GET /repos/{owner}/{repo}/collaborators/{username}/permission` — requiert `permission ∈ {admin, maintain, write}` **(défaut proposé)**. Le champ `comment.author_association` du payload n'est jamais suffisant (déclaratif, et `CONTRIBUTOR` couvre n'importe quel auteur déjà mergé une fois).
- `/deploy` : (re)déploie la preview au head SHA courant — y compris pour une PR de fork **si et seulement si** l'auteur de la commande est un mainteneur autorisé (la commande vaut approbation, voir e). `/destroy` : détruit la preview (cycle `destroying` §8.9).
- Accusé de réception : réaction `rocket` sur le commentaire de commande (`POST /repos/{owner}/{repo}/issues/comments/{comment_id}/reactions`, `content: "rocket"`) **(défaut proposé)** ; refus de droits → aucune réaction, événement audité.
- Chaque commande est une livraison webhook normale : dédupliquée, auditée, tracée.

**e) Approbation des forks (opt-in, `preview_fork_approval_enabled`, §20.4.8)** — trois voies équivalentes :

1. **Label** : un mainteneur autorisé (même vérification de droits que d) appose le label configuré — défaut `AkerDock/approved` **(défaut proposé)** ; l'événement `pull_request`/`labeled` porte `sender` = l'utilisateur qui a labellisé, dont les droits sont vérifiés via l'API.
2. **Commentaire** : `/deploy` d'un mainteneur (voie d).
3. **Dashboard** : approbation UI par un utilisateur AkerDock autorisé → `previews.fork_approved_by`/`fork_approved_at`. Pour les voies 1–2, `fork_approved_by` reste NULL (l'approbateur n'est pas un utilisateur AkerDock) et l'identité GitHub est conservée dans l'audit et le payload d'événement **(défaut proposé)**.

- **L'approbation vaut pour le SHA approuvé uniquement** : tout nouveau push sur la PR de fork invalide l'approbation et repasse la preview en attente d'approbation **(défaut proposé)** — sinon un attaquant pousse du code sûr, obtient l'approbation, puis pousse le payload malveillant (INV-010).
- Même approuvée : builder isolé, aucun secret injecté (§1.6).

---

## 3. GitHub — Deploy key + webhook manuel

Voie sans GitHub App (parité §5.1) : accès au code par clé SSH, événements par webhook de repo configuré à la main.

### 3.1 Deploy key

- AkerDock génère une paire **ed25519** **(défaut proposé)** dans `private_keys` (chiffrée, scopée team) ou importe une clé existante ; la clé publique est affichée avec un bouton copier.
- L'utilisateur la dépose dans le repo : Settings → Deploy keys → Add deploy key, **sans** « Allow write access » (lecture seule — AkerDock ne pousse jamais).
- Clone : `git@github.com:{owner}/{repo}.git` avec la clé (host key vérifiée selon la politique SSH de l'instance). Une deploy key GitHub est **mono-repo** : une clé partagée entre plusieurs repos sera refusée par GitHub au dépôt (clé déjà utilisée) — générer une clé par repo.

### 3.2 Webhook manuel de repo

- AkerDock crée le `webhook_endpoints` (provider `github`) avec secret généré aléatoirement (256 bits **(défaut proposé)**), affiche l'URL `https://<fqdn>/webhooks/github/{endpoint_uuid}` et le secret à copier.
- Configuration côté GitHub : repo Settings → Webhooks → Add webhook — Payload URL, `Content type: application/json`, Secret, événements : `push` + `pull_requests` (previews) + `issue_comment` (commandes, si activées).
- Validation identique à l'App : `X-Hub-Signature-256` (HMAC-SHA256, temps constant), dédup par `X-GitHub-Delivery`.
- L'événement `ping` (envoyé à la création du hook) est persisté et répond `200` sans autre effet (`ignore_reason = ping`) **(défaut proposé)**.

### 3.3 Différences de capacités

- **Pas de Checks API ni de Deployments API** : ces APIs exigent une authentification d'App (checks) ou un token utilisateur (deployments) que cette voie n'a pas.
- **Feedback dégradé optionnel** : si l'utilisateur fournit un token (PAT fine-grained « Commit statuses: write » — ou classique scope `repo:status`) **(défaut proposé : champ token optionnel sur la git source)**, AkerDock publie des **commit statuses** : `POST /repos/{owner}/{repo}/statuses/{sha}` avec `state` ∈ `pending`/`success`/`failure`/`error`, `context: "AkerDock/preview"` **(défaut proposé)**, `target_url`. Avec un PAT à portée plus large (`Pull requests: write`), le commentaire upserté redevient possible — même mécanique qu'en §2.7c. **Sans token : aucun feedback** — la preview fonctionne (URL, cleanup) mais la PR n'affiche rien.
- Vérification des droits d'un auteur de commande impossible sans token → les commandes en commentaire exigent un token configuré, sinon elles sont refusées (`ignore_reason = no_api_credentials`) **(défaut proposé)**.
- Discovery des repos indisponible : l'utilisateur saisit l'URL du repo ; `repositories` n'est pas alimenté, l'association passe par le `webhook_endpoints` de l'application (§1.5) avec contrôle de cohérence sur `repository.full_name` (comparaison **exacte**, insensible à la casse **(défaut proposé)** — jamais par préfixe).

---

## 4. GitLab

### 4.1 Intégration

- **Accès API** : token d'accès (project/group/personal access token, scope `api`) ou OAuth **(défaut proposé : token d'accès en v1, OAuth pour le login dashboard seulement)** ; stocké chiffré sur la git source. **Accès Git** : deploy key SSH (comme §3.1 — GitLab accepte la même clé sur plusieurs projets) ou clone HTTPS avec le token.
- **GitLab self-hosted** : `git_sources.api_url` (ex. `https://gitlab.example.com/api/v4`) et `html_url` configurables ; policy SSRF (§23.3).

### 4.2 Webhooks

- Configuration : projet → Settings → Webhooks — URL `https://<fqdn>/webhooks/gitlab/{endpoint_uuid}`, « Secret token », événements cochés : **Push events**, **Merge request events**, **Comments** (Note Hook, si commandes activées).
- **Authentification** : GitLab n'envoie **pas de HMAC** — le header `X-Gitlab-Token` contient le secret **en clair**, comparé en temps constant au secret déchiffré de l'endpoint. (TLS obligatoire de fait ; c'est le modèle GitLab, pas un choix AkerDock.)
- Headers : `X-Gitlab-Event` (`Push Hook`, `Merge Request Hook`, `Note Hook`), `X-Gitlab-Event-UUID` (→ `delivery_id` ; **(à vérifier)** : conservé à l'identique lors des retries automatiques — sinon la dédup dégénère en no-op et le garde-fou §1.10 prend le relais), `X-Gitlab-Instance`.
- GitLab **désactive automatiquement** un webhook en échec répété (backoff puis désactivation) : raison de plus pour répondre `200` aux livraisons ignorées (§1.2).

### 4.3 Événements

| `X-Gitlab-Event` | Champs clés | Effet |
|---|---|---|
| `Push Hook` (`object_kind: push`) | `ref`, `before`, `after`, `checkout_sha`, `commits[]` (plafonné à 20, `total_commits_count`) | Auto-deploy ; watch paths via `commits[].added/modified/removed`, fallback API compare (§1.8) |
| `Merge Request Hook` (`object_kind: merge_request`) | `object_attributes.action` ∈ `open`, `update`, `reopen`, `close`, `merge` ; `object_attributes.last_commit.id` (head SHA) ; `object_attributes.oldrev` ; `source_project_id` / `target_project_id` ; `work_in_progress`/`draft` | Previews MR (§5.6) : `open`/`reopen` → deploy ; `update` **avec `oldrev` présent** → redeploy (un `update` sans `oldrev` est un changement de titre/labels, ignoré) ; `close`/`merge` → cleanup |
| `Note Hook` (`object_kind: note`) | `object_attributes.noteable_type == "MergeRequest"`, `object_attributes.note`, `merge_request` | Commandes `/deploy`, `/destroy` et approbation de fork |

- **Fork** : `object_attributes.source_project_id ≠ object_attributes.target_project_id` (comparaison par ID).
- **Droits d'un auteur de commande** : `GET /projects/:id/members/all/:user_id` — `access_level ≥ 30` (Developer) **(défaut proposé)**.

### 4.4 Feedback (parité §20.4.6 : commit statuses + note upsertée)

- **Commit statuses** : `POST /projects/:id/statuses/:sha` avec `state` ∈ `pending`, `running`, `success`, `failed`, `canceled`, `name: "AkerDock/preview"` **(défaut proposé)**, `target_url`. Affiché dans la MR et utilisable dans les règles de merge (pipelines must succeed s'applique aux statuses externes **(à vérifier)**).
- **Note upsertée** sur la MR : marqueur invisible identique (§2.7c) ; `GET /projects/:id/merge_requests/:iid/notes` puis `PUT /projects/:id/merge_requests/:iid/notes/:note_id`, sinon `POST /projects/:id/merge_requests/:iid/notes`.
- Équivalent Deployments API : l'API Environments/Deployments GitLab existe mais n'est **pas utilisée en v1** (dégradé volontaire, réévaluable) **(défaut proposé)**.

---

## 5. Bitbucket (Cloud)

### 5.1 Webhook + secret

- Configuration : Repository settings → Webhooks — URL `https://<fqdn>/webhooks/bitbucket/{endpoint_uuid}`, **Secret** (support natif des « secure webhooks » Bitbucket Cloud).
- **Signature** : header `X-Hub-Signature` contenant `sha256=<hex>` — HMAC-SHA256 du corps brut avec le secret **(à vérifier : Bitbucket Cloud réutilise le nom de header historique `X-Hub-Signature` avec un HMAC-SHA256, sans variante `-256`)** ; comparaison temps constant. Si le webhook a été créé **sans** secret côté Bitbucket, aucune signature n'est envoyée : AkerDock refuse (`401`) — le mode sans secret n'est pas supporté, l'IP allowlist Atlassian n'étant qu'un complément (§1.2).
- Headers : `X-Event-Key`, `X-Hook-UUID` (ID de configuration du hook), `X-Request-UUID` (→ `delivery_id`), `X-Attempt-Number` (retries).

### 5.2 Événements

| `X-Event-Key` | Effet |
|---|---|
| `repo:push` | Auto-deploy : `push.changes[]` avec `new`/`old` (branche, `target.hash`) ; **pas de liste de fichiers par commit** dans le payload → watch paths via l'API diffstat (voir §5.3) |
| `pullrequest:created` | Preview : deploy |
| `pullrequest:updated` | Preview : redeploy **si le head a changé** (`pullrequest.source.commit.hash` différent du dernier déployé — Bitbucket émet aussi `updated` pour les éditions de description) **(défaut proposé)** |
| `pullrequest:fulfilled` | Merge → cleanup |
| `pullrequest:rejected` | Déclinée → cleanup |
| `pullrequest:comment_created` | Commandes `/deploy`, `/destroy` et approbation de fork |

- **Fork** : `pullrequest.source.repository.uuid ≠ pullrequest.destination.repository.uuid`.
- **Attention SHA court** : les payloads Bitbucket portent des hashes tronqués (12 caractères) à certains endroits — le SHA complet est résolu via `GET /2.0/repositories/{workspace}/{repo_slug}/commit/{hash}` avant enqueue **(défaut proposé)**.

### 5.3 Accès API et feedback

- Auth API : API token / app password (scopes `repository`, `pullrequest:write`) ou OAuth 2.0 **(défaut proposé : token en v1)** ; Git en HTTPS avec token ou deploy key SSH (« Access keys »).
- **Watch paths (dégradé)** : `GET /2.0/repositories/{workspace}/{repo_slug}/diffstat/{spec}` (spec `after..before` ou SHA de PR) pour obtenir les fichiers modifiés.
- **Build status** (équivalent checks, dégradé) : `POST /2.0/repositories/{workspace}/{repo_slug}/commit/{commit}/statuses/build` avec `state` ∈ `INPROGRESS`, `SUCCESSFUL`, `FAILED`, `STOPPED`, `key: "akerdock-preview"` **(défaut proposé)**, `url`. L'upsert est natif : re-POST avec la même `key` met à jour le statut.
- **Commentaire upserté** : `POST /2.0/repositories/{ws}/{slug}/pullrequests/{id}/comments` puis `PUT …/comments/{comment_id}` ; marqueur invisible identique (§2.7c) — le rendu Bitbucket conserve-t-il les commentaires HTML dans `content.raw` **(à vérifier)** ; à défaut, marqueur textuel discret en pied de commentaire.
- Pas d'équivalent Deployments API exploité (l'API « deployments » Bitbucket est liée à Bitbucket Pipelines) : non supporté.
- **Droits d'un auteur de commande** : `GET /2.0/workspaces/{workspace}/permissions/repositories/{repo_slug}?q=user.uuid="{uuid}"` — `permission ∈ {admin, write}` **(défaut proposé)**.

---

## 6. Gitea / Forgejo

### 6.1 Webhooks

- Configuration : repo (ou org) Settings → Webhooks → Gitea/Forgejo — URL `https://<fqdn>/webhooks/gitea/{endpoint_uuid}`, Content type JSON, Secret.
- **Signature** : `X-Gitea-Signature` = HMAC-SHA256 hexadécimal du corps brut (sans préfixe `sha256=`) ; Forgejo envoie `X-Forgejo-Signature` équivalent. Gitea émet aussi des headers de compatibilité GitHub (`X-GitHub-Event`, `X-Hub-Signature-256`) **(à vérifier pour `X-Hub-Signature-256` selon versions)** — AkerDock valide en priorité le header natif du provider déclaré, temps constant.
- Headers : `X-Gitea-Event` / `X-Forgejo-Event`, `X-Gitea-Delivery` / `X-Forgejo-Delivery` (→ `delivery_id`).
- Un même endpoint AkerDock `gitea` accepte les deux familles de headers (Forgejo est traité comme Gitea, provider `gitea`) **(défaut proposé)**.

### 6.2 Événements et previews PR

| Événement | Actions | Effet |
|---|---|---|
| `push` | — | Auto-deploy ; payload de type GitHub : `ref`, `before`, `after`, `commits[]` avec `added/modified/removed` → watch paths natifs |
| `pull_request` | `opened`, `synchronized`, `reopened` | Preview : deploy/redeploy (`pull_request.head.sha`) — action `synchronized` (avec « d », diffère de GitHub) **(à vérifier)** |
| `pull_request` | `closed` | Cleanup (`pull_request.merged` distingue merge/fermeture) |
| `issue_comment` | `created` | Commandes et approbation de fork (PR si `issue.pull_request` présent) |

- **Fork** : `pull_request.head.repo.id ≠ pull_request.base.repo.id`.
- **Self-hosted par nature** : `api_url` (ex. `https://gitea.example.com/api/v1`) et `html_url` sur la git source.

### 6.3 Accès API et feedback

- Auth : token d'accès (`Authorization: token <token>`) ; Git en deploy key SSH ou HTTPS+token.
- **Commit status** (dégradé, pas de Checks API) : `POST /repos/{owner}/{repo}/statuses/{sha}` avec `state` ∈ `pending`, `success`, `error`, `failure`, `warning`, `context: "AkerDock/preview"` **(défaut proposé)**, `target_url`.
- **Commentaire upserté** : API issues — `POST /repos/{owner}/{repo}/issues/{index}/comments`, édition `PATCH /repos/{owner}/{repo}/issues/comments/{id}` ; marqueur invisible identique (§2.7c).
- **Droits d'un auteur de commande** : `GET /repos/{owner}/{repo}/collaborators/{username}/permission` (réponse `permission` ∈ `admin`/`write`/`read`) **(défaut proposé)**.

---

## 7. Deploy webhook générique AkerDock (§12)

Endpoint API pour CI externes (pattern « build GitHub Actions → push registry → pull + redeploy », §5.1) :

```
GET|POST /api/v1/deploy?uuid={uuid}[,{uuid}…]&tag={tag}[,{tag}…]&force=true|false
Authorization: Bearer <token>          # permission `deploy` (§10.3)
Idempotency-Key: <clé>                 # optionnelle (§24.1)
```

- **Sémantique multi-cibles** : `uuid` accepte une liste séparée par virgules (UUIDs de ressources) ; `tag` déploie toutes les ressources portant ce(s) tag(s) dans la team du token. `uuid` et `tag` sont combinables ; l'ensemble des cibles est l'union dédupliquée **(défaut proposé)**. `force=true` = build sans cache (deployment-engine §5.2) ; pour une application « image », force = re-pull.
- **Réponse** : `200` avec un résultat par cible — `{"deployments": [{"resource_uuid": …, "deployment_uuid": …, "message": "queued"} | {"resource_uuid": …, "error": …}]}` **(défaut proposé)** ; un UUID inconnu **ou d'une autre team** produit la même entrée d'erreur générique (INV-002) ; `404` seulement si aucune cible ne résout. Chaque déploiement accepté répond la sémantique `202`-like du moteur (job en file, suivi par `deployment_uuid`).
- **Idempotence** : `Idempotency-Key` dédoublonne l'enqueue (jobs `idempotency_key`, INV-004) — une CI qui retente son appel ne crée pas deux déploiements. Sans clé, deux appels = deux déploiements (le coalescing §1.9 ne s'applique pas : il est réservé aux livraisons webhook de la même branche) **(défaut proposé)**.
- **Traçabilité** : chaque appel est enregistré comme livraison `provider = generic` (delivery_id généré, data dictionary §7.5), liée aux déploiements créés ; `deployments.trigger = api` **(défaut proposé — le vocabulaire canonique réserve `webhook` aux livraisons provider)**.
- Ce chemin **ignore** les politiques d'auto-deploy : `auto_deploy_enabled = false`, `[skip ci]` et watch paths ne s'y appliquent pas (§1.7) — l'appelant est authentifié et explicite. La politique fork (INV-010) est sans objet (pas de code non fiable : on déploie la config de la ressource).
- Rate limit API standard (200 req/min, §10.3) ; le plafond de file par serveur (`deployment_queue_limit`) répond `429`-like via l'erreur de plafond du moteur (deployment-engine §3.2).

---

## 8. Tableau de capacités par provider

✔ = supporté ; ◐ = dégradé (mécanisme moindre ou credential optionnel requis) ; ✘ = non supporté.

| Capacité | GitHub App | GitHub deploy key + webhook | GitLab | Bitbucket | Gitea/Forgejo | Générique (§7) |
|---|---|---|---|---|---|---|
| Auto-deploy on push | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ (appel explicite CI) |
| Previews PR/MR (deploy, redeploy, cleanup) | ✔ | ✔ (sans feedback) | ✔ | ✔ | ✔ | ✘ |
| Checks (condition de merge) | ✔ Checks API | ◐ commit statuses si PAT fourni | ◐ commit statuses | ◐ build statuses | ◐ commit statuses | ✘ |
| Deployments API (« View deployment ») | ✔ | ✘ | ✘ (Environments non exploité v1) | ✘ | ✘ | ✘ |
| Commentaire unique upserté | ✔ | ◐ si PAT `pull_requests:write` | ✔ note MR | ✔ | ✔ | ✘ |
| Commandes `/deploy` `/destroy` (opt-in) | ✔ `issue_comment` | ◐ token requis pour vérifier les droits | ✔ Note Hook | ✔ `pullrequest:comment_created` | ✔ `issue_comment` | ✘ |
| Forks sur approbation (§20.4.8) | ✔ label / commentaire / UI | ◐ UI seule (droits Git invérifiables sans token) | ✔ note / UI | ◐ commentaire / UI | ✔ commentaire / UI | ✘ |
| Watch paths (push) | ✔ (payload + compare API) | ✔ (payload ; compare si PAT, sinon fail-open >20 commits) | ✔ (payload + compare API) | ◐ (diffstat API obligatoire) | ✔ (payload) | ✘ (non applicable) |
| Watch paths (previews, §20.4.5) | ✔ compare API | ◐ PAT requis, sinon fail-open | ✔ diffs MR | ◐ diffstat | ✔ files API | ✘ |
| Discovery des repos | ✔ | ✘ (saisie manuelle) | ◐ (listing par token, optionnel v1) | ◐ | ◐ | ✘ |
| Self-hosted / Enterprise | ✔ GHES (`api_url`) | ✔ GHES | ✔ | ✘ (Cloud uniquement ; Data Center non couvert v1 **(défaut proposé)**) | ✔ natif | ✔ |

---

## 9. Gestion d'erreurs et retries côté provider

### 9.1 Réponses aux comportements des providers

- **Replays / redeliveries** : absorbés par la dédup `(provider, delivery_id)` (§1.4) — réponse `200`, statut `duplicate`, zéro effet. Un replay d'une livraison **au-delà de la rétention** (ligne purgée) réussirait la dédup : le garde-fou `out_of_order` (§1.10) et le coalescing bornent l'effet à un redéploiement du même SHA — idempotent au sens produit.
- **Livraisons désordonnées** : §1.10. Pour les previews, l'équivalent est le contrôle « head SHA du payload == head SHA courant de la PR » ; en cas de doute (événements PR croisés), le worker re-lit le head via l'API avant d'enqueue **(défaut proposé)**.
- **Payloads tronqués/corrompus** : une troncature réseau invalide la signature (`401`) ; un JSON invalide avec signature valide (bug provider) → `400`, livraison `failed`, jamais de déploiement partiel. Les champs attendus manquants après parse → `failed` avec classification, notification si récurrent.
- **Webhooks désactivés côté provider** (GitLab auto-disable, hook GitHub en erreur) : indétectable en push — une absence prolongée de livraisons pour une source active déclenche une **alerte d'inactivité** optionnelle **(défaut proposé : désactivée v1, backlog)**.

### 9.2 Rate limits des APIs providers (budgets et backoff)

- Budget d'appels de feedback par déploiement de preview : ~6 appels (check create/update ×2, deployment + status, comment list/upsert). À 5 000 req/h par installation GitHub (baseline Apps, susceptible d'être plus élevée selon taille d'installation), le feedback n'est pas le facteur limitant ; le **compare API des watch paths** sur monorepos actifs peut l'être — cache court des comparaisons par `(repo, before, after)` **(défaut proposé)**.
- Tous les clients provider honorent : `429`/`Retry-After`, et chez GitHub `403` secondaire + `X-RateLimit-Remaining`/`X-RateLimit-Reset`. Retry borné avec backoff exponentiel + jitter (§22.1), circuit breaker par `(provider, host)` pour qu'une panne provider ne sature pas les workers.
- Sous rate limit : le **feedback** est différé/abandonné (best-effort, §2.7) ; le **clone/déploiement** est retenté selon la politique du moteur (deployment-engine §2.4) ; jamais de retry immédiat en boucle.
- Réserve de sécurité : suspendre les appels non critiques (discovery, resync) quand `X-RateLimit-Remaining` < 10 % **(défaut proposé)**.

### 9.3 Expiration et rotation des credentials

- **PAT/token expiré ou révoqué** (`401` API, échec de clone HTTPS) : la git source passe en état **dégradé**, notification via les canaux (§11, sévérité `warning`), les livraisons continuent d'être acceptées mais les actions nécessitant l'API échouent proprement (feedback sauté, watch paths fail-open, commandes refusées `no_api_credentials`). L'erreur n'expose jamais le token (INV-003).
- **Clé privée d'App GitHub** : rotation supportée — l'utilisateur génère une nouvelle clé chez GitHub, la colle dans AkerDock (`app_private_key_enc` remplacée, ancienne clé révoquée chez GitHub ensuite) ; les JWT suivants utilisent la nouvelle clé, aucun impact sur les tokens d'installation déjà émis (valables ≤ 1 h).
- **Webhook secret** : rotation en deux temps — AkerDock génère le nouveau secret et accepte **les deux** (ancien + nouveau) pendant une fenêtre de **24 h (défaut proposé)** le temps que l'utilisateur mette à jour le provider ; l'audit trace quelle version a validé chaque livraison.
- **Deploy key retirée du repo** : échec de clone classifié `auth` → déploiement `failed` avec remédiation explicite, notification.
- **App désinstallée/suspendue** (`installation` `deleted`/`suspend`) : source dégradée immédiatement + notification ; les applications liées refusent l'auto-deploy avec motif clair.

---

## 10. Scénarios de test (base du plan E2E, §23.5 / §29.9)

Chaque scénario cible le pipeline complet (réception → décision → effet), par provider quand applicable :

| # | Scénario | Résultat attendu |
|---|---|---|
| T1 | **Replay** : même livraison rejouée (même `delivery_id`), y compris redelivery manuelle GitHub | `200`, statut `duplicate`, exactement un déploiement au total |
| T2 | **Mauvaise signature** : secret erroné, signature absente, signature d'un autre endpoint, corps modifié d'un octet | `401`, livraison `failed` `signature_valid=false`, aucun déclenchement, audit présent |
| T3 | **Repo homonyme/préfixe** : livraison pour `org/app-2` alors que l'application suit `org/app` (et variante `org/app` vs `org2/app`) | Aucune association (comparaison par ID/exacte), `ignored`/`failed`, jamais de déploiement de la mauvaise application |
| T4 | **Fork non approuvé** : PR de fork, approbation désactivée puis activée sans approbation | `ignored` `fork_untrusted` ; aucun secret dans l'environnement du builder dans tous les cas (assertion sur l'env du build) |
| T5 | **Fork approuvé puis nouveau push** : approbation par label, puis commit supplémentaire sur la PR fork | Preview du SHA approuvé uniquement ; le nouveau SHA repasse en attente d'approbation |
| T6 | **PR fermée pendant le build** : `closed` reçu alors que le build de preview est `building` | Build annulé (coopératif), preview → `destroying` → `destroyed`, deployment `cancelled`, check `cancelled`, deployment status GitHub `inactive` |
| T7 | **Delivery dupliquée concurrente** : deux POST simultanés du même `delivery_id` (course sur la contrainte UNIQUE) | Un seul `accepted`, l'autre `duplicate`, un seul déploiement |
| T8 | **Out-of-order** : push A→B puis push B→C, livraisons reçues C d'abord puis B | B ignorée `out_of_order` (ou coalescée) ; SHA final déployé = C |
| T9 | **Coalescing** : 3 pushes rapides pendant qu'un build occupe le slot | Déploiements intermédiaires `superseded` avec `superseded_by`, livraisons toutes tracées, dernier SHA déployé |
| T10 | **[skip ci]** : push dont le head commit contient le marqueur ; puis deploy via §7 | `ignored` `skip_ci` ; l'appel API générique déploie quand même |
| T11 | **Watch paths** : push monorepo ne touchant pas les patterns ; push >20 commits touchant les patterns (fallback compare API) ; force push | `ignored` `watch_paths` / déployé / déployé (fail-open documenté) |
| T12 | **Auto Deploy off** : `auto_deploy_enabled=false` | `ignored` `auto_deploy_disabled` ; deploy webhook générique fonctionnel |
| T13 | **Payload volumineux/tronqué** : corps > 2 MiB ; JSON invalide signé correctement | `413` ; `400` + `failed`, aucun effet |
| T14 | **Commande non autorisée** : `/deploy` par un auteur sans droit write (dont `author_association=CONTRIBUTOR`) | Refus silencieux côté PR, audit + `ignored`, aucun déploiement |
| T15 | **Commentaire unique** : 3 déploiements successifs d'une preview ; suppression manuelle du commentaire entre deux | Un seul commentaire upserté ; recréé après suppression (fallback marqueur) |
| T16 | **Credential expiré** : PAT révoqué avant un redeploy de preview | Déploiement selon capacité (clone SSH ok / échec classifié), feedback sauté proprement, source dégradée + notification, aucun secret dans les erreurs |
| T17 | **Isolation team** : livraison valide visant une application d'une autre team via un `endpoint_uuid` deviné/copié | Impossible par construction (endpoint → application → team) ; test API : token team A ne peut pas créer d'endpoint sur app team B (INV-002) |
| T18 | **Idempotence générique** : double `POST /api/v1/deploy` avec même `Idempotency-Key` ; multi-uuid avec un UUID inconnu | Un seul déploiement ; réponse par cible avec erreur générique pour l'inconnu |

Matrice d'exécution : T1–T3, T7, T13 par provider (GitHub App, GitHub manuel, GitLab, Bitbucket, Gitea) ; T4–T6, T14, T15 sur GitHub App + GitLab + Gitea au minimum ; le tout en Docker-in-Docker avec providers simulés (fixtures de payloads réels versionnées) conformément à la décision §27.26.
