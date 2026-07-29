# RBAC / Permissions Matrix — AkerDock (artifact §29.7)

> Authorization specification document (artifact §29.7 of the PRD, `docs/PRD.md`).
> Reference decisions: **ADR-007 / §27.7** — fine-grained RBAC, **à la carte permissions**
> model: each product action produces a granular `domain:action` permission; a role is a
> named set of permissions, assignable at the **team, project or environment** level (the
> most specific scope wins) — and **[ADR-038](../adr/ADR-038-roles-model.md)**, which
> replaced the system roles with **`admin` / `member` / `reviewer` + custom roles**
> (`owner` merged into `admin`; the **root reserved to the instance**, `users.is_root`,
> outside the team model).
>
> Consistency: the §10.3 API token permissions (`read`, `read:sensitive`, `write`,
> `deploy`, `root`) remain the per-action evaluation baseline (§24.1) and are **mapped**
> onto this granular model (§4 + §7). The OpenAPI `x-required-permission` values are
> a projection of these granular permissions (mapping table §7).
>
> Defaults proposed beyond parity are marked **(proposed default)**.

### State of implementation

This document describes both what the code enforces today and what it is specified to
enforce. Keeping the two apart matters: a spec read as a description of the running system
is how an operator ends up believing in a boundary that is not there.

| Part | State |
|---|---|
| Granular `domain:action` catalogue (§1.2) and per-operation enforcement | **Implemented** — `internal/auth/permissions.go` is the catalogue in code; every operation carries a granular `x-required-permission` |
| System roles `admin` / `member` / `reviewer` (§2) | **Implemented** — `session.PermissionsForRole`; §2 below is generated from that code |
| Custom roles, composed from the catalogue, with prerequisite closure and anti-elevation | **Implemented** — `custom_roles`, `auth.ValidateCustomPermissions` |
| Instance root outside the team model (§3.9) | **Implemented** — `users.is_root` |
| Token permissions = intersection with the creator's, re-evaluated per request (§4.2) | **Implemented** — `Middleware.boundToCreator`; a demoted, scoped or departed creator narrows their tokens without any revocation |
| Automatic token revocation on loss of rights (§4.4) | **Not implemented** (proposed default) — the intersection above makes it a tidiness measure rather than a control |

**Per-project and per-environment scoping was built and withdrawn**
([ADR-047](../adr/ADR-047-withdraw-scoped-role-assignments.md), superseding ADR-046 and the
scoped half of ADR-007). A member holds their role over **every project of their team**, and
the team is the only isolation boundary (§23.1). Two perimeters that must not see each other
are two teams. This document describes that model, not an intended one: where it once
promised scopes, it now says they are not coming back without a new decision.

---

## 1. Model

### 1.1 Granular permissions

A permission is named `domain:action`. It represents **one atomic product capability**.
It is **positive only** (no negative permissions): the absence of a permission means
**implicit deny** (§3.3).

Domain families:

| Domain | Scope |
|---|---|
| `team` | Team administration, members, invitations |
| `roles` | Custom roles and assignments |
| `tokens` | API tokens |
| `projects` / `environments` | Organizational hierarchy |
| `resources` | Cross-cutting view and resource adoption |
| `applications` | Applications (config, lifecycle, deployment) |
| `databases` | Managed databases |
| `services` | One-click services / compose stacks |
| `secrets` | Environment variables and sensitive values |
| `servers` | Target servers, proxy, maintenance |
| `certificates` | Server TLS certificates (observed reflection, expiration, renewal) |
| `keys` | Private SSH keys |
| `sources` | Git sources / GitHub Apps / webhooks |
| `registries` | Registry credentials |
| `cloud` | DNS-01 credentials for wildcard certificates (§4.3; cloud provisioning is removed — ADR-027) |
| `storages` | S3 storages |
| `backups` | Backup plans, executions, restore |
| `deployments` | History, logs, cancellation |
| `jobs` | Asynchronous jobs (durable queue) and dead-letter (retry/forget) |
| `previews` | PR / fork deployments |
| `templates` | Team template catalog |
| `terminal` | Terminal sessions |
| `port-forwards` | CLI TCP tunnels (ADR-032) |
| `external-endpoints` | Declared bastion targets outside the server (ADR-045) |
| `logs` | Runtime logs, drains |
| `metrics` | Metrics, uptime |
| `notifications` | Channels and rules |
| `audit` | Audit log |
| `config` | Config-as-code (export/apply) |
| `instance` | Instance settings (root only) |

### 1.2 Complete list of permissions (78)

