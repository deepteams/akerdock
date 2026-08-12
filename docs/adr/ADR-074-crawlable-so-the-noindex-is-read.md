# ADR-074 — The control plane allows crawling, so that its noindex can be read

- **Status**: Accepted
- **Date**: 2026-08-12
- **Corrects**: PRD §14.2 and [proxy-contract](../specs/proxy-contract.md) §4.7, which mandated
  a `Disallow: /` robots.txt *and* an `X-Robots-Tag: noindex, nofollow` on the control plane.
  No ADR is superseded: the control plane's own visibility was decided in the PRD without one,
  which is part of why the interaction between the two measures was never examined
- **Related**: [ADR-021](ADR-021-single-port-control-plane.md) (the single port this applies to),
  [ADR-060](ADR-060-dev-ingress-tunnels.md) §5 and
  [ADR-073](ADR-073-preview-url-answers-before-the-container.md) (unconditional noindex on
  ingress endpoints and previews — header-only, and therefore already correct)
- **Related PRD sections**: §14.2, §26

## Context

The control plane answers every request with `X-Robots-Tag: noindex, nofollow`, and serves a
`robots.txt` reading `User-agent: * / Disallow: /`. Both were added together, deliberately, as
belt and braces.

They are not belt and braces. They are belt *versus* braces.

`Disallow: /` tells a crawler not to **fetch** the page. `X-Robots-Tag` is an HTTP response
header — it only exists once the page has been fetched. Forbidding the fetch forbids the
crawler from ever reading the instruction to stay out of the index. The two do not add up;
the first one disarms the second.

The consequence is not theoretical, and it is the reason this ADR exists: an instance whose
FQDN was indexed **before** the header shipped is still listed today, title and snippet
included, with no way out. Every recrawl that would let Googlebot discover the `noindex` is
refused by our own robots.txt. The URL is frozen in the index — and this is the documented
behaviour of every major crawler, not a bug in one of them.

The reasoning recorded in PRD §14.2 was half right: *"robots.txt alone would not keep a linked
page out of the index"*. True. A page linked from anywhere else gets indexed, URL only, even
when disallowed — which is exactly why the header was added. What was not noticed is that the
converse also holds, and is stronger: **robots.txt does not merely fail to help, it prevents
the thing that does help from working.**

## Decision

### 1. The control plane invites the crawl it wants to be refused by

`robots.txt` keeps being served — see §3 — but it disallows nothing:

```
# This control plane must not appear in search results, and says so with an
# `X-Robots-Tag: noindex, nofollow` header on every single response. Reading
# that header requires fetching the page, so crawling is allowed on purpose.
# A `Disallow: /` here would not add protection: it would silence the header
# and leave any already-indexed URL in the index permanently.
User-agent: *
Allow: /
```

The comment is part of the decision, not decoration. A bare `Allow: /` on a product that
advertises "never indexable" reads as a mistake, and the next person to see it will fix it
back. The file states why it is what it is, at the only place someone will be looking.

### 2. `X-Robots-Tag: noindex, nofollow` remains the whole mechanism, unchanged

On every response, on every path, not an operator setting (PRD §14.2). It covers what a meta
tag could not — JSON responses, assets, error pages — and it now covers what it was always
supposed to: it gets read.

### 3. The middleware stays, only its body changes

Deleting the route is not the same as allowing the crawl. The SPA catch-all answers unknown
paths with `index.html` and a `200`, so a removed `robots.txt` comes back as HTML — which a
crawler reads as "no rules" while an operator reading a `curl` sees a working page. That
regression already happened once and is already covered by a test
(`TestRobotsBeatsTheSPACatchAll`). The middleware keeps owning the path, and keeps answering
before the SPA, in api-mode instances with no dashboard embedded too.

### 4. Applications, previews, ingress endpoints: nothing changes

The proxy has never served a `robots.txt` for a routed resource. Their noindex — unconditional
for previews (proxy-contract §4.5) and ingress endpoints (ADR-060 §5), opt-in per resource in
production (§4.7) — is a response header and nothing else, which is precisely the shape this
ADR concludes is correct. A tenant's own application may of course serve whatever robots.txt
it likes; that is the tenant's file, on the tenant's domain, and the platform does not touch it.

### 5. Operational note: an already-indexed URL needs a push

This ADR makes future indexing impossible and makes existing entries *droppable*. It does not
drop them on its own: the crawler has to come back, read the header, and remove the entry —
weeks, on a URL it has learned nothing changes at. An operator wanting it gone now asks their
search console for a removal (Google's is immediate but expires after ~6 months) — which only
sticks because the `noindex` is finally readable when it does. Both halves are in the manual
(`instance.md`); neither is a platform feature, and inventing a "de-index my instance" button
that drives third-party APIs is out of scope.

## Consequences

- Googlebot and friends will now fetch the login page, follow no link from it, and index
  nothing. A handful of unauthenticated requests, on a page built to face the internet.
- We give up an appearance of defence in depth that was never depth. Nothing is exposed that
  was not: every route beyond the login screen is behind the auth wall, and a crawler holds no
  session.
- The claim in the product documentation changes shape: not "we forbid crawlers", but "we let
  them in and tell them not to keep anything". The manual and the PRD are updated to say the
  second thing, with the reason, so the file does not get "fixed" back.
- One instance's index entry — and any other already indexed — will clear on the next crawl
  rather than never.

## Alternatives rejected

- **Keeping both, and adding a `<meta name="robots">` to `index.html`**: rejected. A meta tag
  is read after a fetch, exactly like the header, so a disallowed path hides it just the same;
  it would also only cover the one HTML document in an Angular SPA. It solves nothing that the
  header does not already solve better.
- **Disallowing everything except `/`**: rejected. The indexed URL *is* `/` — the login page is
  the only thing a crawler can reach anyway. Carving the exception around the one page that
  matters is the current situation with extra steps.
- **Removing the `robots.txt` route entirely**: rejected — see §3. Absence is answered by the
  SPA with a `200` of HTML, which is a worse robots.txt than the permissive one.
- **`Disallow:` with an empty value** (the historical spelling of "nothing is forbidden"):
  rejected in favour of `Allow: /`. Both are correct and equally supported; a line that reads
  `Disallow` on a file whose point is to permit is a trap for the next reader.
- **Making it an instance setting**: rejected, consistent with PRD §14.2 — no operator benefits
  from a dashboard in a search index, and an operator who wants the FQDN public can publish it
  themselves without handing it to crawlers.

## Verification

Unit tests, per the pyramid (ADR-026/028):

- `robots.txt` contains no `Disallow: /` — the regression test for this ADR, stated as the
  invariant rather than as an exact-body match so a later edit to the comment does not fail it.
- It is served as `text/plain`, ahead of the SPA catch-all, on `/robots.txt` and
  `/robots.txt/`, `HEAD` without a body, write methods `405` with `Allow` — the existing suite,
  unchanged.
- Every response, including `robots.txt` itself and error responses, carries
  `X-Robots-Tag: noindex, nofollow`: with the crawl now allowed, this header is the only thing
  standing between the dashboard and the index, so it is asserted on the paths a crawler
  actually reaches.
