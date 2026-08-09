# ADR-068 — A live session is ended by its owner, or by a team admin

- **Status**: Proposed
- **Date**: 2026-08-09
- **Extends**: [ADR-067](ADR-067-tunnels-are-scale-to-zero-citizens.md) §10, which parks
  exactly this question — "whether an administrator may end another member's session from the
  dashboard, under which permission, and what the person on the other end is told" — and
  states that nothing in it waits on the answer. This ADR answers it and adds nothing to the
  cut mechanism §2 built
- **Extends**: [ADR-038](ADR-038-roles-model.md) §4 by one catalogue entry. No system role's
  shape is restated and no role's membership in the model changes; the admin set is computed
  from the catalogue, so adding one permission absent from `member` and `reviewer` is the
  whole change
- **Related**: [ADR-024](ADR-024-realtime-sse-websocket-terminal.md) (the terminal and its end
  reasons), [ADR-032](ADR-032-tcp-tunnel-cli-websocket.md) (the tunnel),
  [ADR-045](ADR-045-external-endpoint-port-forwards.md) §5 (grants, revocation tearing down
  the sessions it opened, and `revoked`'s existing meaning),
  [ADR-058](ADR-058-session-role-inspection.md) §2/§4/§5 (permissions are the intersection of
  what you hold with the role you simulate; the identity is unchanged),
  [ADR-060](ADR-060-dev-ingress-tunnels.md) (the third session family),
  [ADR-064](ADR-064-one-transport-ladder-for-every-cli-tunnel.md) (one transport, so a cut
  must reach every rung), [ADR-065](ADR-065-idempotent-attach-token-claim.md) §5 (the attach
  register a cut rides)
- **Related PRD sections**: §5.7, §10.4, §12, §23.4, §24.4

## Context

ADR-067 §10 left this open on purpose, and was right to: the mechanism was the hard part and
it is done. `TunnelPresence.Cut` and `terminalCut` end a live session from outside, on every
rung of both families, with a reason queued before the teardown so the bridge reports the
cause instead of inferring one. What has never been decided is who is allowed to pull it.

**Part of the surface is already shipped, and an ADR that ignores that is worse than none.**
`GET /port-forward-sessions` lists the team's tunnel sessions and
`DELETE /port-forward-sessions/{uuid}` closes one; ADR-060's ingress family has the same pair.
Both handlers apply the same rule, invented twice at the keyboard and written down nowhere:
if the caller owns the session, close it as `user_close`; otherwise require the domain's
management permission — `external-endpoints:manage`, `ingress-endpoints:manage` — and close it
as `revoked`.

That rule has three defects, and each one is the reason a clause exists below.

**The permission is borrowed.** `external-endpoints:manage` means "may declare a bastion
target": drawing a network boundary, which is why ADR-045 §5 put it at admin level. Most
port-forward sessions target a preview or an application and have no external endpoint
anywhere near them. Using the endpoint-declaration permission to authorize cutting them is a
coincidence of who happens to hold it, not a rule — and the coincidence is imperfect.
`member` does not hold it, so today's behaviour is roughly the intended one; but an admin may
compose a custom role that does (ADR-038 §4), and that role then ends colleagues' sessions
while holding nothing that says so. An authorization rule that is right by accident fails the
first time somebody uses the feature it was borrowed from.

**The terminal family has no surface at all.** `POST .../terminal-sessions` mints a session;
nothing lists them and nothing ends one. So the session kind where the question actually bites
— an interactive shell on a production container, the one session that can do arbitrary damage
while somebody watches — is precisely the one with no answer. An admin who can see a
deployment they must stop can stop it; an admin watching a shell do the same damage can only
go and stop the container underneath it, which is ADR-067's "documented workaround through
another protocol" wearing a new costume.

**The administrative close lies about what happened.** It writes `revoked`, which the CLI
prints as *"tunnel closed: an administrator revoked the access grant"* and the browser as
*"Session revoked by the server."* For a `sensitive` endpoint whose grant was withdrawn that
is true and it is ADR-045 §5's own wording. For every other session it is false in the part
that matters: nothing was revoked, the developer's access is intact, and the message sends
them to ask for a grant back that they never lost. ADR-067 §2 refused exactly this shape of
error when it stopped a vanished container from reading as `revoked` — a wrong reason is worse
than a silence, because the developer acts on it.

There is a fourth thing, found by reading rather than by review, and it belongs here because
it decides whether any of this works: **a cut that lands on the wrong replica currently tells
the developer `disconnect`.** Both bridges converge across replicas through the beat — the row
is finalized, and the replica holding the socket sees zero rows updated and leaves — but both
report `EndDisconnect` when they do, and `internal/terminal`'s own comment on that branch says
so out loud: *"the reason it ended with is on the row"*. It is on the row and it does not reach
the person. On a single-replica instance an administrative cut says the right thing; on a
multi-replica one it reads as a network glitch, which is the failure this whole family of
decisions exists to stop.

## Decision

**A live session may be ended by the person who owns it, or by a team admin and above.
Nobody else.** A member cannot end a colleague's session, whatever else that member may do to
the resource underneath it.

Throughout: *session* means one row of `port_forward_sessions`, `terminal_sessions` or
`ingress_tunnel_sessions` — a tunnel (ADR-032/045), a container terminal or a server shell
(ADR-024), an ingress attach (ADR-060). *End* means the durable row is finalized with an end
reason and the live attach is cut wherever it is held.

### 1. Every session family, including the server shell

The rule covers all three families and every kind within them. This is said rather than left
to be inferred from the mechanism being shared, because the families acquired their surfaces
at different times and one of them has none.

- **Port-forward tunnels** keep the endpoints they have; only the authorization and the end
  reason change (§2, §5).
- **Container terminals** gain the listing and the close they never had (§4).
- **Ingress tunnel sessions** take the same rule, so one sentence governs three tables.
- **Server shells are included.** ADR-067 excluded them from every one of its clauses, and
  the exclusion was sound for the reason it gave: a server shell has no container, no
  resource and no scale-to-zero clock, so there is nothing to keep awake and nothing to wake.
  None of that reasoning survives the move to *ending* a session. A root shell as `ssh_user`
  on a production host is the session an administrator most needs to be able to end, and it
  is a `terminal_sessions` row like any other.

**Ending a server shell requires no passkey step-up.** The ceremony on `terminal:root` gates
*acquiring* the power; ending a session removes access and grants nothing, so there is nothing
for a second factor to protect. This is ADR-058 §5's shape — entering the mode is gated,
leaving it is unconditional, because restoring or removing authority is not the same act as
taking it.

### 2. Two tests of two different kinds, because "mine" is not a permission

The rule is a disjunction, and the honest statement is that its two halves cannot be folded
into one check. The permission system speaks in verbs and resources; ownership is a fact about
a row. Pretending otherwise would mean inventing a permission that means "on things that are
yours", which is not a permission but a predicate.

**The owner half is a row test**: the session's `user_id` equals the acting user's. Two
consequences, both already correct in the shipped tunnel handler and both worth keeping:

- **An API token owns nothing.** A token has no user, so a token-authenticated call is always
  ending somebody else's session and always takes the authority half. `ownsPortForwardSession`
  says this today and it is right.
- **A session with a NULL `user_id` is owned by nobody**, and is reachable only by the
  authority half.

**The authority half is a permission**, and the interesting part is that it turns out to be
expressible after all — but not by any of the routes a reader would try first.

- `Identity` carries **no role**. There is no "is admin" bit to read: `Identity.IsRoot()` is
  the *instance* root's coarse wildcard, appended only for `membership.IsRoot`, and a team
  admin does not carry it — `TeamAdminPermissions()` is every catalogue entry whose socle is
  not `root`, so the string `root` is never in an admin's set. Checking `IsRoot()` here would
  restrict the act to the platform administrator, which is not the rule.
- Reading `team_memberships.role` directly, as `MayInspectRoles` does, is available and is the
  wrong tool. §3 explains why, and the reason is not stylistic.
- Comparing permission *sets* — "holds everything a team admin holds" — does not work either:
  a maximal custom role is indistinguishable from `admin` by its permissions, which is by
  design.

So: **a new granular permission, `sessions:manage`, socle `write`.** ADR-038's model does not
have "admin" as something you check; it has "admin holds everything the catalogue offers below
`instance:*`", computed. Adding one catalogue entry that is present in that computed set and
absent from the explicit `memberPermissions` and `reviewerPermissions` lists **is** the rule
"team admin and above", stated in the only vocabulary the enforcement layer has.

- **One permission for both families**, not `terminal:kill` beside `port-forwards:close`. The
  reviewer's answer is one rule, and ADR-067 already refused to let the door decide the
  permission: "the permission that starts an application does not depend on which door asked".
  The permission that ends a session does not depend on what the session carries.
- **The prerequisite closure adds nothing**, and this is deliberate rather than an oversight a
  later reader should fix. `Prerequisites` adds `<domain>:read` for a non-read action only when
  that permission exists in the catalogue; there is no `sessions:read`, because the listings
  ride each family's own open permission (§4). If a `sessions:read` is ever added, it will
  silently change what every role holds — hence the test in Verification that pins the closure
  as empty.
- **A custom role may be granted it**, by an admin who holds it, capped by
  `ValidateCustomPermissions`' existing anti-elevation. So "team admin and above" means in
  practice "an admin, the instance root, or somebody an admin deliberately handed this to" —
  which is what every admin-level permission in this model already means, and it is a
  deliberate, named, auditable grant rather than the accident it replaces.

The three shipped handlers are re-pointed from `external-endpoints:manage` /
`ingress-endpoints:manage` to `sessions:manage`. **The migration is a narrowing in one
direction and a widening in none**: every role that could cut somebody else's session before
can still do so, except a custom role that had been granted an endpoint-management permission
for endpoint management and got session-cutting thrown in.

### 3. A simulated role narrows what you may end

This is the ADR-058 check, and it is the reason §2 chose a permission over a membership read.

**The authority half flows through `Identity.Permissions`**, which `narrowToViewAs` has
already intersected with the simulated role. So an admin inspecting `member` **cannot** end a
colleague's session, and the degraded view tells the truth about it. Reading
`team_memberships.role` instead would bypass the intersection entirely and let a session
narrowed to `member` perform an act no member can perform — ADR-058 §2's guarantee is that no
path through the feature grants anything, and a membership read here would be that path.

`MayInspectRoles` reads the real membership row for a reason that does not generalise: it
governs *entering and leaving the mode*, and gating the exit on narrowed permissions would
strand an admin inside a reviewer session until the cookie expired. Ending somebody's shell is
an ordinary API act, not an escape hatch from the inspection, and it takes the narrowed
permissions like every other.

**The owner half is deliberately not narrowed.** ADR-058 §4 keeps the identity unchanged while
inspecting — the acting user is still the acting user — so your own sessions stay yours. Ending
your own session grants nothing and is refused to nobody who can reach the endpoint at all.

**The floor, and the edge it creates.** The operation's declared permission is the session
family's own open permission (§4), checked before anything else, so an admin inspecting
`reviewer` — who holds neither `terminal:open` nor `port-forwards:open` — is refused before
ownership is consulted. That is correct: a reviewer has no sessions. It also means a member
demoted to reviewer while holding a live shell can no longer end it themselves. We accept that
rather than routing the owner branch around the floor: a permission floor that ownership can
walk through is a hole shaped like a feature. The session still ends on its own bounds, and
any admin can end it sooner.

### 4. The surface, named as operations

Spec-first (ADR-025): the shapes below are what `docs/specs/openapi-v1.yaml` must carry before
any handler moves.

| Operation | Declared `x-required-permission` | Non-owner branch |
|---|---|---|
| `GET /terminal-sessions` — **new** | `terminal:open` | — |
| `DELETE /terminal-sessions/{session_uuid}` — **new** | `terminal:open` | `sessions:manage` |
| `GET /port-forward-sessions` — unchanged | `port-forwards:open` | — |
| `DELETE /port-forward-sessions/{session_uuid}` | `port-forwards:open` | `sessions:manage` (was `external-endpoints:manage`) |
| `GET /ingress-tunnel-sessions` — unchanged | `ingress-endpoints:read` | — |
| `DELETE /ingress-tunnel-sessions/{session_uuid}` | `ingress-tunnels:open` | `sessions:manage` (was `ingress-endpoints:manage`) |

`TerminalSessionInfo` mirrors `PortForwardSessionInfo` field for field where the fields mean
the same thing — uuid, target kind, target name and component, owner's email, client ip,
active, created/started/ended, end reason — plus the one distinction the tunnel does not have
to make: a **server shell** has a server and no resource, and must be legible as such in the
listing rather than appearing as a container terminal whose target has gone missing.

**Seeing is not cutting.** No listing moves and none takes `sessions:manage`: the tunnel
listing keeps `port-forwards:open`, the ingress listing keeps the `ingress-endpoints:read` it
shipped with, and the new terminal listing takes `terminal:open` — the tunnel's shape, because
it is the closer neighbour. They stay team-wide, which is what the tunnel listing has shipped
with since ADR-045 and what its own header comment argues for: what is forwarded out of this
team right now, by whom, onto what.
Knowing that a colleague is attached to a preview is operational information a team shares —
the audit trail already tells the same story to anyone with `audit:read`. Ending their session
is the power, and it is the one that moved.

**Idempotence**: an already-ended session answers `204`, as the tunnel handler already does. A
double click is not a failure, and a `404` there would read as one.

**One tension, named rather than buried.** ADR-038 §2 calls `x-required-permission` "the single
source of truth for authorization". For these three DELETEs it is the **floor**, not the whole
rule — the second check lives in the handler. That was already true before this ADR; what
changes is that it becomes deliberate, is written into the operations' descriptions so the
contract still tells a reader the whole rule, and is covered by tests that assert both branches
rather than only the declared one. The coverage test that every operation declares a catalogue
permission is unaffected.

### 5. What the person on the other end is told

Three facts reach a client whose session just ended, and today two enum values carry them.

**The owner ended it from somewhere else** — a second CLI, the dashboard. This stays
`user_close` and needs no new value. The client that receives `user_close` **without having
asked for it** knows it did not ask, and phrases it as *closed from another client*, never as
a failure.

A reviewer will hold this against ADR-067 §2, which forbade the bridge inferring a cause from
cancellation, so the distinction is stated: that inference was about somebody *else's* act,
guessed from a signal that did not carry it. This one is a client reading its own local state.
**A client may say what it did; it may not guess what the server did.**

**An admin ended it** — a **new member of the shared enum, `admin_close`**, one
`ALTER TYPE terminal_end_reason ADD VALUE IF NOT EXISTS`, the fifth time that enum has taken
one and the same one-line migration each time. It is not `revoked`, for the reason `revoked`
exists:

- `revoked` is a claim about the developer's **authorization** — ADR-045 §5's grant was
  withdrawn, the door is shut, and the remedy is to ask for it back.
- `admin_close` is a claim about **this session only** — nothing was revoked, the access is
  intact, and the developer may open a new session immediately.

Reusing `revoked` sends someone to renew a grant they still hold, and on a session that never
had a grant it sends them to renew something that does not exist. `revoked` keeps its meaning
and both its callers — grant revocation and endpoint deletion — which are the cases where it is
true.

The name states the kind of act, not the actor's role: a custom role holding `sessions:manage`
produces `admin_close` too. `user_close` / `admin_close` is the pair, and it reads as *who
closed it*.

**Both clients must phrase it, and the phrasing has a required shape**: it names a person and
it says the access survives. *"Session revoked by the server"* is the wording to avoid —
passive, sourceless, and read mid-keystroke it is indistinguishable from a platform failure.
ADR-067 §2 keyed the browser's list by the union of reasons precisely so a new member nobody
phrased fails to compile instead of arriving as a blank line; that guard now earns its keep.

**And the reason must survive the replica boundary.** This is the obligation the Context
found: both bridges report `EndDisconnect` when `OnHeartbeat` returns false, which is the
branch every cross-replica cut goes through. A cut that reaches the socket directly says
`admin_close`; the same cut against a session held by another replica says "connection lost".
The beat's zero-rows exit must therefore **read the reason the row was finalized with and
report that**, falling back to `disconnect` only when the row genuinely says nothing. Without
it this ADR ships correct authorization behind a message that is wrong on exactly the
deployments large enough to have administrators.

**Deliberately not carried: who did it.** The end reason is an enum member with no actor and
no free-text note. Adding either is a wire change on two families' control vocabularies and
puts an operator-typed string on another user's terminal; the enum member is the floor and a
note can be added later without reopening who may cut. This is the open question §7 records.

### 6. Audit

An admin ending somebody else's shell is the act an audit trail exists for. The shape follows
the neighbours rather than inventing one: `port-forward.open` and `terminal.open` record
against the session, with `target_kind` = `port_forward_session` / `terminal_session` and the
session uuid as target. So does this.

**The administrative cut gets its own action name**: `terminal.terminate`,
`port-forward.terminate`, `ingress-tunnel.terminate` — beside, not merged with, the
`*.close` the owner's own close keeps. (`terminal.close` is new only because no code path
could emit it before.) Two reasons:

