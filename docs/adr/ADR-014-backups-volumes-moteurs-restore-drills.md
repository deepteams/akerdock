# ADR-014 — Backups beyond databases: encrypted/deduplicated volumes, Redis and ClickHouse, restore drills

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.14, §20.5, §7, §15, §26.2

## Context

Backing up only the SQL/Mongo engines leaves half the state on the table: application data outside databases (uploads, files of one-click services) is not covered. And a backup that has **never been restored** offers no guarantee (§7, §15). A decision is needed on the backup scope and the level of proof required.

## Decision

Three extensions recorded (detailed requirements in §20.5), on top of §7 parity:

1. **Application volume backups**: backup plans on the volumes and bind mounts of applications and services — not only databases — **encrypted and deduplicated** (restic-style tool), with a per-resource quiesce/stop option for consistency, and the same scheduling, local/S3 retention and notifications as database backups.
2. **Additional engines**: **Redis** (RDB snapshot) and **ClickHouse** covered natively, lifting the parity limitation (§15).
3. **Automatic restore drills**: periodic restoration test in a disposable environment — real restoration + integrity verification (checksum, counting) — with an alert if a backup plan turns out to be non-restorable. A backup that has never been restored is not considered reliable.

This capability is classified **P1** in the parity matrix (§26.2).

## Alternatives considered

- **Strict parity (4 engines, no volumes)**: rejected — leaves application data unprotected and reproduces the reference's most serious flaw: restores that are never proven.
- **Delegating volumes to an external tool (restic/borg managed by the user)**: rejected as a product answer — without integration into the platform's scheduling, retention and notifications, coverage remains hit-or-miss; the restic-style tool is however retained as an internal building block.
- **Infrastructure-level snapshots (LVM/cloud snapshots)**: rejected — provider-dependent, not portable between servers, outside the "any Linux server" contract.

## Consequences

- **Positive**: complete data coverage (databases + volumes); encryption and deduplication reduce storage cost and exposure; drills turn backups into a measured guarantee rather than a hope; a strong differentiator classified P1.
- **Negative**: dependency on a file backup tool (restic-style) to integrate, supervise and update; the optional quiesce/stop introduces a consistency/availability trade-off the user must understand; drills consume CPU, disk and time on disposable infrastructure that must be provisioned.
- **Accepted risks**: a volume backup without quiesce may be inconsistent for applications that write continuously (per-resource choice, documented); drills validate technical restorability, not the business validity of the restored data.