> Convention: `read`/`view`/`list` = non-sensitive read; `read:sensitive` = secret
> revelation (INV-003); `manage`/`create`/`update`/`delete` = mutation; `deploy`/actions
> = lifecycle. Each permission maps to a §10.3 token permission ("token" column).

#### Identity, team, RBAC

| # | Permission | Description | Token map |
|---|---|---|---|
| 1 | `team:read` | View the team and its non-sensitive settings | `read` |
| 2 | `team:manage` | Modify the team, settings, deletion | `write` |
| 3 | `members:read` | List members and their roles | `read` |
| 4 | `members:manage` | Invite, remove, change a member's role | `write` |
| 5 | `invitations:manage` | Create/revoke invitations | `write` |
| 6 | `roles:read` | View roles and assignments | `read` |
| 7 | `roles:manage` | Create/edit/delete custom roles and assign them | `write` |
| 8 | `tokens:read` | List API tokens (metadata, never the value) | `read` |
| 9 | `tokens:create` | Create an API token (anti-elevation guard §4) | `write` |
| 10 | `tokens:revoke` | Revoke an API token | `write` |

#### Organization

| # | Permission | Description | Token map |
|---|---|---|---|
| 11 | `projects:read` | List/view projects | `read` |
| 12 | `projects:manage` | Create/edit/delete projects | `write` |
| 13 | `environments:read` | List/view environments | `read` |
| 14 | `environments:manage` | Create/edit/delete environments | `write` |
| 15 | `resources:read` | Cross-cutting resource view | `read` |
| 16 | `resources:adopt` | Adopt/unadopt an existing resource (§20.7) | `write` |
| 17 | `environments:deploy` | Coordinated deployment of an environment (§20.8) | `deploy` |

#### Applications

| # | Permission | Description | Token map |
|---|---|---|---|
| 18 | `applications:read` | List/view application config (secrets masked) | `read` |
| 19 | `applications:create` | Create an application | `write` |
| 20 | `applications:update` | Modify configuration (source, ports, limits, options…) | `write` |
| 21 | `applications:delete` | Delete an application | `write` |
| 22 | `applications:deploy` | Trigger deploy/redeploy/rollback | `deploy` |
| 23 | `applications:lifecycle` | Start/stop/restart | `deploy` |
| 24 | `applications:exec` | Scheduled tasks / pre-post commands (exec) | `deploy` |

#### Databases

| # | Permission | Description | Token map |
|---|---|---|---|
| 25 | `databases:read` | List/view databases (credentials masked) | `read` |
| 26 | `databases:create` | Create a managed database | `write` |
| 27 | `databases:update` | Modify a database's config/SSL/limits | `write` |
| 28 | `databases:delete` | Delete a database | `write` |
| 29 | `databases:lifecycle` | Start/stop/restart a database | `deploy` |
| 30 | `databases:credentials` | Reveal connection credentials/URLs (`read:sensitive`) | `read:sensitive` |

#### Services / Compose

| # | Permission | Description | Token map |
|---|---|---|---|
| 31 | `services:read` | View services/compose stacks | `read` |
| 32 | `services:manage` | Create/edit/delete a service, edit the compose | `write` |
| 33 | `services:deploy` | Deploy/pull latest/per-subcontainer lifecycle | `deploy` |

#### Secrets / variables

| # | Permission | Description | Token map |
|---|---|---|---|
| 34 | `secrets:read` | List variable keys (values masked) | `read` |
| 35 | `secrets:reveal` | Reveal variable/secret values (INV-003) | `read:sensitive` |
| 36 | `secrets:write` | Create/edit/delete variables (including bulk) | `write` |

#### Infrastructure: servers, keys, proxy

| # | Permission | Description | Token map |
|---|---|---|---|
| 37 | `servers:read` | List/view servers, resources, domains | `read` |
| 38 | `servers:manage` | Create/edit/remove a server, validation/install | `write` |
| 39 | `servers:maintain` | Cleanup, proxy lifecycle | `write` |
| 40 | `servers:proxy` | Edit proxy config, regenerate labels, CA rotation | `write` |
| 41 | `keys:read` | List SSH keys (metadata) | `read` |
| 42 | `keys:reveal` | Reveal private key material (`read:sensitive`) | `read:sensitive` |
| 43 | `keys:manage` | Create/edit/delete/rotate SSH keys | `write` |
| 68 | `certificates:read` | Inventory of a server's certificates (domains, expiration, status — observed reflection) | `read` |
| 69 | `certificates:renew` | Force renewal/re-issuance of a certificate (202 + job, audited) | `write` |

