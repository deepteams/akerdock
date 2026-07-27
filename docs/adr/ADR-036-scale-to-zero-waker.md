# ADR-036 — Scale-to-zero via an in-path waker

## Status

Accepted — pins down the **proposed defaults** of
[proxy-contract §8](../specs/proxy-contract.md) (Scale-to-zero, SHOULD), which
it locks in; complements [ADR-011](ADR-011-cycle-de-vie-des-previews.md)
(preview lifecycle) and [ADR-024](ADR-024-sse-et-websocket.md) without
superseding them.

## Context

A PR preview spends most of its time **inactive**: opened for a review,
consulted for a few minutes, then forgotten until the next review pass or its
TTL. Yet it continuously consumes CPU/RAM (often a full nats/postgres/redis/app
stack). Scale-to-zero shuts the resource down after a period of inactivity and
starts it back up on the first request — the structuring goal of the
"CD per PR" flow: many previews can be kept alive without paying for all of
them permanently.

Two invariants frame the mechanism:

- **INV-007** — the control plane never proxies application traffic. The
  wake-up must therefore happen **server-side**, not in the control plane.
- **push §18.1** — the server never contacts the control plane; the control
  plane connects to the server (SSH). Wake-up must work
  **even with the control plane down**.

proxy-contract §8 proposes (all clauses being "proposed defaults") an
`akerdock-waker` helper container and a switch between **two variants** of the
dynamic file (`sleeping` → route to the waker, `awake` → direct route),
swapped by an atomic `mv -f` on wake-up. This default has two blind spots for
our choice of inactivity measurement: once **awake**, the `awake` variant
routes directly to the app, so **the waker no longer sees the traffic** and
cannot timestamp the last request; measuring inactivity would then require
parsing the proxy's access logs.

## Decision

### 1. The waker is a mode of the single binary

No second artifact. `akerdock waker` is a subcommand of the existing binary
(ADR-021, full-Cobra ADR-033), deployed as a helper container with the
**same image** pinned by the release, labeled `akerdock.type=helper` and
`akerdock.managed=true`, on the server's internal network (**never published**),
with access to the local Docker socket.

The reference to this image is **baked into the binary at build time**
(`-ldflags -X main.image=...`, like `version`): a release therefore deploys the
waker from its own image without any runtime configuration — a container does
not know its own tag, but the build does. `AKERDOCK_IMAGE` (env) overrides it
(mirror registry, local build). Empty on both sides ⇒ scale-to-zero stays inert
with an explicit error at deployment, never a guessed registry. Its code is
**restricted** to starting `akerdock.managed=true` containers: it creates,
deletes and builds nothing.

### 2. The waker stays in the traffic path and reports activity

We **discard the two-variant switch** in favor of a **single variant**: for a
`scale_to_zero` resource, the dynamic file **always** routes to the waker
(`http://akerdock-waker:8080`, middleware adding the
`X-AkerDock-Wake: <resource_uuid>` header). The waker is thus a permanent
reverse proxy **in the traffic path** in front of STZ resources:

- **asleep** (`sleeping`): the target container is stopped. On the first
  request, the waker runs `docker start <uuid>` (idempotent, `waking` state),
  waits for `healthy` (or stable *running* for 10 s absent a healthcheck), max
  delay **60 s**, then **holds-and-forwards** the original request;
- **awake** (`running`): the waker forwards each request to the container and
  **timestamps the last activity** in a local file
  (`/var/lib/akerdock/waker/<uuid>.activity`, Unix timestamp, atomic rewrite).

This file is what embodies "the waker reports activity": the control plane
**reads it via SSH** (never the server calling the control plane —
push §18.1 preserved) during its sleep pass.

### 3. Putting to sleep is driven by the control plane

A scheduler pass (alongside the TTL reaper) selects `running` STZ resources
whose last activity (waker file read via SSH) exceeds
`scale_to_zero_after_minutes` (default 30) and enqueues a job that:
`docker stop`s the container then transitions the state to `sleeping`. The
dynamic file does **not** need to change (it already points to the waker) —
putting to sleep is a simple `docker stop`, waking is a `docker start`. No file
switch, no access log parsing.

### 4. State machine and limits

States: `sleeping → waking → running → (inactivity) → sleeping`. Addition of
the `sleeping` and `waking` states to the `preview_status` enum. Limits
(proxy-contract §8.3): wake-up > 60 s → **504**; held request body ≤ **1 MiB**,
beyond that **503 Retry-After: 5**; WebSockets held during `waking` (a long WS
is a poor STZ candidate); opt-in **per resource**, **previews first**, never
implicit in production.

## Consequences

- **Positive**: **exact** inactivity measurement (per request) without parsing
  logs; wake-up functional with the control plane down (INV-007, push §18.1);
  zero second artifact (ADR-021); sleep/wake = `docker stop`/`start`, without
  touching the dynamic file or certificates.
- **Negative / limits**: the waker is **permanently in the traffic path** of
  STZ resources — one additional internal hop (local, internal network,
  negligible cost) and a **SPOF for STZ resources only** if the waker goes down
  (mitigated: `restart: always`, previews-first, opt-in). This is the accepted
  price of the "the waker reports activity" choice: the direct `awake` variant
  of §8.2 would make it blind to established traffic.
- End-to-end wake-up (hold-and-forward, healthy, timeout) falls under
  **E2E validation** (ADR-028): unit tests cover the wake decision, the limits
  and the dynamic file generation; live behavior goes through the DinD journey.
- **Distributing the image to remote servers**: the waker runs the AkerDock
  image — the first *project* image that must run on a target server (the
  proxy, for its part, uses a public image). It gets there via a registry pull
  (ghcr flow) or because it is local (source-only single-host install). On a
  **remote** server with a source-only install *without a registry*, it is
  missing: to be addressed by a registry, or by a `docker save`→`docker load`
  streamed over SSH to remain "registry-free" (avenue noted in `TODO.md`, not
  implemented — pointless as long as deployment stays single-host).

## Rejected alternatives

- **Two variants + atomic `mv -f` (default §8.2)**: makes the waker blind to
  traffic once awake, thus requires parsing Traefik access logs to measure
  inactivity — a coupling to the log format and a fragile source of truth, to
  save a negligible internal hop.
- **Dedicated `akerdock-waker` image (literal §8.1)**: a second binary to
  build, publish, version and pin, against ADR-021 (single deliverable).
- **The waker reports activity to the control plane (HTTP)**: would violate
  push §18.1 (the server never contacts the control plane) and would break
  wake-up when the control plane is down. The local file read via SSH preserves
  both invariants.
- **Measurement via Sentinel metrics**: approximate (an idle but alive
  container consumes a bit of CPU/network) and dependent on Sentinel being
  enabled; the in-path waker provides the real activity.
