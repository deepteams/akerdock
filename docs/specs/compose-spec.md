# Specification — Docker Compose subset and transformations

> Artifact §29.5 of the PRD (`docs/PRD.md`). The PRD and the existing specs (`deployment-engine.md`, `data-dictionary.md`) are the source of truth; this specification details the **Docker Compose** build pack (PRD §5.2) and the one-click services (PRD §9) at the level of compose keys, transformations, variables and error codes. Where the PRD is silent, the chosen value is marked **(proposed default)**.
>
> Scope: `docker compose` build pack for applications and service stacks (one-click or free-form compose, `services`/`service_components` tables of the data dictionary). The contracts shared with the deployment engine (queue, locks, state machine, compensation) are those of `deployment-engine.md` §5.7; detailed proxy generation belongs to the proxy contract (§29.6, upcoming).

---

## 1. Supported Compose subset

### 1.1 Reference version

- Reference schema: the current **Compose Specification** (versionless specification maintained by compose-spec.io), as implemented by **Docker Compose v2** on Docker Engine ≥ 24 (PRD §22.4).
- The top-level `version:` key is **obsolete** in the Compose Specification: it is **ignored with a warning** (`compose_version_ignored`), whatever its content.
- Every file is first parsed and validated against the Compose Specification schema; the rules below apply **after** that syntactic validation.
- Four possible treatments per key: **supported** (passed through as-is), **transformed** (rewritten by AkerDock, §2), **ignored with warning** (key removed, warning in `details[]`), **rejected with error** (deployment blocked at validation, stable code §11). An unknown key not listed is **ignored with warning** (`compose_key_ignored`) **(proposed default)**; `x-*` keys are silently ignored in accordance with the Compose Specification, except `x-akerdock` (§5).

### 1.2 Top-level keys

| Key | Treatment | Detail |
|---|---|---|
| `services` | supported | Core of the model; each service becomes a `service_component` (data dictionary §9.2) |
| `networks` | transformed | Enforced isolated stack network (§2.1); additional internal networks created with a prefix; `external: true` subject to policy (§1.4) |
| `volumes` | transformed | Names prefixed with the stack UUID (§2.4); `external: true` subject to policy |
| `configs` | supported | Materialized as managed file mounts (PRD §8); `external: true` rejected (`compose_external_object_rejected`) |
| `secrets` | transformed | `file:` materialized as a `0600` file mount **(proposed default)**; `external: true` (Swarm) rejected (`compose_swarm_key_rejected`) |
| `name` | ignored with warning | The project name is enforced: stack UUID (INV-011) |
| `version` | ignored with warning | Obsolete (see §1.1) |
| `include` | rejected with error | `compose_include_rejected` — path traversal surface; one stack = one file |
| `x-akerdock` | supported | AkerDock extensions (§5) |
| other `x-*` | ignored (no warning) | Compliant with the Compose Specification |

### 1.3 `services.<name>.*` keys — supported and transformed

| Key | Treatment | Detail |
|---|---|---|
| `image` | supported | OCI digest resolved at pull time (PRD §18.3) |
| `build` (`context`, `dockerfile`, `args`, `target`, `additional_contexts`) | transformed | Context resolved inside the clone (`base_directory`), paths validated against traversal (PRD §23.3); build via the engine's `building` flow (deployment-engine §5.3.2), §2.3 labels injected; `build.secrets` → BuildKit build secrets |
| `command`, `entrypoint` | supported | — |
| `environment`, `env_file` | transformed | Merged with the stack variables (§3); `env_file` resolved inside the clone, anti-traversal |
| `ports` | supported | Host mapping = “Ports Mappings” (PRD §5.3); makes the service **ineligible for zero-downtime** (§8.4) |
| `expose` | supported | Documents the internal port; used as the default routing port (§6) |
| `volumes` | transformed | Prefixing, storage extensions, bind mount validation (§2.4, §5) |
| `labels` | transformed | Merged after the system labels; reserved `AkerDock.` prefix → `compose_reserved_label` (deployment-engine §6.2) |
| `container_name` | ignored with warning | `compose_container_name_ignored` — naming is enforced (§2.2, INV-011) |
| `hostname` | supported | Does not change the injected DNS aliases (§2.1) |
| `networks` | transformed | Mapped onto the stack network + prefixed additional networks; `aliases` preserved |
| `depends_on` | transformed | Rewritten into an ordering plan (§2.6) |
| `healthcheck` | transformed | Mapped onto `docker create` flags (§7) |
| `restart` | transformed | Defaults to `unless-stopped` when absent **(proposed default)**; `no` honored (one-shot jobs, §7.3) |
| `deploy.resources.limits` / `deploy.resources.reservations` | transformed | **Actually enforced** via cgroups (decision §27.15, §8.5) |
| `mem_limit`, `mem_reservation`, `memswap_limit`, `cpus`, `cpu_shares`, `cpuset` (legacy) | transformed | Normalized to the same flags as `deploy.resources`; conflict between the two forms → `compose_conflicting_limits` |
| `stop_grace_period`, `stop_signal` | supported | `stop_grace_period` → `--stop-timeout` |
| `user`, `working_dir`, `init`, `tty`, `stdin_open` | supported | — |
| `read_only`, `tmpfs`, `shm_size` | supported | — |
| `ulimits`, `group_add` | supported | — |
| `dns`, `dns_search`, `extra_hosts` | supported | — |
| `platform`, `pull_policy`, `profiles` | supported | Services outside active profiles are not deployed |
| `logging` | supported | Driver and options bounded by instance policy **(proposed default)** |
| `extends` | supported | Resolved at parse time; `file:` limited to the clone, anti-traversal |
| `env_file` outside the repository, absolute path | rejected with error | `compose_path_traversal` |

