# ADR-042 — Application access protection (auth wall) reusing the preview mechanism

- **Status**: Accepted
- **Date**: 2026-07-28
- **Extends**: [ADR-030](ADR-030-preview-sso-forward-auth.md) (preview SSO / forward-auth)
- **Related PRD sections**: §20.4.4, §21, §27.7

## Context

Previews have had an access wall since ADR-030: `none`, `basic_auth` (shared
credentials) or `sso` (the AkerDock session, whatever login produced it),
enforced by Traefik middlewares — basicAuth, or forwardAuth to the control
plane. Production applications have nothing: a staging environment, an admin
back-office or a client demo is public the moment it has a domain, and the
only workarounds are application-level code or hand-written proxy config
outside AkerDock's model.

The need is the same as previews', on a longer-lived resource. The mechanism
is already built, audited and in production.

## Decision

Applications gain the same access protection, reusing ADR-030 end to end.

1. **Two modes plus none**: `none` (default, unchanged behavior),
   `basic_auth` (shared credentials, generated and revealed to the team like
   the preview secret) and `sso` (AkerDock session; access requires
   membership of the application's team — INV-001).
2. **Application-wide scope**: one setting per application. Every domain of
   the application, and every routed service of a compose stack, is behind
   the same wall. Per-domain protection is explicitly rejected below.
3. **Same enforcement path**: injected Traefik middlewares — `basicAuth` with
   a bcrypt hash (the clear text never enters a routing file), or
   `forwardAuth` to the control plane carrying the application identity in
   the middleware ADDRESS (`?application=<uuid>`), because intermediate
   proxies rewrite `X-Forwarded-Host` (ADR-030's lesson).
4. **Same session ritual, application-scoped cookie**: the forward-auth
   redirects to the panel's authorize endpoint, which verifies the session
   and the team, mints a one-shot token, and bounces to the application's own
   host where a callback exchanges it for a scoped cookie (12 h TTL). Access
   tokens are stored hashed, one table shared by both resource kinds.
5. **Non-navigations get a clean 401**: a fetch/XHR cannot complete a
   cross-origin redirect ritual, and no `WWW-Authenticate` ever reaches the
   browser — the application's own 401s stay its own (ADR-030).
6. **Uptime probes and the waker are unaffected**: the wall sits in front of
   the waker exactly as for previews (the waker routes by Host), and uptime
   checks carry their marker header before the wall would matter.

## Alternatives considered

- **Per-domain protection**: rejected — more API/UI surface, and a new domain
  added later silently defaults to unprotected, which is precisely the
  failure mode an access wall must not have. An application whose domains
  need different exposure levels is two applications.
- **SSO only**: rejected — a client demo or an external reviewer without an
  AkerDock account is a real case; basic auth covers it, with the trade-off
  (no per-person traceability) documented.
- **Basic auth only**: rejected — for team-internal environments the session
  wall is both stronger and friendlier (no shared password to rotate, access
  follows team membership and revocation).
- **A dedicated identity provider / OAuth proxy in front**: rejected — a
  second auth stack to operate for a need the panel session already covers.

## Consequences

- **Positive**: staging environments and back-offices stop being public by
  default; the same mental model and the same code path as previews (one
  mechanism to audit, one to maintain); revocation follows team membership
  for SSO.
- **Negative**: SSO protection requires the instance FQDN to be set (like
  previews) — without it the setting cannot be honored and the deploy says so
  rather than silently serving publicly; the wall applies to every domain of
  the application, so a mixed public/private need must be split into two
  applications.
- **Accepted risks**: shared basic-auth credentials have no per-person
  traceability and are only rotated by regenerating them; a protected
  application answering machine clients (webhooks, APIs) will 401 them —
  documented, and the reason `none` remains the default.
