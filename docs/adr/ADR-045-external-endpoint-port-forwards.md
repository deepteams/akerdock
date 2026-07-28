# ADR-045 — Port-forwards to declared external endpoints (bastion)

- **Status**: Accepted
- **Date**: 2026-07-28
- **Revises**: [ADR-032](ADR-032-tcp-tunnel-cli-websocket.md) — widens the tunnel's target
  set beyond container-backed resources, and raises its idle timeout from 15 to 30 min for
  every tunnel (§5); the wire protocol is untouched
- **Related PRD sections**: §5.7 (operations), §12 (CLI), §23.3 (network egress), §24.4
  (real-time sessions)

## Context

ADR-032 ships a CLI tunnel whose target is always a container AkerDock deploys:
an application, a database, a compose service, a preview. `/servers/{uuid}/port-forwards`
was excluded on the grounds that a server-level forward is `ssh -L` reinvented with the
platform's deployment key, and the protocol was made **addressless by design** to close
the door on scope creep.

Real fleets are not that tidy. A team that runs its applications on an AkerDock server
frequently keeps **one database outside** it: a managed RDS/Cloud SQL instance, a legacy
PostgreSQL on a neighboring VM, an analytics cluster on the private network. Reaching it
today means one of three things, all worse than a tunnel: distributing the database
credentials **and** a network path to every developer, `docker exec`-ing into an unrelated
container to use it as a jump host, or deploying a `socat` container as a permanent
unaudited relay. The first widens the credential blast radius, the last two are exactly the
access that ADR-032 wanted to make explicit and auditable — obtained by working around it.

The decisive observation is that **this grants no new network reach**. Anyone holding
`terminal:open` on any resource of a server can already open an arbitrary outbound
connection from that server, interactively and with far less trace. What is missing is not
the capability, it is a *named, bounded, audited* form of it. That is what a bastion is.

The tension with ADR-032's addressless invariant is real and must be resolved, not waved
through: if the CLI may name an address at mint time, the door the ADR closed is open
again, and a `write` holder becomes a port scanner for the server's whole private network.

## Decision

### 1. The address belongs to a declared resource, never to the request

An **external endpoint** is a first-class team resource, declared ahead of time:

```
external_endpoints(uuid, team_id, name, host, port, server_id,
                   project_id?, environment_id?, description, created_by, timestamps)
```

- `host` + `port` are an **exact pair** — no CIDR, no port range, no wildcard host, no
  DNS pattern. One endpoint is one destination; a network is not addressable as a unit,
  which is what keeps this from becoming a scanner.
- `server_id` is the **egress server**: the tunnel is dialed from it, over the existing
  pooled SSH connection. An endpoint is meaningful only relative to the network vantage
  point it is reached from, so this is not optional.
- `project_id` / `environment_id` are the optional RBAC scope (ADR-038): a "prod replica"
  endpoint can be restricted to the people who already hold rights on production.
- `UNIQUE (team_id, name)`.

### 2. Mint names the endpoint; the protocol stays addressless

`POST /external-endpoints/{uuid}/port-forwards`, **empty body** — neither host nor port is
accepted from the client, since both were frozen at declaration. The mint is therefore
*stricter* than the existing ones, which still take a `port`.

Everything else is ADR-032 unchanged: single-use `akdp_` token, 60 s TTL, hash stored,
the same per-team cap of 10 open sessions (shared across all target kinds), open/close
audit, `/tunnel/ws` redeem, subprotocol `akerdock-tunnel-v1`, one multiplexed WebSocket per
session. **No wire change**: the frames stay addressless, and an existing CLI build speaks
this tunnel without modification.

`port_forward_sessions` gains a nullable `external_endpoint_id`, with a CHECK enforcing
**exactly one** target kind (`resource_id` XOR `external_endpoint_id`). A session whose
endpoint is deleted mid-flight is torn down at the next dial, like a destroyed preview.

### 3. Two distinct permissions, because they are two distinct powers

- **`external-endpoints:manage`** (`admin` level) — declare, update, delete an endpoint.
  This draws a network boundary; it is not a developer's call.
- **`port-forwards:open`** (`write` level) — open a session, evaluated against the
  endpoint's RBAC scope.

