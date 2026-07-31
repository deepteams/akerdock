# ADR-054 — Host operations through the agent: pure-Go file primitives on the mounted tree

- **Status**: Accepted
- **Date**: 2026-07-31
- **Related**: [ADR-051](ADR-051-docker-runtime-adapter.md), [ADR-052](ADR-052-agent-command-channel.md), [ADR-053](ADR-053-compose-config-hash-v2.md); builds and git move with ADR-055 (BuildKit).

## Context

With Docker-core migrated (ADR-051/052/053), what SSH still executes falls
into families: file deposits and reads under `/var/lib/akerdock` (proxy
routing, TLS material, waker tables, tree removals), git operations, backup
pipelines, the build path, and the bootstrap that installs the agent itself.

Two facts shape the design. The helper container is **distroless** — no
shell, no git, no curl; anything it executes must be plain Go. And it
mounted only its own corner (`/var/lib/akerdock/waker`): the rest of the
tree was invisible to it.

## Decision

1. **Host-ops ride the command channel.** The ADR-052 vocabulary grows a
   file family — `FileWrite`, `FileRead`, `FileRemove`, `FileStat`,
   `FileChown`, `FileCopy`, `DirEnsure` — executed by the agent in pure Go
   (`internal/hostops`). Same rail, same telemetry, same mandatory-agent
   stance: no shell fallback below it.
2. **The helper mounts the whole tree.** `/var/lib/akerdock` replaces the
   waker-dir mount (spec 7). Every path is validated agent-side against that
   root — absolute, clean, inside — and a violation answers a typed
   invalid-argument. A pre-spec-7 helper (no mount) answers a typed
   unavailability; the existing reconciliation recreates it.
3. **Semantics are explicit.** Modes are applied against the umask, never
   left to it; `Atomic` stages next to the target and renames, so a watching
   reader (the proxy, the waker) never sees a partial file; a read of an
   absent file answers `Found=false` — absence is data, not an error;
   removals are idempotent.
4. **Ownership is stated, not inherited.** The agent runs as root, so files
   it writes are root-owned (SSH wrote them as the SSH user). Key material
   is chowned explicitly — the postgres uid is probed from the image with a
   typed one-shot container, retiring the TLS path's last docker CLI use.
5. **Delivery in tranches.** A (this ADR's delivery): the file family —
   certificate sync and forced renewal (no SSH left at all), database
   config/TLS deposits and TCP route file, resource-tree removals
   (application delete, preview destroy, database delete), remnant
   inventory. B: the proxy-routing family (ProxyApplier, drift
   reconciliation, STZ activity reads) together with agent provisioning at
   server validation — those sites serve servers whose agent may not exist
   yet, and move only once it always does. C: pipe primitives for backups
   (exec↔file with compression, url↔file for presigned transfers, file
   hashing).
6. **What stays SSH.** First-contact validation and the agent's own
   bootstrap/repair/upgrade (nothing else can carry them); the server-shell
   PTY; break-glass. Git stays SSH until ADR-055 decides the build/source
   story. Adoption's compose reads stay SSH **by design**: unmanaged stacks
   live at arbitrary host paths, outside the allowlisted root.

## Consequences

- The worker's relay needed no change: host-ops are unary commands, and the
  bridge is method-agnostic.
- CertificateSync drops its SSH dial entirely; database provisioning keeps
  one, solely for the proxy-bootstrap convergence (family B's subject).
- Ownership shift: files under `/var/lib/akerdock` written by migrated paths
  are now root-owned. Directories the SSH-side build/git path writes into
  (cloned sources, build env deposits) deliberately did NOT migrate — a
  root-owned parent would break the SSH user's clone.
- `internal/hostops` carries the interface, the guarded local implementation,
  the channel client and a typed fake; unit coverage pins the guard, the
  atomic write, absence-is-data, idempotent removal and the executor's
  unavailability contract.
