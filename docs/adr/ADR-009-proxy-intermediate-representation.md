# ADR-009 — Proxy: common intermediate representation, Traefik only in P0, Caddy in P2

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.9, §4.1, §18.1, §26.1, §29.6

## Context

Supporting multiple proxies (Traefik, Caddy) by directly generating their routing labels on containers makes each proxy a distinct code path: hard to test equivalently, and behaviors silently diverge. A decision is needed on how to support multiple proxies without duplicating the routing logic.

## Decision

Validated decision:

- A **common intermediate representation** of routing (domains, paths, ports, middlewares, certificates) is the single source; Traefik or Caddy generation is derived from it deterministically (proxy contract §18.1: generation, validation, atomic application, rollback).
- **Routing labels on containers** remain supported (compatibility with common ecosystem usage).
- **Shared conformance fixtures** for Traefik/Caddy guarantee identical behavior of both backends (§29.6).

**Sequencing**: **Traefik only in P0**; **Caddy arrives in P2** via the intermediate representation, **whose fixtures exist from P0 onward**.

## Alternatives considered

- **Direct per-proxy label generation (strict parity)**: rejected — duplicated logic, untestable behavioral divergences, adding a third proxy prohibitive.
- **A single mandated proxy (Traefik permanently)**: rejected — Caddy is expected for parity (P2) and the abstraction also protects against changes in Traefik itself.
- **Writing the abstraction later, when adding Caddy**: rejected — costly retrofit; the conformance fixtures created from P0 make adding Caddy incremental.

## Consequences

- **Positive**: a single routing model to validate; both proxies are tested against the same fixtures, and are therefore genuinely interchangeable per server; atomic reload and rollback defined once and for all.
- **Negative**: the intermediate representation must cover the union of useful capabilities (basic auth, rate limiting, IP whitelisting, headers, path priorities, custom certificates — §4.1), an additional specification effort in P0 (§29.6) even though only one proxy is shipped.
- **Accepted risks**: some proxy-specific capabilities will not fit cleanly into the abstraction and will have to be either excluded or exposed as explicit extensions; since Caddy only ships in P2, the fixtures written in P0 will only be truly proven against it late in the process.
