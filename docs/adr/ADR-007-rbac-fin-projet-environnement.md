# ADR-007 — Fine-grained RBAC per project and per environment

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.7, §10.1, §15, §16.3, §23.1, §29.7

## Context

A coarse role model — instance owner, then admin and member per team — gives a mere member the power to touch the production of every project of their team (§15). Yet AkerDock establishes the team as the security boundary (§23.1) and describes actors with distinct needs (developer, operator/SRE, CI pipeline, read-only integration — §16.3). The granularity of access control must be decided.

## Decision

**Fine-grained RBAC adopted**: roles and permissions **per project and per environment**, beyond admin/member parity. A developer may, for example, be authorized to deploy to `staging` of a project without being able to touch `production`, nor the team's other projects.

The details (actions × resource types × roles) are to be specified in the **RBAC/permissions matrix** (§29.7), which is a mandatory artifact before full implementation. The existing API permissions (`read`, `read:sensitive`, `write`, `deploy`, `root` — §10.3, §24.1) remain the foundation for per-action evaluation.

## Alternatives considered

- **Strict admin/member parity**: rejected — reproduces a known and criticized limitation of the reference (§15) whereas AkerDock's team/project/environment model allows better from the start.
- **ACL per individual resource**: rejected — maximum granularity but disproportionate administration and audit complexity; the project/environment grain covers the real cases (protecting `production`).
- **External policies (OPA & co)**: rejected — unjustified dependency and operational complexity for a self-hosted product; the internal model suffices.

## Consequences

- **Positive**: real least privilege (protecting production from members), unblocking a known friction point of the reference, alignment with the actors of §16.3 and with auditing (§23.4).
- **Negative**: the entire authorization layer must evaluate project and environment in addition to the team — every endpoint and every indirect relationship must be covered by the inter-team and inter-role test matrix (§23.5); the UI must reflect partial rights (grayed-out actions, filtered lists).
- **Accepted risks**: specification complexity deferred to the §29.7 matrix, which becomes blocking before implementation; risk of over-granularity if the grain is not firmly settled (project/environment, not resource).
