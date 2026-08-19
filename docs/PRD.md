# PRD — AkerDock

> Product specification for AkerDock, a self-hosted PaaS in Go. This document defines the product, its guarantees and its acceptance criteria.

### Status and reading convention

- **Two kinds of sections**: sections 1 through 14 describe the product's **functional scope** (what it does); sections 16 and onward derive **verifiable requirements** from it (how we prove that it does it).
- **Normative words**: **MUST**, **MUST NOT**, **SHOULD** and **MAY** have their usual meaning as requirement levels.
- **Goal**: behaviors, guarantees and acceptance criteria. Implementation choices remain substitutable as long as these are respected.
- **Traceability**: every structuring decision is recorded in an ADR (`docs/adr/`), and its delivery status in the §26 matrix.

---

## 1. Product vision

**AkerDock** is an open source self-hosted PaaS (Apache 2.0 license): a self-hosted alternative to Heroku / Netlify / Vercel. The user connects their own servers (VPS, bare metal, Raspberry Pi…) via SSH and deploys applications, databases and services in Docker containers, with reverse proxy, automatic SSL, backups and monitoring — without vendor lock-in.

**Value proposition:**
- Deploy any app (Git or Docker image) on any Linux server in a few clicks.
- No paywalled feature: everything the product does is in the binary you host.
- Everything is standard Docker: no proprietary format, reversible at any time — resources remain usable without AkerDock.

**Technical stack:** single Go binary (control plane, API, UI, workers), PostgreSQL as the only external dependency (state **and** queue — ADR-002), Angular UI served on the control plane's single port, target servers driven via SSH, distribution as two compose services (ADR-021).

---

## 2. Concepts and data model

Organizational hierarchy:

```
Team → Project → Environment (production, staging…) → Resource
Resource = Application | Database | Service (one-click)
Resource ⟶ deployed on → Server + Destination (target Docker network)
```

- **Team**: isolation boundary (servers, resources, API tokens, notifications are scoped per team). Multi-team supported.
- **Project**: logical grouping; contains environments (default: `production`).
- **Environment**: a set of resources + shared variables.
- **Server**: Linux machine reachable over SSH, with its own proxy.
- **Destination**: Docker network on a server.
- Each resource has a **UUID** used as the container/network/internal hostname name.

---

## 3. Server management

### 3.1 Connection and validation
- Any Linux server reachable over SSH can be added (VPS, EC2, Raspberry Pi, local machine). **AMD64 and ARM64** architectures.
- Authentication **exclusively by SSH key** (no passphrase, no 2FA); private keys are stored encrypted in the instance ("Private Keys").
- Root user by default; **experimental non-root user** (requires `sudo NOPASSWD: ALL`).
- **Docker Engine ≥ 24** required (snap not supported).
- "Validate Server & Install Docker Engine" button: checks SSH connectivity, installs dependencies (curl, wget, git, jq), installs/configures Docker, runs health checks (3 retries per step).
- SSH connection timeout configurable per server; SSH username notably accepting dots.
- Pre-registered **localhost** server (the machine hosting the instance), usable but discouraged for production.
- Environment variables **shared at server level**, inheritable by the resources deployed on it.

### 3.2 System maintenance and cloud provisioning (removed — ADR-027)

Server patching (APT/DNF/Zypper from the dashboard), cloud provider tokens and Hetzner provisioning are **removed from the product scope** (ADR-027, re-assessable upon proven demand). Section numbering is kept for cross-reference stability. Unrelated and still in scope: Hetzner/Cloudflare as **DNS-01** providers (§4.3) and Hetzner as an **S3** provider (§7.2).

### 3.3 Multi-server
- Each server is independent with its own proxy; application traffic goes directly to the target server (never through the control instance). The instance only does UI + SSH deployments + health monitoring.
- **Multi-server deployment of the same app** (HA, experimental): same architecture required + external Docker registry (build → push → pull); external load balancer is the user's responsibility.

### 3.4 Build servers
- Server dedicated to compilation ("Build Server" flag) to offload production servers.
- Prerequisites: Docker Engine, access to source code, **mandatory push to a container registry**, same architecture as the deployment servers.
- Enabled per application ("Use a Build Server?"). Random selection if several build servers. A build server cannot host applications.

### 3.5 Docker Swarm (experimental, deprecated)
- Swarm Manager (mandatory) + workers; external registry mandatory; recommended minimum 3 nodes; multi-node persistent storage unresolved. Not production-ready and announced as deprecated for the next generation.

### 3.6 Cloudflare Tunnels (removed — ADR-027)
Removed from the product scope (ADR-027, re-assessable upon proven demand). Section numbering is kept for cross-reference stability. Cloudflare as a **DNS-01** provider (§4.3) is not affected.

### 3.7 Automated disk cleanup
- "Automated Docker Cleanup" per server: triggered by **disk usage threshold** (%) and/or **scheduled cron**; opt-in options to purge unused volumes and networks.
- Only targets managed resources (orphaned candidate containers, managed dangling images and volumes/networks) plus reconstructible build cache; never during an in-progress deployment on either the target or build server. The guard is atomic and a blocked cleanup is durably retried. Old helper images are reclaimed immediately after a successful helper replacement.

### 3.8 Server monitoring — Sentinel agent (experimental)
- Lightweight Go agent deployed as a container: server and per-container CPU/RAM (~10 s), disk (~60 s); **push** architecture toward the instance (endpoint + token); configurable retention and frequency; local REST API (`localhost:8888`).
- Historical graphs in the UI (server and per resource). Limitation: no metrics for Docker Compose stacks / one-click services.

---

## 4. Proxy, domains and SSL

### 4.1 Reverse proxy
- **Traefik** (default) and **Caddy** (experimental); switch possible per server at any time (label regeneration).
- **Automatic** configuration: the platform generates container routing — several apps per server without manual port management.
- Proxy config editable per server in the UI + Traefik dynamic config files (`/var/lib/akerdock/proxy/dynamic`).
- **Proxy lifecycle**: start / stop / restart of the proxy per server from the UI, visible status, proxy logs viewable; stopping the proxy cuts all inbound traffic of the server (explicit warning); notification if the proxy image is outdated (see §11).
- Capabilities via labels/middlewares: Basic Auth, rate limiting, IP whitelisting, custom headers, load balancing, Traefik dashboard.

### 4.2 Domains
- Formats supported per application: simple FQDN, **multi-domain** (commas), **domain:port** (routing to a specific internal port), **path-based routing** (most specific path takes priority).
- **Wildcard domain per server**: new apps automatically receive `<uuid>.example.com` (fallback: **sslip.io** domains).
- Native **www/non-www** redirect ("Direction": both / to-www / to-non-www).
- DNS validation via 1.1.1.1 (customizable validation DNS).
- **Keep out of search engines** option per application and per Compose stack: every domain of the resource answers `X-Robots-Tag: noindex, nofollow` (proxy-contract §4.7). Off by default — a production domain is often a site whose ranking is the point. Preview URLs are noindexed regardless (§20.4.4), and the control plane itself is never indexable (§14.2).

### 4.3 SSL/TLS
- **Automatic Let's Encrypt** certificates (issuance + renewal, HTTP-01 by default); fallback to self-signed certificate if issuance fails.
- **Wildcard certificates** via DNS-01 challenge (DNS providers supported by Lego: Cloudflare, Route 53, OVH, Hetzner…).
- **Custom certificates**: dropped into `/var/lib/akerdock/proxy/certs` + dynamic config.
- **Force HTTPS** option per application.

---

## 5. Applications

### 5.1 Deployment sources
| Source | Description |
|---|---|
| Public Git Repository | HTTPS URL of a public repo (GitHub, GitLab, Bitbucket, Gitea, others) |
| Private Repo — GitHub App | Official integration: repo discovery, auto-deploy on push, preview deployments, status comments on PRs; GitHub Enterprise supported |
| Private Repo — Deploy Key | SSH key (generated or imported) added as a deploy key; GitHub/GitLab/Bitbucket/Gitea; auto-deploy via manual webhooks |
| Dockerfile | Inline Dockerfile or from the repo |
| Docker Compose | Compose file from the repo as a multi-service definition |
| Docker Image | Pre-built image from a registry (Docker Hub, GHCR, GitLab Registry, custom); private registries via `docker login` on the server |

- Ancillary Git features: branch selection, **base directory** (monorepos), git **submodules**, git **LFS**, shallow clone.
- External CI pattern: build in GitHub Actions → push to registry → call the AkerDock deploy webhook (pull + redeploy without rebuild).

### 5.2 Build packs
| Build pack | Role |
|---|---|
| **Nixpacks** (default) | Language/framework auto-detection, Dockerfile generation; install/build/start override; `nixpacks.toml`; static mode (Nginx, publish directory) |
| **Railpack** (beta) | Successor to Nixpacks: smaller images, better BuildKit caching; supports regular, preview and static deployments |
| **Static** | Pre-built files served by **Nginx** (editable nginx config); SPA option |
| **Dockerfile** | Full control; auto-injected build args (can be disabled); opt-in `SOURCE_COMMIT` |
| **Docker Compose** | The compose file is the source of truth (env, storage, network); domain per service; isolated bridge network per UUID; `x-akerdock` extensions (`is_directory`, `content`, `exclude_from_hc`); advanced "raw compose" mode |

- **Post-build push to registry** (image + tag fields): required for Swarm and build servers; custom tag + commit SHA tag.

### 5.3 Application configuration
- **Ports**: "Ports Exposes" (internal port used by the proxy, optional for an application without inbound traffic) and "Ports Mappings" (optional host mapping, outside the proxy, TCP/UDP/SCTP protocols and IP binding supported).
- **Health checks**: path, port, method, interval/timeout/retries/start period (requires curl/wget in the container); Dockerfile `HEALTHCHECK` takes precedence; conditions Traefik routing and rolling updates.
- **Resource limits**: memory limit/reservation/swap/swappiness, CPU limit/sets/shares.
- **Custom Docker options**: arbitrary `docker run` options (`--cap-add`, `--gpus`, `--ulimit`…).
- **Custom labels**: editable container labels (proxy labels regenerable); injected system labels (`akerdock.managed=true`, `akerdock.resource_uuid`, `akerdock.type`).
- **Pre/post-deployment commands**: pre = in the existing container before deployment; post = in the new container after (post failure = failed deployment, without auto rollback).
- **Stop and restart**: configurable stop grace period and restart-loop cap.
- **Persistent storage**: see §8.