#### Sources, registries, cloud, storages

| # | Permission | Description | Token map |
|---|---|---|---|
| 44 | `sources:read` | View Git sources / GitHub Apps / webhooks | `read` |
| 45 | `sources:manage` | Configure Git sources, GitHub Apps, webhooks | `write` |
| 46 | `registries:manage` | Manage registry credentials | `write` |
| 47 | `cloud:read` | View cloud provider tokens (metadata) | `read` |
| 48 | `cloud:manage` | Manage DNS-01 credentials (§4.3, proxy-contract §7.2) | `write` |
| 49 | `storages:manage` | Manage S3 storages (CRUD, verification) | `write` |

#### Backups

| # | Permission | Description | Token map |
|---|---|---|---|
| 50 | `backups:read` | View backup plans and executions | `read` |
| 51 | `backups:manage` | Create/edit/delete plans, Backup Now | `write` |
| 52 | `backups:restore` | Restore a database/volume from a backup | `write` |

#### Execution, observability, terminal

| # | Permission | Description | Token map |
|---|---|---|---|
| 53 | `deployments:read` | History, detail, build logs (SSE) | `read` |
| 54 | `deployments:cancel` | Cancel an in-progress deployment | `deploy` |
| 70 | `jobs:manage` | Retry/forget dead-letter jobs (audited manual action, §21.3, deployment-engine §2.4) | `write` |
| 75 | `previews:read` | See the PR previews and their status — the whole of the `reviewer` role (ADR-038) | `read` |
| 55 | `previews:manage` | Manage previews, approve a fork PR (§20.4.8) | `write` |
| 56 | `templates:manage` | Register/sync template repos (§27.10) | `write` |
| 57 | `terminal:open` | Open a container/server terminal (non-root) | `write` |
| 58 | `terminal:root` | Open a **root** terminal (dual control §5) | `write` |
| 72 | `port-forwards:open` | Open a TCP tunnel to a resource's container or to a declared external endpoint (CLI, ADR-032/ADR-045) — boundary at resource granularity; on an external endpoint it is evaluated against that endpoint's scope. Also lists the team's tunnel sessions and closes **one's own**; closing somebody else's requires `external-endpoints:manage`, the same power that revokes a grant | `write` |
| 73 | `external-endpoints:read` | List the team's declared external endpoints (bastion targets, ADR-045) | `read` |
| 74 | `external-endpoints:manage` | Declare/update/delete an external endpoint — draws a network boundary, admin-level (ADR-045) | `write` |
| 59 | `logs:read` | Container runtime logs | `read` |
| 60 | `logs:manage` | Configure log drains | `write` |
| 61 | `metrics:read` | Server/resource metrics, uptime | `read` |
| 76 | `uptime:read` | Uptime monitors and their history | `read` |
| 77 | `uptime:manage` | Create/update/delete uptime monitors | `write` |
| 78 | `notifications:read` | List notification channels and rules (without their secrets) | `read` |
| 62 | `notifications:manage` | Notification channels and rules | `write` |
| 63 | `audit:read` | View the audit log | `read` |
| 64 | `config:export` | Export config-as-code (YAML) | `read` |
| 65 | `config:apply` | Idempotent apply of config-as-code (§24.5) | `write` |

#### Instance (instance root only — outside team scope)

| # | Permission | Description | Token map |
|---|---|---|---|
| 66 | `instance:manage` | Instance settings, enable/disable the API, updates | `root` |
| 67 | `instance:audit` | Global cross-team audit | `root` |
| 71 | `instance:encryption` | Encryption-at-rest status and forced master key rotation (re-encryption — ADR-003) | `root` |

> **Total: 78 granular permissions** (of which 3 are exclusively `instance:*`, reserved to the
> instance root, outside the team role model), so 75 in the team model. This is above the
> §29.7 target range (~40-60): the range was set before the product covered previews, uptime,
> tunnels and bastion endpoints, and a capability that exists is better named than folded into
> a neighbour. The count must match `auth.Catalog` — the numbering column is historical and
> has gaps, which is harmless; a missing permission is not.

---

## 2. Permissions × system roles matrix

> **Generated from the code** (`internal/auth/permissions.go` for the catalogue and its
> socles, `session.PermissionsForRole` for the roles). Where this table and the code
> disagree, the code is right and this table is stale — regenerate it rather than argue
> with it. ● = granted; ○ = not granted. `socle` is the coarse token scope the permission
> projects onto (§4). The three system roles are **immutable** (§3.3); an admin who wants
> to deviate composes a **custom role** (§1).

