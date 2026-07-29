# ADR-046 — Role assignments scoped to a project or an environment

- **Status**: Accepted
- **Date**: 2026-07-28
- **Implements**: [ADR-007](ADR-007-fine-grained-rbac-project-environment.md) — the scoped
  half of it, decided in July 2026 and never built
- **Completes**: [ADR-038](ADR-038-roles-model.md) (system roles and granular enforcement,
  which stopped at the team level)
- **Updates**: [rbac-matrix.md](../specs/rbac-matrix.md) §3 (the specification this ADR
  decides), §2 (a fourth system role)
- **Related PRD sections**: §15, §16.3, §23.1, §27.7, §29.7

## Context

Inside a team, a member holds their permissions on every project of that team. There is no
way to say "Alice works on billing" or "deploy to staging, not to production", and the only
isolation boundary a running instance has is the team itself (§23.1).

This is not what the house decided. ADR-007 chose fine-grained RBAC *per project and per
environment*; ADR-038 replaced the role set and made the granular `domain:action`
permissions the real unit of evaluation — but assignment stayed on `team_memberships`, which
has exactly one row per member per team. The permission model is granular in *what* it
grants and coarse in *where*.

The gap is not merely missing work, it is a documented promise. `rbac-matrix.md` §3 has
described assignment at three scopes, most-specific-wins, since it was written; ADR-045
declares that an external endpoint carries a project scope and that `port-forwards:open` is
"evaluated against that endpoint's scope", and the dashboard now shows that field on a form.
An operator reading either document concludes that a boundary exists. `endpointInScope`
checks that the endpoint's project belongs to the caller's team and returns true. **A control
that is documented, displayed, and absent is worse than one that was never promised**, and
that is the state we are in today.

The workaround available now is one team per perimeter. It works — teams are genuinely
isolated — and it is expensive: servers, SSH keys, git sources, registries and S3 storages
are team-scoped, so each perimeter re-declares and re-maintains its own infrastructure, a
person on three perimeters juggles three teams, and nobody has a cross-perimeter view. It is
the right answer for two clients who must never see each other; it is the wrong answer for
two squads of the same company.

## Decision

### 1. A base role on the membership, exceptions in their own table

Every member keeps **exactly one base role**, where it already lives — `team_memberships`
(`role` + `custom_role_id`). Narrower assignments are **exceptions** to it, in a new table:

```
role_assignments(id, uuid, team_id, user_id,
                 role,               -- system role, XOR custom_role_id
                 custom_role_id,
                 project_id,         -- XOR environment_id
                 environment_id,
                 created_by, created_at, updated_at)
```

- CHECK: exactly one of (`role`, `custom_role_id`).
- CHECK: exactly one of (`project_id`, `environment_id`) — the team level is the membership
  row, never a row here with two NULLs.
- `UNIQUE NULLS NOT DISTINCT (user_id, project_id, environment_id, role, custom_role_id)`.
  `NULLS NOT DISTINCT` is load-bearing: a plain UNIQUE lets rows whose `custom_role_id` is
  NULL duplicate freely, because NULL never equals NULL (same reasoning as
  `00024_notifications.sql`).
- `ON DELETE CASCADE` from team, user, project, environment and custom role.

**Why not fold the team level into the same table.** It would be more uniform and it would
cost three things: the "last admin cannot be demoted" guard would become a query over two
places, the members list — one of the most-read screens — would need a join to say what
someone is, and "member with no row" would become a state the code has to interpret. Every
member having a base role is an invariant worth keeping cheap.

### 2. `none` — the empty base role

A fourth immutable system role, holding **no permission**. It is the brick without which
none of this partitions anything: to restrict somebody *to* a project, their team-level role
must grant nothing, and `member` grants everything while `reviewer` still sees every preview
of the team.

A member whose base role is `none` and who holds no assignment sees an **empty dashboard,
not an error** — they are a member of the team who has not been given anything yet, which is
a legitimate state (the day after an invitation, for instance) and must read as one.

`none` never satisfies the last-admin guard, and the enum value is added expand-only
(`ALTER TYPE … ADD VALUE`, never used in the same transaction).

