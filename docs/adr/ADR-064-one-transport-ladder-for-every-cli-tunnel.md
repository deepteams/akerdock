# ADR-064 — One transport ladder for every CLI-to-server tunnel

- **Status**: Proposed
- **Date**: 2026-08-08
- **Extends**: [ADR-061](ADR-061-ingress-http3-http2-websocket-fallback.md) — its ladder,
  its wire and its bounds stand; this makes them the transport of every CLI tunnel rather
  than of the ingress one alone
- **Revises**: [ADR-024](ADR-024-realtime-sse-websocket-terminal.md) — the
  "WebSocket reserved for the terminal" clause only. SSE for logs, statuses and jobs is
  untouched, and WebSocket remains the terminal's transport whenever the ladder falls
  back to it
- **Related**: [ADR-027](ADR-027-removal-tunnels-provisioning-patching.md) (a distinct protocol name per access
  path), [ADR-032](ADR-032-tcp-tunnel-cli-websocket.md),
  [ADR-045](ADR-045-external-endpoint-port-forwards.md)
- **Related PRD sections**: §5.7, §12, §21, §24.4

## Context

Three things establish a tunnel between the CLI and a server, and they do not agree on
how:

| Path | Transport | Terminates at |
|---|---|---|
| Ingress (ADR-060/061) | HTTP/3 → HTTP/2 → WebSocket | the agent |
| Port-forward and bastion (ADR-032/045) | WebSocket only | the control plane |
| Terminal (ADR-024) | WebSocket only | the control plane |

The divergence is not free, and its price was paid in a single day. Three transport
defects were found and fixed on the ingress path:

1. an intermediary's **absolute** request deadline (Traefik's `readTimeout`, 60 s by
   default) cutting a long-lived request no keep-alive can save, because the deadline is
   armed once on the request body and never refreshed;
2. a **stream ceiling below the admission bound** — a QUIC peer advertising 100 streams
   while the tunnel admitted 128, over a single connection;
3. an **open that blocks instead of failing** when that ceiling is reached, turning
   exhaustion into a fifteen-second stall and then an error, rather than an immediate
   refusal.

Not one of them is about ingress. All three apply verbatim to any long-lived tunnel over
HTTP. The other two families did not inherit the fixes, and will not inherit the next
ones, for one reason only: they do not share the code.

The obvious objection to homogenising upward is that WebSocket is the *more robust*
transport here — a hijacked connection leaves the server's read deadline behind, which is
precisely why defect 1 never touched port-forward. That objection holds against
*replacing* WebSocket. It does not hold against the ladder, which keeps WebSocket as its
bottom rung and, since ADR-061's failure budget, demotes to it automatically when an
HTTP transport keeps dying. The ladder does not trade robustness for speed; it makes the
fragility self-correcting.

## Decision

**One transport layer serves every CLI-to-server tunnel**: capability probe, then
HTTP/3, then HTTP/2, then WebSocket, with one per-process memory of what this network
carries — probe cooldown, consecutive-failure budget, and the classification that tells a
policy refusal (an expired mint, an occupied session) apart from a transport that cannot
carry the tunnel.

### 1. Shared transport, per-path identity

The layer is protocol-agnostic. The wire identifiers — protocol name, attach headers,
content types — are **parameterised per access path**, never pooled: ADR-027's rule is
load-bearing, an attach token minted for one path must not be redeemable on another, and
a control request offered to the wrong endpoint must be refusable on its content type
alone.

### 2. The endpoints do not move

Ingress keeps attaching to the agent (INV-007: no visitor byte through the control
plane); port-forward, bastion and terminal keep attaching to the control plane, which is
already in their byte path since it brokers the SSH channel.

This ADR decides **how bytes are carried, not who terminates them**. Routing the other
families through the agent as well would be a coherent next step and an orthogonal one:
it needs a reachable, certificate-bearing attach surface per server, which servers with
no proxy or no wildcard domain do not have. Deciding both here would couple two choices
that can be taken, shipped and reverted independently.

### 3. The terminal joins the ladder

ADR-024 reserved WebSocket for the terminal because it is the one genuinely
bidirectional stream and WebSocket traverses enterprise proxies. Both reasons survive:
the ladder's bottom rung *is* WebSocket, reached automatically wherever the rungs above
fail. What the terminal gains is QUIC connection migration — an interactive session that
outlives a move from wifi to a phone hotspot instead of dying with its TCP connection.

### 4. Choreography stays per path

A shared transport is not a shared protocol. Ingress needs a control stream because the
*server* asks the client to open a connection when a visitor arrives; port-forward and
terminal do not, because the client opens on demand — a local accept, or the single
stream of a PTY. `HTTPOrigin` therefore stays ingress-only. What is shared is the layer
below: transport selection, connection pooling, bounded opens.

### 5. The invariants travel with the layer

- A transport must be able to carry what admission accepts (ADR-061 §4). Exceeding a
  transport's stream capacity surfaces as a stall, not as an overload answer.
- A stream open is bounded; the stream itself is not.
- An idle pooled connection still holds its stream, so keep-alive windows stay well under
  the session's lifetime.

## Alternatives considered

- **Leave the three families as they are.** Rejected: it means paying for every transport
  defect once per family, discovering each one separately in production.
- **Homogenise downward, WebSocket everywhere.** Rejected: it would give up per-stream
  flow control and connection migration for the path that actually fans out.
- **Move every family to the agent at the same time.** Deferred, not rejected — §2.

## Consequences

- Port-forward, bastion and terminal gain the ladder, the three fixes above and every
  later one, at the cost of a migration each.
- The control plane grows two attach endpoints (spec-first, OpenAPI) and carries HTTP/2 or
  HTTP/3 attach streams. Its concurrent-stream count rises; it was already in the byte
  path of these families, so this is a load change, not a trust-boundary change.
- A CLI older than the migration keeps working: WebSocket is a rung, not a removed
  fallback. A server older than the migration refuses the HTTP attach, which the ladder
  reads as "not over this protocol" and answers by stepping down.
- Any tunnel family added later starts on the ladder rather than inventing a fourth
  transport.

## Verification

- Unit: ladder order and demotion; probe cooldown; failure budget and its diagnosis;
  policy refusals never retiring a transport; and that a control request carrying one
  path's content type is refused by another path's endpoint.
- Module: each family carries its full admission bound concurrently over each HTTP
  transport — the assertion that catches a stream ceiling hiding under a sampled load.
- The terminal keeps its PTY tests on every rung, WebSocket included.
- Existing WebSocket tests stay green on all three families: they are the compatibility
  proof, not legacy.