The RBAC matrix already specifies `port-forwards:open` (§1.2 no. 72), but the code mints
tunnels behind `auth.PermTerminalOpen` (`internal/handlers/portforward.go`). **This ADR
requires closing that gap first**: without a permission of its own, "may open a tunnel"
cannot be granted separately from "may open a shell", and the endpoint scope has nothing to
be evaluated against. Stating it here rather than discovering it during implementation.

### 4. No SSRF guard on this path, deliberately

`internal/safedial` is explicitly not applied to operator-configured infrastructure (the
SMTP relay, the OTLP collector, the S3 endpoint, the OIDC issuer) — only to
attacker-influenceable URLs. A declared endpoint belongs to the first category: an
`admin`-level actor sets it, and private/loopback destinations are the *point* of a bastion,
not an attack on it. The guard here is the declaration itself, plus the exact-pair rule.

### 5. Access is requested from the dashboard for a bounded window

A 30-day CLI token (ADR-031 §75) is a reasonable credential for deploying an application
and a poor one for reaching a production database: it sits in a `0600` file on a laptop, it
is warm at every hour of every day, and it proves neither presence nor intent. An endpoint
whose `criticality` is `sensitive` (the default — §6) therefore cannot be minted against
without an **active grant**, obtained by asking for it in the dashboard.

```
external_endpoint_grants(uuid, endpoint_id, user_id, reason, requested_at,
                         expires_at, granted_by, revoked_at)
```

- The request is made from a **browser session** — a second credential, which the holder of
  a stolen CLI token does not have. It states a **reason** (mandatory) and a duration
  bounded by the endpoint's `max_grant_duration` (default **4 h** — one ceremony in the
  morning, one in the afternoon; the friction is calibrated on a working day, not on a
  round number), itself capped at **8 h** so that a single request cannot buy a week.
- A mint outside any grant returns `403 access_request_required` **carrying the exact URL**
  of the request page. The CLI opens it and polls until the grant exists, then replays the
  mint — the same choreography as `akerdock login` (ADR-031), driven by machinery that
  already exists.
- The grant authorizes *minting*, not one long-lived session: within its window a developer
  may open tunnels as often as needed — after a reboot, a sleep, a network change — without
  another ceremony. That is what the window is for, and it is why it is measured in hours
  rather than minutes.
- **A session never outlives its authorization — the grant *is* the deadline.** On a
  `sensitive` endpoint the session ends at `grant.expires_at`, full stop: ADR-032's 4 h
  session ceiling does not apply on top of it, because two bounds racing each other are two
  numbers to explain where one suffices. The rule a developer has to remember is *your
  tunnel lives until your access ends, and dies after 30 minutes of inactivity*.
  Revoking a grant tears down the sessions it opened.
- The property ADR-032's ceiling protected — no session running forever while active, since
  a tunnel carrying continuous traffic never trips the idle timer — survives in a different
  form: a session cannot outlast its grant, and a grant cannot exceed 8 h without its holder
  standing in front of a second factor again. Duration is no longer the bound; **repeated
  proof of presence** is. `standard` endpoints keep ADR-032's 4 h ceiling.
- **The idle timeout moves from 15 to 30 minutes, for every tunnel** (`standard`,
  `sensitive`, and the container-backed targets of ADR-032 — `tunnel.DefaultIdleTimeout`).
  This is a revision of an ADR-032 parameter beyond this feature's own scope, made
  deliberately rather than as a side effect: 15 min was calibrated on HTTP debugging, where
  traffic is near-continuous, and it is too short for the interactive database work this
  ADR exists to serve — a developer who runs a query, reads the result and thinks is idle
  for fifteen minutes routinely. The terminal keeps its own 15 min
  (`terminal.DefaultIdleTimeout`, a separate constant): a shell left open is a different
  risk from a port-forward left open, and nothing here argues for touching it.
- **The deadline is announced when the tunnel opens, not only when it ends.** The mint
  response carries the session's `authorized_until`, and the CLI prints it on the line it
  already writes when the listener comes up: *forwarding 127.0.0.1:15432 → :5432 — authorized
  until 14:30 (3 h 47), Ctrl-C to stop*. Absolute time so the developer can plan a long
  transfer around it, relative so they take it in at a glance. On a `standard` endpoint the
  same line shows the ADR-032 session ceiling; the developer never has to work out which
  bound applies to them.
