# ADR-035 — Preview routes via a template table

- **Status**: Accepted
- **Date**: 2026-07-26

## Relation to other decisions

Revises the "single `preview_url_template`" wording of §5.6 (PRD)
without touching the rest of the preview scope (ADR-011).

## Context

A preview could only expose **one** URL, derived from a single
`preview_url_template` (`{{pr_id}}`, `{{domain}}`, `{{random}}`). Compose
multi-service was handled by a non-configurable automatic `<service>-<base>`
prefix.

That is too limited for serious use of the PR→preview flow: the operator wants
the same control as with **application routing** (multiple hosts, one target
port per route), not a single imposed pattern.

## Decision

The preview URL configuration becomes an **ordered table of routes**, modeled
on application routing (neighboring ADR-034, §4.2):

- Each row = `{ host, port? }` where `host` is a pattern with placeholders
  **`{{pr_id}}`**, **`{{service}}`**, **`{{domain}}`** (the app's 1st domain),
  **`{{random}}`** (stable slug per preview).
- A row **without `{{service}}`** = an explicit route; the target service is
  resolved via the `port` (like an application domain, `resolveWebComponent`).
- A row **with `{{service}}`** = a template applied to **each served service**
  not already covered by an explicit row; the port is the one resolved for the
  service (the row's `port` takes precedence).
- Storage: column `applications.preview_url_templates` (JSONB, array).
  **Backward compatibility**: empty/absent ⇒ historical behavior
  (single `preview_url_template` + auto-prefix). No backfill required.
- `{{random}}` relies on `previews.random_slug`, generated once at scaffolding
  and reused for all routes/deployments — hosts remain stable
  (no certificate churn).
- The preview's **primary** host (`previews.fqdn`, displayed, SSO, feedback) =
  the 1st resolved row (`{{service}}` → main web component).

The single-level wildcard (§4.2) remains the constraint: a pattern producing
multiple levels under the wildcard does not get a certificate (unchanged,
stated).

## Consequences

- **Positive**: UX parity with app routing, multi-hosts/ports per preview,
  controlled patterns instead of the imposed prefix; backward-compatible.
- **Negative**: richer preview routing engine (shared resolver
  `single-container` + compose); `random_slug` added; increased test surface.
  The deterministic logic (pattern resolution, port→service mapping) is
  proven by unit tests; the end-to-end proxy/cert/SSO behavior falls under
  manual/E2E validation (ADR-028).

## Rejected alternatives

- **Keep the single template + more placeholders**: does not provide the
  requested per-route/port control.
- **Base + per-service overrides**: less rewrite but two competing mechanisms;
  the single table is more consistent with app routing.
