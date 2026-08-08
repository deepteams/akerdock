# ADR-063 — The ingress attach hop speaks h2c to the agent; the visitor hop cannot

- **Status**: Proposed
- **Date**: 2026-08-08
- **Extends**: [ADR-061](ADR-061-ingress-http3-http2-websocket-fallback.md) — the laptop's
  transport preference, the `akerdock-ingress-http-v2` wire and the bounds stand unchanged;
  this extends the multiplexing to the last hop of the chain, Traefik → agent
- **Updates**: [proxy-contract.md](../specs/proxy-contract.md) §5.1 (a second Traefik
  service name for an ingress group), §5.3 (the ingress example)
- **Related**: [ADR-009](ADR-009-proxy-intermediate-representation.md),
  [ADR-060](ADR-060-dev-ingress-tunnels.md),
  [ADR-056](ADR-056-waker-becomes-the-agent.md)
- **Related PRD sections**: §4 (proxy/domains), §12 (CLI), §18.1 (agent)

## Context

ADR-061 gave the laptop independent, flow-controlled streams: one HTTP request per relayed
connection, over HTTP/3 or HTTP/2, to the reserved attach path on the endpoint's own FQDN.
That is true up to Traefik and false after it. The chain is:

```
CLI ──h3 | h2 over TLS──▶ Traefik ──HTTP/1.1──▶ agent
```

Traefik terminates the laptop's HTTP/2 (or HTTP/3) and dials the backend with the default
`http://` scheme, which is HTTP/1.1. Everything ADR-061 bought is spent at the last hop:

- the tunnel's multiplexed streams are re-serialized into **one TCP connection per relayed
  stream** on the backend — up to `tunnel.IngressMaxStreams` (128) active, plus the control
  request, plus whatever the pending queue releases;
- full duplex on that hop is not a property of the transport but a favor asked of net/http,
  `http.NewResponseController(w).EnableFullDuplex()`, which exists precisely because
  HTTP/1.1 has no such thing natively;
- HTTP/2 flow control, which is what stops one slow relayed connection from starving its
  siblings, is not carried across.

The hop is on a Docker network between two containers on the same host, so this is not a
bandwidth problem. It is a **shape** problem: the transport the CLI negotiated stops being
the transport in use one hop before the code that consumes it.

The obvious fix — "make the agent's backend hop h2c" — is only half available, and the
other half is not a preference:

> `Connection` and `Upgrade` are **invalid HTTP/2 request headers** (RFC 7540 §8.1.2.2,
> enforced by the `connHeaders` list in `x/net/http2/server.go`), and Traefik's reverse
> proxy performs no RFC 8441 extended-`CONNECT` translation.

Ingress exists to relay a developer's own app, and that app's WebSocket and SSE traffic is
the point of the feature, not an edge case. A visitor's WebSocket upgrade arrives at
Traefik as HTTP/1.1 with `Connection: Upgrade`; if the backend hop were h2c, that upgrade
would have to be either dropped or translated, and nothing in the chain translates it. So
the constraint is not "h2c is risky for visitors" — it is that h2c **cannot** carry the
visitor path as it exists. That constraint is the shape of this decision, not a caveat
attached to it.

## Decision

### 1. The attach path gets its own Traefik service, over h2c

The reserved attach router (`proxy.IngressAttachPath` = `/.akerdock/ingress`, ADR-060 §2)
stops borrowing the visitor route's service and gets its own:

```yaml
    <uuid>-ingress-attach-0:
      loadBalancer:
        servers:
          - url: "h2c://akerdock-agent:8080"      # Traefik v3 cleartext HTTP/2 backend
```

while the visitor routers keep `http://akerdock-agent:8080`. Same host, same port, same
process — two backend protocols, selected by route. The split is generated from the IR
(ADR-009) like everything else in that file: a second deterministic service name next to
`<uuid>-s<n>`, one per distinct FQDN of the group, emitted in the same sorted order. It is
never a hand-edited label, and it shows up in the conformance golden as a reviewable diff.

Only groups carrying `IngressAttachPath` emit an h2c backend. An application, a preview,
a compose component or the control-plane scope never do.

### 2. The visitor path stays HTTP/1.1, for the RFC 7540 reason above

Not "for now", and not tunable. Any future move of the visitor hop off HTTP/1.1 requires
solving the upgrade translation first (RFC 8441 in the proxy, or a proxy that speaks it),
and that is a new decision, not a configuration change.

### 3. The agent serves h2c on its whole front, not on a scoped handler

The agent's front (`internal/agent/serve.go`) enables cleartext HTTP/2 alongside HTTP/1.1
on its single listener:

```go
protocols.SetHTTP1(true)
protocols.SetUnencryptedHTTP2(true)
```

A scoped handler was considered and is not possible: the protocol is decided by the
connection preface, **before any request line exists to route on**. There is no path at
which a handler could switch, because the switch happens a layer below paths. What can be
scoped is which peer connections arrive as HTTP/2, and that scoping lives in §1: only the
attach router has an `h2c://` service, so only attach connections speak the preface.

The cost of the wider surface is nil in practice and bounded in principle. Traefik dials
the waker's routes, the ingress offline page and `/metrics` with `http://`, so those
connections are HTTP/1.1 and are served exactly as before; an HTTP/1.1 peer never triggers
the h2c path. And a peer that *did* speak h2c to the waker would be served correctly — Go's
server handles both on one listener — so the wider surface is a capability, not an exposure.
The agent's front is not published on the host (ADR-036/056): its only reachable clients
are containers on the server's internal network.

### 4. Two properties of that server are load-bearing