- **Precedent.** ADR-067 §7 gave a wake its own event beside the open rather than a flag on it,
  for the same reason: a reader of a resource's history must see the act without reconstructing
  it from a detail.
- **The operational one, which is load-bearing.** Ending a colleague's session is the act an
  operator would alert on, and retention and alerting are built on action names. Sharing a name
  with the self-close — which will be almost all of the traffic — makes the interesting event
  unqueryable in practice.

**What the event records beyond the actor.** The recorder already writes actor, team, ip and
time. This event adds the **owner whose session it was**, the target the session was attached
to, and the end reason. The owner is the load-bearing field: without it the trail names who cut
and never who was cut, which is the half a reader actually needs. It rides `Event.Diff`, the
only free-form field the recorder has, redacted like anything else and adding no column — and
if a proper details field is ever added, this moves to it.

**The event is emitted when the row is finalized**, including when no live attach was reached
in this process. The audit records the decision, not whether this replica happened to hold the
socket. A second DELETE on an already-ended session emits nothing: it changed nothing.

### 7. What this does not decide

- **The cut mechanism itself.** ADR-067 §2's registry, its every-rung requirement and its
  "queue the reason before the teardown" ordering are unchanged. This ADR adds a caller, not a
  rung.
- **The idle and duration limits** — ADR-024's, ADR-032's and ADR-045 §5's, at their differing
  values. Nothing here shortens or lengthens a session that nobody ends.
