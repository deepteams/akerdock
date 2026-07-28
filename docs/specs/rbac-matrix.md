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
| **Role assignment scoped to a project or an environment (§3)** | **Implemented** (ADR-046) — `role_assignments` + `auth.Resolve`; the scoped check lives in the `resolve*` helpers and the collection filters |
| The access view — who reaches a resource / what a member reaches (§3.11) | **Implemented** — computed on demand, no stored copy |
| `projects:create` (team-only) and the `none` base role | **Implemented** — the catalogue holds 79 permissions and four system roles |
| Invitations and SCIM defaulting to `none` | **Implemented** — an arrival holds nothing until assigned |
| Automatic token revocation on loss of rights (§4.4) | **Not implemented** (proposed default) |
| Scope of an external endpoint (ADR-045 §1) | **Implemented** — `endpointInScope` evaluates `port-forwards:open` at the endpoint's scope |
| Scoped delegation (`roles:manage` at a project scope) | **Not implemented** — deliberately out of ADR-046's v1 |

Partitioning is **inert until it is used**: with no scoped assignment and no member set to
`none`, every caller resolves to their base role exactly as before ADR-046, and the extra
lookup is never performed. A team that wants a boundary sets a member's base role to `none`
and assigns them the projects they work on — an assignment alone changes nothing, because it
is an *exception* to a base role that otherwise still grants everything.

---

## 1. Model

### 1.1 Granular permissions

A permission is named `domain:action`. It represents **one atomic product capability**.
It is **positive only** (no negative permissions): the absence of a permission means
**implicit deny** (§3.6).

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

### 1.2 Complete list of permissions (79)

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
| 12 | `projects:manage` | Edit/delete **this** project — scoped (ADR-046 §4) | `write` |
| 79 | `projects:create` | Create a project — team-only, since creation has no parent scope to be evaluated against (ADR-046 §4) | `write` |
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

> **Total: 79 granular permissions** (of which 3 are exclusively `instance:*`, reserved to the
> instance root, outside the team role model), so 76 in the team model. This is above the
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
> projects onto (§4). The three system roles are **immutable** (§3.6); an admin who wants
> to deviate composes a **custom role** (§1). ADR-046 adds a fourth, `none` — the empty set —
> as the base role of a member who only holds scoped assignments (§3.3).

> A fourth system role, **`none`**, holds nothing at all and is therefore not worth a column:
> every cell would read ○. It exists so a member can be restricted to their scoped
> assignments (ADR-046 §2).

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
| `projects:create` | write | ● | ● | ○ |
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

> §3.1 to §3.8 specify the **scoped assignment model**, decided by
> **[ADR-046](../adr/ADR-046-scoped-role-assignments.md)** and **not implemented today**
> (see *State of implementation*). They are written to be implementable as they stand: the
> data model, what a scope can and cannot grant, how a resource's scope is derived, and what
> a denial looks like. §3.9 (instance root) is implemented and describes the running system.

### 3.1 What an assignment is

```
team  ⊃  project  ⊃  environment
```

An assignment is a triple `(member, role, scope)`:

- **member** — a user in a team. Never a group, never an email: an invitation carries a
  role, but the assignment exists only once the membership does.
- **role** — a system role (§2) or a custom role of the same team. A role is a *name for a
  set of permissions*; nothing is ever assigned permission by permission, which is what
  keeps the model auditable ("Alice is member on billing", not a list of 40 checkboxes).
- **scope** — the team, one of its projects, or one of its environments.

The team-level assignment stays where it is today, on `team_memberships`
(`role` + `custom_role_id`): every member has exactly one **base role**, and the narrower
assignments are the exceptions to it. Keeping the base on the membership row is what
preserves the "last admin cannot be demoted" guard and lets the members list stay one query.

Narrower assignments live in a new table, sketched here because its shape carries the
rules rather than merely storing them:

```
role_assignments(uuid, team_id, user_id,
                 role,              -- system role, XOR custom_role_id
                 custom_role_id,
                 project_id,        -- XOR environment_id, both NULL is not an assignment
                 environment_id,
                 created_by, created_at, updated_at)
```

