# TODO — AkerDock roadmap

Status as of July 31, 2026. This file is the operational tracking of the remaining work;
the functional reference remains the [PRD](docs/PRD.md) (parity matrix §26.2)
and the [ADRs](docs/adr/README.md). What shipped is recorded in the PRD's
tracking grid, not restated here.

**Progress**: **all 228 operations** of [`openapi-v1.yaml`](docs/specs/openapi-v1.yaml)
are implemented, with 89 migrations. The test strategy was simplified
by ADR-028: unit/module tests on every pull request and **a single
product E2E journey** after merge and before release. The v1 contract surface
is therefore complete: what remains below is **depth** (engines,
channels, build packs), not missing endpoints.

---

## Open items

### Product depth

- [ ] The 7 other database engines (MySQL, MariaDB, MongoDB, Redis, KeyDB, Dragonfly, ClickHouse) — **out of v1 by product decision**: the contract records "PostgreSQL only". Adding them requires a spec amendment + an engine abstraction (provisioning, backup, drill): dedicated batch. Volume backups (§20.5) travel with it
- [ ] **One-click catalog** (ADR-010): signed template Git repositories + user repositories
- [ ] **Config as code** (§24.5, ADR-012): YAML export, idempotent apply with dry-run, Terraform provider
- [ ] **`akerdock up` CLI** (ADR-018): deployment of a local context
- [ ] **Caddy** as a second proxy (the IR is already provider-agnostic — ADR-009; conformance holds Caddy to the same fixtures)
- [ ] **Sentinel** (metrics agent) and **log drains** (§3.8, §13)
- [ ] **Coordinated environment deployment** + auto rollback on degraded health (ADR-016)
- [ ] Multi-server HA of a single app (§3.3) — external registry required **or** image distribution (`save|load`, building block shared with scale-to-zero). HA itself stays out of scope (inter-node LB, rolling, placement — Swarm not reimplemented, ADR-004): the registry is only a prerequisite
- [ ] **Scale-to-zero** (ADR-036/037): mechanism implemented; remaining: end-to-end wake validation in the E2E journey, and the deferred multi-server image distribution idea

### Technical debt and hardening

- [ ] Validation of `custom_docker_options` (INV-012) when the field gets exposed
- [ ] **Angular UI — remainder**: i18n (no hard-coded strings); automated a11y (Storybook test-runner/axe in CI — the addon is in place, the automation remains); enriched deployment timeline; Uptime and Shared variables pages (API and TS client ready)

### Docker runtime migration (ADR-051→056) — follow-ups

- [ ] **`make test-docker` against a live daemon before release**: the integration tier covers the whole agent-build rail (BuildKit session, args, secrets, labels) but has only run against the fake so far
- [ ] **Nixpacks plan-emission rework** (ADR-055's noted follow-up): nixpacks still drives the host CLI over SSH and sources a host `build.env` — the last build path with env materialized on a host
