# ADR-049 — Narrow public routes through an application access wall

- **Status**: Accepted
- **Date**: 2026-07-30
- **Revises**: [ADR-042](ADR-042-application-access-protection.md)
- **Related**: [ADR-009](ADR-009-proxy-intermediate-representation.md), [ADR-030](ADR-030-preview-sso-forward-auth.md)

## Context

ADR-042 deliberately made an access wall application-wide. That default is
safe, but it also blocks machine-to-machine endpoints which cannot complete an
interactive login: signed webhooks and HTTP callbacks are the common cases.
Splitting one workload into two applications solely to expose one callback
duplicates deployment state and does not work naturally for a Compose service
which serves both its user interface and its webhook handler.

An exception is security-sensitive. A free-form proxy expression or regular
expression would make a typo capable of publishing substantially more than the
operator intended.

## Decision

1. A protected resource MAY declare `access_public_routes`. Each route has an
   absolute path, a match mode and an explicit non-empty list of HTTP methods.
2. Match modes are:
   - `exact`: the request path must be equal;
   - `template`: a `:name` segment matches exactly one non-empty URL segment
     made of RFC 3986 unreserved characters (`A-Z a-z 0-9 - . _ ~`);
   - `prefix`: the declared segment and its descendants match. `/hooks` matches
     `/hooks` and `/hooks/...`, never `/hooks-admin`.
3. A template parameter MAY carry an allow-list. Without one it matches one
   syntactically valid URL segment. Arbitrary regular expressions, `*` and
   `**` are not accepted.
4. A single-container application declares the routes on its access policy.
   Compose declares them per service under
   `services.<name>.x-akerdock.access_public_routes`; the exception therefore
   applies only to domains routed to that component.
5. The proxy IR owns the distinction between protected and public routers.
   A public router has higher deterministic priority and omits only the access
   middleware. HTTPS redirection, scale-to-zero wake-up, noindex and any other
   non-access middleware remain in force.
6. Access protection is generalized to both application resources and inline
   Compose service resources. SSO grants are bound to the resource identity,
   not inferred from a host header.
7. Invalid or unavailable protection material fails closed. A protected
   resource is never rendered publicly because a credential could not be
   decrypted or the SSO endpoint could not be resolved.
8. This setting concerns production resource URLs. Preview protection remains
   an independent policy.

## Consequences

- Webhook and callback endpoints can remain reachable while the surrounding
  application stays protected.
- Compose exceptions are versioned with the service that owns the endpoint.
- Route generation becomes policy-aware in the provider-independent IR instead
  of rewriting generated YAML after the fact.
- Operators must enumerate methods and opt into broad prefix matching
  explicitly. A new public route is security-relevant configuration and is
  included in the normal resource update audit.
