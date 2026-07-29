# ADR-048 — Applying a configuration without rebuilding: the deployment that skips the build

- **Status**: Accepted
- **Date**: 2026-07-29
- **Related ADRs**: [ADR-006](ADR-006-rollback-artifacts.md) (redeploying a verified artifact
  without a rebuild — the same mechanism, aimed backwards)
- **Related PRD sections**: §5.3, §5.5, §20.2, §21.1, §21.2

## Context

An operator edits an environment variable and presses **Restart**. Nothing happens — not
visibly, not in the container. A Docker container freezes its environment when it is
**created**; `docker restart` starts the same container again, and the process comes back to
the values it already had. The same is true of the image, of the port mapping, of every flag
that was on the `docker create` line.

That is exactly what AkerDock did: `application.restart` ran `docker restart` on the
container (single application), on every container of a stack by label (compose), and on the
database container. The only code path that ever applied a variable was a **deployment** —
`renderRuntimeEnv` writes `env/runtime.sh` on the server and the container is created again
against it.

So the honest instruction was "deploy again". For a git-sourced application that means a
clone and a full build — minutes of work, a new image, cache pressure and a new artifact —
to change a string the build never sees. Operators do it anyway, which makes the build
system the tax on editing a variable.

Two facts make the fix small rather than structural:

1. The engine **already knows** how to deploy without building. A rollback (ADR-006) skips
   clone and build, verifies that the image is present, and runs the rest of the pipeline
   unchanged — env file, create, health check, switchover, routing.
2. The compose path **already knows** what changed. Each service carries an
   `akerdock.config_hash` label over its rendered create command and its environment; a
   service whose hash is unchanged is left alone, and one whose variable moved is recreated.

What was missing was a way to ask for that pipeline without a build, and a place in the UI
to ask for it.

## Decision

**A deployment may skip the build.** `deployments.skip_build` marks a deployment that
reruns the pipeline over the artifact already running, with the configuration as it stands
now.

1. **`skip_build` is a column, not a reuse of `is_rollback`.** A rollback means "an earlier
   image", and the history, the UI and the retention rules read it that way. Applying a
   configuration is not going back.
2. **The trigger is `config_apply`** — a value the vocabulary has carried since the first
   migration (`deployment_trigger`) and that nothing produced until now.
3. **Nothing is rebuilt, and nothing is re-resolved.** The image comes from the current
   artifact (`GetCurrentArtifact`, scoped to the preview for a PR instance) and the commit
   from the last succeeded deployment. A branch that moved since is **not** deployed:
   applying a configuration must not smuggle in code nobody asked for. For a compose stack,
   each service reuses the image tagged for that commit — build or pull alike, since pulling
   a mobile tag would swap the artifact under an action that promised not to.
4. **The artifact must exist.** No successful deployment to stand on, or an image reclaimed
   by the retention, is a `409` naming what to do (deploy once first), never a 500 and never
   a silent rebuild.
5. **It produces no artifact and reclaims nothing.** It added no image; recording one would
   duplicate a rollback candidate, and pruning would count an image twice.
6. **`force_rebuild` and `skip_build` are mutually exclusive** — one rebuilds everything, the
   other builds nothing — and asking for both is `422`.
7. **`restart` keeps its meaning.** It restarts the container as it stands. Making it
   silently recreate would turn the cheapest operation into a deployment, and would take
   away the one verb that means "bounce this process". The UI names the difference instead
   of hiding it: **Recreate (apply config)** sits next to **Restart**, each with a hint.
8. **A PR instance gets the same verb** (`POST …/previews/{uuid}/redeploy`), scoped to its
   own variables (INV-010) and its own artifact (INV-011). It redeploys the instance at the
   head SHA it is already pinned to — the pull request is **not** re-read; that is what
   `POST …/previews` does. A fork awaiting approval stays refused (§20.4.8).

## Consequences

- **Positive**: editing a variable costs a container recreation instead of a build. The
  pipeline is the existing one, so health checks, zero-downtime switchover, routing and the
  post-deployment hook behave exactly as on a normal deployment — no second, weaker code
  path that drifts. The deployment history records what happened (`config_apply`,
  `skip_build`), where a restart left no trace of an intent that had no effect.
- **Negative**: a recreation is not free — the container stops and starts, and an
  application without a health check falls back to stop-then-start (§7.4), which is a brief
  interruption where `restart` was already one. The operator now has one more verb to
  choose from; the hints in the menu are what make that choice, and they have to stay
  accurate.
- **Accepted risks**: `skip_build` on a compose stack still clones — the compose file is
  needed to plan the stack — so it is cheap, not free. And if the image for the deployed
  commit is gone from the server, the stack rebuilds it rather than failing: refusing would
  break the action whose whole point is to be the cheap one, but it means a "no build"
  request can, in that one case, build.

## What this does not fix

**Databases keep the bug.** `UpdateDatabase` answers `restart_required: true`, and the UI
says "a configuration change is waiting for a restart" — but `database.restart` runs
`docker restart`, so the new password, image or `postgresql.conf` are not applied either.
The fix there is different (rerun `provision`, which is idempotent and already recreates the
container) and it touches the credential and TLS paths, so it is its own change rather than
a rider on this one. The contract stays wrong until then, which is stated here so it is not
mistaken for a decision.

## Alternatives considered

- **Make `restart` recreate the container when the configuration changed** (compose's own
  semantics, via the `akerdock.config_hash` label): the most convenient behavior, and the
  one most likely to surprise. "Restart" would sometimes be a deployment — with a health
  check, a switchover and a rollback candidate — and sometimes not, depending on a hash the
  operator cannot see. Rejected in favor of two verbs that each mean one thing.
- **Always recreate on `restart`**: predictable, but it takes away the cheap bounce and
  makes every restart pay for a create + health check + switchover.
- **Only flag the drift in the UI** ("variables changed — redeploy required", the
  `restart_required` pattern the databases already use): honest, but it leaves the operator
  with a full rebuild as the only way to act on the warning. The warning was never the
  missing piece; the cheap action was.
- **Rebuild without cache** (`force_rebuild`, which already existed but was never wired into
  the dashboard): applies the variables as a side effect of doing the most expensive thing
  possible. It is exposed now as its own menu entry, for when the build itself is what is
  suspect.
