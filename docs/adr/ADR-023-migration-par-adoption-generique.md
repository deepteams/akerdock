# ADR-023 — Inbound migration: through generic adoption, no proprietary import wizard

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.23, §20.7, §27.13, §16.2

## Context

The first population likely to arrive on AkerDock already operates workloads under another deployment platform. A dedicated import wizard would read that platform's **internal database** — an undocumented, moving schema specific to its implementation — to recreate the objects on the AkerDock side. The PRD already excludes this coupling from its goals (§16.2). Moreover, AkerDock has **generic resource adoption** (§20.7, ADR-013), which takes over standard Docker objects. The official migration path must be chosen.

## Decision

**No proprietary import wizard**: **generic resource adoption (§20.7) IS the migration path**. AkerDock adopts the **standard Docker containers, compose stacks, volumes and networks** already present on the server — what *any* container platform produces — **without knowing anything about the internal schema of the one that created them**.

This decision is **re-evaluable upon proven user demand**.

## Alternatives considered

- **Import wizard reading a third-party platform's internal database**: rejected — coupling to an undocumented, moving schema, perpetual maintenance at the pace of its releases, contrary to non-goal §16.2; and the essential part (the running workloads) is already covered by adoption.
- **Export/import via a third-party platform's API**: rejected as an official deliverable — a third-party API never covers everything (secrets, history), the result would still require a redeployment, and the tool would break with every upstream change. A community tool remains possible on top of config as code (ADR-012).
- **No documented migration path**: rejected — being able to take over an existing setup without interruption is an explicit product argument (§27.13); it must be documented, just not in the form of a wizard tied to a third party.

## Consequences

- **Positive**: **zero third-party-platform-specific code** to maintain; the migration path automatically benefits from every improvement to generic adoption; migration happens **without interrupting the workloads** (adoption without redeployment, previewed and reversible).
- **Negative**: what lived in the original platform's database and **not** in the Docker objects — backup plans, scheduled tasks, non-injected variables, members and teams, history — is **not** migrated automatically and must be re-entered.
- **Accepted risks**: the experience is less "turnkey" than a dedicated wizard, which may deter some migrants — accepted, with re-evaluation explicitly planned if proven user demand justifies it.