- **Who may OPEN a session.** ADR-038's permissions, ADR-045's grants and ADR-067 §7's wake
  authorization stand untouched; no door is widened, and `sessions:manage` opens nothing.
- **The passkey step-up on `terminal:root`**, which is unchanged for opening a server shell
  and, per §1, not required for ending one.
- **Whether the end reason should name the admin who ended it** — the deliberate open question
  (§5).
- **Whether demoting or removing a member should end their live sessions.** It does not today;
  only a grant revocation tears sessions down (ADR-045 §5). Making a membership change cut
  sessions is a different decision with a much larger blast radius, and it would have to answer
  what happens to a member moved between roles that both allow the session.
- **Cross-replica immediacy.** Convergence stays the beat's, within one beat; making a cut
  instantaneous across replicas remains the LISTEN/NOTIFY question `TunnelPresence` already
  names. §5 fixes what the developer is *told*, not how fast.
- **Any dashboard beyond the endpoints.** The listing UI, its filters and where the button
  lives are not this ADR's.

## Consequences

- An administrator can end a session they are watching go wrong, from the dashboard, on any of
  the three families and on a server shell — and the person on the other end reads a sentence
  that names a human and does not claim their access is gone.
- **One new permission in the catalogue**, which is one more row in the rbac-matrix and one
  more column in the role×operation tests. `admin` gains it by construction (its set is
  computed from the catalogue); `member` and `reviewer` do not, because their sets are explicit
  lists. No migration touches a role.
