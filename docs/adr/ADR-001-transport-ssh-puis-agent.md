# ADR-001 — Control transport: SSH first, outbound agent as target, configurable ports

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.1, §3.1, §4.1, §10.4, §16.1(2)(6), §18.1, §25.2

## Context

Driving target servers over SSH (push from the instance) is the simplest model to operate: nothing to install on the target, nothing to version. But it requires an open inbound port on every server and does not allow receiving Docker events continuously (state is *polled*, never *pushed*). An outbound (pull) agent would reduce the inbound surface and open the way to real-time events, at the cost of versioning, enrollment and upgrading an agent. The transport model must be settled and, as a corollary, the network surface exposed by the platform.

## Decision

Direction validated in two stages:

1. **SSH for initial parity**: the remote transport is an abstract interface whose initial implementation is SSH (§18.1). Any Linux server reachable over SSH can be managed, as in the reference.
2. **Outbound agent as target**: an optional outbound agent is the long-term direction to reduce the inbound surface of target servers and enable Docker event reporting. It can be added without modifying the business services, thanks to the transport abstraction.

Associated requirements on ports:

- Each server's proxy listens on **80/443 by default**, but its listening ports **MUST be configurable per server** (for example 8080/8443 when an upstream reverse proxy already holds 80/443).
- The control plane is exposed on **a single port**, behind its own domain/DNS: UI, API and real-time streams share it (§25.2, cf. ADR-024) — one port, one certificate, one firewall rule.

## Alternatives considered

- **Pull agent from the start**: rejected for the first version because it introduces agent versioning, enrollment and upgrading right away, and delays feature parity with the reference.
- **SSH forever, with no agent trajectory**: rejected because it locks in open inbound ports on every target server and rules out real-time Docker events, while surface reduction is a product goal.
- **One port per use (dashboard, WebSocket, terminal)**: rejected — needlessly large network surface and more complex operations (three firewall rules, three certificates), contrary to the "a single exposed port" goal (§16.1(6)).

## Consequences

- **Positive**: immediate parity with the reference (any SSH server is eligible); control plane exposure surface reduced to one port; possible cohabitation with an upstream reverse proxy thanks to configurable proxy ports; the future switch to an agent does not touch the business services.
- **Negative**: the SSH model requires keeping inbound SSH ports open on target servers as long as the agent does not exist; no pushed Docker events at first (polling required for observed state, §18.3).
- **Accepted risks**: maintaining two transports in the long run (SSH + agent) will increase the test matrix; the transport abstraction must be designed from P0 onward to prevent SSH details from leaking into the business services.
