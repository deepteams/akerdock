# ADR-013 — Adoption of existing resources without redeployment, previewed and reversible

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.13, §20.7, §27.23, INV-015

## Context

No platform in the segment knows how to take control of a container or compose stack that is **already deployed**: the user has to recreate everything in the platform and then redeploy, with interruption and risk to data. Yet AkerDock already structurally distinguishes managed from unmanaged resources (INV-015). A decision is needed on whether the platform can "adopt" existing workloads, which also conditions the migration strategy (cf. ADR-023).

## Decision

**Adoption without redeployment, previewed and reversible** — it is also the entry path from any platform (ADR-023). The workflow is that of §20.7:

1. **Scan** of a server: inventory of unmanaged containers and compose stacks (relies on INV-015).
2. **Proposed mapping** to the AkerDock model: application or service, networks, volumes, variables, ports and domains detected by inspection and labels.
3. **Preview**: what will be managed, what will be modified (labels/metadata added), what is not adoptable and why.
4. **Adoption without redeployment**: taking control without restarting the workload when possible; the first redeployment fully normalizes the resource.
5. **Reversibility**: "unadopting" returns the resource to its unmanaged state without destroying it.

Acceptance criteria (§20.7): adopt a multi-service compose stack with volumes then redeploy it without data loss; a non-representable resource is flagged with the reason, never silently partially adopted.

## Alternatives considered

- **No adoption (parity)**: rejected — migrating to AkerDock would require redeploying everything, maximum friction precisely at the moment you want to convince.
- **Import by assisted recreation (a wizard that regenerates the resource and redeploys)**: rejected as the primary mechanism — service interruption and risk to volumes; recreation remains available via the first normalizing redeployment.
- **Silent automatic adoption of everything that is running**: rejected — would violate the managed/unmanaged boundary (INV-015) and create non-consented takeovers; adoption is always explicit and previewed.

## Consequences

- **Positive**: a migration argument unique in the segment; no interruption at adoption time; reversibility that reduces the risk of trying it; it is the product's inbound migration path (ADR-023).
- **Negative**: the mapping engine (Docker inspection → internal model) is complex: partial cases, heterogeneous labels, non-standard compose; the "adopted but not yet normalized" coexistence creates an intermediate state that the UI and the deployment engine must handle explicitly.
- **Accepted risks**: some resources will remain non-adoptable (flagged with the reason); between adoption and first normalization, the resource may diverge from the internal model — the normalizing redeployment is the accepted convergence point.