- **A narrowing on upgrade**, and only a narrowing: a custom role granted
  `external-endpoints:manage` or `ingress-endpoints:manage` loses the ability to cut other
  people's sessions, which it never held on purpose. No role gains anything.
- One `ALTER TYPE ... ADD VALUE IF NOT EXISTS` on the enum both session tables share — the same
  additive, rolling-upgrade-safe migration it has taken four times already. An older replica
  reading `admin_close` reads a string it does not model; an older client drops through to its
  default phrasing, which is why §5's browser-side compile guard matters more than the wire.
- **A message bug is fixed for every cut, not just this one.** Making the beat's zero-rows exit
  report the row's persisted reason repairs `target_stopped`, `grant_expired` and `wake_failed`
  on multi-replica deployments too, all of which currently degrade to `disconnect` by the same
  branch. That is a consequence of this ADR and an argument for it.
- The terminal family gains a listing, so a team can see who has a shell open where. That is
  new visibility; it is the visibility the tunnel listing has always had, and the opens were
  already in the audit trail.
- **`x-required-permission` remains a floor rather than the whole rule on three operations.**
  Documented, tested on both branches, and unchanged in kind from what shipped — but a reader
  of the contract alone still needs the description to learn the second half, which is a cost.
- An owner who has lost the family's open permission cannot end their own session early. It
  ends on its own bounds; an admin can end it sooner.

