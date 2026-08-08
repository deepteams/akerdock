# ADR-061 — Ingress tunnel transport: HTTP/3, then HTTP/2, then WebSocket

- **Status**: Proposed
- **Date**: 2026-08-08
- **Revises**: [ADR-060](ADR-060-dev-ingress-tunnels.md) §§2–4 — the ingress
  endpoint, mint, authorization, lifecycle and data-path ownership stay unchanged; only
  the laptop-to-agent transport and stream wire are revised.
- **Related**: [ADR-008](ADR-008-observability-otlp-everywhere.md),
  [ADR-009](ADR-009-proxy-intermediate-representation.md),
  [ADR-032](ADR-032-tcp-tunnel-cli-websocket.md)

## Context

ADR-060 reused ADR-032's addressless multiplexing protocol over one WebSocket. That made
the first implementation small and proxy-safe, but it also inherited two throughput
limits that matter much more for ingress than for an interactive port-forward:

1. all visitor connections share one ordered TCP connection, so packet loss blocks every
   logical stream; and
2. the in-process decoder writes each frame synchronously to its consumer, so one slow
   consumer can stop progress on every sibling before the network is involved.

A browser commonly requests tens or hundreds of assets concurrently. Raising the pending
queue prevents the 33rd request from failing, but does not create capacity: it exchanges a
fast `502` for queue latency. The transport needs independent streams, bounded flow
control and connection reuse.

HTTP/3 is HTTP over QUIC, not a fourth transport. QUIC removes cross-stream
head-of-line blocking on packet loss. HTTP/2 retains TCP's transport-level blocking but
provides independent application streams and flow control. WebSocket remains the most
widely traversable fallback for enterprise proxies.

Traefik terminates public TLS and HTTP/3 for the endpoint. A raw QUIC listener in the
agent would therefore either compete for UDP 443 or require a second public port and a
second certificate owner, both contradicting ADR-001/009. WebTransport would expose the
right server-opened stream abstraction, but Traefik does not provide a portable
end-to-end WebTransport route to an HTTP backend. The protocol must use ordinary HTTP
requests that Traefik can route while retaining independent HTTP/3 or HTTP/2 streams on
the WAN side.

## Decision

### 1. Transport preference and capability probe

The CLI attempts transports in this strict order:

1. **HTTP/3** over UDP on the endpoint's configured HTTPS port;
2. **HTTP/2** over TCP/TLS on that same host and port;
3. **WebSocket** (`wss`) using the four-lane v2 wire, with ADR-060 v1 negotiation
   for older peers.

Before consuming a single-use mint token, the CLI sends an unauthenticated `OPTIONS` to
the reserved attach path. The response advertises supported protocol versions. A failed
probe falls through without consuming the token. A transport that fails during initial
attach is suppressed for the rest of that CLI run; the failed durable session is closed,
a new token is minted, and the next transport is attempted. A transport failure after a
healthy session remains a reconnect, not a permanent downgrade.

The fallback is automatic and emits the selected transport once. There is no flag that
can silently disable TLS verification. UDP-blocked and explicit-proxy environments reach
HTTP/2 or WebSocket without operator action.

### 2. HTTP v2 wire: one HTTP stream per relayed connection

HTTP/3 and HTTP/2 use the same **`akerdock-ingress-http-v2`** application protocol over
full-duplex streaming requests to `/.akerdock/ingress`:

- one long-lived control request carries newline-delimited JSON control frames;
- when the agent needs a connection, it sends `open(id)` on control;
- the CLI dials the fixed loopback target and opens one new full-duplex HTTP request
  carrying that `id`;
- the request body is laptop→agent bytes and the response body is agent→laptop bytes;
- `open_err`, session close reasons and heartbeats stay on the control request.

The data request itself is the flow-controlled stream. There is no `[stream-id][payload]`
framing, global data writer lock or cross-stream application buffer. On HTTP/3, every
relayed connection is an independent QUIC stream. On HTTP/2, it is an independent HTTP/2
stream, subject only to the shared TCP connection's packet-loss ordering.

The initial control request consumes the `akdi_` token and carries a client-generated
256-bit attach key. Only the key hash is retained in agent memory. Data requests carry
the durable session UUID, stream ID and attach key in headers; constant-time validation
binds them to that claimed session. The key is never logged or persisted and dies with
the session. The token remains single-use.

### 3. Connection strategy

