# ADR-029 — Seeding PR previews by volume clone

- **Status**: Accepted
- **Date**: 2026-07-24
- **Related PRD sections**: §20.4 (previews), §5.2 (compose), INV-010
- **Revises**: nothing — extends the compose previews contract (§20.4.4)

## Context

A PR preview starts with empty volumes: this is the direct consequence of
INV-010 (a preview never mounts production data) and it is the right default.
But a stack whose core is a database then becomes hard to review: the PR
instance works, technically, on a database with no data — real functional
evaluation requires a dataset.

One-shot jobs (`restart: no`, compose-spec §7.3) already cover seeding via
versioned fixtures. They do not cover the need for "production-like data,
ready to use, without maintaining fixtures".

## Decision

A named volume of a compose stack MAY declare, at the level of its top-level
declaration:

```yaml
volumes:
  pgdata:
    x-akerdock:
      preview_seed: clone
```

Semantics, for a **preview** deployment only:

1. The preview's volume (`<uuid-preview>_<name>`), if it is **still empty**,
   is initialized by **copying** the corresponding production volume
   (`<uuid-app>_<name>`), just before the first start of the service that
   mounts it — after the build/pull, hence with the service's image available.
2. The copy runs in an ephemeral container of the service's image
   (`--user 0`, `cp -a`), with the source mounted **read-only**: production
   is never mutated, owners/permissions are preserved. The service's image
   MUST provide `/bin/sh` (true for all common database images); a copy
   failure fails the preview deployment — a silently empty seed would
   contradict the declared intent.
3. A non-empty volume is **never** touched again: redeployments of an
   existing preview keep its data.
4. If the production volume does not exist yet (stack never deployed), the
   seed is skipped — there is nothing to copy, and the production volume must
   not be created as a side effect.
5. `preview_seed` is rejected at validation on an `external:` volume (adopted
   objects are production itself) and in raw compose mode (names are not
   prefixed there: production and preview would designate the same volume).

## Relationship to INV-010

INV-010 forbids **mounting** production data in a preview and any access
"without an explicit policy". `preview_seed: clone` is precisely that
explicit policy: a per-volume opt-in, versioned in the repository's compose
file, which produces a **disposable copy** destroyed with the preview
(`previewdestroy`). Preview protections remain intact: basic auth by
default, PRs from forks subject to approval. The operator who enables the
clone accepts that the volume's data is visible behind those protections —
that is the acknowledged trade-off of this decision.

## Documented limitation: copy consistency

A file-by-file copy of a database **being written to** is equivalent to a
post-crash snapshot: PostgreSQL and its kin replay it via their journal in
the vast majority of cases, without an absolute guarantee. This is an
accepted trade-off for review usage; for a consistency guarantee, the
alternatives remain the one-shot fixtures job or a logical dump (alternative
rejected below, open to re-evaluation).

## Alternatives considered

- **Logical dump (pg_dump → restore)**: consistent by construction, but
  specific to each engine (credentials, tooling, restore time) — rejected as
  the base mechanism; the clone is generic. Open to re-evaluation as an
  additional mode (`preview_seed: dump`) if the need is confirmed.
- **Filesystem snapshot (LVM/ZFS)**: excellent consistency, but imposes an
  infrastructure assumption that AkerDock makes nowhere else — rejected.
- **Fixtures only**: already covered by one-shots; does not address the need
  for "production data without fixture maintenance".

## Consequences

- Extension of the compose parser (strict validation, dedicated findings)
  and of the compose deployment engine (per-service seed script, before the
  first start, previews only).
- The scope is **compose only**: single-container application storages may
  receive an equivalent flag later, in an amendment to this decision.
- Tracking grid §26.2: the "Enriched PR previews" capability includes
  seeding by volume clone.