## Alternatives considered

- **Keep `external-endpoints:manage` / `ingress-endpoints:manage`.** Rejected: it authorizes
  cutting a preview's shell with the permission for declaring a bastion target. The overlap
  with "admin" is a property of the current role sets, not a rule, and the first custom role
  composed for endpoint management breaks it silently.
- **Read `team_memberships.role`, as `MayInspectRoles` does.** Rejected, and this is the
  alternative closest to being right: it bypasses the ADR-058 intersection, so an admin
  inspecting `member` would still end a colleague's session and the degraded view would lie
  about the one act most worth verifying. `MayInspectRoles` reads the real row because
  *leaving the mode* must never be gated; an ordinary API act is not that.
- **A per-family permission** (`terminal:kill` beside `port-forwards:close`). Rejected: the
  answer is one rule, and splitting it invites the two halves to drift the way the two session
  layers already did — the drift ADR-067 had to spend a clause on.
- **Reuse `revoked` for the administrative cut.** Rejected: it is a claim about the
  developer's authorization and it is false for every session not opened under a grant. It
  sends them to renew something they never lost, which is the ADR-067 §2 failure exactly.
- **A free-text reason typed by the admin and shown to the developer** — the session-reason
  annotation ADR-045 cites from AWS. Rejected for now, not on principle: it is a wire change on
  two control vocabularies and an unredacted operator string arriving on someone else's
  terminal. It can be added later without changing who may cut.
