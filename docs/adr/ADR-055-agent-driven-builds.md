# ADR-055 — Agent-driven builds: registry operations now, BuildKit execution next

- **Status**: Accepted
- **Date**: 2026-07-31
- **Related**: [ADR-051](ADR-051-docker-runtime-adapter.md), [ADR-052](ADR-052-agent-command-channel.md), [ADR-054](ADR-054-agent-host-ops.md); revisits ADR-005 (BuildKit) and the ADR-021 minimality note a second time.

## Context

The build path is the last Docker CLI territory: `DOCKER_BUILDKIT=1 docker
build` (dockerfile, static packaging, per-service compose builds), the
nixpacks invocation, and around them `docker tag`, `docker push`,
`docker image inspect` — and `docker login`/`docker logout`, which park a
registry token in the host's `~/.docker/config.json` for the duration of
every push (INV-003's known worst offender).

ADR-051 deliberately excluded `ImageBuild` from the runtime adapter: the
build CONTEXT lives on the build server (the clone happens there), and no
control-plane call can carry it. The agent, however, sits on that same
machine with the daemon socket and the `/var/lib/akerdock` tree mounted
(ADR-054) — everything a build needs is local to it.

## Decision

Builds move to the agent in two phases; the registry perimeter moves first
because it needs nothing the channel does not already have.

1. **Phase 1 (delivered with this ADR): the registry family rides the
   channel.** The build server's agent executes `ImageTag`, `ImagePush` with
   a per-request `RegistryAuth` (progress streamed like pulls, failures read
   from the stream) and the post-build/post-push `ImageInspect`s.
   `docker login`/`docker logout` are DELETED: no token ever touches any
   host's `~/.docker/config.json`, and there is no logout to forget on a
   cancelled deployment. The pushed digest is picked among the image's repo
   digests by the push repository — the shell era's blind "index 0" could
   hand the target a digest inherited from the base image's registry.
2. **Phase 2 (this ADR's target state): the build invocation itself.** The
   agent drives the daemon's embedded BuildKit directly — the same rail
   `docker build` uses: a gRPC session hijacked over the local Docker API,
   the `dockerfile.v0` frontend solved with the context and dockerfile as
   local directories under the mounted tree, the `moby` exporter landing the
   image in the daemon's store, and progress streamed back as a typed
   command stream. Build-time variables travel in the typed command body —
   plain ones as frontend build-args, secret ones as BuildKit secret
   attachables — which retires `build.env` from the host exactly as
   ADR-051/053 retired the runtime env files. Registry auth for base-image
   pulls is a session auth provider fed per request, never a config file.
   nixpacks keeps its own binary but stops driving Docker: it emits its
   build plan (a generated Dockerfile) and the agent builds it like any
   other.
3. **Out of scope, unchanged.** Acquiring the sources — `git ls-remote`,
   clone, deploy keys, and the file deposits that feed the build — stays on
   SSH: it writes as the SSH user into directories that user owns, and it is
   a different decision (BuildKit's native git contexts could remove the
   clone for plain dockerfile builds, but nixpacks, static packaging and
   compose all need a checkout on disk). A later ADR may move it; nothing
   here depends on it.

## Consequences

- INV-003 gains its last missing piece on the registry side: credentials now
  exist only in flight — decrypted in the control plane, carried once per
  request over the encrypted channel.
- Phase 2 accepts the `moby/buildkit` client dependency in the binary — the
  second and larger revisit of ADR-021's minimality after ADR-051's SDK
  (accepted: one binary that IS the platform beats a shell contract with
  whatever CLI the host carries).
- Until phase 2 lands, the `docker build`/nixpacks invocations remain the
  only Docker CLI the control plane still speaks, all of it over the build
  path's SSH connection; behavior is unchanged there, BuildKit features
  included.
- Rollout: phase 1 requires the build server's agent, which server
  validation provisions and the scheduler reconciles (ADR-054 tranche B) —
  the same mandatory-agent stance as everywhere else.
