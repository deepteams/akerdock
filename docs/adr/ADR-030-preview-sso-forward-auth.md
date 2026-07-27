# ADR-030 — Protecting previews with AkerDock authentication (SSO)

- **Status**: Accepted
- **Date**: 2026-07-24
- **Related PRD sections**: §20.4.4 (preview protection), §10.2 (authentication), INV-010
- **Revises**: nothing — adds a protection mode alongside `basic_auth` and `none`

## Context

Basic auth protects previews but its ergonomics are poor (browser dialog
box, re-prompts when the application behind emits its own 401s, a single
password shared by the whole team) and its level of guarantee is inferior to
the panel's authentication — which already handles email/password, passkeys,
MFA and OIDC/Google. The team reviewing a PR is by definition the one that
has access to the panel: the preview must be able to delegate its access
control to AkerDock.

## Decision

New mode `preview_protection: sso`:

1. **Traefik `forwardAuth`** on every HTTPS router of the preview, towards
   `https://<fqdn-instance>/webhooks/previews/forward-auth` — the control
   plane decides request by request. The mode requires a configured instance
   FQDN; without it the preview deployment fails with the cause.
2. **Per-preview access cookie**: the forward-auth accepts an
   `akerdock_preview` cookie carried by the preview's domain — an opaque
   token of which only the hash is stored (`preview_access_tokens`), bound
   to THE preview, to the user who obtained it, and expiring (12 h). Its
   revocation follows the preview (cascade).
3. **Bootstrap by redirect**: without a cookie, the forward-auth redirects
   the browser to `/webhooks/previews/authorize?redirect=<url>` on the
   panel. There, the AkerDock SESSION is authoritative — whatever the login
   method (password, passkey, OIDC). Access is granted if the user belongs
   to the application's team (INV-001 isolation); a token is issued,
   audited, and the browser is sent back to
   `https://<host-preview>/.akerdock/preview-callback?token=…&next=<path>` —
   a **dedicated router** in the preview's routing file (maximum priority,
   without auth middleware) proxies this path server-side to the control
   plane (`passHostHeader: false`), which sets the cookie and redirects to
   `next` (constrained to a local path). The token travels in the request's
   URL: query strings survive every proxy hop, the `X-Forwarded-*` headers
   do not (purged by intermediate entrypoints as untrusted) — this is what
   makes the flow robust to hairpin topologies (the instance's auth passing
   back through its own proxy).
   The preview's identity likewise travels in the **address** of the
   forward-auth middleware (`?preview=<uuid>`), never inferred from an
   `X-Forwarded-Host`.
4. The preview's host is resolved server-side (exact fqdn or
   `<service>-<fqdn>` for compose stacks) — the `redirect` parameter is
   never followed to a host that is not that of a known preview
   (anti open redirect).

`basic_auth` remains the default: it works without an instance FQDN and for
guests outside the team. `signed_link` remains reserved (existing enum
value, not implemented).

## Alternatives considered

- **Widening the session cookie to the parent domain**: simple but widens
  the PANEL cookie's surface to all subdomains — including previews that run
  PR code. Rejected: the preview cookie is a dedicated token, with limited
  scope and lifetime, with no power whatsoever over the API.
- **Per-user basic auth**: keeps all the ergonomic flaws.
- **External OAuth2-proxy**: one more component to operate — contrary to
  ADR-025 (PostgreSQL as the only dependency).

## Application service workers

A PWA installs a service worker that owns the preview's origin. A
**correct** worker is transparent to this flow: a redirected response
(`opaqueredirect`) passed through as-is to a navigation is followed natively
by the browser, and the login dance traverses the worker without friction. A
worker that caches or reprocesses redirected navigation responses, however,
gets stuck behind **any** 302 emitter (OAuth, CDN, load balancer) — not just
this flow; fixing it is the application's responsibility (rules: never cache
a `redirected`/non-200 response as the shell; ideally exclude
`/.akerdock/**`).

**Documented negative decision**: the platform NEVER sends
`Clear-Site-Data` to evict a faulty worker. Tested and rejected: Chrome
refuses it on non-credentialed requests (manifest, no-cors), and on a
navigation transiting through a worker it orders the eviction of the worker
that is processing the response — a measured stall of about 10 s per
request, worse than the disease.

## Consequences

- Table `preview_access_tokens` (hash only), two non-bearer browser routes
  (`/webhooks/previews/*`), one DB lookup per preview request in sso mode —
  accepted: review traffic, and the hash is indexed.
- The application behind can return its own 401s without triggering a
  dialog box: no more `WWW-Authenticate` on the proxy side.
