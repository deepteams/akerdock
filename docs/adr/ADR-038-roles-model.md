# ADR-038 — Role model: admin/member/reviewer + custom roles

## Status

Accepted — the `reviewer` permission set is **amended by
[ADR-059](ADR-059-reviewer-inventory-read-access.md)** (read-only path to previews);
the rest stands. **Supersedes the "roles" part of [ADR-007](ADR-007-fine-grained-rbac-project-environment.md)**
(the set of system roles and the degree of granularity) and updates
[rbac-matrix.md](../specs/rbac-matrix.md) (§2, §3) accordingly. The rest of
ADR-007 (permissions carried by the Identity, most-specific scope,
anti-elevation) stands. Complements the hardening of the **instance root**
(`users.is_root` session, outside the team model) already in place: this ADR
only deals with **team** roles.

## Context

The implementation and the spec already diverged:

- `team_role` enum in the database = `owner`, `admin`, `member`;
- `PermissionsForRole`: `owner` = everything + `root` (team); `admin` =
  everything except `root`; `member` = `read` + `deploy` (not even `write`);
- `rbac-matrix.md` still describes `owner / admin / developer / viewer` —
  names that exist nowhere in the code.

The intended model (clarified with the maintainer) is simpler and more
explicit:

- **root** — platform administrator (all of AkerDock). *Outside teams.*
- **team admin** — invites/removes members, manages the team **and** its
  resources. "owner" and "admin" are **the same thing**: a single top team
  role, distinct from `root`.
- **member** — manages resources.
- **reviewer** — sees **only** the PR previews.
- **custom role** — composed in the UI.

## Decision

### 1. Three system team roles: `admin`, `member`, `reviewer`

`owner` is **merged into `admin`** (there is only one top team role; the
creator of a team is `admin`). `owner` disappears from the model — the enum
value remains in the database (PostgreSQL does not remove an enum value) but
is never assigned again. `reviewer` is added.

| Role | Permissions (coarse base) | Scope |
|---|---|---|
| `admin` | `read, read:sensitive, write, deploy, root` | Team + all its resources + member/role management + team deletion. The `root` here is **team-scoped** (root terminal, sensitive team infra) — **never** the instance root. |
| `member` | `read, write, deploy` | Creates/deploys/manages resources. No secret revelation (`read:sensitive`), no `root`, no member management. |
| `reviewer` | `preview:read` | Sees the PR previews (list, detail, logs, env, metrics) and **nothing else**. |

`member` gains `write` (it did not have it — anomaly fixed); it still has
neither `read:sensitive` nor `root`.

### 2. **Granular** permissions become the unit of evaluation

We finally wire up the `domain:action` model of ADR-007 (the ~72 permissions
of `rbac-matrix.md` §2), today purely documentary — the actual enforcement is
coarse (`require(auth.PermWrite)` etc.). Concretely:

- Each OpenAPI operation declares its **granular** `x-required-permission`
  (e.g. `applications:deploy`, `databases:credentials`, `secrets:reveal`)
  instead of the coarse base. This is the **single source of truth** for
  authorization.
- `require()` checks the operation's **granular** permission; the Identity
  carries the granular set of permissions.
- **Tokens**: they keep their coarse scopes `{read, read:sensitive, write,
  deploy, root}` (§10.3) which are **projected** onto the granular set
  ("base" table of rbac-matrix §1). The §4 anti-elevation remains:
  `perms(token) = projected scopes ∩ creator's RBAC perms`.

Without this, "a member deploys apps but not databases", "reviewer = previews
only" or a fine-grained custom role **are not expressible** — hence the
refusal of the coarse shortcut.

### 3. Dependencies between permissions (prerequisite closure)

An action implies others — a role granting `X` must grant its prerequisites,
otherwise it is unusable. The rules (to be frozen in code **and** in
rbac-matrix, `depends_on` table):

- any mutation/deployment/lifecycle action of a domain ⇒ the `:read` of the
  same domain (`applications:update` ⇒ `applications:read`, `databases:deploy` ⇒
  `databases:read`, `services:manage` ⇒ `services:read`…);
- `secrets:reveal` ⇒ `secrets:read`; `databases:credentials` ⇒ `databases:read`;
- `members:manage` ⇒ `members:read`; `roles:manage` ⇒ `roles:read`;
  `tokens:create`/`tokens:revoke` ⇒ `tokens:read`;
- `environments:deploy` ⇒ `resources:read` + the `:read` of the targeted
  resources.

The **closure** (transitive addition of prerequisites) is computed at role
composition time and at resolution time, never left to the operator.

### 4. Roles = named sets of granular permissions

- **System** (immutable):
  - `admin` = **all** team permissions (including team-scoped `root`
    actions: root terminal, sensitive infra) — but **never** `instance:*`;
  - `member` = create/update/deploy/lifecycle/read on projects, environments,
    applications, databases, services, secrets (`secrets:write`/`:read`),
    **without** `secrets:reveal`, without member/role/token management,
    without sensitive-maintenance `servers:*`, without `root` actions;
  - `reviewer` = only the `:read` of **previews** (list, detail, logs,
    env, metrics) — nothing else.
- **Custom** (per team): **any** set of granular permissions from the
  catalog, **with prerequisite closure** (§3), **excluding `root`/`instance:*`**
  (never selectable — anti-elevation guardrail), and **⊆ composer's
  permissions** (rbac-matrix §4.3). Schema: `custom_roles(team_id, name,
  permissions[])` + `team_memberships.custom_role_id` (a membership carries
  either a system role or a custom role).

`PermissionsForMembership`: custom role if present, otherwise the system
role's set; then prerequisite closure; then anti-elevation intersection for
tokens.

## Consequences

- **Positive**: real à-la-carte RBAC (per domain/action) finally enforced,
  code ↔ spec reconciled (ADR-007 made concrete), truly fine-grained custom
  roles, strict reviewer, member fixed. The contract = source of truth for
  authorization (granular `x-required-permission`).
- **Negative / cost**: **this is the big piece** — every operation (~150) must
  be given a granular `x-required-permission`, granular enforcement wired up
  (replacing the `require(coarse)` calls), token projection, the prerequisite
  table, and the rbac-matrix grid regenerated from the contract. Data
  migration `owner→admin`. Risk to be covered by tests (a test "every op has
  a granular permission from the catalog" + role×op matrix).
- **Security**: `root`/`instance:*` never in a custom role; anti-elevation at
  composition and at use; instance root outside the team model.

## Implementation plan (slices)

1. **Granular base**: permission catalog in code (constants + prerequisite
   table); switch `x-required-permission` to granular on all operations;
   granular enforcement; coarse→granular token projection; coverage tests
   (every op ↦ known perm). *No functional regression expected: the system
   roles keep the same effective behavior.*
2. **System roles**: migration (enum `+reviewer`, `owner→admin`, creator =
   `admin`), granular admin/member/reviewer sets, invitations, UI dropdown.
3. **Custom roles**: table + `custom_role_id`, OpenAPI CRUD (`/teams/{uuid}/roles`),
   resolution + prerequisite closure + anti-elevation, composition UI.

## Rejected alternatives

- **Custom roles = coarse subset {read,write,deploy,…}**: does not allow
  "deploy apps but not databases", ignores the dependencies between actions —
  rejected (it was the first proposal, corrected by the maintainer).
- **Keeping `owner` + `admin` distinct**: a single top team role is what is
  wanted.
- **Staying coarse and not wiring up the granular model**: leaves rbac-matrix
  aspirational and makes custom roles impossible to do seriously.
