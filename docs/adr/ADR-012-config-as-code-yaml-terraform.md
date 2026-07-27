# ADR-012 — Configuration as code: YAML export + idempotent apply + official Terraform/OpenTofu provider

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.12, §24.5, §22.4, §12, §24.1

## Context

A platform driven solely through the UI offers no native declarative configuration (§12). For teams that version their infrastructure, the absence of reproducible export/apply is an adoption blocker and a source of drift between environments. A decision is needed on whether AkerDock remains UI/API-first or offers a real declarative configuration contract.

## Decision

Decision recorded (detailed requirements in §24.5):

- **Complete YAML export** of all of a team's non-secret configuration (projects, environments, resources, domains, non-secret variables, backup plans, scheduled tasks), in a format that is **stable and versionable in Git**, a versioned contract with a published schema, subject to the same compatibility policy as the API (§22.4).
- **Idempotent apply**: submitting the YAML converges the state — creation, update, and deletion **only on explicit request**; a **dry-run** mode producing the full diff; conflicts detected via optimistic versioning (§24.1); apply audited and executed as a visible job.
- **Secrets are referenced** (name + version), never inline; their values go exclusively through the dedicated endpoints.
- An **official Terraform/OpenTofu provider** is built on the API and covers at minimum the P0/P1 scope.

## Alternatives considered

- **UI/API only (parity)**: rejected — reproduces a known gap of the reference; configuration drift between environments then remains invisible and irreversible.
- **Community Terraform without official commitment**: rejected — quality and coverage not guaranteed; an official provider is a signal of commitment and a product tested against the API.
- **Rich proprietary format (Pulumi-style DSL / full GitOps operator)**: rejected — the exported YAML + idempotent apply covers the need without imposing an additional runtime; a GitOps flow can be built on top by the user.

## Consequences

- **Positive**: configuration that is versionable and reviewed in PRs; reproducible environments; dry-run/diff before application; escape from lock-in (with the export of §22.4); the official Terraform provider relies on the public API, guaranteeing that everything the UI does is scriptable (§25.2).
- **Negative**: the YAML format becomes one more public contract to evolve with backward compatibility; the convergence logic (diff, application order, explicit deletions) is an engine in its own right to design and test (round-trip export→apply required, §26.2); the Terraform provider is an additional deliverable and release cadence.
- **Accepted risks**: possible divergence between the YAML schema, OpenAPI and the internal model if generation is not tooled; since deletions remain explicit, an "orphaned resources" drift may persist deliberately — this is an accepted safety choice.