- **`ReadTimeout` MUST stay unset.** Go's HTTP/2 server arms `Server.ReadTimeout` as a
  per-stream deadline on the **request body** — `net/http`'s `h2_bundle.go` disarms the
  connection deadline after the headers and starts
  `st.readDeadline = time.AfterFunc(sc.hs.ReadTimeout, st.onReadTimeout)`. The ingress
  control request and every data request are long-lived full-duplex bodies, so a
  `ReadTimeout` here cuts the tunnel — and every relayed WebSocket with it — on a fixed
  period. This is the same bug ADR-061 removed one hop up (`readTimeout: 0s` on the
  `websecure` entrypoint, proxy-contract §5.2), and enabling h2c is exactly what makes the
  agent able to reproduce it. `ReadHeaderTimeout` stays: it bounds a stalled request head
  and the HTTP/2 path does not consult it.
- **`MaxConcurrentStreams` is raised to 1024.** The tunnel admits 128 active relayed
  connections and queues 512 more (ADR-061 §4), and each one is a request the laptop opens
  toward this front, plus the control request. Go's default of 250 would rebuild, inside
  HTTP/2, the queue this hop exists to remove — and it would do so invisibly, as stream
  stalling, where the tunnel's own bound answers explicitly (`503` + `Retry-After`).
  Admission belongs to the tunnel, which can say no; the transport should not silently
  say "later".

### 5. `x/net/http2/h2c` is not used

That package is deprecated in `golang.org/x/net` (its own doc comment points at
`http.Server.Protocols`), and the repository's `staticcheck` configuration rejects it
(`SA1019`), so `make lint` would fail on it. The standard library's native unencrypted
HTTP/2 is the same wire protocol with the same prior-knowledge handshake — which is what
Traefik's `h2c://` transport speaks — and it exposes the stream bound through
`http.HTTP2Config`. No new dependency is added.

## Alternatives considered

- **h2c on the whole ingress service (visitors included)**: rejected — it cannot work.
  `Connection`/`Upgrade` are invalid HTTP/2 request headers (RFC 7540 §8.1.2.2) and no
  component in the chain performs the RFC 8441 translation, so every relayed WebSocket
  upgrade would break. The relayed WebSocket path is the feature, not a corner of it.
- **Leaving the hop on HTTP/1.1**: rejected — it silently voids ADR-061's premise on the
  last hop and makes the tunnel's multiplexing an accounting fiction: 128 relayed streams
  become 128 backend TCP connections, and per-stream flow control is replaced by whatever
  the HTTP/1.1 connection pool happens to do.
- **A second listener on the agent, h2c-only**: rejected — a second port to document,
  firewall and converge, for a distinction the proxy IR already expresses per route. The
  agent's contract is one internal port (`proxy.AgentPort`), and a service URL is a cheaper
  place to carry a protocol than a port is.
- **`servers[].url` left alone and Traefik told through a `serversTransport`**: rejected —
  it moves the same fact into a second object that must be named, generated and kept in
  sync with the service, for no gain; the scheme in the URL is Traefik v3's own way to say
  this and stays visible in the conformance golden.
- **Keeping `EnableFullDuplex()` as the mechanism and doing nothing else**: rejected as
  insufficient rather than wrong — it stays in the code (it is what the WebSocket and
  HTTP/1.1 fallbacks need) but it makes one hijack-free HTTP/1.1 exchange duplex; it does
  not multiplex, and it does not carry flow control.

## Consequences

- **Positive**: the transport the CLI negotiates is the transport in force end to end; the
  control stream and every data stream share one connection to the agent with native full
  duplex and per-stream flow control; the backend connection count for a busy tunnel drops
  from "one per relayed stream" to one; the split is IR-generated, so it is reviewed as a
  golden diff and reconciled by the ordinary checksum path (§6.2).
- **Negative**: an ingress group's dynamic file now carries two services instead of one,
  and the two speak different backend protocols — a reader must know why, which is what
  §2 exists to state; the agent's front now has two protocol paths to reason about; and the
  `ReadTimeout` trap is now reachable in a second place, guarded by a test rather than by
  the absence of HTTP/2.
- **Accepted risks**: Traefik's `h2c://` scheme is the documented v3 form but is exercised
  here for the first time in this codebase — the conformance golden pins the emitted
  configuration, and a real Traefik proves it in the proxy conformance tier, not in a unit
  test; a future proxy provider (Caddy, P2) must map the attach service to its own
  cleartext-HTTP/2 backend form or the attach silently degrades to HTTP/1.1 (correct, but
  slower), which is why the scheme lives in the IR-derived output rather than in Traefik
  vocabulary alone.

## Verification

- Unit: the ingress IR emits a dedicated `<uuid>-ingress-attach-<n>` service with an
  `h2c://` URL for each distinct FQDN, the attach router references it, the visitor routers
  keep their `http://` service, and exactly one h2c backend exists per attach router; an
  ordinary application group emits neither an attach router nor an h2c backend; the
  generator stays deterministic and byte-stable (the checksum-based apply depends on it).
- Unit: the agent's front answers the reserved attach path over prior-knowledge h2c — the
  control request is answered `HTTP/2.0`, the agent writes an `open` frame on the response
  body while the request body is still open, and a `session_close` sent on that request
  body ends the stream (full duplex, both directions, on one HTTP/2 stream).
- Unit: the front's SETTINGS frame advertises more concurrent streams than the tunnel's
  active + queued admission bound; `ReadTimeout` is zero and `ReadHeaderTimeout` is not.
- Conformance: the `ingress-endpoint` case's Traefik golden shows the attach router moving
  to its own h2c service and nothing else moving.
- The single E2E journey (ADR-026/028) is untouched.