### 1.4 Policy-gated keys (denied by default)

These keys elevate the container's privileges on the server. They are **denied by default** (`compose_privileged_denied`) and can be enabled by an **explicit per-server policy** reserved to admins **(proposed default)** — consistent with the centralized validation of Docker options (PRD §23.3, INV-012).

| Key | Policy |
|---|---|
| `privileged: true` | Denied by default |
| `cap_add` | Default allowlist: `NET_BIND_SERVICE`, `CHOWN`, `SETUID`, `SETGID` **(proposed default)**; beyond that, server policy |
| `cap_drop` | Always allowed (privilege reduction) |
| `devices` | Denied by default |
| `security_opt` | Denied by default, except `no-new-privileges:true` always allowed **(proposed default)** |
| `sysctls` | Network allowlist (unprivileged `net.*`) **(proposed default)**; the rest under policy |
| bind mount outside allowed roots (including `/var/run/docker.sock`, `/`, `/etc`, `/var/lib/akerdock`) | Denied by default (`compose_bind_mount_denied`); allowed roots configurable per server **(proposed default)** |
| `networks.*.external: true`, `volumes.*.external: true` | Denied by default (`compose_external_object_rejected`); allowable by policy (unmanaged objects, INV-015: never touched by cleanup) |

### 1.5 Keys rejected with error

| Key | Code | Reason |
|---|---|---|
| `deploy.replicas`, `deploy.mode`, `deploy.placement`, `deploy.update_config`, `deploy.rollback_config`, `deploy.endpoint_mode`, `deploy.labels` | `compose_swarm_key_rejected` | Swarm semantics — not reimplemented (decision §27.4, ADR-004) |
| `network_mode: host` | `compose_network_mode_host_rejected` | Breaks per-stack network isolation and proxy routing |
| `network_mode: service:*` / `container:*` | `compose_network_mode_rejected` | Incompatible with container replacement (zero-downtime) |
| `pid: host`, `ipc: host`, `userns_mode: host`, `cgroup_parent`, `cgroup: host` | `compose_host_namespace_rejected` | Isolation escape |
| `external_links` | `compose_swarm_key_rejected` | Legacy, bypasses the destinations model |
| Top-level `secrets` with `external: true` | `compose_swarm_key_rejected` | Swarm secrets |
| `scale` | `compose_swarm_key_rejected` | One instance per service (multi-instance = P3, PRD §3.3) |
| `credential_spec`, `isolation` | `compose_platform_unsupported` | Windows only |

`links` (legacy) is **ignored with warning** (`compose_key_ignored`): the isolated network's DNS covers the need.

---

## 2. Applied transformations

All transformations are **deterministic**: the same compose + the same configuration snapshot produce exactly the same plan (INV-011, INV-014). The transformed compose (canonical form) is traced in the deployment logs.

### 2.1 Network

- Each stack receives an **isolated bridge network named after the stack UUID** (`resources.uuid`, PRD §9): `docker network create --label <labels §2.3> <stack_uuid>` — idempotent creation.
- All services of the stack are attached to it with **two DNS aliases**: the compose service name (`<service>` — inter-service references in the file work without rewriting) and `<stack_uuid>-<service>` **(proposed default)**.
- **Connection to the predefined network** (`services.connect_to_predefined_network`, PRD §9): each service is additionally attached to the `Destination`'s network, with **only the alias `<stack_uuid>-<service>`** (never the short alias: two stacks with a `db` service would collide) **(proposed default)**.
- Additional networks declared in the file become stack-internal networks, named `<stack_uuid>_<network_name>`.

### 2.2 Container naming

| Object | Name |
|---|---|
| Container of a service | `<stack_uuid>-<service>` |
| Zero-downtime candidate (§8) | `<stack_uuid>-<service>-next` |
| Preview container (PRD §20.4) | `<preview_uuid>-<service>` — `previews.uuid` is the base of the Docker names of the preview instance (data dictionary §8.9) |
| Locally built image | `AkerDock/<stack_uuid>-<service>:<sha12>` **(proposed default)**, §2.3 labels |

`<service>` is the compose service name, validated against `[a-z0-9][a-z0-9_.-]*` (`compose_invalid_service_name` otherwise). User-provided `container_name` is ignored (§1.3): names stay deterministic and derived from stable UUIDs (INV-011).

### 2.3 Injected system labels

Aligned **exactly** with the deployment-engine §6.2 table, with one additional label `akerdock.component`. Applied to the stack's containers, images, volumes and networks:

