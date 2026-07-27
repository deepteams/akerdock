# AkerDock

Self-hosted PaaS in Go. Deploy applications, databases and Docker Compose stacks
to **your own servers** over SSH, with a managed reverse proxy, automatic HTTPS,
PR previews, backups and monitoring — no vendor lock-in.

A single static Go binary. **PostgreSQL is the only dependency** — it holds both
the state and the job queue (no Redis, no external bus). The API is spec-first
(OpenAPI), and the control plane never runs your workloads: it drives Docker on
your servers over SSH.

> Design docs (PRD, ADRs, specs), code, the CLI and this README are all in
> English.

## What it does

- **Deploy** apps from a Dockerfile, a git repo (Nixpacks / Dockerfile / static)
  or a Docker image; **databases** (Postgres, MySQL, Redis…) and **Docker
  Compose** stacks — with zero-downtime rolling switches when a health check is
  configured.
- **Reverse proxy + automatic HTTPS** (Traefik + Let's Encrypt, HTTP-01 or
  DNS-01 wildcard), per server.
- **PR previews**: every pull request gets its own isolated instance and URL,
  torn down on merge/close.
- **Backups** of databases and volumes with local + S3 retention and restore
  drills.
- **Auth**: password, passkeys (WebAuthn), OIDC SSO (Google, Entra…), enforced
  MFA, and SCIM 2.0 provisioning; granular team RBAC.
- **Adopt** containers and compose stacks already running on a server, without
  restarting them (migrate in place).