| Permission | socle | admin | member | reviewer |
|---|---|:---:|:---:|:---:|
| `applications:create` | write | ● | ● | ○ |
| `applications:delete` | write | ● | ● | ○ |
| `applications:deploy` | deploy | ● | ● | ○ |
| `applications:exec` | deploy | ● | ● | ○ |
| `applications:lifecycle` | deploy | ● | ● | ○ |
| `applications:read` | read | ● | ● | ○ |
| `applications:update` | write | ● | ● | ○ |
| `audit:read` | read | ● | ● | ○ |
| `backups:manage` | write | ● | ● | ○ |
| `backups:read` | read | ● | ● | ○ |
| `backups:restore` | write | ● | ● | ○ |
| `certificates:read` | read | ● | ● | ○ |
| `certificates:renew` | write | ● | ○ | ○ |
| `cloud:manage` | write | ● | ○ | ○ |
| `cloud:read` | read | ● | ○ | ○ |
| `config:apply` | write | ● | ○ | ○ |
| `config:export` | read | ● | ○ | ○ |
| `databases:create` | write | ● | ● | ○ |
| `databases:credentials` | read:sensitive | ● | ○ | ○ |
| `databases:delete` | write | ● | ● | ○ |
| `databases:lifecycle` | deploy | ● | ● | ○ |
| `databases:read` | read | ● | ● | ○ |
| `databases:update` | write | ● | ● | ○ |
| `deployments:cancel` | deploy | ● | ● | ○ |
| `deployments:read` | read | ● | ● | ○ |
| `environments:deploy` | deploy | ● | ○ | ○ |
| `environments:manage` | write | ● | ● | ○ |
| `environments:read` | read | ● | ● | ○ |
| `external-endpoints:manage` | write | ● | ○ | ○ |
| `external-endpoints:read` | read | ● | ● | ○ |
| `instance:audit` | root | ○ | ○ | ○ |
| `instance:encryption` | root | ○ | ○ | ○ |
| `instance:manage` | root | ○ | ○ | ○ |
| `invitations:manage` | write | ● | ○ | ○ |
| `jobs:manage` | write | ● | ○ | ○ |
| `keys:manage` | write | ● | ○ | ○ |
| `keys:read` | read | ● | ● | ○ |
| `keys:reveal` | read:sensitive | ● | ○ | ○ |
| `logs:manage` | write | ● | ○ | ○ |
| `logs:read` | read | ● | ● | ○ |
| `members:manage` | write | ● | ○ | ○ |
| `members:read` | read | ● | ● | ○ |
| `metrics:read` | read | ● | ● | ○ |
| `notifications:manage` | write | ● | ● | ○ |
| `notifications:read` | read | ● | ● | ○ |
| `port-forwards:open` | write | ● | ● | ○ |
| `previews:manage` | write | ● | ● | ○ |
| `previews:read` | read | ● | ● | ● |
| `projects:manage` | write | ● | ● | ○ |
| `projects:read` | read | ● | ● | ○ |
| `registries:manage` | write | ● | ● | ○ |
| `resources:adopt` | write | ● | ● | ○ |
| `resources:read` | read | ● | ● | ○ |
| `roles:manage` | write | ● | ○ | ○ |
| `roles:read` | read | ● | ○ | ○ |
| `secrets:read` | read | ● | ● | ○ |
| `secrets:reveal` | read:sensitive | ● | ○ | ○ |
| `secrets:write` | write | ● | ● | ○ |
| `servers:maintain` | write | ● | ○ | ○ |
| `servers:manage` | write | ● | ○ | ○ |
| `servers:proxy` | write | ● | ○ | ○ |
| `servers:read` | read | ● | ● | ○ |
| `services:deploy` | deploy | ● | ● | ○ |
| `services:manage` | write | ● | ● | ○ |
| `services:read` | read | ● | ● | ○ |
| `sources:manage` | write | ● | ● | ○ |
| `sources:read` | read | ● | ● | ○ |
| `storages:manage` | write | ● | ● | ○ |
| `team:manage` | write | ● | ○ | ○ |
| `team:read` | read | ● | ● | ○ |
| `templates:manage` | write | ● | ○ | ○ |
| `terminal:open` | write | ● | ● | ○ |
| `terminal:root` | write | ● | ○ | ○ |
| `tokens:create` | write | ● | ○ | ○ |
| `tokens:read` | read | ● | ○ | ○ |
| `tokens:revoke` | write | ● | ○ | ○ |
| `uptime:manage` | write | ● | ● | ○ |
| `uptime:read` | read | ● | ● | ○ |