- CHECK: exactly one of (`role`, `custom_role_id`) — a role source that is neither or both
  is not a role.
- CHECK: exactly one of (`project_id`, `environment_id`) — the team level is the membership
  row, not a row here with two NULLs.
- `UNIQUE NULLS NOT DISTINCT (user_id, project_id, environment_id, role, custom_role_id)`:
  assigning the same role twice at the same scope is a no-op, not a duplicate. `NULLS NOT
  DISTINCT` is load-bearing — a plain UNIQUE lets rows whose `custom_role_id` is NULL
  duplicate freely, because NULL never equals NULL. Same reasoning as the notification-rule
  index (`00024_notifications.sql`).
- `ON DELETE CASCADE` from team, user, project, environment and custom role: an assignment
  outliving its scope is a dangling grant, which is exactly the kind of thing that is still
  in the table three years later.

> Decided by **[ADR-046](../adr/ADR-046-scoped-role-assignments.md)**: the
> base-role-plus-exceptions shape above, the override semantics of §3.4 (a narrow scope may
> *reduce* rights), the `none` base role of §3.3, invisibility outside the scope (§3.6), and
> both levels — project **and** environment — in v1. Scoped delegation (`roles:manage` at a
> project scope) stays out of scope there too.

### 3.2 The scope of a resource

Resolution needs one function: given the resource an operation targets, which project and
environment does it belong to? It is the part that touches every handler, so it is spelled
out here rather than discovered per endpoint.

| Resource | Scope |
|---|---|
| Application, database, compose service | its environment → that environment's project |
| Environment variable, storage attached to a resource, backup plan/execution, deployment, preview, uptime monitor | the scope of the resource it belongs to |
| Job | the scope of the resource it acts on; a job with no resource is team-level |
| External endpoint (ADR-045) | its declared `project_id` / `environment_id`, team-level when both are NULL |
| Project | itself |
| Environment | itself, and its project |
| **Server, SSH key, git source, GitHub App, registry credential, DNS credential, S3 storage, notification channel, template, API token, member, custom role, audit trail, instance settings** | **team-level — they have no project** |

The bottom row is the load-bearing one: a server is shared by every project of the team, so
"scoping a server to a project" would be a lie the moment a second project deploys onto it.
Infrastructure is administered at the team level, full stop.

### 3.3 What a scoped assignment can grant

A role is a set of permissions, but not every permission means anything at a project scope.
Three classes, and every catalogue permission belongs to exactly one:

| Class | Permissions | Behavior when the role is assigned at project/environment scope |
|---|---|---|
| **Scoped** | `applications:read`, `applications:create`, `applications:update`, `applications:delete`, `applications:deploy`, `applications:lifecycle`, `applications:exec`, `databases:read`, `databases:create`, `databases:update`, `databases:delete`, `databases:lifecycle`, `services:read`, `services:manage`, `services:deploy`, `secrets:read`, `secrets:write`, `backups:read`, `backups:manage`, `backups:restore`, `deployments:read`, `deployments:cancel`, `previews:read`, `previews:manage`, `environments:read`, `environments:manage`, `environments:deploy`, `projects:read`, `projects:manage`, `resources:read`, `resources:adopt`, `logs:read`, `metrics:read`, `uptime:read`, `uptime:manage`, `terminal:open`, `port-forwards:open`, `external-endpoints:read` | Granted **on the resources of that scope only** |
| **Team-read** | `team:read`, `members:read`, `servers:read`, `certificates:read`, `keys:read`, `sources:read`, `notifications:read` | Granted **team-wide**, because they are working prerequisites: you cannot deploy an application without seeing the server it lands on. They expose no secret — the sensitive half lives in `*:reveal` / `*:credentials`, which are not in this class (INV-003). Note that `resources:read` is **not** here: it is the cross-cutting view of the resources themselves, so it is scoped like them, or the scoping leaks through the one endpoint that lists everything |
| **Team-only** | `team:manage`, `members:manage`, `invitations:manage`, `roles:read`, `roles:manage`*, `tokens:read`, `tokens:create`, `tokens:revoke`, `servers:manage`, `servers:maintain`, `servers:proxy`, `certificates:renew`, `keys:manage`, `keys:reveal`, `sources:manage`, `registries:manage`, `cloud:read`, `cloud:manage`, `storages:manage`, `templates:manage`, `config:export`, `config:apply`, `logs:manage`, `jobs:manage`, `audit:read`, `notifications:manage`, `external-endpoints:manage`, `secrets:reveal`, `databases:credentials`, `terminal:root`, `instance:manage`, `instance:audit`, `instance:encryption` | **Never granted by a scoped assignment.** Assigning a role that contains them at a project scope is not an error — the permission is simply not conferred — but the API MUST say so in the response, or an admin will believe they delegated something they did not |

