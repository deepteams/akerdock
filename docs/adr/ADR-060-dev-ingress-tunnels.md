# ADR-060 — Ingress tunnels: a declared public URL relayed to a developer's machine

- **Status**: Proposed
- **Date**: 2026-08-08
- **Revises**: [ADR-031](ADR-031-cli-login-poll-code-pkce.md) — the transport invariant's
  "manager FQDN only" clause is widened, narrowly (§4); everything else (no inbound port,
  443/wss, proxy-safe Upgrade) stands unchanged
- **Related**: [ADR-032](ADR-032-tcp-tunnel-cli-websocket.md)/[ADR-045](ADR-045-external-endpoint-port-forwards.md)
  (the egress tunnels — this is their mirror image),
  [ADR-041](ADR-041-agent-websocket-channel.md)/[ADR-052](ADR-052-agent-command-channel.md)
  (the agent rail carrying control), [ADR-009](ADR-009-proxy-intermediate-representation.md)
  (the router is ordinary IR output), [ADR-030](ADR-030-preview-sso-forward-auth.md)/[ADR-042](ADR-042-application-access-protection.md)
  (the access wall, reused), [ADR-027](ADR-027-removal-tunnels-provisioning-patching.md)
  (standing rule: a new access path gets its own ADR and subprotocol)
- **Related PRD sections**: §4 (proxy/domains), §12 (CLI), §21 (access protection), §23
  (security model)

## Context

Every tunnel the platform ships points **outward**: `akerdock port-forward` lets a laptop
reach a container (ADR-032) or a declared external endpoint (ADR-045). Nothing points
**inward**. A developer who wants a public URL for the service running on their machine —
to receive a Stripe or GitHub webhook, to test an OAuth callback that requires a
registered HTTPS redirect, to show work in progress to a teammate or a client — leaves the
platform for ngrok, Cloudflare Tunnel or `ssh -R` against a personal VPS. All three
outcomes are worse than the gap they fill: team traffic transits a third party's relay,
the URL is anonymous and unaudited, and whatever access protection the team standardized
on (ADR-042) does not apply to precisely the most unfinished software the team runs.