- Because a deadline that arrives unannounced reads as a bug, the CLI also warns **before**
  it lands, offering the renewal URL at that moment — the point where a developer discovers
  their transfer will not fit. The warning and the opening line quote the same instant.
- **Every automatic close states why, in words, with what to do next.** A tunnel that dies
  in silence is read as a bug in AkerDock, and the developer's next move is to look for a
  way around the platform rather than back into it. The reason already travels on the wire —
  the bridge closes with `conn.Close(StatusNormalClosure, string(reason))` — but the CLI
  discards it today, returning on a read error without a word (`readLoop`,
  `internal/cli/portforward.go`). It must print it instead, phrased as an instruction rather
  than a status: *your access expired at 14:30 — renew: <url>*, *closed after 30 min with no
  traffic — rerun the command*, *access revoked by an administrator*, *the target closed the
  connection*. The `terminal_end_reason` enum already carries `revoked`; `grant_expired` is
  added to it (`ALTER TYPE … ADD VALUE`), so the audit trail and the message the developer
  reads come from the same value.
- **Renewal extends the session; it does not cheapen the control.** Requesting again while
  a grant is still live pushes back the deadline of the sessions it opened, so a running
  dump survives instead of restarting from zero. It costs exactly what the first request
  cost: a reason (a fresh one — "dump still running, +2 h" is the audit trail's whole
  point) and a fresh second factor. A renewal that skipped the ceremony would be an
  unbounded grant delivered in slices.
- **Renewal is not capped by a total.** An earlier draft of this ADR bounded the chain of
  renewals at 8 h; the bound does not survive examination. Each renewal costs a fresh second
  factor, so an attacker cannot perform one, and a legitimate user who re-proves presence
  every four hours has satisfied the control the window exists to impose. A total would only
  tax the honest party — someone doing a genuine twelve-hour migration — and would not stop
  the one scenario it appears to address, since a compromised workstation simply waits for
  the next legitimate ceremony. The bound is therefore **the window itself, repeated**: no
  one holds this access without re-proving who they are every `max_grant_duration`.
- Once a grant has **expired** there is no renewal — only a new request, and the old grant's
  sessions are already gone. Renewals are audited and notified like any other grant, which
  is what makes a long chain visible rather than merely allowed.
- Grants are revocable, listed per endpoint, and audited on request, use and expiry.

**Why bound the window at all, given the second factor.** The factor proves *who asks*, at
the instant of asking; it says nothing about the four hours that follow. The window buys
three things the factor does not: automatic convergence (someone who leaves the team loses
access without anyone remembering to revoke it), the active-but-forgotten tunnel (a local
service polling the database never trips the idle timer), and the guarantee that access
does not quietly become a permanent state. Those are the reasons — not "MFA is weak" — and
they are also why the window should be generous rather than tight: its cost is paid in full,
daily, by legitimate users, while its benefit is probabilistic. A setting that pushes people
back toward a `socat` relay or a local copy of the database has produced the opposite of its
purpose.

**Why the browser rather than the CLI token.** Re-authentication is required either way
(below); the question is where it happens. rbac-matrix §5 is explicit that a token cannot
re-authenticate, so putting the control on the CLI token would have meant inventing
elevation attached to an API token *and* driving a WebAuthn ceremony from a terminal. The
grant needs neither: it lives in the browser, where the session, the ceremonies and the UI
already are. It is also the better **audit artifact** — "who was present" is worth less to
an auditor than "who asked for access to the production replica, for what reason, for how
long, and what they did with it".

**Requesting a grant always re-authenticates.** The request itself is a sensitive action in
the sense of rbac-matrix §5 — the "sudo mode" a platform demands before an administrative
act — so the page requires a **fresh second factor**, every time, with no per-endpoint
opt-out. Without it a stolen dashboard cookie is enough to grant oneself the tunnel, which
would leave the grant proving intent but not identity. This costs nothing to add: the
ceremony runs in a browser session, which is exactly what the existing step-up
(`/auth/passkey/stepup/*`, consumed today by the root terminal) was built for.

