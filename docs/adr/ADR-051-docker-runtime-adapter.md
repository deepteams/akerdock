# ADR-051 — Docker runtime adapter: official SDK behind one interface, executed by the agent

- **Status**: Accepted
- **Date**: 2026-07-31
- **Related**: [ADR-001](ADR-001-transport-ssh-then-agent.md), [ADR-004](ADR-004-standalone-docker-runtime.md), [ADR-005](ADR-005-builds-buildkit-rootless-untrusted-code.md), [ADR-021](ADR-021-compose-distribution-two-services.md), [ADR-036](ADR-036-scale-to-zero-waker.md), [ADR-040](ADR-040-server-agent-outbound-observations.md), [ADR-041](ADR-041-agent-websocket-channel.md)

## Context

ADR-004 and PRD §18.1 mandate that "all calls to the runtime go through a single
runtime adapter, instrumented and secured". That adapter was never built: today
every Docker operation is a shell string composed inline and executed over SSH
(`internal/sshexec`) — roughly two hundred `docker …` invocations across
twenty-six files. Errors are recognized by matching stderr text, state is read
by parsing `--format` templates, and tests assert on generated command strings.

Meanwhile ADR-040/041 gave every managed server an agent (the waker, a mode of
the single binary) holding the local Docker socket and a persistent outbound
WebSocket to the control plane — deliberately restricted to observations, with
the command channel "the next ADR" (ADR-041 §5). The waker talks to the daemon
through a hand-rolled HTTP client (`SocketDocker`) whose only reason was to
keep the binary free of the full Docker SDK.

Two facts shape the target:

- The Engine API's Go SDK types **are** the API's wire types: an interface
  mirroring the SDK serializes to a typed command frame for free.
- Build contexts live on the target server (`git clone` on the host), so
  builds can never be driven from the control plane through the Engine API
  without shipping the context across the wire.

## Decision

1. **One adapter.** `internal/dockerruntime.Runtime` is the single Docker
   runtime adapter (PRD §18.1). Its method set is the strict subset of the
   Engine API the codebase uses, with the official SDK's signatures and types
   verbatim. Business logic never touches a transport, a shell string or the
   SDK client directly.
2. **The official SDK.** `github.com/docker/docker/client` is the only
   implementation of daemon access. The waker's hand-rolled `SocketDocker` is
   retired; the waker consumes the adapter over the local socket, restricted
   to start/inspect/stop/list/events by the narrow interface it is handed —
   the ADR-036 §8.1 code limitation moves up one layer and survives.
3. **Execution model: typed commands to the agent.** The target architecture
   is control plane → typed method-level command → agent → local socket. The
   agent executes atomic Engine API calls and decides nothing; all business
   logic (rolling switches, prune policies, health waits) stays central
   (ADR-001). Commands are an enumerated, auditable vocabulary — not an opaque
   proxied root socket. The channel protocol (frames, streams, replica relay,
   per-command authorization) is the next ADR, riding the ADR-041 rail; that
   ADR is also where ADR-036's "creates nothing" invariant and ADR-040/041's
   "accelerator, never a dependency" stance are formally superseded.
4. **The agent is mandatory for Docker operations.** Once an area migrates,
   there is no CLI or SSH-socket fallback for it: an unreachable agent fails
   the operation with a clear error, and the existing reconciliation
   (`ensureAgents`, extended to every managed server) repairs the agent. SSH
   remains for exactly: server first contact and validation, agent
   bootstrap/repair/upgrade (which can never depend on the agent), host-side
   operations (file deposits, git, backup pipelines) until their own
   migration, and operator break-glass.
5. **Builds target BuildKit driven by the agent.** `ImageBuild` is
   deliberately absent from the adapter: the legacy builder is not BuildKit
   (ADR-005) and the context is server-local. The target is a build command
   executed agent-side with the BuildKit client against the local daemon,
   streaming progress back; until that phase ships, builds keep the current
   `DOCKER_BUILDKIT=1 docker build` over SSH.
6. **Secrets ride the command body (INV-003 clarified).** `ContainerCreate`
   carries env values inside the typed command over the encrypted channel —
   never on argv, never in shell history. At-rest exposure on the host is
   unchanged (`docker inspect` always showed `Config.Env`); only the transport
   of the values changes.
7. **Registry auth is per request.** API-path pulls and pushes authenticate
   with `RegistryAuth` headers from control-plane-held credentials; nothing is
   persisted in the host's docker config. The CLI `docker login`/`logout`
   dance survives only on the build path, and dies with it.
8. **Binary weight is accepted.** Linking the SDK grows the stripped static
   binary by ~0.7 MiB (35.5 → 36.2 MiB, ~2%); the minimalism that justified
   `SocketDocker` is obsolete the moment the control plane links the SDK
   anyway (one binary, ADR-021's distribution format is unaffected).

## Consequences

- Call sites migrate once, to the interface; where an operation executes is
  decided by the implementation handed to them — local socket today, typed
  command channel next. Migration proceeds in shippable slices (read-only
  handlers first, compose deploy and its config-hash migration last).
- Errors become typed (`IsNotFound`/`IsConflict`/`IsNotModified` over
  `errdefs`) instead of stderr grepping; multiplexed log streams demux through
  one adapter (`Demux`) that preserves the merged-stream callback contract job
  code already has.
- Tests assert on recorded typed calls (`dockerruntime/fake`) instead of
  substring-matching shell strings; a `dockerintegration`-tagged tier
  (`make test-docker`) exercises the real daemon from the adapter package
  only.
- Shell one-liners (`a && b || true`) decompose into sequenced calls with
  explicit idempotence; compound atomicity must be reconstructed in Go where
  it mattered.
- The waker loses its bespoke client (~240 lines) and its behavior is
  unchanged; `AKERDOCK_DOCKER_SOCKET` and `AKERDOCK_DOCKER_API_VERSION` keep
  their meaning (empty version now negotiates instead of sending unversioned
  requests).