- **Local CLI** for day-to-day debugging: logs, shell, TCP port-forward and typed
  DB consoles — see [Using the CLI](#using-the-cli).

## Run your own instance

Requirements: Docker Engine ≥ 24 with Compose v2, and `openssl`.

```sh
git clone https://github.com/deepteams/akerdock.git
cd akerdock
./install.sh
```

`install.sh` builds the image from the local Dockerfile (no published image
needed), generates the master key (`keys/master.key` — **back it up off the
machine immediately**) and the `.env`, starts the reference two-service stack
(AkerDock + PostgreSQL), and prints the first root user's credentials. Customise
the first run with `AKERDOCK_PORT`, `AKERDOCK_INSTANCE_FQDN`,
`AKERDOCK_ROOT_EMAIL`, etc. (see the script header).

Update an existing instance: `git pull && ./install.sh` — the image is rebuilt,
migrations apply at boot, and state persists in the named volumes. The manual
install is documented in [docs/runbooks/install.md](docs/runbooks/install.md).

### Migrating from another platform

AkerDock **adopts** containers and compose stacks already deployed, without
restarting them (PRD §20.7): scan the server, preview the mapping, adopt, then
normalise on the first redeploy — volumes and domains kept, de-adoption possible
at any time. For a Coolify server,
[`scripts/migrate/coolify.sh`](scripts/migrate/coolify.sh) drives the whole
migration over the public API (dry-run by default).

## Using the CLI

`akerdock` is the same binary as the server (Cobra subcommands). It talks **only
to your instance over HTTPS**, opens **no local port**, and works from anywhere —
behind a proxy, over SSH, in a container. This is what team members use to debug
a resource without a manual SSH tunnel.

### Get the CLI

```sh
# straight from the repo — installs `akerdock` into $GOBIN (usually ~/go/bin):
go install github.com/deepteams/akerdock/cmd/akerdock@latest

# or build from a checkout, or grab a release binary:
go build -o akerdock ./cmd/akerdock && sudo mv akerdock /usr/local/bin/
```

Make sure `$GOBIN` (or `$(go env GOPATH)/bin`) is on your `PATH`.

### Log in

```sh
akerdock login --url https://manager.example.com
```

This opens your browser to authorise (SSO / password / passkey), then stores a
named, revocable token under `~/.akerdock/` (config `0700`, tokens `0600`). No
port is opened; the browser flow uses a confirmation code you match on screen.
CI or headless? Paste an existing API token instead:

```sh
akerdock login --url https://manager.example.com --with-token < token.txt
```

### Everyday commands

A resource is addressed by a `REF` of the form `type/name`:
`app/…`, `db/…`, `svc/…`, `preview/…`.

```sh
akerdock ls                              # apps, databases and services in the team
akerdock logs app/varuna -f              # follow container logs
akerdock logs app/varuna --deployment    # logs of the latest build/deploy
akerdock shell app/varuna                # interactive shell in the container
akerdock shell app/varuna -c postgres    # a specific compose service

# Tunnel a container port to localhost through the manager (never exposes it):
akerdock port-forward db/pg 15432:5432
akerdock port-forward app/varuna 15432:5432 -c postgres --pr 8   # a PR preview

# Typed console: opens a forward + the right client (psql/redis-cli/…):
akerdock db db/pg
```

Output is human tables by default; add `-o json` for scripting, `--quiet` for
bare output.

### Contexts (multiple instances)

Each `login` creates a **context** (an instance + active team). Switch between
them without re-typing the URL:

```sh
akerdock context list
akerdock context use staging
akerdock logout --context staging --revoke   # also revoke the server-side token
```

### Per-directory defaults (`.akerdock`)

Drop a committable `.akerdock` file in a repo to set defaults for that directory
tree — no more repeating `--context`, `--team` or the target on every command
(found by walking up, like `.git`; it never holds secrets):

```yaml
# .akerdock
context: prod
application: varuna
component: web
```

Then, from that repo:

```sh
akerdock logs -f          # follows the default app on the configured instance
akerdock shell            # shell into it
```

Resolution precedence (most specific wins):
`flags > AKERDOCK_* env vars > .akerdock > ~/.akerdock (global)`.

## Documentation

| Directory | Contents |
|---|---|
| [`docs/PRD.md`](docs/PRD.md) | Product spec: functional scope and verifiable requirements |
| [`docs/adr/`](docs/adr/README.md) | Architecture Decision Records (accepted) |
| [`docs/specs/`](docs/specs/) | Technical specs: [OpenAPI v1](docs/specs/openapi-v1.yaml), [CLI](docs/specs/cli.md), ERD, threat model, RBAC matrix, proxy contract, deployment engine… |
| [`docs/runbooks/`](docs/runbooks/README.md) | Operational runbooks (install, failures, key rotation, upgrades…) |

## Key architecture decisions

- **Transport**: SSH first, outbound agent on the target ([ADR-001](docs/adr/ADR-001-transport-ssh-then-agent.md))
- **Durable queue in PostgreSQL**, no external bus ([ADR-002](docs/adr/ADR-002-postgresql-queue.md))
- **Standalone Docker runtime** — Kubernetes and Swarm ruled out ([ADR-004](docs/adr/ADR-004-standalone-docker-runtime.md))
- **Go core**: pgx + sqlc, chi + oapi-codegen, spec-first ([ADR-025](docs/adr/ADR-025-go-stack-pgx-sqlc-chi-oapi-codegen.md))
- **Distribution**: minimal two-service compose (AkerDock + PostgreSQL) ([ADR-021](docs/adr/ADR-021-compose-distribution-two-services.md))
- **Real-time**: SSE, WebSocket reserved for the terminal and tunnels ([ADR-024](docs/adr/ADR-024-realtime-sse-websocket-terminal.md))
- **Single-binary CLI** (Cobra), client and server modes ([ADR-033](docs/adr/ADR-033-cli-cobra-migration-run-modes.md))

## Development

Requirements: Go ≥ 1.26 and [golangci-lint](https://golangci-lint.run) v2 (the
other tools — sqlc, oapi-codegen, goose — are pinned in `go.mod` and run via
`go tool`).

```sh
make generate   # regenerate code from the OpenAPI spec and sqlc queries
make build      # compile bin/akerdock
make test lint  # tests and lint
```

Conventions (commits, spec-first workflow, migrations) are in
[CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache 2.0](LICENSE) ([ADR-020](docs/adr/ADR-020-apache-2-0-license.md)).
