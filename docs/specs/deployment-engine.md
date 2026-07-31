# Specification — Deployment engine

> PRD §29.4 artifact (`docs/PRD.md`). The PRD is the source of truth; this specification refines it down to the level of commands, directories, timeouts, locks and compensation. Where the PRD is silent, the chosen value is marked **(proposed default)**. No Go implementation decision is made here; the contracts (queue, SSH transport, runtime adapter, proxy provider) are those of §18.1.
>
> Scope: applications (docker image and dockerfile build packs in P0; nixpacks, railpack, static in P1). The **Docker Compose** build pack is covered in the dedicated specification (§29.5, `docs/specs/compose-spec.md`); only its contact points with the engine (queue, locks, states) are mentioned here.

---

## 1. Overview

### 1.1 From trigger to container in production

```text
Trigger (UI / API / webhook / CLI / schedule)
        │  validation, authorization, config snapshot, SHA resolution
        ▼
API : INSERT Deployment (queued) + INSERT job + INSERT outbox  ── same PostgreSQL transaction
        │
        ▼
PostgreSQL queue (FOR UPDATE SKIP LOCKED, lease + heartbeat)
        │
        ▼
Worker : state machine (§4) — each step = SSH commands on the target server
        │
        ▼
Target server : Docker Engine/BuildKit + proxy (Traefik) + /var/lib/akerdock/ tree
```

### 1.2 Actors

| Actor | Responsibility | What it never does |
|---|---|---|
| **API (control plane)** | Auth/policy, validation, versioned configuration snapshot (INV-014), branch → immutable SHA resolution, transactional creation of `Deployment` + job + outbox event, `202` response with `deployment_uuid` | Executing a remote command; waiting for the deployment to finish |
| **Queue (PostgreSQL)** | Job durability (INV-013), ordering by priority/date, leases, retries, dead-letter | Business logic |
| **Worker** | Job acquisition, locks and slots, state machine execution, log streaming, compensation, guaranteed resource release | Serving user HTTP traffic; being the source of truth for any state |
| **Target server (via SSH)** | Running git/docker/buildkit, hosting the containers, the proxy and the files under `/var/lib/akerdock/` | Contacting the control plane (push architecture, §18.1) |
| **Proxy (Traefik, P0)** | Routing traffic, applying the intermediate representation (§27.9) | Deciding the switchover (the worker drives it) |

### 1.3 Sources of truth involved (§18.3)

- Desired configuration: PostgreSQL snapshot frozen at enqueue time — a replayed deployment uses **its** snapshot, never the current config.
- Source code: SHA resolved at enqueue time, immutable (a later push = a new deployment, possibly coalesced §3.4).
- Image: **OCI digest** resolved before switchover; a tag is never sufficient for a rollback.
- Remote state: the target server's Docker, queried by inspection — never assumed from the database (INV-004, §22.1).
- Routing: the proxy's dynamic configuration file on the server, generated deterministically from the intermediate representation, validated and checksummed.

---

## 2. Queue and jobs

### 2.1 Semantic schema

Two tables (names are indicative, the semantic contract is normative):

**`jobs`** — generic durable queue (§21.3), critical queries in explicit SQL (decision §27.25).

| Column | Type | Semantics |
|---|---|---|
| `id` | uuid | Job identifier |
| `queue` | text | Logical queue: `deployments`, `server_ops`, `backups`, `maintenance`… (separate priorities, §24.3) |
| `type` | text | `deployment.run`, `deployment.cancel_cleanup`… |
| `payload` | jsonb | References only: `deployment_uuid`, `application_uuid`, `server_uuid` — **never a secret** (INV-003) |
| `status` | enum | `scheduled → queued → leased → running → succeeded \| retry_wait \| cancelled \| dead_letter` (§21.3) |
| `priority` | int | Lower = higher priority; default `100` **(proposed default)** |
| `run_at` | timestamptz | Eligibility date (retry backoff) |
| `attempt` / `max_attempts` | int | Current attempt / ceiling (§2.4) |
| `leased_by` | text | Worker identity (`hostname:pid:uuid`) |
| `lease_expires_at` | timestamptz | Lease expiration |
| `heartbeat_at` | timestamptz | Last heartbeat |
| `idempotency_key` | text unique | Enqueue deduplication (INV-004, §24.1) |
| `cancel_requested_at` | timestamptz | Cooperative cancellation (§2.6) |
| `last_error` | jsonb | Classification of the last error (code, step, redacted) |
| `created_at`, `started_at`, `finished_at` | timestamptz | UTC timestamps (§22.3) |

**`deployments`** — business history (§19.1): `uuid`, `application_uuid`, `server_uuid`, `destination_uuid`, `state` (state machine §4), `commit_sha`, `config_snapshot_id`, `image_ref`, `image_digest`, `trigger` (`manual|api|webhook|preview|schedule|config_apply|cli_local` — canonical vocabulary of the data dictionary, rollback being carried by `is_rollback`), `webhook_delivery_id`, `forced` (build without cache), `skip_build` (build nothing — §5.8), `superseded_by` (coalescing §3.4), `attempt`, per-step timestamps, `finished_at`.

### 2.2 Transactional enqueue

A single PostgreSQL transaction contains: creation of the `Deployment` (state `queued`), configuration snapshot, `INSERT jobs`, `INSERT outbox` (`deployment.queued.v1`), and the queue-cap check (§3.2). Commit = the job exists and will survive any crash (INV-013). After commit: `NOTIFY akerdock_jobs` to wake up the workers **(proposed default)**.

### 2.3 Acquisition, lease, heartbeat

Acquisition by a worker (exact semantics):

```sql
SELECT id FROM jobs
WHERE queue = $1 AND status = 'queued' AND run_at <= now()
ORDER BY priority, run_at
FOR UPDATE SKIP LOCKED
LIMIT 1;
-- then, same transaction:
UPDATE jobs SET status = 'leased', leased_by = $worker,
  lease_expires_at = now() + interval '90 seconds',
  heartbeat_at = now()
WHERE id = $id;
```

Slot and lock constraints (§3) are checked **within the same transaction** as the acquisition; if a slot is missing, the job is left in `queued` (with `run_at = now() + 5 s` to avoid a busy-loop) and the worker moves on to the next one.