- HTTP/3 uses one persistent QUIC connection. Its native independent streams already
  avoid cross-stream loss blocking and share congestion control intentionally.
- HTTP/2 uses **four persistent TLS/TCP connections**. Data streams are assigned to the
  least-loaded connection, reducing the blast radius of TCP head-of-line blocking while
  keeping the connection count bounded.
- WebSocket v1 remains wire-compatible. Its in-process receive path gains bounded
  per-stream queues so a slow consumer cannot block the socket decoder. New v2 peers
  negotiate four physical WebSocket lanes and pin each logical stream to one lane;
  one lane remains accepted for older clients.

No mode creates one connection per visitor request. HTTP transports reuse their session
connections; the agent's per-session reverse proxy also reuses idle connections to the
laptop. Because one `http.Transport` instance belongs to exactly one endpoint session,
that keep-alive pool cannot route a request to another endpoint.

### 4. Bounds and overload

The existing ingress bounds remain: 128 active upstream connections, 512 pending opens,
30-second queue wait. They are overload protection, not a throughput control. HTTP
keep-alive commonly serves many sequential requests on one active connection.

Every compatibility WebSocket stream has a bounded receive queue. A peer that violates
the negotiated receive window loses that stream, not the entire tunnel. Control frames
remain processable under data backpressure.

### 5. Proxy and deployment

Traefik's `websecure` entrypoint enables HTTP/3 and the managed proxy container publishes
the configured HTTPS port for both TCP and UDP. HTTP/1.1 and HTTP/2 visitors are
unchanged. The generated static configuration and proxy conformance fixtures own this
setting through ADR-009; it is never a hand-edited label.

HTTP/3 is unavailable when an upstream load balancer does not forward UDP. That is an
expected fallback condition, not a server validation failure. HTTP/2 and WebSocket remain
on TCP 443.

### 6. Measurement

The agent records transport, active streams, queue wait, stream-open latency, relayed
bytes and session failures through OpenTelemetry instruments. The tunnel package ships
benchmarks for one bulk stream and concurrent small streams. Performance changes are
judged on throughput plus p50/p95/p99 latency; queueing alone is never reported as a
throughput improvement.

## Alternatives considered

- **Raw QUIC directly in the agent**: rejected — it creates a second TLS/certificate
  owner or a second public port and bypasses the proxy IR.
- **WebTransport end to end**: deferred — it is the ideal server-opened stream API, but
  the managed proxy cannot currently carry it portably to the agent backend.
- **One multiplexed stream over HTTP/3**: rejected — putting the ADR-032 wire inside one
  QUIC stream preserves application and loss head-of-line blocking among visitor flows.
- **One HTTP/2 connection**: rejected — correct flow control would fix the application
  stall, but a lost TCP segment would still pause every active stream.
- **More active streams or a larger queue only**: rejected — neither removes the shared
  serialization points, and unbounded fan-out can exhaust the developer's laptop.
- **Disable certificate verification for the QUIC attempt**: rejected — the attach token
  is a bearer secret and must never be exposed to an unauthenticated peer.

## Consequences

- **Positive**: browser asset bursts use native HTTP stream flow control; HTTP/3 removes
  cross-stream loss blocking; HTTP/2 and WebSocket preserve reachability; local TCP and
  tunnel handshakes are amortized by keep-alive.
- **Negative**: three client transports and two application wires must be maintained;
  the HTTP v2 reverse-open handshake adds one established-connection round trip before a
  brand-new local connection; the proxy publishes UDP in addition to TCP.
- **Accepted risk**: some reverse proxies may advertise HTTP/2 yet buffer a full-duplex
  request. Startup attach has a deadline and downgrades to WebSocket rather than hanging;
  proxy conformance covers the managed Traefik path.

## Verification

- Unit tests: transport order, probe not consuming a token, attach-key hashing and
  constant-time rejection, one-use token, stream/session binding, open failure, queue
  bounds, close reason, keep-alive isolation, least-loaded HTTP/2 lane selection and
  WebSocket slow-consumer isolation.
- Module tests: real HTTP/2 and HTTP/3 servers carry at least 40 concurrent streams, with
  128 active and the remainder queued, without `502`; a deliberately stalled stream does
  not block a sibling.
- Proxy conformance: static Traefik output enables HTTP/3 deterministically and deployment
  publishes the HTTPS UDP port.
- Existing ingress WebSocket tests remain green to prove old CLI compatibility.
- `go test -race ./...`, generation synchronization and lint are required before merge.