\* `roles:manage` is the one exception worth allowing later: a project-scoped `roles:manage`
would let a team lead manage assignments *on their own project*. It is deliberately **not**
in v1 — delegating the power to delegate is the kind of thing that needs its own ADR.

Three entries in that table deserve their reason, because each looks misplaced until you ask
what the permission actually reaches:

- **`terminal:root` is team-only.** It opens a shell on the *server*, not in a container, and
  a server is shared by every project (§3.2). Scoping it to a project would suggest a
  boundary the shell does not have. `terminal:open` — a shell inside one resource's
  container — is scoped, as it should be.
- **`secrets:reveal` and `databases:credentials` are team-only**, although `secrets:write`
  and `databases:read` are scoped. This is §2's asymmetry again: writing configuration is a
  project act, reading a secret back is exfiltration, and INV-003 keeps the second one an
  admin decision.
- **`notifications:manage` is team-only** even though a notification *rule* already carries
  its own `project_id`/`environment_id` (`00024_notifications.sql`). The channel — with its
  webhook URL and its token — is team-level, and managing rules today means managing
  channels. Splitting the two is a reasonable follow-up, not part of this specification.

Two entries move with ADR-046 and are listed here in their decided form, ahead of the code:

- **`projects:create` (team-only, new)** — creating a project has no scope to be evaluated
  against, so the capability is split out rather than turned into a special case of
  `projects:manage`, which stays **scoped** and means "rename or delete *this* project".
- **`notifications:read` stays team-read, but notification *rules* are filtered.** A channel
  carries no project name and knowing which exist is how anyone asks for a rule; a rule
  carries `project_id`/`environment_id` and would otherwise publish the names of projects a
  scoped member must not see — the §3.6 leak, through the one collection nobody thinks of as
  a resource list.

The classification must stay exhaustive: **every catalogue permission appears in exactly one
class**, and a new permission added to `auth.Catalog` without a class here is a permission
whose behavior under scoping nobody decided. Worth a test that walks the catalogue against
this table once §3 exists.

`audit:read` is team-only on purpose: the trail is team-wide and un-partitioned, so granting
it at a project scope would either leak other projects' activity or require partitioning the
audit read path — a much larger question (§23.4).

**The `none` base role.** Restricting somebody *to* a project needs a base role that grants
nothing, otherwise the team-level role keeps leaking everywhere. `member` is too much,
`reviewer` still sees every preview of the team. The model therefore needs a fourth system
role — `none`, the empty permission set — as the base for a member who only holds scoped
assignments. Without it, §3 buys nothing: this is the first thing to build, not the last.

### 3.4 Inheritance — the most specific wins (override, not intersection)

- A **team**-level assignment applies to every project and environment.
- A **project**-level assignment applies to all of that project's environments.
- An **environment**-level assignment applies to that environment only.
- **The most specific scope that has an assignment wins**, and it *replaces* the broader one
  rather than adding to it. A member who is `member` on the team and `reviewer` on
  `project=payments` is a reviewer there — a narrow scope can **reduce** rights, which is
  what makes "everything except production" expressible.
- Resolution happens **per operation, against the targeted resource**, not once per session.

For an action on resource `r`, with `S(r)` the scopes covering `r` ordered
environment → project → team:

