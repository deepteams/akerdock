GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test lint generate api-gen sqlc-gen openapi-validate migrate-status e2e e2e-smoke clean web

all: generate build test

build:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o bin/akerdock ./cmd/akerdock

test:
	$(GO) test ./...

lint:
	golangci-lint run

## Code generation (ADR-025: server handlers and DB layer are generated,
## never edited by hand). Run after changing the OpenAPI spec, a migration
## or a query file, and commit the result.
generate: api-gen sqlc-gen ts-gen

api-gen:
	$(GO) tool oapi-codegen -config oapi-codegen.yaml docs/specs/openapi-v1.yaml

# The TypeScript client comes from the same contract as the Go server (ADR-025).
# Skipped when node is absent: a Go-only checkout must still build.
# Builds the dashboard into the Go embed directory. Requires node.
web:
	npm --prefix web ci --silent || npm --prefix web install --silent
	npm --prefix web run build
	rm -rf internal/web/dist && mkdir -p internal/web/dist
	cp -r web/dist/akerdock-web/browser/. internal/web/dist/

ts-gen:
	@if command -v npm >/dev/null 2>&1; then \
		npm --prefix web ci --silent >/dev/null 2>&1 || npm --prefix web install --silent >/dev/null 2>&1; \
		npm --prefix web run generate --silent; \
	else \
		echo "npm not found — skipping the TypeScript client"; \
	fi

# Skipped while db/queries is empty: sqlc fails on a query-less project.
sqlc-gen:
	@if ls db/queries/*.sql >/dev/null 2>&1; then \
		$(GO) tool sqlc generate; \
	else \
		echo "sqlc: no queries in db/queries yet, skipping"; \
	fi

openapi-validate:
	$(GO) run github.com/getkin/kin-openapi/cmd/validate@latest docs/specs/openapi-v1.yaml

# Requires AKERDOCK_DATABASE_URL to point at a PostgreSQL instance.
migrate-status:
	$(GO) tool goose -dir db/migrations postgres "$$AKERDOCK_DATABASE_URL" status

# Full E2E catalogue against real containers (ADR-026). Needs Docker.
# Every shard, in parallel — the NIGHTLY suite. `bash scripts/e2e.sh <shard>`
# runs one on its own, which is how you debug a failure.
e2e:
	bash scripts/e2e.sh

# The minimal per-commit E2E gate (e2e-test-plan §2): one stack, the vertical
# slice only — onboarding, deploy, HTTPS routing, zero-downtime switch, safe
# deletion. Everything provable in Go belongs in `make test`, not here.
e2e-smoke:
	bash scripts/e2e.sh smoke

# Smoke test of the shipped artefact: distroless image + reference compose.
dist-smoke:
	bash scripts/dist-smoke.sh

clean:
	rm -rf bin