**Joining a team lands on `none`.** Both doors into a team default to it: an invitation
(`invitations.role`, today `member` by default) and SCIM provisioning (`scim.go`, today
`TeamRoleMember`). Someone arrives holding nothing and is given scopes deliberately.

This is a **behavior change for existing instances** and it is the intended one: with the
current default, every arrival opens a window — minutes if an admin is watching, days
otherwise — during which the newcomer reaches every project of the team, and that window is
precisely what partitioning exists to close. It also aligns the provisioning path with the
deprovisioning one that SCIM already implements: an access lifecycle that grants broadly and
revokes precisely is not a lifecycle.

Two consequences worth stating rather than discovering. An invitation now needs a follow-up
act — the assignment — so the invitation UI must say so, or admins will read the empty
dashboard as a bug in AkerDock. And SCIM group membership (`systemRoleGroups`) keeps mapping
to system roles at team level; **scoped groups are out of scope** (§12), so an IdP-driven
per-project access still ends in a manual assignment. Instances that want the old behavior
keep it by assigning `member` at the team level, which is exactly what they have today.

### 3. The most specific scope wins, and it may reduce

Scopes are ordered `team ⊃ project ⊃ environment`. For an action on resource `r`, let `S(r)`
be the scopes covering `r`, ordered environment → project → team:

```
perms(subject, r) = ⋃ { role.permissions | assignment(subject, role, s) }
                    where s = the first scope in S(r) that carries at least one assignment
                  ∪ { team-read permissions of every role the subject holds anywhere }
```

- The narrowest scope carrying an assignment **replaces** the broader one — it does not add
  to it. A `member` on the team who is `reviewer` on `project=payments` cannot deploy in
  payments. This is what makes "everything except production" expressible without demoting
  the person everywhere else, and it is the single most requested shape of this feature.
- **Union applies at equal scope only**: several roles on the same project accumulate; a
  project role never accumulates with the team role it overrides. Union across scopes would
  make the override meaningless, since the broader role would always win by addition.
- Resolution happens **per operation, against the targeted resource**, never once per
  session: a session is not scoped, an action is.

The cost of "may reduce" is that an admin can lock somebody out of a project by adding an
assignment, which reads as a promotion. The API and the UI therefore state the effective
result of an assignment ("Alice will no longer be able to deploy in payments"), rather than
leaving the semantics to be discovered.

### 4. Not every permission means something at a project scope

Permissions fall into three classes, specified exhaustively in
[rbac-matrix §3.3](../specs/rbac-matrix.md):

- **Scoped** (38) — granted on the resources of the scope only.
- **Team-read** (7) — `team:read`, `members:read`, `servers:read`, `certificates:read`,
  `keys:read`, `sources:read`, `notifications:read`: granted **team-wide** even from a scoped
  assignment, because they are working prerequisites. You cannot deploy an application
  without seeing the server it lands on. They expose no secret — the sensitive half lives in
  `*:reveal` / `*:credentials`, which are not in this class (INV-003).
- **Team-only** (33) — never conferred by a scoped assignment: team/member/role/token
  administration, infrastructure mutation, `audit:read`, `secrets:reveal`,
  `databases:credentials`, `terminal:root` (a shell on a *server*, which no project owns),
  `instance:*`.

Assigning a role containing team-only permissions at a project scope is **not an error** —
it grants the scoped subset — but the API MUST report what was not conferred, or an admin
will believe they delegated something they did not.

**`projects:create` is split out of `projects:manage`.** Creating a project is the one
mutation with no scope to be evaluated against — the project does not exist yet, so the
permission can only come from the team level. Rather than carve a special case into the
general rule ("`projects:manage` counts only when held at team level"), the capability gets
its own name: **`projects:create`, team-only**, while `projects:manage` stays scoped and
means "rename or delete *this* project". A project lead can then run their project without
being able to create new ones, which is the distinction the split buys beyond mere tidiness.
The catalogue goes from 78 to 79 permissions.

**Notification rules are filtered, the channels are not.** `notifications:read` is team-read:
knowing which channels exist is how someone asks for a rule at all, and a channel carries no
project name. The *rules* do carry `project_id`/`environment_id` (`00024_notifications.sql`),
so listing them unfiltered would publish the names of projects a scoped member must not see —
the leak §5 exists to prevent, through the one collection nobody thinks of as a resource
list. Rules are therefore filtered by scope like any other collection.

