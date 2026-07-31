# ADR-052 — Agent command channel: typed method frames on the observation rail

- **Status**: Accepted
- **Date**: 2026-07-31
- **Revises**: [ADR-036](ADR-036-scale-to-zero-waker.md) (§ "creates, deletes and builds nothing"), [ADR-040](ADR-040-server-agent-outbound-observations.md) (§4 token scope, §5 "every action remains SSH-initiated"), [ADR-041](ADR-041-agent-websocket-channel.md) (§4 ladder, §5 deferred commands)
- **Related**: [ADR-001](ADR-001-transport-ssh-then-agent.md), [ADR-051](ADR-051-docker-runtime-adapter.md)

## Context

ADR-041 shipped one persistent outbound WebSocket per server and explicitly
reserved the command protocol for "the next ADR, riding this same rail once
the channel has proven itself". ADR-051 then decided the execution model:
Docker operations are typed, method-level commands executed by the agent
against the local daemon, with no CLI or SSH-socket fallback. This ADR is that
reserved command protocol.

## Decision

1. **Same rail, one more subprotocol.** The channel stays
   `GET /agent/v1/ws`, same `akda_` per-server bearer token. An agent that
   holds an executor offers `akerdock-agent-v2` before `akerdock-agent-v1`;
   the control plane's preference order picks v2. Either side speaking only
   v1 degrades the connection to observations — no flag day, no second
   socket. Observation batches and their acks keep their v1 semantics
   verbatim inside v2.
2. **One wire module.** `internal/agentwire` defines the frames — `cmd`,
   `res`, `stream`, `cancel`, next to the v1 `observations`/`ack` — and the
   enumerated method vocabulary: exactly the `dockerruntime.Runtime` methods,
   whose params and results are the Docker SDK types, i.e. the Engine API's
   own wire representations. The executor refuses any method outside the
   list. Daemon errors are flattened to codes (`not_found`, `conflict`, …)
   and re-wrapped on arrival, so `IsNotFound`/`IsConflict` answer identically
   on both sides of the channel.
3. **Multiplexed by id.** Commands carry a control-plane-assigned id; the
   agent runs each on its own goroutine and both sides serialize writes, so a
   long `ContainerWait` never blocks a `Ping`. A `cancel` frame aborts the
   command's ctx. The one-batch-in-flight observation discipline is
   unchanged; under v2 the agent's read loop routes the acks.
4. **Streams.** `ContainerLogs`, `ImagePull`, `ImagePush` and `Events` open
   with an acknowledging result, then flow as 32 KiB chunks until a clean
   `eof` or an error chunk. Backpressure is deliberately coarse in this
   revision: the control plane buffers a bounded window per stream and kills
   an overflowing stream with an explicit error — one slow log follower must
   never stall a server's whole channel. Credit-based flow control can come
   later without a wire break (a new frame type).
5. **Exec attach stays out.** The hijacked bidirectional terminal stream
   (`ContainerExecAttach`) is deferred to the terminal migration slice; until
   then it answers `unimplemented` and container terminals stay on SSH.
6. **Security model.** The channel now carries mutations: this supersedes
   ADR-036's "creates, deletes and builds nothing" and ADR-040's
   "mutate nothing" token scope. What is kept: the agent still *decides*
   nothing — every action is control-plane-initiated, the agent executes
   enumerated, auditable, atomic Docker calls against the socket it always
   held. A stolen `akda_` token still reaches exactly one server's daemon —
   the same blast radius the socket mount always implied — and the ingestion
   side still scopes every observation query by server id.
7. **The agent is the only Docker path.** Superseding ADR-040 §6 / ADR-041
   §4 for Docker operations: no fallback below the channel. A missing channel
   surfaces as a typed unavailable error; the remedy is the agent's
   reconciliation (`wakerSpec` bumped to 6 so every fleet member is recreated
   offering v2). Observations keep their WS → POST ladder untouched.
8. **Topology.** Command traffic terminates on the process holding the
   WebSocket (`AgentConns`, per-process, latest connection wins). In split
   api/worker deployments, a worker reaches a channel through an internal
   relay exposed by the api process — decided here, delivered with the first
   job-side consumer slice, since handlers (this revision's consumers) run
   where the socket lands.

## Consequences

- `internal/handlers.AgentConns.Runtime(serverID)` hands any caller a
  `dockerruntime.Runtime` executing on that server — the migration's
  entry point for call sites (read-only handlers first).
- The agent (`internal/waker`) gains an executor and a channel read loop;
  its wake path is untouched and every loop stays panic-contained.
- Remaining for the next slices: the worker→api relay, extending
  `ensureAgents` beyond proxy-bearing servers to every managed server, and
  per-operation telemetry (`docker_runtime_ops_total`).
- Unit coverage: wire error round-trips, executor dispatch/streams/cancel,
  control-plane conn routing (including slow-consumer stream kill), and an
  end-to-end in-process rail test (agent + executor + fake daemon).
