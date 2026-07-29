# Specification — AkerDock instance configuration

> Configuration contract of the AkerDock control plane. Upstream sources of truth: PRD (`docs/PRD.md`) §10.2, §14.1–14.3, §18.2, §22.1, §23.2, §27.1, §27.3, §27.21; ADR-003 (envelope encryption, versioned master key); ADR-008 (OTLP everywhere); ADR-021 (2-service compose distribution); data dictionary §2.7 (`*_enc` format) and §11.7 (`instance_settings`). This specification **makes normative** the defaults posed as “(proposed default)” by the runbooks [install.md](../runbooks/install.md), [key-rotation.md](../runbooks/key-rotation.md), [upgrade-downgrade.md](../runbooks/upgrade-downgrade.md) and [control-plane-restore.md](../runbooks/control-plane-restore.md); the runbooks now reference the corresponding sections.
>
> Scope: configuration of the **AkerDock process** (the instance/control plane) and of its compose distribution. The directory tree of the **target servers** is out of scope — it is defined by the deployment-engine spec §5.1. Persisted settings modifiable at runtime (FQDN, transactional email, API on/off…) belong to the `instance_settings` table (data dictionary §11.7), not to this document — except their bootstrapping at first startup (§6).
>
> Assumed deviations from the runbook excerpts (justified in a note): technical identifiers in lowercase (`akerdock` as the compose service name, PostgreSQL user and database — §4, note N1); named volumes rather than bind mounts for the services' state (§4, note N2).

---

## 1. Overview

### 1.1 Configuration sources and precedence

Three sources, in decreasing order of precedence:

1. **Environment variables** `AKERDOCK_*` (plus the standard `OTEL_*` variables, §2.4) — recommended source, the one the compose distribution (§4) uses;
2. **Optional configuration file** (YAML), path given by `AKERDOCK_CONFIG_FILE` — keys in `snake_case` without the prefix (e.g. `port: 8080`, `log_level: debug`). No file is looked for by default: without `AKERDOCK_CONFIG_FILE`, this source does not exist;
3. **Compiled-in defaults** in the binary (the “Default” column of §2).

The configuration is read **once at startup**: any change requires a process restart. Settings modifiable at runtime live in `instance_settings` and in the UI/API — never in both at once: when a value exists on the database side, the environment only serves to bootstrap the first startup (§6.2).

### 1.2 Principle: zero mandatory configuration besides the DB and the master key

An instance starts with exactly **two elements provided by the operator**:

- `AKERDOCK_DATABASE_URL` — PostgreSQL access (ADR-002, ADR-021);
- `AKERDOCK_MASTER_KEY_FILE` (or, discouraged, `AKERDOCK_MASTER_KEY`) — the encryption master key (ADR-003).

Everything else has a safe, documented default. Corollaries: no default may be silently “guessed” (no automatic master key generation, no embedded database); and the absence of either of the two elements is a **fatal startup error** with an explicit message (§6.4, §7) — never a degraded mode.

---

## 2. Normative environment variables

### 2.1 Variables read by the binary

