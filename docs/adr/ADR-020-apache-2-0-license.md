# ADR-020 — Project license: Apache 2.0

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.20, §1, §29.11

## Context

AkerDock must choose its open source license. The self-hosted PaaS segment was built on permissive licenses, and the absence of paywalled features is an explicit product promise (§1). The sector's recent landscape shows two paths: permissive licenses (maximum adoption) or restrictive BSL/AGPL-style licenses (protection against a "cloud fork" by a hyperscaler). This must be settled before any code is published.

## Decision

**Apache 2.0** — the same license as the reference:

- maximum adoption and contributions (no legal friction for companies);
- **patent clause included** (explicit protection for contributors and users, an advantage over MIT);
- the **competitive moat is the product, not the license**; the "cloud fork by a third party" risk is **accepted**.

## Alternatives considered

- **AGPL v3**: rejected — strong adoption friction in companies (policies forbidding AGPL), at odds with the goal of maximum adoption.
- **BSL / "source available" licenses (SSPL, FSL…)**: rejected — protect against a cloud fork but exclude the project from the open source definition, complicate packaging and contributions, and would send a signal opposite to the reference's.
- **MIT**: set aside in favor of Apache 2.0 — nearly equivalent in permissiveness but without an explicit patent clause.

## Consequences

- **Positive**: maximum compatibility with the ecosystem (dependencies, distributions, companies); permissive license aligned with that of the domain's compose templates, which simplifies importing them in compliance with their licenses (§27.10, inventory §29.11); patent protection for contributors and users.
- **Negative**: no legal protection against an actor who would host AkerDock as a competing managed service without contributing.
- **Accepted risks**: the "cloud fork by a third party" is explicitly accepted — the defense is product cadence, community, and brand, not the license; this bet is reversible for future code but not retroactively for code already published under Apache 2.0.
