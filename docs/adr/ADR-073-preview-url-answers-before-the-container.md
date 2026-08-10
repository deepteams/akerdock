# ADR-073 — A preview URL answers from the moment the PR is opened

- **Status**: Accepted
- **Date**: 2026-08-10
- **Extends**: [ADR-036](ADR-036-scale-to-zero-waker.md) — the agent already answers for a
  resource that is not currently serving, and already renders a self-refreshing waiting page.
  This adds one more reason for it to answer: a preview that does not exist **yet**
- **Extends**: [ADR-060](ADR-060-dev-ingress-tunnels.md) §3, which established the pattern
  being reused: a router provisioned **at declaration**, pointing at the agent, serving a
  holding page until the real thing is behind it
- **Related**: [ADR-030](ADR-030-preview-sso-forward-auth.md) (the preview access wall this
  page inherits rather than reinvents), [ADR-037](ADR-037-scale-to-zero-applications.md) (sleeping
  and waking previews, whose page this one must not fight over),
  [ADR-052](ADR-052-agent-command-channel.md)/[ADR-054](ADR-054-agent-host-ops.md) (how the
  routing table reaches the server)
- **Related PRD sections**: §5.5, §20.4.4, §26

## Context

A pull request is opened, the URL is posted in the PR within seconds by the platform, someone
clicks it — and gets **404 not found**. Nothing is wrong: the route is written during the
deployment (`apply_routing`, and `switch_routing` on a redeploy), so between the PR being
opened and the first successful deploy, the FQDN belongs to nobody. Traefik answers for a host
it has never heard of, in the only way it can.

The window is not small. It covers the queue, the clone, the build — minutes on a cold cache —
and it is precisely the window in which the link gets clicked, because the platform published
it the moment the preview was reserved. The first impression of a preview environment is,
today, a 404 with no explanation, followed by a manual refresh loop.

The platform already knows how to answer for something that is not serving. **Twice.** The
scale-to-zero agent renders a waiting page that auto-refreshes while containers start
(ADR-036 §8.2), and the ingress agent serves an offline page on a declared FQDN whose laptop
is not attached (ADR-060 §3), with its router provisioned at declaration precisely so the URL
exists before the thing behind it does. What is missing is not a mechanism. It is the decision
to route a preview before it has a container.

## Decision

### 1. The route is written when the preview is reserved, not when it is deployed

A preview that reaches `queued` gets its dynamic Traefik file immediately, with its FQDN and
its usual middlewares, pointing at **the agent** — the same target a sleeping preview already
points at. The deployment's `apply_routing` / `switch_routing` step then does what it does
today: it points the file at the real container. Nothing about the deployment changes; what
changes is that the file already existed.

Two consequences worth naming. The certificate can be issued while the build runs, instead of
after it, so HTTPS is ready when the container is. And the FQDN stops being a hole in the
proxy's configuration during the exact minutes people click it.

### 2. The agent answers with the preview's state, and it is the truth it was given

The routing table grows a **pending** entry: a host, the preview's uuid, and the state the
control plane last wrote (`queued`, `deploying`, `failed`). The agent renders it, refreshes
itself, and says what is happening in the words the dashboard uses — a queued preview says it
is queued, a building one says it is building, a failed one says so and stops refreshing.

The agent is told, it does not ask: it holds no API token and no view of the control plane
(INV-007). The state is pushed on the transitions that already write to the server — the same
path that deposits the routing table today (ADR-052/054). A state that is momentarily stale
costs one refresh cycle; a state the agent had to fetch would cost an outbound dependency on
every visitor request.

### 3. A page that is never in front of something that works

The waiting page exists **only while there is no container to serve**. The moment the
deployment switches the route, the file points at the container and the visitor gets the
application — that is the same `switch_routing` step as today, not a new decision, and it is
what makes this safe to ship.

Three mechanics keep it honest, because a holding page stuck in front of a working preview
would be a worse defect than the 404 it replaces:

- The page is served `Cache-Control: no-store` (as the waker's already is), so no browser and
  no intermediary keeps it after the switch.
- The pending entry is **removed** when the route is pointed at a container. A stale pending
  entry cannot shadow a real route: the dynamic file has one service per host, and it is the
  file that decides.
- A preview that is `sleeping` or `waking` is **not** pending — it has a container, and
  ADR-036's waking page owns that case. The two pages never compete for the same host,
  because the states are disjoint by construction.

### 4. The access wall is the preview's own, unchanged

The dynamic file for a pending preview carries the same middlewares as any other preview:
`basic_auth` by default, `sso`, or `none` (ADR-030), plus the unconditional `noindex`
the control plane imposes on preview URLs. The wall does not open because the container is not ready — a visitor
who cannot see the preview cannot see that it is being built for PR #482 either.

This costs nothing to implement: a scale-to-zero preview already routes to the agent through
those same middlewares. It is stated here because it is the property a reviewer would want
checked, not because it required work.

### 5. Previews only, for now

Applications and stacks have the same hole — a domain configured, no route until the first
successful deploy — and the same fix would work. It is not done here: a preview's URL is
published automatically the second the PR opens, which is what makes the window acute, while
an application's domain is typed by someone who is already in the dashboard watching the
deployment. Extending it is a small step once this one is proven, and deliberately not taken
in the dark.

## Consequences

- The preview lifecycle writes routing at reservation and at destruction, not only through the
  deployment: `queued` writes the pending entry, a terminal state updates or clears it.
- A failed preview keeps a URL that explains itself rather than a 404 — for the author of the
  PR, that is the difference between "the platform is broken" and "my build failed, here is
  the link to the log".
- One more page rendered by the agent, in the same style as the waker's, and it needs the same
  discipline: no secret, no stack trace, nothing the access wall would not already have shown.
- The certificate for a preview FQDN is now requested earlier in the sequence. On a wildcard
  setup nothing changes; on per-host ACME this makes the first HTTPS hit likelier to succeed.

## Alternatives rejected

- **Serving the waiting page from the control plane**: rejected — the control plane carries no
  visitor byte (INV-007), and Traefik is on the server. Routing a preview's visitors through
  the control plane to render a holding page would invert the one invariant the whole proxy
  design is built on.
- **A static "coming soon" page with no state**: rejected — the reason to click a preview URL
  early is to find out whether it is ready. A page that cannot say *queued* from *building*
  from *failed* sends the reader back to the dashboard, which is what they were avoiding.
- **Letting the agent poll the control plane for the state**: rejected — see §2.
- **Answering `503` with a `Retry-After` and no page**: rejected. Correct for an API client,
  useless for the human the link was posted for; the agent already distinguishes browser
  navigations from other requests (ADR-036 §8.2) and can keep doing so here.
- **Doing applications and stacks in the same slice**: rejected for now — see §5.

## Verification

Unit tests, per the pyramid (ADR-026/028):

- Reserving a preview writes a dynamic file whose service is the agent and whose middlewares
  are the preview's own (basic auth / sso / noindex), asserted per protection mode.
- The pending entry reaches the agent's routing table with the preview's state, and a state
  transition updates it.
- The agent renders queued / deploying / failed distinctly, refreshes on the first two, stops
  on the last, and sends `Cache-Control: no-store` on all three.
- **The switch removes the pending entry**: after `switch_routing`, the host resolves to the
  container and the agent no longer claims it — the regression test for "the page must never
  sit in front of a working preview".
- A `sleeping` preview goes to ADR-036's waking page, not to this one.
- A destroyed preview's route is removed, as today.
