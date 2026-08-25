# ADR-077 — The edge relays by SNI; the server behind it stays the authority on everything it serves

- **Status**: Accepted
- **Date**: 2026-08-17
- **Related**: [ADR-009](ADR-009-proxy-intermediate-representation.md) (the IR whose output
  gains a second destination), [ADR-062](ADR-062-proxy-convergence-and-lockout-recovery.md)
  (the convergence that keeps the edge file honest),
  [ADR-060](ADR-060-dev-ingress-tunnels.md) (the mirror problem — a target the control plane
  *cannot* reach; this ADR's target it reaches over plain LAN, which is why no splice, no
  token and no agent hop are needed),
  [ADR-042](ADR-042-application-access-protection.md)/[ADR-036](ADR-036-scale-to-zero-waker.md)
  (the wall and the waker, which stay exactly where they are),
  [ADR-076](ADR-076-non-root-escalates-through-sudo-n.md) (the onboarding this topology came
  from), [ADR-025](ADR-025-go-stack-pgx-sqlc-chi-oapi-codegen.md) (why no mesh underlay)
- **Related PRD sections**: §3 (servers), §4 (proxy/domains), §26

## Context

The topology this ADR serves is the unexceptional one of a homelab, an office, a GPU box:

```
internet ── box (NAT 80/443 → one machine) ── edge server ── LAN ── private server(s)
```

One machine receives the public 80/443; the others must never be reachable from outside —
that is the point of the topology, not a defect of it. Both are AkerDock servers; the
control plane reaches both; each has its own managed proxy (§3). What the product cannot
say today is: *this application runs on the private server and answers on a public
domain*. The IR emits exactly one kind of HTTP backend — a container name on the server's
own Docker network (`routeTarget`) — so a route written on the edge cannot point at
anything the edge does not host, and a route written on the private server is only
reachable by whoever can already reach that server. The operator's options are to move
the workload to the edge (impossible when the workload is the GPU), to NAT the private
server (the thing the topology forbids), or to leave the platform for hand-rolled
`proxy_pass` — which the drift repair of ADR-009/062 would rightly fight.

Two shapes were considered and rejected before this one:

- **An internal DNS zone (`*.akerdock`) with a platform CA.** Issuing certificates is the
  easy half — `internal/pki` already does it for per-server database SSL. The hard half is
  *trust*: a private CA must enter the trust store of every consumer — every application
  container (distroless, alpine, scratch: each different, some impossible), every runtime,
  every browser, the CLI. The one product class that pulls this off (Kubernetes meshes)
  does it by inserting sidecars that terminate TLS *instead of* the application — an
  entire product, not a feature. No competitor in our class does anything of the sort;
  their communities put WireGuard under the PaaS and keep real certificates. And the
  platform already holds the no-PKI answer: DNS-01 wildcards issue real certificates to a
  server the internet cannot reach. An invented pseudo-TLD would also be a permanent
  mistake — ICANN reserved `.internal` for exactly this, and a name outside public DNS can
  never be validated by anyone.
- **A `has_internet` boolean deciding that the base instance's Traefik proxies the
  request.** Right feature, wrong axis. The private server *has* internet — outbound —
  or its agent could not dial back and its image pulls would fail. What it lacks is
  **inbound reachability**, and "the base instance" is an accident of today's layout: the
  property is a relationship between two servers, not a global.

The requirement that shaped the mechanism: **one flat namespace**. The operator holds one
wildcard (`*.service.example.com`), one DNS record pointing at the box, and wants
`llm.service.example.com` on the GPU box and `blog.service.example.com` on the edge —
placement must be invisible in the name. Nothing per-server, nothing per-opening.

## Decision

### 1. A server declares who serves its public routes: `edge_server_id`

One nullable self-reference on `servers`, not a boolean. `NULL` — the default, and the
migration's only writing — means "I serve my own routes": today's behaviour, untouched.
Set, it means "my public routes are answered by that server". Constraints enforced at the
API: same team, not self, the designated edge runs a Traefik (`proxy_type`), and **no
chaining** — a server designated as someone's edge cannot itself designate one; two hops
answer no topology anyone has shown us and multiply every failure mode. Changing the
value does not re-validate the server (SSH is untouched) but does re-render routing for
every resource placed on it.

The spec gains `edge_server_uuid` on `Server`/`ServerCreate`/`ServerUpdate` (spec-first,
as always). The dashboard offers it on the server settings, listing the team's eligible
servers.

### 2. The relay is SNI passthrough on 443, Host relay on 80 — never re-termination

When a routed resource lives on a server with an edge, the routing jobs write — **in
addition to** the origin's unchanged dynamic file — an edge-side dynamic file containing,
per public FQDN:

- a **TCP router** `HostSNI(fqdn)` with `passthrough: true`, whose service points at
  `<origin server.Host>:443` with **PROXY protocol v2** toward the origin;
