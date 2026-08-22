# ADR-078 — A source install propagates itself: servers rebuild the instance's own commit

- **Status**: Accepted
- **Date**: 2026-08-17
- **Related**: [ADR-051](ADR-051-docker-runtime-adapter.md)/[ADR-054](ADR-054-agent-host-ops.md)
  (why every managed server must run the AkerDock image), [ADR-036](ADR-036-scale-to-zero-waker.md)
  ("empty AKERDOCK_IMAGE disables provisioning with a clear error, never a guessed registry" —
  the stance this ADR keeps), [ADR-039](ADR-039-postgres-major-upgrade-in-place-optin.md)
  (precedent: `install.sh` decisions go through an ADR),
  [ADR-076](ADR-076-non-root-escalates-through-sudo-n.md) (the sudo ladder the seed fallback
  reuses on the remote side)
- **Related PRD sections**: §3.1, §22.4, §26

## Context

The reference distribution pulls `ghcr.io/deepteams/akerdock` — multi-arch, built by the
release workflow. `install.sh` is the other lane: it builds the image **from the checkout**,
tags it `akerdock:<git describe>`, and points `AKERDOCK_IMAGE` at that local tag — exactly
right for a developer on a branch: no registry, no publishing, the running commit is the
checked-out commit.

That lane was silently **single-server**, and worse, **single-architecture**. The agent rides
`AKERDOCK_IMAGE` onto every managed server via `docker run` (ADR-051/054), which pulls from a
registry when the image is absent — and a locally built tag exists in no registry, so
onboarding any server beyond `localhost` died at `agent deploy` with a pull error naming
neither cause nor fix. The shortcuts do not exist: a binary knows its version and commit
(`-ldflags`) but does not contain its sources, so the running instance cannot rebuild itself;
and `docker save | docker load` of the instance's own image ships the instance host's CPU
architecture — wrong in precisely the topology this matters for (an amd64 edge, an arm64 GPU
box).

Two facts make the real fix natural. The repository is **public** — any managed server can
fetch any pushed commit. And each server already runs a Docker daemon that builds natively
for its own CPU, from a Dockerfile whose toolchains run on `$BUILDPLATFORM` and whose output
is decided by `$TARGETARCH`. The machine that needs the image is the best-placed machine to
build it — what it lacked was knowing *which commit it is supposed to be*.

## Decision

### 1. The build carries its own source coordinates

`install.sh` bakes the exact commit into the image (`-ldflags -X main.commit`, via a `COMMIT`
build arg the release workflow also fills with `github.sha`). A **dirty tree bakes an empty
commit, deliberately**: a build that matches no commit must not claim one. The repository URL
defaults to the canonical public one, overridable by `AKERDOCK_SOURCE_REPO` — a fork is a
one-variable change, not a patch. Both are process-wide facts of the running binary, set once
at boot (`jobs.SetAgentSource`), like the version they travel with — not a dependency to
thread through nine call signatures.

### 2. A server that lacks the image builds it, at that commit, for its own CPU

`AgentEnsureCommand` — the single rendering every agent deploy crosses: validation, the
scheduler's cross-server reconciliation, the scale-to-zero ensure — gains a prelude, emitted
only when a source commit is known:

```
docker image inspect <image> || docker build -t <image> … <repo>#<commit> || exit $?
```

The server clones the public repository at the instance's exact commit and builds natively —
no emulation, no transfer, no registry, and by construction the same bytes of source as the
instance. `./install.sh` therefore **propagates by itself**: the tag changes with the
checkout, the reconciler notices the difference on every server, finds the tag absent, and
rebuilds it there. A failed build stops the whole ensure with the *build's* exit and stderr —
never a follow-up `docker run` whose pull error would bury the real cause. Absent a commit,
the prelude is empty and the command is exactly what it always was: registry installs are
untouched, and ADR-036's stance stands — the platform never guesses a registry; here it does
not guess, it *knows*, because the coordinates were baked at build time.

First build on a server costs minutes (toolchain images, npm); Docker's layer cache pays it
once. A job cut mid-build resumes against that cache on retry.

### 3. What git does not hold ships by hand: `./install.sh seed user@server`

Uncommitted or unpushed work exists nowhere a server can fetch. For it, the checkout — the
only place those bytes live — cross-builds the target's architecture locally (buildx,
`--output type=docker,dest=-`: the stream **never touches the local image store**, whose tag
the running stack itself uses) and pipes the tarball over SSH into the server's daemon,
climbing the ADR-076 docker → `sudo -n` ladder. A clean tag already present is skipped; a
`-dirty` tag is always rebuilt and shipped, because it names nothing. `install.sh` warns at
update time when propagation cannot work — dirty tree, or a commit no remote holds — naming
this fallback.

### 4. Validation says which situation you are in

The `agent deploy` failure classifies the output (pull denied, ref not found, …) and names
the remediation for the actual case: commit known → "is that commit pushed?"; no commit →
"this instance was built from a dirty tree — commit and push, or seed".

### What this ADR does not decide

No registry container shipped by the platform (a registry is somebody's whole product, and
the published ghcr image already serves the non-development case). No source distribution by
the control plane (it has no sources). No private-repository support for the rebuild — the
mechanism assumes what the project is: public; a private fork uses the seed, or its own
registry.

## Verification

- Unit: the prelude renders with repo#commit, quoted build args and the `|| exit $?` gate,
  and renders nothing without a commit (`TestAgentEnsureCommandBuildsFromSourceWhenKnown`);
  the failure hint asks "is that commit pushed" when the commit is known and names the seed
  when it is not (`TestServalcovProvisionAgent`).
- `install.sh`: POSIX `sh -n` clean; seed and propagation are operator-exercised (two Docker
  daemons and an SSH hop — exactly what the unit lane, by ADR-026, does not simulate).
