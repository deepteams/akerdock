# ADR-038 — Modèle de rôles : admin/member/reviewer + rôles custom

## Statut

Accepté — **supersede la partie « rôles » d'[ADR-007](ADR-007-rbac-fin-projet-environnement.md)**
(l'ensemble des rôles système et le degré de granularité) et met à jour
[rbac-matrix.md](../specs/rbac-matrix.md) (§2, §3) en conséquence. Le reste
d'ADR-007 (permissions portées par l'Identity, scope le plus spécifique,
anti-élévation) tient. Complète le durcissement du **root d'instance** (session
`users.is_root`, hors modèle de team) déjà en place : cet ADR ne traite que des
rôles **de team**.

## Contexte

L'implémentation et la spec divergeaient déjà :

- enum `team_role` en base = `owner`, `admin`, `member` ;
- `PermissionsForRole` : `owner` = tout + `root` (team) ; `admin` = tout sauf
  `root` ; `member` = `read` + `deploy` (pas même `write`) ;
- `rbac-matrix.md` décrit encore `owner / admin / developer / viewer` — des noms
  qui n'existent nulle part dans le code.

Le modèle voulu (clarifié avec le mainteneur) est plus simple et explicite :

- **root** — administrateur de la plateforme (tout AkerDock). *Hors team.*
- **admin de team** — invite/exclut des membres, gère la team **et** ses
  ressources. « owner » et « admin » sont **la même chose** : un seul rôle haut
  de team, distinct de `root`.
- **member** — gère les ressources.
- **reviewer** — voit **uniquement** les PR previews.
- **rôle custom** — composé dans l'UI.

## Décision

### 1. Trois rôles système de team : `admin`, `member`, `reviewer`

`owner` est **fusionné dans `admin`** (il n'existe qu'un seul rôle haut de team ;
le créateur d'une team est `admin`). `owner` disparaît du modèle — la valeur
d'enum reste en base (PostgreSQL ne supprime pas une valeur d'enum) mais n'est
plus jamais attribuée. `reviewer` est ajouté.

| Rôle | Permissions (socle coarse) | Portée |
|---|---|---|
| `admin` | `read, read:sensitive, write, deploy, root` | Team + toutes ses ressources + gestion des membres/rôles + suppression de la team. Le `root` ici est **team-scoped** (terminal root, infra sensible de la team) — **jamais** le root d'instance. |
| `member` | `read, write, deploy` | Crée/déploie/gère les ressources. Pas de révélation de secret (`read:sensitive`), pas de `root`, pas de gestion des membres. |
| `reviewer` | `preview:read` | Voit les PR previews (liste, détail, logs, env, métriques) et **rien d'autre**. |

`member` gagne `write` (il ne l'avait pas — anomalie corrigée) ; il n'a
toujours pas `read:sensitive` ni `root`.

### 2. Les permissions **granulaires** deviennent l'unité d'évaluation

On câble enfin le modèle `domaine:action` d'ADR-007 (les ~72 permissions de
`rbac-matrix.md` §2), aujourd'hui purement documentaire — l'enforcement réel est
coarse (`require(auth.PermWrite)` etc.). Concrètement :

- Chaque opération OpenAPI déclare son `x-required-permission` **granulaire**
  (ex. `applications:deploy`, `databases:credentials`, `secrets:reveal`) au lieu
  du socle coarse. C'est la **source de vérité unique** de l'autorisation.
- `require()` vérifie la permission **granulaire** de l'opération ; l'Identity
  porte l'ensemble granulaire des permissions.