**Which factor.** The strongest one the user actually holds, chosen by the server, never
offered as a menu — a choice would let an attacker pick the weakest:

- a user with an enrolled passkey **must** use the passkey ceremony, unchanged;
- a user whose second factor is TOTP re-verifies with a **fresh dedicated challenge**, on
  the `mfa_challenges` machinery already in place (hash in database, `DELETE … RETURNING`,
  per-step replay protection). This is a new ceremony purpose, and the smallest piece of
  new code in this ADR.

The second bullet is a deliberate departure worth stating plainly: the root terminal is
passkey-only, and **stays** passkey-only — nothing here weakens it. But `mfa_required` can
be satisfied with TOTP alone (`internal/auth/auth.go`), so a passkey-only rule would lock
every TOTP-only user out of external endpoints entirely, and the predictable outcome is
every endpoint being declared `standard`. A TOTP consumed at this instant, for this purpose,
is a real second factor; what the house rejected was the login TOTP bleeding into an
elevation, and that is not what happens here.

A user with **no** confirmed factor cannot obtain a grant. On an instance where
`mfa_required` is off, that is a real gate, and it is the intended one: an endpoint that
reaches production is not reachable behind a password alone.

**What none of this defeats.** An actively compromised workstation: malware waits for the
developer to obtain their own grant, then rides the tunnel they open. Re-authentication
raises the cost of a stolen credential; it does not replace trusting the device.

**Self-service in v1.** The requester is their own grantor — the control is intent,
traceability and a lifetime, not four eyes. `granted_by` is stored separately from
`user_id` from the start so that per-endpoint approval by a third party is a later feature
rather than a migration.

### 6. Two profiles, not a checklist of switches

Everything in §5 is governed by a single per-endpoint dimension, `criticality`, with two
values. It is not six independent booleans: a security property that can be switched off
individually is one an operator eventually finds was off on the day it mattered, and the
combinations would multiply the test surface for cases nobody actually has. The house has
made this call twice already — ADR-036 discarded a two-variant switch for a single variant,
and ADR-011 keeps preview *triggers* optional while access protection stays on by default.

| | `standard` | `sensitive` |
|---|---|---|
| Access request | none — `port-forwards:open` on the scope is enough | **required** |
| Reason | — | **mandatory** |
| Second factor at request time | — | **always** (passkey when enrolled, else TOTP) |
| Grant window | — | `max_grant_duration`, default **4 h**; renewable without a total, each renewal costing a fresh factor |
| Session deadline | idle 30 min, max 4 h (ADR-032) | idle 30 min, then **the grant's expiry** — announced in advance |
| Revocation | close the session | revoking the grant tears down its sessions |
| Notification | — | the team's configured channel on each grant (ADR-019) |
| Audit | open/close | request, reason, every mint, expiry |

`standard` is therefore **exactly today's ADR-032 tunnel**, the only difference being that
the target was declared instead of derived from a container. That is deliberate: the
everyday case — a staging replica, an internal cache — gains no new friction, which is what
makes the `sensitive` regime credible rather than something to be routed around.

**Default on creation: `sensitive`.** Declaring an external endpoint usually means reaching
a real database; downgrading is then a conscious act, in keeping with ADR-011's
protection-by-default posture.

The profile is **atomic**. Only `max_grant_duration` is adjustable within `sensitive`, and
only downward from the cap — "sensitive but without a reason" is not a configuration, it is
the checklist this section exists to avoid.

### 7. Out of scope for v1

UDP; endpoint discovery or network scanning; endpoints without an egress server; any
browser-side console. **Registering an external database as a managed resource** — with
backups, monitoring and adoption semantics — is a much larger question and is *not* settled
here; this ADR ships transport only.

