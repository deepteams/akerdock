# Contributing to AkerDock

This document defines the engineering conventions for the project. The product
design lives in `docs/`: the [PRD](docs/PRD.md) is the
functional reference, the [ADRs](docs/adr/README.md) are binding architecture
decisions, and `docs/specs/` holds the technical specifications.

## Language

- **Code, comments, error messages, commit messages, and this kind of
  contributor documentation: English.**
- Design documents under `docs/` (PRD, ADRs, specs, runbooks): English.
- UI strings: English via i18n keys, never hardcoded (PRD §25.2).

## Repository layout

```
cmd/akerdock/        Entry point — single binary, modes all-in-one/api/worker (ADR-021)
internal/            Private application code
  api/               HTTP layer: oapi-codegen output (api.gen.go) + handler implementations
  store/             Data access: sqlc output over pgx
db/
  migrations/        Goose SQL migrations (also the schema source for sqlc)
  queries/           sqlc query files (*.sql)
docs/                Product and architecture documentation (French)
web/                 Angular SPA, embedded into the Go binary at build time (PRD §25.2)
```

Monorepo: the Angular UI lives in `web/` in this repository so a single commit
can change the OpenAPI spec, the server, and the generated TypeScript client
together.

## Spec-first workflow (ADR-025)

`docs/specs/openapi-v1.yaml` is the contract. It is edited **before** any
endpoint change; Go server stubs and the TypeScript client are generated from
it, never written by hand and then documented afterwards.

1. Edit the OpenAPI spec.
2. `make generate` — regenerates `internal/api/api.gen.go` (and later the
   TypeScript client in `web/`).
3. Implement or adjust the handlers.
4. Commit the spec, the generated code, and the handlers together. CI fails if
   generated code is out of date (`make generate && git diff --exit-code`).

### The pre-commit hook

Two artefacts are built from sources that live elsewhere in the tree and are
compared byte for byte in CI: the generated code above, and the embedded
dashboard (`internal/web/dist`, which the binary serves — never `web/src`). A
commit that changes one without the other is internally inconsistent, and
nothing local says so; CI says so twenty minutes later.

```sh
make hooks          # or just run `make generate` / `make web` once
```

Git cannot ship the *activation* of a hook, only the hook itself: a clone that
armed its repository's hooks would be arbitrary code execution on `git clone`,
so `core.hooksPath` is deliberately a local act. `make hooks` is therefore a
prerequisite of `generate` and `web` — the two targets whose output the hook
guards — so it arms itself the first time you regenerate anything. It is
idempotent, silent once set, and skipped under `CI`.

The hook **checks**, it does not regenerate into your commit: the build reads
the working tree while the commit carries the index, so regenerating and
staging would let a partly staged change ship an artefact built from sources
that commit does not contain. It costs nothing on a commit that touches
neither source set, around 12 s on one that touches `web/`, and a couple of
seconds on one that touches the spec, the queries or the migrations. Bypass
with `git commit --no-verify` or `AKERDOCK_SKIP_PRECOMMIT=1`.

## Database (ADR-002, ADR-025)

- PostgreSQL is the only external dependency: application state **and** the
  job queue (leases, outbox). No Redis, no external bus.
- Access goes through **pgx + sqlc**: explicit SQL in `db/queries/*.sql`,
  compile-time-checked Go generated into `internal/store`. No ORM.
- Schema changes are **goose migrations** in `db/migrations`
  (`NNNNN_description.sql` with `-- +goose Up` / `-- +goose Down` sections).
  Migrations must stay compatible with rolling upgrades (PRD §18.2): additive
  first, destructive changes only once no supported version reads the old
  shape.
- After changing a migration or a query: `make generate`.

## Coding conventions

- `gofumpt` + `goimports` formatting (enforced by golangci-lint; local prefix
  `github.com/deepteams/akerdock`). Run `make lint` before pushing.
- Errors: wrap with `fmt.Errorf("...: %w", err)`, sentinel errors as
  `Err`-prefixed variables. Never discard errors silently.
- Comments explain *why*, not *what*; exported identifiers get doc comments.
- Generated files (`*.gen.go`, `internal/store`) are never edited by hand.
- Predefined environment variables use the `AKERDOCK_*` prefix — never
  aliases under another vendor's prefix (ADR-022).
- Secrets and key material never appear in logs, error messages, or test
  fixtures (see `docs/specs/threat-model.md`).

## Commits and pull requests

- **Conventional Commits** in English: `type(scope): summary` with types
  `feat`, `fix`, `docs`, `chore`, `refactor`, `test`, `ci`, `perf`.
  Example: `feat(queue): acquire job leases with FOR UPDATE SKIP LOCKED`.
- Work happens on branches merged into `main` via pull request; CI must be
  green (build, tests, lint, OpenAPI validation, generated-code check).
- A structural decision requires an ADR and an entry
  in the parity matrix (PRD §26). Accepted ADRs are immutable: revisions go
  through a new ADR that supersedes the old one.

## Tests (ADR-026, ADR-028)

The pyramid (`docs/specs/e2e-test-plan.md` §2): the bulk of the coverage is
**unit tests**, and E2E is kept to the minimum the assembled product alone
can prove. That is what keeps the development loop fast.

- Unit tests with the standard `testing` package, table-driven where natural.
  New logic MUST ship with unit tests — parsing, validation (the `4xx`
  guards), config generation, state machines, retention/backoff math all
  belong here, never in an E2E scenario.
- There is exactly **one** product E2E journey (`make e2e`). It proves the
  assembled SSH → Docker → Traefik path, including a real zero-downtime
  switch. Do not add a shard or a second journey. If the full stack is truly
  required, enrich or replace one assertion in that journey.
- Cadence: pull requests run unit/module tests only. The E2E journey runs
  after merge to `main`, on demand, and before a release. There is no nightly
  E2E catalogue.
- A feature is not "complete" without documentation, migrations, metrics,
  audit events, authorization tests, and a recovery scenario (PRD §26.1).

## Toolchain

Build tools are pinned in `go.mod` (`tool` directive) and invoked through
`go tool` — no global installs needed apart from `golangci-lint`:

| Task | Command |
|---|---|
| Build the binary | `make build` |
| Run tests | `make test` |
| Lint | `make lint` |
| Regenerate API + store code | `make generate` |
| Validate the OpenAPI spec | `make openapi-validate` |
| Migration status | `AKERDOCK_DATABASE_URL=... make migrate-status` |
| Complete E2E journey (needs Docker) | `make e2e` |

The E2E harness (`scripts/e2e.sh` — ADR-026/028) boots PostgreSQL and a
Docker-in-Docker target server, then drives one complete public-API journey:
server validation with Traefik bootstrap, a docker_image deployment with an
encrypted environment variable, HTTPS routing, logs (JSON + SSE), the
zero-downtime rolling switch, authentication, and safe deletion.