> Design notes:
> - **admin** is every catalogue permission **except** the `instance:*` ones. There is a
>   single top team role: `owner` was merged into it (ADR-038) and the enum value survives
>   in the database only because PostgreSQL does not remove one.
> - **member** manages the team's resources — applications, databases, services, secrets,
>   deployments, backups, previews, notifications, uptime — and administers nothing: no
>   members/roles/tokens/invitations, no servers/keys/cloud, no root terminal.
> - **member writes secrets but cannot reveal them.** `secrets:write` without
>   `secrets:reveal`, `databases:read` without `databases:credentials`, `keys:read` without
>   `keys:reveal`: setting a value is a configuration act, reading one back is exfiltration
>   of a secret, and INV-003 separates the two. This surprises people; it is deliberate.
> - **reviewer** holds exactly one permission, `previews:read`. It is not a "read-only
>   member" — someone reviewing a pull request has no business listing the team's databases.
>   A read-only profile broader than that is a **custom role**, which is what custom roles
>   are for.
> - **`environments:deploy` is admin-only**, while `applications:deploy` is granted to
>   member. Deploying one application is routine; redeploying an entire environment at once
>   is a fleet operation. Worth re-examining if members end up asking for it — it is a
>   defensible line, not an obviously correct one.
> - The `instance:*` permissions are held by nobody in the team model: they belong to the
>   instance root (§3.9), whose identity bypasses this table entirely.

---

## 3. Resolution rules

> Per-project and per-environment assignment was specified here for a year, built in July
> 2026 (ADR-046) and **withdrawn** the next day
> ([ADR-047](../adr/ADR-047-withdraw-scoped-role-assignments.md)): it made authorization the
> hardest part of the platform to hold in one's head, for an expressiveness a separate team
> already provides. What follows is the model the code enforces, in full.

### 3.1 One role per member per team

A member holds **exactly one role** in a team — a system role (§2) or one of the team's
custom roles, on `team_memberships`. It applies to every project, environment and resource
of that team. There is no narrower assignment: the permission set resolved at authentication
is the whole answer, and `require(permission)` is the only gate.

The **team is the isolation boundary** (§23.1). Two perimeters that must not see each other
are two teams; servers, keys, git sources, registries and storages are then declared in each,
which is the honest price of a boundary that is actually enforced.

### 3.2 Multi-role accumulation

A member holds one role at a time. A custom role **overrides** the system role on the same
membership (the system role stays as the fallback if the custom role is deleted), and its
permission set is the effective one — closed under prerequisites at write time.

### 3.3 Implicit deny and immutability

- **Deny by default**: a permission absent from the effective set is denied. There are no
  negative permissions, so there are no exceptions to compose and none to forget.
- **Immutable system roles**: `admin`, `member`, `reviewer` are neither editable nor
  deletable. Deviating means composing a custom role (§1). (`none` exists in the database as
  a legacy enum value from ADR-046 and is never written.)
- **What a denial looks like**: a resource of **another team** answers `404` (INV-002: no
  oracle); a resource of this team the caller may not act on answers `403`.

### 3.4 The instance root case

- The **instance root** (`users.is_root`) is outside the team role model: it implicitly holds
  every permission on every team **plus** `instance:*` (§10.1).
- It is not an implicit member for audit purposes: its cross-team actions are recorded with
  `actor.type=user` plus the root flag (§23.4).
- A token created by the root is **scoped to one team** like any other (§10.3); there is no
  global token.

### 3.5 External endpoints carry intent, not a boundary

An external endpoint (ADR-045 §1) may name a project and an environment. With no per-project
permissions to evaluate, those fields **describe what the destination is for and enforce
nothing** — anyone holding `port-forwards:open` in the team may mint a tunnel to it. The
dashboard labels them as descriptive for that reason: a field that looks like a control and
is not is worse than no field, and the labelling is part of the decision (ADR-047).

### 3.6 The acting team of a session, and switching

A user may belong to several teams (PRD §37) with a **different role in each**. A browser
session therefore acts in exactly one team at a time — its *acting team* — and the whole
permission set comes from the membership held **in that team**.

- The acting team is resolved on **every request**, from `sessions.current_team_id`, and it
  is a **preference, not an authority**: the lookup returns a row only if the user still
  holds a membership in that team and the team is not soft-deleted. A session pinned to a
  team the user was removed from silently falls back to their oldest membership — a
  demotion, never a retained access.
