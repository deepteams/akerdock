# ADR-017 — Integrated uptime monitoring: simple HTTP/TCP checks, no APM

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.17, §11, §13, §26.2

## Context

Monitoring container state and server reachability does not tell you whether the application **responds from the Internet** (§13). Without an external check, the user must install, configure, and maintain a second tool to know that. We must decide whether AkerDock integrates this capability and how far.

## Decision

**Simple integrated HTTP/TCP checks**: target, interval, failure thresholds, **executed outside the monitored workload** — with **alerting via the existing notification channels** (§11) and **per-resource availability history**.

**No APM**: the scope stops at **up/down and latency**. Everything related to profiling, transactions, or application errors remains out of scope.

## Alternatives considered

- **Strict parity (delegate to Uptime Kuma)**: rejected — breaks the integrated experience (second tool, second alerting configuration) for a capability that is simple to provide; Uptime Kuma remains available in the catalog for advanced needs.
- **Full APM/application monitoring**: rejected — disproportionate scope, a market already served by specialized players, and contrary to the product's light footprint.
- **Checks executed from the server hosting the workload**: rejected — a struggling server would falsely report its own workloads as healthy or would report nothing; checks are executed outside the monitored workload.

## Consequences

- **Positive**: integrated answer to the question "is my app responding?" without a third-party tool; direct reuse of notification channels and rules (§11, ADR-019); per-resource availability history in the same UI.
- **Negative**: a reliable check scheduler (intervals, jitter, thresholds, anti-flapping) and availability history storage are full-fledged components; "outside the workload" implies precisely defining the check execution point(s) and their network blind spots.
- **Accepted risks**: without multi-region probes, a check reflects the point of view of the execution point, not that of all end users; the "no APM" boundary will have to be defended against extension requests (detailed error codes, public status pages…), which would require new decisions.
