# RBAC / Permissions Matrix — AkerDock (artifact §29.7)

> ⚠️ **Role model updated by [ADR-038](../adr/ADR-038-roles-model.md)**
> (supersedes the roles part of ADR-007). Team roles = **`admin` / `member` /
> `reviewer`** + **custom roles**; `owner` is merged into `admin`; the **root
> is reserved to the instance** (`users.is_root`, outside the team model). ADR-038 also
> records that the **granular `domain:action` permissions in this document
> become the actual unit of evaluation** (today enforcement is coarse
> and the granular level is documentation-only): each operation will carry a
> granular `x-required-permission`, with a **prerequisites table** (§3 ADR-038)
> and transitive closure. The `owner / developer / viewer` columns below
> are replaced by `admin / member / reviewer` (+ custom) and **regenerated** at
> implementation time.

> Authorization specification document (artifact §29.7 of the PRD, `docs/PRD.md`).
> Reference decision: **ADR-007 / §27.7** — fine-grained RBAC, **à la carte permissions** model:
> each product action produces a granular `domain:action` permission; a role is a
> named set of permissions, assignable at the **team, project or environment** level
> (the most specific scope wins). Immutable system roles: **owner, admin, developer,
> viewer** (strictly read-only); custom roles composable by team admins.
>
> Consistency: the §10.3 API token permissions (`read`, `read:sensitive`, `write`,
> `deploy`, `root`) remain the per-action evaluation baseline (§24.1) and are **mapped**
> onto this granular model (§4 + §7). The OpenAPI `x-required-permission` values become
> a projection of these granular permissions (mapping table §7).
>
> Defaults proposed beyond parity are marked **(proposed default)**.

---

## 1. Model

### 1.1 Granular permissions

A permission is named `domain:action`. It represents **one atomic product capability**.
It is **positive only** (no negative permissions): the absence of a permission means
**implicit deny** (§3.4).

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

### 1.2 Complete list of permissions (74)

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
| 55 | `previews:manage` | Manage previews, approve a fork PR (§20.4.8) | `write` |
| 56 | `templates:manage` | Register/sync template repos (§27.10) | `write` |
| 57 | `terminal:open` | Open a container/server terminal (non-root) | `write` |
| 58 | `terminal:root` | Open a **root** terminal (dual control §5) | `write` |
| 72 | `port-forwards:open` | Open a TCP tunnel to a resource's container or to a declared external endpoint (CLI, ADR-032/ADR-045) — boundary at resource granularity; on an external endpoint it is evaluated against that endpoint's scope | `write` |
| 73 | `external-endpoints:read` | List the team's declared external endpoints (bastion targets, ADR-045) | `read` |
| 74 | `external-endpoints:manage` | Declare/update/delete an external endpoint — draws a network boundary, admin-level (ADR-045) | `write` |
| 59 | `logs:read` | Container runtime logs | `read` |
| 60 | `logs:manage` | Configure log drains | `write` |
| 61 | `metrics:read` | Server/resource metrics, uptime | `read` |
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

> **Total: 72 granular permissions** (of which 3 are exclusively `instance:*`, reserved to the
> instance root, outside the team role model). The "team product" baseline covers 68 permissions,
> i.e. within the §29.7 target range (~40-60), widened to cover the full scope of the PRD.

---

## 2. Permissions × system roles matrix

> Legend: ● = granted; ○ = not granted. The 4 system roles are **immutable** (§3.4).
> `owner` and `admin` differ only on administration of the team itself
> (team deletion, role management, owner removal). `viewer` is **strictly read-only**:
> **no mutation, no secrets** (INV-003).