- Team id, team uuid, role and permissions all come from that **same membership row**. They
  can never describe two teams at once, which is what makes `INV-001` checkable: every
  team-scoped query takes the identity's team id, and no other value exists to take.
- **Switching** (`POST /auth/session/team`, outside the v1 contract like the rest of
  `/auth`) moves the session and requires session + CSRF. It is refused for any team the user
  is not a member of, with the same `404` a non-existent team gets (INV-002). The **instance
  root is no exception**: seeing every team through `GET /teams` (§3.4) is not being a member
  of every team, and the switcher offers memberships only.
- The choice is remembered on `users.last_team_id`, so the next login opens where the user
  left off. It is a preference like the session's: re-checked against memberships at login.
- Switching is audited as `auth.team.switch`, recorded against the team being **left** —
  that team's administrators are the ones entitled to see the departure.
- **API tokens are unaffected**: a token is bound to one team at creation (§4.1) and has no
  session to move.

---

## 4. API tokens — mapping and anti-elevation guard

### 4.1 Token model recap (§10.3)

An API token carries a subset of `{read, read:sensitive, write, deploy, root}`, is scoped
to **one team**, hashed SHA-256, with IP allowlist and expiration (`api_tokens`, §10.3).

### 4.2 A token's effective permissions = intersection

> **Decision (resolution of the OpenAPI point)**: a token never grants more than its creator.

```
perms_effective(token) = perms_token(token)  ∩  perms_RBAC(creator, re-evaluated at use)
```

- `perms_token`: projection of the token's `{read, read:sensitive, write, deploy, root}` scopes
  onto granular permissions (via the "token map" column of §1 and the §7 table).
- `perms_RBAC(creator)`: the creator's RBAC permissions **in the token's team**, at the relevant
  scope, **re-evaluated on every request** (not frozen at creation).
- Consequence: if the creator loses a right (role downgrade), the token loses that right on
  the next request, without explicit revocation.

### 4.3 Anti-elevation guard at creation (`tokens:create`)

- Token creation requires the dedicated permission **`tokens:create`** (held by `admin`,
  absent from `member` and `reviewer` — see §2). This is the resolution of the point raised by
  the OpenAPI (`createApiToken`, `x-required-permission: write` + guard): `write` alone is not
  conceptually sufficient; the capability is carried by `tokens:create`, reserved to admins.
- **Mandatory anti-elevation**: the creator can only grant the token permissions
  they **hold themselves** at the targeted scope. Any request for a token scope whose projection
  exceeds `perms_RBAC(creator)` → `403` (consistent with the OpenAPI description of `createApiToken`).
- A `root` token can only be created by a holder of `instance:manage` (instance root) or,
  by extension, a team admin for a `root` token **bounded to their team** — never a token with
  instance privileges.

### 4.4 Automatic revocation on loss of rights **(proposed default)**

- **(Proposed default)**: when a creator loses the `tokens:create` permission (downgrade,
  removal from the team, account deletion), their active tokens are **automatically revoked**
  (`revoked_at` set, audit event).
- Rationale: re-evaluation at use time (§4.2) already neutralizes elevation, but explicit
  revocation closes the window where a token would "survive" its creator and clarifies the audit.
- Alternative kept if the default is rejected: keep the token but reduce it to
  the intersection (§4.2), with an alert to the admin. To be settled in an ADR if there is divergence.

---

## 5. Sensitive actions with dual control

> These actions require: **permission** + **reinforced confirmation** (§22.5) + **audit** (§23.4).
> The reinforced confirmation is a re-authentication/step-up or an explicit confirmation
> input depending on criticality.

| Action | Required permission | Additional control | Ref. |
|---|---|---|---|
| Open a **root** terminal (server shell) | `terminal:root` | Step-up: recent **passkey re-authentication** for a browser session (`403 stepup_required` otherwise), `root` permission for an API token — a token cannot re-authenticate. Plus open/close audit + idle/kill | §24.4, §23.4, §10.4 |
| **Restore onto a non-empty database** | `backups:restore` | Explicit reinforced confirmation + prior format test + full journal | §20.5, §22.5 |
| Deletion **with volumes/data** | `applications:delete` / `databases:delete` / `services:manage` | Preview of affected objects + separate "keep the volumes?" question + confirmation | §20.6, §22.5, INV-008 |
| Database **CA rotation** | `servers:proxy` | Reinforced confirmation + audit | §6.3, §22.5, §23.4 |
| Deletion of a team / project-environment cascade | `team:manage` / `projects:manage` | Cascade preview + confirmation; RESTRICT while dependencies exist | §19.2, §10.1, INV-008 |
| Creation of an elevated `root`/`deploy` token | `tokens:create` | Anti-elevation guard (§4.3) + creation/revocation audit | §10.3, §23.4 |
| **Forced master key rotation** (active re-encryption) | `instance:encryption` | Reinforced confirmation + `Idempotency-Key` + audit; reserved to the instance root | §23.2, ADR-003 |
| **Forget** of a dead-letter job with remote remnants | `jobs:manage` | Mandatory `acknowledge_remnants=true` body (otherwise `409 remnants_present`) + audit | §20.6.4, §21.3 |

