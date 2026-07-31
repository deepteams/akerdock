GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# IMAGE is this release's own container image, baked into the binary so the
# scale-to-zero waker (ADR-036) is deployed from the exact same image without
# any runtime configuration. The release pipeline sets it; local builds leave it
# empty (scale-to-zero then needs AKERDOCK_IMAGE, or stays inert).
IMAGE   ?=

.PHONY: all build test test-docker unit-coverage go-unit-coverage web-unit-coverage lint generate api-gen sqlc-gen openapi-validate migrate-status e2e clean web

all: generate build test

build:
	$(GO) build -ldflags "-X main.version=$(VERSION) -X main.image=$(IMAGE)" -o bin/akerdock ./cmd/akerdock

test:
	$(GO) test ./...

# Integration tier of the Docker runtime adapter (ADR-051): runs the SDK
# implementation against the local /var/run/docker.sock. Scoped to the
# dockerruntime package on purpose — jobs and handlers stay on the fake.
test-docker:
	$(GO) test -tags dockerintegration ./internal/dockerruntime/... ./internal/hostops/...

# Fast coverage gates used on pull requests. Angular enforces 90% on
# statements, branches, functions and lines; Go enforces 90% per unit package
# and keeps explicit anti-regression floors for the two orchestration
# boundaries while they are split into smaller policy modules.
unit-coverage: go-unit-coverage web-unit-coverage

go-unit-coverage:
	bash scripts/check-go-unit-coverage.sh

web-unit-coverage:
	npm --prefix web test -- --no-progress

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

# The single complete assembled-product journey (ADR-028). Needs Docker.
# Pull requests use `make test`; this slower proof runs after merge and before
# release because real SSH, Traefik and a zero-downtime switch cannot be mocked.
e2e:
	bash scripts/e2e.sh

# Smoke test of the shipped artefact: distroless image + reference compose.
dist-smoke:
	bash scripts/dist-smoke.sh

clean:
	rm -rf bin
