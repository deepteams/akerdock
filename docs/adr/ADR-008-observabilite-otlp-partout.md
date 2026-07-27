# ADR-008 — Observability: OTLP everywhere, Prometheus exposition, no proprietary protocol

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.8, §3.8, §13

## Context

A metrics agent that pushes to the instance using a home-grown protocol (endpoint + token) locks in telemetry data: it is impossible to plug in standard tooling (collectors, dashboards, alerting) without writing an adapter. A choice must be made between a proprietary protocol and the domain's open protocols (§3.8).

## Decision

**OTLP everywhere**: the server agent, the control plane and the workers emit **metrics, traces and logs in OpenTelemetry (OTLP)**, with **Prometheus exposition**; **no proprietary protocol** is introduced.

The principle of a lightweight agent per server is retained (Sentinel parity: server and per-container CPU/RAM, disk, history in the UI — §3.8, §13), but its transport and format are standard.

## Alternatives considered

- **Proprietary push protocol (Sentinel parity)**: rejected — locks in telemetry, requires maintaining a protocol, and prevents the user from plugging in their own backend (Grafana, Datadog, etc.).
- **Prometheus pull only, without OTLP**: rejected — covers metrics but neither traces nor logs, and pull requires inbound ports on the target servers, running counter to surface reduction (ADR-001).
- **No agent at all (executing commands over SSH)**: rejected — no reliable history, repeated SSH cost, no satisfactory per-container granularity.

## Consequences

- **Positive**: full interoperability with the ecosystem (OTel collectors, Prometheus, Grafana); the same standard serves both the control plane's self-observation and server monitoring; trace/correlation-ID instrumentation consistent with the audit and DoD requirements (§26.3).
- **Negative**: dependency on the OpenTelemetry SDKs/semconv, which are still evolving; the agent must embed an OTLP exporter rather than a simple home-grown POST, which slightly increases its footprint.
- **Accepted risks**: OTLP trace/log volume can be significant on small installations — frequencies and retentions must remain configurable as in the reference; the UI's built-in charts must consume this standard data without reintroducing a parallel channel.