All these actions emit an audit event with actor/token, target, result and redacted
diff (§23.4).

---

## 6. Required authorization tests

> Each authorization invariant has at least one API/integration test (§17). The matrix
> below is the basis of the security test suite (§23.5).

### 6.1 Cross-team matrix (INV-002)
- For **each endpoint** and each indirect relation: an actor of team A attempting
  to access a UUID of team B receives `not_found` (never `403` revealing existence, never
  a leak).
- Covers: servers, keys, sources, destinations, storages, resources, tokens, backups, previews.

### 6.2 Team isolation is the boundary (§3.1)

- A member of team A reaches every project of team A and **nothing** of team B — the only
  partition the product has, and the one every cross-team test above already covers.
- No per-project expectation is tested any more: ADR-047 withdrew scoping, and a test
  asserting a boundary the code does not draw is a false comfort.
- **Who can reach a resource** is answered by the members list: every member of the team
  reaches every project of it, at the level their role grants. There is no per-resource view
  to keep in step with the rules (ADR-047).

### 6.3 Elevation via token (§4)
- A `member` creator cannot create a token carrying `write`/`deploy` exceeding their rights
  → `403` (anti-elevation §4.3).
- A token whose creator is downgraded loses the corresponding right on the next request
  (re-evaluation §4.2); **(proposed default)** verify automatic revocation (§4.4).
- A `write` without `tokens:create` cannot create a token → `403` (§4.3).
- **(Scoped work)** A token created by a member scoped to `project=X` reaches X and returns
  `404` on Y, and narrows on its own when the creator's assignment narrows (§3.7).

### 6.4 Each system role × each endpoint family
- `admin`, `member`, `reviewer` tested on each family (applications, databases, services,
  secrets, servers, keys, backups, terminal, deployments, cloud, config).
- **reviewer**: everything except `previews:read` → `403`/`404`. It is not a read-only
  member, and a test that only checks "cannot mutate" would miss the point.
- **member**: `servers:manage`, `keys:manage`, `cloud:*`, `terminal:root`, `members:manage`,
  `roles:manage`, `tokens:create`, `jobs:manage`, `config:*` → `403`.
- **member and secrets**: `secrets:write` succeeds, `secrets:reveal`,
  `databases:credentials` and `keys:reveal` are refused (INV-003) — the asymmetry of §2 is
  deliberate and must stay tested, or it will be "fixed" by someone who reads it as a bug.
- **admin**: every team permission granted, every `instance:*` refused.

### 6.5 Sensitive actions (dual control §5)
- Root terminal, restore onto a non-empty database, deletion with volumes, CA rotation: verify
  that in the absence of reinforced confirmation the action is denied even if the permission is
  present.

### 6.6 Consistency with audit
- Each denied/granted authorization action on a sensitive action produces a usable
  audit entry (§23.4).

---

## 7. OpenAPI `x-required-permission` → granular permissions mapping table

> ⚠️ **Historical and partial.** Since ADR-038, each operation carries its granular
> `x-required-permission` **in the OpenAPI contract itself**, which is the single source of
> truth and is checked against `auth.Catalog` by a test. The table below predates that and
> covers only the operations that existed then — it is kept for the reasoning in its notes,
> not as an inventory. Read the contract for the mapping of any given operation.
>
> Effective access control = the granular permission carried by the operation, **and** the
> token must carry the socle it projects onto (both conditions, §4.2 intersection).

