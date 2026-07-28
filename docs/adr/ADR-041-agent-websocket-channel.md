# ADR-041 — Agent channel: persistent outbound WebSocket (presence + observations)

- **Status**: Accepted
- **Date**: 2026-07-28
- **Extends**: [ADR-040](ADR-040-server-agent-outbound-observations.md); third sanctioned WebSocket use after the terminal (ADR-024) and the CLI tunnel (ADR-032)
- **Related PRD sections**: §18.1, §18.3, §16.1

## Context

ADR-040 phase 1 ships observations as plain outbound POSTs. That works, but it
leaves two gaps. **Presence** is inferred from a one-minute heartbeat: the
control plane cannot tell a live agent from one that died fifty seconds ago,
so the "agent silent" signal is both late and fuzzy. And the **phase 2
trajectory** (a command channel replacing inbound SSH, ADR-001's stated goal)
requires the control plane to reach the server without an inbound port — which
only a connection *initiated by the agent* can provide. Every batch POSTed
today is a connection the next batch has to rebuild.

ADR-024 reserved WebSocket for the terminal, and ADR-032 set the precedent
that each new WebSocket use gets its own ADR with a dedicated subprotocol.
This is that ADR for the agent.

## Decision

1. **One persistent outbound WebSocket per server**: the agent dials
   `/agent/v1/ws` on the control plane's single port (§16.1(6)), subprotocol
   `akerdock-agent-v1`, authenticated at the upgrade by its per-server token
   (ADR-040 §4 — same credential, same observations-only scope).
2. **Presence is the connection**: connected agent = live agent, tracked in an
   in-memory registry; the control plane pings the socket periodically so a
   dead peer is detected within seconds, not minutes. `last_seen_at` keeps the
   durable trace. The server API exposes both (connected + last seen).
3. **Observations ride the socket as frames**: the same batches as the POST
   body (`{"type":"observations","seq":N,"observations":[…]}`), acknowledged
   by the control plane (`{"type":"ack","seq":N}`). One batch in flight at a
   time; an unacknowledged batch is retried — at-least-once, unchanged from
   phase 1, and ingestion idempotency already absorbs duplicates.
4. **POST stays as the fallback**: some egress paths break WebSockets. When
   the dial or the socket fails, the agent falls back to the phase-1 POSTs
   (with a re-dial cooldown), and below that the SSH scans remain — the
   degradation ladder is WS → POST → SSH, and every rung is the previous
   status quo (ADR-040 §6).
5. **Commands stay OUT of scope**: the socket is strictly agent→control-plane
   data for now. The command protocol (orders, idempotence, per-command
   authorization, SSH retirement) is the next ADR, and will ride this same
   rail once the channel has proven itself in production.

## Alternatives considered

- **POST-only (status quo)**: no presence better than heartbeat granularity,
  and no rail toward phase 2; rejected by the operator after weighing the
  added complexity.
- **Long-polling / SSE downstream**: gives the control plane a push path but
  still no instant presence, and a second mechanism to maintain next to the
  eventual command channel; a WebSocket serves both with one connection.
- **gRPC streaming**: a second RPC stack in a repo whose realtime is already
  SSE + WebSocket (ADR-024/032); rejected for consistency.
- **Jumping straight to commands**: front-loads the hardest design (order
  idempotence, authorization taxonomy) before the transport has run a single
  day in production; rejected — presence + observations first.

## Consequences

- **Positive**: exact real-time presence (the remaining ADR-040 UI item
  becomes trivial); one warm connection instead of a connection per batch;
  the phase-2 command channel becomes an incremental frame type on an
  existing, proven socket.
- **Negative**: WebSocket traversal must be allowed by whatever fronts the
  control plane (the nginx/proxy must pass the upgrade on `/agent/v1/ws`,
  like `/terminal` and `/tunnel`); reconnect/backoff logic on the agent; the
  in-memory presence registry is per-api-process (accurate in the supported
  single-api topology, to be revisited with api replicas).
- **Accepted risks**: a middlebox that silently kills idle WebSockets shows
  up as flapping presence — mitigated by server-side pings and the POST
  fallback; the same observations may arrive twice during a transport
  switchover (ingestion is idempotent by design).
