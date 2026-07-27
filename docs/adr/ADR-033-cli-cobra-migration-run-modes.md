# ADR-033 — Moving to Cobra: server modes as subcommands

- **Status**: Accepted
- **Date**: 2026-07-25
- **Related PRD sections**: §12 (official CLI), §18.2 (run modes), §14.1 (installation)
- **Related**: ADR-021 (compose distribution, single binary), ADR-031/ADR-032 (CLI client commands)

## Context

The `akerdock` binary today parses its arguments by hand: the first positional
argument picks the server mode (`all-in-one`, `api`, `worker`, `scheduler`) or the
`healthcheck` subcommand. The local CLI (ADR-031/032) adds a family of client
commands (`login`, `logout`, `context`, `ls`, `logs`, `shell`, `port-forward`, `db`) with
flags, nested subcommands, generated help and completion — which manual parsing
cannot carry cleanly. `github.com/spf13/cobra` is already present in the dependency
tree (as `// indirect`).

## Decision

The binary adopts **Cobra for the entire command tree**, in the single binary (ADR-021).
Server modes become explicit subcommands:

- `akerdock serve all-in-one|api|worker|scheduler` — former positional mode arguments.
- `akerdock healthcheck` — unchanged (probe of the distroless compose healthcheck).
- `akerdock version` — unchanged.
- The client commands from ADR-031/032 are top-level subcommands.

`AKERDOCK_MODE` remains read as the default for `serve` (parity with the existing behavior).

### Compatibility fallback (for the duration of one major version)

An `akerdock all-in-one` (historical positional argument, without `serve`) **MUST** remain
recognized, run the corresponding mode, and emit a **deprecation warning** on
stderr pointing to `serve`. This avoids breaking existing instances at the first
`git pull && ./install.sh` before the compose file is updated.

### Migration of launch artifacts (in the same change)

- `docker-compose.yml`: `command: ["serve", "all-in-one"]`.
- `Dockerfile`: the distroless healthcheck remains `["/akerdock", "healthcheck"]`.
- `install.sh`, runbooks (`docs/runbooks/*`) and ADR-021: launch commands updated
  to `serve …`.

## Alternatives considered

- **Cohabitation (Cobra for the client, server modes positional)**: rejected — two
  parsing styles in the same binary, inconsistent and a source of confusion in the long run.
  The user explicitly decided in favor of full-Cobra.
- **Separate client binary (`akerdockctl`)**: rejected — one more artifact and release
  cycle, contrary to the "one binary" principle of ADR-021.
- **Hard break without fallback**: rejected — would break every deployed instance whose
  compose does not yet have the new command.

## Consequences

- **Positive**: consistent command tree, generated help/completion, a sound base for the
  client commands and v2 extensions (`up`, `env`, `domains`…); Cobra simply moves
  from indirect to direct dependency.
- **Negative**: a breaking migration to coordinate across compose/Dockerfile/install.sh/
  runbooks; a deprecated fallback to carry and then remove at the next major version.
- **Accepted risks**: a transitional window where the old and new invocations
  coexist — bounded by the announced removal of the fallback.