| operationId (OpenAPI) | x-required-permission | Granular permission(s) |
|---|---|---|
| getHealth | (none) | — (public) |
| getVersion | read | `team:read` |
| enableApi / disableApi | root | `instance:manage` |
| listTeams / getTeam | read | `team:read` |
| **createTeam** | root | **`instance:manage`** — instance-root SESSION only (§3.4): a team is the isolation boundary of every resource, so creating one is an instance-level act. The creator joins as `admin`. |
| listTeamMembers | read | `members:read` |
| listTeamInvitations | read | `members:read` |
| createTeamInvitation / revokeTeamInvitation | write | `invitations:manage` |
| listApiTokens | read | `tokens:read` |
| **createApiToken** | write | **`tokens:create`** (+ anti-elevation guard §4.3) |
| revokeApiToken | write | `tokens:revoke` |
| listProjects / getProject | read | `projects:read` |
| createProject / updateProject / deleteProject | write | `projects:manage` |
| listEnvironments / getEnvironment | read | `environments:read` |
| createEnvironment / updateEnvironment / deleteEnvironment | write | `environments:manage` |
| listPrivateKeys / getPrivateKey | read | `keys:read` (+ `keys:reveal` if key material requested) |
| createPrivateKey / updatePrivateKey / deletePrivateKey | write | `keys:manage` |
| listServers / getServer / listServerResources / listServerDomains | read | `servers:read` |
| createServer / updateServer / deleteServer | write | `servers:manage` |
| validateServer | write | `servers:manage` |
| listApplications / getApplication | read | `applications:read` |
| createApplication | write | `applications:create` |
| updateApplication | write | `applications:update` |
| deleteApplication | write | `applications:delete` (dual control if volumes §5) |
| listApplicationEnvs | read | `secrets:read` (+ `secrets:reveal` for the values) |
| createApplicationEnv / updateApplicationEnv / deleteApplicationEnv / replaceApplicationEnvs | write | `secrets:write` |
| startApplication / stopApplication / restartApplication | deploy | `applications:lifecycle` |
| deployApplication / rollbackApplication | deploy | `applications:deploy` |
| listApplicationDeployments / getDeployment / getDeploymentLogs | read | `deployments:read` |
| cancelDeployment | deploy | `deployments:cancel` |
| webhookDeploy / webhookDeployPost | deploy | `applications:deploy` (`deploy` token) |
| listDatabases / getDatabase | read | `databases:read` (+ `databases:credentials` for URLs/creds) |
| createPostgresqlDatabase | write | `databases:create` |
| updateDatabase | write | `databases:update` |
| deleteDatabase | write | `databases:delete` (dual control if volumes §5) |
| startDatabase / stopDatabase / restartDatabase | deploy | `databases:lifecycle` |
| listBackupPlans / getBackupPlan / listBackupExecutions | read | `backups:read` |
| createBackupPlan / updateBackupPlan / deleteBackupPlan / executeBackupPlan | write | `backups:manage` |
| **restoreBackupExecution** | write | **`backups:restore`** (dual control if non-empty database §5) |
| getJob | read | read permission of the underlying resource (contextual) |
| listJobs | read | read permission of the underlying resource (contextual, like getJob) |
| retryJob / forgetJob | write | `jobs:manage` (forget with remote remnants: dual control §5) |
| listServerCertificates / getCertificate | read | `certificates:read` |
| renewCertificate | write | `certificates:renew` |
| getEncryptionStatus / rotateEncryption | root | `instance:encryption` (rotation: dual control §5) |

> Observations forwarded for OpenAPI revision:
> - `createApiToken` should expose `x-required-permission: write` **plus** an extension
>   `x-required-grant: tokens:create` (or equivalent) to materialize the §4.3 guard.
> - `restoreBackupExecution` and the `delete*` operations with volumes should carry a marker
>   `x-sensitive-action: true` to signal the §5 dual control.
> - `listPrivateKeys`/`listApplicationEnvs`/`getDatabase`: revelation of sensitive material
>   depends on `read:sensitive` (INV-003); the granular `*:reveal` / `*:credentials` permission
>   gates the fields, consistent with `is_redacted`.

---

## 8. Summary

- **78 granular permissions** defined (`domain:action`), of which 75 for the team role model
  and 3 exclusively `instance:*` (instance root). §1.2 and §2 are kept in step with
  `internal/auth/permissions.go`, which is the catalogue in code.
- **3 immutable system roles**: `admin`, `member`, `reviewer` (previews only) + custom roles
  composable by team admins (ADR-038, replacing ADR-007's owner/developer/viewer).
- **One role per member per team**, applying to every project of that team; **implicit deny**;
  the team is the only isolation boundary (§23.1). Per-project scoping was built and withdrawn
  (ADR-047).
- API tokens = **intersection** (token perms ∩ creator's RBAC perms re-evaluated at use time);
  creation via `tokens:create` (admin+) with **mandatory anti-elevation guard**; **automatic
  revocation on loss of rights (proposed default)**.
- Sensitive actions (root terminal, restore onto non-empty database, deletion with volumes, CA rotation)
  = permission + reinforced confirmation + audit.
