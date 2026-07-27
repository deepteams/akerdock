# ADR-010 — One-click catalog: dedicated signed template repository + user template repositories

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.10, §9, §27.22, §29.11

## Context

A one-click service catalog is a set of compose files annotated with metadata (§9). Shipping it compiled into the binary couples it to releases and centralizes its admission: a team cannot maintain its own internal templates. A decision is needed on the provenance, lifecycle and trust granted to templates, while respecting the licenses of imported templates.

## Decision

The catalog relies on two sources:

1. **A dedicated template repository maintained by the project**: versioned, validated, **signed**, and **refreshable independently of the binary**. Compatible templates from the ecosystem are imported into it in compliance with their licenses (and rewritten with `AKERDOCK_*` variables, cf. ADR-022 / §27.22).
2. **User template repositories**: each team can register **one or more Git repositories** (public or private, via existing keys/credentials) containing its own templates, with **validation on import** and **on-demand resynchronization**.

## Alternatives considered

- **Catalog embedded in the binary (parity)**: rejected — couples catalog freshness to platform releases and rules out private enterprise catalogs.
- **Direct, unvalidated import of any compose URL**: rejected — a template is executed with the privileges of the target server; without validation or signing, it is an obvious attack vector (still possible via the ordinary compose build pack, outside the catalog).
- **Centralized marketplace with third-party submissions**: rejected — disproportionate moderation and infrastructure cost; user Git repositories cover the customization need.

## Consequences

- **Positive**: official catalog updated without a binary release; integrity chain (signing) over what the platform offers to execute; teams can maintain private internal templates with their existing Git credentials; easier license inventory (§29.11).
- **Negative**: signing and validation infrastructure to build and operate (project signing key, instance-side verification); a template validation pipeline (compose lint, magic variables, metadata) becomes a component in its own right.
- **Accepted risks**: templates from user repositories are validated syntactically but remain the responsibility of the team that registers them (no project signature); rewriting the variables of templates imported from a third-party ecosystem is a recurring maintenance cost, accepted (cf. ADR-022).
