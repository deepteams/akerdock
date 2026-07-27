# ADR-025 — Go/API technical foundation: pgx + sqlc, versioned SQL migrations, chi + oapi-codegen spec-first

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.25, §24.1, §25.2, §18.2, ADR-002

## Context

The Go control plane needs a data-access foundation and an API toolchain. The critical paths — queue, leases, outbox in PostgreSQL (ADR-002) — rely on precise SQL (locks, `SKIP LOCKED`, transactions) that an ORM hides or degrades. On the API side, the PRD requires OpenAPI to be a versioned artifact tested in CI (§24.1) and the UI's TypeScript client to be generated from that same artifact (§25.2) to prevent any UI/API drift. These structuring choices must be fixed before the first line of code.

## Decision

- **PostgreSQL access via pgx + sqlc**: explicit SQL, types checked at compile time — essential for the critical **queue/leases/outbox** queries; **versioned SQL migrations**.
- **Spec-first API** with the **chi** router and **oapi-codegen**: the **Go handlers** and the **UI's TypeScript client** (§25.2) are generated from the **same OpenAPI artifact** (§24.1).

## Alternatives considered

- **ORM (GORM/ent)**: rejected — an abstraction ill-suited to the critical queue/leases/outbox queries (locks, `FOR UPDATE SKIP LOCKED`), hidden cost in performance and control; sqlc provides type safety without hiding the SQL.
- **Code-first (OpenAPI generated from the Go code)**: rejected — the spec becomes a by-product instead of a contract; spec-first guarantees that the Go handlers and the TypeScript client derive from the same artifact, with no possible drift.
- **Heavy web frameworks (gin, echo) or gRPC-gateway**: rejected — chi is a minimal router compatible with standard `net/http`, sufficient and lock-in free; gRPC would add a translation layer for a contractual public REST API.

## Consequences

- **Positive**: critical queries auditable and optimizable in native SQL, verified at compile time; a single API contract from which server and client derive (no UI/API drift possible); minimal dependencies that are standard in the Go ecosystem; explicit SQL migrations compatible with rolling upgrades (§18.2).
- **Negative**: sqlc requires writing all SQL by hand (more verbose than an ORM for simple CRUD) and a code-generation step in the build; spec-first requires maintaining the OpenAPI artifact ahead of every endpoint change, with the corresponding CI discipline (§24.1).
- **Accepted risks**: deliberate coupling to PostgreSQL (no multi-DBMS portability — consistent with ADR-002 and ADR-021); dependency on third-party generators (sqlc, oapi-codegen) whose evolution must be tracked; design mistakes in the OpenAPI spec propagate mechanically to the server and the client.
