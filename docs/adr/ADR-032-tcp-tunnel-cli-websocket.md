# ADR-032 — CLI TCP tunnel: multiplexed WebSocket (extension of ADR-024)

- **Status**: Accepted
- **Date**: 2026-07-25
- **Related PRD sections**: §12 (CLI), §5.7 (operations), §24.4 (real-time sessions), §27.1 (single port)
- **Revises**: ADR-024 (widens the WebSocket's scope); falls under the ADR-027 clause (any new access path requires an ADR)

## Context

The CLI (ADR-031) must make it possible to **debug a resource without exposing it**: connect
to a database, a redis, a compose service or a preview from the developer's workstation. These
services publish no public port (compose-spec §9: no domain = private, by design). The only
channel available from the client to them is the control plane, on 443.

ADR-024 **reserved the WebSocket for the terminal**, on the grounds that it was "the only
bidirectional flow". A TCP tunnel is a second genuinely bidirectional flow (bytes
in both directions) — the same criterion, a second time. Hence the need for an ADR that
extends ADR-024 rather than contradicting it. ADR-027 (removal of Cloudflare tunnels) further
requires that any reintroduction of an access path start from an ADR: this is that one. Note
that this tunnel is **not a new public exposure** — it is an authenticated operator
reaching their own workload through the already-exposed control plane port.

## Decision

### Mint / redeem (terminal pattern)

- **Mint (in the OpenAPI contract, `x-required-permission: write`)**:
  `POST /applications/{uuid}/port-forwards`, `POST /databases/{uuid}/port-forwards`,
  `POST /services/{uuid}/port-forwards`, `POST /applications/{a}/previews/{p}/port-forwards`
  (optional `component` parameter, same semantics as terminal sessions). Body:
  `{port: 1–65535}`. The **target (container, port) is frozen and authorized at mint**, a
  single time, audited. `PortForwardSession` response: `{uuid, token ("akdp_"+hex, single
  use, TTL 60 s, hash stored), websocket_path:"/tunnel/ws", expires_at}`. Per-team cap
  `port_forward_limit` (default **10, proposed**) → `409` beyond it.
- **Redeem (outside the contract, like `/terminal/ws`)**: `GET /tunnel/ws?token=akdp_…` —
  per-IP rate limiter, single-use atomic claim.
- **Explicitly excluded**: `/servers/{uuid}/port-forwards`. A server-level forward is
  an `ssh -L` reinvented with the platform's deployment key; operators already
  have SSH. Targets are container-backed resources, only.

### One multiplexed WebSocket per session

One WS per TCP connection would force either a mint+DB write+audit **per TCP connection**
(pathological for dev HTTP traffic that opens dozens of connections), or a reusable
token (breaking the house's single-use invariant). Therefore **one session = one mint,
one open audit, one close audit, one WS**; the terminal's invariants (single-use
`akdp_` token, idle 30 min, max 4 h, heartbeat 20 s, guaranteed teardown, open/close
audit) apply to the **session**. The manager persists each successful heartbeat:
after 90 seconds without one, the session is no longer counted or listed as open
and the scheduler finalizes it as disconnected. A nullable heartbeat preserves
rolling compatibility with an N-1 manager, whose sessions remain bounded by the
four-hour ceiling. Full yamux is rejected as a
dependency: the WS already provides frame boundaries, a minimal multiplexing layer
is enough.

### Protocol (subprotocol `akerdock-tunnel-v1`)

- **Text frames** = JSON control. Client→server: `{"t":"open","id":N}` (no address — the
  target was frozen at mint; the protocol is **addressless by design**, which
  closes the door on any scope creep). Server: `{"t":"open_ok","id":N}` or
  `{"t":"open_err","id":N,"code":"dial_failed|limit","msg":…}`. Both directions:
  `{"t":"eof","id":N}` (TCP half-close), `{"t":"close","id":N}`.
- **Binary frames** = `[u32 big-endian stream id][payload]`.
- Limits (proposed defaults): **32** concurrent streams max per session; pending server
  buffer **1 MiB** per stream, then stream closure. **No flow-control window
  in v1** — head-of-line blocking between streams of the same session is an accepted and
  documented limitation (debug tool, typically 1–3 concurrent connections).

### Server side

One **SSH `direct-tcpip` channel per stream** over the existing pooled SSH connection
to the server (`golang.org/x/crypto/ssh` natively multiplexes channels — no new
SSH connection per stream). Dial target: the container's IP on its Docker network (reachable
from the host), resolved by `docker inspect` at session opening. Honest constraint
to state in the spec: `internal/sshexec.Client` keeps `*ssh.Client` private and only exposes
exec-oriented methods — a small `DialTCP(addr)` extension is needed; and **any
port of the target container is reachable from the host** (Docker does not filter
host→container) — the **authorization boundary is therefore the resource, not the port**. The spec
says this explicitly, rather than pretending there is an `EXPOSE`-based control that is not one.

## Alternatives considered

- **One WS per TCP connection**: rejected — mint/audit per connection (unmanageable) or a
  reusable token (breaks the single-use invariant).
- **yamux / full multiplexer**: rejected — heavy dependency for a need covered by
  a minimal layer on top of WS frames.
- **Client-side SSH tunnel** (the CLI opens the SSH to the server itself): rejected — the
  deployment key never leaves the control plane (ADR-001/ADR-003), and it would open
  direct client→server network access, contrary to the transport invariant (ADR-031).

## Consequences

- **Positive**: debugging of any container resource from the workstation, without exposure or
  direct SSH; full reuse of the terminal pattern (token, cap, audit, teardown); a single port,
  a single auth stack.
- **Negative**: head-of-line blocking between streams of a session (accepted); `DialTCP`
  extension to `internal/sshexec`; one more technical table (`port_forward_sessions`).
- **Accepted risks**: authorization is at the granularity of the resource, not the port — a
  holder of `write` on a resource reaches all the ports of its containers. This is
  consistent with the terminal (`docker exec` already gives the whole container).