- **Tokens** : ils gardent leurs scopes coarse `{read, read:sensitive, write,
  deploy, root}` (§10.3) qui sont **projetés** vers l'ensemble granulaire (table
  « socle » de rbac-matrix §1). L'anti-élévation §4 reste : `perms(token) =
  scopes projetés ∩ perms RBAC du créateur`.

Sans cela, « member déploie les apps mais pas les bases », « reviewer = previews
seulement » ou un rôle custom fin **ne sont pas exprimables** — d'où le refus du
raccourci coarse.

### 3. Dépendances entre permissions (fermeture de prérequis)

Une action en implique d'autres — un rôle qui accorde `X` doit accorder ses
prérequis, sinon il est inutilisable. Les règles (à figer en code **et** dans
rbac-matrix, table `depends_on`) :

- toute action de mutation/déploiement/cycle de vie d'un domaine ⇒ le `:read` du
  même domaine (`applications:update` ⇒ `applications:read`, `databases:deploy` ⇒
  `databases:read`, `services:manage` ⇒ `services:read`…) ;
- `secrets:reveal` ⇒ `secrets:read` ; `databases:credentials` ⇒ `databases:read` ;
- `members:manage` ⇒ `members:read` ; `roles:manage` ⇒ `roles:read` ;
  `tokens:create`/`tokens:revoke` ⇒ `tokens:read` ;
- `environments:deploy` ⇒ `resources:read` + le `:read` des ressources visées.

La **fermeture** (ajout transitif des prérequis) est calculée à la composition
d'un rôle et à la résolution, jamais laissée à l'opérateur.

### 4. Rôles = ensembles nommés de permissions granulaires

- **Système** (immuables) :
  - `admin` = **toutes** les permissions de team (dont les actions `root`
    team-scoped : terminal root, infra sensible) — mais **jamais** `instance:*` ;
  - `member` = create/update/deploy/lifecycle/read sur projets, environnements,
    applications, databases, services, secrets (`secrets:write`/`:read`), **sans**
    `secrets:reveal`, sans gestion membres/rôles/tokens, sans `servers:*` de
    maintenance sensible, sans actions `root` ;
  - `reviewer` = uniquement les `:read` des **previews** (liste, détail, logs,
    env, métriques) — rien d'autre.
- **Custom** (par team) : ensemble **quelconque** de permissions granulaires du
  catalogue, **avec fermeture de prérequis** (§3), **hors `root`/`instance:*`**
  (jamais sélectionnables — garde-fou anti-élévation), et **⊆ permissions du
  composeur** (rbac-matrix §4.3). Schéma : `custom_roles(team_id, name,
  permissions[])` + `team_memberships.custom_role_id` (une adhésion porte soit un
  rôle système, soit un rôle custom).

`PermissionsForMembership` : rôle custom si présent, sinon set du rôle système ;
puis fermeture de prérequis ; puis intersection anti-élévation pour les tokens.

## Conséquences

- **Positives** : vrai RBAC à la carte (par domaine/action) enfin appliqué, code
  ↔ spec réconciliés (ADR-007 concrétisé), rôles custom réellement fins, reviewer
  strict, member réparé. Contrat = source de vérité de l'autorisation
  (`x-required-permission` granulaire).
- **Négatives / coût** : **c'est le gros morceau** — il faut donner un
  `x-required-permission` granulaire à **chaque** opération (~150), câbler
  l'enforcement granulaire (remplacer les `require(coarse)`), la projection
  token, la table de prérequis, et régénérer la grille rbac-matrix depuis le
  contrat. Migration de données `owner→admin`. Risque à couvrir par tests (un
  test « chaque op a une permission granulaire du catalogue » + matrice
  rôle×op).
- **Sécurité** : `root`/`instance:*` jamais dans un rôle custom ; anti-élévation
  à la composition et à l'usage ; root d'instance hors modèle de team.

## Plan d'implémentation (tranches)

1. **Socle granulaire** : catalogue des permissions en code (constantes + table
   de prérequis) ; passer `x-required-permission` en granulaire sur toutes les
   opérations ; enforcement granulaire ; projection token coarse→granulaire ;
   tests de couverture (chaque op ↦ perm connue). *Aucune régression fonctionnelle
   attendue : les rôles système gardent le même comportement effectif.*
2. **Rôles système** : migration (enum `+reviewer`, `owner→admin`, créateur =
   `admin`), sets granulaires admin/member/reviewer, invitations, UI dropdown.
3. **Rôles custom** : table + `custom_role_id`, CRUD OpenAPI (`/teams/{uuid}/roles`),
   résolution + fermeture de prérequis + anti-élévation, UI de composition.

## Alternatives rejetées

- **Rôles custom = sous-ensemble coarse {read,write,deploy,…}** : ne permet pas
  « deploy apps mais pas bases », ignore les dépendances entre actions — rejeté
  (c'était la première proposition, corrigée par le mainteneur).
- **Garder `owner` + `admin` distincts** : un seul rôle haut de team voulu.
- **En rester au coarse et ne pas câbler le granulaire** : laisse rbac-matrix
  aspirationnel et rend les rôles custom impossibles à faire sérieusement.
