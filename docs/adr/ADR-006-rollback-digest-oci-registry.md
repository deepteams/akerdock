# ADR-006 — Rollback: OCI digests in a registry when configured, protected local retention otherwise

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.6, §5.5, §15, §18.3, INV-015

## Context

A rollback limited to images still present locally on the server is not one: if the disk cleanup has purged the image, it is impossible — and a mutable tag does not guarantee returning to the exact binary (§15). The PRD's source of truth (§18.3) already requires identifying the deployed image by **OCI digest**, not only by tag. It must be decided how to guarantee a reproducible rollback without imposing a registry on all installations.

## Decision

- **If a registry is configured**: every deployment is **pushed and referenced by OCI digest**. Rollback is reproducible to **any version retained** in the registry, regardless of the state of the server's disk.
- **Without a registry**: **local retention of the last N images**, with these images **explicitly protected from automatic cleanup** (INV-015 — cleanup never destroys a required persistent object).

## Alternatives considered

- **Local-only rollback (strict parity)**: rejected — rollback becomes unpredictable (depends on when cleanup ran) and non-reproducible, a known limitation of the reference (§15).
- **Registry mandatory for everyone**: rejected — burdens the minimal installation (one VPS, no ancillary infrastructure) and contradicts operational simplicity; the registry remains a choice.
- **Rebuilding the previous commit on rollback**: rejected as the primary mechanism — a rebuild is not bit-for-bit reproducible (dependencies, mutable base images) and is slow at the very moment one wants to roll back fast.

## Consequences

- **Positive**: deterministic rollback by digest when a registry exists; without a registry, a guaranteed rollback window (N protected images) instead of the reference's unpredictable behavior; consistent with §18.3 (digest resolution before switchover).
- **Negative**: the systematic push to the registry lengthens every deployment and consumes registry storage (retention to manage on the registry side); without a registry, the N retained images consume server disk and the rollback depth is bounded.
- **Accepted risks**: two rollback paths to test (registry and local); the value of N and the precise interaction with automated cleanup (§3.7) will have to be specified in the deployment engine (§29.4).