The classification must stay **exhaustive**: a permission added to `auth.Catalog` without a
class is a permission whose behavior under scoping nobody decided. A test walks the catalogue
against the table.

### 5. Out of scope is invisible, and collections filter

- A resource of **another team** → `404` (INV-002, unchanged).
- A resource of this team the caller lacks the domain's `:read` for **at the scope covering
  it** → `404`. This departs from "404 only across teams", deliberately: the point of scoping
  someone out of `project=payments` is that payments is not part of their world. A `403`
  would answer precisely the question the boundary exists to refuse — and project names are
  half of what an internal leak is about.
- A resource the caller **can read but not act on** → `403`, as today.

**Collections filter, they do not merely guard.** `GET /projects`, `GET /applications`,
`GET /databases`, `GET /servers/{uuid}/resources`, search and every SSE stream return only
what the caller's scopes cover. A list endpoint that returns everything and relies on the
detail endpoint to say no has already leaked the inventory. This is the largest piece of
work in this ADR and the one most likely to be forgotten in a handler added later, which is
why the resolution helper must be **the only way** to load a scoped collection.

### 6. Where the check happens: in the resolvers

`require(w, r, perm)` tests a **flat set of permissions computed once at authentication**
(`session.go`) and knows nothing about the resource being touched. Scoping does not fit that
shape, and the choice of where to put the missing check decides whether this ADR is
implementable or a slow leak of forgotten endpoints.

The check goes **into the `resolve*` helpers**, not into `require`:

- `require(perm)` keeps its meaning — "holds this permission *somewhere*" — and stays a
  cheap pre-filter over the union of the caller's roles. The ~200 call sites are untouched.
- `resolveApplication`, `resolveDatabase`, `resolveProject` and their ~30 siblings already
  load the row, already own the team-boundary 404, and are the **only** way a handler gets a
  resource. They are therefore where the scope is derived (§3.2) and the permission
  re-evaluated at that scope, answering `404` when the caller cannot see it.
- Collections (§5) go through the same helper for their filter clause, so a list and a detail
  can never disagree about what exists.

The reason is mechanical, not aesthetic: **a handler that forgets the scoped check cannot
compile**, because it has no resource without calling a resolver. A `requireOn(perm, scope)`
sprinkled by hand across 200 handlers would be more readable one line at a time and would
leak the first time someone adds an endpoint and forgets it — and forgetting is the normal
case, not the exceptional one.

Consequence to accept: `Identity` stops carrying the answer and starts carrying the
*material* — the base role plus the caller's assignments, loaded once per request. The
resolution itself is pure and testable in isolation, which is what makes §9's reverse reading
the same code rather than a second implementation that reads "about the same".

### 7. Tokens narrow with their creator

§4.2 of the matrix is unchanged in principle and gains one word — the intersection is
evaluated **at the scope of the targeted resource**:

```
perms_effective(token, r) = perms_token(token) ∩ perms_RBAC(creator, r)
```

A token created by a member scoped to `project=billing` reaches billing and returns `404`
elsewhere, and it narrows on its own the day the creator's assignment narrows, because the
creator's permissions are re-evaluated on every request rather than frozen at creation.

This had been specified since rbac-matrix §4.2 and was **never built**: a token carried its
own scopes and nothing else, so it outlived the authority that produced it — and would have
been the side door out of every boundary this ADR draws (mint a token, get the whole team
back). It is implemented here (`Middleware.boundToCreator`). Two consequences worth stating:
a creator who leaves the team empties their tokens rather than leaving them running, and a
token with **no creator on record** — minted before the column existed, or by the bootstrap —
keeps its own permissions, because there is no authority to intersect with and breaking those
instances would punish them for our history. The access view marks those rows.

### 8. Assigning is an admin act

- Creating, changing or deleting an assignment requires **`members:manage`**, which is
  team-only in v1: only a team admin assigns.
- An assigner may never grant a role whose permission set exceeds their own at the target
  scope — the same anti-elevation rule as custom-role composition
  (`auth.ValidateCustomPermissions`), applied to assignment rather than authorship.
