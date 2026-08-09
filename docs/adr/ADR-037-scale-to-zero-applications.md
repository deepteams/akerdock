# ADR-037 — Scale-to-zero for production applications

- **Status**: Accepted
- **Date**: 2026-07-26

## Relation to other decisions

**extends** [ADR-036](ADR-036-scale-to-zero-waker.md) (in-path
waker) to production applications, beyond previews. Does not change the waker
mechanism; adds an opt-in and guardrails specific to production.

## Context

Scale-to-zero (ADR-036) was shipped **previews first** — proxy-contract
§8.3 even says "never implicit in production". The waker, however, is
**generic**: it routes by Host and wakes a set of containers, it knows nothing
about the notion of preview. Many self-hosted apps (internal tools,
side-projects, rarely used back-offices) would benefit from the same "off when
inactive, awake on the first request" — this is a direct request.

Production, however, does not have the same risk profile as a preview:

- the **cold-start** is paid by a **real user** (up to 60 s → 504),
  not by a developer reviewing their PR;
- an app may carry **workers/crons** that a `docker stop` would kill;
- **uptime monitoring** would ping the app continuously and keep it awake;
- "asleep" must not be confused with "crashed" by the UI and alerts.

## Decision

Scale-to-zero is extended to applications, **as an explicit opt-in separate**
from previews, with guardrails.

### 1. Two distinct opt-ins

`scale_to_zero` (+ `scale_to_zero_after_minutes`) on `applications` governs
**the application itself**; the former preview flag is **renamed**
`preview_scale_to_zero` (+ `preview_scale_to_zero_after_minutes`). The two are
not coupled: putting one's previews to sleep and putting one's production to
sleep are two different risk decisions. "Never implicit in production" (§8.3)
is respected — it is a switch the operator arms knowingly, never a default.

### 2. Scope and guardrails

- **Request-driven workloads only.** The UI warns: an app running workers,
  queue consumers or background crons is **not** a good candidate — the
  `docker stop` stops them too.
- **Cold-start accepted.** The UI displays that the first request after
  inactivity may wait for startup (up to 60 s). To be reserved for apps that
  tolerate this latency.
- **Managed databases excluded by construction.** The flag only exists on
  `applications`, not on `databases`: a standalone database is never put to
  sleep (severed connections, backup windows). A *compose* app embedding its
  own database remains the operator's choice (volumes persist).

### 3. Explicit state, distinct from an outage

`applications.scale_slept_at` (timestamptz, NULL = awake) embodies
**voluntary** sleep. The UI and monitoring read this state: a sleeping app is
displayed as "asleep (scale-to-zero)", **never** "down"/"unhealthy". The
control plane only puts to sleep an app whose `desired_status = running` — a
manually stopped app stays stopped, and a deployment wakes it (the waker
`docker start`s the new container on the first hit).

### 4. Uptime: answered without waking a sleeping app

An AkerDock uptime check carries an identification header (`X-AkerDock-Uptime`).
The waker **never** counts it as activity, and above all:

- **sleeping app** → the waker **responds `200` directly** (header
  `X-AkerDock-Scale: asleep`) **without starting anything**. This is honest:
  a sleeping scale-to-zero app *is* available — it wakes on the first real
  traffic. Monitoring sees it *up*, and a check does not cold-start the whole
  stack;
- **already awake app** → the check is **forwarded** to the real app (real
  health), still without counting as activity.

Thus monitoring does not defeat scale-to-zero and causes no periodic wake-up.
Accepted trade-off: on a sleeping app, uptime measures the *availability of
the service* (ability to respond), not the internal health of a stopped
container — which is the intended meaning of scale-to-zero. (The alternatives —
waking on every check, excluding the app from monitoring, or leaving it
permanently awake — were discarded.)

### 5. Mechanism reused as is

No change to the waker (ADR-036): same container (1 per server, shared by
previews + apps), routing by Host, `routes.json` merged per resource. An app's
`wake set` is the set of its containers (label
`akerdock.resource_uuid`, INV-011). The scheduler scan for apps mirrors the
previews' one (reading the activity file via SSH, `docker stop` of inactive
ones, wake-up of sleeping ones whose activity becomes fresh again).

## Consequences

- **Positive**: a single mechanism, a single waker per server, covers previews
  **and** apps; opt-in per app; resource savings on rarely used apps without a
  registry or an additional component.
- **Negative / limits**: cold-start on real traffic (to be reserved for apps
  that tolerate it); incompatible with background-task workloads (documented,
  not technically blocked — it is an operator choice); an uptime check causes
  a periodic wake-up. Live behavior remains **validated in E2E** (ADR-028);
  unit tests cover the decision (sleep/wake, respect of
  `desired_status`, uptime header).

## Rejected alternatives

- **Reusing the single preview flag for the app**: couples previews and
  production under a single switch, whereas they are two distinct risk
  decisions. Discarded in favor of two separate opt-ins.
- **Excluding STZ apps from uptime monitoring**: deprives the operator of the
  real availability measurement. Discarded in favor of "the check wakes but
  does not count as activity".
- **Also putting managed databases to sleep**: severs connections and weakens
  backups for a dubious gain. Excluded by construction (flag on `applications`
  only).