| Permission | owner | admin | developer | viewer |
|---|:---:|:---:|:---:|:---:|
| team:read | ● | ● | ● | ● |
| team:manage | ● | ○ | ○ | ○ |
| members:read | ● | ● | ● | ● |
| members:manage | ● | ● | ○ | ○ |
| invitations:manage | ● | ● | ○ | ○ |
| roles:read | ● | ● | ● | ● |
| roles:manage | ● | ● | ○ | ○ |
| tokens:read | ● | ● | ● | ○ |
| tokens:create | ● | ● | ○ | ○ |
| tokens:revoke | ● | ● | ○ | ○ |
| projects:read | ● | ● | ● | ● |
| projects:manage | ● | ● | ● | ○ |
| environments:read | ● | ● | ● | ● |
| environments:manage | ● | ● | ● | ○ |
| resources:read | ● | ● | ● | ● |
| resources:adopt | ● | ● | ● | ○ |
| environments:deploy | ● | ● | ● | ○ |
| applications:read | ● | ● | ● | ● |
| applications:create | ● | ● | ● | ○ |
| applications:update | ● | ● | ● | ○ |
| applications:delete | ● | ● | ● | ○ |
| applications:deploy | ● | ● | ● | ○ |
| applications:lifecycle | ● | ● | ● | ○ |
| applications:exec | ● | ● | ● | ○ |
| databases:read | ● | ● | ● | ● |
| databases:create | ● | ● | ● | ○ |
| databases:update | ● | ● | ● | ○ |
| databases:delete | ● | ● | ● | ○ |
| databases:lifecycle | ● | ● | ● | ○ |
| databases:credentials | ● | ● | ● | ○ |
| services:read | ● | ● | ● | ● |
| services:manage | ● | ● | ● | ○ |
| services:deploy | ● | ● | ● | ○ |
| secrets:read | ● | ● | ● | ● |
| secrets:reveal | ● | ● | ● | ○ |
| secrets:write | ● | ● | ● | ○ |
| servers:read | ● | ● | ● | ● |
| servers:manage | ● | ● | ○ | ○ |
| servers:maintain | ● | ● | ○ | ○ |
| servers:proxy | ● | ● | ○ | ○ |
| certificates:read | ● | ● | ● | ● |
| certificates:renew | ● | ● | ○ | ○ |
| keys:read | ● | ● | ● | ● |
| keys:reveal | ● | ● | ○ | ○ |
| keys:manage | ● | ● | ○ | ○ |
| sources:read | ● | ● | ● | ● |
| sources:manage | ● | ● | ● | ○ |
| registries:manage | ● | ● | ● | ○ |
| cloud:read | ● | ● | ○ | ○ |
| cloud:manage | ● | ● | ○ | ○ |
| storages:manage | ● | ● | ● | ○ |
| backups:read | ● | ● | ● | ● |
| backups:manage | ● | ● | ● | ○ |
| backups:restore | ● | ● | ● | ○ |
| deployments:read | ● | ● | ● | ● |
| deployments:cancel | ● | ● | ● | ○ |
| jobs:manage | ● | ● | ○ | ○ |
| previews:manage | ● | ● | ● | ○ |
| templates:manage | ● | ● | ● | ○ |
| terminal:open | ● | ● | ● | ○ |
| terminal:root | ● | ● | ○ | ○ |
| port-forwards:open | ● | ● | ● | ○ |
| external-endpoints:read | ● | ● | ● | ○ |
| external-endpoints:manage | ● | ● | ○ | ○ |
| logs:read | ● | ● | ● | ● |
| logs:manage | ● | ● | ● | ○ |
| metrics:read | ● | ● | ● | ● |
| notifications:manage | ● | ● | ● | ○ |
| audit:read | ● | ● | ● | ○ |
| config:export | ● | ● | ● | ● |
| config:apply | ● | ● | ● | ○ |

> Design notes:
> - **developer** = the "Member/Developer" and "Operator/SRE" actor (§16.3): full application
>   power (create/deploy/backup/restore/non-root terminal) but **no** administration
>   of sensitive infrastructure (servers, SSH keys, cloud, root terminal, member/role management).
>   Finer-grained deployment (e.g. "deploy to staging but not production") is done via
>   **environment-scoped role assignment** (§3.1) — the `developer` system role assigned
>   to `env=staging` only.
> - **viewer** = "read-only/MCP integration" and audit: no mutation, `secrets:reveal` denied,
>   `databases:credentials`/`keys:reveal` denied (INV-003). `config:export` allowed because it
>   never contains inline secrets (§24.5).
> - **certificates:read** is granted to `viewer`: the expiration inventory (domains,
>   `not_after`, status) contains no secret — the private key material never leaves
>   the server — and read-only monitoring is precisely the viewer/MCP use case,
>   consistent with `servers:read` and `metrics:read` (INV-003 respected). **certificates:renew**
>   is aligned with `servers:maintain` (admin+): a forced renewal touches the server's
>   infrastructure (editing `acme.json`, restarting the proxy) and consumes Let's Encrypt quota.
> - **jobs:manage** (dead-letter retry/forget) is reserved to admin+: forget can abandon
>   a deletion leaving remote remnants (§20.6.4); replay through the business channel (deploy,
>   backup, server validation) remains accessible to the developer via their existing permissions.
> - The `instance:*` permissions (66, 67, 71) do not appear in team roles: they
>   are carried exclusively by the **instance root** (§3.5).

---

## 3. Resolution rules

### 3.1 Assignment and scopes

A role is assignable at three levels, from most general to most specific:

```
team  ⊃  project  ⊃  environment
```

An assignment = `(subject, role, scope)` where `subject ∈ {member, custom role}` and
`scope ∈ {team_uuid, project_uuid, environment_uuid}`.

### 3.2 Inheritance (the most specific wins — override, not intersection)

- A **team**-level assignment applies to all its projects and environments.
- A **project**-level assignment applies to all its environments.
- An **environment**-level assignment applies only to that environment.
- **The most specific scope wins**: if a member is `viewer` at the team level but
  `developer` on `project=X`, they are developer on X and reader elsewhere.
- The override is **per assignment set**, resolved at action time for the targeted resource:
  we retain the most specific assignment covering the resource's scope.

### 3.3 Multi-role accumulation (union)

- A member can hold multiple roles (system and/or custom) **at the same scope**: the effective
  set of permissions at that scope is the **union** of their permissions.
- Across different scopes, the most-specific rule is applied first (§3.2), then the union
  within the retained scope.
