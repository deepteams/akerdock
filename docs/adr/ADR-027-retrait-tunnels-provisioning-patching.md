# ADR-027 — Scope removal: Cloudflare tunnels, cloud provisioning, server patching

- **Status**: Accepted
- **Date**: 2026-07-14
- **Related PRD sections**: §3.2, §3.6, §26.2

## Context

The PRD inherited three P3 capabilities from parity with the segment: **Cloudflare tunnels** (§3.6 — exposure without a public IP), **cloud provisioning** (§3.2 — provider tokens + Hetzner VPS creation) and **server patching** (§3.2 — APT/DNF/Zypper updates from the dashboard). None is implemented, none has a verifiable requirement beyond its description, and each extends the product surface toward an adjacent trade: access networking for tunnels, fleet management for provisioning, OS administration for patching — three things a well-equipped operator already does better elsewhere, and that the threat model would have to cover in full if the product carried them.

Three naming ambiguities make the removal treacherous: **Cloudflare** remains a shipped **DNS-01** provider (wildcards, amendment No. 21), **Hetzner** remains a **DNS-01 and S3** provider (§4.3, §7.2), and the **`cloud_credentials`** table exists in the database (migration 00035) but carries the **DNS-01 credentials** — not the provisioning tokens the dictionary described.

## Decision

**Cloudflare tunnels, cloud provisioning (provider tokens + VPS creation) and server patching are removed from the product scope.** PRD sections §3.2 and §3.6 are emptied in favor of a reference to this ADR (section numbering is stable, it does not move); the §26.2 grid carries the row with the status `Abandonné`.

**Not** affected: DNS-01 (Cloudflare, Hetzner and any Lego provider — shipped), S3-compatible providers (Hetzner included), the `cloud_credentials` table (real, it carries the DNS-01 credentials — the dictionary is corrected to say what the database actually does), and the cloud provider firewall recommended by the threat model (the user's responsibility, as before).

This decision is **re-evaluable upon proven user demand**, capability by capability.

## Alternatives considered

- **Keeping all three at P3 indefinitely**: rejected — a capability that is specified but never prioritized is debt: it maintains tables, permissions (`cloud:manage`), threat model entries and API promises for code that does not exist.
- **Removing only from the TODO, keeping the PRD intact**: rejected — the TODO is the operational tracking, the PRD is the source of truth (CLAUDE.md); a scope that lives only in the TODO diverges at the first re-read.
- **Implementing minimally (tokens without provisioning, read-only patching)**: rejected — a half-capability has the surface cost of a full one without its value.

## Consequences

- **Positive**: scope refocused on the core (deployment, databases, backups, adoption); removal of the `cloud:manage` permission, of the "VPS deletion" sensitive action and of the `cloud_provider` enum (never created in the database); the dictionary becomes accurate again about `cloud_credentials`.
- **Negative**: a server without a public IP has no managed access path (tunnels would have provided it); server creation remains entirely manual; OS updates remain the operator's responsibility — accepted, this is already the threat model's position on hardening.
- **Accepted risks**: if demand emerges, reintroduction will require a new ADR and will start over from the spec — nothing about the removal is irreversible, no data is destroyed.