```
perms(subject, r) = ⋃ { role.permissions | assignment(subject, role, s) }
                    where s = the first scope in S(r) that has at least one assignment
                  ∪ { team-read permissions of every assignment held by the subject }
```

### 3.5 Multi-role accumulation (union at equal scope)

- Several roles may be held **at the same scope**: the effective set there is their **union**.
- Across scopes, §3.4 selects the scope first, then the union applies within it. Union never
  crosses a scope boundary — that is what an override means.

### 3.6 Implicit deny, invisibility and immutability

- **Deny by default**: a permission absent from the effective set is denied. There are no
  negative permissions, so there are no exceptions to compose and none to forget.
- **Immutable system roles**: `admin`, `member`, `reviewer` (and `none`, §3.3) are neither
  editable nor deletable. Deviating means composing a custom role (§1).
- **What a denial looks like**:
  - resource of **another team** → `404` (INV-002: no oracle);
  - resource of this team that the caller lacks the domain's `:read` for **at the scope
    covering it** → `404`. This is a departure from "404 only across teams", and it is
    deliberate: the point of scoping a member out of `project=payments` is that the project
    does not exist as far as they are concerned. A `403` here would answer the question the
    boundary exists to refuse;
  - resource the caller **can read but not act on** → `403`.
- **Where the check happens.** `require(perm)` keeps its meaning — holds the permission
  *somewhere* — and the scoped evaluation lives in the `resolve*` helpers, which already load
  the row and own the team-boundary 404 (ADR-046 §6). A handler that skips it cannot compile,
  because it has no resource without calling a resolver.
- **Collections must filter, not just guard.** `GET /projects`, `GET /applications`,
  `GET /servers/{uuid}/resources`, the search endpoints and every SSE stream return only what
  the caller's scopes cover. A list endpoint that returns everything and relies on the detail
  endpoint to say no has already leaked the names, and names are half of what a competitor
  wants. This is the largest single piece of implementation work in §3.

### 3.7 API tokens under scoping

§4 is unchanged in principle and gains one word: the intersection is evaluated **at the
scope of the targeted resource**.

```
perms_effective(token, r) = perms_token(token) ∩ perms_RBAC(creator, r)
```

A token created by a member scoped to `project=billing` therefore reaches `billing` and
nothing else, and it narrows by itself the day their assignment narrows — re-evaluated on
every request, never frozen at creation (§4.2).

### 3.8 Anti-elevation on assignment

- Creating, changing or deleting an assignment requires `members:manage` — team-level in v1
  (§3.3), so only a team admin assigns.
- An assigner may never grant a role whose permission set exceeds their own **at the target
  scope** — the same rule as custom-role composition (`auth.ValidateCustomPermissions`),
  applied to assignment rather than authorship.
- The **last admin** guard stays team-level: an admin cannot be demoted or scoped away if
  they are the last one, or a team locks itself out.
- Every assignment change is audited with actor, subject, role and scope (§23.4). "Who could
  reach production last March" is an audit question, and it is answerable only if the
  assignment history is in the trail.

### 3.9 The instance root case

- The **instance root** (`users.is_root`) is outside the team role model: it implicitly holds
  every permission on every team **plus** `instance:*` (§10.1), and §3.4 never runs for it.
- It is not an implicit member for audit purposes: its cross-team actions are recorded with
  `actor.type=user` plus the root flag (§23.4).
- A token created by the root is **scoped to one team** like any other (§10.3); there is no
  global token.

### 3.11 The access view (reverse resolution)

The rules above answer "may this subject act on this resource". An operator also needs the
same question with the subject unbound — **who can reach this resource** — and its mirror,
**what does this member reach**. Both are the resolution of §3.4 evaluated on demand, never a
stored copy: a denormalized access table drifts from the rules it summarizes, and a review
reading a stale copy asserts a safety nobody verified.

- Per resource, on applications, databases, services, projects and environments; per member,
  on the member's own screen (the offboarding question).
- Each row names **the scope that granted it**, because "Bob" is not actionable and "Bob —
  member on `project:billing`" is.