- **Let any member end any session.** Rejected: cutting a shell destroys unsaved work, and the
  platform cannot tell a helpful cut from a hostile one. Owner-or-admin is the smallest rule
  that covers the operational need.
- **Let nobody but the owner end a session**, relying on the idle and duration limits.
  Rejected: an administrator watching a session do damage would wait up to four hours, and the
  platform's answer to "stop that" would be to go and stop the container underneath it — a
  workaround through another protocol, which is the shape this corpus has refused twice.
- **Audit the administrative cut as `*.close` with a flag in the details.** Rejected: alerting
  and retention operate on action names, and the interesting event would be unqueryable among
  the self-closes.
- **Make membership changes end sessions**, so the question answers itself. Rejected as a
  different decision (§7) with a much larger blast radius.

## Verification

Unit tests the implementation owes. Every clause is asserted **on all three families** unless
it names one, and the terminal assertions cover the container shell and the server shell
separately, since only one of them has a resource.

**Authorization (§2, §3)**

- The owner ends their own session holding only the family's open permission and **no**
  `sessions:manage` — on a tunnel, a container terminal, a server shell and an ingress attach.
- **A member holding `terminal:open` / `port-forwards:open` but not `sessions:manage` is
  refused `403` on a peer's session, and the peer's session is still live afterwards** —
  asserted as the absence of a cut *and* of a finalized row, not merely as the status code.
  This is the failure mode worth fearing and the test that would catch a handler that checks
  the permission after it cuts.
