# ADR-004 — Runtime: Docker standalone confirmed, Kubernetes ruled out, Swarm not reimplemented

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.4, §3.5, §16.1(3)(6), §16.2, §18.1, §26.1

## Context

The target runtime must be chosen: Docker standalone (Engine/Compose), or an orchestrator (Swarm, Nomad, Kubernetes — including an "embedded and transparent" Kubernetes such as k3s) to get scheduling and high availability. The product's value proposition rests on modest VPSes (from 2 GB of RAM), reversible standard Docker objects and a catalog of compose templates.

## Decision

**Docker standalone is confirmed as the runtime**: Docker Engine, Compose and BuildKit.

**Kubernetes is ruled out**, including in an "embedded and transparent" form: it contradicts the value proposition (modest VPSes from 2 GB, reversible standard Docker objects §16.1(3), catalog of compose templates) and the abstraction would leak at the first incident — pods, PVCs, ingresses would surface in front of users who chose the platform precisely to avoid learning Kubernetes.

**Swarm is not reimplemented**: at best a deprecated compatibility, behind a feature flag, in P3.

An orchestrator will only be **re-evaluated upon validated user need**, via the **runtime adapter contract** (§18.1) — all calls to the runtime go through a single adapter, instrumented and secured — and **without ever being imposed on existing installations**. This ADR records this rejection and its reasons.

## Alternatives considered

- **Embedded Kubernetes (k3s or equivalent), hidden by the UI**: rejected — memory footprint incompatible with a 2 GB VPS, loss of the "everything is standard Docker" reversibility, and an abstraction that leaks at the first incident (pods, PVCs, ingresses).
- **Docker Swarm reimplemented properly**: rejected — deprecated by the reference itself, multi-node storage unsolved, declining ecosystem.
- **Nomad**: rejected — an additional orchestrator to install and learn, with no validated user demand; re-evaluable via the runtime adapter.

## Consequences

- **Positive**: minimal footprint on target servers; all resources remain standard Docker/Compose objects, manageable outside of AkerDock (§16.1(3)); direct parity with the compose template catalog; simple diagnostics for the user.
- **Negative**: no automatic multi-node scheduling, no native HA for an application (multi-server remains build → push registry → pull, with an external load balancer, as in the reference §3.3); no auto-scaling.
- **Accepted risks**: users whose needs evolve toward orchestration will have to move off AkerDock or wait for a re-evaluation upon validated need; the runtime adapter contract must remain genuinely watertight so that this re-evaluation remains possible without rewriting the business services.