| Parameter | Value | Note |
|---|---|---|
| Lease duration | **90 s** | Renewed by heartbeat **(proposed default)** |
| Heartbeat interval | **20 s** | `UPDATE jobs SET heartbeat_at = now(), lease_expires_at = now() + 90 s WHERE id = $1 AND leased_by = $worker`; a heartbeat failure (lost lease) requires the worker to **abandon the job immediately** without any further remote mutation **(proposed default)** |
| Wake-up | `LISTEN akerdock_jobs` + fallback polling every **5 s** **(proposed default)** |
| Expired-lease scan | Every **30 s**: `status IN ('leased','running') AND lease_expires_at < now()` → put back to `queued` with `attempt` unchanged and marker `recovered = true` **(proposed default)** |

### 2.4 Retry, backoff, dead-letter

Mandatory classification of every failure (§22.1):

- **Transient infrastructure error** (SSH unreachable, network timeout, registry 5xx, remote disk momentarily full) → automatic retry.
- **Deterministic error** (build failure, healthcheck never healthy, failing pre/post command, invalid config, empty `${VAR:?}`) → **no automatic retry**; the deployment goes to `failed`, manual retry possible (§21.1: `failed → retrying → preparing`, new attempt explicitly linked).

| Parameter | Value |
|---|---|
| `max_attempts` (`deployment.run` jobs) | **3** (1 execution + 2 automatic infra retries) **(proposed default)** |
| Backoff | `delay = min(30 s × 2^(attempt−1), 15 min)` **(proposed default)** |
| Jitter | Full jitter: `run_at = now() + random(0, delay)` (bounds: 0 s – 15 min) **(proposed default)** |
| Dead-letter | `attempt ≥ max_attempts` → `status = 'dead_letter'`, `Deployment.state = failed`, `deployment.failed.v1` event, notification, dashboard entry under "priority actions". Replay from the dead-letter = an audited manual action that creates a **new linked attempt** |

### 2.5 Crash recovery (INV-004, INV-013, §22.1)

A job recovered after lease expiration is **never replayed blindly**. The recovering worker first performs a **remote inspection** and then decides to resume / compensate / finish:

1. Read `Deployment.state` and the step metadata in the database (last committed checkpoint).
2. Inspect the server: `docker image inspect` of the expected image (`akerdock.deployment_uuid` label), `docker container inspect <uuid>-next` and `<uuid>`, checksum of the proxy file, presence of the clone directory.
3. Apply the recovery rule of the state concerned ("crash during this state" column of the table in §4).

General rule: every remote effect is **idempotent or detectable** — the objects created carry the label `akerdock.deployment_uuid=<uuid>`, which makes it possible to know whether the step has already produced its effect before replaying it.

### 2.6 Cooperative cancellation

- Cancellation (UI/API, §5.5) writes `cancel_requested_at` and publishes `NOTIFY`.
- The worker checks the flag at **every checkpoint** (= every state transition of the machine §4) and **before every long remote command** (clone, build, pull, push).
- To interrupt an in-flight command: the SSH channel is closed with a signal sent, long commands being launched via `timeout -k 10 <secs> <cmd>` on the remote side to guarantee termination **(proposed default)**.
- **Cancellation barrier**: from the moment `switching` is entered, cancellation is refused (the switchover is atomic: it either completes or is compensated, never interrupted).
- After cancellation: compensation identical to a failure at the same point (§9), terminal state `cancelled`, release of locks/slots, cleanup of the candidate.

---

## 3. Locks and concurrency control

All locks are materialized in PostgreSQL (multi-instance locks, §18.2). **Guaranteed** release: every acquisition is recorded with the holding `job_id`; the end of the job (success, failure, panic, cancellation) goes through a single exit point that releases locks and slots (defer/finally semantics); in addition, the expired-lease scan (§2.3) releases the locks of dead jobs.

### 3.1 Exclusive lock per (application, destination)

- **A single active deployment** (states `preparing` → `finishing`) per `(application_uuid, destination_uuid)` pair: the others wait in `queued`. PR previews have their own identity (§20.4) and therefore their own lock.
- The `switching` state is furthermore protected by this same lock in a **strict** manner (§21.1): no crash recovery may re-switch until the inspection has determined the outcome of the previous switchover (no double switchover, §16.4).
- Semantic implementation: a `resource_locks(application_uuid, destination_uuid, holder_job_id, acquired_at)` row with a uniqueness constraint, taken within the job acquisition transaction.

### 3.2 Slots and per-server caps (§5.5)

| Parameter | Default | Semantics |
|---|---|---|
| `concurrent_builds` | **2** | Max number of deployment jobs running simultaneously per server (count of `jobs` in `leased/running` targeting the server, checked at acquisition). A deployment without a build (docker image, rollback) also consumes a slot: it runs pull/start/switchover on the same Docker **(proposed default: a single slot type, no separate queue)** |
| `deployment_queue_limit` | **25** | Max number of `queued` deployments per server; beyond that, the enqueue is **refused** at the API with `429` and a stable error code (`deployment_queue_full`) **(proposed default: refusal rather than blocking)** |

Both values are configurable per server; the per-team limit (§22.2) applies on top, with the same rule.

### 3.3 Queue ordering

FIFO by `(priority, run_at)` within a server. An "urgent" manual redeploy MAY receive `priority = 50` **(proposed default)**. The "running/pending deployments" view (§5.5) reads directly from `deployments` + `jobs`.

### 3.4 Push coalescing (§20.3.5)

When enqueuing a webhook-triggered deployment for `(application, branch)`:

1. Look for an existing `queued` deployment (job not yet `leased`) for the same application and the same branch, originating from a webhook.
2. If one exists with an older SHA: mark it `superseded` (terminal state assimilated to `cancelled`, `superseded_by = <new uuid>`), cancel its job, create the new deployment at the recent SHA. The original webhook delivery remains traced and points to the deployment that superseded it.
3. A deployment already `leased`/in progress is **never** coalesced: it runs to completion, the new one waits for the §3.1 lock.

Coalescing window = as long as the job is in `queued`; no artificial delay **(proposed default)**.

---

## 4. Deployment state machine (§21.1)

```text
queued → preparing → cloning → building → pushing? → starting
   └──────────────────────────────────────────────→ cancelled
starting → healthchecking → switching → finishing → succeeded
    └──────────────→ failed ←──────────────────────────┘
failed → retrying → preparing
```

Every transition is committed to the database **before** the remote action of the next state (write-ahead: the state in the database says "what may have started", the remote inspection says "what actually happened"). Every transition publishes an outbox event (§12). `cancelled`, `failed`, `succeeded` are terminal for an attempt.

