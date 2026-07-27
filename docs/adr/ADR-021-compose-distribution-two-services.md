# ADR-021 — Instance distribution: minimal 2-service docker-compose

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.21, §16.1(6), §18.2, §14.1, §27.1

## Context

AkerDock commits to a light footprint: a single Go binary + PostgreSQL, without Redis or an application runtime, a single exposed port, on a 2 vCPU / 2 GB sizing template (§14.1, §16.1(6)). It remains to translate this commitment into a **distribution format** — that is what the operator installs, updates, and backs up.

## Decision

**Minimal 2-service docker-compose**:

1. **the AkerDock image**: static Go binary in a **distroless** image, with the `all-in-one`/`api`/`worker` modes of the modular monolith (§18.2);
2. **PostgreSQL**.

Guaranteed properties: **a single `docker compose up`** to install; **upgrade by changing the tag**; **standard PostgreSQL backups** (no state to back up other than the database and the configuration); **a single exposed port** for the control plane (§27.1).

## Alternatives considered

- **Multi-container stack (app + cache + real-time service + helpers)**: rejected — contradicts the footprint commitment; each additional container is a component to install, supervise, back up, and update.
- **Bare binary (systemd) without Docker**: rejected as the primary mode — shifts PostgreSQL installation and upgrade management onto the user; Docker Compose is already a conceptual prerequisite of the product.
- **PostgreSQL embedded in the AkerDock image (or SQLite)**: rejected — coupling database and application breaks independent upgrades and standard backups; SQLite is incompatible with multi-instance operation (§18.2) and PostgreSQL's central role (queue, leases, outbox — ADR-002).

## Consequences

- **Positive**: minimal installation and mental model (2 services); trivial upgrade/downgrade by tag; reduced attack surface (distroless: no shell or packages); instance backup = standard PostgreSQL backup; direct consistency with ADR-002 (no Redis) and ADR-001/024 (single port).
- **Negative**: distroless complicates in-situ debugging (no shell in the container) — diagnostics go through logs, the API, and external tools; the user manages major PostgreSQL upgrades themselves (procedure to document, §29.10).
- **Accepted risks**: the `all-in-one` mode makes the API and workers cohabit in a single process — the limits of this cohabitation (failure isolation, sizing) are accepted for small installations, with the switch to the separate `api`/`worker` modes remaining the growth path.