| Name | Required | Default | Description | Sensitive |
|---|---|---|---|---|
| `AKERDOCK_DATABASE_URL` | **yes** | — | PostgreSQL DSN (`postgres://user:pass@host:5432/db?sslmode=…`). PostgreSQL ≥ 15 (data dictionary §2). | **yes** (contains the password) |
| `AKERDOCK_MASTER_KEY_FILE` | **yes**¹ | — | Path of the multi-version master key file (§3). In the compose distribution: `/run/secrets/master.key`. | no (the path; its content is) |
| `AKERDOCK_MASTER_KEY` | no¹ (**discouraged**) | — | Alternative: content of the key file directly in a variable (same format as §3, lines separated by `\n`). Emits a warning at startup (§7.2): a process's environment is more exposed than a `0600` file (`/proc` inspection, `docker inspect`, orchestrator logs). | **yes** |
| `AKERDOCK_MODE` | no | `all-in-one` | Mode of the modular monolith (PRD §18.2): `all-in-one` \| `api` \| `worker` \| `scheduler`. The first command-line argument (e.g. `command: ["all-in-one"]`) takes precedence over the variable. Several `api`/`worker` can coexist (§22.1); several `scheduler` are safe (election via PostgreSQL advisory lock). | no |
| `AKERDOCK_PORT` | no | `8080` | **Single port** of the control plane (PRD §27.1, ADR-021): UI, API, SSE, terminal WebSocket and `/api/v1/health` — nothing else listens. In the compose distribution, it is also the published port (§4). | no |
| `AKERDOCK_INSTANCE_FQDN` | no | — | FQDN of the instance (PRD §14.2). Only serves to bootstrap `instance_settings.fqdn` at first startup (§6.2); afterwards the value in the database is authoritative (modifiable in the UI). | no |
| `AKERDOCK_INSTANCE_PORT` | no | = `AKERDOCK_PORT` | Port at which the instance is reachable **on its host** — the target of the `00-control-plane` proxy route (proxy-contract §5.7). Differs from `AKERDOCK_PORT` under the compose distribution: the mapping publishes `${AKERDOCK_PORT}:8080` and the process always listens on 8080 inside the container, so the compose forwards the published port through this variable (§4). A binary launched directly on the host does not need to set it. | no |
| `AKERDOCK_ROOT_EMAIL` | no² | — | Non-interactive bootstrap of the first root user (PRD §10.2). Strictly validated email. | no |
| `AKERDOCK_ROOT_NAME` | no² | — | Name of the root user. Non-empty after trim, ≤ 255 characters. | no |
| `AKERDOCK_ROOT_PASSWORD` | no² | — | Password of the root user, strict validation (≥ 12 characters — PRD §10.2); hashed with Argon2id, never logged. To be removed from the environment after the first startup (§6.3, §7.2). | **yes** |
| `AKERDOCK_TIMEZONE` | no | `UTC` | IANA timezone (e.g. `Europe/Paris`). Bootstraps `instance_settings.timezone` at first startup (default aligned with data dictionary §11.7); afterwards the value in the database is authoritative. Stored and logged timestamps remain in UTC. | no |
| `AKERDOCK_LOCALHOST_HOST` | no | `host.docker.internal` | Address through which the process reaches the host machine over SSH for the pre-registered `localhost` server (§6.2). Read at bootstrap only; afterwards the server record in the database is authoritative (modifiable via `PATCH /servers/{uuid}`). In the compose distribution, the name is resolved by `extra_hosts: host-gateway` (§4.1). | no |
| `AKERDOCK_GITHUB_CA_FILE` | no | — | Additional CA certificate(s) (PEM) to reach a GitHub Enterprise Server with a private CA (git-webhook-protocols §2.6). Added to the system roots for calls to the GitHub API only. | no |
| `AKERDOCK_LOCALHOST_USER` | no | `root` | SSH user of the pre-registered `localhost` server (§6.2). `install.sh` puts there the user who runs the installation. Read at bootstrap only. | no |
| `AKERDOCK_LOG_LEVEL` | no | `info` | `debug` \| `info` \| `warn` \| `error`. | no |
| `AKERDOCK_LOG_FORMAT` | no | `json` | `json` (production, one line per event) \| `text` (readable, development). | no |
| `AKERDOCK_DATA_DIR` | no | `/var/lib/akerdock` | Data directory of the process (§5.2). In the compose distribution: named volume `akerdock_data`. Created at startup if it does not exist; not writable = fatal error. | no |
| `AKERDOCK_WORKER_CONCURRENCY` | no | `10` | Maximum number of jobs executed in parallel **per process** in `worker` or `all-in-one` mode (integer ≥ 1). Default calibrated on the minimal 2 vCPU / 2 GB sizing (PRD §14.1); the per-server and per-team caps (PRD §22.2) apply on top, on the queue side. | no |
| `AKERDOCK_SHUTDOWN_TIMEOUT` | no | `30s` | Drain delay on graceful shutdown (§6.5): Go duration (`30s`, `2m`). MUST remain below the compose `stop_grace_period` (40 s, §4) and the jobs' lease expiration (90 s, deployment-engine §2.5). | no |
| `AKERDOCK_TERMINAL_IDLE_TIMEOUT` | no | `15m` | Inactivity (no keystroke) beyond which a web terminal session is closed (PRD §24.4, ADR-024): Go duration. Terminal output does not count as activity — a spinner does not keep a forgotten root shell alive. | no |
| `AKERDOCK_TERMINAL_MAX_DURATION` | no | `4h` | Maximum duration of a web terminal session, regardless of activity (PRD §24.4): Go duration. Closure is guaranteed (kill of the remote PTY) and logged with its reason. | no |
| `AKERDOCK_TRUSTED_PROXIES` | no | — | Peers whose `X-Forwarded-For` / `X-Real-IP` may be believed: comma-separated IPs and CIDRs (`172.18.0.1`, `10.0.0.0/8`), or the shorthand `private` (RFC 1918 + loopback + link-local + ULA). **Required as soon as a reverse proxy fronts the instance**, otherwise every caller address recorded is the proxy's: the audit trail attributes everything to one address, the `/auth` rate limiter gives the whole internet one shared bucket, a token's CIDR allowlist admits everyone the proxy admits, and tunnel/terminal sessions record the wrong client. Empty (the default) reads no such header at all — the only safe posture for a process exposed directly, since the header is written by whoever speaks last. Unparsable entry = fatal error. | no |
| `AKERDOCK_CONFIG_FILE` | no | — | Path of an optional YAML configuration file (§1.1). Unreadable or invalid file = fatal error. | no |

