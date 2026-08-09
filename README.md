# AkerDock

Self-hosted PaaS in Go. Deploy applications, databases and Docker Compose stacks
to **your own servers** over SSH, with a managed reverse proxy, automatic HTTPS,
PR previews, backups and monitoring — no vendor lock-in.

A single static Go binary. **PostgreSQL is the only dependency** — it holds both
the state and the job queue (no Redis, no external bus). The API is spec-first
(OpenAPI), and the control plane never runs your workloads: Docker operations
ride an outbound channel opened by a small agent on each server
([ADR-051](docs/adr/ADR-051-docker-runtime-adapter.md)/[052](docs/adr/ADR-052-agent-command-channel.md)), with SSH kept
for bootstrap, repair, git clones and Nixpacks builds only.

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
  MFA, and SCIM 2.0 provisioning; granular team RBAC, with an invitee able to
  create their account straight from the invitation link.
- **Teams as the isolation boundary**: a user can belong to several teams with a
  different role in each, and switches between them from the sidebar; everything
  else (servers, resources, tokens, notifications) is scoped per team.
- **Adopt** containers and compose stacks already running on a server, without
  restarting them (migrate in place).
- **Scale to zero**: idle apps stop and wake on the first request
  ([ADR-036](docs/adr/ADR-036-scale-to-zero-waker.md)).
- **Bastion**: declared external endpoints and audited TCP tunnels to them
  ([ADR-045](docs/adr/ADR-045-external-endpoint-port-forwards.md)).
- **Local CLI** for day-to-day debugging: logs, shell, TCP port-forward and typed
  DB consoles — see [Using the CLI](#using-the-cli).
- **MCP server** (read-only, opt-in) so an assistant can inspect the instance —
  `akerdock mcp` ([ADR-043](docs/adr/ADR-043-mcp-server-oauth-and-cli.md)).

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
`AKERDOCK_ROOT_EMAIL`, etc. (see the script header). Two variables matter for
server onboarding since [ADR-051](docs/adr/ADR-051-docker-runtime-adapter.md):
`AKERDOCK_IMAGE` (the instance's own image, from which the per-server agent
helper is deployed — the compose derives it from `AKERDOCK_TAG`) and
`AKERDOCK_INSTANCE_URL` (the base URL agents dial back; derived from the
instance FQDN or host gateway when unset).

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
named, revocable token under `~/.akerdock/` (directory `0700`, files `0600`; the
token lives in `credentials.yaml`, apart from the inspectable `config.yaml`). **No
port is opened** — the browser flow is a poll bound by PKCE, with a confirmation
code you match on screen. The token defaults to `read,write` — never `root`,
`deploy` or `read:sensitive` unless you ask for them with `--scopes`, and never
more than the approving session holds — and expires after 30 days.

CI or headless? Paste an existing API token instead, or print the URL rather than
opening a browser:

```sh
akerdock login --url https://manager.example.com --with-token < token.txt
akerdock login --url https://manager.example.com --no-browser
```

### Everyday commands

A resource is addressed by a `REF` of the form `type/name`, where the name is the
resource's name or its UUID: `app/…`, `db/…`, `svc/…`, `preview/…`, and
`endpoint/…` for a declared external target.

```sh
akerdock ls                              # apps, databases and services in the team
akerdock ls servers                      # or one kind: apps|databases|services|servers
akerdock logs app/varuna -f              # follow container logs
akerdock logs app/varuna -n 500          # snapshot, last N lines (default 200)
akerdock logs app/varuna --deployment    # logs of the latest build/deploy
akerdock shell app/varuna                # interactive shell in the container
akerdock shell app/varuna -c postgres    # a specific compose service

# Tunnel a container port to localhost through the manager (never exposes it):
akerdock port-forward db/pg 15432:5432
akerdock port-forward app/varuna 15432:5432 -c postgres --pr 8   # a PR preview

# A declared external endpoint — a managed DB, an internal API — is its own
# command (ADR-045/070). No remote port to give: the endpoint froze its own host
# and port. Without a local port either, the OS picks a free one and prints it.
akerdock tunnel open prod-replica
akerdock tunnel open prod-replica 15432   # …on a chosen local port
akerdock tunnel ls                        # every tunnel open in the team
akerdock tunnel close <session-uuid>

# Typed console: opens a forward + the right client
# (psql / mysql / redis-cli / mongosh, picked from the engine):
akerdock db db/pg
```

Output is human tables by default; add `-o json` for scripting, `--quiet` for
bare output. Exit codes: `0` success, `1` error, `2` usage.

### Give a local assistant read-only access (MCP)

`akerdock mcp` bridges the instance's MCP server over stdio, using the current
context's credentials. The tools are read-only, and the server-side surface is
**off by default** — enable it in the instance settings first
([ADR-043](docs/adr/ADR-043-mcp-server-oauth-and-cli.md)).

### Contexts (multiple instances, multiple teams)

Each `login` creates a **context**: one instance, and the team its token belongs
to. Switch between them without re-typing the URL:

```sh
akerdock context list
akerdock context use staging
akerdock context current
akerdock logout --context staging --revoke   # also revoke the server-side token
```

An API token is **bound to one team** when it is created, so a context acts in
that team and nothing else — `--team` does not move it (it only tells
`logout --revoke` where to look for the token to delete). To work in another
team, log in again into a separate context with a token of that team. The
dashboard's team switcher moves a *session*; the CLI holds a token, and tokens
do not move.

### Per-directory defaults (`.akerdock`)


Drop a committable `.akerdock` file in a repo to set defaults for that directory
tree — no more repeating `--context` or the target on every command (found by
walking up, like `.git`; it never holds secrets):

```yaml
# .akerdock — every field optional
context: prod          # a context created by `akerdock login`
application: varuna    # default target for logs / shell
component: web         # default compose service
project: platform
environment: production
```

Then, from that repo:

```sh
akerdock logs -f          # follows the default app on the configured instance
akerdock shell            # shell into it
```

Resolution precedence (most specific wins):
`flags > AKERDOCK_* env vars > .akerdock > ~/.akerdock (global)` — with
`AKERDOCK_CONTEXT`, `AKERDOCK_APPLICATION`, `AKERDOCK_COMPONENT`,
`AKERDOCK_PROJECT`, `AKERDOCK_ENVIRONMENT`, `AKERDOCK_TEAM`.

### Server modes

The same binary runs the control plane, which is why `akerdock --help` lists more
than the client commands:

```sh
akerdock serve            # all-in-one (default, or $AKERDOCK_MODE)
akerdock serve api        # HTTP API only …  worker | scheduler for the others
akerdock healthcheck      # probe used by the compose healthcheck
akerdock agent            # server agent helper container (ADR-051/056; `waker` is a deprecated alias)
akerdock version
```

The full contract is in [docs/specs/cli.md](docs/specs/cli.md).

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
