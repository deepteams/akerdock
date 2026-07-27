# ADR-039 — Major upgrade of the instance's PostgreSQL: opt-in in-place via pgautoupgrade, backup-first

- **Status**: Accepted
- **Date**: 2026-07-27
- **Related PRD sections**: §14.3, §22.4, ADR-021, ADR-025
- **Supersedes**: the clause "no in-place `pg_upgrade` between container volumes" of the `upgrade-downgrade.md` runbook §C (wording, not an ADR) — replaced by the tooled path below.

## Context

The reference distribution (ADR-021) pins the PostgreSQL major version in `docker-compose.yml`. A Postgres major is **not in-place compatible** between container volumes: bumping the image (e.g. 16 → 17) on an existing volume makes the container crash-loop (`database files are incompatible`). Until now the only documented procedure was a manual dump/restore — slow on a large database, and its text described a bind-mount `mv postgres` that does not match the named volume actually used (`akerdock_pgdata`). Since the database contains **all** of the instance's state (state + queue, ADR-025), the operation is the most destructive one there is; automating it silently in `install.sh` would break that install's "nothing is lost" invariant.

## Decision

- The major upgrade remains **opt-in and explicit** — never launched automatically during `install.sh` nor at boot.
- `install.sh` (and the boot error message) **detects** the gap between the volume's major and the pinned one, and **stops cleanly** while pointing to the tool — instead of letting the container crash-loop. No data is touched by the detection.
- The `scripts/pg-upgrade.sh` tool performs the **in-place** upgrade via the third-party image **`pgautoupgrade/pgautoupgrade`** (which bundles the source + target binaries and runs `pg_upgrade`), in one-shot mode, **preceded by a full copy of the data volume** (filesystem-level, version-agnostic) kept under `backups/` as the sole rollback. The stack then restarts on the **official** `postgres:<major>` image — `pgautoupgrade` is only used during the migration window, never as a permanent runtime image.

## Alternatives considered

- **Silent automatic in-place in `install.sh`**: rejected — destructive operation on the datastore without a human checkpoint or backup guarantee, contrary to the "never risk persistent state" ethos (INV-015).
- **Dump/restore only (status quo)**: kept as a documented **fallback**, but slow on large volumes and without boot-time detection; in-place `pg_upgrade` is significantly faster.
- **`pgautoupgrade` as the permanent runtime image**: rejected — permanent third-party supply chain (ADR-021 aims for minimal/official); it is only exposed for the duration of the migration.
- **Upgrade orchestrated by the akerdock binary itself**: rejected — distroless without `pg_*` tools, and the control plane talks to Postgres over the network, it cannot migrate an on-disk format.

## Consequences

- **Positive**: no more cryptic crash-loop after a major bump (detection + guided stop); fast, tooled, backup-first upgrade with explicit rollback; the runtime image remains the official one.
- **Negative**: one-off dependency on a third-party image (`pgautoupgrade`), to be pinned and scanned; the operator must have ~2× the volume's space available (copy + upgrade) for the duration of the migration.
- **Accepted risks**: a failed in-place upgrade potentially leaves a half-migrated volume — mitigated by the mandatory prior copy and an explicit restore message; the rollback copy is only deleted after several days of verified operation.