**Credentials stay outside**, and the comparison is worth recording because the question
returns every time. Three models exist in this market: AWS Session Manager brokers nothing
(a TCP tunnel; the RDS credentials are the user's problem); Boundary brokers or injects
short-lived credentials generated by Vault's database secrets engine, revoked when the
session ends; Teleport removes the password entirely, the database trusting Teleport's
client CA and `psql` authenticating with a short-lived x509 certificate.

Neither of the sophisticated two transfers to an endpoint we do not own. Vault is an
external dependency ADR-025 rules out, and the certificate model requires controlling the
target's authentication configuration — which is precisely what "external" excludes. We
therefore hold AWS's position knowingly: **the tunnel carries the path, not the secret**,
and a stolen grant alone opens nothing.

Worth noting for a later ADR, not this one: for databases AkerDock *does* manage, both
models are within reach — `internal/pki` already runs a per-server CA whose private key
never leaves the control plane, and ADR-003 already holds those databases' credentials
encrypted. The gap is that this CA signs *server* certificates; client-certificate
authentication would need a client CA, `pg_hba.conf` in `cert` mode and a CN-to-role
mapping. That is a question about ADR-032's managed targets, not about this bastion.

### 8. Where these numbers come from

The §5 parameters are not invented; they are calibrated against what established bastions
do, and the two places we deviate are deliberate.

- **A re-authentication window, not per-connection MFA.** Teleport pairs
  `require_session_mfa` with an `mfa_verification_interval` (documented example: `1h`) — MFA
  enforced, with a tolerance window so it stays usable. That is exactly the grant's shape,
  and it puts our 4 h in the right order of magnitude next to Teleport's 8 h default
  `max_session_ttl` and Boundary's 8 h `session_max_seconds`.
- **A mandatory reason.** AWS shipped session reason annotation for Session Manager; the
  reason field is standard practice for this class of access, not ceremony.
- **Idle timeouts.** AWS Session Manager defaults to 20 min (settable 1–60); Teleport ships
  `client_idle_timeout` **disabled by default**, its documentation showing 15 min as a
  global example. Our 30 min therefore sits between the two, and is stricter than Teleport's
  default of never disconnecting. Teleport also carries an open defect (#32073) in which
  this timeout, applied to *database* sessions, fires despite running queries because the
  timer is not reset — the exact trap this ADR's targets walk into, and one our
  implementation avoids by resetting on real traffic in both directions.
- **Deviation 1 — we terminate on expiry.** Teleport's `disconnect_expired_cert` defaults
  to **false**: an active session survives its own certificate expiring. Boundary does the
  opposite, terminating at `session_max_seconds` (default 8 h) and revoking the session's
  credentials. We follow Boundary: a tunnel that outlives its authorization makes the audit
  trail lie, which is the entire point of the feature. Teleport's default is nevertheless a
  warning about the UX cost, which is why the expiry is announced in advance rather than
  merely enforced.
- **Deviation 2 — self-service.** Teleport's Access Requests are built around review and
  approval, with auto-approval as a configuration. This is the point where we are furthest
  from the norm, and it is a conscious v1 scope call (§5), not an oversight: the schema
  carries `granted_by` separately so approval is a later feature and not a migration.

## Alternatives considered

- **Let the mint take an arbitrary `host:port`** (the naive form of the request): rejected —
  turns every `write` holder into a scanner of the server's private network, deletes
  ADR-032's addressless invariant with nothing in its place, and leaves the audit trail
  saying only "a tunnel to somewhere".
- **Reuse `/servers/{uuid}/port-forwards`**: rejected — ADR-032 excluded it by name, and the
  objection stands: a server-level forward is unbounded by construction. An endpoint is
  bounded by construction.
- **Client-side `ssh -L`**: rejected for the same reason as in ADR-032 — the deployment key
  never leaves the control plane (ADR-001/ADR-003).
- **A relay container (`socat`) deployed as a normal resource**: rejected — it works today,
  which is precisely the problem: a permanent listener, no session audit, no TTL, no cap,
  and it survives the person who created it.
- **A dedicated permission on the token instead of a grant** (what the root terminal does
  for API tokens today): rejected — a stolen token carries its permissions with it, so this
  answers none of the threat that motivates §5. It stays a sensible *additional* filter,
  not a replacement.
- **Step-up attached to the CLI token**: rejected — requires a concept the product does not
  have (elevation bound to an API token) and a WebAuthn ceremony driven from a terminal.
  The same re-authentication placed on the request page costs nothing, because the ceremony
  already runs in the browser.
- **Making the re-authentication optional per endpoint**: rejected — the endpoints that
  would keep it are exactly the ones nobody bothers to configure, and an optional control
  is one an operator discovers was off on the day it mattered.
- **Passkey-only, like the root terminal**: rejected for *this* action — `mfa_required` is
  satisfiable with TOTP alone, so it would lock TOTP-only users out entirely. The root
  terminal's passkey-only rule is untouched.
- **A control at every mint, with no window**: rejected — with a 15-minute idle timeout it
  means several interruptions an hour, and its predictable outcome is every endpoint being
  declared `standard`. A control that gets switched off is worse than
  no control, because it looks like one.
- **Shortening the CLI token's 30-day TTL instead**: rejected as a substitute (it degrades
  everyday use to mitigate one sensitive path, and a 3-day stolen token is still a stolen
  token), kept as an orthogonal knob if the instance wants it.
- **Waiting for the agent's command channel (ADR-041 phase 2)**: rejected — the SSH dial
  path exists and is proven; nothing here needs the new rail, and the endpoint model will
  transfer to it unchanged.

## Consequences

- **Positive**: the access developers already improvise becomes named, scoped, capped and
  audited ("who opened a tunnel to the production replica, when") — directly useful to the
  SOC 2 / ISO 27001 posture; no wire or CLI protocol change; the mint is stricter than the
  existing ones; the implementation is mostly plumbing, since `sshexec.Client.DialTCP`
  already takes an arbitrary address and `internal/tunnel` is already dialer-agnostic.
  `tunnel.Options` likewise already carries a per-session `MaxDuration`: the redeem handler
  passes the zero value today and will pass the grant's remaining time instead, with no
  change to the bridge.
- **Negative**: one more table, CRUD, OpenAPI surface, permission and UI screen; one more
  mid-session invalidation case (endpoint deleted); the threat model (§3.4) must state that
  a server is an explicit, declared egress point. The grant flow (§5) adds a table, a
  request page, a `403 access_request_required` path in the CLI, an expiry sweep and a TOTP
  step-up ceremony — worth pricing separately from the tunnel, and shippable as a second
  cut in which every endpoint is `standard` in the interim. The CLI does not
  reconnect a dropped tunnel today (`runPortForward` mints once and exits when the socket
  dies) — pre-existing behavior from ADR-032, but a grant measured in hours makes it
  conspicuous: transparent re-minting while the grant lives is the natural follow-up, and
  it is CLI work outside this ADR.
- **Accepted risks**: a team admin can declare any address the server can reach, including
  its loopback and the datacenter's private network — that is the definition of a bastion,
  and it is bounded by what the server already reaches; the target's credentials stay
  outside AkerDock, so a tunnel alone grants nothing.

## Verification

Unit level (ADR-028 — no new E2E journey): endpoint validation (exact pair, port range,
scope), authorization per endpoint scope, target XOR constraint, team cap shared across
target kinds, mint rejecting any client-supplied address, teardown on a deleted endpoint,
audit emission on declare/update/delete and open/close. For §5: mint refused with
`403 access_request_required` (and the request URL) outside any grant, accepted inside one;
a grant belonging to another user grants nothing; duration clamped to the endpoint's
maximum; a mandatory reason; expiry and explicit revocation both closing the window;
repeated mints accepted inside one window; a `sensitive` session ending at the grant's
expiry and not before (a five-hour grant is not cut at four); the idle timer resetting on
traffic in either direction and firing at 30 min, on container-backed tunnels too; every end
reason reaching the client and being printed (no silent close, including on revocation and
grant expiry); `authorized_until` present in every mint response, on both profiles, and
equal to the instant the session is actually cut; a renewal extending live
sessions rather than replacing them, refused without a fresh factor, and refused after
expiry; revocation tearing down live sessions; and audit of the request, of each mint that consumed
it, and of its expiry. For the
re-authentication: no grant without a fresh factor,
a `standard` endpoint minting with none of the grant machinery involved, a `sensitive` one
refusing without it, the server picking the passkey ceremony
whenever one is enrolled (a TOTP does not substitute for it), a TOTP challenge that is
single-use and replay-protected, a user with no confirmed factor refused, and a ceremony
belonging to another user granting nothing.
