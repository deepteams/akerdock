# ADR-057 — Base-repository previews resolve shared variables

- **Status**: Accepted
- **Date**: 2026-08-01
- **Related**: PRD §5.4, §5.6, §20.4(8), INV-010, [ADR-011](ADR-011-enriched-previews-from-delivery.md), [ADR-029](ADR-029-seed-preview-clone-volume.md), [ADR-050](ADR-050-public-routes-in-previews.md)

## Context

Shared variables (§5.4) let a value reference `{{team.KEY}}`, `{{project.KEY}}`
and `{{environment.KEY}}`, and inject server-scoped variables into every
resource deployed on a server. Until now the deployment engine skipped that
whole resolution for **every** preview, citing INV-010; only the
`{{deployment.*}}` pseudo-scope resolved. A preview variable whose value was
`{{environment.SOME_URL}}` therefore reached the container verbatim — the
deliberate "visibly unresolved" behavior for unknown references — and teams
that model prod/dev as separate AkerDock environments could not point a
preview at the dev environment's shared values without duplicating them by
hand into every preview set.

That blanket skip was stricter than the invariant it cited. INV-010 reads: "an
**untrusted PR or one coming from a fork** obtains no production secret".
§20.4(8) adds that even an approved fork gets "no secret injected". Nothing in
the PRD forbids a PR from the base repository — authored by someone with write
access to the repo that already builds and runs this code — from resolving the
shared scopes its own resource inherits. The environment scope in particular
resolves against the resource's **own** environment: a preview of a dev-environment
resource sees dev values, never another environment's.

## Decision

1. A preview whose PR lives in the base repository (`is_fork = false`)
   resolves shared variables exactly like a production deployment of the same
   resource: `{{team.*}}`, `{{project.*}}` and `{{environment.*}}` references
   interpolate inside its (still strictly dedicated, §5.6) variable set, and
   the destination server's server-scoped variables are injected unless the
   preview set defines the key itself.
2. A fork preview (`is_fork = true`) never resolves the shared scopes and
   never receives server-scoped variables — **approval included**: approval
   grants a build and a URL (§20.4(8)), never secrets. Only `{{deployment.*}}`
   resolves, as before.
3. The preview variable set stays dedicated: nothing is copied implicitly.
   Shared values reach a preview only through a reference the team wrote
   explicitly into that preview set, or through server-scope injection the
   team configured — the same explicit acts that expose them to production.
4. Unknown references keep resolving to themselves (a visible placeholder
   beats a silently empty value), which is also what a fork preview shows for
   any shared reference.

## Consequences

- Teams with separate prod/dev environments reference per-environment shared
  values from preview sets instead of duplicating them per application.
- The fork boundary is now the single place where INV-010 bites, matching its
  text; the trust decision (base repo vs fork) is already made and recorded by
  the preview trigger pipeline.
- A base-repo preview of a **production-environment** resource can reference
  that production environment's shared values. That is explicit, user-authored
  configuration in a security-relevant place — same stance as ADR-050 —
  and no more implicit than production itself resolving them.