- **API tokens are subjects** (§3.7 makes a token's reach exactly its creator's at that
  scope), and the instance root is listed apart (§3.9) — omitting either makes the view
  reassure without grounds.
- Guarded in two halves: the human rows need `members:read`, the token rows need
  `tokens:read`.
- It says nothing about network reach (ADR-042's auth wall), live tunnels or grants
  (ADR-045), or credentials held outside the platform. Platform permissions, no more.

Specified by [ADR-046 §8](../adr/ADR-046-scoped-role-assignments.md); shippable before the
assignments exist, where it truthfully answers "every member of the team".

### 3.10 Consequence for external endpoints (ADR-045)

ADR-045 declares that an external endpoint carries an optional project/environment scope and
that `port-forwards:open` is "evaluated against that endpoint's scope". Today
`endpointInScope` only checks that the endpoint's project belongs to the caller's team —
the field is **declared but not enforced**, and the dashboard exposes it, which is worse than
not having it.

Once §3 exists, the enforcement is one line of the general rule: the endpoint's scope is
`(project_id, environment_id)` per §3.2, and the mint requires `port-forwards:open` **there**.
No special case, which is the point of specifying scoping once for the whole product.

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

### 6.2 Scope hopping (§3.4) — **for the scoped-assignment work**

These tests do not exist yet; they are the acceptance criteria of §3.

- A member scoped `member` on `env=staging` cannot act on `env=production` of the same
  project → `404` on read, `403` on an action they can see.
- A role scoped to `project=X` grants nothing on `project=Y`, including through an indirect
  route: a deployment, a backup, a job, an SSE stream or a preview belonging to Y.
- **Override, not addition**: a `member` on the team who is `reviewer` on `project=payments`
  can no longer deploy in payments.
- **Union at equal scope only**: two roles on the same project accumulate; a role on the
  project never accumulates with the team role it overrides.
- **Team-only permissions are not conferred by a scoped assignment** (§3.3): a member scoped
  `admin` on `project=X` still cannot manage servers, keys, tokens, members or read the audit
  trail.
- **Team-read permissions are conferred team-wide**: the same member can list the servers,
  or they cannot deploy at all.
- **Collections filter**: `GET /projects`, `GET /applications`, `GET /databases`,
  `GET /servers/{uuid}/resources` and the SSE streams never mention a resource outside the
  caller's scopes. This is tested per collection, not once — a single unfiltered list is the
  whole leak.
- A member whose base role is `none` and who holds no assignment sees an empty dashboard,
  not an error.
- **Regression, no-assignment case**: with no scoped assignment anywhere, every existing
  authorization test still passes unchanged. Scoping must be inert until it is used.

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
| **createTeam** | root | **`instance:manage`** — instance-root SESSION only (§3.9): a team is the isolation boundary of every resource, so creating one is an instance-level act. The creator joins as `admin`. |
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

- **79 granular permissions** defined (`domain:action`), of which 76 for the team role model
  and 3 exclusively `instance:*` (instance root). §1.2 and §2 are kept in step with
  `internal/auth/permissions.go`, which is the catalogue in code.
- **4 immutable system roles**: `admin`, `member`, `reviewer` (previews only), `none` (nothing at all) + custom roles
  composable by team admins (ADR-038, replacing ADR-007's owner/developer/viewer); `none` is
  what makes partitioning possible (§3.3).
- Assignment scoped team/project/environment, **the most specific wins** (an override, so a
  narrow scope may *reduce* rights); multi-role accumulation by **union** at equal scope;
  **implicit deny**. **Specified in §3, not implemented** — today a member holds their
  permissions across their whole team, and the team is the only isolation boundary.
- API tokens = **intersection** (token perms ∩ creator's RBAC perms re-evaluated at use time);
  creation via `tokens:create` (admin+) with **mandatory anti-elevation guard**; **automatic
  revocation on loss of rights (proposed default)**.
- Sensitive actions (root terminal, restore onto non-empty database, deletion with volumes, CA rotation)
  = permission + reinforced confirmation + audit.