- an **HTTP router** `Host(fqdn)` on the web entrypoint, whose service points at
  `http://<origin server.Host>:80` — this is what lets ACME HTTP-01 challenges and the
  origin's own HTTP→HTTPS redirect traverse.

The SNI is read from the ClientHello, in clear, before any cryptography: the edge routes
without holding a certificate, a key, or any knowledge of the application. Everything
that makes the route what it is — the certificate, the access wall, the noindex, the
scale-to-zero waiting page, the compose per-component routing — executes on the origin,
where it already lives, rendered by code that does not change. The edge is a pipe with a
table, and the table is derived from placements the control plane already knows, so the
edge can never become an open relay: an SNI with no known placement matches no TCP router
and falls through to the edge's own local routing, exactly as today.

Both routers ride the **existing** entrypoints. No static-config change on the edge, so
no proxy container recreate there. The origin pays the one static change of the feature:
its 443/80 entrypoints learn to accept **PROXY protocol from the edge's address only** —
without it, every wall decision, rate limit and access log at the origin would see the
edge's LAN IP as the visitor. That is an entrypoint (static) setting, which only a new
container reads (§1.4): the existing drift-detection-and-recreate of the proxy bootstrap
handles it, the same way it handles a changed port. Direct LAN connections are unaffected:
addresses outside the trusted list are simply never interpreted as PROXY headers.

### 3. The wildcard becomes what the spec already says it is: a naming template

The same `wildcard_domain` may be declared on several servers. It never was a certificate
claim by itself — without a DNS credential, "the wildcard is only a naming template and
each assigned host receives its own individual certificate via HTTP-01" (spec, verbatim).
The relay is what makes that sentence true behind an edge: the HTTP-01 challenge for
`llm.service.example.com` arrives from the internet, crosses the box and the edge's Host
relay, and is answered by the origin's Traefik, which then holds the certificate. One DNS
record (`*.service.example.com → box`), zero per-server domains, zero DNS credentials
required — DNS-01 remains available and remains the better answer when a single wildcard
certificate is wanted.

### 4. Rendering, lifecycle and failure follow the existing machinery

The edge-side file is an ordinary checksummed proxy revision under a **reserved scope**,
`01-edge-<origin server uuid>` — reserved the way `00-control-plane` is: application
scopes are UUIDs, so the prefix cannot collide. One file per origin server, rewritten
whole by the same jobs that write the origin's routes (apply_routing, compose, previews)
and swept by the same drift repair (ADR-009/062) on the edge. A destroyed preview or an
undeployed application leaves the edge file the same way it leaves the origin's. An
origin whose proxy an operator explicitly stopped goes dark through the edge exactly as
it goes dark on its own LAN — ADR-062's rule that an explicit stop is never repaired is
not re-litigated here, and the edge deliberately serves no error page for it: inventing
an answer would make the edge an authority, which is the one thing it must not be.

Internal-only services are expressed by *absence*: a resource with no public domain gets
no edge entry, and from the internet its name — even resolving to the box — meets a
router that does not exist. Reaching those services from inside the LAN under the same
flat namespace requires split-horizon DNS (the LAN resolver answering the origin's
address for those names); that is the operator's resolver, documented in the manual, not
a platform feature.

### What this ADR does not decide

No relay of raw TCP database entrypoints (no SNI to route by in the general case; they
stay LAN-reachable as today). No HTTP/3 across the relay — QUIC is UDP and Traefik's UDP
routing has no SNI matching; clients that see the origin's Alt-Svc fall back to TCP per
spec, and the visitor-facing hop simply is HTTP/1.1-or-2 over TCP. No edge chains, no
per-route edge override, no automatic reachability probing (the operator states the
topology; the platform does not guess it). No mesh underlay (ADR-025: PostgreSQL is the
only external dependency, and a VPN is somebody's whole product). And — recorded so the
question does not return without its arguments — no internal DNS zone and no platform CA
for service-to-service TLS: trust distribution into arbitrary application images is the
part that does not ship, and DNS-01 already issues real certificates to unreachable
servers.

## Verification

- IR/conformance: a golden fixture for the edge file — TCP router with `HostSNI` and
  passthrough, PROXY protocol toward the origin, Host relay on 80, reserved scope name;
  a fixture proving a resource without a public domain produces no edge entry.
- Routing jobs: unit tests that placements on an edge-routed server write and remove the
  edge revision alongside the origin's, for applications, compose components and previews.
- Static config: the origin's entrypoints accept PROXY protocol exactly from its edge's
  address, the drift detector recreates the proxy on that change, and a server with no
  edge renders no PROXY protocol stanza.
- API: `edge_server_uuid` round-trips; self, cross-team, chained and proxy-less
  designations are refused with named errors; changing it re-renders the server's routes.
- The single E2E journey is unchanged (one server); a two-server relay journey is not
  added — ADR-026/028's one-journey rule stands, module tests carry this feature.