¹ Exactly one of the two master key sources must be provided. Both at once = fatal error (ambiguous); neither = fatal error (§6.4).
² The three `AKERDOCK_ROOT_*` variables form an all-or-nothing trio: providing only one or two = fatal error. They are **read only if no user exists** in the database, and consumed only once (§6.3).

### 2.2 Variables consumed by the compose (not by the binary)

These variables live in `/var/lib/akerdock/.env` and are interpolated by Docker Compose (§4); the binary does not read them:

| Name | Required | Default | Description | Sensitive |
|---|---|---|---|---|
| `AKERDOCK_TAG` | **yes** | — | Explicit image tag (`v1.0.0`) — never `latest` (upgrade-downgrade runbook). The compose refuses to start without it (`:?`). | no |
| `POSTGRES_PASSWORD` | **yes** | — | Password of the internal PostgreSQL (generated at installation: `openssl rand -hex 24`). | **yes** |

`AKERDOCK_PORT` and the `AKERDOCK_ROOT_*`/`AKERDOCK_INSTANCE_FQDN` variables may also appear in `.env`: the compose forwards them to the service (§4).

### 2.3 Naming rules

Prefix **`AKERDOCK_*` only**, with no alias under another brand (decision §27.22, ADR-022). These instance variables are a namespace distinct from the predefined variables injected into workloads (`AKERDOCK_FQDN`, `AKERDOCK_URL`, `AKERDOCK_BRANCH`, `AKERDOCK_PR_ID`… — deployment-engine §5.2): no variable from the §2.1 table is injected into a deployed container.

### 2.4 Telemetry: standard OpenTelemetry variables (ADR-008)

In accordance with “OTLP everywhere, no proprietary protocol”, telemetry export is configured through the **standard** variables of the OpenTelemetry SDK — this is the single assumed exception to the `AKERDOCK_*` prefix (a proprietary variable duplicating `OTEL_EXPORTER_OTLP_ENDPOINT` would recreate the home-grown protocol that ADR-008 rejects):

| Name | Required | Default | Description | Sensitive |
|---|---|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | no | — (export disabled) | OTLP endpoint (gRPC 4317 or HTTP 4318) for metrics, traces and logs of the control plane and the workers. Absent = no export attempt, no repeated warning. | no |
| `OTEL_SERVICE_NAME` | no | `akerdock` | Emitted service name. | no |
| Other `OTEL_*` | no | SDK defaults | All standard variables of the OTel Go SDK are honored as-is (`OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER`…). | depends on the variable (`…_HEADERS` may carry a token: **yes**) |

---

## 3. Multi-version master key file (normative — ADR-003)

### 3.1 Format

UTF-8 text file, **one line per key version**:

```text
<version>:<key in standard base64, 32 decoded bytes>
```

