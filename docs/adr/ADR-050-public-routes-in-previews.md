# ADR-050 — Public access-wall routes are inherited by previews

- **Status**: Accepted
- **Date**: 2026-07-30
- **Revises**: [ADR-049](ADR-049-auth-wall-public-routes.md)
- **Related**: [ADR-030](ADR-030-preview-sso-forward-auth.md), [ADR-035](ADR-035-preview-route-templates.md)

## Context

ADR-049 limited `access_public_routes` to production URLs while keeping preview
protection independent. That makes a valid machine-to-machine endpoint work in
production but redirect to the preview login during pull-request validation.
Webhook senders and MCP clients cannot complete that interactive login, so the
preview no longer represents the behavior being reviewed.

A second preview-only exception list would duplicate security-sensitive
configuration and could drift from production.

## Decision

1. A preview inherits the `access_public_routes` of the resource it previews.
   No preview-specific public-route parameter is introduced.
2. A single-container application inherits its application-level list.
3. A Compose preview inherits each service's
   `services.<name>.x-akerdock.access_public_routes`; an exception applies only
   to the preview hosts routed to that service.
4. The inherited route keeps the exact same path, match mode, parameter
   allow-lists and explicit HTTP methods as production.
5. A public preview router omits only the preview access middleware. HTTPS,
   `X-Robots-Tag: noindex`, scale-to-zero wake-up and all other applicable
   middleware remain active.
6. `preview_protection = none` remains unchanged: the ordinary preview route is
   already public, so inherited exceptions have no additional effect.

## Consequences

- PR validation can exercise webhooks, HTTP callbacks and MCP routes without an
  interactive preview login.
- The reviewed Compose file is the single source of truth for production and
  preview exceptions.
- Exposing a route in production also exposes the same narrow route in every
  protected preview. This is intentional and visible in the same
  security-relevant configuration change.
