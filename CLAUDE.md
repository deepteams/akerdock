# AkerDock — project conventions

Self-hosted PaaS in Go: deployment of applications, databases and compose stacks onto SSH servers, with proxy, automatic HTTPS, backups and PR previews.

**Engineering conventions live in [CONTRIBUTING.md](CONTRIBUTING.md)** (repository layout, spec-first workflow, migrations, commits). Summary of the choices: code/comments/commits **in English** (Conventional Commits); design docs in English; **goose** migrations; monorepo (Angular UI in `web/`); tools pinned via the `tool` directive of `go.mod` (`go tool sqlc|oapi-codegen|goose`).

## Sources of truth

- `docs/PRD.md` — product specification. Sections 1–14 describe the functional scope; sections 16+ are the verifiable requirements (normative keywords MUST / MUST NOT / SHOULD / MAY).
- `docs/adr/` — 47 accepted ADRs (index: `docs/adr/README.md`). **An accepted ADR is immutable**: any revision of the DECISION goes through a new ADR that supersedes the old one. (The wording itself may be fixed in place — rephrasing is not deciding.) Any structural decision requires an ADR + an entry in the tracking grid (PRD §26).
- `docs/specs/openapi-v1.yaml` — API contract. **Spec-first**: Go handlers and the TypeScript client are generated from this file (oapi-codegen), never written by hand and documented after the fact. The spec stays on **OpenAPI 3.0.3** (oapi-codegen does not support 3.1). After any change: `make generate` and commit the generated code (CI checks the synchronization).

## Mandated stack (ADR-025, ADR-021)

- PostgreSQL is the only external dependency: state **and** job queue (no Redis/NATS).
- pgx + sqlc (explicit SQL typed at compile time, versioned migrations), chi + oapi-codegen.
- Deliverable: static Go binary, distroless image, all-in-one/api/worker modes.

## Conventions

- Documentation in **English**; code, identifiers and commit messages follow standard Go usage.
- Predefined variables prefixed `AKERDOCK_*` — never an alias under another brand (ADR-022). The project's former name was "dockerbox": do not reintroduce that name.
- Real time: SSE with `Last-Event-ID` resumption; WebSocket reserved for the terminal (ADR-024).
- Tests: most of the coverage is **unit-level** (any new logic MUST be unit-tested); there is exactly **one E2E journey**, Docker-in-Docker, run post-merge and pre-release, never sharded and no nightly catalog (ADR-026/028, test plan §2).