| Label | Value | Role |
|---|---|---|
| `akerdock.managed` | `true` | Managed / unmanaged boundary (INV-015) |
| `akerdock.resource_uuid` | Stack UUID (the `service` resource) | Attachment to the model |
| `akerdock.type` | `service` (`application` for the compose build pack of an application) | Typing |
| `akerdock.team_uuid` | Team UUID | Isolation, audit |
| `akerdock.deployment_uuid` | Deployment UUID | Idempotence of resumptions (deployment-engine §2.5) — containers and images |
| `akerdock.commit_sha` | Full SHA | Traceability — images built from Git |
| `akerdock.retain` | `true` | Cleanup protection of rollback images (deployment-engine §8.2) |
| `akerdock.component` | Compose service name (= `service_components.name`) | Attachment to the sub-container; absent from the stack's shared objects (network) |

User labels (`services.<name>.labels`) are added **afterwards** and cannot override the `AkerDock.` prefix (reserved, `compose_reserved_label`).

### 2.4 Volumes

- Named volume `<vol>` → **`<stack_uuid>_<vol>`** (anti-collision, PRD §8), created with the §2.3 labels: `docker volume create --label … <stack_uuid>_<vol>`. References in all services are rewritten consistently (a volume shared between services of the stack stays shared).
- Bind mounts: relative paths resolved from the clone (compose build pack of an application) or from `/var/lib/akerdock/services/<stack_uuid>/` **(proposed default)**; absolute paths subject to the §1.4 policy; anti path traversal (PRD §23.3).
- File mounts and storage extensions: §5.
- Every declared volume/bind/file is synchronized into `persistent_storages` (data dictionary §8.7) for the UI and the deletion workflow (PRD §20.6).

### 2.5 Restart policy

- `restart` absent → **`unless-stopped`** injected **(proposed default)** (parity with the application container, deployment-engine §5.3.4).
- `restart: no` preserved: it is the marker of one-shot jobs (migrations, seeds) — see `exclude_from_hc` (§7.3).
- `restart: always` and `on-failure[:n]` preserved as-is.

### 2.6 `depends_on` rewriting

`depends_on` is not passed to Docker (containers are created individually by the engine): it is compiled into an **ordering plan**:

- Short form → `condition: service_started`; long form preserved (`service_started`, `service_healthy`, `service_completed_successfully`).
- Startup order = **topological sort**; cycle → `compose_dependency_cycle`.
- `service_healthy` requires a healthcheck on the dependency (`compose_dependency_needs_healthcheck` otherwise).
- `service_completed_successfully`: the dependency must be a one-shot job (`restart: no`); dependents' startup waits for its exit code 0.
- During a zero-downtime redeployment (§8), `depends_on` orders the **replacement** of services but never restarts a dependent whose image and configuration have not changed **(proposed default)**.

### 2.7 Injected environment variables

Each container receives via `--env-file` (never in argv, INV-012; per-stack `env/` files, deployment-engine §5.1–5.2): the resolved stack variables (§3), the magic variables (§4) and the relevant predefined `AKERDOCK_*` variables (decision §27.22): `AKERDOCK_RESOURCE_UUID`, `AKERDOCK_CONTAINER_NAME` (= `<stack_uuid>-<service>`), `AKERDOCK_FQDN`/`AKERDOCK_URL` (components with a domain), `AKERDOCK_BRANCH`/`SOURCE_COMMIT` (compose from Git), `AKERDOCK_PR_ID` (previews).

---

## 3. Variables and interpolation

### 3.1 Interpolation syntax

Interpolation compliant with the Compose Specification, applied **on the control plane side before enqueue** (deployment-engine §5.2: a failure blocks at validation, not mid-build):

| Syntax | Behavior |
|---|---|
| `${VAR}` / `$VAR` | Resolved value; empty if undefined, with warning `compose_variable_undefined` **(proposed default)** |
| `${VAR:-def}` / `${VAR-def}` | Default if empty-or-undefined / if undefined |
| `${VAR:?msg}` / `${VAR:?}` | **Required variable**: blocks the deployment if empty or undefined (`compose_required_variable_missing`, PRD §5.4) |
| `${VAR:+alt}` | Alternative value if defined |
| `$$` | Literal `$` (no interpolation) |

### 3.2 Resolution order for a variable name

From most specific to most general — the first definition wins:

1. **Stack variables** (the resource's `environment_variables`, the `is_preview` set in preview context — never production secrets in a preview, INV-010); the magic variables (§4) are part of them (`is_generated`).
2. **Shared variables of the environment** (scope `environment`).
3. **Shared variables of the target server** (scope `server`, PRD §3.1) **(proposed default for the position: between environment and project)**.
4. **Shared variables of the project** (scope `project`).
5. **Shared variables of the team** (scope `team`).

In addition, the explicit syntax **`{{team.VAR}}` / `{{project.VAR}}` / `{{environment.VAR}}`** (PRD §5.4) directly references a specific scope inside a stack variable **value**; it is resolved at the same time, short-circuits the order above, and an unresolvable reference is an error (`compose_shared_variable_missing`) **(proposed default)**.

### 3.3 Variable types

Carried by `environment_variables` (data dictionary §8.5):

| Type | Effect in the compose pipeline |
|---|---|
| `is_multiline` | Value written with safe quoting in the `env-file` (keys, certificates) |
| `is_literal` | **No interpolation** of the value: `${…}` and `{{…}}` are treated as text |
| `is_locked` | Masked and non-editable in the UI; used normally at runtime |
| `is_build_time` | Available at build time (`build.args` / BuildKit), stored outside the image |
| `is_secret` | UI/API masking without `read:sensitive`; redaction in logs (INV-003) |

---

## 4. Magic variables `SERVICE_<TYPE>_<ID>` — full specification

Syntax preserved as-is (decision §27.22, ADR-022: functional, not tied to the brand).

### 4.1 Resolution of the `<ID>` identifier

- `<ID>` is a `[A-Z0-9_]+` token. Convention: compose service name uppercased, non-alphanumeric characters replaced with `_` (e.g. service `open-webui` → `OPEN_WEBUI`).
- **The same `<ID>` denotes the same value throughout the stack**: two services referencing `SERVICE_PASSWORD_DB` receive the same value (PRD §5.4, §9).
- For `URL` and `FQDN`, `<ID>` must match a component of the stack (normalized name); otherwise `compose_magic_variable_unknown_component`.

### 4.2 Types and exact generation rules

All alphabets and lengths are **(proposed default)**, aligned with the observed behavior of the reference; generator: CSPRNG.

| Type | Generated value | Alphabet | Length |
|---|---|---|---|
| `SERVICE_FQDN_<ID>` | FQDN of component `<ID>`, **without scheme** (e.g. `app.example.com`); generated from the server wildcard / sslip.io if no domain (PRD §4.2) | — | — |
| `SERVICE_FQDN_<ID>_<PORT>` | Like `FQDN`, binding the domain's routing to the internal port `<PORT>` (equivalent to `domain:port`, PRD §4.2) | — | — |
| `SERVICE_URL_<ID>` (+ `_<PORT>` variant) | Full URL with scheme (`https://` if TLS is active, otherwise `http://`) | — | — |
| `SERVICE_USER_<ID>` | Random username | `[a-z]` first character, then `[a-z0-9]` | 16 |
| `SERVICE_PASSWORD_<ID>` | Password without symbols | `[A-Za-z0-9]` | 32 |
| `SERVICE_PASSWORD_64_<ID>` | Same | `[A-Za-z0-9]` | 64 |
| `SERVICE_PASSWORDWITHSYMBOLS_<ID>` | Password with symbols | `[A-Za-z0-9]` + `!@#$%^&*()-_=+[]{}<>~` | 32 |
| `SERVICE_PASSWORDWITHSYMBOLS_64_<ID>` | Same | same | 64 |
| `SERVICE_BASE64_32_<ID>` / `_64_` / `_128_` | Random “base64-like” string (not an encoding) | `[A-Za-z0-9]` | 32 / 64 / 128 characters |
| `SERVICE_REALBASE64_32_<ID>` / `_64_` / `_128_` | **True base64 encoding** of N random bytes: `base64(random_bytes(N))`, padding preserved | Standard Base64 | N = 32 / 64 / 128 bytes |
| `SERVICE_HEX_32_<ID>` / `_64_` / `_128_` | Hexadecimal encoding of N/2 random bytes → **N hex characters** | `[0-9a-f]` | 32 / 64 / 128 characters |

`SERVICE_<TYPE>` without an explicit length where a length exists (`PASSWORD`, `PASSWORDWITHSYMBOLS`) = short variant (32). An unknown type → `compose_magic_variable_invalid_type`.

### 4.3 Lifecycle

- **Generation**: at the first save/deployment of the compose that references the variable. Written into `environment_variables` with `is_generated = true`, `is_secret = true` for credential types **(proposed default)**.
- **Persistence**: the value is stable across redeployments (PRD §5.4) — never regenerated as long as the row exists.
- **Sharing**: scope = the stack (the resource); all services access it under the same name.
- **UI editing**: editable like any variable (PRD §5.4); editing does not break the `is_generated` link (the variable is not regenerated afterwards) **(proposed default)**.
- **Removal from the compose**: a magic variable no longer referenced is kept but flagged in the UI as orphaned; explicit manual deletion **(proposed default)** — never a silent loss of a credential still used by existing data.
- **Preview (PRD §20.4)**: each preview generates **its own instance** of each magic variable (new value per preview identity, stored in the `is_preview` set attached to the preview); `SERVICE_FQDN_*`/`SERVICE_URL_*` resolve to the preview URL (`SERVICE_URL_<ID>` = preview URL, PRD §5.6). Destroying the preview = destroying its variable instances.

---

## 5. AkerDock extensions (`x-akerdock`)

Dedicated namespace `x-akerdock` **(proposed default)**, compliant with the Compose Specification's extension mechanism (`x-*` keys are legal at any level). Extensions written **outside** this namespace (unprefixed keys at service level, as produced by some third-party platforms) are **not** interpreted: they follow the general rule of §1.3 — unknown key, removed with warning `compose_key_ignored`. A key that has no effect must say so; two spellings for the same extension would be a divergence waiting to happen. Rewriting to `x-akerdock` is done **at import time** into the template repository (ADR-010, ADR-022), once, not at every deployment.

### 5.1 Supported extensions

| Need (PRD §5.2, §8) | `x-akerdock` key (proposed default) | Level | Effect |
|---|---|---|---|
| `is_directory: true` (volume entry) | `x-akerdock.is_directory: true` | `volumes` entry (long form) | Creation of the host directory before mounting (`persistent_storages.is_directory`) |
| `content: \|` (volume entry) | `x-akerdock.content: \|` | `volumes` entry (long form, `type: bind` file) | Creation of the host file with **variable interpolation** (§3); content editable in the UI (≤ 5 MiB, PRD §23.3); optional `mode`/`uid`/`gid` via `x-akerdock.file_mode`, `x-akerdock.owner_uid`, `x-akerdock.group_gid` |
| `exclude_from_hc: true` (service) | `x-akerdock.exclude_from_hc: true` | `services.<name>` | Exclusion from the stack's aggregated health check (§7.3, `service_components.exclude_from_hc`) |
| Stack that cannot tolerate two simultaneous instances | `x-akerdock.zero_downtime: false` | `services.<name>` | Disables the two-instance replacement for this service (ADR-015) |
| Pre-deployment hook | `x-akerdock.pre_deployment_command: <cmd>` | `services.<name>` | Executed in the service's **existing** container, before any build and any mutation (deployment-engine §10); skipped if no container is running |
| Post-deployment hook | `x-akerdock.post_deployment_command: <cmd>` | `services.<name>` | Executed in the service's healthy **candidate**, before its switchover — a failure removes the candidate and the old one stays routed (C2, INV-005). Requires a routed, zero-downtime-eligible service: otherwise refused at deployment **before any mutation**; refused at validation on a one-shot (`compose_hook_on_one_shot`), warned when no healthcheck is declared (`compose_hook_without_healthcheck`) |
| Template metadata in comments | `x-akerdock.template` (top-level) | Top-level | §12 |
| Seeding a preview by cloning the production volume | `x-akerdock.preview_seed: clone` | `volumes.<name>` (top-level declaration) | **ADR-029.** When deploying a **preview**, the preview's still-empty volume is initialized by copying (`cp -a`, source read-only) the production volume `<uuid-app>_<name>`, in an ephemeral container of the service's image, before its first start. A non-empty volume is never touched; missing production volume → seed skipped; copy failure → preview deployment failure. Refused on an `external:` volume and in raw mode (`compose_preview_seed_invalid`). Only accepted value: `clone`. |

Example:

```yaml
services:
  app:
    image: ghcr.io/example/app:1.4
    volumes:
      - type: bind
        source: ./config/app.ini
        target: /etc/app/app.ini
        x-akerdock:
          content: |
            [server]
            url = ${SERVICE_URL_APP}
      - type: bind
        source: ./data
        target: /var/lib/app
        x-akerdock:
          is_directory: true
  migrate:
    image: ghcr.io/example/app:1.4
    command: ["app", "migrate"]
    restart: "no"
    x-akerdock:
      exclude_from_hc: true
```

`x-akerdock.content` and `x-akerdock.is_directory` are mutually exclusive on the same entry (`compose_storage_extension_conflict`).

---

## 6. Domains and routing

- **One domain per service**: each `service_component` may carry zero, one or several domains (`domains.service_component_id`, data dictionary §8.4) — FQDN + path + optional `target_port` (`domain:port`, PRD §4.2).
- **Port per service**: the routing target port is, in order: the domain's `target_port` → the service's first `expose` → the template's port (`x-akerdock.template.port`, §12) → error `compose_routable_port_unresolved` if the service has a domain but no determinable port **(proposed default)**.
- **Services without a domain = private**: no proxy configuration generated, no published port; reachable only through the internal DNS of the stack network (aliases §2.1) — PRD §9 parity.
- **Application-level domains** (the UI's Routing field): on a compose stack, they are resolved to the stack's **web service**, deterministically — the service whose first `expose` matches the domain's `target_port` (`fqdn:port`), otherwise the **single** service exposing a port. No exposed service → `compose_routable_port_unresolved`; several candidates with no discriminating `target_port` → `compose_routable_component_ambiguous`. Never a guessed container: an application domain never targets the stack name (that container does not exist in compose).
- Generation: each component with a domain produces an entry in the **proxy intermediate representation** (decision §27.9); Traefik/Caddy materialization, path-based priorities, www redirect, certificates: see the **proxy contract (§29.6, upcoming)**. Dynamic file per resource: `/var/lib/akerdock/proxy/dynamic/<stack_uuid>.yaml`, sections per component **(proposed default)**.
- `SERVICE_FQDN_<ID>` referenced in the compose counts as a declaration of domain intent: if the component has no configured domain, a domain is generated from the server wildcard (sslip.io fallback, PRD §4.2) at the first deployment.

---

## 7. Health checks and `exclude_from_hc`

### 7.1 Compose ↔ AkerDock mapping

The compose file is the source of truth (PRD §5.2): `services.<name>.healthcheck` takes precedence.

| Compose key | `docker create` flag | Default when a healthcheck is present without the key |
|---|---|---|
| `test` | `--health-cmd` | — (required) |
| `interval` | `--health-interval` | `30s` (Compose spec) |
| `timeout` | `--health-timeout` | `30s` |
| `retries` | `--health-retries` | `3` |
| `start_period` | `--health-start-period` | `0s` |
| `start_interval` | `--health-start-interval` | `5s` |
| `disable: true` | `--no-healthcheck` | — |

Priority order per service: compose `healthcheck` > image `HEALTHCHECK` > the resource's UI health check (applied to the routed component, generated as an HTTP `--health-cmd` like deployment-engine §5.3.4) **(proposed default)**. A web service with none of the three is ineligible for zero-downtime (§8.4).

### 7.2 Aggregated stack status

Status observed per component (`service_components.observed_status`); the stack status is the aggregate of the **non-excluded** components: `healthy` if all healthy, `unhealthy` if at least one degraded, etc. **(proposed default)**.

### 7.3 One-shot jobs and `exclude_from_hc`

- `x-akerdock.exclude_from_hc: true` excludes the component: from the aggregated status, from the deployment's `healthchecking` barrier, and from status-change notifications (PRD §9).
- A `restart: "no"` service **without** `exclude_from_hc` triggers the warning `compose_oneshot_without_exclude` suggesting the extension **(proposed default)**.
- During a deployment, an excluded one-shot job is launched according to the `depends_on` order; `service_completed_successfully` remains verifiable (§2.6); its exit ≠ 0 fails the deployment (`failed`, deterministic classification).

---

## 8. Zero-downtime compose (decision §27.15, ADR-015)

### 8.1 Service classification

- **Web service**: at least one domain (§6). **Two-instance replacement with per-service proxy switchover**.
- **Non-web service** (workers, databases, caches): **recreate** replacement (stop-then-start, deployment-engine §7.4), without interrupting the routing of web services.

### 8.2 Algorithm per stack deployment

Same queue, locks, slots and state machine as the engine (deployment-engine §2–4) — §3.1 lock at the stack level; the `starting`/`healthchecking`/`switching` states operate **per service**:

1. **Plan**: parse, transformations (§2), per-service diff (image, config, volumes); an unchanged service is not replaced **(proposed default)**.
2. **Build/pull** of all the stack's images (shared `cloning`/`building`/`pushing` states), digests resolved before any mutation.
3. Traversal in **topological order** (§2.6). For each modified **non-web service**: `docker stop -t <grace>` + `rm` of `<stack_uuid>-<service>`, creation of the new one under the same name, wait for `running`/`healthy` depending on healthcheck.
4. For each eligible **web service**: creation of the candidate **`<stack_uuid>-<service>-next`** on the same networks; wait for `healthy`; **proxy switchover of this component's routing only** (deployment-engine §7.2 algorithm: IR → dynamic file → atomic application → verification → graceful stop of the old one → `docker rename` → stabilization by DNS name); cancellation barrier active per switchover.
5. **One-shot jobs**: executed at their topological position (§7.3).
6. `finishing`: parity labels, protection of rollback images, `service_components` synchronization, asynchronous cleanup.

**Failure mid-plan**: the failing service follows the C2 compensation (candidate removed, old one intact and routed — INV-005/006); services **already switched over stay in place** (no implicit un-switch, C3); the deployment is `failed` with per-component detail — explicit partial state, resumption possible (PRD §20.8).

**Resumption after a crash** (deployment-engine §2.5, applied **per service**): a job resumed beyond `preparing` inspects each service before acting — a service with the right `akerdock.config_hash` and `running` is recognized as done; a **surviving healthy candidate** means a crash mid-switchover: its promotion is **completed**, never replayed (INV-004/005, step `resume_<svc>`); a dead or sick candidate is removed and the service redone from scratch (C2). Never blind replay.

### 8.3 Temporary coexistence

During the switchover of a web service, two instances coexist on the stack network; the short DNS alias `<service>` points at the old container until the rename **(proposed default)** — the other services never see the candidate before promotion. Stacks that cannot tolerate any coexistence disable the mechanism per service: `x-akerdock.zero_downtime: false` (§5.1, risk accepted ADR-015).

### 8.4 Ineligibility conditions (per service)

A web service is handled as recreate (with an assumed interruption, displayed as such) if:

- no resolved healthcheck (§7.1);
- host port mapping (`ports`) — two instances cannot bind the same port;
- `x-akerdock.zero_downtime: false`;
- raw compose mode (§9);
- named volume mounted read-write shared with its own instance that cannot tolerate two writers — not automatically detectable: documented, to be covered by `zero_downtime: false` (warning produced, PRD §8).

### 8.5 Resource limits actually enforced (decision §27.15)

`deploy.resources` and the legacy keys (§1.3) are translated into flags of the `docker create` of **every** container of the stack — never ignored:

| Compose key | Flag |
|---|---|
| `deploy.resources.limits.memory` / `mem_limit` | `--memory` |
| `deploy.resources.reservations.memory` / `mem_reservation` | `--memory-reservation` |
| `memswap_limit` | `--memory-swap` |
| `deploy.resources.limits.cpus` / `cpus` | `--cpus` |
| `cpu_shares` | `--cpu-shares` |
| `cpuset` | `--cpuset-cpus` |
| `deploy.resources.limits.pids` | `--pids-limit` |

Required proof: cgroup verification (`docker inspect` + `/sys/fs/cgroup`) in the E2E tests (PRD §26.2).

---

## 9. Raw compose mode

Advanced opt-in mode per resource (PRD §5.2): the file is applied as close as possible to the `docker compose up` semantics.

**Stays active (cannot be disabled)**:

- §2.3 management labels on all created objects (INV-015: without them, cleanup and adoption are blind);
- isolated stack network + project name enforced by UUID (INV-011);
- interpolation of variables and magic variables (§3–4);
- **rejected** keys §1.5 and policy §1.4 (security boundaries, non-negotiable);
- enforced resource limits (§8.5).

**Disabled**:

- container renaming (§2.2): standard Compose names `<stack_uuid>-<service>-1` via the project name;
- prefixing/rewriting of volumes and additional networks (§2.4) apart from labels;
- injection of the restart policy (§2.5) and `depends_on` rewriting (§2.6 — native compose semantics);
- storage extensions and per-component domain management (the user manages their own proxy labels);
- **zero-downtime** (§8): every redeployment is a `down`/`up` of the stack, interruption assumed and displayed;
- fine-grained `service_components` synchronization (statuses limited to running/exited) **(proposed default)**.

---

## 10. Backups of internal databases (PRD §7.1)

- At each compose synchronization, each service is classified by **image detection**: the image name (basename, registry and namespace ignored) is compared against `postgres`/`postgresql`, `mysql`, `mariadb`, `mongo`/`mongodb`, including the common variants `bitnami/postgresql`, `pgvector/pgvector`, `supabase/postgres`, `percona`, `mongodb/mongodb-community-server` **(proposed default, list maintained with the catalog)**.
- The result is carried by `service_components.is_database` + `database_engine` (data dictionary §9.2): the component becomes a valid target of a `database_backup_plan` (`service_component_id`, §9.5) with the same engines/tools as managed databases (`pg_dump`, `mysqldump`, `mariadb-dump`, `mongodump --gzip`). **v1 scope: PostgreSQL only**, aligned with the “PostgreSQL only” decision of managed databases — a component of another engine is refused with a `422` (`engine_not_supported`) at plan creation, never accepted and then failing at the first backup. Contract: `/service-components/{uuid}/backups[...]` operations, exact mirror of the database operations (execution, confirmed restore, scheduled drills included).
- Credentials are read from the component's resolved variables (including magic variables `SERVICE_USER_*`/`SERVICE_PASSWORD_*`) **(proposed default)**; never logged (INV-003).
- An unrecognized image remains backupable via the out-of-scope mechanisms (volumes, §27.14).

---

## 11. Validation — errors and warnings with stable codes

Stable codes consumable by the API in `details[]` (PRD §24.1: `code`, `message`, `details`). Severity `error` = deployment/save blocked; `warning` = accepted, traced and displayed. Normative list (extensible per API version):

| Code | Severity | Case |
|---|---|---|
| `compose_parse_error` | error | Invalid YAML or non-compliant with the Compose Specification schema (position included) |
| `compose_version_ignored` | warning | `version:` key present (§1.1) |
| `compose_key_ignored` | warning | Unsupported key removed (§1.2–1.3, `links`…) |
| `compose_container_name_ignored` | warning | `container_name` removed (§2.2) |
| `compose_swarm_key_rejected` | error | Swarm key (§1.5) |
| `compose_network_mode_host_rejected` | error | `network_mode: host` |
| `compose_network_mode_rejected` | error | `network_mode: service:*` / `container:*` |
| `compose_host_namespace_rejected` | error | `pid`/`ipc`/`userns_mode`/`cgroup` host, `cgroup_parent` |
| `compose_privileged_denied` | error | `privileged`, `cap_add`, `devices`, `security_opt`, `sysctls` outside server policy (§1.4) |
| `compose_bind_mount_denied` | error | Bind mount outside allowed roots (including `docker.sock`) |
| `compose_external_object_rejected` | error | `external: true` (network/volume/config) outside policy |
| `compose_include_rejected` | error | `include` key |
| `compose_platform_unsupported` | error | `credential_spec`, `isolation` |
| `compose_invalid_service_name` | error | Service name outside `[a-z0-9][a-z0-9_.-]*` |
| `compose_reserved_label` | error | User label prefixed with `AkerDock.` |
| `compose_path_traversal` | error | `env_file`, `build.context`, `extends.file`, relative bind escaping the clone (PRD §23.3) |
| `compose_conflicting_limits` | error | Contradictory `deploy.resources` and legacy keys (§1.3) |
| `compose_dependency_cycle` | error | Cycle in `depends_on` |
| `compose_dependency_needs_healthcheck` | error | `service_healthy` towards a service without a healthcheck |
| `compose_required_variable_missing` | error | `${VAR:?}` empty or undefined (§3.1) |
| `compose_variable_undefined` | warning | `${VAR}` undefined without a default |
| `compose_shared_variable_missing` | error | `{{team.VAR}}`/`{{project.VAR}}`/`{{environment.VAR}}` not found |
| `compose_magic_variable_invalid_type` | error | `SERVICE_<TYPE>_…` unknown type (§4.2) |
| `compose_magic_variable_unknown_component` | error | `SERVICE_FQDN/URL_<ID>` without a matching component |
| `compose_storage_extension_conflict` | error | `content` + `is_directory` on the same entry (§5.1) |
| `compose_routable_port_unresolved` | error | Domain without a determinable target port (§6) |
| `compose_domain_conflict` | error | Violation of the global `UNIQUE (fqdn, path)` (data dictionary §8.4) |
| `compose_oneshot_without_exclude` | warning | `restart: no` without `exclude_from_hc` (§7.3) |
| `compose_zero_downtime_ineligible` | warning | Web service handled as recreate, reason included (§8.4) |
| `compose_hook_on_one_shot` | error | `post_deployment_command` on a `restart: no` service — no candidate to run it in (deployment-engine §10) |
| `compose_hook_without_healthcheck` | warning | `post_deployment_command` without a declared healthcheck — refused at deployment if the image does not provide one either |
| `compose_file_content_too_large` | error | `x-akerdock.content` > 5 MiB (PRD §23.3) |

Each `details[]` entry carries: `code`, `severity`, `service` (name of the affected service, if applicable), `path` (YAML path, e.g. `services.app.deploy.replicas`), `message` (generic, never a secret — INV-003).

---

## 12. One-click templates (§9, §27.10, ADR-010)

### 12.1 Anatomy of an AkerDock template

A template = a compose file valid in the sense of this spec + a top-level metadata block **(proposed default)**:

```yaml
x-akerdock:
  template:
    slug: umami            # required, unique in the repository, [a-z0-9-]+
    name: Umami            # required, displayed name
    documentation: https://umami.is/docs   # required (PRD §9)
    slogan: Simple, privacy-focused analytics   # required
    category: analytics    # required, controlled vocabulary of the catalog
    tags: [analytics, privacy]   # optional
    logo: svgs/umami.svg   # required, path relative to the repository (SVG/PNG, ≤ 128 KiB (proposed default))
    port: 3000             # required if a service must be exposed: default routing port (§6)
    min_akerdock_version: "1.2"   # optional
services:
  umami: …
```

Metadata carried in **comments** (`# documentation:`, `# slogan:`, `# category:`, `# tags:`, `# logo:`, `# port:`), a widespread format in the compose catalog ecosystem, are recognized at import and rewritten to `x-akerdock.template` — same pipeline as the rewriting of another platform's predefined variables (ADR-022) **(proposed default)**.

### 12.2 Compilation of the catalog into signed JSON (§27.10)

- The official repository is compiled into a **catalog JSON artifact**: `{ schema_version, generated_at, source_commit, templates: [{ slug, version (SHA-256 checksum of the canonical compose), metadata, compose (canonical content), logo_data_uri }] }` **(proposed default)**.
- The artifact is **signed** (detached **Ed25519 (proposed default)** signature); the project's public key is embedded in the binary; the instance **verifies the signature before any refresh** of the catalog — an unverifiable catalog is refused and the old one stays in service.
- Refreshable independently of the binary's releases (ADR-010); `template_slug`/`template_version`/`template_repository` are frozen on the resource at instantiation (data dictionary §9.1).
- CI of the official repository: each template passes the §12.3 validation + a smoke deployment **(proposed default)**.

### 12.3 Validation at import of a user template repository

Team Git repositories (public/private via the existing keys/credentials, INV-002), **validation at import and at each resynchronization**; user templates are **not signed** by the project (team responsibility, risk accepted ADR-010). Rules, each reported per template with the §11 codes plus:

| Rule | Code | Severity |
|---|---|---|
| Compose compliant with the present spec (§1, §11) — a rejected template does not enter the team's catalog, the others do | (§11 codes) | error |
| Required metadata present and valid (`slug`, `name`, `documentation`, `slogan`, `category`, `logo`; `port` if a service is exposed) | `template_metadata_missing` | error |
| `slug` unique in the repository | `template_slug_conflict` | error |
| Logo present, SVG/PNG format, size ≤ 128 KiB | `template_logo_invalid` | error |
| Predefined variables of another platform detected (foreign prefix) | `template_foreign_variables` | error at official import (mandatory rewriting to `AKERDOCK_*`, ADR-022); warning in a user repository **(proposed default)** |
| Consistent magic variables (§4: valid types, `FQDN`/`URL` pointing at a service of the template) | `compose_magic_variable_*` | error |
| Image without a tag or `:latest` | `template_unpinned_image` | warning **(proposed default)** |
| Policy-gated keys (§1.4) used | `template_requires_policy` | warning (flagged before instantiation: the deployment will fail if the server policy does not allow it) |

Instantiating a template creates an ordinary `service` resource: the compose becomes `services.compose_content`, editable in the UI, entirely subject to sections 1 to 11.

---

## 13. PRD traceability

| Section of this spec | PRD sections / specs |
|---|---|
| 1 | §5.2, §22.4, §23.3, §27.4 (ADR-004), INV-012, INV-015 |
| 2 | §2, §5.3, §8, §9, INV-011, INV-014; deployment-engine §5.1–5.3, §6 |
| 3 | §5.4, §3.1, §5.6, INV-003, INV-010; data-dictionary §8.5–8.6; deployment-engine §5.2 |
| 4 | §5.4, §9, §20.4, §27.22 (ADR-022); data-dictionary §8.5, §8.9 |
| 5 | §5.2, §8, §27.10/§27.22 (ADR-010/022); data-dictionary §8.7, §9.2 |
| 6 | §4.2, §9, §27.9 (ADR-009, proxy contract §29.6); data-dictionary §8.4 |
| 7 | §5.3, §9; data-dictionary §8.8, §9.2; deployment-engine §5.3.4 |
| 8 | §15, §27.15 (ADR-015), §20.8, INV-005/006; deployment-engine §2–4, §7 |
| 9 | §5.2, INV-011, INV-015 |
| 10 | §7.1; data-dictionary §9.2, §9.5 |
| 11 | §24.1, §23.3, INV-003 |
| 12 | §9, §27.10 (ADR-010), §29.11; data-dictionary §9.1 |