Build packs without a build step (docker image, rollback) pass through `cloning`/`building`/`pushing` as no-ops (immediate, traced transition). Per-build-pack action details in §5.

| State | Preconditions | Exact actions | Remote side effects | Timeout | Crash during this state (recovery rule) | Failure transition |
|---|---|---|---|---|---|---|
| **queued** | Deployment + job committed; §3.2 cap respected | Waiting for acquisition; target of coalescing §3.4 | None | None (bounded by `deployment_queue_limit`) | Nothing to recover (no effect) | Cancellation → `cancelled` |
| **preparing** | §3.1 lock acquired, §3.2 slot acquired, server `ready` | Load the config snapshot; SSH connection (`docker info` test); check disk space (`df -P /var/lib/akerdock`, min threshold **2 GiB free (proposed default)**); create the directory tree (§5.1) — since ADR-054/055 no `build.env`/`runtime.env` is uploaded here: runtime variables ride the typed create body over the agent channel and build args/secrets travel in the typed build command (only the nixpacks build pack still sources a host `build.env`, written at build time); run the **pre-deployment command** (§10); check the destination network (`docker network inspect`, create if absent) | Directories (0700); destination network | SSH connect: **10 s** (configurable per server, PRD §3.1); full state: **120 s (proposed default)** | Idempotent: replay everything (mkdir -p, re-upload files, re-run the pre-command — it MUST be idempotent, documented §10) | `failed`; compensation C1 (§9) |
| **cloning** | Git source; valid credentials | Commands §5.3.1: shallow clone at the exact SHA, submodules/LFS if enabled | `source/<deployment_uuid>/` directory | **600 s (proposed default)**, configurable per application | Potentially partial directory → `rm -rf` of this deployment's directory then re-clone (idempotent by destruction) | `failed`; C1 |
| **building** | Source present; build plan generated | Commands §5.3.2/§5.4/§5.5/§5.6 depending on build pack; logs streamed (§12.2); `--no-cache` if `forced` | Local image `akerdock/<app_uuid>:<sha12>` with §6 labels | **3600 s (proposed default)**, configurable per application | `docker image inspect akerdock/<app_uuid>:<sha12>` + `akerdock.deployment_uuid` label: if present and complete → move to the next state; otherwise rerun the build (the BuildKit cache makes the replay cheap) | `failed` (deterministic: no auto retry); C1 |
| **pushing** *(optional)* | Registry configured (decision §27.6) or required (build server, multi-server) | `docker tag` + `docker push` (§5.3.3); **OCI digest resolution** and recording in `DeploymentArtifact` | Image in the registry | **900 s (proposed default)** | Idempotent push (deduplicated layers) → replay; re-resolve the digest | `failed` if registry mandatory; **otherwise** degradation to local-retention mode + warning **(proposed default)**; C1 |
| **starting** | Image present (local or pulled); digest resolved | Create missing volumes (§6.3); `docker create` of the candidate `<uuid>-next` + `docker start` (§5.3.4); in non-rolling mode (§7.4): stop the old one first | Candidate container; volumes | Creation + start: **60 s (proposed default)** | `docker container inspect <uuid>-next`: absent → recreate; present `created` → start; present `running` → continue; present `exited` → remove and recreate | `failed`; C2 (§9) |
| **healthchecking** | Candidate `running` | Polling `docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' <uuid>-next` every **2 s (proposed default)** until `healthy`; without a defined healthcheck: app **ineligible for rolling** (§7.3), in non-rolling mode wait for `running` stable for **10 s (proposed default)**; then **post-deployment command** in the candidate (§10) | None (read + exec) | `start_period + (interval + timeout) × retries + 30 s` of margin (defaults §11 → ~135 s) | Re-read the health state: `healthy` → continue; `unhealthy`/gone → `failed` + C2; in progress → resume polling | `failed` (deterministic); C2; the candidate's logs (`docker logs --tail 200 <uuid>-next`) are captured into the build log before removal **(proposed default)** |
| **switching** | Candidate `healthy` + post-command OK; strict §3.1 lock; **cancellation barrier active** | Algorithm §7.2: generation of the intermediate representation → Traefik dynamic file → atomic application → verification; then graceful stop of the old one, `docker rename` of the candidate | Proxy file modified; old container stopped/removed; candidate renamed `<uuid>` | **120 s (proposed default)** excluding stop grace; + `stop_grace_period` | **Critical case.** Inspection: (a) proxy file still points to the old one → candidate still healthy? yes: replay the switchover; no: `failed` + C2; (b) file points to the candidate, old one still present → resume at stopping the old one; (c) old one absent, rename not done → resume at the rename; never a second switchover without this inspection (INV-004/005) | Before proxy application: `failed` + C2. After verified application: the switchover has happened → continue toward `finishing` if possible, otherwise `failed` + C3 (§9) |
| **finishing** | Traffic switched, final container named `<uuid>` | Update the proxy config to the stable form (§7.2 step 7); record `DeploymentArtifact` (digest, retained tags); reclaim best-effort the images outside the retention window (§8.2, `image_retention_count`) and schedule the asynchronous cleanup of old sources; update the resource's observed state | Proxy file stabilized; metadata | **60 s (proposed default)** | All actions are idempotent → replay in full | A failure here = deployment **succeeded with a warning** (`succeeded` + `deployment.finishing_degraded.v1` event): traffic is already on the new version, never break it for a cleanup **(proposed default)** |
| **succeeded** | — | Event + notification; lock/slot release | — | — | — | — |
| **failed** | — | Compensation executed (§9); event + notification; lock/slot release; error classification (§2.4) | — | — | Compensation itself is idempotent and replayable | — |
| **cancelled** | Cancellation before the barrier | Compensation at the current point (§9); lock/slot release | — | — | Same as `failed` | — |
| **retrying** | Manual action on `failed`, or automatic infra retry | New linked attempt (`attempt + 1`, history preserved §21.1) → `preparing` with the **same snapshot and the same SHA** | — | — | — | — |

---

## 5. Execution plan per build pack

### 5.1 Remote directory tree (normative)

> **Amendment (July 19, 2026)**: the root moves from `/data/akerdock` to
> **`/var/lib/akerdock`** (FHS compliance — a service's persistent state lives
> under `/var/lib`). Migration: on the first proxy bootstrap on a server
> carrying the old layout, the engine **moves** `/data/akerdock` to
> `/var/lib/akerdock` (the ACME storage — Let's Encrypt account and certificates —
> moves along, nothing is re-issued) after removing the proxy container,
> recreated immediately on the new path. Application containers already running
> keep their original bind mounts until their next deployment.