- The **last admin** guard stays team-level: the last admin can be neither demoted nor
  reduced to `none`, or a team locks itself out of its own instance.
- Every assignment change is audited with actor, subject, role and scope. "Who could reach
  production last March" is an audit question, and it is answerable only if the assignment
  history is in the trail (§23.4) — the current state of a table answers "who can", never
  "who could".

### 9. The access view: the same resolution, read backwards

Scoping is only as good as an operator's ability to check it. The resolution of §3 answers
"may this subject act on this resource"; an access review asks the same question with the
subject unbound. Both readings come from **one function**, evaluated on demand:

- **Per resource** — `GET /{applications|databases|services|projects|environments}/{uuid}/access`
  returns who can reach it. Available as an **Access tab** on applications, databases,
  services, projects and environments — the five places where the question is actually asked.
- **Per subject** — `GET /teams/{uuid}/members/{user_uuid}/access` returns the scopes a member
  holds and what each one covers. This is the offboarding screen: "what does Alice reach"
  is the question asked the day Alice leaves, and answering it by opening resources one by
  one is how something gets missed.

**Computed, never stored.** No table, no cache, no `last_reviewed_at`. A denormalized copy of
who-can-see-what is a copy that drifts, and a stale access review is worse than none: it is
an assertion of safety with nothing behind it. The cost is bounded — per resource it is one
pass over the team's members with their assignments loaded in a single query; per subject it
is a walk of that subject's own assignments.

**Each row states the path, not just the name.** "Bob" is not actionable; "Bob — member on
`project:billing`" tells the reviewer exactly what to change. A row carries the subject, the
role, the scope that granted it, and the capability summary that matters at review time —
can see / can deploy / can read secrets / can open a terminal.

**API tokens are subjects.** A token is a real, durable and rarely watched access, and a
review that omits it reassures without grounds. Tokens are listed with their creator, their
coarse scopes and their expiry. §7 makes this exact rather than approximate: a token reaches
a resource only if its creator does, so a token row is always the intersection of the two and
must be read as derived — revoking Bob's assignment narrows Bob's tokens on the next request.

**Two different permissions guard the two halves of the view.** Seeing *who else* can reach a
resource you already read needs `members:read` — it is the "who do I ask" question, and
hiding it protects nothing. Seeing the **token** rows needs `tokens:read`, which is admin-only
(§2 of the matrix): a token's existence, owner and expiry is administrative information, not
team small talk.

**The instance root appears, set apart.** It reaches every resource of every team (§3.9), so
omitting it would make the view lie by exactly the account that matters most in an audit. It
is listed as what it is — an instance-level actor — never blended into the team's members.

