# ADR-016 — Coordinated deployment of an environment: graph, migration hooks, opt-in rollback

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.16, §20.8, §21.1, §26.2

## Context

Deploying resource by resource, with no notion of order, dependencies, or hooks, leaves coordination to the operator: an application and its schema migration, or a frontend depending on an API, must be launched by hand in the right order. For real multi-resource environments, this absence produces partial switchovers and windows of inconsistency. We must decide whether the environment becomes a deployable unit.

## Decision

An environment can be deployed **as a unit** (workflow detailed in §20.8):

- **Explicit dependency graph** between resources, topological order, parallelism within a same level.
- **Migration hooks**: one-shot job executed after build and before switchover (e.g. schema migration); a hook failure prevents any switchover in the environment.
- **Per-level atomic mode** (optional): the traffic switchover waits until all resources of the level are healthy.
- **Automatic rollback on degraded health** (policy **opt-in per application**): after switchover, observation window (bake time) on health checks; in case of degradation, rollback to the previous verified artifact, notified and audited.
- **Explicit partial failure**: detailed environment state (resources deployed / not deployed / failed), possible resumption at the point of failure — never a silent half-switchover.

## Alternatives considered

- **Strict parity (independent deployments only)**: rejected — leaves coordination to users' scripts, precisely what a PaaS must absorb; unit deployments of course remain available.
- **External CI pipelines as the answer**: rejected — an external pipeline has neither visibility into health checks nor control over the proxy switchover; it cannot guarantee "migration before switchover".
- **Automatic rollback enabled by default**: rejected — an unwanted automatic rollback can worsen an incident (data already migrated); it remains opt-in per application.

## Consequences

- **Positive**: reproducible and ordered multi-resource deployments; schema migrations finally find a canonical place (before switchover); rollback on bake time brings "progressive delivery"-style safety without an orchestrator.
- **Negative**: the deployment engine must handle a graph (cycles to detect, levels, parallelism) and composite environment states in addition to unit states (§21.1); the UI must represent a multi-resource timeline; demanding E2E proof (§26.2: graph + migration hook + rollback on health).
- **Accepted risks**: the per-level atomic mode holds healthy resources waiting for the others — switchover latency accepted when the option is chosen; an automatic rollback does not undo the side effects of an already-executed migration (backward compatibility of migrations remains the user's responsibility).