```text
/var/lib/akerdock/                              # root, 0750, owner = AkerDock SSH user
├── applications/<app_uuid>/
│   ├── source/<deployment_uuid>/             # throwaway clone per deployment, purged in finishing (retention: previous + current)
│   ├── env/
│   │   └── build.env                         # build-time variables — written only for the nixpacks build pack (0600, ADR-055); other paths never materialize env on the host
│   └── keys/deploy_key                       # ephemeral Git deploy key if needed (0600, deleted after clone)
├── proxy/
│   ├── dynamic/<app_uuid>.yaml               # Traefik dynamic config per application (§7)
│   └── certs/                                # custom certificates (PRD §4.3)
├── backups/                                  # out of scope for this spec (§20.5)
└── tmp/                                      # temporary space, purged by the cleanup
```

**(proposed default)** for the whole tree: all of a target server's state lives under `/var/lib/akerdock`, which makes inventory, backup and cleanup obvious (PRD §14.1). All files containing variable values are mode `0600`, directories `0700`.

### 5.2 Variables: build-time vs runtime (PRD §5.4, INV-003, INV-012)

| Category | Materialization | Consumption |
|---|---|---|
| Runtime | Rendered on the control plane, never on the host (ADR-054/055) | Rides the **typed container-create body** over the agent channel — never `-e KEY=value` on a command line, no `runtime.env`/`runtime.sh` on the host |
| Non-secret build-time | Typed build command body (ADR-055); nixpacks only: `env/build.env` on the host | BuildKit build args in the typed command — nothing sensitive in `ps` (INV-012); nixpacks exports them via `set -a; . build.env; set +a` in the remote session |
| Build secret (BuildKit opt-in) | Typed build command body — never a host file (ADR-055) | BuildKit **session secret**; consumed in the Dockerfile via `RUN --mount=type=secret,id=<NAME>`; absent from image layers and history |
| Predefined (decision §27.22) | Injected with the runtime variables: `AKERDOCK_FQDN`, `AKERDOCK_URL`, `AKERDOCK_BRANCH`, `AKERDOCK_RESOURCE_UUID`, `AKERDOCK_CONTAINER_NAME`, `PORT`, `HOST`, `AKERDOCK_PR_ID` (previews); `SOURCE_COMMIT` as an **opt-in** build arg (PRD §5.2) | Like the categories above |