- `<version>`: decimal integer **from 1 to 4294967295** (uint32 — exactly the space of the 4-byte big-endian `key_version` prefix of the `*_enc` columns, data dictionary §2.7), without leading zeros. Each version is **unique** within the file;
- `<key>`: standard base64 (RFC 4648, with padding) decoding to **exactly 32 bytes** (AES-256-GCM, PRD §27.3);
- separator: `:`, without surrounding spaces; line ending `\n`;
- empty lines and lines starting with `#`: ignored (operator comments allowed);
- any other line, a duplicated version, invalid base64 or a key of another length: **invalid** file → refusal to start (§6.4).

The **active version** — the one that encrypts every new write — is the **highest-numbered version** present in the file. No explicit marker: this is the behavior the [key-rotation.md](../runbooks/key-rotation.md) runbook makes operational (adding a line is enough to activate the new key at reload).

### 3.2 Complete example

```text
# /var/lib/akerdock/keys/master.key — 0600 root:root
# v1: installed 2026-07-11; v2: planned rotation 2027-01-15
1:m4C9Zk0vG8kQ2m1H0cVvXHkq3D3jUj0F3q5m8Q2xX9s=
2:Zk3q8W1mB7hT4nJ6cR9vY0dL2aP5sG8uK1oE4wI7xN0=
```

Here the active version is **2**; version 1 stays present to decrypt the data that still references it.

### 3.3 Permissions and location

- Host: `/var/lib/akerdock/keys/master.key`, owner `root:root`, mode **`0600`**, `keys/` directory in `0700` (PRD §23.2);
- Container: mounted **read-only** on `/run/secrets/master.key` (§4);
- At startup, the binary checks the file's permissions: readable or writable by “other” = **fatal error**; any other deviation from `0600` = warning (§7.2).

### 3.4 Version management rules

1. **Adding a version = rotation**: adding a line with a version strictly greater than the existing ones makes it the active version at the next startup/reload (`docker compose up -d akerdock`). Versions do not need to be contiguous;
2. **Never remove a version while encrypted data references it**: each `*_enc` value in the database begins with the `key_version` (4 bytes big-endian) that encrypted it (data dictionary §2.7) — removing a still-referenced version makes those ciphertexts permanently unreadable. The verification procedure (SQL histogram of versions column by column, over the 16 encrypted columns of the data dictionary §12) is in [key-rotation.md](../runbooks/key-rotation.md);
3. **Startup**: if a decryption operation encounters a `key_version` absent from the file, the error is explicit (missing version named) and the operation fails — the instance never masks a missing key (§6.4). Re-encryption towards the active version is lazy, without a blocking rewrite (PRD §19.2, ADR-003);
4. **Backup**: the file (all versions) is copied off-machine, separately from the database dumps (PRD §23.1) — see [install.md](../runbooks/install.md) step 2 and [control-plane-restore.md](../runbooks/control-plane-restore.md) (“the 3 pieces”).

---

## 4. Reference compose distribution (normative — ADR-021)

### 4.1 `docker-compose.yml`

Two services, a single published port, named volumes, healthchecks on both services, pinned tags. Reference file, shipped with each release at the location `/var/lib/akerdock/docker-compose.yml`:

```yaml
name: akerdock

services:
  akerdock:
    image: ghcr.io/deepteams/akerdock:${AKERDOCK_TAG:?tag d'image explicite requis (jamais latest)}
    command: ["all-in-one"]              # modes all-in-one|api|worker|scheduler (PRD §18.2, §2.1)
    restart: unless-stopped
    ports:
      - "${AKERDOCK_PORT:-8080}:8080"    # the single published port of the control plane (§27.1)
    environment:
      AKERDOCK_DATABASE_URL: postgres://akerdock:${POSTGRES_PASSWORD}@postgres:5432/akerdock?sslmode=disable
      AKERDOCK_MASTER_KEY_FILE: /run/secrets/master.key
      AKERDOCK_INSTANCE_FQDN: ${AKERDOCK_INSTANCE_FQDN:-}
      # Bootstrap of the first root user (PRD §10.2) — read only if no user exists (§6.3)
      AKERDOCK_ROOT_EMAIL: ${AKERDOCK_ROOT_EMAIL:-}
      AKERDOCK_ROOT_NAME: ${AKERDOCK_ROOT_NAME:-}
      AKERDOCK_ROOT_PASSWORD: ${AKERDOCK_ROOT_PASSWORD:-}
      # Pre-registered localhost server (§6.2) — read at bootstrap only
      AKERDOCK_LOCALHOST_USER: ${AKERDOCK_LOCALHOST_USER:-}
    volumes:
      - ./keys/master.key:/run/secrets/master.key:ro
      - akerdock_data:/var/lib/akerdock
    networks: [akerdock]
    extra_hosts:
      # Makes host.docker.internal resolve to the compose network gateway
      # on Linux (native on Docker Desktop): it is the SSH address of the
      # pre-registered localhost server (§6.2).
      - "host.docker.internal:host-gateway"
    depends_on:
      postgres:
        condition: service_healthy
    healthcheck:
      # Distroless image without a shell: the binary embeds a `healthcheck` subcommand
      # that queries http://127.0.0.1:8080/api/v1/health and exits with 0/1 (§6.6).
      test: ["CMD", "/akerdock", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 30s
    stop_grace_period: 40s               # > AKERDOCK_SHUTDOWN_TIMEOUT (30 s, §6.5)

  postgres:
    image: postgres:18                   # exact tag pinned by the release notes (≥ 15, data dictionary §2)
    restart: unless-stopped
    environment:
      POSTGRES_USER: akerdock
      POSTGRES_DB: akerdock
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?requis (openssl rand -hex 24)}
      PGDATA: /var/lib/postgresql/data   # stable path: PG18 moved the default PGDATA to a versioned subfolder — pinning it keeps the data in the mounted volume (ADR-039)
    volumes:
      - akerdock_pgdata:/var/lib/postgresql/data
      - ./backups:/backups               # local dumps visible on the host (§5.1)
    networks: [akerdock]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U akerdock -d akerdock"]
      interval: 5s
      timeout: 3s
      retries: 10

networks:
  akerdock:
    name: akerdock

volumes:
  akerdock_data:
  akerdock_pgdata:
```

Guaranteed properties (ADR-021): a single `docker compose up -d` installs; an upgrade is a change of `AKERDOCK_TAG` ([upgrade-downgrade.md](../runbooks/upgrade-downgrade.md)); PostgreSQL is **not** published on the host; `sslmode=disable` is acceptable only because the traffic stays on the private `akerdock` compose network — any external database requires `sslmode=verify-full` (§7.2 warning otherwise).

> **Note N1 — lowercase identifiers.** The compose service name, the PostgreSQL user and database are `akerdock` (lowercase). The excerpts of the [install.md](../runbooks/install.md) runbook written before this spec use the `AkerDock` casing: the present spec is authoritative — technical identifiers are lowercase (a mixed-case PostgreSQL role created via `POSTGRES_USER` would require quoted identifiers in every `psql`/`pg_dump` command, a source of operator errors).
>
> **Note N2 — named volumes (justified deviation from install.md).** The installation runbook proposed bind mounts (`./postgres`, implicitly the application state on the host). This spec retains the **named volumes** `akerdock_pgdata` and `akerdock_data`: UID/permission management by Docker (the distroless image runs non-root, the postgres image with its own UID — bind mounts require manual chowns), clear backup semantics (the restorable state goes through `pg_dump`/`pg_restore`, never through a file copy — ADR-021 “standard PostgreSQL backups”), and a volume movable independently of the compose directory. The elements the operator must touch or exfiltrate remain host files under `/var/lib/akerdock/` (compose, `.env`, `keys/master.key`, `backups/` — §5.1): the “3 pieces” procedure of [control-plane-restore.md](../runbooks/control-plane-restore.md) is unchanged, no named volume needs to be backed up (§5.3).

### 4.2 `docker-compose.override.yml` — persistent customizations