- An admin ends a peer's session on every family.
- A custom role granted `sessions:manage` may; the same role without it may not — the assertion
  that keeps the permission from being decorative.
- A session whose `user_id` is NULL is owned by nobody and takes the authority path; a
  token-authenticated caller never owns a session, and a `write`-scoped token whose creator was
  a member is refused (anti-elevation, unchanged).
- **Role inspection does not widen what someone can end**: an admin with `view_as = member` is
  refused on a peer's session; the same admin with no `view_as` is allowed; and an admin
  inspecting any role can still end **their own**. The three together are the ADR-058
  assertion, and the first is the one that fails if the authority half is ever re-implemented
  as a membership read.
- The floor holds in both directions: a caller lacking the family's open permission is refused
  before ownership is consulted, **including on their own session**; and `sessions:manage`
  alone does not reach the terminal endpoints.

**End reason and the client (§5)**

- The owner's close writes `user_close`; the administrative close writes `admin_close`; neither
  writes `revoked`.
- Grant revocation (ADR-045 §5) and endpoint deletion still write `revoked` — the regression
  test that keeps the new member from swallowing the old one's callers.
- `admin_close` reaches the live client on **every rung** of both families, WebSocket and HTTP
  — ADR-067 §2's registrability assertion re-run for this caller, which is where a rung that
  publishes no attach shows up.
- Both clients phrase `admin_close`: the CLI prints it, and the browser's union-keyed list
  makes an unphrased member a compile error. The phrasing names a person and does not state
  that access was revoked.
- A client that receives `user_close` it did not ask for phrases it as closed elsewhere; a
  client that asked prints its own local close. A client-side test, because the distinction is
  the client's own state.

**Ending, and the replica that does not hold the socket (§5)**

- **The owner ends their own session from a replica that does not hold the attach**: the row is
  finalized with the reason, the endpoint answers `204`, and the beat on the replica that does
  hold it observes zero rows updated and ends the bridge within one beat **reporting the
  persisted reason**, not `disconnect`. Asserted for `admin_close` and for `user_close`, and
  re-asserted for `target_stopped` — the fix is shared, so the regression would be too.
- A row finalized with no reason at all still ends the socket, falling back to `disconnect`:
  the fallback is a fallback, not the path.
- A cut that reaches no live attach anywhere still finalizes the row, still answers `204` and
  still audits.
- A second DELETE on an already-ended session answers `204`, does not overwrite the first
  reason, and emits no second audit event.

**Audit (§6)**

- The administrative branch emits `terminal.terminate` / `port-forward.terminate` /
  `ingress-tunnel.terminate`; the owner branch emits `*.close`. Asserted as **different action
  names**, since alerting is built on them.
- The event names the **owner whose session was ended**, the session's target, and the end
  reason — the assertion that would catch a trail recording only who acted.
- A terminal self-close emits `terminal.close`, which no code path emits today.

**Catalogue and contract (§2, §4)**

- `sessions:manage` is in `Catalog` with socle `write`, present in `TeamAdminPermissions()`,
  and absent from `memberPermissions` and `reviewerPermissions` — the three assertions that
  together *are* "team admin and above".
- `Prerequisites("sessions:manage")` is empty, and `Closure` adds nothing for it. Pinned so
  that introducing a `sessions:read` later cannot silently change what every role holds.
- `ValidateCustomPermissions` accepts `sessions:manage` from an admin composer and refuses it
  from a member composer.
- Every new operation declares an `x-required-permission` from the catalogue (the existing
  coverage test), and the two DELETE operations' descriptions state the second check.

No E2E assertion is added. The live path this touches — a socket cut from another process —
has no assembled-product behaviour the unit tests above do not already pin, and ADR-028's
single journey is not where authorization is proven.
