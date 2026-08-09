# ADR-018 — Deployment from the workstation: `akerdock up` with local context

- **Status**: Superseded by [ADR-070](ADR-070-cli-typed-command-groups.md) (2026-08-09) — the
  decision below was never implemented and `akerdock up` leaves the product. The text is kept
  verbatim, as the record of what was decided and why it was withdrawn
- **Date**: 2026-07-11
- **Related PRD sections**: §27.18, §12, §5.1, §26.2

## Context

Requiring an accessible Git repository (public, GitHub App, or deploy key) or an already-published image to deploy anything (§5.1) makes it impossible to prototype from a local directory before having created and pushed a repository. The CLIs of the domain (heroku, fly, etc.) have shown the value of a "push of the current folder". We must decide whether the AkerDock CLI offers this path and with what traceability guarantees.

## Decision

The CLI **MAY push a local context** (`akerdock up`): build pack detection, application creation if needed, build and deployment — intended for **prototyping before connecting a Git provider**.

Traceability safeguards:

- A deployment from a local source is **marked as such in the history**: no Git SHA, a **context digest** replaces it.
- Such a deployment **never enables auto-deploy**: no webhook or automatic triggering can result from it.

## Alternatives considered

- **Strict parity (Git or image only)**: rejected — unnecessary friction at first contact with the product, while the CLI already exists for everything else (§12).
- **Making local push a full-fledged production mode** (watch, continuous sync): rejected — would encourage non-traceable deployments to production; the positioning is explicitly prototyping, the nominal path remains Git.
- **Requiring a local Git commit and pushing the SHA**: rejected — imposes a repository and a committed state for a simple try; the context digest provides sufficient traceability without that constraint.

## Consequences

- **Positive**: minimal time-to-first-deploy (a folder, a command); product evaluation journey without Git configuration; traceability preserved (context digest, explicit marking in the history).
- **Negative**: uploading the build context (size, .dockerignore-style exclusions, streaming to the build server) is an additional ingestion channel to secure and limit; the history must handle deployments without a Git reference (configuration diff without code diff).
- **Accepted risks**: a local deployment is not reproducible from an external source of truth — accepted and signaled by the marking; risk of production use despite the prototyping positioning, mitigated by the absence of auto-deploy and the visibility of the marking in the history.