Rules:
- Interpolation of shared variables (`{{team.VAR}}`…) and of the `deployment` pseudo-scope (`{{deployment.fqdn}}`, `{{deployment.url}}`, `{{deployment.pr_id}}` — the deployment's own identity, resolved to the primary domain in production and to the preview FQDN which changes per PR; case-insensitive keys) and verification of required variables `${VAR:?}` **on the control plane, before enqueue** — a missing value blocks the deployment at validation, not mid-build.
- File transfer via **SFTP** (content never in argv nor in a logged command heredoc), mode `0600` set at upload.
- The `env/` files are rewritten at every deployment from the snapshot; a replayed deployment reproduces exactly the same files (INV-014).

### 5.3 `dockerfile` build pack (P0)

#### 5.3.1 Clone (`cloning` state)

Shallow clone **at the exact SHA** (a `git clone --depth 1 -b <branch>` would follow the moving head — forbidden):

```sh
mkdir -p /var/lib/akerdock/applications/<app_uuid>/source/<deployment_uuid>
cd /var/lib/akerdock/applications/<app_uuid>/source/<deployment_uuid>
git init -q
git remote add origin <repo_url_without_credentials>
git fetch -q --depth 1 origin <commit_sha>
git checkout -q --detach FETCH_HEAD
# if submodules enabled:
git submodule update --init --recursive --depth 1
# if LFS enabled:
git lfs install --local && git lfs pull
```

Authentication (INV-003, INV-012) — never a credential in the URL or in argv:
- **SSH deploy key**: key uploaded to `keys/deploy_key` (0600), `GIT_SSH_COMMAND="ssh -i /var/lib/akerdock/applications/<app_uuid>/keys/deploy_key -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"` set in the session environment; file deleted at the end of the state.
- **HTTPS token (GitHub App / PAT)**: `git config credential.helper 'store --file=/var/lib/akerdock/applications/<app_uuid>/keys/git_credentials'` with the file uploaded via SFTP (0600), deleted at the end of the state **(proposed default)**.

The **base directory** (monorepo) does not change the clone; it changes the build context (`<clone>/<base_directory>`).

#### 5.3.2 Build (`building` state)

> **ADR-055 phase 2.** Dockerfile builds no longer run this shell command: the
> agent drives the daemon's embedded BuildKit through a typed build command
> whose body carries the build args and BuildKit session secrets — no
> `build.env` sourcing, no `--secret src=` host files. The snippet below is
> kept as the semantic reference for flags, labels and arg/secret handling
> (only the nixpacks path still builds through the host CLI, §5.5).

```sh
cd /var/lib/akerdock/applications/<app_uuid>/source/<deployment_uuid>/<base_directory>
set -a; . /var/lib/akerdock/applications/<app_uuid>/env/build.env; set +a   # superseded — see note above
DOCKER_BUILDKIT=1 docker build \
  --file <dockerfile_location>            # default: ./Dockerfile
  --progress plain \
  --tag akerdock/<app_uuid>:<sha12> \
  --label akerdock.managed=true \
  --label akerdock.resource_uuid=<app_uuid> \
  --label akerdock.team_uuid=<team_uuid> \
  --label akerdock.deployment_uuid=<deployment_uuid> \
  --label akerdock.commit_sha=<commit_sha> \
  --build-arg AKERDOCK_FQDN --build-arg AKERDOCK_BRANCH … \   # auto-injected build args, can be disabled (PRD §5.2); values via env, not argv
  [--build-arg SOURCE_COMMIT]             # opt-in
  [--secret id=<NAME>,src=/var/lib/akerdock/applications/<app_uuid>/env/secrets/<NAME>]…
  [--no-cache]                            # if forced (deploy webhook force=true, PRD §5.5)
  .
```

`<sha12>` = first 12 hexadecimal characters of the commit SHA **(proposed default)**. Builds run via the server Docker's BuildKit in P0/P1 (decision §27.5); the build adapter contract is defined from P0 for the later move to rootless builders — this spec only uses operations expressible in both modes.

#### 5.3.3 Push (`pushing` state, if a registry is configured — decision §27.6)

```sh
# auth: encoded per request into the push/pull command sent over the typed
# agent channel (ADR-051/055) — no `docker login` ever runs, so no token
# lands in any host's ~/.docker/config.json (INV-003)
docker tag akerdock/<app_uuid>:<sha12> <registry>/<image>:<sha12>
[docker tag akerdock/<app_uuid>:<sha12> <registry>/<image>:<tag_custom>]   # PRD §5.2
docker push <registry>/<image>:<sha12>
# OCI digest resolution (source of truth of the artifact, §18.3):
docker image inspect --format '{{index .RepoDigests 0}}' <registry>/<image>:<sha12>
```

The digest (`<registry>/<image>@sha256:…`) is recorded in `DeploymentArtifact` before any switchover.

#### 5.3.4 Candidate creation and start (`starting` state)

```sh
# declared volumes (§6.3) — idempotent creation:
docker volume create --label akerdock.managed=true --label akerdock.resource_uuid=<app_uuid> \
  --label akerdock.team_uuid=<team_uuid> <app_uuid>_<volume_name>

docker create \
  --name <app_uuid>-next \
  --network <destination_network> \
  # runtime variables travel in the typed create body over the agent channel (ADR-054/055), not via --env-file \
  --restart unless-stopped \
  --stop-timeout <stop_grace_period> \
  --label akerdock.managed=true \
  --label akerdock.resource_uuid=<app_uuid> \
  --label akerdock.type=application \
  --label akerdock.team_uuid=<team_uuid> \
  --label akerdock.deployment_uuid=<deployment_uuid> \
  [--health-cmd '<cmd>' --health-interval <i>s --health-timeout <t>s --health-retries <r> --health-start-period <s>s] \  # only if no Dockerfile HEALTHCHECK (takes precedence, PRD §5.3)
  [-v <app_uuid>_<volume_name>:<mount_path>]… [-v <host_path>:<mount_path>]… \
  [--memory … --memory-reservation … --memory-swap … --cpus … --cpuset-cpus … --cpu-shares …] \
  [-p <ip:host:container[/proto]>]…       # Ports Mappings — makes the app ineligible for rolling (§7.3)
  [<custom_docker_options>]               # validated/escaped centrally (INV-012, §23.3)
  <image_ref>                             # digest if registry (<registry>/<image>@sha256:…), local tag otherwise
docker start <app_uuid>-next
```

HTTP healthcheck generated when path/port are configured without a Dockerfile `HEALTHCHECK`:
`--health-cmd 'curl -fsS -X <method> http://127.0.0.1:<port><path> || wget -q -O /dev/null http://127.0.0.1:<port><path>'` (requires curl or wget in the image, PRD §5.3; the absence of both = documented `unhealthy` state with remediation).

In **non-rolling** mode (§7.4), the old container is stopped and removed before `docker create`, and the container is created directly under the name `<app_uuid>`.

### 5.4 `docker image` build pack (P0)

No Git source: `cloning`/`building` are no-ops.

```sh
# private registry: per-request auth on the typed agent channel (ADR-055), no docker login
docker pull <image>:<tag>
docker image inspect --format '{{index .RepoDigests 0}}' <image>:<tag>   # OCI digest frozen for this deployment
```

Pull timeout: **900 s (proposed default)**. The rest (starting → finishing) is identical to §5.3.4, with `<image>@sha256:…` as the reference. The "external CI → deploy webhook" pattern (PRD §5.1) lands here: pull + redeploy without rebuild.

### 5.5 `nixpacks` and `railpack` build packs (P1)

Both produce a Dockerfile/plan, then join **exactly** the `dockerfile` flow (§5.3.2 → §5.3.4). The build pack binary is provisioned on the server during onboarding (§20.1) at a version pinned per AkerDock release **(proposed default)**.

**Nixpacks** — plan generation then context, within the `building` state:

```sh
cd <clone>/<base_directory>
nixpacks plan . --format json > /var/lib/akerdock/applications/<app_uuid>/source/<deployment_uuid>/.nixpacks-plan.json   # traced in the build logs
nixpacks build . --name akerdock/<app_uuid>:<sha12> \
  --label akerdock.managed=true --label akerdock.resource_uuid=<app_uuid> \
  --label akerdock.deployment_uuid=<deployment_uuid> --label akerdock.commit_sha=<commit_sha> \
  [--install-cmd '<override>'] [--build-cmd '<override>'] [--start-cmd '<override>'] \
  [--config nixpacks.toml] \
  [--no-cache]
```

Build-time variables are provided via the process environment (`set -a; . build.env; set +a`), Nixpacks propagating the environment to the build; no value in argv. Nixpacks static mode (publish directory + Nginx): handled as §5.6 with the directory produced by the build.

**Railpack (beta)** — same contract:

```sh
cd <clone>/<base_directory>
railpack build . --name akerdock/<app_uuid>:<sha12> [--env-file …/build.env] [--no-cache]
```

Railpack's exact flags are to be frozen during the P1 implementation against the pinned version; the normative requirement is: image tagged `akerdock/<app_uuid>:<sha12>`, §6 labels, secrets never in argv nor in the image.

### 5.6 `static` build pack (P1)

1. Clone (§5.3.1).
2. Generation on the server of two files in `source/<deployment_uuid>/.akerdock/`: `nginx.conf` (editable by the user in the UI, with SPA option `try_files $uri $uri/ /index.html`) and a minimal Dockerfile:

```dockerfile
FROM nginx:alpine
COPY .akerdock/nginx.conf /etc/nginx/conf.d/default.conf
COPY <publish_directory>/ /usr/share/nginx/html/
```

3. `docker build` identical to §5.3.2 (tag, labels), internal port `80`, then the standard flow.

### 5.7 `docker compose` build pack

Out of scope: see the Compose specification (§29.5, forthcoming). Contracts shared with the engine: same queue/locks/slots (§2–3), same state machine (the `starting/healthchecking/switching` states operate per service), zero-downtime per service behind the proxy required by decision §27.15, isolated network named by UUID, `is_directory`/`content`/`exclude_from_hc` extensions.

### 5.8 `skip_build`: applying a configuration (ADR-048)

A deployment carrying `skip_build` runs the plan of its build pack **minus the source and
the build**. It exists because a container freezes its environment when it is created: a
variable edited after the last deployment reaches the process only when the container is
created again, which `restart` (§21.2) does not do.

| Stage | Behavior |
|---|---|
| `clone` | Skipped — the step is recorded as skipped with its reason, never silently absent. |
| Image | The current artifact (`deployment_artifacts` of the last succeeded deployment, scoped to the preview for a PR instance), pinned by digest when it has one. Absent from the server → the deployment fails naming the image, as a rollback does (ADR-006). |
| `commit_sha` | Inherited from the last succeeded deployment. The branch is **not** re-resolved: applying a configuration must not deploy code nobody asked for. |
| Build (compose) | The compose file is still cloned — it is what plans the stack — but each service reuses the image tagged for that commit, build or pull alike; pulling a mobile tag would swap the artifact. Missing image → built or pulled as usual. |
| Everything after | Unchanged: environment file, `docker create`, health check, switchover, routing, post-deployment hook. |
| Artifacts | None recorded, none reclaimed (§10.3, §29.4) — no image was added. |

`skip_build` and `force_rebuild` are mutually exclusive (`422`), and the trigger of such a
deployment is `config_apply`.

---

## 6. Naming and labels (INV-011, §8, decision §27.22)

### 6.1 Names

| Object | Name | Note |
|---|---|---|
| Application container | `<app_uuid>` | The name is also the internal DNS hostname (PRD §2) |
| Candidate container (rolling) | `<app_uuid>-next` | Exists only between `starting` and the end of `switching`; renamed `<app_uuid>` after the switchover |
| Preview container | `<app_uuid>-pr-<pr_id>` | Identity `(application_uuid, provider, pr_id)` (§20.4) **(proposed default)** |
| Local image | `akerdock/<app_uuid>:<sha12>` | + registry tags §5.3.3 |
| Volume | `<app_uuid>_<volume_name>` | UUID prefix as anti-collision (PRD §8) |
| Destination network | name of the `Destination` (UUID for networks created by AkerDock) | Compose stacks: dedicated network named by UUID (PRD §9) |
| Proxy file | `/var/lib/akerdock/proxy/dynamic/<app_uuid>.yaml` | One file per application → atomic application/removal per resource |

All names are deterministic, derived from stable UUIDs, with no free-form user input (INV-011). Custom container names remain possible (PRD §5.3) but make the application ineligible for rolling (§7.3).

### 6.2 System labels

Set on managed **containers, images, volumes and networks**:

| Label | Value | Role |
|---|---|---|
| `akerdock.managed` | `true` | Managed / unmanaged boundary (INV-015): cleanup and adoption rely on it |
| `akerdock.resource_uuid` | Resource UUID | Attachment to the model |
| `akerdock.type` | `application` \| `database` \| `service` \| `proxy` \| `helper` | Typing |
| `akerdock.team_uuid` | Team UUID | Isolation, audit |
| `akerdock.deployment_uuid` | Deployment UUID | Idempotence of recoveries (§2.5) — containers and images |
| `akerdock.commit_sha` | Full SHA | Traceability — images |
| `akerdock.retain` | `true` | Explicit cleanup protection for rollback images (§8.2) **(proposed default)** |

User custom labels (PRD §5.3) are added after the system labels and cannot overwrite them (`akerdock.` prefix reserved, rejected at validation).

---

## 7. Zero-downtime (rolling update)

### 7.1 Routing representation

Routing is generated from the common **intermediate representation** (decision §27.9) and materialized as a **Traefik dynamic configuration file** per application (`/var/lib/akerdock/proxy/dynamic/<app_uuid>.yaml`), mounted in the Traefik container (`file` provider with `watch: true`). Routing labels are still set on the containers for parity and diagnostics, but **the file is authoritative** — it is what enables an atomic, verifiable switchover (checksum, §18.3) **(proposed default, consistent with "proxy file/labels" §18.3)**.

### 7.2 Switchover algorithm (`switching` then `finishing` states)

Precondition: candidate `<uuid>-next` `healthy`, post-command OK, strict §3.1 lock, cancellation barrier.

1. **Resolve the candidate's endpoint**: `docker inspect --format '{{(index .NetworkSettings.Networks "<destination_network>").IPAddress}}' <uuid>-next`. The IP is stable for the lifetime of the container: it serves as the **transitional** target (the name `<uuid>-next` will disappear at the rename, the IP will not).
2. **Generate** the dynamic config from the intermediate representation: routers (domains, path-based with priority to the most specific path, www redirect, middlewares), service → `url: http://<ip_next>:<exposed_ports>`.
3. **Apply atomically**: SFTP upload to `/var/lib/akerdock/proxy/dynamic/.<app_uuid>.yaml.tmp` then `mv -f` (atomic rename on the same filesystem); record the SHA-256 checksum in the database.
4. **Verify**: polling (every 1 s, max **30 s (proposed default)**) of the local Traefik API (`wget -qO- http://127.0.0.1:8080/api/http/services` executed inside the proxy container) until the new endpoint is seen; then a smoke request through the proxy: `curl -fsS -o /dev/null --max-time 5 --resolve <fqdn>:<proxy_port>:127.0.0.1 http://<fqdn><health_path>` **(proposed default)**. Verification failure → re-point the file to the old container (compensation C2) → `failed`. The old container never stopped running (INV-005).
5. **Graceful stop of the old one**: `docker stop -t <stop_grace_period> <uuid>` (SIGTERM, then SIGKILL after the delay) then `docker rm <uuid>`. The old one's image is **not** deleted (rollback §8, INV-006).
6. **Rename**: `docker rename <uuid>-next <uuid>`. Traffic keeps flowing through the IP (step 2): no window of unavailability during the rename.
7. **Stabilize** (`finishing` state): regenerate the file with `url: http://<uuid>:<exposed_ports>` (Docker DNS resolution by name, robust to container restarts that would change the IP), apply (steps 3–4). Set the parity routing labels on the final container.

Each step is individually idempotent or detectable, which grounds the recovery rules of §4 (`switching`).

### 7.3 Eligibility conditions (§5.5, PRD §15)

Rolling update **only if**: health check configured and functional, default container name (no custom name), no Docker Compose (P0/P1 — lifted by §27.15 in the Compose spec), **no host port mapping** ("Ports Mappings": two containers cannot bind the same host port).

### 7.4 Stop-then-start fallback

For ineligible applications: on entering `starting`, run `docker stop -t <stop_grace_period> <uuid> && docker rm <uuid>`, create the new container directly named `<uuid>`, start it, wait for `running` (+ health if a healthcheck exists), apply the proxy config (same steps 2–4 and 7, without the IP transition). Assumed service interruption = stop + start duration; the UI displays it as such. If the new container fails, the old one has already been removed: the compensation offers a **redeployment of the last verified artifact** (§8) — this is precisely why its images are protected (INV-006: a failure never deletes the last healthy artifact).

---

## 8. Rollback (decision §27.6)

### 8.1 Principle

A rollback is a **redeployment of a verified artifact, without rebuild**: new `Deployment` entry (`rollback` trigger, link to the original deployment and its config snapshot — INV-014), state machine with `cloning/building` as no-ops, entering `starting` only after artifact verification.

### 8.2 Artifact resolution

| Context | Verification | Source |
|---|---|---|
| Registry configured | `docker pull <registry>/<image>@sha256:<digest>` (the digest guarantees immutability, regardless of moved tags) | `DeploymentArtifact.image_digest` |
| Without registry (local fallback) | `docker image inspect akerdock/<app_uuid>:<sha12>` — presence + `akerdock.deployment_uuid` label match | Local retention |

Local retention: the images of the **last N successful deployments** are kept and recorded as protected; the automatic cleanup (PRD §3.7) excludes them anyway (it never touches a tagged image, INV-015). **N is the instance setting `image_retention_count`** (global setting, **default 5, minimum 1** — the minimum guarantees that the image in service, always the most recent, remains protected). In `finishing`, right after recording the new artifact, the engine reclaims (`docker rmi`) the images leaving the window and deletes their `DeploymentArtifact` pointers (they are no longer valid rollback targets); a **best-effort and never blocking** operation (a rebuild reproduces any image, we never break a successful deployment for a cleanup). A rollback reclaims nothing: it redeploys an existing artifact.

Previews: the same N retention applies **per preview** (its images live under the `akerdock/<preview_uuid>` namespace, distinct from production). On PR close/merge, the preview's destruction deletes **all** of its images without exception — the retention window does not survive the PR.

If the requested artifact is missing/unverifiable, the rollback is **refused at validation** with the list of artifacts actually available — never a mid-flight failure for this cause.

### 8.3 Automatic rollback (opt-in, §20.8)

Per-application policy, disabled by default. After `succeeded`: an observation window (**bake time, default 300 s (proposed default)**) on the promoted container's health check. Degradation (`unhealthy` or container exit) during the window → automatic triggering of a rollback to the previous verified artifact, notified and audited. The automatic rollback applies only once per deployment (no ping-pong loop) **(proposed default)**.

---

## 9. Compensation and failures

### 9.1 Compensation policies

| ID | Name | Actions |
|---|---|---|
| **C1** | Before any candidate container | Delete the deployment's clone (`rm -rf source/<deployment_uuid>`); keep the built image (useful for diagnostics and retry, purged by the cleanup if not promoted **(proposed default)**); the old container and its routing are intact (INV-005/006) |
| **C2** | Candidate created, switchover not effective | Capture `docker logs --tail 200 <uuid>-next` into the build log; `docker stop -t 10 <uuid>-next && docker rm <uuid>-next`; if the proxy file was modified without conclusive verification: regenerate it toward the old container and verify (steps 3–4 of §7.2); **never touch** the old container, its volumes, or the protected images (INV-006) |
| **C3** | After verified switchover | No implicit un-switch: the new container stays in production; if the automatic rollback policy (§8.3) is active and the previous artifact verified → auto rollback; otherwise `failed` with a proposed manual remediation (rollback button) — §20.2 |

### 9.2 Step × failure → action table

| State at failure time | Failure examples | Compensation | Auto retry? |
|---|---|---|---|
| `preparing` | SSH down, disk full, failing pre-command | C1 (nothing to clean except env files, left in place — rewritten on the next run) | SSH/network: yes; pre-command/disk: no |
| `cloning` | Git auth, SHA not found, timeout | C1 | Timeout/network: yes; auth/SHA: no |
| `building` | Compilation error, `${VAR:?}` (defense in depth), build OOM | C1 | No (deterministic) |
| `pushing` | Registry unreachable, 401 | C1; if registry optional: degradation to local retention + warning | 5xx/network: yes; 401: no |
| `starting` | Corrupted image, host port taken, invalid Docker options | C2 | No |
| `healthchecking` | Never `healthy`, container exited, failing post-command | C2 | No |
| `switching` before verified proxy application | Invalid IR generation, upload failure, Traefik verification failure | C2 | Application/verification error: one immediate local re-attempt then C2 **(proposed default)** |
| `switching` after verified application | `docker stop`/`rm`/`rename` failure | Continue: re-attempt, otherwise `failed` + C3 (traffic is already correct; the stopped-but-not-removed old container is flagged for reconcilable cleanup, §20.6) | Yes (idempotent) |
| `finishing` | Cleanup/labels/protection failure | None: degraded `succeeded` + asynchronous reconciliation task | Yes (separate job) |
| Any state, cancellation | `cancel_requested_at` | Compensation of the current state (C1 or C2); refused after the barrier (§2.6) | — |

### 9.3 Guaranteed release

The lock (§3.1) and slot (§3.2) are released on **all** exit paths (success, failure, cancellation, dead-letter) by the job's single exit point; if the worker dies, the release is performed by the expired-lease scan (§2.3) after the recovery inspection. The compensation itself is a set of idempotent operations: a crash during compensation leads to its resumption, not its abandonment.

---

## 10. Pre/post-deployment commands (PRD §5.3)

| | Pre-deployment | Post-deployment |
|---|---|---|
| **Where** | `docker exec <uuid> sh -c '<command>'` in the **existing container** (the old one) | `docker exec <uuid>-next sh -c '<command>'` in the **new container** (the candidate) |
| **When** | End of the `preparing` state, before any clone/build | End of the `healthchecking` state, after `healthy`, **before** `switching` |
| **If no existing container** (first deployment, or stopped app) | Step skipped, traced in the log (`skipped: no running container`) **(proposed default)** | N/A (the candidate always exists at this point) |
| **Timeout** | **600 s (proposed default, configurable per application)** | **600 s (proposed default, configurable per application)** |
| **Effect of a failure** (exit code ≠ 0 or timeout) | Deployment `failed` before any build mutation — the existing one is untouched | Deployment `failed`, compensation C2 (candidate removed), **no switchover, no automatic rollback** (PRD §5.3: "post failure = failed deployment, without auto rollback") — the old container remains routed (INV-005) |
| **Logs** | stdout/stderr integrated into the build log, secrets not interpolated in the logged command line | same |

The commands are user-supplied: documented as required to be **idempotent** (they may be replayed during a crash recovery, §4). They run with the container's environment (the runtime variables are already there); no variable is added in argv.

---

## 11. Timeouts, intervals and retries — recap

Unless stated otherwise, every value is a **(proposed default)**; "Configurable" indicates the intended override level. All remote operations have timeout + cancellation + classification + bounded retry with jitter (§22.1).

| Parameter | Default | Configurable | Reference |
|---|---|---|---|
| SSH connect timeout | 10 s | Per server (PRD §3.1) | §4 `preparing` |
| SSH keepalive (ServerAlive) | 15 s, 3 failures max | Instance | — |
| SSH command inactivity (no output) | 300 s | Instance | Frozen-command detection |
| Minimal disk space before build | 2 GiB | Per server | §4 `preparing` |
| `preparing` state (total) | 120 s | No | §4 |
| Git clone (+ submodules/LFS) | 600 s | Per application | §4 `cloning` |
| Build | 3600 s | Per application | §4 `building` |
| Image pull | 900 s | Per application | §5.4 |
| Registry push | 900 s | Per application | §4 `pushing` |
| Container creation + start | 60 s | No | §4 `starting` |
| Health check — interval / timeout / retries / start period | 5 s / 5 s / 10 / 5 s | Per application (PRD §5.3) | §4 `healthchecking` |
| Health state polling | 2 s | No | §4 |
| Total healthchecking budget | start_period + (interval+timeout)×retries + 30 s | Derived | §4 |
| `running` stability without healthcheck (non-rolling mode) | 10 s | No | §4 |
| Pre/post-deployment command | 600 s | Per application | §10 |
| Proxy verification after application | 30 s (1 s polling) | No | §7.2 |
| `switching` state (excluding stop grace) | 120 s | No | §4 |
| Stop grace period | 30 s | Per application (PRD §5.3) | §7.2 |
| `finishing` state | 60 s | No | §4 |
| Job lease | 90 s | Instance | §2.3 |
| Heartbeat | 20 s | Instance | §2.3 |
| Expired-lease scan | 30 s | Instance | §2.3 |
| Queue fallback polling | 5 s | Instance | §2.3 |
| Retry backoff (base / factor / max / jitter) | 30 s / ×2 / 15 min / full | Instance | §2.4 |
| `max_attempts` (deployment.run) | 3 | Instance | §2.4 |
| `concurrent_builds` | 2 (PRD §5.5) | Per server | §3.2 |
| `deployment_queue_limit` | 25 (PRD §5.5) | Per server | §3.2 |
| Rollback image retention (`image_retention_count`) | 5 images (min 1) | Instance; same per preview | §8.2 |
| Bake time (opt-in auto rollback) | 300 s | Per application | §8.3 |
| Source clone retention | current + previous | Instance | §5.1 |

---

## 12. Observability

### 12.1 Events (transactional outbox, §18.2, §24.2)

Every state transition publishes an event in the same transaction as the transition (envelope §24.2, versioned, without secrets):

| Event | Emitted at |
|---|---|
| `deployment.queued.v1` | Enqueue (§2.2) |
| `deployment.started.v1` | `queued → preparing` |
| `deployment.step_changed.v1` | Every intermediate transition (`payload.from`, `payload.to`, `payload.attempt`) |
| `deployment.superseded.v1` | Coalescing (§3.4) |
| `deployment.cancel_requested.v1` / `deployment.cancelled.v1` | Cancellation (§2.6) |
| `deployment.succeeded.v1` | Terminal (payload: `commit_sha`, `image_digest`, per-step duration) |
| `deployment.failed.v1` | Terminal (payload: failing step, classification, `attempt`, dead-letter if any) |
| `deployment.finishing_degraded.v1` | Success with degraded cleanup (§4) |
| `deployment.rollback_triggered.v1` | Manual or automatic rollback (§8) |

Consumers: realtime hub (UI progress), notifications (PRD §11 — routing/aggregation §27.19), audit, future outgoing webhooks. Idempotent consumers, deduplication by `id`.

### 12.2 Build logs (streaming)

- The worker captures stdout/stderr of every remote command as a stream, line by line: UTC timestamp, machine state, monotonic sequence number.
- Persistence in the database (`deployment_logs` table, append-only, cursor = sequence number) and **SSE** broadcast with `Last-Event-ID` resumption (decision §27.24).
- Backpressure: bounded buffer, cursor-based resumption, explicit `lines_dropped` signal if lines were dropped (§22.2).
- Redaction before persistence: secret values known from the snapshot are masked (`***`) in every line; ANSI/HTML sequences neutralized at display time (§23.3); INV-003.
- Log retention aligned with the deployment history retention (§19.2).

### 12.3 Audit and correlation

- Audit entries (§23.4) for: triggering (actor/token, trigger, `Idempotency-Key`), cancellation, manual retry, rollback (manual and automatic), replay from the dead-letter.
- Unique `correlation_id` propagated: API request → job → events → logs → notifications; the `webhook_delivery_id` links an auto-deploy to its original delivery (§20.3).
- Metrics (OTLP, decision §27.8): duration per state, queue size per server, success rate (§16.4: ≥ 99 % excluding application errors), expired leases, retries, coalescings, dead-letters, proxy switchover duration.
- The configuration diff included in a redeploy (PRD §5.5) is attached to the `Deployment` (versioned snapshot, INV-014).

---

## 13. PRD traceability

| Section of this spec | PRD sections |
|---|---|
| 1 | §5.5, §18.1–18.3, §20.2 |
| 2 | §17 (INV-004/013), §21.3, §22.1, §27.2, §27.25 |
| 3 | §5.5, §20.3.5, §21.1 |
| 4 | §20.2, §21.1, §22.1 |
| 5 | §5.1–5.4, §8, §17 (INV-003/012), §27.5, §27.22 |
| 6 | §2, §8, §17 (INV-011/015), §27.22 |
| 7 | §4.1, §5.5, §15, §17 (INV-005), §18.3, §27.9, §27.15 |
| 8 | §5.5, §15, §17 (INV-006), §20.8, §27.6 |
| 9 | §17 (INV-005/006), §20.2, §20.6, §20.8 |
| 10 | §5.3 |
| 11 | §3.1, §5.3, §5.5, §22.1 |
| 12 | §13, §18.2, §22.2, §23.3–23.4, §24.2, §27.8, §27.19, §27.24 |
