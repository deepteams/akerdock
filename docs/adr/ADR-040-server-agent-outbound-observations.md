# ADR-040 — The waker becomes the server agent: outbound observation push

- **Status**: Accepted
- **Date**: 2026-07-28
- **Extends**: [ADR-001](ADR-001-transport-ssh-then-agent.md) (stage 2), [ADR-036](ADR-036-scale-to-zero-waker.md)
- **Related PRD sections**: §18.1, §18.3, §21.2, §16.1, §27.1

## Context

The SSH push model (ADR-001) polls observed state: the control plane learns what
happens on a server only when a scheduled scan reads it over SSH. ADR-001
accepted this explicitly ("no pushed Docker events at first") and named an
**outbound agent** as the long-term direction, without designing it. The cost is
now measured in production: scale-to-zero status transitions reach the UI up to
a minute after the fact, observed container states lag behind reality, and every
tick spends one SSH round-trip per server whether anything changed or not.

Meanwhile ADR-036 put a helper on the servers: the waker — the release binary in
a dedicated mode, holding the local Docker socket, provisioned and upgraded over
SSH by the control plane (image + `wakerSpec` reconcile), already producing the
very observations we miss (activity, wakes, sleeps). The operator constraint is
firm: **no third helper**. `proxy + waker + agent` is not an acceptable server
footprint; either the waker takes the role, or an agent replaces the waker.

## Decision

The waker mode absorbs the agent role — same image, same container, one helper
per server. Phase 1 is **outbound observations only**; the command channel that
would replace inbound SSH is explicitly out of scope (a future ADR).

1. **One helper**: the `akerdock-waker` container (name kept for reconcile
   continuity) gains the agent capability. It is deployed on **every managed
   server**, no longer only where scale-to-zero is enabled. Its wake role is
   unchanged (ADR-036/037 stand).
2. **Outbound observations over HTTPS**: the agent POSTs to the control plane's
   single port (§16.1(6)), under a dedicated path (`/agent/v1/*`) outside the
   user-facing OpenAPI contract (like `/auth`). It pushes, batched with
   at-least-once semantics and backoff: Docker state transitions of
   `akerdock.managed=true` containers (from `docker events`), scale-to-zero
   activity/wake/sleep transitions, and a periodic heartbeat. No long-lived
   duplex channel: plain requests are enough for observations and keep the
   surface auditable.
3. **Enrollment through the existing provisioning**: the control plane already
   creates the helper over SSH — it injects the instance URL and a **per-server
   agent token** as environment at (re)creation, and rotates the token on every
   recreate. No pairing flow, no shared secrets across servers.
4. **Token scope — observations only**: agent tokens are a new principal type,
   bound to one server, allowed exactly one thing: submitting observations for
   that server. They can read nothing, mutate nothing, mint nothing. The
   control plane treats agent input as **untrusted hints scoped to that
   server**: it may refresh observed state and emit SSE events from them, and
   keeps SSH as the authoritative read whenever a *decision* depends on state
   (sleep decisions keep their SSH read).
5. **§18.1 amendment**: "the server never contacts the control plane" becomes
   "the server pushes only unprivileged observations; every action remains
   control-plane-initiated (SSH)". INV-007 (the control plane never proxies
   application traffic) is untouched.
6. **Degradation is the SSH status quo**: an old helper image, blocked egress
   or a down control plane MUST degrade to today's behavior — the SSH scans
   remain as reconciliation and fallback. The agent is an accelerator, never a
   dependency; a silent agent is a signal, not an outage.
7. **Isolation from the traffic path**: the push loop runs apart from the wake
   proxy (panic-contained, bounded queue, drop-oldest on overflow). A slow or
   unreachable control plane must never delay a proxied request or a wake.

## Alternatives considered

- **A dedicated agent container next to the waker**: rejected — third helper to
  deploy, supervise and upgrade on every server; the operator constraint that
  motivated this ADR.
- **Shorter SSH polling (seconds)**: rejected — O(servers) SSH churn per tick,
  still not real-time, and it scales exactly the wrong way (cost grows with the
  fleet whether or not anything changes).
- **A duplex channel (WebSocket/gRPC) from day one**: rejected for phase 1 —
  observations need only outbound requests; the duplex channel belongs to the
  phase-2 command-channel decision and would front-load enrollment complexity
  ADR-001 deliberately deferred.
- **Forward-auth signal only**: the preview SSO forward-auth hop already gives
  the control plane a per-request signal and can flip `sleeping → waking`
  instantly; kept as a tactical interim, but it only covers SSO-protected
  previews — neither basic-auth previews nor production apps.

## Consequences

- **Positive**: observed states and scale-to-zero badges become real-time for
  every resource on every server; SSH polling shrinks to reconciliation; the
  fleet-wide agent presence prepares phase 2 (inbound SSH surface reduction,
  ADR-001's goal); still exactly one AkerDock helper per server.
- **Negative**: servers now hold an instance URL and a credential — minimal in
  scope, but a real addition to the threat model (§21 gains an "agent" trust
  boundary: ingestion endpoint authenticated per server, rate-limited,
  idempotent under at-least-once delivery); a new internal API surface must be
  versioned (`/agent/v1`) and documented.
- **Accepted risks**: two sources of observed state (agent push + SSH scans)
  need a merge rule — latest observation wins, SSH authoritative on conflict;
  heartbeat absence must be surfaced as "agent silent" rather than treated as a
  server outage (egress may be legitimately blocked).
- **Follow-ups**: §26.2 grid entry; threat-model section; `agent_tokens` in the
  data dictionary; ingestion endpoint spec; phase-2 ADR (command channel) when
  proven demand exists.
