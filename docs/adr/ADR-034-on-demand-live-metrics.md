# ADR-034 — On-demand live metrics via the runtime connection

## Status

Accepted — complements (does not supersede) [ADR-008](ADR-008-observability-otlp-everywhere.md).

## Context

ADR-008 decides that **historical** observability (server and container
CPU/RAM metrics, traces, logs) travels over **OTLP** to an external
time-series storage: nothing is modeled in PostgreSQL, and the agent's
proprietary push protocol is rejected in favor of standard OTLP.

That decision is right for history and analytics, but it leaves a
gap for the dashboard's most common use: **"how much is this service
consuming, right now?"**. Answering that would today require the operator to
wire up an OTLP backend + a third-party UI (Grafana), where what they want is
an immediate gauge next to the logs and the shell they already have at hand
(§13, §3.16 — the `akd-metric-chart` component was specified but never
shipped, for lack of a data source on the control plane side).

## Decision

The control plane exposes **live, on-demand metrics, without persistence**:

- The source is a **`docker stats --no-stream`** executed on the target server
  **via the existing runtime SSH connection** (the same channel as `docker logs`,
  the terminal and the port-forward), resolved by the deterministic container
  naming `<uuid>-<service>` (INV-011).
- The read is **point-in-time**: one call = one sample. The dashboard
  refreshes by polling periodically and builds a mini-trend
  **client-side**; no sample is written to the database or buffered beyond
  one HTTP response.
- Read-only endpoints under the resource (`GET …/metrics`), permission
  `read`; a metric is never a secret.

History, alerting and aggregation remain **out of scope** for this ADR
and continue to follow ADR-008 (OTLP to an external backend).

## Consequences

- **Positive**: immediate per-service CPU/RAM gauges, zero external dependency,
  zero metrics table, zero push protocol to operate — consistent with the
  "standard, reversible Docker" philosophy (§16.1) and with the other runtime
  accesses already going through SSH.
- **Negative / limitations**: no history or long-term trend (that is the
  role of ADR-008); each read opens/relays a `docker stats` (bounded cost,
  `--no-stream`, a single call for all the containers of a stack); if the
  server is unreachable the response is a 409, like `docker logs`.
- The `metrics:read` permission of the RBAC grid remains reserved for history
  (external backend); on-demand live falls under `read` on the resource.

## Rejected alternatives

- **Resurrecting the Sentinel push + short retention in the database**: reintroduces the
  proprietary protocol that ADR-008 rejects and adds a metrics table —
  for a simple live display, a disproportionate structural cost.
- **Querying the external OTLP backend (PromQL)**: ties the dashboard to a
  Prometheus/compatible that the operator does not necessarily have, and couples the UI to a
  third-party query format. Remains the right path for **history** (ADR-008),
  not for the live gauge.
