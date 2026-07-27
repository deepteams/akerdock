# ADR-026 — E2E test strategy: Docker-in-Docker only, residual risk documented

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.26, §29.9, §22.4, §26.2, §26.3

## Context

AkerDock drives real Linux servers: the full matrix (varied OSes, AMD64/ARM64, systemd, reboots, firewalls — §22.4) can only be covered by automated E2E at the cost of VMs provisioned on every run — slow, expensive and poorly parallelizable. Conversely, target servers simulated as containers (Docker-in-Docker) enable fast and free E2E on every commit, but do not faithfully reproduce a real server. The automation strategy must be settled and what it does not cover must be explicitly owned.

## Decision

Automated E2E runs in **Docker-in-Docker only**: target servers are **simulated as containers** — fast, free, **parallelizable on every commit**. The E2E test plan (§29.9) thus covers database engines, build packs, proxies, Git providers and S3 storages.

**Accepted and documented residual risk**: **systemd, real reboots, firewalls, full disks and ARM64 are not covered by automation** — these bug classes will be discovered **in real-world usage or during occasional manual validations**. The OS/architecture matrix remains supported (§22.4) but validated manually, outside automation (§29.9); the parity matrix records this explicitly (§26.2, e.g. "VM/ARM64 not automated, risk accepted §27.26").

## Alternatives considered

- **Ephemeral VMs in CI (cloud or libvirt/Vagrant)**: rejected as a systematic foundation — costly CI minutes, slow runs, limited parallelism; the speed of the feedback loop on every commit takes priority.
- **Hybrid strategy (DinD on every commit + nightly VMs)**: not retained at this stage — it is the natural evolution if the uncovered bug classes materialize, but no VM pipeline is committed to today.
- **Dedicated ARM64 hardware farm**: rejected — disproportionate infrastructure and maintenance cost; ARM64 remains supported but validated manually.

## Consequences

- **Positive**: E2E on every commit, with no infrastructure cost and no parallelism bottleneck; regressions in the core (deployments, proxy, backups, webhooks) are detected immediately; the "at least one representative E2E" requirement of the Definition of Done (§26.3) remains tenable.
- **Negative**: entire slices of the real world are **never** exercised automatically — systemd interactions, behavior after a server reboot, firewall rules, disk filling up mid-deployment, ARM64 specifics; bugs in these classes will reach users before being known.
- **Accepted risks**: this is the heart of the decision — the residual risk (systemd, reboots, firewalls, full disks, ARM64 uncovered) is **explicitly accepted** and documented everywhere it matters (matrix §26.2, test plan §29.9); in return, occasional manual validations remain due, and this decision will have to be revised (new ADR) if real-world usage reveals an unacceptable bug frequency in these classes.
