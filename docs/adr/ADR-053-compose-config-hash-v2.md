# ADR-053 — Compose config hash v2: fingerprint the typed create body, frozen v1 as fallback

- **Status**: Accepted
- **Date**: 2026-07-31
- **Related**: [ADR-051](ADR-051-docker-runtime-adapter.md), [ADR-052](ADR-052-agent-command-channel.md)

## Context

The compose pipeline skips a service whose desired state matches what runs:
`composeConfigHash` fingerprinted the fully RENDERED `docker create` shell
command plus the per-service env file. ADR-051/052 replace that command with
a typed create body over the agent channel — the string the hash consumed no
longer executes, and any change to how it would have been rendered would
recreate every service of every stack on the next deployment.

The hash lives as an immutable label (`akerdock.config_hash`) on running
containers: nothing can rewrite it in place.

## Decision

1. **v2 fingerprints the typed body.** `akerdock.config_hash_v2` =
   `"2:"` + sha256 (truncated like v1) of the canonical JSON of the create
   spec (`container.Config` + `HostConfig` + networking aliases), built
   WITHOUT the per-deployment labels — the same invariance rule v1 had. The
   prefix names the format; a v1 value can never collide with it.
2. **v1 freezes as a pure hash input.** `composeCreateCommand` (and the env
   file renderer, `envFlags`, `shellQuote` it depends on) stops executing
   anywhere and survives byte-for-byte identical, ONLY to compute the v1
   fingerprint. Its string-assertion tests become the goldens that freeze it.
3. **Skip decision: v2 first, v1 as the mandatory fallback.** Every
   (re)create writes BOTH labels. A running container with a v2 label is
   judged by v2 alone; one without (created before this rollout — labels are
   immutable) is judged by the frozen v1. The window closes per container at
   its next real change, no deadline needed.
4. **Data-driven retirement.** A skip decided by v1 alone logs
   (`compose skip decided by the v1 config hash`); when that trace stays
   silent across the fleet, the frozen renderer, its goldens and the v1
   fallback are deleted in one commit.

## Consequences

- No mass recreation at rollout: an unchanged stack redeploys to a wall of
  "unchanged" skips, exactly as before, whichever hash decided.
- The typed body becomes the single source of the fingerprint: future spec
  changes (a new field mapped from compose) intentionally change v2 — that
  is a real desired-state change, and the recreate is correct.
- The host loses `runtime.sh` and the per-service env files: the values ride
  the create body over the encrypted channel (ADR-051's INV-003 stance); the
  frozen v1 renderer still mentions their PATHS, as pure strings.
- Unit coverage: v2 determinism/sensitivity/prefix, the six-case skip
  decision, and the v1 goldens.