**What this view does not answer**, stated because an "Access" tab invites the wider reading:
it lists who holds *platform* permissions on the resource. It says nothing about who can
reach the deployed application over the network (that is ADR-042's auth wall), who holds a
live tunnel or grant (ADR-045, listed on their own screens), or who has the credentials of a
database the platform merely deploys. Access review at the platform layer, no more.

**Periodic attestation is deliberately out.** No campaign, no quarterly cycle, no
`reviewed_at` (§12). The view is the substrate such a control would sit on, and it can be
added later without rewriting any of this — but a "mark as reviewed" button whose meaning
nobody defined is a checkbox that produces an audit artifact and no security.

**This view is shippable before the assignments are.** Run against today's model it answers
truthfully — "every member of the team, plus these tokens" — which is worth having on its
own: it makes the *absence* of partitioning visible to the people who assumed otherwise. It
then becomes the acceptance test of the rest of this ADR: after slice 1, the same screen must
show a shorter list.

### 10. Relational storage, path-shaped API

Scopes are stored as **typed foreign keys** (`project_id`, `environment_id`), not as a
hierarchical path such as `team.project.*`. The path notation is attractive — one matching
rule by prefix, a new level added without a migration — and it is rejected for one reason
that outweighs both: **a UUID inside a text column is not a foreign key**. Deleting a project
would leave an assignment matching nothing, or worse, matching something else later; the
cascade that keeps this table honest comes free with the FK and would have to be
reimplemented, in application code, forever. The hierarchy has three levels and has been
stable since ADR-007; paying integrity for an extensibility we do not need is the wrong
trade.

What the path notation *is* good for is naming a scope, so the **API and the audit trail
speak it** while storage stays relational:

```
team                                        the base role
project:9f3c…                               a project assignment
project:9f3c…/environment:41ab…             an environment assignment
```

One canonical string, sortable by specificity (longer prefix = narrower), readable in an
audit row without a join. If a fourth level ever appears, the notation absorbs it and the
schema takes one column.

### 11. Rollout in four slices

1. **The access view first** (§9) — `/access` on the five resource kinds and on a member,
   plus the Access tab. It ships against today's team-wide model, tells the truth about it,
   and becomes the acceptance screen for everything below: after slice 2, the same tab must
   show a shorter list. Nothing else in this ADR is a prerequisite.
2. **Foundation** — the table, the `none` role, `projects:create`, the resolution helper and
   the scoped check inside the `resolve*` helpers (§6), **including collection filtering**
   (notification rules included). No API to create an assignment yet: the slice is inert and
   provably so.
3. **Surface** — `GET/POST/DELETE /teams/{uuid}/role-assignments` (filterable by user and by
   scope), the effective-permissions endpoint returning permissions *per scope* so the UI can
   hide what it must, and the assignment UI in the members screen.
4. **Convergence** — invitations and SCIM defaulting to `none` (§2), `endpointInScope`
   (ADR-045) evaluated against the endpoint's real scope, assignment events in the audit
   trail, and the matrix's §6.2 test suite.

The default-to-`none` change sits in the last slice on purpose: it is the only piece that
alters the behavior of an instance that never asked for scoping, and it should land once the
assignment UI exists — otherwise an admin invites somebody and has no way to give them
anything.

Slice 1 before slice 2 is deliberate: an operator who can *see* that everyone reaches
everything is better served than one who is told partitioning is coming, and building the
observer before the mechanism keeps the mechanism honest.

**Inertia is a requirement, not a hope**: with no assignment anywhere and no member set to
`none`, every existing authorization test must pass unchanged. Scoping that alters behavior
before anyone uses it is a migration, and this is not one.

### 12. Out of scope for v1

Scoped delegation (`roles:manage` at project scope — delegating the power to delegate needs
its own guard rails and its own ADR); **periodic access attestation** (campaigns, reminders,
`reviewed_at`, auditor export — §9 is the substrate it would sit on, and it can be added
without rewriting any of it); partitioning the audit trail per scope (§23.4 is team-wide and
un-partitioned, which is why `audit:read` is team-only); assignment at resource granularity
(a single application); scoped SCIM/group provisioning; scoping infrastructure itself (a
server belongs to the team, and a server "scoped to a project" is a lie the moment a second
project deploys onto it).

## Alternatives considered

- **A hierarchical scope path (`team.*`, `team.project.*`), text or `ltree`**: rejected —
  loses referential integrity and cascade (§10). Kept as the API/audit *notation*.
- **A materialized who-can-see-what table behind the access view**: rejected — it would make
  §9 a lookup instead of a computation, and a denormalized access table is one that drifts
  from the rules it claims to summarize. A review reading a stale copy is worse than no
  review, because it asserts safety. Revisit only if the on-demand computation is measured
  to be too slow, which a pass over a team's members is not.
- **Shipping the access view only after the assignments exist**: rejected — the view has
  standalone value today (it makes the absence of partitioning visible) and it is the
  acceptance test of the rest (§11).
- **Additive scopes (a scoped role can only add)**: rejected — simpler to reason about and
  incapable of expressing the main use case. "Everything except production" would require
  demoting the person to `none` and re-granting every other project by hand, which is the
  same permission set expressed with more moving parts, and it silently rots as projects are
  added.
- **Per-resource ACLs**: rejected — the model would leave "a role is a named set of
  permissions", the audit surface would explode, and the recurring request is per project,
  not per application.
- **One team per perimeter (the status quo)**: rejected as the general answer, kept as the
  right one for genuine multi-tenancy. It duplicates infrastructure and has no cross-team
  view; see Context.
- **`403` instead of `404` outside the scope**: rejected — a clearer message for the user,
  bought by publishing the list of project names to everyone in the team (§5).
- **Everything in `role_assignments`, including the team level**: rejected — costs the
  last-admin guard, the single-query members list, and introduces "member with no role" as a
  state (§1).
- **Waiting for a group/team-hierarchy model**: rejected — it solves a different problem
  (who is in what) and would not remove the need for scopes; nothing here blocks it later.

## Consequences

- **Positive**: the boundary the product has documented since ADR-007 becomes real; a squad
  can be restricted to its projects without duplicating servers, keys and sources into a
  second team; "deploy to staging but not production" becomes expressible; ADR-045's endpoint
  scope stops being a field that suggests a protection it does not provide; tokens inherit
  the narrowing for free (§7); the audit trail gains "who could reach what, when"; and the
  access view (§9) makes the whole thing **checkable by the person accountable for it**,
  which is the difference between a control and a belief — directly useful to the SOC 2 /
  ISO 27001 posture, and useful on day one since it ships before the partitioning does.
- **Negative**: one table, one enum value, one resolution path in every read; **every
  collection endpoint must filter**, which is broad, mechanical and easy to forget in a
  handler written six months from now — the mitigation is that the helper is the only
  sanctioned way to load a scoped collection, not a convention; the UI gains an assignment
  screen and must explain a semantics ("this will reduce Alice's rights here") that a
  checkbox does not convey; four slices to ship before the feature is usable end to end; the
  access view adds an inverse resolution to maintain alongside the forward one — the two must
  never disagree, which is an argument for both going through the same helper rather than a
  second implementation that reads "about the same".
- **Accepted risks**: an admin can lock a colleague out of a project by adding what looks
  like a grant (§3) — mitigated by stating the effective result, not by refusing the case;
  `audit:read` staying team-only means a scoped member sees the whole team's trail or none of
  it, and we chose none of it; team-read permissions mean a scoped member can enumerate the
  team's servers, keys (metadata) and git sources — deliberate, since without them they
  cannot deploy, and the sensitive half stays behind `*:reveal`.

## Verification

Unit level (ADR-028 — no new E2E journey). The acceptance criteria are
[rbac-matrix §6.2](../specs/rbac-matrix.md), notably: a member scoped to `env=staging`
cannot act on `env=production` (404 on read, 403 on a visible action); a project role grants
nothing on another project, including through a deployment, a backup, a job, an SSE stream or
a preview; override reduces (team `member` + project `reviewer` cannot deploy there); union
applies at equal scope and never across scopes; team-only permissions are not conferred by a
scoped assignment while team-read ones are conferred team-wide; every collection endpoint
filters (tested per collection — one unfiltered list is the whole leak); a `none` member with
no assignment gets an empty dashboard rather than an error; a token created by a scoped
member reaches only that scope and narrows when the creator's assignment narrows; assignment
is refused when it would exceed the assigner's own permissions at the target scope; the last
admin can be neither demoted nor reduced to `none`; every assignment change is audited with
its scope; and the catalogue-to-class table is exhaustive.

For the mechanics (§6): a handler that resolves a resource outside the caller's scope gets
`404` from the resolver itself, and the same helper backs the list filter, so a resource
absent from a collection is never reachable by its detail endpoint. For the arrival path
(§2): an invitation accepted with no assignment yields a member who can sign in and sees an
empty dashboard, not an error; a SCIM-provisioned user lands on `none`; and neither path can
produce a team-wide `member` by omission. For `projects:create` (§4): a member scoped
`projects:manage` on a project can rename it and cannot create another; the split is refused
at team level for anyone lacking `projects:create`. For notification rules (§4): a scoped
member lists the team's channels and only the rules of the projects they can see.

For the access view (§9): the per-resource answer matches the forward resolution for every
listed subject (property-style — for each row, `may(subject, resource)` agrees with the row's
claim, which is what keeps the two readings from drifting); a member outside the scope is
absent from the list, not shown as denied; each row names the scope that granted it; token
rows appear only for a caller holding `tokens:read` and only for tokens whose creator reaches
the resource; a token whose creator's assignment narrows disappears on the next read; the
instance root is listed apart and never as a team member; the per-member view lists exactly
the scopes that member holds; and the view stays correct with no assignment at all, where it
must answer "every member of the team". Plus the inertia test of §11: with
no assignment anywhere, the existing authorization suite passes unchanged.