- Formally, for an action on a resource `r`:
  `perms(subject, r) = ⋃ { role.permissions | assignment(subject, role, scope) ∧ scope = most_specific_covering(r) }`.

### 3.4 Implicit deny and immutability

- **Deny by default**: a permission absent from the effective set is denied. No
  negative permission exists (no exceptions to compose).
- **Immutable system roles**: `owner`, `admin`, `developer`, `viewer` are neither editable nor
  deletable. An admin who wants to deviate creates a **custom role** (composable, §1).
- Denial response: `not_found` for a resource of another team (no oracle, INV-002);
  `403 forbidden` for an intra-team permission denial.

### 3.5 The instance root case

- The **instance root** (`users.is_root`) is outside the team role model: it implicitly
  holds all permissions on all teams **plus** `instance:*` (§10.1).
- It is never an implicit member of a team for audit purposes: its cross-team actions are traced
  with `actor.type=user` + root flag (§23.4).
- A token created by the root is **scoped to a team** like any token (§10.3); the root cannot
  create a "global" token — a `root` token remains bounded to its team (see §4).

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

- Token creation requires the dedicated permission **`tokens:create`** (included in `admin` and
  `owner`, absent from `developer` and `viewer` — see §2). This is the resolution of the point raised by
  the OpenAPI (`createApiToken`, `x-required-permission: write` + guard): `write` alone is not
  conceptually sufficient; the capability is carried by `tokens:create`, reserved to admin+.
- **Mandatory anti-elevation**: the creator can only grant the token permissions
  they **hold themselves** at the targeted scope. Any request for a token scope whose projection
  exceeds `perms_RBAC(creator)` → `403` (consistent with the OpenAPI description of `createApiToken`).
- A `root` token can only be created by a holder of `instance:manage` (instance root) or,
  by extension, an owner/admin for a `root` token **bounded to their team** — never a token with
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

### 6.2 Scope hopping (inheritance §3.2)
- A `developer` scoped to `env=staging` cannot act on `env=production` of the same project.
- A role scoped to `project=X` does not leak to `project=Y`.
- Verify that the most specific wins (override) and that multi-role union does not cross
  scope boundaries (§3.3).

### 6.3 Elevation via token (§4)
- A `developer` creator cannot create a token carrying `write`/`deploy` exceeding their rights
  → `403` (anti-elevation §4.3).
- A token whose creator is downgraded loses the corresponding right on the next request
  (re-evaluation §4.2); **(proposed default)** verify automatic revocation (§4.4).
- A `write` without `tokens:create` cannot create a token → `403` (§4.3).

### 6.4 Each system role × each endpoint family
- `owner`, `admin`, `developer`, `viewer` tested on each family (applications, databases,
  services, secrets, servers, keys, backups, terminal, deployments, cloud, config).
- **viewer**: any mutation → `403`; `secrets:reveal`, `keys:reveal`,
  `databases:credentials`, `terminal:*` → `403` (strictly read-only, INV-003).
- **developer**: `servers:manage`, `keys:manage`, `cloud:*`, `terminal:root`,
  `members:manage`, `roles:manage`, `tokens:create` → `403`.

### 6.5 Sensitive actions (dual control §5)
- Root terminal, restore onto a non-empty database, deletion with volumes, CA rotation: verify
  that in the absence of reinforced confirmation the action is denied even if the permission is
  present.

### 6.6 Consistency with audit
- Each denied/granted authorization action on a sensitive action produces a usable
  audit entry (§23.4).

---

## 7. OpenAPI `x-required-permission` → granular permissions mapping table

> The `x-required-permission` values of `docs/specs/openapi-v1.yaml` remain the per-action
> evaluation baseline (§24.1). This table projects them onto granular permissions: effective
> access control = granular permission below, **and** the token must carry the indicated
> `x-required-permission` scope (both conditions, consistent with §4.2 intersection).

| operationId (OpenAPI) | x-required-permission | Granular permission(s) |
|---|---|---|
| getHealth | (none) | — (public) |
| getVersion | read | `team:read` |
| enableApi / disableApi | root | `instance:manage` |
| listTeams / getTeam | read | `team:read` |
| **createTeam** | root | **`instance:manage`** — instance-root SESSION only (§3.5): a team is the isolation boundary of every resource, so creating one is an instance-level act. The creator joins as `admin`. |
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

- **71 granular permissions** defined (`domain:action`), of which 68 for the team role
  model and 3 exclusively `instance:*` (instance root).
- **4 immutable system roles**: owner, admin, developer, viewer (strictly read-only) + custom
  roles composable by team admins (ADR-007 / §27.7).
- Assignment scoped team/project/environment, **the most specific wins**; multi-role accumulation
  by **union**; **implicit deny**.
- API tokens = **intersection** (token perms ∩ creator's RBAC perms re-evaluated at use time);
  creation via `tokens:create` (admin+) with **mandatory anti-elevation guard**; **automatic
  revocation on loss of rights (proposed default)**.
- Sensitive actions (root terminal, restore onto non-empty database, deletion with volumes, CA rotation)
  = permission + reinforced confirmation + audit.