### 5.4 Environment variables
- **Build-time** flags (`ARG` / `--env-file`, stored outside the image) and **runtime** flags (`.env` + `env_file`) per variable.
- Optional **Docker Build Secrets** (BuildKit `--secret`) to avoid leaking secrets into image metadata.
- Special types: **multiline** (keys, certificates), **literal** (no interpolation), **locked** (masked, not re-editable).
- Two views: Normal (cards) and **Developer** (bulk `.env` editor).
- Hierarchical **shared variables**: `{{team.VAR}}`, `{{project.VAR}}`, `{{environment.VAR}}`, complemented by the target server's shared variables.
- **`deployment` pseudo-scope** (the deployment's own identity): `{{deployment.fqdn}}`, `{{deployment.url}}`, `{{deployment.pr_id}}`, interpolable inside a value (build and runtime) — resolved to the app's primary domain in production and to the generated FQDN in preview (which changes per PR), useful to compose a CORS origin or a callback URL. Also exposed as the predefined `AKERDOCK_FQDN`/`AKERDOCK_URL`/`AKERDOCK_PR_ID`.
- Predefined variables: `AKERDOCK_FQDN`, `AKERDOCK_URL`, `AKERDOCK_BRANCH`, `AKERDOCK_RESOURCE_UUID`, `AKERDOCK_CONTAINER_NAME`, `SOURCE_COMMIT`, `PORT`, `HOST`, `AKERDOCK_PR_ID` (ADR-022).
- **Magic variables** (compose/services): `SERVICE_<TYPE>_<ID>` — `URL`, `FQDN`, `USER`, `PASSWORD(_64)`, `PASSWORDWITHSYMBOLS(_64)`, `BASE64_32/64/128`, `REALBASE64_*`, `HEX_*`. Generated by the platform, persistent across redeployments, shared between services of the stack, editable in the UI.
- Required variables: `${VAR:?}` syntax (blocks the deployment if empty).

### 5.5 Deployment cycle
- **Auto-deploy on push**: GitHub App or manual webhooks (GitHub/GitLab/Bitbucket/Gitea, with secret and signature validation).
- Commits containing the `[skip ci]` or `[skip cd]` markers do not trigger an auto-deployment.
- **Watch paths**: per-application path patterns restricting auto-deploy to pushes modifying certain files (essential in a monorepo); known limitation: not applied to preview deployments (any opened/updated PR deploys).
- **"Auto Deploy" toggle** can be disabled per application: webhook events are then ignored for this resource (the manual/API deploy webhook remains usable).
- **Deploy webhook / API**: `GET|POST /api/v1/deploy?uuid=…&force=…` (multi-uuid, deploy by tag, force = build without cache), Bearer auth.
- **Per-server deployment queue**: `concurrent_builds` (default 2) + `deployment_queue_limit` (default 25); view of in-progress/pending deployments; cancellation.
- **Zero-downtime / rolling update**: new container started next to the old one → health check OK → traffic switch → old one stopped. Conditions: passing health check, default container names, no Docker Compose, no host port mapping.
- **Rollback**: to a previous local image still present on the server.
- **History**: list of deployments (queued / in progress / finished / failed), real-time build logs, cancellation.
- **Configuration diff**: recording and presentation of the application configuration changes included in a redeployment, in addition to the Git SHA.

### 5.6 Preview deployments (PR / MR deployments)

Ephemeral environment deployed automatically **for each pull request** (GitHub) or merge request (GitLab).

- **Prerequisites**: **GitHub App** integration (recommended) or manual webhooks; **wildcard DNS** (`A` record `*.domain` to the server IP).
- **Trigger**: PR opened → build + deployment of a separate instance; **automatic redeploy on each new commit** of the PR. On the GitLab side: "Merge request" event of the manual webhook. PRs already open before the feature is enabled require a manual deployment from the dashboard.
- **URL per PR**: configurable template, e.g. `{{pr_id}}.{{domain}}`; `{{random}}` placeholder for a random subdomain on each deployment.
- **Separate environment variables**: a dedicated set of variables for previews — nothing is copied implicitly from production. A base-repository preview resolves shared `{{scope.KEY}}` references inside that set and receives server-scoped variables like production; a fork preview never does, approval included (ADR-057, INV-010). Injected predefined variables: `AKERDOCK_PR_ID` (PR number), `AKERDOCK_URL` / `SERVICE_URL_<ID>` (preview URL, including for compose stacks).
- **Feedback on the PR** (GitHub App): automatic status comments with the preview link on each deployment.
- **Scoped deployments**: by default, only PRs from repo members/collaborators/contributors trigger a preview; opt-in toggle to allow public PRs (open source projects).
- Pull requests coming from a fork are ignored by default so as not to expose the runner's secrets and capabilities to untrusted code.
- **Automatic cleanup**: the preview environment is destroyed when the PR is closed or merged.
- Manual deletion possible, including via API by PR identifier.
- **Limitations**: no native cap on the number of simultaneous previews; supports regular, Railpack and static build packs.

### 5.7 Operations
- **Lifecycle**: Deploy / Redeploy, Start, Stop, Restart, force rebuild without cache — in UI and API.
- **Recreate (apply configuration)** (ADR-048): redeploys the artifact already running with the configuration as it stands — no clone, no build. This is what applies an environment variable edited since the last deployment: a container freezes its environment at creation time, so Restart hands the process back the values it already had. Available on applications, compose stacks and PR previews (whose own variable set, INV-010, is what gets applied). Refused with `409` when nothing has been deployed yet.
- **Runtime logs**: streaming of container logs per resource (and per service of a stack), configurable number of lines.
- Search, collapsible sections and log download; timestamps aligned to the target server's timezone; HTML rendering neutralized.
- **Web terminal** (xterm.js): shell into any managed container or server, via WebSocket → SSH; reconnection, scrollback.
- **Scheduled tasks**: crons per application/service (name, cron expression or `daily`/`hourly`/… aliases); two kinds (ADR-071): `container_command` (a command executed via `docker exec`, target container in a stack) or `github_workflow` (a GitHub Actions `workflow_dispatch` on the application's repository, signed by its GitHub App — the reliable replacement for GitHub's own best-effort `on: schedule`); execution history + notifications.
- **Status**: container state (running/exited, healthy/unhealthy) at app level and per service.
- **Resource cloning**: duplication of a resource to another project, environment or server/destination — copies the configuration (source, variables, declared storage), **not the volume data**; move between environments possible; no cross-team transfer (security boundary, recurring community request).

---

## 6. Managed databases

### 6.1 One-click engines
**PostgreSQL, MySQL, MariaDB, MongoDB, Redis, KeyDB, Dragonfly, ClickHouse.** (Any other engine remains deployable as image/compose, without the managed features.)

### 6.2 Common features
- **Auto-generated credentials** (64-character passwords); fields adapted per engine.
- **Internal URL** (hostname = resource UUID on the Docker network) and **External URL** (if public access enabled).
- **Public access**: Docker port mapping (permanent, restart required) or **dynamic Nginx TCP proxy** ("Accessible over the internet", public port changeable without restarting the database, configurable timeout, default 3600 s).
- **Custom configs** per engine: `postgres_conf` + `initdb args` + init scripts (`/docker-entrypoint-initdb.d/`), `mysql_conf`, `mariadb_conf`, `mongo_conf`, `redis_conf`, `keydb_conf` (no custom config for Dragonfly/ClickHouse).
- **Free image/tag**, custom docker run options, resource limits, configurable health checks, log drain, volumes and file mounts.
- **Lifecycle**: start/stop/restart + status, restart counters, `last_online_at`.

### 6.3 Database SSL
- "Enable SSL" + per-engine mode (PostgreSQL: allow/prefer/require/verify-ca/verify-full; MySQL/MongoDB: prefer→verify-full; MariaDB/Redis/KeyDB/Dragonfly: on/off; ClickHouse: not supported).
- **Platform-managed CA** (viewing/regeneration in the UI), mountable into client containers; custom CA possible.

---

## 7. Backups

### 7.1 Scheduled database backups
- Supported engines: **PostgreSQL (`pg_dump`/`pg_dumpall`), MySQL (`mysqldump`), MariaDB (`mariadb-dump`), MongoDB (`mongodump --gzip`)**; "dump all databases" option; database selection (list), collection exclusion (MongoDB).
- **The internal databases of one-click services are also backupable** (detection by image: postgres/mysql/mariadb/mongo).
- **The instance itself** backs up its own PostgreSQL database through the same mechanism.
- **Scheduling**: cron expressions + aliases (`every_minute`, `hourly`, `daily`, `weekly`, `monthly`, `yearly`); configurable timeout (default 3600 s); "Backup Now" button.

### 7.2 Destinations and retention
- **Local** (`/var/lib/akerdock/backups/...`) and/or **S3** (upload via MinIO `mc` client); "S3 only" option (local file deleted after upload).
- **Separate local / S3 retention**, three cumulative rules: max number of backups, max age (days), max total size (GB); 0 = unlimited.
- Success/failure notifications (including "local success but S3 failure").

### 7.3 Restore
- **Import Backups** per database instance: direct upload (drag & drop), file already on the server, or from a configured S3; customizable default restore commands (`pg_restore`, `mysql`, `mariadb`, `mongorestore`).
- Each backup execution is tracked (status, file, size, S3 upload); download / delete from the UI.

### 7.4 S3 Storages (configuration resource)
- Endpoint, bucket, region, access key / secret (encrypted in the database), path-style; **mandatory verification** (`ListObjectsV2`) before use; usability flag + alert if the storage becomes unusable.
- Tested providers: AWS S3, Cloudflare R2, DigitalOcean Spaces, MinIO, Backblaze B2, Scaleway, Hetzner, Wasabi, Supabase Storage…

### 7.5 Instance backup/restore
- All state = `/var/lib/akerdock` (config, SSH keys, proxy) + internal PostgreSQL database; scheduled database backup with S3 upload; documented restore procedure (master key, SSH keys, `pg_restore`).

---

## 8. Persistent storage

- **Named Docker volume**: name + mount path; the name is prefixed with the resource UUID (anti-collision).
- **Bind mount** (host directory → container).
- **File mount**: individual file whose **content is editable in the UI** (chown/chmod, file↔directory conversion, content reload from the server).
- Compose extensions: `is_directory: true` (host directory creation), `content: |` (file creation with env variable interpolation); top-level `configs` supported.
- Product warning: sharing a volume between containers is discouraged (locking).

---

## 9. One-click services

- **Catalog of 280+ one-click** docker-compose services (the introductory documentation sometimes keeps the more cautious mention "200+"): WordPress, Ghost, Directus, Strapi (CMS); Plausible, PostHog, Umami, Metabase (analytics); **Supabase, Appwrite**, PocketBase, GitLab, Gitea (dev); **n8n**, ActivePieces (automation); **MinIO**, Nextcloud (storage); Ollama, Open WebUI, Langfuse, Qdrant, Weaviate (AI); Authentik, Keycloak, Vaultwarden (security); Grafana, Uptime Kuma (monitoring); Elasticsearch, Meilisearch, Typesense (search); Immich, Jellyfin, Cal.com, Odoo, Home Assistant…
- **Anatomy of a template**: standard compose file + metadata in comments (`documentation`, `slogan`, `category`, `tags`, `logo`, `port`); compiled into a catalog JSON shipped with releases (refreshable from GitHub). Admission criterion: repo ≥ 1000 stars.
- **Magic variables** (`SERVICE_FQDN_*`, `SERVICE_PASSWORD_*`, etc.) for auto-configuration: URLs, credentials generated and shared between the services of the stack.
- **Managing a deployed service**: Deploy / Stop / Restart / "Pull latest images & restart"; **per-sub-container** restart; compose editor in the UI; domain per sub-service; env vars; storage/file storage; scheduled tasks; **backups of internal databases**; per-container logs; `exclude_from_hc` for one-shot jobs.
- **Network**: stack isolated in a network named by UUID; "Connect to Predefined Network" for inter-stack communication; services without domain/port = private (internal DNS).

---

## 10. Organization, auth and security

### 10.1 Teams and roles
- Multi-team; members invited by email or created by an admin.
- A user may belong to several teams and holds an **independent role in each**. A session acts in one team at a time and switches explicitly from the dashboard (team switcher); the permissions of the very next request are those of the role held in the team switched into, and the choice is remembered for the next sign-in. Switching into a team one is not a member of is refused — the instance root included (see rbac-matrix §3.6).
- Roles: **instance root/owner** (first user: global access, updates, settings), then **admin** and **member** per team. Limited granularity (no RBAC per project/resource — recurring community request).
- **User deletion**: documented procedure (self-service or by the root); teams where they are the sole member and their resources must be handled explicitly before deletion — never a silent cascade.

### 10.2 Authentication
- Email/password, public registration can be disabled, **TOTP 2FA**.
- **Account creation from an invitation**: an invitee who has no account yet creates one from the invitation link itself — the address comes from the invitation (never chosen by the invitee), the link is claimed single-use, and the instance password policy applies. This is what makes invitations work on an instance with no SSO provider configured; on an SSO-only instance (`password_login_disabled`) the link asks the invitee to sign in with SSO instead. Self-service signup outside an invitation stays governed by `registration_enabled`.
- **Password reset** by email ("forgot password"): requires the instance's transactional email to be configured (see §14.2); otherwise manual reset by the root.
- **Dashboard OAuth**: Azure, Bitbucket, GitHub, GitLab and Google.
- **OpenID Connect SSO**: configuration of a generic OIDC IdP (Okta documented; compatible IdPs possible depending on their conformance). Native SAML not documented.
- Non-interactive bootstrap of the first root user via environment variables, with strict validation of the email, name and password.

### 10.3 API tokens
- API disabled by default (enabled in the settings); tokens with **granular permissions**: `read`, `read:sensitive`, `write`, `deploy`, `root`; SHA-256 hashed, shown only once, expiration, **IP allowlist (CIDR)**, scoped per team; rate limit 200 req/min.

### 10.4 Network surface
- Platform ports: 8000 (dashboard), 6001 (WebSocket), 6002 (terminal), 22 (SSH), 80/443 (proxy). 8000/6001/6002 can be closed behind a proxied domain.
- Product warning: Docker bypasses UFW (prefer the cloud provider's firewall); OS hardening remains the user's responsibility.
- Terminal access to servers and containers controllable at instance/team level and reserved for authorized roles; every session must be authenticated, audited and bounded to the active team.

---

## 11. Notifications

- **Channels**: Email (SMTP or Resend), Discord, Telegram, Slack (Mattermost-compatible), Pushover, custom webhooks.
- **Events** (individually toggleable **per channel**): deployment succeeded/failed, container status change (app stopped/unhealthy), **preview created / updated / expiring soon / destroyed** (`application.preview.created|updated|expiring|deleted.v1`), backup succeeded/failed, scheduled task succeeded/failed, Docker cleanup status, disk usage threshold, **server unreachable / reachable again**, updates available, outdated proxy.

---

## 12. API, CLI and automation

- **REST API** `/api/v1` (OpenAPI 3.1, Bearer): CRUD for applications, databases (+ backups), services, servers (+ validation, domains and resources), projects/environments, teams, GitHub Apps, private keys, env variables (including bulk), deployments (trigger/list/logs/cancel), deploy by UUID/tag; cross-cutting resource list; system endpoints (unauthenticated healthcheck, version, API enable/disable).
- **Inbound webhooks**: dedicated GitHub/GitLab/Bitbucket/Gitea endpoints (signature verified, auto-deploy, previews) + generic per-resource deploy webhook for custom CI.
- **Official CLI** (Go, single binary, Cobra — ADR-033): multi-instance (contexts), management of servers/projects/resources/deployments (log streaming), domains, keys, databases and backups. **v1 "debug"** (spec `docs/specs/cli.md`, ADR-031/032): browser-based `login` without opening a port (poll+code+PKCE, SSO included), listing, logs (snapshot and `-f`), shell into a container, **TCP port-forward** to a container without exposing it, **`tunnel`** to a declared external endpoint (ADR-045/070), typed console; the client talks only to the manager on 80/443 and traverses proxies/LBs. Deploying from the workstation (`akerdock up`, ADR-018) is **withdrawn** (ADR-070 §3).
- **Built-in MCP server**: enabled at instance level, Streamable HTTP transport on `/mcp`, API token authentication, per-team scoping and 10 read-only tools (`overview`, list/get servers, projects, applications, databases and services), pagination 50 by default/100 maximum. OAuth grants remain bound to the human's current membership; API-token IP/creator restrictions are preserved; every tool enforces its domain-level `*:read` permission. Write operations are not part of v4.1.2.
- **Terraform**: community providers only (no official one).

---

## 13. Observability

- Real-time build logs; application logs per container; web terminal.
- **Log drains** per server then per resource: Axiom, New Relic, **custom Fluent Bit** config.
- Server + container CPU/RAM metrics (Sentinel) with history.
- **Structured audit** channel for API requests and webhook events; correlation with actor/token, team, target, result and request identifier, without logging secrets. This audit choke point is also the OTLP instrumentation point: each action emits an `akerdock.actions.total{action, actor, result}` counter and a span-event on the active trace — traces, metrics and logs being enable/disable-able signal by signal in the instance config (§14.2, ADR-008). Jobs (deployments, backups, cleanup, Git sync, notifications…) each carry their span + duration/outcome metric, and each API request its own server span.
- Application health checks; server reachability monitoring with notifications; disk alerts.
- No APM. **Built-in uptime monitoring** is decided by ADR-017: simple HTTP/TCP checks executed outside the workload, history and alerting via the existing channels — the scope stops at up/down and latency (Uptime Kuma & co remain available as one-click for advanced needs).

---

## 14. The platform itself

### 14.1 Installation
- Installation script (`install.sh`): checks prerequisites (Docker ≥ 24, Compose v2), generates the master key and the `.env`, builds the image and starts the stack via Docker Compose. Dashboard and API on the control plane's **single port** (ADR-021).
- OS: Debian/Ubuntu (LTS for the script), RHEL-like, SLES, Arch, Alpine, Raspberry Pi OS 64-bit. **Minimum: 2 vCPU, 2 GB RAM, 30 GB disk.**
- Advanced parameters: custom Docker CIDR range, custom installation registry, `docker-compose.custom.yml` persistent across upgrades.

### 14.2 Instance settings
- **Instance FQDN**: dashboard served behind the proxy with automatic certificate, allowing the direct ports 8000/6001/6002 to be closed.
- **Never indexable**: the control-plane port answers every request with `X-Robots-Tag: noindex, nofollow`, and serves a `robots.txt` that **allows** the crawl (ADR-074) — a crawler must fetch the page to read the header, and a `Disallow: /` would silence it while stranding any already-indexed URL in the index for good. Not a setting — an indexed dashboard publishes the login URL of the instance.
- **Instance timezone** configurable (display and platform maintenance crons).
- Public registration on/off, API on/off (see §10), custom validation DNS server (see §4.2).
- **Instance transactional email** (SMTP or Resend): invitations, password reset, test email; teams can reuse this system configuration for their notifications instead of their own SMTP.
- **Remote OTLP export** (ADR-008/§27.8): endpoint, protocol (HTTP/gRPC), auth headers (encrypted at rest) and choice of signals (traces, metrics, logs) toward an OpenTelemetry collector; configured here, encrypted, applied at the binary's next restart. Failing that, fallback to the `OTEL_*` variables.
- **Guided onboarding** at first startup: creation of the root user, first team, first server (localhost or remote) and first resource.

### 14.3 Updates
- **Auto-update** (periodic CDN check, can be disabled), semi-automatic update (button, reserved for the root user), or manual (script).
- Configurable auto-update cron; upgrade/downgrade to an explicit version; separate upgrade/rollback procedure for the internal PostgreSQL.
- Uninstall documented and destructive only after confirmation; resource/volume migrations between servers are explicit operations, distinct from the control plane backup.

### 14.4 Business model
- **Self-hosted: free and complete.** No paywalled feature, no "enterprise" edition: what the product does is in the binary you host (Apache 2.0 license, ADR-020).
- A possible managed control plane would remain a **hosting service** for the same binary, without reserved capability — it is a non-goal of the current scope (§16.2).

---

## 15. Structural pitfalls addressed by design

The limitations that make a container PaaS fail in production are known. They are addressed **by design** in AkerDock, and each is proven by a test:

- **Zero-downtime for compose stacks**: per-service switchover behind the proxy (ADR-015) — not only for single-container applications.
- **Resource limits actually enforced** on compose resources (cgroups verified in E2E), never declarative without effect.
- **Rollback by verified artifact** (OCI digest, ADR-006) — never "the image still present locally, if it is there".
- **Preview cap and TTL** (§20.4.3): an open PR cannot consume a server without bound.
- **Watch paths applied to previews** too (§20.4.5): in a monorepo, only the affected application redeploys.
- **Fine-grained RBAC** per project and environment (ADR-007), not a simple admin/member pair.
- **Restore drills** (ADR-014): a backup that has never been restored is not a backup.
- **Routed and aggregated notifications** (ADR-019): a flapping server does not produce dozens of alerts.

---

## 16. AkerDock product objectives

> Sections 16 to 28 turn the functional scope (§1–14) into **verifiable requirements**: each must be provable by a test, a fixture or a runbook.

### 16.1 Objectives

1. Allow a team to deploy and operate a containerized application on a fresh server in under 15 minutes, without writing a CI/CD pipeline.
2. Provide a self-hosted control plane that is never in the path of application requests.
3. Guarantee that all resources remain standard Docker, Compose, network, volume and file objects, administrable outside of AkerDock.
4. Ship a complete and proven functional core (§26) before adding surface: a capability is only "shipped" with its documentation, migrations, audit and test.
5. Design modules as replaceable capabilities: build engine, scheduler, proxy, secret store, remote transport, metrics and service catalog.
6. Stay lightweight and simple to operate: a single Go binary + PostgreSQL (no Redis nor application runtime), a single exposed port for the control plane, and a dashboard that stays responsive on a modest VPS (2 vCPU / 2 GB). This footprint is a product commitment, measured in CI, not a side effect.

### 16.2 Initial non-goals

- Becoming a general-purpose orchestrator equivalent to Kubernetes.
- Providing distributed storage or a proprietary global load balancer.
- Offering billing, commercial support or the operation of a managed service.
- Reimplementing Nixpacks, Railpack, Docker, BuildKit, Traefik or Caddy; AkerDock orchestrates them.
- Importing the internal schema of another platform. The entry path is the **adoption of existing Docker resources** (§20.7, ADR-013): what is already running is taken over as-is, without depending on the format of whoever created it.

### 16.3 Actors

| Actor | Main need | Expected rights |
|---|---|---|
| Instance root | Install, update, secure and diagnose the control plane | All teams and instance settings |
| Team owner/admin | Administer members, servers, sources, secrets and resources of their team | Read/write/deploy within their team |
| Member/Developer | Configure and deploy authorized resources | Per team policy, never cross-team |
| Operator/SRE | Observe, restart, rollback, backup/restore, terminal | Explicit and audited operational access |
| CI pipeline | Trigger a deployment and read its result | Minimal `deploy` token |
| Read-only/MCP integration | Inventory the infrastructure | `read` token, secrets masked |
| Target server | Run builds, workloads, proxy and agents | Trust limited to its server scope |
| Git/Cloud/S3 provider | Emit events or execute a requested action | Minimal credentials, rotation possible |

### 16.4 Proposed success indicators

- Successful deployment rate excluding application errors ≥ 99%.
- No cross-team overlap in authorization and isolation tests.
- Worker recovery after crash without double traffic switch or loss of an accepted job.
- 95th percentile of read API response < 300 ms excluding SSH/external providers, at 50 concurrent users.
- Webhook event accepted in < 500 ms then processed asynchronously.
- Control plane RPO ≤ 24 h with daily backup; documented RTO ≤ 2 h on a standard installation.
- All destructive, secret-access and terminal operations produce a usable audit trail.

## 17. Mandatory functional invariants

Each invariant must have at least one automated test at API or integration level.

| ID | Requirement |
|---|---|
| INV-001 | Every resource belongs to exactly one team, directly or through its Project → Environment chain. |
| INV-002 | A request cannot reference a key, source, destination, storage, server or resource of another team, even with a valid UUID. |
| INV-003 | A secret is never returned without the `read:sensitive` permission, nor written to logs, events or error messages. |
| INV-004 | A remote operation is idempotent or carries an idempotency key and a detection/reconciliation mechanism. |
| INV-005 | An existing healthy application remains routed as long as its replacement has not satisfied the switchover conditions. |
| INV-006 | A deployment failure deletes neither a persistent volume nor the last healthy container. |
| INV-007 | The control plane does not proxy application traffic; its outage does not stop already-active workloads. |
| INV-008 | Deleting a logical object requires checking its dependencies and clearly separates "remove from AkerDock" from "delete the data". |
| INV-009 | A webhook is authenticated, associated with exactly the right repository and deduplicated before triggering a deployment. |
| INV-010 | An untrusted PR or one coming from a fork obtains no production secret and triggers nothing without an explicit policy. |
| INV-011 | Generated Docker names are deterministic, non-conflicting and attachable to a stable internal UUID. |
| INV-012 | Any shell command built from user input is passed as typed arguments or escaped with a centralized and tested library. |
| INV-013 | An accepted job survives a process restart and does not remain indefinitely `in_progress` without heartbeat/lease. |
| INV-014 | Configuration changes are versioned sufficiently to explain and reproduce each deployment. |
| INV-015 | Resources discovered on a server are distinguished from managed resources; cleanup never destroys an unmanaged or persistent object. |

## 18. System boundaries and target architecture in Go

### 18.1 Logical components

```text
Browser / CLI / API / MCP / Webhooks
                  │
          API + Auth + Policy
                  │
        Business services / Postgres
          │          │          │
      Job queue   Event bus   Realtime hub
          │                     │
       Workers ───────────── logs/states
          │
  SSH/agent or provider API
          │
Target servers: Docker/BuildKit + Proxy + Sentinel
```

- **API/control plane**: HTTP, UI, auth, validation, policies, persistence, OpenAPI and MCP.
- **Workers**: deployments, server validation, backups, scheduled tasks, cleanup, notifications, Git synchronization and maintenance.
- **Realtime hub**: job progress, build/runtime logs and terminal. It is not the source of truth.
- **PostgreSQL**: configuration, desired/observed states, history, audits, leases and outbox.
- **Queue**: durable queue in PostgreSQL (decision §27.2). The interface remains abstract in the code, but no external bus is planned.
- **Remote transport**: abstract interface with an initial SSH implementation. An optional outbound agent may be added without modifying the business services.
- **Target runtime**: Docker Engine/Compose/BuildKit (decision §27.4: standalone Docker confirmed, Kubernetes ruled out). All calls go through a single runtime adapter, instrumented and secured — it is this contract that would allow evaluating another orchestrator later without touching the business services.
- **Proxy provider**: common Traefik/Caddy contract; configuration generation, validation, atomic application and rollback.

### 18.2 Packaging recommendation

- Start as a **modular Go monolith** with `api`, `worker`, `scheduler` and `all-in-one` binaries or modes from the same repository.
- Forbid circular dependencies between domains; expose interfaces for Git, SSH, Docker, proxy, registry, secret store, object storage and notification.
- Use PostgreSQL as the source of truth and the **transactional outbox** pattern to publish events after commit.
- Plan for multi-instance from the schema onward: jobs with lease, distributed locks per resource/server, migrations compatible with rolling upgrade.
- Keep the UI decoupled from orchestration: every important product action must have a stable API contract.

### 18.3 Sources of truth

| Data | Authoritative source | Reconciliation |
|---|---|---|
| Desired configuration | PostgreSQL | Version + diff per mutation |
| Container/network/volume state | Docker on the target server | Polling + event/agent |
| Source code | Git provider at the resolved SHA | Immutable SHA kept with the deployment |
| Deployed image | OCI digest, not only the tag | Digest resolution before switchover |
| Secrets | Encrypted secret store | Reference/version, never a value in events |
| Routing | Proxy file/labels on the server | Deterministic generation + validation + checksum |
| Job | Durable queue + PostgreSQL history | Lease, heartbeat, retry and dead-letter |

## 19. Logical data model

### 19.1 Main entities

| Aggregate | Key entities and relations |
|---|---|
| Identity | `User`, `Identity`, `MFAFactor`, `Session`, `Team`, `TeamMembership`, `Invitation`, `APIToken` |
| Organization | `Project` 1—N `Environment`; `Environment` 1—N `Resource`; `Tag` N—N resources |
| Infrastructure | `Server`, `Destination`, `PrivateKey`, `CloudCredential`, `RegistryCredential`, `S3Storage` |
| Source | `GitSource`, `GitHubApp`, `Repository`, `WebhookEndpoint`, `WebhookDelivery` |
| Application | `Application`, `BuildConfig`, `RuntimeConfig`, `Domain`, `EnvironmentVariable`, `PersistentStorage`, `HealthCheck` |
| Service/DB | `Service`, `ServiceComponent`, `Database`, `DatabaseCredential`, `DatabaseBackupPlan`, `BackupExecution` |
| Execution | `Deployment`, `DeploymentStep`, `DeploymentArtifact`, `ScheduledTask`, `TaskExecution`, `TerminalSession` |
| Platform | `ProxyConfigRevision`, `NotificationChannel`, `NotificationRule`, `AuditEvent`, `OutboxEvent`, `FeatureFlag` |

`Resource` is a logical union (`Application | Database | Service`) with the common fields: UUID, team, environment, destination, name, description, desired/observed status, timestamps and deletion policy.

### 19.2 Constraints and lifecycle

- Random, non-sequential public UUIDs; separate internal identifiers if necessary.
- Uniqueness of Project/Environment slugs within their parent and of Docker names within their destination.
- Project/Environment deletion forbidden while it contains resources, except for an explicitly previewed and confirmed cascade operation.
- Deletion of a key, source, destination or storage forbidden while it is referenced.
- Envelope-encrypted secrets with key version; rotation without a blocking rewrite of the whole database.
- `created_at`, `updated_at`, `deleted_at`, `created_by`, `updated_by` and optimistic version number on mutable aggregates.
- Deployment history, audit and backup executions subject to configurable retention; no accidental cascade from a deleted user.
- Observed statuses have an `observed_at`: beyond a threshold, the UI shows "unknown/stale", never a false `running`.

## 20. Critical workflows and acceptance criteria

### 20.1 Server onboarding

1. The admin picks a team, a key and enters host, port, user and timeout.
2. The API validates syntax, uniqueness and ownership of the references, then creates the server as `pending`.
3. A worker tests host key/SSH policy, connection, sudo, OS/architecture, disk space, Docker/Compose and ports.
4. If authorized, the worker installs or upgrades the prerequisites and creates the network/directories/helper containers.
5. It deploys and verifies the proxy and Sentinel according to the options, then moves the server to `ready`. The proxy is only involved if its intent is `running`: a server is created with the `stopped` intent, and the **first proxy start is an explicit operator act** (review of the settings — ports, wildcard, ACME email — then Start), never a side effect of validation.
6. Each step is replayable, logged, cancellable between two mutations and accompanied by a remediation instruction.

**Acceptance**: bad host key, key from another team, Docker Snap, unknown architecture, interactive sudo, insufficient disk and timeout each produce a distinct error without a falsely `ready` server.

### 20.2 Creation and deployment of a Git application

1. Selection of Project/Environment/Destination, source, repository, branch and build pack.
2. Validation of Git access and resolution of the branch into an immutable SHA.
3. Versioned snapshot of the configuration and creation of the `Deployment` as `queued`.
4. Acquisition of an application lock and a server build slot.
5. Isolated clone, submodules/LFS, build plan generation, controlled injection of variables and BuildKit secrets.
6. Build with structured logs; production of an image identified by digest; registry push if required.
7. Preparation of the candidate container, volumes, network, labels and inactive proxy configuration.
8. Startup, health checks and post-command; atomic traffic switch; graceful stop of the old container.
9. Status publication, notification, retention of the rollback artifact and asynchronous cleanup.

**Failure/compensation**: before switchover, delete only the candidate and keep the old one; after switchover, attempt an automatic rollback only if the policy allows it and if the old artifact is verified. Always release lease and slot.

### 20.3 Auto-deployment via webhook

1. Verify size limit, IP if configured, HMAC signature and timestamp.
2. Persist the delivery and respond quickly with `2xx`; deduplicate by provider + delivery ID.
3. Associate exactly provider/installation/repository/branch or PR with a resource of the same team.
4. Apply contributor/fork policy, skip markers and path filters.
5. Coalesce rapid pushes: a stale SHA in the queue MAY be replaced by the most recent one before the build starts.
6. Trigger the deployment workflow with a reference to the original delivery.

### 20.4 Pull request preview

Baseline (parity):

- Create a deterministic preview identity `(application_uuid, provider, pr_id)` and a collision-free URL.
- Use a dedicated set of variables; no implicit copy of production secrets.
- Redeploy at the new SHA, keep only the defined retention and destroy containers/routing on close/merge.
- If cleanup fails, mark `cleanup_failed`, notify and retry; never recycle a PR's identity for another application.

The following requirements are part of the feature's **priority scope** (decision §27.11, tracking §26) — they are shipped with it, not as a later extension:

1. **Docker Compose in preview**: the compose build pack MUST be supported — a complete ephemeral stack per PR (isolated network, own volumes, magic variables resolved per preview instance), destroyed entirely on cleanup.
2. **Ephemeral data**: a preview MAY provision its ephemeral databases, initialized by a seed script or by cloning a reference snapshot; it MUST NEVER implicitly share a database with production or another preview.
3. **Lifecycle and costs**: cap on simultaneous previews per application and per server (queue beyond it), **inactivity TTL** with automatic destruction, distinct resource limits for previews, optional dedicated preview server pool; the proxy SHOULD support **scale-to-zero** (idle container stopped, woken on the first request).
4. **Access protection by default**: every preview URL is protected (basic auth or signed link) and serves `X-Robots-Tag: noindex`; public exposure is an explicit per-application choice.
5. **Monorepo**: watch paths also apply to previews — only an application affected by the PR's modified files is (re)deployed.
6. **Rich Git integration**: commit statuses/checks (pending/success/failure) usable as a merge condition, GitHub Deployments API ("View deployment"), **single comment updated in place** (not one comment per deployment), and feedback parity for GitLab/Gitea, not only GitHub App.
7. **Trigger controls — options enableable per application** (disabled by default, the parity behavior remaining the default): opt-in via PR label, comment commands (`/deploy`, `/destroy`), draft PR exclusion, automatic cancellation of a preview build made stale by a new commit. Each control is individually enableable.
8. **Forks on approval**: a fork PR MAY obtain a preview after manual approval by a maintainer — isolated builder, no secret injected; without approval, it remains ignored (INV-010).

### 20.5 Database backup and restore

- Lock one execution per plan, verify temporary space and the S3 destination before launch.
- Run the appropriate tool in a controlled environment, capture exit code, size, checksum and engine version.
- Upload by stream, verify the remote object, then apply local/S3 retention without deleting the last valid backup.
- Restore is a separate, confirmed operation, with a prior format test and a complete log. A restore into a non-empty database requires reinforced confirmation.
- **Acceptance**: a local success + S3 failure is an explicit partial status, not an overall success.

Complementary requirements (decision §27.14):

- **Application volume backup**: backup plans on the volumes and bind mounts of applications and services — not only databases — encrypted and deduplicated (restic-like tool), with a per-resource quiesce/stop option for consistency, and the same scheduling, local/S3 retention and notifications as database backups.
- **Additional engines**: Redis (RDB snapshot) and ClickHouse covered natively, lifting the parity limitation (§15).
- **Restore drills**: periodic automatic restore test in a throwaway environment — real restore + integrity verification (checksum, counting) — with an alert if a backup plan proves non-restorable. A backup never restored is not considered reliable.

### 20.6 Resource deletion

1. Display a preview: affected containers, networks, domains, tasks, backups and volumes.
2. Ask distinctly whether the volumes/persistent data should be kept.
3. Create an idempotent deletion job; remove routing first, then workloads, ephemeral objects and finally the logical object.
4. On partial failure, keep a reconcilable tombstone and offer retry/forget; do not lose the list of remote leftovers.

### 20.7 Adoption of an existing resource (decision §27.13)

1. Scan a server: inventory of **unmanaged** containers and compose stacks (relies on INV-015).
2. Propose a mapping to the AkerDock model: application or service, networks, volumes, variables, ports and domains detected via inspection and labels.
3. Preview: what will be managed, what will be modified (labels/metadata added), what is not adoptable and why.
4. Adopt **without redeployment**: AkerDock takes control without restarting the workload when possible; the first redeployment fully normalizes the resource.
5. Reversible operation: "un-adopting" returns the resource to its unmanaged state without destroying it.

**Acceptance**: adopt a multi-service compose stack with volumes then redeploy it without data loss; a resource not representable in the model is flagged with the reason, never silently partially adopted.

### 20.8 Coordinated deployment of an environment (decision §27.16)

- An environment can be deployed **as a unit**: explicit dependency graph between resources, topological order, parallelism within the same level.
- **Migration hooks**: one-shot job executed after build and before switchover (e.g. schema migration); hook failure prevents any switchover in the environment.
- Per-level atomic mode (optional): the traffic switch waits until all resources of the level are healthy.
- **Automatic rollback on degraded health** (opt-in policy per application): after switchover, observation window (bake time) on the health checks; on degradation, rollback to the previous verified artifact, notified and audited.
- Partial failure: explicit environment state (resources deployed / not deployed / failed), resumption possible at the point of failure — never a silent half-switchover.

## 21. State machines

### 21.1 Deployment

```text
queued → preparing → cloning → building → pushing? → starting
   └──────────────────────────────────────────────→ cancelled
starting → healthchecking → switching → finishing → succeeded
    └──────────────→ failed ←──────────────────────────┘
failed → retrying → preparing
```

- `cancelled`, `failed` and `succeeded` are terminal for an attempt.
- `queued → superseded`: a deployment still in the queue can be replaced by a more recent one of the same application (coalescing §20.3.5); `superseded` is terminal, treated like `cancelled`, with a link to the superseding deployment.
- A retry creates a linked attempt or explicitly increments `attempt`; it does not silently rewrite history.
- `switching` is protected by an exclusive lock per application/destination.

### 21.2 Resource and server

```text
Desired resource: stopped ↔ running → deleting → deleted
Observed resource: unknown | starting | healthy | unhealthy | exited | missing
Server: pending → validating → ready ↔ unreachable → maintenance → deleting
```

Desired state and observed state are stored separately. Reconciliation converges toward the desired state but suspends destructive actions when the observation is too old.

### 21.3 Generic job

```text
scheduled → queued → leased → running → succeeded
                       │          ├→ retry_wait → queued
                       │          ├→ cancelled
                       └───────────└→ dead_letter
```

Each lease has an expiration and a heartbeat. After a crash, another worker takes over only after expiration and verifies the effect already produced before replaying.

## 22. Proposed non-functional requirements

### 22.1 Availability and resilience

- Workloads and proxies keep working without the control plane.
- The API and workers support at least two instances behind a load balancer, without mandatory local session.
- All SSH, Git, registry, S3 and provider calls have timeout, cancellation, error classification and bounded retry with jitter.
- A circuit breaker prevents a provider outage from saturating the workers.
- Deployment and restore jobs are never blindly replayed; their resumption starts with a remote inspection.
- A documented procedure restores PostgreSQL, encryption keys, SSH keys, proxy configurations and required files.

### 22.2 Reference performance and capacity

For the first stable version, on 4 vCPU/8 GB and a properly sized PostgreSQL:

- 100 servers, 2,000 resources and 100,000 historical deployments per instance.
- 50 simultaneous distributed builds; configurable limit per server and per team.
- 1,000 webhook deliveries/minute in burst, queued without loss.
- 500 concurrent realtime streams and 50 simultaneous terminal sessions.
- Mandatory pagination for every collection; no list endpoint loads an unbounded relation.
- Backpressure on logs: bounded buffer, cursor-based resumption, explicit signal if lines were dropped.

These numbers are test targets, not license limits. They must be revised after benchmarks.

### 22.3 Durability and consistency

- ACID transactions for business mutations and outbox; versioned and restorable migrations.
- Control plane backup encryptable, checksummed and periodically tested via automated restore.
- Eventual consistency accepted for statuses and metrics; strong consistency required for authorization, name/port reservation, secrets and traffic switchover.
- Optimistic locking on UI/API edits to avoid silently overwriting a concurrent configuration.
- All internal timestamps are UTC; display in the user/server timezone with explicit indication.

### 22.4 Compatibility

- AMD64/ARM64 Linux servers with Docker Engine ≥ 24 and Compose v2.
- Internal PostgreSQL on an explicitly tested version range; guided major upgrade.
- Evergreen browsers; UI responsive down to mobile for viewing, emergency actions and terminal.
- Versioned API; backward compatibility across a minor version, deprecation announced before removal.
- JSON/YAML export of non-secret configurations and optional encrypted export of secrets to avoid lock-in; export is part of the declarative configuration contract (§24.5).

### 22.5 Accessibility and ergonomics

- Keyboard navigation, visible focus, form labels, WCAG 2.1 AA contrast and live announcements for progress/errors.
- Every long action becomes a visible job with steps, duration, logs, possible cancellation and remediation.
- Reinforced confirmation for data deletion, restore, CA rotation and root terminal.
- Generated values (UUID, domain, URLs, displayable credentials) have a copy action and clear context.

## 23. Security and threat model

### 23.1 Trust and isolation

- The control plane, its root administrators and anyone with a root terminal are highly privileged.
- A compromised target server must not give access to the other servers: separable keys/credentials and secrets distributed on a strict need basis.
- A team is a security boundary. All repositories/queries/services receive the `team_id` from the authenticated context, never from an unverified client parameter.
- Builders execute untrusted code. They must be isolated from the control plane's credentials, from the global Docker socket when possible and from the sensitive internal network.
- Public previews use dedicated builders or a reinforced isolation policy; no production secret by default.

### 23.2 Secrets and cryptography

- Authenticated encryption at rest (AEAD) with an external master key or root-only file; versioning and rotation.
- Passwords hashed with Argon2id; API tokens stored as an irreversible hash with an identification prefix.
- Secrets masked in UI/API/logs/audit; explicit reveal only if the product allows it and if `read:sensitive` is present.
- SSH keys without passphrase accepted for compatibility, but `0600` files, `0700` directory, per-team selection and assisted rotation.
- Webhook secrets, OAuth client secrets, DNS-01 credentials, registry/S3 credentials and private CAs follow the same secret store.

### 23.3 Application controls

- CSRF for browser sessions, Secure/HttpOnly/SameSite cookies, session rotation after login/elevation, invalidation on logout/role change.
- TOTP 2FA with recovery codes; anti-bruteforce and progressive delay on login.
- OIDC: strict validation of issuer, audience, nonce, PKCE and normalized email; explicit account linking against takeover by email collision.
- SSRF: allow/deny policy on Git, registry, S3, webhook, proxy and uptime HTTP/TCP targets; cloud metadata/link-local/private/reserved ranges blocked by default on the IP resolved at connection time (IPv4/IPv6, redirects and DNS rebinding included).
- Centralized validation of images, branches, paths, domains, CIDRs, ports, cron and Docker options.
- Path traversal/symlink protection during file mounts, archives, clones and backup uploads.
- Size/type limit on uploads and UI display of file mounts (5 MiB maximum for inline editing per v4.1.x parity).
- ANSI/HTML neutralization in logs and limitation of terminal sequences on the display side.

### 23.4 Minimal audit

Log: login/logout/failures, MFA, members/roles, token creation/revocation, sensitive access, secret mutation, terminal, server/proxy changes, deployment/rollback, backup/restore, deletion, instance settings and mutating webhook/API calls.

Each event contains `event_id`, UTC date, actor/type/token, team, action, resource/type/UUID, result, IP, user-agent, request/correlation ID and redacted diff. The audit is append-only, paginated, filterable, exportable and subject to retention.

### 23.5 Mandatory security tests

- Cross-team matrix on every endpoint and indirect relation.
- Fuzzing of the Compose, env, cron, domain, port and custom Docker options parsers.
- Shell injection tests on every remote command.
- Webhook scenarios: replay, bad signature, prefix-named repo, fork, large payload and out-of-order events.
- Concurrency scenarios: double deploy, delete during deploy, key rotation during a job, double restore.
- SAST, dependency/container scanning, SBOM and signed images for AkerDock releases.

## 24. API, event and job contracts

### 24.1 REST

- OpenAPI is a versioned artifact tested in CI; the API is under `/api/v1`.
- Errors in a stable format: `code`, generic `message`, validated `details`, `request_id`; no stack nor sensitive command.
- Cursor pagination recommended for histories/logs; page/per-page pagination accepted for MCP compatibility.
- `Idempotency-Key` supported on creations, deploy, backup and restore.
- ETag/optimistic version on sensitive PATCHes; `409` response with the current version in case of conflict.
- Long actions respond `202` with `job_uuid` and a tracking URL.
- Permissions are evaluated at the action, not only at the route group: `read`, `read:sensitive`, `write`, `deploy`, `root`.

### 24.2 Internal events

Minimal envelope:

```json
{
  "id": "uuid",
  "type": "deployment.succeeded.v1",
  "occurred_at": "RFC3339Nano",
  "team_uuid": "uuid",
  "resource_uuid": "uuid",
  "actor": {"type": "user|token|system", "uuid": "uuid"},
  "correlation_id": "uuid",
  "payload": {}
}
```

- Version in the type; idempotent consumers; ordering guaranteed only per aggregate key if necessary.
- Outbox published after commit, inbox/deduplication at consumers with external effects.
- Payloads contain references and redacted metadata, never secret values.

### 24.3 Scheduling

- Cron interpreted in an explicit timezone, with the next execution previewed.
- Overlap policy per task: `forbid` by default, optional `allow` or `replace`.
- Missed-run policy after unavailability: `skip` by default or `catch_up_one`; never an unlimited burst.
- Backups, cleanup and user tasks use the same scheduler but separate queues/priorities.
- A user task fires one of two actions (ADR-071): a command in the resource's container, or a GitHub Actions `workflow_dispatch` on the application's repository. A dispatch records "accepted by GitHub" as its success (GitHub reveals no run id); it is never retried by the queue — a dispatch is not idempotent, each one starts a build.

### 24.4 Realtime and terminal

- Transport: **SSE** with `Last-Event-ID` resumption for logs, statuses and progress (decision §27.24). WebSocket is no longer *reserved* for the terminal: ADR-064 revised that clause so every CLI-to-server tunnel climbs one transport ladder (HTTP/3 → HTTP/2 → WebSocket), the terminal included — WebSocket stays the terminal's transport whenever the ladder falls back to it. SSE for logs, statuses and jobs is untouched.
- Log/status streams resumable by cursor and protected by the same policy as the equivalent REST endpoint.
- Short-lived realtime token, single-use or bounded to the resource; revocation on session close.
- Terminal via PTY with resize, heartbeat, idle timeout, configurable maximum duration and guaranteed kill on disconnect/expiration.
- Opening and closing are audited; keystrokes are not recorded by default to avoid collecting secrets, except in an explicit regulatory mode.

### 24.5 Declarative configuration — config as code (decision §27.12)

- All of a team's non-secret configuration (projects, environments, resources, domains, non-secret variables, backup plans, scheduled tasks) is **exportable as stable YAML**, versionable in Git.
- **Idempotent apply**: submitting this YAML converges the state — creation, update, deletion only on explicit request; a **dry-run** mode produces the complete diff before application; conflicts are detected by optimistic version (§24.1).
- Secrets are **referenced** (name + version), never inline in the export; their values go exclusively through the dedicated endpoints.
- The format is a versioned contract (published schema), subject to the same compatibility policy as the API (§22.4).
- An **official Terraform/OpenTofu provider** is built on the API and covers at least the P0/P1 scope.
- An apply is audited like any mutation and executed as a visible job with steps and cancellation (§22.5).

## 25. Web dashboard and UX requirements

### 25.1 Requirements per journey

| Journey | Minimum requirements |
|---|---|
| Onboarding | First-startup wizard: root user, first team, first server (localhost or remote) and first resource, with a possible exit at each step |
| Dashboard | Global state, unreachable servers, active/failed deployments, disk/backup/update alerts and priority actions |
| Resource creation | Project/Environment/Destination selector, source/build pack, summary before creation, inline validation and safe defaults |
| Application detail | Desired/observed state, domain, source/SHA, config, env, storage, health, deployments, logs, terminal, tasks and lifecycle actions |
| Deployment | Step timeline, log stream, config diff, author/trigger, SHA/digest, duration, cancel/retry/rollback depending on state |
| Server | Reachability, Docker/proxy/Sentinel, resources, destinations, disk/CPU/RAM, cleanup, logs and terminal |
| Database | Internal/external URLs, masked credentials, SSL, config, volumes, health, backups/restores and data warnings |
| Compose service | Validated editor, diff, component list, domains/env/storage/health/logs per component |
| Security | Members/roles, invitations, tokens, sessions, MFA/SSO, keys/credentials and audit |
| Documentation | In-app manual of every capability, reachable from the sidebar, searchable, and **filtered to the reader's permissions** (§25.4) |

Forms systematically distinguish: saved value, inherited value, generated value, locked secret and not-yet-deployed change.

### 25.2 Dashboard stack and architecture

- The dashboard is an **Angular SPA** (latest LTS version), TypeScript in strict mode, **standalone** components and signals; no SSR required (authenticated administration tool).
- The UI consumes **exclusively the public API** (§24) and the realtime streams — no undocumented private route; every capability visible in the UI is therefore scriptable via API/CLI (consistent with §18.2).
- **Distribution**: compiled static assets, embedded and served by the control plane's Go binary; no Node runtime in production; the UI and the API share the control plane's single port (§27.1).
- **Lazy loading per functional domain** (servers, projects, resources, security, settings); performance budget defined and tracked in CI (bundle sizes, initial load time).
- Generation of the API client and its types from the OpenAPI artifact (§24.1) to prevent any UI/API drift.
- **i18n from the first component onward**: UI in English (default language), no hardcoded string — translation keys everywhere; French arrives as a second locale without refactoring.

### 25.3 Design system and components

- **Internal, minimalist component library**: buttons, forms, tables, status badges, deployment timeline, log viewer, editors (env, compose), confirmation modals — **no heavy third-party UI kit** (Material & co); third-party dependencies limited to specialized needs (xterm.js for the terminal, code editor, metrics charts).
- **Documented and versioned "clean" design system**: design tokens (colors, typography, spacing, radii, elevations), **light and dark** themes, density suited to an operations tool (compact tables and lists, dense information without decorative overload).
- Default theme: **follows the system** (`prefers-color-scheme`) with a manual toggle persisted per user; brand accent color: **teal/cyan**, chosen so as not to collide with the semantic state colors (success, warning, danger).
- **Normalized visual states** across the whole product: same colors/badges for running, starting, healthy, unhealthy, exited, failed, queued, stale/unknown — a given state reads the same way on the dashboard, a resource, a deployment or a job.
- Consistent iconography and vocabulary; every destructive action follows the same confirmation pattern (§22.5).
- Browsable component catalog (Storybook-like or equivalent) serving as the single reference; a component only enters the UI if it is in the catalog.
- The design system meets the accessibility requirements of §22.5 (keyboard, focus, WCAG 2.1 AA contrast) from component design onward, not as a retrofit.

### 25.4 In-app documentation

- The dashboard ships its own **manual**: one page per capability, grouped by task (start here, ship code, run and debug, automate, account and team, instance administration), reachable from the sidebar and from the topbar on every screen.
- The manual is written for the **daily user** — the developer who deploys, reads logs, opens a shell and forwards a port — not for the operator who installed the instance. Instance administration is documented, at the end, and never first.
- **The manual MUST be filtered by the reader's permissions**, at topic and section level, with the same predicate that filters the navigation (rbac-matrix §2): documenting an action the role cannot take turns the manual into something the reader must diff against their own dashboard. A reader MAY opt into the whole manual, in which case what is beyond their role is **marked as such** rather than silently mixed in. **Revised by ADR-072**: the filter runs **server-side**, on `GET /docs` — the manual used to be compiled into the bundle, so every reader downloaded the chapters their role could not open and the filter only governed what was displayed.
- **Revised by ADR-072**: documentation content is **Markdown with a YAML front-matter**, one file per topic under `internal/docs/manual/` (beside its parser: `go:embed` cannot reach above its own package), embedded in the binary and parsed at boot. It renders to HTML produced by the Markdown parser itself — raw HTML written in a source file is dropped, never passed through — and the dashboard binds it through Angular's sanitiser. The closed block vocabulary this section originally required is retired with the format that needed it.
- Content invariants are unit-tested — unique identifiers, icons that ship, links that resolve to declared routes, permission gates that exist in the catalogue, and a manual that still covers the daily surface for a plain `member` — **in Go since ADR-072**, where the corpus now lives.

## 26. Delivery strategy and tracking matrix

### 26.1 Levels

- **P0 — Foundation**: auth/team, projects/environments, SSH servers, standalone Docker, Dockerfile/image applications, variables, volumes, Traefik/HTTPS, queue, logs and lifecycle.
- **P1 — Usable PaaS**: GitHub/GitLab/webhooks, Nixpacks/Railpack, zero-downtime, rollback, databases, S3 backups, notifications, scheduled tasks and public API.
- **P2 — Broad scope**: Compose/services, one-click catalog, previews, build servers, Caddy, Sentinel, log drains, terminal, OAuth/OIDC, shared vars and cleanup.
- **P3 — Periphery/experimental**: multi-server for one app, MCP, advanced DNS-01 and deprecated Swarm behind a feature flag. (Cloudflare tunnels, cloud provisioning and patching: removed — ADR-027.)

Each phase is usable on its own. A feature does not move to "complete" without documentation, migrations, metrics, audit, authorization tests and a recovery scenario.

### 26.2 Tracking matrix

The "Sections" column refers to the requirements of this document that define the capability.

| Capability | Sections | Priority | Status | Expected proof |
|---|---|---:|---|---|
| Team isolation/auth/tokens | §10, §23 | P0 | Compliant | ADR-038/047; team-scoped queries on every resource, uniform 404 across the boundary (INV-002), token capped by its creator (rbac-matrix §4.2), multi-team sessions with an explicit switch resolved through the membership on every request (rbac-matrix §3.6); cross-team + API unit tests |
| Server onboarding/SSH | §3, §20.1 | P0 | Compliant | Validator module tests + DinD E2E journey (onboarding + Traefik bootstrap); agent provisioned at validation (ADR-051/052); manual VM/ARM64 (§27.26) |
| Private keys write-only + server-side generation | §3.1, §5.1, §23.2 | P2 | Compliant | ADR-075; once a key enters the platform its private material is never served again, whatever the permission — reveal parameter, `private_key` response field and the `keys:reveal` permission removed (deliberate INV-003 narrowing for this resource; `secrets:reveal`/`databases:credentials` untouched). `POST /private-keys/generate` (ed25519, `keys:manage`) answers with the public key only; rotation by `PATCH` stays (a write). Dashboard: generate flow with copy-ready `authorized_keys` line, public key unfoldable per row, no reveal affordance. Unit tests per ADR-075 §Verification |
| Non-root SSH via opt-in `use_sudo` (every command escalated through non-interactive `sudo -n`, distinct `check_sudo` validation step) | §3.1, §20.1 | P2 | Compliant | ADR-076; the flag is real end to end (spec, handlers, dashboard forms; toggling revalidates), escalation wrapped at the `sshexec` choke point (`LC_ALL=C sudo -n -- sh -c …`), refusals classified as typed errors carrying the sudoers `NOPASSWD` remediation, terminal and tunnels deliberately not escalated. Unit tests per ADR-076 §Verification |
| Edge routing: public routes of a LAN-only server relayed by SNI passthrough through a designated edge server | §3, §4 | P3 | Compliant | ADR-077; `edge_server_id` on servers (same team, no chaining in either direction, edge must run Traefik, NULL = serve your own routes; migration 00100), every routing apply on an edge-routed origin rebuilds the edge's checksummed relay revision whole from placements (reserved scope `01-edge-<origin uuid>`, hook inside ProxyApplier so no writer can forget it): `HostSNI` passthrough → origin:443 with PROXY protocol v2 + Host relay on 80 (ACME HTTP-01 and redirects traverse); the origin's static config trusts the edge's addresses alone (proxyProtocol on 443, forwardedHeaders on 80 — converged by the proxy start enqueued on designation change); certificates, walls, noindex and the scale-to-zero page stay on the origin; shared `wildcard_domain` as pure naming template — one DNS record to the public entry, no per-server domains, no internal CA (rejected with arguments in the ADR). Conformance golden (`edge-relay`) + syncer/hook/job/trust unit tests + handler refusal tests per ADR-077 §Verification |
| Source-only installs on several servers (self-propagating: each server rebuilds the instance's exact commit from the public repository, natively for its own CPU) | §3.1, §22.4 | P3 | Compliant | ADR-078; the build bakes its own coordinates (`-X main.commit`, empty on a dirty tree; repo overridable via `AKERDOCK_SOURCE_REPO`) and `AgentEnsureCommand` — the one rendering every agent deploy crosses (validation, reconciliation, scale-to-zero ensure) — prepends `docker image inspect \|\| docker build <repo>#<commit> \|\| exit $?` when a commit is known, so `./install.sh` updates propagate with no per-server step; registry installs render no prelude. Unpushed/dirty work ships by hand: `./install.sh seed user@server` (cross-build via buildx `--output type=docker,dest=-` — local store untouched — streamed over SSH, ADR-076 docker→`sudo -n` ladder); install.sh warns when propagation cannot work, and the agent-deploy failure hint names the actual situation (commit unpushed vs dirty install). Unit tests on the prelude rendering and both hints; seed and propagation operator-exercised (two daemons + an SSH hop, outside the unit lane by ADR-026) |
| GPU as an observed server fact + typed device carriage | §3.1, §20.1, §22.4 | P3 | Compliant | ADR-079; validation probes the NVIDIA runtime and `nvidia-smi`, records `gpu_name`/`gpu_memory_mb` as nullable facts (toolkit missing → GPU-less + warning naming the fix); producers fill `DeviceRequests`/`IpcMode`/`ShmSize` on the already-verbatim `HostConfig` wire for resources whose config demands it; placement on a GPU-less server refused by name (build-arch guard mirror). Unit tests per ADR-079 §Verification |
| Models: first-class inference resource (vLLM/SGLang) + GPU-gated dashboard section | §3, §6, §26 | P3 | Compliant | ADR-080; DELIVERED (migration 00102, `internal/inference` renderer/parser with the round-trip identity pinned, database-style lifecycle jobs with the swap ordered inside one job, full API incl. command export/import, HF search proxy, `models:*` permissions + matrix rows; unit-tested, coverage gates green); plus the Models dashboard section (GPU-gated nav, transverse list, model-first creation flow with Hub search, flag editor, command preview/paste, one-click swap on the occupied-GPU 409) and the manual chapter; resource kind `model` mirroring the databases shape (engine enum, HF model_id, single `memory_fraction` knob rendered per engine, per-engine/per-arch image default, HF token via secret vars, server-scoped HF cache volume, two-tier config: typed core + structured `engine_flags` list covering the whole upstream surface with reserved flags refused by name, first-class stop/swap/resume with an occupied-GPU guard that **counts memory rather than models** (ADR-082, revising ADR-080 §5's third bullet: the declared `memory_fraction` of the running neighbours plus the candidate's is summed against a 0.95 budget — within it the start proceeds with no confirmation, over it a 409 states the arithmetic and offers `swap=true` or `force=true`, mutually exclusive; an undeclared fraction counts as the engines' own 0.9 default, never zero), serve command exportable/importable with round-trip identity pinned by test); follow-up UX tranche: one lifecycle operation at a time (409 at enqueue via the lock key), jobs cancellable at any point (`POST /jobs/{uuid}/cancel`: 200 outright while queued, 202 cooperative once running — the model family polls the flag at its readiness checkpoint, stops the container and reports a cancellation rather than a failure), a start that crash-loops failing on the container's restart count instead of the full readiness budget and never being auto-retried, optional public domains on the model resource — the shared `domains` table grows a fourth owner kind (00105), routing and edge-relay derivation cover them, empty meaning LAN-only, active-job banner with polling, runtime logs endpoint + follow view, environment variables on the resource machinery (shared refs + server scope resolved into the container env, the model's own variable winning last); managed native API key (enveloped, `--api-key`, readable under `models:credentials`), live HF search proxied by the control plane, database-style lifecycle jobs with minutes-scale readiness, scale-to-zero opt-in off by default, Models section gated on the observed GPU with the resource staying project-anchored. Unit tests per ADR-080 §Verification; manual Spark checklist (§27) |
| HF cache visibility/pruning + per-server write-only token | §3, §6 | P3 | Compliant | ADR-081; per-server `hf_token_enc` (enveloped, write-only, wins over the instance env for that server's engines; migration 00104), cache listed and pruned through typed one-shot busybox containers (pure-argv `rm -rf`, Hub-charset validation as the boundary), Hugging Face tab on GPU servers (token, per-model sizes, delete/empty with confirmation, audited). Unit tests per ADR-081 §Verification |
| Deploy image/Dockerfile | §5, §20.2 | P0 | Compliant | Engine/resumption unit tests + single E2E journey (image, inline Dockerfile, Git at pinned SHA; real zero-downtime, ADR-015) |
| Proxy/domains/ACME | §4 | P0 | Compliant | Conformance fixtures (atomic apply, checksummed revisions, drift repair) + real routing in the single E2E journey; DNS-01 wildcards, D-30/D-7 expiry alerts, per-server proxy lifecycle |
| Git/build packs/webhooks | §5.1–5.6 | P1 | Compliant | Protocol/module tests per provider and build pack: deploy keys, GitHub App manifest flow, signed webhooks + delivery dedup (INV-009), static/nixpacks build packs, build args + BuildKit secrets, dedicated build servers (Railpack deliberately refused with `422`) |
| Databases/backups/restore | §6–7 | P1 | Documented divergence | **§6 announces eight engines, one ships.** v1 is PostgreSQL only by recorded product decision; the divergence is the spec's, not the implementation's, and it stays until §6 is amended or the engines land. What is delivered is complete for that engine: module tests, SSL per-server CA, TCP proxy mode, S3 (SigV4 in-house), independent retentions, scheduled runs, verified restore |
| Compose/services/templates | §5.2, §9 | P2 | Compliant | Compose conformance fixtures: compose-go subset + `x-akerdock`, magic `SERVICE_*` variables, `service_components`, per-component routing, stack hooks, compose previews; one-click catalog (ADR-010) tracked separately in TODO |
| Enriched PR previews (compose, ephemeral data, seed by volume clone — ADR-029, TTL/caps, protection, checks, approved forks; shared variables resolved for base-repo PRs — ADR-057) | §5.6, §20.4, §27.11 | P2 | Compliant | Multi-provider protocol tests + fork/access security + seed module tests + shared-scope gate unit tests; the INV-010 variable boundary is selected in a single place and pinned on **all three renderers** (build, runtime, compose interpolation) by unit tests asserting on rendered VALUES — a preview carries no production value on any surface a Dockerfile can read (build args, BuildKit secrets, nixpacks `build.env`, which no longer survives its build) |
| Scale-to-zero for previews **and applications** (sleep/wake via an in-line waker) | §20.4.3, proxy-contract §8 | P2 | In progress | Waker module tests (wake, 503/504 limits, single-flight, non-activity uptime) + dynamic file generation + sleep decision; end-to-end wake in the E2E journey (ADR-036, ADR-037) |
| Volume backups + Redis/ClickHouse + restore drills | §20.5, §27.14 | P1 | Partial | Automated restore drills delivered (ADR-014: throwaway restore, table-count comparison, loud failure); volume backups and the Redis/ClickHouse engines remain out of v1 (product decision — see Databases row) |
| Instance PostgreSQL major upgrade (opt-in in-place, backup-first) | §14.3, §22.4 | P2 | Compliant | `scripts/pg-upgrade.sh` (version detection, volume copy, one-shot `pgautoupgrade`, health check) + `install.sh` guardrail + runbook §C (ADR-039) |
| Config as code + official Terraform | §24.5, §27.12 | P2 | To do | Round-trip export→apply + provider tests |
| Adoption of existing resources | §20.7, §27.13 | P2 | Compliant | Scan/reconciliation module tests without loss |
| Coordinated deployment + auto-rollback | §20.8, §27.16 | P2 | To do | Unit tests for graph, hooks and rollback |
| Compose reliability (zero-downtime, limits) | §27.15 | P2 | Compliant | Command/state tests + targeted cgroups fixtures: per-web-service zero-downtime (config-hash diff, `-next` candidate, C2/C3 compensations), normalized cgroup limits, §8.4 ineligibility traced; byte-exact goldens on the FROZEN v1 rendering and both config-hash values (ADR-053), since a reordered flag would re-fingerprint and therefore recreate every existing container. **No DinD proof**: the single E2E journey deploys `docker_image` applications only, compose never runs in it (ADR-026/028 — one journey, deliberately) |
| Built-in uptime monitoring | §27.17 | P2 | Compliant | Module tests for checks, thresholds and alerting |
| Local CLI (debug: browser login, contexts, ls/logs/shell/port-forward/tunnel, typed console, MCP bridge) | §12, §5.7 | P2 | Compliant | ADR-031/032/033/043; `login` (poll+PKCE, `--with-token` fallback), `context list\|current\|use\|remove`, `ls`, `logs` (snapshot/`-f`/`--deployment`), `shell`, `port-forward` (containers only, ADR-069), `tunnel open|ls|close` (declared external endpoints and the team's tunnel sessions), `db` (postgres/mysql/redis/mongo), `mcp` stdio bridge, `.akerdock` per-directory defaults; module tests (login state machine, tunnel mux, REF/contexts, dir config) + manual shell/forward validation |
| Local CLI deploy (`akerdock up`) | §12, §27.18 | — | **Withdrawn** | ADR-070 supersedes ADR-018: never implemented, and the CLI's reach is now decided by the typed command tree. Nothing to test, nothing to remove — no code was ever written |
| Notifications: routing/aggregation | §11, §27.19 | P2 | Compliant | Flapping/debounce tests + quiet hours (midnight-straddling, `critical` always crosses); outbox cursor, 7 channels (webhook/Slack/Discord/SMTP/Resend/Telegram/Pushover), deferred digest, SMTP proven E2E against a real relay |
| Observability/terminal | §3.8, §5.7, §13 | P2 | Compliant | OTLP metrics/traces + opt-in Prometheus exposition (ADR-008), redacted audit diffs; web terminal (WS, ADR-024) with passkey step-up for the server shell, idle/max-duration bounds, bridge state machine unit-tested, xterm.js bundled |
| Multi-server HA for one app | §3.3, §27.4 | P3 | To do | Spike + manual validation (Swarm not reimplemented, ADR-004) |
| Server agent: outbound observation push (waker merged) | §18.1, §18.3, §21.2 | P2 | Compliant | ADR-040/041; agent_tokens + SSH-injected enrollment, push loop (docker events, wakes, heartbeat; bounded, at-least-once), persistent WebSocket channel `akerdock-agent-v1` (presence = connection, acked frames, POST fallback, SSH last), `/agent/v1` ingestion (server-scoped), helper ensured on every server, threat-model §3.4bis, application slept/woken events + live UI, agent presence in the server API/UI, unit tests |
| Resource access protection (auth wall) with narrow public routes | §20.4.4, §21 | P2 | Compliant | ADR-042/049/050; `none`/`basic_auth`/`sso` on applications and inline Compose services; exact, `:name` template and segment-bounded prefix exceptions with explicit methods; per-service `x-akerdock.access_public_routes`, inherited by previews; proxy-IR protected/public split, app-scoped cookie + authorize/callback ritual, API + UI, unit tests |
| Built-in MCP server (read-only) | §12 | P3 | Compliant | ADR-043/044; JSON-RPC over Streamable HTTP on `/mcp`, ten read-only tools scoped per team (50/100 pagination), API-token and OAuth 2.1 auth (metadata, DCR, PKCE, session consent), `akerdock mcp` stdio bridge, CIMD client identity by default with DCR behind an instance opt-in (ADR-044), instance toggle off by default, unit tests |
| CLI bastion: tunnels to declared external endpoints | §5.7, §12, §23.3 | P3 | Compliant | ADR-045; `external_endpoints` resource (exact host:port, egress server, optional project/environment scope), admin-level declaration permission separated from `port-forwards:open`, mint without a client-supplied address, unchanged `akerdock-tunnel-v1` wire, two per-endpoint profiles — `standard` (today's tunnel) and `sensitive` (default: dashboard access request with a mandatory reason, fresh second factor per rbac §5, 4 h window (single grant capped at 8 h) renewable without a total, each renewal costing a fresh reason + fresh factor and extending live sessions, the grant's expiry being the session deadline (ADR-032's 4 h ceiling kept for `standard` only), revocation tearing down sessions, team notification), ADR-032's tunnel idle timeout raised 15 → 30 min for every tunnel (terminal unchanged), `authorized_until` announced at open and every automatic close reported to the CLI with an actionable reason (`grant_expired` added to `terminal_end_reason`), unit tests (validation, scope authorization, target XOR, shared team cap, teardown on deletion, both profiles, grant window, step-up factor selection, audit) |
| Scoped RBAC: role assignments per project and environment | §15, §16.3, §23.1, §27.7 | — | Abandoned | ADR-046 built it, **ADR-047 withdrew it** the next day: authorization became the hardest part of the platform to hold in one's head, for an expressiveness a separate team already provides. One role per member per team; the team is the isolation boundary (§23.1). Kept from the work: the token↔creator intersection of rbac-matrix §4.2, which had never been implemented (a token no longer outlives its creator's authority). The per-resource access view was removed with it: with one role per team it repeated Team → Members on five screens. Re-assessable upon proven demand. |
| Applying a configuration without rebuilding (`skip_build` deployment) | §5.3, §20.2, §21.1 | P2 | Compliant | ADR-048; `deployments.skip_build` + `config_apply` trigger, current artifact and deployed commit inherited (no clone, no build, no new artifact, no pruning), compose services reusing the image tagged for that commit, `skip_build` on `POST /applications/{uuid}/deploy` and the new `POST …/previews/{uuid}/redeploy` (409 with nothing deployed, 422 with `force_rebuild`), Actions menu in the dashboard (Deploy / Rebuild no-cache / Recreate / Restart / Stop) for applications, stacks and PR previews, unit tests (artifact reuse, mutually exclusive flags, menu behavior) |
| Docker runtime adapter (official SDK, typed commands to the agent) | §18.1, §18.2 | P2 | In progress | ADR-051/052; `internal/dockerruntime` single adapter (SDK signatures, typed errors, stream demux, stats formulas, `fake` recorder, `make test-docker` integration tier), waker/agent migrated off the hand-rolled socket client; command channel live (`akerdock-agent-v2`, `internal/agentwire` frames, executor + `AgentConns.Runtime`, wakerSpec 6, `akerdock.docker.runtime.ops` counter, unit + in-process rail tests); agent ensured on every ready server (`ListReadyServers`); read-only handler slice migrated (app/preview/proxy logs incl. SSE follow, live metrics computed from raw stats, port-forward container IP); worker→api relay live (`/agent/v1/relay` bridged onto the api's channels, `internal/agentrelay` source authenticated by the target server's agent token, `AKERDOCK_RELAY_URL`, end-to-end tests) with application lifecycle as its first job-side consumer (start/stop/restart + stack sweep via typed calls); adoption migrated (whole-inventory list+inspect via `adoption.FromInspect`, image envs, re-inspect probe — compose-file reads stay SSH host-ops) and the scheduler's scale-to-zero sleep stops go through the channel (real stop failures now surface instead of `>/dev/null`); cleanup slice migrated (server cleanup steps typed with `PruneReport` figures, dead-candidate matching in Go, preview/application teardown via shared label sweeps — build cache, tmp purge, `df` and directory removals stay host-side; swept failures now surface instead of `>/dev/null`); exec slice migrated (bidirectional attach frames on the channel — the piece ADR-052 §5 reserved for the terminal slice: input chunks + EOF stdin close, relayed too; one-shot `execCapture` for scheduled tasks and deploy hooks with argv-only commands, exit codes from the exec inspect; the container terminal is a TTY exec attach with typed resize, the server shell stays SSH PTY forever); database and proxy lifecycles fully typed — the migration's first `ContainerCreate`: provisioning passes env (password included) in the typed create body over the channel (no argv, the host `db.env` file is gone), volume ensured with labels, §6.2 healthcheck/readiness polled control-plane-side with the container's last lines on timeout; config/TLS deposits, the postgres-uid probe and proxy bootstrap stay host-side; deploymentrun's whole run path typed — the zero-downtime pipeline included: per-request-auth pulls with progress streaming, typed create (runtime env in the body, `runtime.sh` gone), storages ensured with a typed empty-volume ownership fix, control-plane-side health verdicts with live log follow, candidate-IP resolve, stop+rename switch, typed crash-resume inspection, compensation and image GC (builds, git, deposits and the build server stay SSH); composedeploy typed with the ADR-053 hash migration: `akerdock.config_hash_v2` fingerprints the typed create body while the v1 command renderer survives frozen as a pure hash input (skip = v2 first, v1 fallback for pre-rollout containers, v1-decided skips logged for data-driven retirement) — per-service creates/pulls/aliases/promotions/one-shot waits/resume inspection all typed, `runtime.sh` and per-service env files gone from hosts, preview volume seeding via one-shot containers; service builds stay CLI; host-ops file family live (ADR-054): the helper mounts the whole `/var/lib/akerdock` tree (spec 7) and executes the pure-Go file vocabulary (`internal/hostops` — write/read/remove/stat/chown/copy/dir, agent-side path allowlist, atomic writes, absence-is-data reads) on the same channel — certificate sync/renewal fully off SSH, database config/TLS deposits + TCP route file + postgres-uid probe typed, application/preview/database tree removals and remnant inventory through the agent. tranche B live: server validation provisions the agent (new `provision_agent` step — requires AKERDOCK_IMAGE and a resolvable instance URL, waits for the channel to answer before anything else runs), `ProxyApplier` fully typed (atomic routing file writes + typed `wget` verification exec in the proxy container, all 8 call sites incl. `bootstrapProxy`'s FQDN route), proxy drift reconciliation and the STZ activity reads ride the channel (an unreachable channel now SKIPS sleep/wake decisions instead of reading absence as idleness), waker routing table deposits typed; tranche C live — the whole backup path rides the channel: the dump execs in the database container and streams gzipped+hashed to the host file agent-side (`ExecToFile` — size, digest and emptiness verdicts typed, no `stat`/`sha256sum` round-trip), restores gunzip agent-side into psql's stdin (`FileToExec`), presigned S3 transfers run agent-local (`FileToURL`/`URLToFile` — the URL travels in the encrypted command body, never argv), integrity checks are `FileHash`, the drill's scratch instance is a typed create with an explicit pull fallback and `pg_isready`/table-count probes as typed execs; a multi-gigabyte dump never crosses the control plane. SSH keeps only the bootstrap family (agent/proxy container converge), git/builds, adoption compose reads and the cleanup's `df`/builder-prune/tmp-purge. ADR-055 phase 1 live — the build server's registry family rides its own agent channel: typed `ImageTag`/`ImagePush` with per-request auth and streamed progress, post-build/post-push inspects typed, the pushed digest picked by push repository (the shell's blind index-0 could hand the target a base-image digest), and **`docker login`/`docker logout` deleted — no registry token ever lands in any host's `~/.docker/config.json`** (INV-003 closed on the registry side). ADR-055 phase 2 live for the dockerfile family — the agent drives the daemon's embedded BuildKit over a hijacked gRPC session (`hostops.BuildImage`, `dockerfile.v0` frontend on the mounted tree, moby exporter, +6.7 MiB binary): inline-Dockerfile, git-Dockerfile, static packaging and per-service compose builds all run typed with plain-text progress streamed as a channel stream, build-args and **BuildKit session secrets in the typed command body — `build.env` is gone from the host for every dockerfile build** (INV-003/§5.2), synthesized Dockerfiles/nginx confs deposited through the agent, the intermediate builder image removed typed; integration tier `make test-docker` covers the whole rail (session, args, secrets, labels) — **to run against a live daemon before release**. What SSH still speaks: nixpacks (drives the host CLI itself and still sources `build.env` — its plan-emission rework is ADR-055's noted follow-up), source acquisition (git, deploy keys), first-contact/bootstrap, cleanup's `df`/builder-prune/tmp-purge |
| The waker becomes the agent (helper renamed to what it is) | §18.1, §18.3 | P2 | Compliant | ADR-056; container `akerdock-agent` (legacy `akerdock-waker` kept as a network alias for pre-rename route files), CLI `akerdock agent` with a deprecated `waker` alias for one generation, `internal/agent`; legacy container removed at recreate via agentSpec bump, unit tests |
| Role inspection: a session sees the platform as another role | §15, §23.1, §23.4 | P2 | Compliant | ADR-058; simulated role on the session row (system or custom), permissions = real ∩ simulated (root's wildcard dropped, `instance_root` false while on), identity unchanged — no account impersonation, the audit still names the real actor; entering/leaving reserved to team admins + instance root and checked against the REAL membership (a session can never lock itself in), `/auth/session/view-as` outside the permission-checked API for the same reason, cleared on team switch and on session end, nothing granted at all when the simulated role stops resolving; unit tests (intersection, no elevation from a member, custom role + disappearance, admin-only entry, team switch clears) |
| Reviewer role: read-only inventory path to its previews | §15, §20.4, §23.1 | P2 | Compliant | ADR-059 (amends ADR-038's reviewer clause); `reviewer` = `projects:read` + `environments:read` + `applications:read` + `previews:read` — previews are only addressable per application, and the one-permission set left the role unable to discover any UUID. Read-only by construction (every key a `:read`, nothing beyond the `read` socle after expansion); no services/databases/deployments/app-logs/secrets. Dashboard degrades along the same lines (Projects drill-down, application Overview + Previews tabs only, write actions hidden); MCP discovery follows (3 read-only inventory tools). Unit tests (role set + read-only invariant, MCP discovery) |
| Dev ingress tunnels: a declared public URL relayed to a developer's machine | §4, §12, §21, §23 | P3 | In progress | ADR-060/061 (Proposed); `ingress_endpoints` (stable FQDN in `domains`, ingress server, pre-provisioned router + certificate at declaration, agent offline page), mint at the control plane / redeem at the agent (`akdi_`), byte-splice relay entirely on the ingress server (INV-007 holds), strict HTTP/3 → HTTP/2 → WebSocket transport fallback (`akerdock-ingress-http-v2`, compatible `akerdock-ingress-v1`), four persistent connections per HTTP transport so the transport's stream capacity stays above the 128 active bound (a single QUIC connection stalls past 99 instead of refusing), per-session keep-alive and bounded flow control, SSO wall by default with admin-level opt-out, unconditional noindex + force-HTTPS, exclusive occupancy, idle 30 min / ceiling 12 h, CLI re-dial on transport failure only; narrow revision of ADR-031's transport invariant; unit/module tests per ADR-060/061 §Verification |
| Managed proxy convergence and lock-out recovery | §3, §14.2, §20.1 | P2 | Compliant | ADR-062; `internal/scheduler/proxyconverge.go` (decision table, per-server backoff, flip to `unhealthy` past the threshold, unreachable agent and in-flight action both yielding), the lock-out acknowledgement on the proxy lifecycle endpoint and `akerdock proxy repair`, with unit tests on each. Drift reconciliation extended from the dynamic files to the container that reads them — intent `running` + container absent → re-bootstrap, present but stopped → start, exponential backoff and `proxy_observed_status = unhealthy` past a failure threshold, an explicit `stopped` never repaired; stopping or removing the proxy that routes the instance FQDN (reserved `00-control-plane` scope) requires an explicit acknowledgement naming the consequence and the recovery command, since passkey and OIDC sign-in are bound to that origin and an SSH port-forward authenticates nobody; `akerdock proxy repair` on the host re-renders the static config and converges the container without the dashboard. Unit tests per ADR-062 §Verification |
| Ingress attach hop over h2c (Traefik → agent) | §4, §12, §18.1 | P3 | In progress | ADR-063 (Proposed); the reserved attach router (`/.akerdock/ingress`) gets its own IR-generated Traefik service over `h2c://akerdock-agent:8080`, so ADR-061's multiplexed control and data streams survive the last hop instead of being re-serialized into one HTTP/1.1 connection per relayed stream; the agent's front serves cleartext HTTP/2 alongside HTTP/1.1 on its single internal port (the protocol is chosen by the connection preface, so it cannot be scoped to a path — the scoping lives in the IR), never sets `ReadTimeout` (Go arms it as a per-stream deadline on the HTTP/2 **request body**, the same cut ADR-061 removed at the Traefik level with `readTimeout: 0s`) and advertises 1024 concurrent streams, above the tunnel's 128 active + 512 queued admission bound. The **visitor** hop stays HTTP/1.1 by necessity: `Connection`/`Upgrade` are invalid HTTP/2 request headers (RFC 7540 §8.1.2.2) and no component in the chain performs the RFC 8441 translation, so an h2c visitor hop would break every relayed WebSocket. Unit tests (dedicated h2c service in the IR + no h2c outside ingress, prior-knowledge h2c attach answered `HTTP/2.0` with full duplex both ways, advertised stream bound, `ReadTimeout` guard) + the `ingress-endpoint` conformance golden |
| One transport ladder for every CLI-to-server tunnel | §5.7, §12, §21, §24.4 | P3 | In progress | ADR-064; ADR-061's probe → HTTP/3 → HTTP/2 → WebSocket ladder becomes the transport of port-forward, bastion and terminal as well as ingress, with the shared per-process state (probe cooldown, consecutive-failure budget and its operator-facing diagnosis, policy-refusal versus transport-failure classification) and the invariants that go with it (transport capacity above the admission bound, bounded opens, bounded keep-alive). Wire identifiers parameterised per access path (ADR-027) so no attach token crosses paths; choreography per path (`HTTPOrigin` ingress-only). Revises ADR-024's terminal clause — WebSocket remains the bottom rung, the terminal gains QUIC connection migration. Endpoints unchanged (ingress → agent per INV-007, the rest → control plane): who terminates a tunnel is a separate decision. Unit/module tests per ADR-064 §Verification |
| An attach token is spent by the session it opens, not by the request that tries | §5.7, §12, §23.2, §24.4 | P3 | To do | ADR-065; revises the redeem clause of ADR-032 and of ADR-024's terminal in what "single use" counts, not in the property it protects — a mint authorizes at most one live authenticated session, held by one attacher, re-takeable within the token's own 60 s TTL. Covers the two SQL-claimed attach paths (port-forward and terminal); ingress stays out, its claim being a mutex over agent memory. The attach key moves from per-rung to per-mint (today it is generated inside ADR-064's ladder loop, so every rung is a different attacher); the claim stays exactly one statement with the idempotence in its `WHERE`, two additive columns (`attach_key_hash`, `attach_seq`), `claimed_at`/`started_at` pinned to the first claim so a retry loop cannot buy duration; at most one live attach by supersession, the loser dying within one heartbeat on another replica; an abandoned attach stops finalizing the session (`ended_at IS NULL` would otherwise defeat the idempotent claim on its own); the WebSocket rung carries the key in a header and a keyless claim stores server-generated random bytes (the browser terminal being a keyless client). On the terminal, cross-replica supersession does not converge — it has no heartbeat column to carry a generation and no cut registry, a pre-existing gap that already makes mid-session revocation ineffective there — so it is named, scoped out and deferred rather than invented; its cap imprecision is bounded by `streamed_at` plus one sweep clause, the port-forward's 90 s standing as accepted. Supersedes the tactical CLI re-mint shipped ahead of it. Unit tests per ADR-065 §Verification |
| The attach answers before it dials | §5.7, §12, §24.4 | P3 | To do | ADR-066; revises the server-side timing of ADR-032's port-forward and of the terminal attach: policy facts held locally (token claim, attach key, protocol echo, store lookups, preview status, geometry, session bounds) keep their place before the response head with their `409`; reachability facts about two other machines (agent `ContainerInspect`, SSH handshake, or the terminal's `serverdial.Open` + `StartPTY` / exec-create + exec-attach — a container shell awaiting no SSH client at all) move behind it, so the head stops being a bet on someone else's SSH latency (`servers.ssh_timeout_seconds` defaults to 30 s against a 5 s client open budget). The SSH client is dialled eagerly and awaited lazily, ownership still held by the session request through an idempotent promise, the stream's await-plus-dial fitting inside the existing `EgressDialTimeout` so no client constant moves. New `target_unreachable` end reason + session control frame for a failure belonging to no stream (both wires growing a message field, the terminal's dropping it today); a stream arriving mid-resolution waits and is served rather than being refused, so no new status code and nothing to teach the CLI; the WebSocket rung keeps dialling before the upgrade, its open being bounded only by the command's context. Records that ADR-032's "existing pooled SSH connection" was never true — `serverdial.Open` handshakes per attach — while leaving pooling to its own decision. Supersedes the tactical per-path attach budget shipped ahead of it. Unit tests per ADR-066 §Verification |
| Tunnels and terminals are citizens of scale-to-zero | §5.7, §12, §20.4.3, §24.4 | P3 | Partial | ADR-067; the activity half is **built** (heartbeat and mint stamp activity for previews and applications, migration 00094; a vanished target ends the session as `target_stopped` within one beat instead of black-holing) on the port-forward, and owed on the terminal; the wake half is ahead of implementation on both. Revises ADR-036 §2 on both its clauses — what *counts* as activity and what may *start* a wake (the waker module stays the only thing that performs one) — and extends ADR-037 §3's `desired_status = running` gate from sleeping to waking. The mint asks for the wake — the only step carrying the caller's identity — stamps activity so the scheduler cannot re-sleep in the gap, and answers without blocking; the wait is paid inside the open session on the first stream, keeping a 60 s cold start out of any request ADR-065/066 are timing. The agent's waker module performs it through one typed `WakeResource` command (ADR-052/054 precedent), against two starters racing through a Compose stack with no shared single-flight. Ready = two separately bounded gates (the waker's own readiness ≤ 60 s, then the operation the session is about to perform retried rather than probed ≤ 15 s — a TCP dial for a tunnel, an exec attach for a terminal, covering a path that has no port); `waking`/`wake_failed` control frames and one `wake_failed` end reason; previews need the session's own permission alone, applications additionally `applications:lifecycle` whichever door asks (`applications:exec` deliberately not repurposed), audited against the resource; no opt-out flag; the server shell excluded up front. Obliges the terminal to grow a durable heartbeat and a presence registry, which it lacks entirely — hence no mid-session cut of a live terminal today, a pre-existing gap recorded here and parked for its own decision. Unit tests per ADR-067 §Verification |
| A live session is ended by its owner, or by a team admin | §5.7, §12, §21, §24.4 | P3 | To do | ADR-068 (Proposed); decides the surface ADR-067 §10 parked once the cut mechanism existed, and regularises what two shipped `DELETE` endpoints were already doing under a borrowed permission. Covers every session family, the terminal included — it had no listing and no close at all, while being the one session that can do arbitrary damage. "Mine" stays a row test on `user_id` rather than a predicate dressed as a permission; "team admin and above" becomes a new catalogue entry `sessions:manage` (socle `write`), landing in the computed admin set and absent from the explicit member/reviewer lists, because `Identity` carries no role and a maximal custom role is indistinguishable from `admin` by permission set. Re-pointing the shipped handlers at it narrows only. Choosing a permission is what makes ADR-058's intersection bite: an admin inspecting `member` cannot end a peer's session. New `admin_close` end reason (`revoked` claims something about your authorization; this claims something about one session), owner-closed-elsewhere staying `user_close`; audited under its own action name and recording the ended session's owner, not only the actor. Also obliges the repair of a defect it found: both bridges report `disconnect` when the heartbeat sees zero rows, so on a multi-replica deployment every reason persisted by a peer — `target_stopped`, `grant_expired`, `wake_failed` — reaches the developer as a network glitch. Unit tests per ADR-068 §Verification |
| The bastion is its own CLI command | §5.7, §12, §23.3 | P3 | Compliant | ADR-069; `akerdock tunnel open ENDPOINT [LOCAL_PORT]` takes the bastion off `port-forward`, which goes back to ADR-032's sentence — a container AkerDock deploys. One verb had carried two products since ADR-045 shipped its client work under the verb that already existed: positional arguments told apart by *shape* (both optional), a remote port that existed only to give the local one a place to be written, a mint body chosen by `strings.HasPrefix` on the path, and a `Short` that had to say "or". A group rather than a bare verb, because `GET /port-forward-sessions` and `DELETE /port-forward-sessions/{uuid}` shipped with ADR-045 and had **no CLI client**: `tunnel ls` (every target kind, ADR-045's own reasoning for the dashboard view — `--endpoint` and `--all` map onto the API's existing filters) and `tunnel close` (a client of the rule ADR-068 decides, stating none of its own). Endpoint named bare, as `ingress` names its own; `port-forward endpoint/…` refused immediately with an error naming the replacement, no alias. Nothing below the command line moves — same mint, `akdp_` token, `akerdock-tunnel-v1`, ADR-064 ladder, permissions and grant choreography — and no OpenAPI change. Unit tests per ADR-069 §Verification (REF-spelling refusal on both commands, body-less mint, OS-picked local port, grant replay, `ls` filters and rendering, `close`) |
| A CLI a developer can stay in: typed command groups | §5.7, §12, §26 | P2 | Compliant | ADR-070 (supersedes ADR-018); the tree becomes `akerdock <type> <verb> [NAME]` — the `type/name` REF is removed and each of `app`/`db`/`svc` owns its verb space, while commands targeting no type stay top-level (`login`, `context`, `whoami`, `list`, `tunnel`, `ingress`, `mcp`, `version`). New surface, all of it on endpoints that already exist (**no OpenAPI change, no migration**): `env list|get|set|unset` with `--pr` and `--apply` (ADR-048's `skip_build`), `deploy run|list|cancel|rollback` with `-f`, `restart|start|stop` under the three groups, `info`, `app preview list|redeploy|keep`, `app tasks list|run`, `app open`, `db console` (today's `akerdock db`), `db backups list|run`, `whoami` (no network call). Two absences are decisions: **no `preview approve`** (fork governance stays in the dashboard; this CLI is runtime and debugging) and **no `backups restore`** (a production overwrite does not belong behind a one-line confirmation). `run` (one-off, non-interactive) is deferred to its own decision — it is the one thing all four comparable CLIs have that needs an endpoint we lack. Conventions fixed once: `list` everywhere with `ls` aliased, `-a`/`-e`/`-p` short flags, and no compatibility alias — the old spelling fails naming the new one. `akerdock up` withdrawn (§27.18). Fixed in the same slice, without an ADR because they are gaps against a written contract: exit code `2` for usage errors (the binary always exits `1`) and validation of `-o` (an invalid value is silently ignored today). Unit tests per ADR-070 §Verification, including the asserted absences (`preview approve`, `backups restore`/`download`, `svc deploy rollback`) and the tree's own shape. Three surfaces the API turned out not to have, found in implementation and recorded rather than faked: no `--branch` on any deploy body, no `skip_build` for a compose stack (`POST /services/{uuid}/deploy` takes no body at all, so `svc env --apply` redeploys in full), and no component or preview target on the nine lifecycle endpoints — each would have been a flag the server silently ignores |
| The manual is Markdown, served filtered | §25.4, §12 | P2 | Compliant | ADR-072; `internal/docs/manual/*.md` (front-matter + `##` sections), embedded and parsed at boot into `GET /docs`, which returns only what the caller's gates allow (`?all=true` returns the rest marked `beyond_role`). Markdown renders to HTML produced by goldmark **without** raw-HTML support — markup in a source file is dropped, not passed through — and the dashboard binds it through Angular's sanitiser. `docs.content.ts` (1 725 lines of block literals) deleted; the §25.4 invariants now unit-tested in Go: front-matter completeness, unique ids, closed group list, reading order, gates that name a real section and a catalogued permission, icons that ship, routes that resolve, raw HTML dropped, and a manual that still covers the daily surface for a plain `member` |
| A preview URL answers before its container exists | §5.5, §20.4.4 | P2 | To do | ADR-073; the dynamic Traefik file is written when the preview reaches `queued` and points at the agent (the sleeping-preview target) through the preview's own middlewares, so the `basic_auth`/`sso` wall and `noindex` apply to the waiting page exactly as to the preview. A **pending** routing entry carries the state and the agent renders queued/building/failed, self-refreshing except on failure, `Cache-Control: no-store`. The entry is removed when `switch_routing` points the host at the container — the regression test is that the page can never sit in front of a preview that serves — and `sleeping`/`waking` previews stay with ADR-036's waking page. Applications and stacks deliberately out of scope. Unit tests per ADR-073 §Verification |
| Scheduled tasks: per-application crons, exec or GitHub workflow dispatch | §5.7, §19.2, §24.3 | P1 | Compliant | Spec amendment #15 + ADR-071 (the row §26.2 owed since the amendment). Exec kind: cron/aliases + per-task timezone (`cronexpr`, same authority as backup plans), overlap `skip\|queue`, missed-run `run\|skip` with traced `skipped` rows, per-task timeout, tail-clamped output history, `scheduled_task.succeeded\|failed.v1` events, run-now through the scheduler's own path, UI tab + `app tasks list\|run`; scheduler seeds and owns `next_run_at` (leader advisory lock, 30 s tick). Workflow kind (ADR-071): `workflow_dispatch` signed by the application's GitHub App with a repository-scoped installation token, ref resolved at fire time (task ref → app branch → repo default branch), inputs capped at 10, manifest gains `actions: write`, dispatch-accepted = success, never queue-retried (at most one dispatch per occurrence); CHECK constraints tie each field family to its kind (migration 00099); `internal/jobs/scheduledworkflow_test.go`, `internal/handlers/scheduledtaskkinds_test.go`, `internal/githubapp` dispatch tests. Compose stacks (P2) still pending — `container` column ready, handler resolves applications only |
| Search-engine visibility: opt-in per resource, never for the control plane | §4.5, §14.2, §20.4.4 | P2 | Compliant | `X-Robots-Tag: noindex, nofollow` generated natively as a `RouteGroup.Noindex` middleware (proxy-contract §4.7): unconditional on previews and ingress endpoints, per-resource and off by default in production (`runtime_configs.noindex`, `services.noindex`, migration 00090, applied by `ApplyRouting` without a deployment). The control plane sends the header on every response and is not a setting. **ADR-074** corrects the pairing that made the header unreadable: its `robots.txt` now allows the crawl, since `Disallow: /` forbids the fetch that reads the header and freezes an already-indexed URL in the index — found on a live instance still listed weeks after the header shipped. Unit tests in `internal/proxy` (middleware attached to every serving router) and `internal/httpserver` (no `Disallow` directive with a value, header present on `/robots.txt` itself) |
| Cloudflare tunnels / cloud provisioning / server patching | §3.2, §3.6 | — | Abandoned | ADR-027 (re-assessable upon proven demand) |

The allowed status is `To do | In progress | Partial | Compliant | Documented divergence | Abandoned`. A proof points to tests, screenshots, benchmark or ADR.

### 26.3 Definition of Done for a capability

1. Nominal behaviors and errors documented.
2. API and authorization model specified.
3. Unit and module tests at the owner level; the product keeps a
   single representative E2E journey (ADR-028).
4. Idempotence, retry, cancellation and crash recovery tested if long action.
5. Relevant logs, metrics, trace/correlation ID, audit and notifications present.
6. Up/down migration or release rollback procedure.
7. Operator and user documentation.
8. Parity matrix entry updated with proof.

## 27. Divergence points to be decided by ADR

These topics are tracked here with their status. The 26 points below are all settled (product orientations ratified on July 11, 2026) and formalized as ADRs in `docs/adr/` (§29.12); any revision of a decision goes through a new ADR that supersedes the old one.

1. **SSH push vs agent pull**: SSH provides initial parity; an outbound agent reduces inbound ports and enables Docker events, but introduces agent versioning, enrollment and upgrade.
   **Decision**: SSH first, outbound agent as a target to reduce the inbound surface. Associated port requirements: the proxy listens on **80/443 by default** but its listening ports **MUST be configurable per server** (e.g. 8080/8443 when an upstream reverse proxy already holds 80/443); the control plane is exposed on **a single port**, behind its own domain/DNS — one port, one certificate, one firewall rule.
2. **PostgreSQL queue vs Redis/NATS**: PostgreSQL simplifies self-hosting; a separate bus improves throughput but increases operations.
   **Decision**: durable **PostgreSQL** queue retained. The queue interface remains abstract in the code, but no external bus (Redis/NATS) is planned.
3. **Secrets in database vs Vault/SOPS/KMS**: start with envelope encryption, then expose an external secret store interface.
   **Decision**: AEAD envelope encryption (AES-256-GCM) in PostgreSQL, master key in a root-only file or an environment variable, key versioning and rotation (§23.2). Internal `SecretStore` interface from the start, but a single implementation shipped; Vault/KMS only upon validated demand.
4. **Standalone Docker vs orchestrator**: stabilize standalone; treat Swarm as deprecated compatibility and evaluate Nomad/Kubernetes only upon validated need.
   **Decision**: **standalone Docker confirmed as the runtime** (Engine/Compose/BuildKit). Kubernetes — including in an "embedded and transparent" form — is ruled out: it contradicts the value proposition (modest VPSes from 2 GB, reversible standard Docker objects §16.1(3), compose template catalog) and the abstraction would leak at the first incident (pods, PVC, ingress) in front of users who chose the platform precisely to not learn Kubernetes. Swarm is not reimplemented (deprecated compatibility at best, behind a P3 feature flag). An orchestrator will only be re-evaluated upon validated user need, via the runtime adapter contract (§18.1), and without ever being imposed on existing installations. The ADR will record this rejection and its reasons.
5. **Local build vs isolated rootless builders**: Docker socket parity is fast; the security target should isolate untrusted builds (rootless BuildKit/VM/microVM).
   **Decision**: builds via the server Docker's BuildKit in P0/P1 (parity); dedicated **rootless BuildKit builders mandatory for untrusted code** at the latest with approved-fork previews (§20.4.8). The build adapter contract is written from P0 so that the switch does not touch the deployment engine.
6. **Local rollback vs immutable registry**: prefer OCI digests kept in a registry for a reproducible rollback, with local fallback.
   **Decision**: if a registry is configured, each deployment is pushed and referenced by **OCI digest** (reproducible rollback to any retained version); without a registry, local retention of the last N images, explicitly protected from automatic cleanup (INV-015).
7. **RBAC**: an admin/member pair is too coarse as soon as a team shares a production environment; AkerDock introduces roles and permissions per project and environment.
   **Decision**: fine-grained RBAC retained, **à-la-carte permissions** model — each product action is a granular permission; a role is a named set of permissions, assignable at team, project or environment level. **Predefined system roles** provided: owner, admin, developer and **viewer (read-only)**; custom roles composable by team admins. To be specified in the RBAC/permissions matrix (§29.7).
8. **Push metrics vs pull/OpenTelemetry**: keep a lightweight agent but standardize on OTLP/Prometheus to avoid a proprietary protocol.
   **Decision**: **OTLP everywhere** — server agent, control plane and workers emit metrics/traces/logs in OpenTelemetry, with Prometheus exposure; no proprietary protocol.
9. **Proxy labels vs declarative configuration**: support parity labels, generate a common intermediate representation in order to test Traefik and Caddy identically.
   **Decision**: validated — common intermediate representation, labels supported for parity, shared Traefik/Caddy conformance fixtures (§29.6). Sequencing: **Traefik alone in P0**; Caddy arrives in P2 via the intermediate representation, whose fixtures exist from P0.
10. **One-click catalog**: import compatible templates in compliance with licenses, but version, validate and sign the catalog independently of the binary.
    **Decision**: catalog = **dedicated template repository** maintained by the project (versioned, validated, signed, refreshable independently of the binary) **+ user template repositories** — each team can register one or more Git repositories (public or private, via the existing keys/credentials) containing its own templates, with validation at import and on-demand resynchronization.
11. **Rich previews**: a minimal preview (one container + one public URL) is below the domain standard — complete compose environments, ephemeral data, TTL, access protection, Git checks.
    **Decision**: the preview feature is shipped enriched from the outset, the entire scope of §20.4 being priority — ephemeral compose, ephemeral data, TTL/caps/scale-to-zero, access protection by default, watch paths in preview, Git checks, forks on approval; the trigger controls (labels, comment commands, draft exclusion, cancellation of stale builds) are **options enableable per application**, disabled by default.
12. **Configuration as code**: a platform driven only by the UI is not reproducible; the exported YAML and an official Terraform provider make it possible.
    **Decision**: complete YAML export + idempotent apply with dry-run/diff, and official Terraform/OpenTofu provider built on the API. Requirements in §24.5.
13. **Adoption of existing resources**: no platform in the segment knows how to take control of an already-deployed container or compose stack.
    **Decision**: adoption without redeployment, previewed and reversible — it is also the entry path from any platform, since it only assumes standard Docker. Workflow in §20.7.
14. **Backups beyond databases**: backing up four SQL engines and ignoring application volumes leaves half the state on the table — and a restore never replayed is not a backup.
    **Decision**: encrypted/deduplicated volume backup, Redis and ClickHouse engines, and automatic restore drills. Requirements in §20.5.
15. **Compose reliability "by design"**: the structural pitfalls of §15 (stack zero-downtime, resource limits without effect) are addressed from the design stage.
    **Decision**: AkerDock MUST provide zero-downtime for the web services of compose stacks (per-service switchover behind the proxy) and MUST actually enforce the declared resource limits on compose resources.
16. **Coordinated deployment of an environment**: deploying resource by resource, without order or hooks, breaks any environment where a migration must precede an application.
    **Decision**: environment deployable as a unit — dependency graph, migration hooks before switchover, per-level atomic mode, opt-in automatic rollback on degraded health. Workflow in §20.8.
17. **Built-in uptime monitoring**: without an external check, the platform does not know whether what it deploys actually responds from the Internet.
    **Decision**: simple built-in HTTP/TCP checks — target, interval, failure thresholds, executed outside the monitored workload — with alerting via the existing notification channels (§11) and availability history per resource. No APM: the scope stops at up/down and latency.
18. **Deploying from the workstation (`akerdock up`)** — **WITHDRAWN by ADR-070**, never built. A deployment starts from a source the platform can fetch again: a Git ref or an image; a local folder is not one, and every safeguard the idea needed (a context digest standing in for a SHA, a deployment forbidden from enabling auto-deploy) existed only to compensate for that. The feedback loop it aimed at is served by `akerdock app deploy run` on a Git source, and the CLI's own reach is settled by ADR-070 instead.
19. **Notification routing and aggregation**: one message per event makes the channel unusable (a flapping server = dozens of alerts, so nobody reads anymore).
    **Decision**: routing rules per project/environment/severity toward the channels, **aggregation/debounce** of repetitive events (flapping), configurable quiet hours and deferred digest of non-critical events.
20. **Project license**.
    **Decision**: **Apache 2.0** — same license as the reference, maximum adoption and contributions, patent clause included. The competitive moat is the product, not the license; the "cloud fork by a third party" risk is accepted.
21. **Instance distribution** (concretizes §16.1(6)).
    **Decision**: **minimal 2-service docker-compose** — the AkerDock image (static Go binary in a distroless image, `all-in-one`/`api`/`worker` modes §18.2) + PostgreSQL. A single `docker compose up`, upgrade by tag change, standard PostgreSQL backups, a single exposed port (§27.1).
22. **Naming of predefined variables**.
    **Decision**: **`AKERDOCK_*` prefix only** (`AKERDOCK_FQDN`, `AKERDOCK_URL`, `AKERDOCK_BRANCH`, `AKERDOCK_PR_ID`…), **without an alias from any other platform**: own identity, no naming debt, a single name per variable — two names for the same value is a divergence waiting to happen. The `SERVICE_<TYPE>_<ID>` magic variable syntax is kept: it is functional, not tied to a brand.
23. **Migration from another platform**.
    **Decision**: **no proprietary import assistant** — generic resource adoption (§20.7) IS the entry path: AkerDock takes over the standard Docker containers, stacks, volumes and networks already in place, without knowing anything about the internal schema of the tool that created them. An import tied to a third-party format would become a debt to maintain with every version of that third party.
24. **Realtime transport**.
    **Decision**: **SSE** (Server-Sent Events) for logs, statuses and job progress — native reconnection, cursor resumption via `Last-Event-ID`, compatible with enterprise proxies; **WebSocket reserved for the terminal** (the only bidirectional stream). Everything goes through the control plane's single port. — **Revised by ADR-064** on the reservation clause only: bidirectional CLI tunnels (terminal included) share one transport ladder, HTTP/3 → HTTP/2 → WebSocket; the SSE half of this decision stands.
25. **Go/API technical foundation**.
    **Decision**: PostgreSQL access via **pgx + sqlc** (explicit SQL, compile-time-checked types — essential for the critical queue/leases/outbox queries), versioned SQL migrations; **spec-first** API with the **chi** router + **oapi-codegen**: the Go handlers and the UI's TypeScript client (§25.2) are generated from the same OpenAPI artifact (§24.1).
26. **E2E test strategy**.
    **Decision revised by ADR-028**: exactly **one product E2E journey**, in **Docker-in-Docker only**, after merge to `main`, on demand and before release; no E2E on pull requests and no nightly catalog. Deterministic rules are proven in unit/module tests. **Residual risk accepted and documented**: systemd, real reboots, firewalls, full disks and ARM64 are not covered by automation — these classes are validated manually on an ad-hoc basis.

## 28. Maintenance of this document

This PRD is the product source of truth: a requirement that is not in it is not a requirement. Any scope evolution is written here **before** the code (spec-first workflow, CONTRIBUTING.md), and any structuring decision gives rise to an ADR (`docs/adr/`). The §26.2 matrix carries the delivery status and expected proof of each capability.

---

## 29. Artifacts required before a complete implementation

This PRD defines the product and its guarantees, but does not by itself replace the engineering specifications. In order to be able to rebuild the platform without implicit decisions scattered in the code, the following deliverables are mandatory:

1. **Glossary and data dictionary**: all fields, types, constraints, default values, sensitive data and deletion rules. — **Delivered**: `docs/specs/data-dictionary.md`
2. **Versioned ERD**: cardinalities, team ownership, indexes, uniqueness constraints and migration strategy. — **Delivered**: `docs/specs/erd.md`
3. **OpenAPI v1**: schemas, errors, permissions, pagination, idempotence and examples for each endpoint. — **Delivered** (P0 scope + P1 core): `docs/specs/openapi-v1.yaml`
4. **Deployment engine specification**: plan per build pack, exact commands, remote directories, labels, names, timeouts, locks, retry and compensation. — **Delivered**: `docs/specs/deployment-engine.md`
5. **Compose specification**: supported subset, transformations, magic variables, networks, volumes, health checks and rejected cases. — **Delivered**: `docs/specs/compose-spec.md`
6. **Proxy contract**: intermediate representation, Traefik/Caddy generation, route priorities, certificates, atomic reload and conformance fixtures. — **Delivered**: `docs/specs/proxy-contract.md`
7. **STRIDE threat model** and RBAC/permissions matrix per action and resource type. — **Delivered**: `docs/specs/threat-model.md` + `docs/specs/rbac-matrix.md`
8. **Git/webhook protocols** per provider with signatures, events, installation permissions and preview scenarios. — **Delivered**: `docs/specs/git-webhook-protocols.md`
9. **Test plan**: coverage primarily unit/module and a single product E2E journey in Docker-in-Docker (decision §27.26, ADR-028); the OS/architecture matrix remains manually validated. — **Delivered**: `docs/specs/e2e-test-plan.md`
10. **Operator runbooks**: install/upgrade/downgrade, key rotation, PostgreSQL/queue outage, compromised server, restore, stuck cleanup and recovery of an orphaned deployment. — **Delivered**: `docs/runbooks/` (11 runbooks + index)
11. **License inventory/SBOM**: dependencies, helper images, one-click templates, logos and redistribution conditions. — **Delivered**: `docs/specs/licensing-sbom.md`
12. **Initial ADRs and revisions**: choices listed in §27, with decision, alternatives and consequences. — **Delivered**: `docs/adr/` (ADR-001 to ADR-028 + index)
13. **Design system and component catalog** (§25.3): tokens, light/dark themes, normalized visual states and documented components, versioned with the Angular UI. — **Delivered**: `docs/specs/design-system.md`

Implementation can start with a P0 vertical slice (team auth → adding a server → deploying an image → HTTPS domain → logs → safe deletion), while these artifacts are detailed iteratively. Full parity must not, however, be declared before they are covered.