Local customizations go into `/var/lib/akerdock/docker-compose.override.yml`, loaded automatically by Compose v2 and **never touched by upgrades** (parity with the reference's `docker-compose.custom.yml`, PRD §14.1): releases only replace `docker-compose.yml`. Documented example:

```yaml
# /var/lib/akerdock/docker-compose.override.yml — local customizations, survive upgrades
services:
  akerdock:
    environment:
      AKERDOCK_LOG_LEVEL: debug
      AKERDOCK_WORKER_CONCURRENCY: "20"
      OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector.interne:4317
      # A reverse proxy in front of the published port (the `00-control-plane`
      # route, or an nginx of your own): without this, every recorded caller
      # address is the proxy's — audit trail, rate limiter, token CIDR
      # allowlist alike (§2.1). Name the proxy as precisely as you can; the
      # `private` shorthand is right only if nothing untrusted can reach the
      # published port from a private address.
      AKERDOCK_TRUSTED_PROXIES: 172.18.0.1
    deploy:
      resources:
        limits:
          memory: 2g

networks:
  akerdock:
    ipam:
      config:
        - subnet: 172.30.0.0/24        # custom Docker CIDR range (PRD §14.1)
```

Rules: the override MUST **neither** change the images/tags (upgrades go through `AKERDOCK_TAG`), **nor** publish an additional port (§27.1), **nor** remove the healthchecks. An override that breaks these invariants is out of support.

---

## 5. `/var/lib/akerdock/` tree of the instance

Distinct from the `/var/lib/akerdock/` tree **of the target servers** (applications, proxy, sources — deployment-engine §5.1, normative over there): same root name (PRD §7.5 parity, “all the state = one directory”), different contents. When the instance also manages its own host as a target server (`localhost`), the two trees coexist under the same root without subdirectory collision.

### 5.1 On the instance host

```text
/var/lib/akerdock/                          # root, 0750, root
├── docker-compose.yml                   # reference file of the release (§4.1), replaced at upgrade
├── docker-compose.override.yml          # optional — persistent customizations (§4.2)
├── .env                                 # 0600 — AKERDOCK_TAG, POSTGRES_PASSWORD, AKERDOCK_PORT, AKERDOCK_ROOT_*…
├── keys/                                # 0700
│   └── master.key                       # 0600 root:root — multi-version master key (§3)
└── backups/                             # local dumps (pre-upgrade, “Backup Now”) — mounted into postgres (/backups)
```

This is exactly the perimeter to exfiltrate off-machine: `docker-compose.yml` + override + `.env` + `keys/master.key` (+ the dumps, whose reference copy lives on S3 via the `is_instance_backup` plan) — the “3 pieces” of [control-plane-restore.md](../runbooks/control-plane-restore.md).

### 5.2 In the `akerdock_data` volume (mounted on `AKERDOCK_DATA_DIR`, default `/var/lib/akerdock` in the container)

```text
/var/lib/akerdock/                          # in the container = named volume akerdock_data
├── ssh/
│   └── instance_ed25519.pub             # copy of the instance's public key (§6.2) — the private one is in the database, encrypted
└── tmp/                                 # temporary space of the process (downloads, staging), purged at startup
```

### 5.3 Invariant: nothing irrecoverable outside database + master key

The `akerdock_data` volume contains **no irrecoverable state**: everything in it can be regenerated from PostgreSQL + `master.key` (the instance's private SSH key is stored encrypted in `private_keys`, not in the volume). Consequence: neither `akerdock_data` nor `akerdock_pgdata` (covered by `pg_dump`) enters the backup plan — the restore contract remains “dump + `master.key` + compose/`.env`” (PRD §22.1).

---

## 6. Lifecycle

### 6.1 Startup sequence (all startups)

Normative order, each step blocking:

1. **Loading and validation of the configuration** (§7) — any error is fatal, listed exhaustively;
2. **PostgreSQL connection** — retry with backoff for at most 30 s (covers the `depends_on: service_healthy` window), then fatal error;
3. **Versioned, automatic SQL migrations** (ADR-025) — applied at boot by the binary, before any network listening; designed to be rolling-upgrade compatible (PRD §18.2) for the multi-instance modes. Explicit log `migrations applied` (marker used by [upgrade-downgrade.md](../runbooks/upgrade-downgrade.md)). A failed migration = immediate stop, database left in the state of the last complete migration (each migration is transactional);
4. **Loading of the master key** (§3) — strict parsing, permission check, encrypt/decrypt self-test with the active version; any failure = refusal to start (§6.4);
5. **First-startup bootstraps** (§6.2) and **root user bootstrap** (§6.3), if applicable;
6. **Listening** on `0.0.0.0:AKERDOCK_PORT` and startup of the mode's components (`api`: HTTP only; `worker`: queue consumption; `scheduler`: crons under advisory lock; `all-in-one`: all three). In pure `worker`/`scheduler` modes, the port only serves `/api/v1/health`.

### 6.2 First startup (idempotent, replayable)

Detected by the state of the database, not by a marker file — each action is individually idempotent:

- **`instance_settings` singleton** created if it does not exist, bootstrapped with `AKERDOCK_INSTANCE_FQDN` (if present) and `AKERDOCK_TIMEZONE`. At subsequent startups, these variables **never overwrite** the database; a divergence produces a warning (§7.2);
- **Instance SSH key**: generation of an ed25519 pair without a passphrase if no instance key exists — the private key encrypted in the database (`private_keys`, envelope encryption §2.7 of the data dictionary), the public key copied to `AKERDOCK_DATA_DIR/ssh/instance_ed25519.pub` so the operator can deposit it on the `localhost` server or a first target server;
- **Root user bootstrap** (§6.3);
- **Pre-registered `localhost` server** (PRD §3): as soon as a team exists (the root's team — at the same startup if the `AKERDOCK_ROOT_*` trio is provided, otherwise at a later startup), a server record named `localhost` (`is_localhost = true`, status `pending`) is created in the oldest team, pointing at `AKERDOCK_LOCALHOST_HOST`:22 with the user `AKERDOCK_LOCALHOST_USER` and the instance SSH key. Bootstrapped **only once** in the instance's lifetime (`instance_settings.localhost_seeded`): if the operator deletes this server, it is never recreated — bootstrapping does not recreate what the operator has destroyed. As long as this server has **never** been validated, the scheduler retries its validation at each maintenance tick, for 24 h after its creation: as soon as the instance's public key is authorized on the host — which `install.sh` does automatically for the installing user — it becomes `ready` without operator action. Past this delay, or after a first successful validation, its lifecycle becomes entirely that of an ordinary server again (manual validation, PRD §20.1).

### 6.3 Bootstrap of the first root user (PRD §10.2)

- The `AKERDOCK_ROOT_EMAIL` / `AKERDOCK_ROOT_NAME` / `AKERDOCK_ROOT_PASSWORD` variables are **read only if no user exists** in the database; they are **consumed only once** — as soon as a user exists, they are ignored (warning if still present, §7.2);
- **Strict and blocking** validation: syntactically valid and normalized email, non-empty name (§2.1), password ≥ 12 characters; any failure = **fatal error** at startup (never a root created “on the cheap”, never a startup without a root when the trio was provided);
- The password is hashed with Argon2id (PRD §23.2) and appears in no log or audit event; the creation of the root is audited (§23.4);
- Alternative without variables: the UI's guided onboarding creates the root at first access (PRD §14.2);
- After the first successful startup, remove `AKERDOCK_ROOT_PASSWORD` from `.env` ([install.md](../runbooks/install.md) step 4).

### 6.4 Missing or invalid master key: refusal to start

No silent degraded mode; in all the cases below the instance **refuses to start** with an actionable message naming the exact problem:

| Situation | Message (minimal content) |
|---|---|
| Neither `AKERDOCK_MASTER_KEY_FILE` nor `AKERDOCK_MASTER_KEY` | missing variable + pointer to [install.md](../runbooks/install.md) step 2 |
| Both provided | source conflict, remove one |
| File missing / unreadable | exact path + expected permissions |
| Invalid format (§3.1) | offending line number + violated rule (without ever logging the line's content) |
| Permissions too open (readable/writable by “other”) | current mode vs expected `0600` |
| AEAD self-test failure | affected active version |

At runtime, a `key_version` referenced in the database but absent from the file produces an explicit error per operation (§3.4.3) and an OTLP error counter — never an empty value or a “skipped” secret.

### 6.5 Graceful shutdown

On `SIGTERM`/`SIGINT`: stop listening for new requests and leasing new jobs, drain in-flight jobs for at most `AKERDOCK_SHUTDOWN_TIMEOUT` (default 30 s), heartbeats maintained during the drain, then stop. Unfinished jobs are resumed after their lease expires (90 s) through remote inspection, never blindly replayed (PRD §22.1, deployment-engine §2.5). The compose `stop_grace_period: 40s` (§4.1) lets the drain finish before Docker's `SIGKILL`.

### 6.6 Healthcheck

- HTTP endpoint: **`GET /api/v1/health`**, unauthenticated, available even with the API disabled (OpenAPI `/health`), on the single port. `200` = process alive, database reachable, master key loaded;
- **`/akerdock healthcheck`** subcommand: local request to `/api/v1/health`, exit code 0/1 — this is the compose healthcheck (the distroless image having neither shell nor curl, ADR-021).

---

## 7. Startup validation

### 7.1 Fatal errors — exhaustive collection, never a partial startup

All configuration checks are executed **before** opening the port and touching the queue; the errors are **all collected then listed together** (the operator fixes everything in one cycle), and the process exits with code `1`:

```text
FATAL configuration invalide (3 erreurs) :
  - AKERDOCK_DATABASE_URL : absente (requise — DSN PostgreSQL, spec instance-config §2)
  - AKERDOCK_MODE : valeur "workers" invalide (attendu : all-in-one|api|worker|scheduler)
  - AKERDOCK_ROOT_EMAIL : fournie sans AKERDOCK_ROOT_NAME/AKERDOCK_ROOT_PASSWORD (trio tout-ou-rien)
```

Notably fatal: a required variable missing (§1.2); a value outside its domain (`AKERDOCK_MODE`, `AKERDOCK_LOG_LEVEL`, `AKERDOCK_LOG_FORMAT`); `AKERDOCK_PORT` outside `1–65535`; unparsable DSN; unknown IANA timezone; unparsable duration/integer (`AKERDOCK_SHUTDOWN_TIMEOUT`, `AKERDOCK_WORKER_CONCURRENCY < 1`); incomplete or invalid `AKERDOCK_ROOT_*` trio (§6.3); master key conflicts and invalidities (§6.4); `AKERDOCK_CONFIG_FILE` unreadable or invalid YAML; `AKERDOCK_DATA_DIR` not writable. Error messages **never** reproduce the value of a sensitive variable (§2, “Sensitive” column).

### 7.2 Warnings (startup allowed, `warn` log at boot)

- `AKERDOCK_MASTER_KEY` used instead of a file (§2.1);
- `AKERDOCK_ROOT_*` variables still present while users exist — recommend their removal ([install.md](../runbooks/install.md));
- `master.key` permissions different from `0600` without being open to “other” (§3.3);
- `AKERDOCK_INSTANCE_FQDN`/`AKERDOCK_TIMEZONE` diverging from the value in the database after the first startup (the database is authoritative, §6.2);
- `sslmode=disable` in `AKERDOCK_DATABASE_URL` towards a host outside the compose network (§4.1);
- unknown `AKERDOCK_*` environment variable (typo detection: `AKERDOCK_LOGLEVEL` → suggestion `AKERDOCK_LOG_LEVEL`).

Each warning is emitted once at startup, in clear text in the logs — never repeated in a loop, never silent.

---

## Traceability

| Section | Makes normative | Referenced by |
|---|---|---|
| §2 (port 8080, variable names) | PRD §27.1, ADR-022 | install.md (network prerequisites, `.env`) |
| §3 (`version:base64` format, active = highest) | ADR-003, PRD §23.2/§27.3, data dictionary §2.7 | install.md step 2, key-rotation.md §A |
| §4 (2-service compose) | ADR-021, PRD §27.21/§14.1 | install.md step 3 |
| §6 (auto migrations at boot, root bootstrap only once) | ADR-025, PRD §10.2/§18.2 | install.md step 4, upgrade-downgrade.md §A |
| §5–§6 (instance tree, `/api/v1/health`, graceful shutdown) | PRD §7.5/§22.1, OpenAPI `/health` | control-plane-restore.md, install.md (verification) |
