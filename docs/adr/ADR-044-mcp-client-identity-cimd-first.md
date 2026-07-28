# ADR-044 — MCP client identity: CIMD by default, dynamic registration only on instance opt-in

- **Status**: Accepted
- **Date**: 2026-07-28
- **Revises**: [ADR-043](ADR-043-mcp-server-oauth-and-cli.md) §3 (client identification only; every other decision of ADR-043 stands)
- **Related PRD sections**: §12, §21, §23.3

## Context

ADR-043 shipped the OAuth path for remote MCP clients with **dynamic client
registration** (RFC 7591): anyone POSTs a name and redirect URIs, the instance
mints an opaque `client_id` and stores the row. It is what MCP clients
implement today, and it works — but the identity it produces is self-declared
and unverifiable. The consent screen can only say "a client named X asks for
access", where X is a string the caller chose. Nothing ties it to whoever
actually operates the client, and the instance accumulates registrations from
anyone who can reach it.

**Client ID Metadata Documents** (CIMD) invert this: the `client_id` IS an
HTTPS URL, and the authorization server fetches that document to learn the
client's metadata. Identity becomes a domain the client demonstrably controls
— the same anchor a browser padlock gives — and the server stores nothing.
Its cost is a server-side fetch of a caller-supplied URL, which is the textbook
SSRF vector, and a specification still in draft with uneven client support.

## Decision

CIMD is the default and preferred identification; DCR survives as an explicit,
audited instance opt-in.

1. **CIMD is always accepted**: a `client_id` that is an `https://` URL is
   resolved by fetching that document. The document MUST declare a
   `client_id` **equal to the URL it was fetched from**, and every
   `redirect_uri` MUST share that document's origin — otherwise anyone could
   host a document impersonating another client.
2. **The fetch is SSRF-guarded**: it uses the instance's hardened outbound
   client (`internal/safedial`, PRD §23.3) — no loopback, no link-local, no
   private range — plus HTTPS only, a short timeout, a capped body and no
   redirect following. A short in-memory cache keeps a consent round-trip from
   re-fetching every time.
3. **DCR is off by default**: `POST /oauth/mcp/register` answers `403` with a
   message naming CIMD as the way in. An instance root may enable it in Global
   settings when a client they need does not implement CIMD yet; enabling it
   is audited, and the authorization-server metadata then — and only then —
   advertises a `registration_endpoint`.
4. **Consent shows the verified identity**: for a CIMD client, the origin of
   the document is displayed as the client's identity, because that is the
   part nobody can forge. A DCR client keeps showing its self-declared name,
   which is exactly the difference the operator opted into.
5. **Clients already registered keep working**: an opaque `client_id` still
   resolves against `mcp_oauth_clients` whatever the toggle says — turning DCR
   off stops NEW registrations, it does not revoke the grants of an assistant
   that already works.

## Alternatives considered

- **DCR only (ADR-043 as shipped)**: rejected — self-declared identity on a
  consent screen is a phishing surface, and open registration is unbounded
  state written by anonymous callers.
- **CIMD only, no DCR at all**: rejected for now — the draft's client support
  is uneven, and an instance operator must be able to onboard a client that
  only speaks RFC 7591 rather than being told to wait for the ecosystem.
- **Allowlisting client domains instead of a toggle**: rejected as the first
  step — it is a finer control that only makes sense once operators report
  needing it; the binary toggle plus CIMD's verifiable origin covers the
  stated need. Reassessable upon proven demand.

## Consequences

- **Positive**: the default path yields an identity the operator can verify
  (a domain), with no server-side registration state; the instance stops
  accepting anonymous writes on a public endpoint by default; the consent
  screen finally says something trustworthy.
- **Negative**: the instance now performs an outbound fetch during an
  authorization it did not initiate — bounded by the SSRF guard, HTTPS-only,
  timeout and body cap, but it is a new outbound behavior to keep in the
  threat model; a client that implements neither CIMD nor a maintained
  registration path cannot connect without the operator flipping the toggle.
- **Accepted risks**: CIMD is a draft — its document shape may change, which
  is a versioning problem confined to one resolver; a hostile document is
  bounded to advertising redirect URIs on its own origin, which is precisely
  what makes the identity meaningful.
