# ADR-024 — Real-time transport: SSE for logs/statuses/jobs, WebSocket reserved for the terminal

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.24, §24.4, §27.1, §10.4, §22.2

## Context

Using WebSockets for all real-time traffic, on dedicated ports (§10.4), complicates exposure behind enterprise proxies and firewalls. Yet AkerDock's real-time needs are almost all **unidirectional** (build/runtime logs, statuses, job progress); only the terminal is genuinely bidirectional. The transport and its integration with the control plane's single port (ADR-001) must be chosen.

## Decision

- **SSE (Server-Sent Events)** for logs, statuses and job progress: **native reconnection**, **resumption via the `Last-Event-ID` cursor**, compatible with enterprise proxies.
- **WebSocket reserved for the terminal** — the only bidirectional stream (interactive PTY).
- **Everything goes through the control plane's single port** (§27.1): no dedicated real-time port.

The requirements of §24.4 apply: streams protected by the same policy as the equivalent REST endpoint, realtime token short-lived/single-use or scoped to the resource, terminal with heartbeat, idle timeout, maximum duration and guaranteed kill, open/close audited.

## Alternatives considered

- **WebSocket for everything (parity)**: rejected — bidirectionality is useless for log streams, reconnection and resumption would have to be reimplemented by hand, and enterprise proxy/firewall traversal is more fragile.
- **Long/short polling for statuses**: rejected — unnecessary latency and load at the target scale (500 concurrent realtime streams, §22.2); SSE provides push with HTTP simplicity.
- **gRPC streaming**: rejected — not natively consumable by a browser without a gateway, an additional toolchain dependency for a need covered by standard HTTP.

## Consequences

- **Positive**: lossless log resumption via `Last-Event-ID` (aligned with the cursor-based backpressure of §22.2); a single port and a single auth stack for REST and real-time; SSE traverses standard HTTP intermediaries; the terminal keeps the transport suited to its need.
- **Negative**: two transports to maintain nonetheless (SSE + terminal WebSocket); SSE is unidirectional — any future interaction (fine-grained cancellation, user input) will go through separate REST requests; long-lived SSE connections require careful management of buffers and intermediate proxies (keep-alive, timeouts).
- **Accepted risks**: the historical browser-side limit on simultaneous SSE connections per domain over HTTP/1.1 — mitigated by HTTP/2 (multiplexing), which de facto becomes the recommended deployment target; the single port concentrates all control plane traffic, sizing to be monitored (§22.2).