The platform already owns every piece the third party provides: public entry (Traefik on
every server, ADR-009), HTTPS issuance (ACME), a stable wildcard namespace
(`servers.wildcard_domain`), a relay process already standing inline on the server
(the agent's HTTP front, ADR-036/056), a multiplexed tunnel wire (ADR-032), a CLI with
authenticated contexts (ADR-031), and an SSO wall (ADR-030/042). What is missing is the
inward assembly of those pieces — and the reason it must be assembled deliberately rather
than improvised is the same as ADR-045's: the capability exists anyway, in unaudited
forms; the platform's job is to give it a *named, bounded, audited* shape.

Two prior decisions constrain the design. **INV-007**: the control plane is never in the
data path of application requests — visitor traffic must not transit it. **ADR-027's
standing rule**: anything reintroducing an access path needs its own ADR and its own
WebSocket subprotocol; this is the fifth sanctioned WebSocket use (terminal, tunnel,
agent, relay, and now ingress). ADR-027 also removed Cloudflare Tunnel integration; what
returns here is not that integration but a native capability with no third party in the
path, which is exactly the form "reassessable upon proven demand" was reserved for.

## Decision

### 1. The URL belongs to a declared ingress endpoint, never to the session

An **ingress endpoint** is a first-class team resource, declared ahead of time:

```
ingress_endpoints(uuid, team_id, name, fqdn, server_id,
                  access, basic_auth_hash?, created_by, timestamps)
```

- `fqdn` is an **exact hostname**, chosen at declaration — typically under the server's
  `wildcard_domain` (`dev-kedric.apps.example.com`), but any FQDN the operator routes to
  the server is acceptable, exactly like an application domain. It is registered in the
  `domains` table, so the instance-wide `UNIQUE (fqdn, path)` guarantee (INV-002) makes a
  collision with an application, a preview or another endpoint impossible by construction.
- **No random per-session hostnames.** The hostname is as stable as the endpoint. This is
  the `previews.random_slug` lesson applied a second time: an ephemeral hostname churns
  ACME (per-session issuance latency, Let's Encrypt rate limits) and produces URLs nobody
  can bookmark, register as a webhook target, or recognize in an audit line. A stable URL
  is also what makes reconnection invisible to the visitor (§6).
- `server_id` is the **ingress server** — the vantage point whose Traefik terminates the
  hostname. Mirror of ADR-045's egress server, and equally not optional.
- `UNIQUE (team_id, lower(name))`; `server_id ON DELETE RESTRICT`.

Declaring the endpoint **pre-provisions everything static**: a routing file for the FQDN
is generated through the ordinary proxy IR and apply/verify cycle (ADR-009), pointing
permanently at the agent's HTTP front (`akerdock-agent:8080`), the same target the waker
uses — so the certificate is issued once, at declaration, and *opening* a tunnel later
touches no Traefik file at all. When no laptop is attached, the agent answers with a
branded "tunnel offline" page (the waker's waiting-page machinery, without the waking),
which is also what makes declaration-time ACME issuance work: the router must exist and
answer for HTTP-01 to complete. Deleting the endpoint removes the router, the `domains`
row, and cuts any live session.

### 2. The data path stays on the ingress server; the control plane only controls

```
visitor ──HTTPS──▶ Traefik ──Host(fqdn)──▶ agent HTTP front ──mux stream──┐
                                                                          │
laptop ──wss (443, same fqdn, reserved path)──▶ Traefik ──▶ agent ────────┘
```

The agent bridges two things it already touches: connections handed to it by Traefik, and
a multiplexed WebSocket. Per accepted visitor connection, the agent peeks the request head
to resolve the `Host` to an endpoint, opens one mux stream to the attached laptop, replays
the buffered bytes and then **splices the connection at byte level** in both directions.
Because the relay is a connection splice rather than request proxying, WebSocket upgrades
and SSE from the developer's app traverse it with no special handling — Traefik already
forwards an upgraded connection as a raw duplex stream.

The laptop side attaches over a **reserved router on the endpoint's own FQDN** —
`/.akerdock/ingress`, high priority, the mechanism the SSO callback routers already use —
so the attach rides the same public 443 the visitors use, terminates TLS at the same
Traefik, and reaches the same agent. The reserved router carries no access wall (the
attach authenticates with its token, §3) but is otherwise ordinary IR output.

The control plane never carries a visitor byte (INV-007 holds). Its role is control only:
declaration, minting, pushing session expectations to the agent over the typed command
channel (ADR-052), receiving open/close/traffic observations back over the observation
rail (ADR-041), and rendering both in the UI and audit. This also dissolves the
multi-replica weakness recorded for `TunnelPresence` (ADR-045): there is no laptop socket
held by an API replica for a revocation to miss — the socket lives on the server, and a
cut is a typed command to the agent, deliverable from any replica.

### 3. Mint at the control plane, redeem at the agent

`POST /ingress-endpoints/{uuid}/tunnels`, empty body — the endpoint froze everything at
declaration; the local port is the laptop's business and never crosses the API. The mint:

- checks `ingress-tunnels:open` and the endpoint being **unoccupied** (§6);
- creates an `ingress_tunnel_sessions` row and a single-use token, prefix **`akdi_`**,
  60 s TTL, SHA-256 hash stored — ADR-032's mint discipline unchanged;
- pushes `{session, token_hash, expiry}` to the ingress server's agent over the command
  channel — the expectation lives in agent memory only, as befits a 60 s secret;
- returns the attach URL (`wss://<fqdn>/.akerdock/ingress`) and the token.

The CLI verb is **`akerdock ingress <endpoint> <local-port>`** — one word for the whole
feature: the resource is an ingress endpoint, the permission `ingress-tunnels:open`, the
subprotocol `akerdock-ingress-v1`, the verb `ingress`. A noun pressed into verb duty, but
grep-unique and unambiguous where the considered alternatives were not: `relay` overloads
the internal worker→api relay (ADR-052), `tunnel` the egress bastion's UI (ADR-045),
`forward` reads as `port-forward`'s sibling when it is its mirror. The CLI dials the attach URL
with subprotocol **`akerdock-ingress-v1`** and the token in the query string, the redeem
shape of `/tunnel/ws`. The agent claims the token atomically in memory (single use,
expiry), reports the claim as an observation, and the control plane stamps `claimed_at` /
`started_at` on the row. **The wire is the ADR-032 mux verbatim** — text JSON control
frames (`open`/`open_ok`/`open_err`/`eof`/`close`), binary `[u32 stream id][payload]`,
32 KiB chunks, addressless — with the roles reversed: the **agent** originates `open`
when a visitor connection arrives, and the laptop dials `127.0.0.1:<local-port>`. The
distinct subprotocol name is ADR-027's rule, and it is load-bearing: an ingress attach
must never be redeemable against an egress endpoint or vice versa.

Session liveness is agent-reported: the agent heartbeats its live tunnels on the
observation rail, and the scheduler sweep finalizes rows whose agent went silent — the
ADR-045 layering (socket-local timers, reported heartbeat, leader sweep) transposed to
where the socket now lives.

### 4. The transport invariant is revised, narrowly

ADR-031 pinned the CLI to "the manager FQDN only, 443, no inbound port". The attach in §2
connects to the **ingress endpoint's own FQDN** instead, and the honest move is to revise
the invariant rather than tunnel the attach through the control plane to preserve its
letter (rejected in Alternatives — it would put the control plane in the data path, the
larger invariant).

The revised invariant: the CLI connects **outbound only, over 443, wss with standard
Upgrade headers, and opens no inbound network port** — to the manager FQDN, and, for an
ingress attach alone, to the FQDN of a **declared endpoint the manager itself named in
the mint response**. The CLI never dials an address the platform did not hand it inside an
authenticated response. Every property the invariant exists for — corporate-proxy
traversal, no listener on the laptop, nothing but standard TLS on standard ports —
survives intact; only "exactly one hostname" does not, and it was a proxy for those
properties rather than a goal.

The local loopback dial (`127.0.0.1:<port>`) is the mirror of `port-forward`'s sanctioned
loopback *listener* (cli.md §7) and needs no new carve-out: connecting to a local port is
not network reachability.

### 5. Protected by default, deactivatable; noindex always

An ingress endpoint exposes the least reviewed software a team runs, on a hostname that
looks exactly like the team's production. Both defaults follow from that:

- **`access` reuses the ADR-042 wall verbatim** — `sso` | `basic_auth` | `none` — with
  **`sso` as the default**: out of the box, a fresh endpoint is reachable by the team's
  own authenticated users and nobody else, which covers the demo-to-a-teammate case with
  zero configuration. The forwardAuth ritual, cookie scoping and callback router are the
  existing machinery with the endpoint as the resource; the generator emits the same
  middlewares it emits for a protected application. `none` is the conscious opt-out for
  the third-party-webhook case — it is part of the declaration, therefore an
  admin-level act (§7), and audited as such. Narrow public routes (ADR-049) are **not**
  in v1: an endpoint that needs "public webhook path, walled everything else" declares
  two endpoints or opts out; the exception language can extend here later if demand
  proves out.
- **`noindex` is unconditional and not a setting**, previews' regime for the same reason:
  content on an ingress hostname is by definition a competing, unfinished copy of
  something. The header rides every serving router including the offline page.

Force-HTTPS is likewise unconditional; there is no cleartext variant of this feature.

### 6. Session lifecycle: exclusive occupancy, bounded, reconnecting

- **One laptop per endpoint.** A mint against an occupied endpoint is refused with `409`
  naming the occupant — an endpoint is a stable identity, and two laptops behind one URL
  is a coin-flip per request. Taking over a colleague's endpoint is done with words, not
  with a race. (Per-user endpoint reservation is a possible later refinement; in v1 the
  endpoint is a team resource and the audit names the occupant.)
- **Idle timeout 30 min** (`tunnel.DefaultIdleTimeout`, shared with ADR-045), where
  "traffic" is visitor bytes in either direction. An attached tunnel nobody visits for
  half an hour reverts to the offline page.
- **Maximum session 12 h.** ADR-032's 4 h ceiling was calibrated on interactive egress
  sessions; an ingress tunnel's typical day is a webhook target registered in the morning
  and exercised irregularly until evening, and three forced re-attaches inside one
  workday would only train developers toward the opt-out. 12 h still guarantees nothing
  survives an unattended night. Both bounds are announced at attach and every automatic
  close reaches the CLI as an actionable reason, the ADR-045 discipline.
- **The CLI reconnects — on transport failure only.** A dropped socket (laptop sleep,
  network change, agent restart) is re-minted and re-attached automatically with backoff,
  under the CLI's standing `akd_` token; the stable hostname makes the outage invisible
  to visitors beyond a brief offline page. A **policy close never reconnects**: idle
  timeout, 12 h ceiling, operator cut, endpoint deleted, occupancy lost — each prints its
  reason and exits, because a client that automatically re-dials through a revocation has
  turned a control into a suggestion. The `end_reason` vocabulary distinguishes the two
  classes on the wire, so the decision is the reason's, not a heuristic's.

### 7. Permissions, audit, visibility

- **`ingress-endpoints:manage`** (`admin` level) — declare, update, delete, change the
  access mode, cut anyone's session. Declaring a public entry point onto arbitrary laptop
  software draws a security boundary; like ADR-045's declaration power, it is not a
  developer's call.
- **`ingress-endpoints:read`** (member) — list endpoints and sessions.
- **`ingress-tunnels:open`** (member) — mint and attach. A separate key for the same
  reason `port-forwards:open` was separated from `terminal:open`: "may expose their
  machine" must be grantable and revocable independently of everything else.
- Audit: `ingress-endpoint.create/update/delete` (the access-mode change explicitly
  visible in the diff), `ingress-tunnel.open/close` with end reason, denials on refused
  mints. Sessions appear in the existing Tunnels UI alongside ADR-045's, with live status
  from the observation rail and a close action for `manage` holders.

### 8. Out of scope for v1

Raw TCP/UDP exposure (HTTP(S) only — the wall, noindex and splice semantics are HTTP
assumptions); multiple simultaneous laptops per endpoint; a request
inspection/replay console (ngrok's inspector — a large product on its own); narrow public
routes on a walled endpoint (ADR-049's language, extendable later); per-user endpoint
reservation; traffic quotas; and any third-party relay integration (ADR-027's removal
stands — this feature is the native answer to that demand).

## Alternatives considered

- **Random per-session hostnames** (ngrok's free-tier shape): rejected — per-session ACME
  issuance latency and rate-limit exposure, URLs that cannot be a registered webhook
  target or a bookmark, certificate churn the preview `random_slug` decision exists to
  avoid, and an audit trail of meaningless hostnames. Declared endpoints cost one admin
  act, once.
- **Relaying through the control plane** (Traefik → control plane → laptop): rejected —
  puts the control plane in the visitor data path (INV-007), doubles instance traffic,
  and re-imports the multi-replica socket-affinity problem ADR-045 documents for
  `TunnelPresence`, this time on the data path where it cannot converge lazily.
- **Attaching via the manager FQDN and relaying to the agent** (preserves ADR-031's
  letter): rejected for the same reason — the manager becomes a permanent relay for every
  visitor byte. Revising the invariant honestly (§4) is smaller than quietly voiding
  INV-007.
- **Client-side `ssh -R` to the server**: rejected — the deployment key never leaves the
  control plane (ADR-001/003), developers hold no server credential by design, and an
  sshd remote-forward listener would bypass Traefik, the wall, noindex and HTTPS at once.
- **Embedding an existing tunnel implementation (frp, chisel, rathole)**: rejected — a
  third-party server embedded in the agent for a wire the house already owns (the ADR-032
  mux), against the static-binary posture (ADR-025) and with its own auth model to
  reconcile with ours.
- **A dedicated public port on the agent for attaches**: rejected — every server port
  beyond the proxy's 80/443 is firewall surface and a new thing to document; the reserved
  router costs nothing and inherits TLS.
- **An ADR-045-style grant ceremony for opening a tunnel**: rejected for v1 — the grant
  protects *reaching someone else's production data*; here the developer exposes *their
  own machine*, the blast radius is theirs, and the wall (§5) protects the visitors' side.
  The declaration act (admin), the separate `open` permission, exclusive occupancy and
  the audit trail are the proportionate controls. Revisable if practice proves otherwise.

## Consequences

- **Positive**: webhook development, OAuth callbacks and work-in-progress demos stop
  leaving the platform; the URL is stable, team-branded, HTTPS, SSO-walled by default and
  noindexed — a posture no third-party tunnel offers out of the box; the assembly is
  mostly existing machinery (proxy IR router + offline page, ADR-032 mux with reversed
  roles, ADR-052 command push, ADR-030/042 wall, ADR-045 mint/session discipline); the
  multi-replica cut weakness does not transfer, since the socket lives agent-side.
- **Negative**: the agent grows again (head-peek, splice, token claim, ingress
  heartbeats — after ADR-052/054/055 this is the trajectory, but it is real surface); one
  more table pair, OpenAPI surface, permission triple, CLI verb and UI listing; session
  truth is now agent-reported, so the control plane's view is eventually consistent by
  construction (heartbeat + sweep bound the staleness); the transport invariant loses its
  one-hostname simplicity (§4); the CLI gains its first reconnect loop, with the
  policy-vs-transport close distinction to get right.
- **Accepted risks**: an admin can deliberately publish an unauthenticated URL onto a
  team member's localhost (`access: none`) — that is the webhook use case, it is an
  admin-level declaration, and it is audited; a visitor-facing outage window of one
  backoff interval exists during laptop reconnection; the offline page discloses that a
  hostname is an idle ingress endpoint (as the waker's waiting page discloses a sleeping
  app — same posture).

## Verification

Unit level (ADR-028 — no new E2E journey): endpoint validation (FQDN shape, wildcard
coverage, `domains` uniqueness across apps/previews/endpoints, server required);
declaration pre-provisioning the router + offline page and issuing through the ordinary
apply/verify cycle; mint refusing without `ingress-tunnels:open`, on an occupied endpoint
(409 naming the occupant), and never accepting a client-supplied address or port; token
single-use, 60 s expiry, hash-only at rest, claim atomicity agent-side; subprotocol
separation (an `akdi_` token unredeemable on `/tunnel/ws` and an `akdp_` token
unredeemable on the ingress path); head-peek Host resolution incl. an unknown Host and a
request preceding any attach (offline page); splice carrying a WebSocket upgrade and SSE
end-to-end through the mux; wall modes (`sso` default on a fresh endpoint, `basic_auth`,
`none` requiring `manage` and audited), noindex and force-HTTPS present on every serving
router including the offline page, the attach router carrying no wall; idle 30 min on
visitor silence, 12 h ceiling, both announced at attach; every close reason reaching the
CLI, transport closes re-dialing with backoff and policy closes (idle, ceiling, revoked,
deleted, occupancy) exiting with the printed instruction; operator cut delivered through
the command channel from a replica not serving anything; endpoint deletion removing
router + `domains` row and cutting the live session; heartbeat-silent agent sessions
finalized by the sweep with the right reason; audit emission on declare/update/delete
(access-mode change visible), open/close, and refused mints.
