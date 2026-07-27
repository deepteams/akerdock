# Specification — Proxy module contract

> PRD §29.6 artifact (`docs/PRD.md`). The PRD is the source of truth; this specification details the proxy module contract: intermediate representation (IR), Traefik (P0) and Caddy (P2) generation, routing priorities, certificates, atomic apply and conformance fixtures. Where the PRD is silent, the retained value is marked **(proposed default)**.
>
> Mandatory consistency: the zero-downtime switchover mechanics (dynamic file per application, transient target via the candidate's IP, atomic tmp+mv apply, verification through the Traefik API then smoke request, file rollback) are defined in §7 of `docs/specs/deployment-engine.md` — this specification is its proxy-side contract; it does not redefine them. Structuring decisions: §27.9/ADR-009 (common IR, Traefik P0, Caddy P2, shared fixtures), §27.1/ADR-001 (configurable proxy ports, single-port control plane). Reference tables: `domains` and `proxy_config_revisions` (`docs/specs/data-dictionary.md` §8.4, §11.1).

---

## 1. Overview

### 1.1 Role of the proxy provider

The proxy module is a **replaceable capability** (§16.1(5), §18.1) exposed to the business services through a single contract. A **provider** (Traefik in P0, Caddy in P2) implements this contract; the routing logic — domains, paths, priorities, middlewares, certificates — lives in the **intermediate representation** (§2), never in proxy-specific code (ADR-009).

Contract operations (normative semantics, Go signatures not prescribed):

| Operation | Role | Reference |
|---|---|---|
| `DeployProxy(server)` | Create and start the proxy container on a server — only if the intent is `running`; a server is born with the intent `stopped` and the first start goes through `StartProxy` (onboarding §20.1 step 5) | §1.3 |
| `StartProxy` / `StopProxy` / `RestartProxy` | Lifecycle from the UI; `StartProxy` converges config **and** container from scratch (it is the nominal first start); stopping cuts all of the server's inbound traffic (explicit warning, PRD §4.1) | §1.3 |
| `UpgradeProxy(server, image)` | Recreation of the container with the new pinned image; "proxy outdated" notification (PRD §4.1, §11) | §1.4 |
| `GenerateStatic(ir) → files` | Static proxy configuration (entrypoints, ACME resolvers) from the server IR | §5.2 |
| `GenerateApp(ir, app, endpoint) → file` | Dynamic configuration file of an application; `endpoint` = candidate IP (transient form) or container name (stable form) — deployment-engine §7.2 | §5.3 |
| `Validate(files)` | Syntactic and semantic validation before any upload (target format schema + §3 rules) | §6.1 |
| `Apply(server, files) → revision` | Atomic apply (tmp + mv), `proxy_config_revisions` record with SHA-256 checksum | §6 |
| `Verify(server, expectations)` | Verification that the configuration took effect: proxy API + smoke request | §6.3 |
| `Rollback(server, revision)` | Re-application of the last `applied` revision | §6.4 |
| `RemoveApp(server, app_uuid)` | Removal of a resource's routing (precedes any workload deletion, §20.6): deletion of the `<app_uuid>.yaml` file + verification | §6.5 |
| `Status(server)` | Observed state of the proxy container (`proxy_observed_status`), image version, checksum drift (§18.3) | §1.3 |

The deployment worker **drives** the switchover; the proxy applies it (deployment-engine §1.2). The control plane is never in the path of application requests (INV-007).

### 1.2 One proxy per server

- Each server has **its own** proxy (PRD §3.3), of type `proxy_type` (`traefik` | `caddy` | `none`, `servers` table). `none` = server without routed inbound traffic (a build server, for example).
- The per-server type switch (PRD §4.1) = full regeneration from the IR for the new provider, deployment of the new container, removal of the old one. The IR is unchanged: that is precisely its reason for being.
- The proxy is connected (`docker network connect`, idempotent) to **every destination network** hosting a routed resource; the connection is verified in `preparing`/`switching` before any switchover **(proposed default)**.

### 1.3 The proxy is itself a managed container

Reference deployment (Traefik, P0):

```sh
docker run -d \
  --name akerdock-proxy \
  --restart unless-stopped \
  --network AkerDock \
  -p <proxy_http_port>:<proxy_http_port> \
  -p <proxy_https_port>:<proxy_https_port> \
  [-p <tcp_port>:<tcp_port>]... \                    # active TCP routes (§2.6, §5.6)
  -v /var/lib/akerdock/proxy/traefik.yaml:/etc/traefik/traefik.yaml:ro \
  -v /var/lib/akerdock/proxy/dynamic:/dynamic:ro \
  -v /var/lib/akerdock/proxy/acme.json:/acme/acme.json \
  -v /var/lib/akerdock/proxy/certs:/certs:ro \
  -v /var/lib/akerdock/proxy/auth:/auth:ro \
  --env-file /var/lib/akerdock/proxy/acme.env \        # DNS-01 credentials, 0600, if DNS-01 configured (§7.2)
  --label akerdock.managed=true \
  --label akerdock.type=proxy \
  --label akerdock.team_uuid=<server_team_uuid> \
  --health-cmd 'traefik healthcheck --ping' \
  --health-interval 5s --health-retries 3 \
  traefik:v3.5                                        # version pinned per AkerDock release (proposed default)
```

Normative points:

- **Listen ports configurable per server** (`proxy_http_port`/`proxy_https_port`, defaults 80/443, decision §27.1): the entrypoints listen directly on these ports inside the container and are published identically (`8443:8443`, never `8443:443`) — HTTP→HTTPS redirects thus emit the right port without rewriting **(proposed default)**.
- The proxy's local API (Traefik `:8080`) is **never published on the host**: it is only reachable via `docker exec` inside the container (verification §6.3, consistent with deployment-engine §7.2).
- The Docker socket is **not** mounted into the proxy: all configuration goes through the files (no Traefik docker provider; the parity labels are informational, §5.1).
- Directory tree under `/var/lib/akerdock/proxy/` (extension of deployment-engine §5.1, **(proposed default)** — except `acme.json` and `acme.env`, whose locations are **normative**, §7.2/§7.5):

```text
/var/lib/akerdock/proxy/
├── traefik.yaml              # generated static config (§5.2) — container recreated on every change
├── dynamic/                  # file provider, watch: true
│   ├── 00-certificates.yaml  # custom certificates (§7.3) — reserved name
│   ├── 00-control-plane.yaml # routing of the instance FQDN if this server hosts it (PRD §14.2) — reserved name
│   └── <app_uuid>.yaml       # ONE file per routed application/preview/component (§5.3)
│       .<app_uuid>.yaml.tmp      # atomic-apply temporary file (ephemeral)
│       .<app_uuid>.yaml.awake    # pre-generated "awake" variant (scale-to-zero, §8)
├── certs/                    # uploaded custom certificates (PRD §4.3)
├── acme.json                 # ACME storage (0600) — NORMATIVE location (§7.5)
├── acme.env                  # materialized DNS-01 credentials (0600) — NORMATIVE location (§7.2)
└── auth/<app_uuid>.htpasswd  # basic auth user files (0600, §4.2)
```

Lifecycle: `proxy_desired_state` (`running`/`stopped`) and `proxy_observed_status` (`servers` table); start/stop/restart from the UI with status and logs available (PRD §4.1). Stop = `docker stop` of the container (the files stay in place; start = restart without regeneration). Every operation is audited (§23.4).

### 1.4 Image upgrade

1. "Proxy image outdated" notification when the pinned version of the AkerDock release is newer than the container's (PRD §4.1, §11).
2. Upgrade = explicit action: `docker stop` (10 s grace) → `docker rm` → `docker run` with the new image, same mounts and ports. Traffic interruption of a few seconds, announced before confirmation **(proposed default)**.
3. Since `acme.json`, certificates and dynamic files live on the host, the upgrade loses no state.
4. Post-upgrade: global `Verify` (§6.3) on a sample of routes; failure → recreation with the previous image (the image is only pruned after a successful verification) **(proposed default)**.

---

## 2. Intermediate representation (IR)

### 2.1 Principles

- The IR is **the** single generation source: Traefik (P0) and Caddy (P2) derive from it deterministically (ADR-009). No IR field is specific to one proxy, except the `provider_raw` escape hatch (§4.6).
- A server's IR is built by the control plane from PostgreSQL (`servers`, `applications`, `domains`, `service_components`, `databases`, deployment snapshots) — never edited by hand.
- **Canonical serialization**: JSON, sorted keys, no timestamp nor non-deterministic field. Two identical business states produce a byte-for-byte identical IR — the foundation of the checksum and of drift detection (§18.3).
- **No secret value** in the IR: only references (`secret://…`), resolved at generation time into `0600` files on the server (INV-003).
- Versioned: `ir_version` integer, incremented on every incompatible change; the fixtures (§9) are tagged by version.

### 2.2 Schema (documented structure)

```yaml
ir_version: 1
server:
  server_uuid: "6d0f…"
  proxy_type: traefik            # traefik | caddy
  http_port: 80                  # proxy_http_port (§27.1) — e.g. 8080 behind an upstream proxy
  https_port: 443                # proxy_https_port (§27.1) — e.g. 8443
  acme:                          # present if at least one domain uses ACME
    email: "ops@example.com"
    ca_url: "https://acme-v02.api.letsencrypt.org/directory"   # staging in tests
    resolvers:
      - name: http01             # default resolver (§7.1)
        challenge: http-01
      - name: dns01-cloudflare   # deterministic name: dns01-<provider> (§7.2)
        challenge: dns-01
        provider: cloudflare     # Lego provider identifier (PRD §4.3)
        credentials_ref: "secret://team/<team_uuid>/dns/cloudflare"
apps: []                         # list of RouteGroup (§2.3), sorted by resource_uuid
tcp_routes: []                   # list of TCPRoute (§2.6), sorted by listen_port
certificates: []                 # list of Certificate (§2.5), sorted by first domain
```

### 2.3 `RouteGroup` — HTTP routing of a resource

One `RouteGroup` per routed application, preview or service component. It corresponds **exactly** to the scope of the dynamic file `<app_uuid>.yaml` (deployment-engine §6.1): generating a RouteGroup = generating a file; deleting it = deleting the file.

```yaml
resource_uuid: "9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01"
team_uuid: "…"
kind: application                # application | service_component | preview
routes:                          # one entry per `domains` row (fqdn, path) — data dictionary §8.4
  - fqdn: "app.example.com"      # no scheme, citext; server wildcard already materialized (§3.4)
    path: "/"                    # path-based routing (PRD §4.2)
    target_port: 3000            # `domains.target_port`, otherwise default ports_exposes
    strip_path: false            # true = strip the prefix before forwarding (proposed default: false)
    https:
      enabled: true
      force: true                # force_https per application (PRD §4.3, default true)
      cert: { resolver: http01 } # { resolver: <name> } XOR { ref: <certificates §2.5> }
    redirect_direction: both     # both | www | non_www — "Direction" (PRD §4.2, §3.5)
    middlewares: [auth, noindex] # refs to §2.4, application order defined in §4.7
service:                         # single target — the switchover replaces this block (deployment-engine §7.2)
  endpoint_type: container_name  # container_name (stable form) | container_ip (transient form)
  endpoint: "9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01"   # or "172.18.0.7" during switching
  scheme: http                   # http | https (TLS backend) — (proposed default: http)
middlewares: []                  # local definitions (§2.4)
scale_to_zero: null              # optional block (§8), previews first
```

Notes:

- `endpoint_type`/`endpoint` are provided by the worker (`GenerateApp(ir, app, endpoint)`): **candidate IP** during `switching`, **container name** (Docker DNS) in `finishing` — strictly consistent with deployment-engine §7.2 (steps 1–2 and 7).
- Multi-domain = several `routes` entries (one `domains` row each); `domain:port` = explicit `target_port` (PRD §4.2).
- A `kind: preview` carries the protection middlewares by default (§4.5) and the `scale_to_zero` set if enabled.

### 2.4 `Middleware` — definitions

```yaml
- name: auth                       # unique within the RouteGroup; generated name: <app_uuid>-<name>
  type: basic_auth                 # §4.1
  users_ref: "secret://team/<team_uuid>/app/<app_uuid>/basic_auth"   # htpasswd (bcrypt hashes)
- name: limit
  type: rate_limit
  average: 100                     # req/s on average
  burst: 200
  period: "1s"
- name: office-only
  type: ip_whitelist
  cidrs: ["203.0.113.0/24", "2001:db8::/48"]
- name: noindex
  type: custom_headers
  response: { X-Robots-Tag: "noindex" }
  request: {}                      # headers added toward the backend
- name: gzip
  type: compression
- name: sablier                    # escape hatch, out of conformance scope (§4.6)
  type: provider_raw
  provider: traefik
  payload: { … }
```

### 2.5 `Certificate` — certificates

```yaml
- id: "wildcard-preview"           # referenced by routes[].https.cert.ref
  type: acme                       # acme | custom | none
  resolver: dns01-cloudflare       # for acme
  main: "preview.example.com"
  sans: ["*.preview.example.com"]  # wildcard ⇒ DNS-01 resolver required (§7.2)
- id: "corp-example"
  type: custom                     # files uploaded to /var/lib/akerdock/proxy/certs (§7.3)
  cert_file: "certs/corp.example.com.pem"
  key_file: "certs/corp.example.com.key"
```

`type: none` = HTTP only (route without `https.enabled`). The self-signed fallback does not appear in the IR: it is a guaranteed behavior of the provider (§7.4).

### 2.6 `TCPRoute` — TCP proxying of databases (PRD §6.2)

`public_access_mode = tcp_proxy` mode of the `databases` table: public exposure of a database without a port mapping on its container (it never restarts for a public port change).

```yaml
- resource_uuid: "4e7a…"           # database UUID
  team_uuid: "…"
  listen_port: 15432               # databases.public_port — changeable without touching the database
  target: { container: "4e7a…", port: 5432 }
  idle_timeout_seconds: 3600       # databases.tcp_proxy_timeout_seconds — best-effort per provider (§5.6)
  tls_passthrough: true            # the database's TLS (PRD §6.3) passes through as-is (proposed default)
```

Constraint: `listen_port` unique per server, disjoint from `http_port`/`https_port` (validated API-side). The set of active `listen_port`s determines the published ports of the proxy container: **changing** it triggers a static configuration revision + recreation of the proxy container (§5.6) — the database itself does not restart (parity §6.2); **retargeting** (changing the database or its internal port) is purely dynamic, without any restart whatsoever **(proposed default)**.

---

## 3. Routing priority rules

The behavior must be **identical regardless of the provider**: priorities are therefore computed in the IR, never delegated to the proxy's heuristics (Traefik sorts by rule length: close but not contractual).

### 3.1 Priority formula (proposed default)

For each HTTP route:

```text
priority = 1000 × segments(path) + len(path)
```

where `segments(path)` = number of non-empty segments (`/` → 0; `/api` → 1; `/api/v2` → 2) and `len(path)` = length of the path in characters (`/` → 1). The higher the value, the higher the route's priority.

- **Most specific path first** (PRD §4.2): `/api/v2` (2007) > `/api` (1004) > `/` (1) — regardless of declaration order.
- The FQDN does not enter the formula: two different FQDNs never compete (exact host matching), and two routes with the same `(fqdn, path)` are **impossible** (§3.3).
- Redirect routers (www/non-www, §3.5) inherit the priority of the route they serve.

### 3.2 `domain:port`

The `domain:port` syntax (PRD §4.2) targets an **internal port** of the container (`domains.target_port`), not a listen port of the proxy. It creates no entrypoint: the route always listens on `http_port`/`https_port` and forwards to `endpoint:target_port`.

### 3.3 Forbidden collisions

- `(fqdn, path)` uniqueness **instance-wide** — `UNIQUE` constraint of the `domains` table (§8.4), which eliminates cross-team collisions and ambiguities (INV-002). Rejected at API validation, before any IR.
- Defense in depth: `Validate` (§6.1) fails if an IR contains two identical `(fqdn, path)` routes on one server — a colliding IR is never applied.
- TCP `listen_port`: uniqueness per server + disjunction from the HTTP/HTTPS ports (§2.6).

### 3.4 `<uuid>.domain` wildcard and previews

- The **per-server wildcard domain** (`servers.wildcard_domain`, sslip.io fallback, PRD §4.2) is resolved **at resource creation**: the control plane materializes an exact FQDN (`<uuid>.example.com`, `is_generated = true` in `domains`). The IR and the proxies never see a wildcard host rule — only exact hosts.
- Preview URLs (template `{{pr_id}}.{{domain}}`, `{{random}}`, PRD §5.6) follow the same path: exact FQDN per preview, its own `RouteGroup` (`kind: preview`, identity `(application_uuid, provider, pr_id)`), hence its own dynamic file and a switchover independent of production.
- Only **certificates** may be wildcard (`sans: ["*.…"]`, DNS-01, §7.2): one wildcard certificate covers N exact hosts.

### 3.5 www/non-www redirects ("Direction")

`redirect_direction` field (`both` | `www` | `non_www`, default `both` — data dictionary §8.3):

| Value | Generated behavior |
|---|---|
| `both` | The declared FQDN and its counterpart (`www.` added or removed) both serve the application (two routers to the same service) |
| `www` | The apex counterpart redirects `308` to `www.<fqdn>` (scheme and path preserved) |
| `non_www` | `www.<fqdn>` redirects `308` to the apex |

The counterpart is only generated if it does not collide with an existing `domains` row (the §3.3 uniqueness prevails; on conflict, the redirect is omitted and a validation warning is raised) **(proposed default)**. The HTTPS and www redirects are composed in the §4.7 order (HTTPS first: at most one visible redirect per request, to the final URL).

---

## 4. Mapped middlewares

### 4.1 Basic auth

- IR `type: basic_auth`, `users_ref` pointing to the secret store. At generation time, the value (htpasswd format, bcrypt hashes) is written to `/var/lib/akerdock/proxy/auth/<app_uuid>.htpasswd` (0600, SFTP — INV-003/012) and the middleware references this file. The hashes do **not** transit through `proxy_config_revisions.content` (which contains no secret, data dictionary §11.1) **(proposed default)**.
- Traefik: `basicAuth.usersFile: /auth/<app_uuid>.htpasswd`.

### 4.2 Rate limiting

- IR `type: rate_limit` (`average`, `burst`, `period`). Traefik: `rateLimit`. Contractual semantics: limiting by source IP **(proposed default)**; beyond → `429`.

### 4.3 IP whitelist

- IR `type: ip_whitelist` (`cidrs`, IPv4/IPv6, validated centrally §23.3). Traefik v3: `ipAllowList`. Outside the list → `403`.

### 4.4 Custom headers and compression

- `type: custom_headers`: response and/or request headers (`headers.customResponseHeaders` / `customRequestHeaders`). The `X-Forwarded-*` names are reserved for the proxy and rejected at validation **(proposed default)**.
- `type: compression`: Traefik `compress` (negotiated gzip/brotli).

### 4.5 Preview protection (§20.4.4)

For `kind: preview`, the control plane injects by default (according to `preview_protection`, default `basic_auth`):

1. a `basic_auth` middleware (credentials generated per preview, preview variable set) — or **signed link** validation in P2;
2. a `custom_headers` middleware with `X-Robots-Tag: noindex` — present **even if** `preview_protection = none` (public exposure remains unindexed) **(proposed default)**.

`preview_protection = none` is an explicit per-application choice (§20.4.4).

### 4.6 Extensibility

- Adding a middleware type = extending the IR (new `type`, possible `ir_version` bump) **and** providing its mapping for every provider **and** its fixtures (§9). A type without a complete mapping is refused.
- `type: provider_raw` escape hatch (ADR-009, "explicit extensions"): payload passed as-is to the named provider, **excluded from the conformance fixtures**, flagged in the UI as non-portable — a proxy switch reports it and ignores it.

### 4.7 Application order (deterministic, proposed default)

```text
force-https → www/non-www redirect → ip_whitelist → rate_limit → basic_auth → custom_headers → compression → provider_raw
```

This order is a contractual invariant tested by fixtures: a client outside the whitelist receives `403` before any authentication challenge; redirects precede everything else.

---

## 5. Traefik generation (P0)

### 5.1 File provider — the file is authoritative

Consistent with deployment-engine §7.1: routing is materialized as **one dynamic configuration file per application** — `/var/lib/akerdock/proxy/dynamic/<app_uuid>.yaml` — mounted into the container (`file` provider, `watch: true`). The Traefik **parity labels** are set on the final container in `finishing` (deployment-engine §7.2 step 7) for diagnostic and usage-compatibility purposes, but are **never read** by the proxy (no docker provider): the file is authoritative; it is what makes the switchover atomic and verifiable.

Naming conventions in the generated files (deterministic, INV-011):

| Traefik object | Name |
|---|---|
| HTTP router | `<app_uuid>-r<n>` (HTTPS) / `<app_uuid>-r<n>-web` (HTTP) — `n` = index of the route sorted by `(fqdn, path)` |
| www redirect router | `<app_uuid>-r<n>-www` |
| Service | `<app_uuid>` (a single service per RouteGroup) |
| Middleware | `<app_uuid>-<name>` (+ implicit middlewares `<app_uuid>-https-redirect`, `<app_uuid>-www-redirect`) |
| TCP router/service | `<db_uuid>-tcp` |

Mandatory header of every generated file: `# generated by AkerDock — revision <n> — DO NOT EDIT`; any manual edit is detected by checksum (§6.2, §18.3).

### 5.2 Static proxy configuration

`/var/lib/akerdock/proxy/traefik.yaml`, generated by `GenerateStatic(ir)`. Any change (ports, resolvers, TCP entrypoints) creates a revision and **recreates the container** (§1.4); routing changes go exclusively through the dynamic files (hot reload).

```yaml
# generated by AkerDock — revision 12 — DO NOT EDIT
api:
  dashboard: false
  insecure: true          # API on :8080, port NOT published on the host — reachable only
                          # via docker exec (verification §6.3)
ping: {}                  # container healthcheck (§1.3)

entryPoints:
  web:
    address: ":80"        # = servers.proxy_http_port (§27.1) — ":8080" if 8080 configured
  websecure:
    address: ":443"       # = servers.proxy_https_port — ":8443" if 8443 configured
  tcp-15432:              # one entrypoint per active TCPRoute (§2.6)
    address: ":15432/tcp"

providers:
  file:
    directory: /dynamic
    watch: true

certificatesResolvers:
  http01:                                  # default (§7.1)
    acme:
      email: "ops@example.com"
      storage: /acme/acme.json
      httpChallenge:
        entryPoint: web
  dns01-cloudflare:                        # generated if a certificate references it (§7.2)
    acme:
      email: "ops@example.com"
      storage: /acme/acme.json
      dnsChallenge:
        provider: cloudflare               # Lego provider; credentials via env (§7.2)
        resolvers: ["1.1.1.1:53"]          # the instance's dns_validation_server (§4.2, PRD §14.2)

log:
  level: INFO
accessLog: {}
```

> HTTP-01 requires that Let's Encrypt reach the server on public port **80**. If `proxy_http_port ≠ 80` (an upstream proxy holding 80/443, §27.1), the upstream must relay `/.well-known/acme-challenge/` to the configured port, or DNS-01 must be used instead — a constraint documented at domain validation **(proposed default)**.

### 5.3 Example 1 — simple HTTPS application

Application `9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01`, `app.example.com`, internal port 3000, `force_https = true`, `redirect_direction = both` with no declarable counterpart (no `www` generated here for brevity). **Stable** form (`finishing`); during `switching`, only the service URL differs (`http://172.18.0.7:3000`, candidate IP — deployment-engine §7.2).

```yaml
# /var/lib/akerdock/proxy/dynamic/9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01.yaml
# generated by AkerDock — revision 41 — DO NOT EDIT
http:
  routers:
    9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01-r0-web:
      entryPoints: [web]
      rule: Host(`app.example.com`) && PathPrefix(`/`)
      priority: 1
      middlewares: [9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01-https-redirect]
      service: 9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01
    9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01-r0:
      entryPoints: [websecure]
      rule: Host(`app.example.com`) && PathPrefix(`/`)
      priority: 1
      service: 9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01
      tls:
        certResolver: http01
  middlewares:
    9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01-https-redirect:
      redirectScheme:
        scheme: https
        permanent: true
  services:
    9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01:
      loadBalancer:
        servers:
          - url: "http://9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01:3000"   # Docker DNS via container name
```

### 5.4 Example 2 — multi-domain, path routing and non-www redirect

Application `b2d15c78-90ab-4cde-8123-456789abcdef`: `shop.example` → port 3000, `shop.example/api` → port 8081 (`domain:port`), `redirect_direction = non_www`.

```yaml
# /var/lib/akerdock/proxy/dynamic/b2d15c78-90ab-4cde-8123-456789abcdef.yaml
# generated by AkerDock — revision 42 — DO NOT EDIT
http:
  routers:
    b2d15c78-90ab-4cde-8123-456789abcdef-r0:
      entryPoints: [websecure]
      rule: Host(`shop.example`) && PathPrefix(`/`)
      priority: 1                                     # segments=0, len=1 (§3.1)
      service: b2d15c78-90ab-4cde-8123-456789abcdef
      tls: { certResolver: http01 }
    b2d15c78-90ab-4cde-8123-456789abcdef-r1:
      entryPoints: [websecure]
      rule: Host(`shop.example`) && PathPrefix(`/api`)
      priority: 1004                                  # segments=1, len=4 → wins over "/"
      service: b2d15c78-90ab-4cde-8123-456789abcdef-api
      tls: { certResolver: http01 }
    b2d15c78-90ab-4cde-8123-456789abcdef-r0-www:      # www.shop.example → shop.example (308)
      entryPoints: [websecure]
      rule: Host(`www.shop.example`)
      priority: 1
      middlewares: [b2d15c78-90ab-4cde-8123-456789abcdef-www-redirect]
      service: noop@internal
      tls: { certResolver: http01 }
    b2d15c78-90ab-4cde-8123-456789abcdef-r0-web:      # HTTP: HTTPS redirect for both hosts
      entryPoints: [web]
      rule: Host(`shop.example`) || Host(`www.shop.example`)
      priority: 1
      middlewares: [b2d15c78-90ab-4cde-8123-456789abcdef-https-redirect]
      service: noop@internal
  middlewares:
    b2d15c78-90ab-4cde-8123-456789abcdef-https-redirect:
      redirectScheme: { scheme: https, permanent: true }
    b2d15c78-90ab-4cde-8123-456789abcdef-www-redirect:
      redirectRegex:
        regex: "^https?://www\\.shop\\.example/(.*)"
        replacement: "https://shop.example/${1}"
        permanent: true
  services:
    b2d15c78-90ab-4cde-8123-456789abcdef:
      loadBalancer:
        servers:
          - url: "http://b2d15c78-90ab-4cde-8123-456789abcdef:3000"
    b2d15c78-90ab-4cde-8123-456789abcdef-api:
      loadBalancer:
        servers:
          - url: "http://b2d15c78-90ab-4cde-8123-456789abcdef:8081"
```

### 5.5 Example 3 — protected preview (basic auth + noindex + DNS-01 wildcard)

Preview of PR #123 of the application above, container `b2d15c78-…-pr-123`, FQDN `123.preview.example.com`, wildcard certificate `*.preview.example.com` (resolver `dns01-cloudflare`).

```yaml
# /var/lib/akerdock/proxy/dynamic/b2d15c78-90ab-4cde-8123-456789abcdef-pr-123.yaml
# generated by AkerDock — revision 43 — DO NOT EDIT
http:
  routers:
    b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-r0:
      entryPoints: [websecure]
      rule: Host(`123.preview.example.com`) && PathPrefix(`/`)
      priority: 1
      middlewares:
        - b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-auth      # ip_whitelist → rate_limit → auth → headers (§4.7)
        - b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-noindex
      service: b2d15c78-90ab-4cde-8123-456789abcdef-pr-123
      tls:
        certResolver: dns01-cloudflare
        domains:
          - main: "preview.example.com"
            sans: ["*.preview.example.com"]
    b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-r0-web:
      entryPoints: [web]
      rule: Host(`123.preview.example.com`)
      priority: 1
      middlewares: [b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-https-redirect]
      service: noop@internal
  middlewares:
    b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-https-redirect:
      redirectScheme: { scheme: https, permanent: true }
    b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-auth:
      basicAuth:
        usersFile: /auth/b2d15c78-90ab-4cde-8123-456789abcdef-pr-123.htpasswd
    b2d15c78-90ab-4cde-8123-456789abcdef-pr-123-noindex:
      headers:
        customResponseHeaders:
          X-Robots-Tag: "noindex"
  services:
    b2d15c78-90ab-4cde-8123-456789abcdef-pr-123:
      loadBalancer:
        servers:
          - url: "http://b2d15c78-90ab-4cde-8123-456789abcdef-pr-123:3000"
```

### 5.6 Example 4 — TCP route to a database

PostgreSQL `4e7a9b0c-1122-4334-8556-778899aabbcc`, internal port 5432, `public_port = 15432` (`public_access_mode = tcp_proxy`, PRD §6.2). Static prerequisite: entrypoint `tcp-15432` (§5.2) + port published on the proxy container.

```yaml
# /var/lib/akerdock/proxy/dynamic/4e7a9b0c-1122-4334-8556-778899aabbcc.yaml
# generated by AkerDock — revision 44 — DO NOT EDIT
tcp:
  routers:
    4e7a9b0c-1122-4334-8556-778899aabbcc-tcp:
      entryPoints: [tcp-15432]
      rule: HostSNI(`*`)                 # raw TCP passthrough; the database's TLS (PRD §6.3) passes through as-is
      service: 4e7a9b0c-1122-4334-8556-778899aabbcc-tcp
  services:
    4e7a9b0c-1122-4334-8556-778899aabbcc-tcp:
      loadBalancer:
        servers:
          - address: "4e7a9b0c-1122-4334-8556-778899aabbcc:5432"
```

- Changing the **target** (another database, another internal port): dynamic file only, applied hot.
- Changing the **public port**: static revision + proxy recreation (§2.6) — the database does not restart; the UI announces the few-second interruption of the proxy.
- `idle_timeout_seconds`: applied if the provider supports it; Traefik v3 has no per-TCP-router idle timeout — the value has no effect there (documented divergence, tested in fixtures as "not guaranteed"); Caddy (layer4) applies it. The default 3600 s timeout remains carried by the IR for capable providers.

### 5.7 Reserved file `00-control-plane.yaml` — instance FQDN

When the server hosts the instance (`servers.is_localhost`) **and** an instance FQDN is configured (`instance_settings.fqdn`, PRD §14.2), the proxy bootstrap converges the reserved file `00-control-plane.yaml`: the dashboard is served behind the proxy with an automatic certificate (`certResolver: http01`), forced HTTPS redirect, single route `Host(<fqdn>) && PathPrefix(/)` to the control plane.

- Revision scope: `00-control-plane` — same mechanisms as the applications (checksummed revision §6.2.4, atomic apply §6.2, verification §6.3, drift reconciliation §18.3). The name is reserved: application scopes are UUIDs, no collision possible.
- Target: `http://host.docker.internal:<AKERDOCK_INSTANCE_PORT>` — the proxy container is started with `--add-host=host.docker.internal:host-gateway` (the control plane is a compose service on another Docker network, unreachable by container name). The targeted port is the port **published on the host** (`AKERDOCK_INSTANCE_PORT`, default `AKERDOCK_PORT` — instance-config §2.1): under compose the process listens on 8080 inside its container, but it is the host port of the mapping that the route must reach.
- Convergence at bootstrap only (proxy start/validation/recreation), idempotent: no revision is created if the desired content is identical to the last applied revision. FQDN removed or server no longer hosting the instance → routing removal (empty revision, §6.5).
- FQDN removal or value change: taken into account at the next proxy bootstrap (restart from the server page), not hot — the setting is rare and the restart costs a few seconds.

---

## 6. Atomic apply and verification

Contract for applying a dynamic file — identical whether the caller is the deployment worker (`switching`/`finishing`, deployment-engine §7.2 steps 3–4 and 7) or a configuration mutation (domain added, middleware changed, routing removal):

### 6.1 Validation before upload

`Validate` runs control-plane-side, before any remote mutation: target format schema (Traefik YAML / Caddy JSON), `(fqdn, path)` uniqueness within the IR (§3.3), resolved middleware/certificate references, ports within `1..65535`. An invalid IR produces **no** revision.

### 6.2 Atomic write and checksum

1. Deterministic generation of the content from the IR; computation of the **SHA-256**.
2. `INSERT proxy_config_revisions` (`server_id`, `revision` = n+1, `proxy_type`, `content`, `checksum_sha256`, `status = 'generated'`) — the `content` never contains a secret (data dictionary §11.1; the htpasswd files go through separate files, §4.1).
3. SFTP upload to `/var/lib/akerdock/proxy/dynamic/.<app_uuid>.yaml.tmp` then `mv -f` to `<app_uuid>.yaml` — atomic rename on the same filesystem: Traefik never sees a partial file.
4. Drift reconciliation (§18.3): periodically and before each switchover, the checksum of the remote file is compared with the last `applied` revision; a drift (manual edit) is reported and corrected by re-apply **(proposed default)**.

### 6.3 Waiting for effect and verification

Consistent with deployment-engine §7.2 step 4:

1. **Traefik API**: polling every 1 s, max **30 s**, via `docker exec akerdock-proxy wget -qO- http://127.0.0.1:8080/api/http/services` (HTTP routes) and `/api/http/routers`, or `/api/tcp/routers` + `/api/tcp/services` (TCP routes) — until the expected router and endpoint are seen (exact service URL, including the candidate IP during `switching`).
2. **Smoke request** through the proxy, from the server itself:
   `curl -fsS -o /dev/null --max-time 5 --resolve <fqdn>:<proxy_port>:127.0.0.1 http://<fqdn><health_path>` (and `https://` variant with `-k` if the certificate is not yet issued — the self-signed fallback answers, §7.4) **(proposed default)**.
   For a TCP route: connection test `nc -z 127.0.0.1 <listen_port>` **(proposed default)**.
3. Success → `UPDATE proxy_config_revisions SET status = 'applied', applied_at = now()`.

### 6.4 File rollback on failure

Failure of step 6.3 (API timeout or failed smoke request):

1. `status = 'failed'` + `error` on the revision.
2. Re-application of the content of the **last `applied` revision** of the same scope, through the same tmp + mv + verification mechanism; the faulty revision moves to `rolled_back` once the old configuration is re-verified.
3. In the context of a deployment switchover: this is compensation **C2** (deployment-engine §9.1) — the file points back to the old container, which never stopped running (INV-005); an immediate local retry is allowed before C2 (deployment-engine §9.2).
4. If the re-application itself fails (proxy down, disk full): the server enters a routing anomaly — alert, "priority actions" entry, reconciliation by a dedicated job; never a deletion of the last known-good file.

### 6.5 Routing removal

`RemoveApp`: deletion of `<app_uuid>.yaml` (+ `.awake`, associated htpasswd files), verification via the API that the application's routers and services are gone, dedicated traced revision. Routing removal always **precedes** stopping the workload (§20.6).

---

## 7. Certificates

### 7.1 ACME HTTP-01 (default)

- Automatic Let's Encrypt issuance and renewal (PRD §4.3) via the `http01` resolver (§5.2). Renewal is handled by the proxy itself (Traefik renews ~30 days before expiry) — no control plane action.
- Precondition validated when the domain is added: DNS resolution of the FQDN to the server via the instance's validation DNS (`dns_validation_server`, default `1.1.1.1`, PRD §4.2); blocking warning otherwise **(proposed default: non-blocking warning, the user can force — DNS may converge afterwards)**.

### 7.2 DNS-01 (wildcard)

- Required for any wildcard **certificate** (PRD §4.3). One `dns01-<provider>` resolver per DNS provider used on the server, `provider` = **Lego** identifier (cloudflare, route53, ovh, hetzner, …).
- **A `wildcard_domain` without a DNS-01 credential is accepted** (spec amendment): the domain then only serves as a **naming template** — each host assigned under the wildcard gets its **own individual certificate via HTTP-01** (§7.1, `certResolver: http01` per router). Accepted trade-offs, to be shown to the operator: each host must be publicly reachable on the server's HTTP port (ACME HTTP-01 requires it), and the CA's issuance limits apply **per host** (~50 certificates/registered domain/week at Let's Encrypt) — heavy preview usage can exhaust them, whereas a wildcard certificate consumes only one.
- **Credentials**: stored in the secret store (envelope encryption, §23.2 — `cloud_credentials` table, referenced by `certificates.dns_credential_id`, data dictionary §6.7), referenced by `credentials_ref` in the IR; materialized at generation time into `/var/lib/akerdock/proxy/acme.env` (**normative location**, 0600, SFTP) under the variable names expected by Lego (e.g. `CF_DNS_API_TOKEN=…`), injected into the proxy container via `--env-file` (§1.3). Never in `proxy_config_revisions.content`, never in argv (INV-003/012). Rotating a credential = regeneration of the file + recreation of the container.
- A wildcard certificate is requested via `tls.domains` on a router that references it (example §5.5).

### 7.3 Custom certificates

- Upload (UI/API) of the PEM files into `/var/lib/akerdock/proxy/certs/` (0600; the private key never leaves the server — not in the database, data dictionary §11.1), declared in the reserved dynamic file:

```yaml
# /var/lib/akerdock/proxy/dynamic/00-certificates.yaml
# generated by AkerDock — revision 45 — DO NOT EDIT
tls:
  certificates:
    - certFile: /certs/corp.example.com.pem
      keyFile: /certs/corp.example.com.key
```

- Traefik selects the certificate by SNI; a route whose `Certificate` is `type: custom` has no `certResolver` (it carries a plain `tls: {}`). Expiry is monitored by the control plane (reading the PEM): alert at D-30/D-7 **(proposed default)**.

### 7.4 Self-signed fallback

- Contractual behavior (PRD §4.3): as long as no valid certificate is available (ACME issuance in progress or failed), the proxy serves its **default self-signed certificate** — Traefik does so natively — rather than refusing the connection. HTTP traffic and the route remain functional; the corresponding fixture (§9) validates "TLS always answers, even without an issued certificate".

### 7.5 Storage and alerts

- ACME storage: `/var/lib/akerdock/proxy/acme.json` (**normative location**, 0600), included in the instance/server backup scope (PRD §7.5) — losing it is not serious (re-issuance) but costs Let's Encrypt rate limit.
- **Issuance failure alert**: the control plane monitors issuance after an ACME route is applied — presence of the certificate in `acme.json` (or Traefik API) within **10 min (proposed default)**; otherwise, event `proxy.certificate_issue_failed.v1` (outbox §24.2) → notification (PRD §11) with the cause extracted from the proxy logs (challenge failure, rate limit, CAA…). The self-signed fallback (§7.4) keeps being served in the meantime.
- Let's Encrypt rate limits: the instance uses the staging CA in the DinD E2E (§27.26); `ca_url` is configurable in the IR (§2.2).

### 7.6 Synchronization to the `certificates` table

The control plane maintains an **observed reflection** of each server's certificate state in the `certificates` table (data dictionary §6.7): after every `Apply` touching a TLS route and during the periodic reconciliation (§18.3), the worker reads `acme.json` and the PEMs in `certs/` over SFTP (metadata only — private key material is never brought back) and updates covered domains, issuer, `not_before`/`not_after`, status (`pending`/`issued`/`renewing`/`failed`/`expired`/`revoked`), last error and `observed_at`. This reflection feeds the API inventory (`GET /servers/{uuid}/certificates`, `expiring_within_days` filter), the D-30/D-7 expiry alert (§7.3) and the issuance failure alert (§7.5); it is **never** a source of truth — the real state remains `acme.json` and the server's files. `POST /certificates/{uuid}/renew` forces a re-issuance via an audited job (backup then targeted removal of the `acme.json` entry, proxy restart — cf. certificates runbook), followed by a resynchronization of the reflection.

---

## 8. Scale-to-zero (SHOULD, §20.4.3) — proposed mechanism

This entire chapter is **(proposed default)**: the PRD only requires that the proxy SHOULD support scale-to-zero (stopping an idle container, waking on the first request), previews first.

> **Locked by [ADR-036](../adr/ADR-036-scale-to-zero-waker.md)** with two clarifications relative to the defaults below: (1) the waker is a **mode of the single binary** (`akerdock waker`, same image — ADR-021), not a second artifact; (2) the **two-variant switch** of §8.2 is discarded in favor of a **single variant** where the waker stays **permanently inline** in front of the STZ resources (route always to the waker). The waker thus sees all the traffic and **timestamps the last activity** in a local file that the control plane reads over SSH — inactivity is measured exactly, without parsing access logs. Sleeping/waking boils down to `docker stop`/`docker start`, without touching the dynamic file. The rest of the chapter (§8.1 waker role and confinement, §8.3 limits) stands.

### 8.1 Server-local "waker" component

A helper container `akerdock-waker` (`akerdock.type=helper`, project image, pinned per release) is deployed on the servers where at least one resource has `scale_to_zero` enabled. It listens on the internal network (never published on the host) and has the local Docker socket, restricted by its code to starting containers labeled `akerdock.managed=true`.

Why not the control plane: INV-007 (the control plane does not proxy application traffic) and push architecture (§18.1 — the server never contacts the control plane). Waking therefore works even with the control plane down; the control plane is informed after the fact through observed-state reconciliation (§18.3, §21.2).

### 8.2 Sleep and wake

**Sleep** (control plane, on the inactivity TTL §20.4.3 — measured on the proxy's access logs or the Sentinel metrics):

1. Generate **two** variants of the dynamic file: the "sleeping" variant (RouteGroup service → `http://akerdock-waker:8080`, request header `X-AkerDock-Wake: <app_uuid>` added by middleware) and the normal "awake" variant, deposited at `/var/lib/akerdock/proxy/dynamic/.<app_uuid>.yaml.awake`.
2. Apply the sleeping variant (§6), then `docker stop` the container. Desired state: `sleeping`.

**Wake** (waker, on the first request):

1. Incoming request → `docker start` (idempotent); state `waking`. The wake set is **every container of the resource** (ADR-037 §5) — for a compose stack, the whole stack minus one-shot jobs, started **in the stack's topological start order** (compose-spec §2.6), each container ready before the next starts: a stopped dependency loses its Docker DNS alias, so waking only the routed service would boot it against a name that no longer resolves.
2. Wait for the healthcheck (`healthy`, or `running` stable for 10 s without healthcheck), timeout **60 s (proposed default)**.
3. `mv -f .<app_uuid>.yaml.awake <app_uuid>.yaml` (atomic switch, same mechanism as §6.2 — the waker generates nothing, it swaps files pre-generated by the control plane).
4. **Replay**: the held request is forwarded to the container (hold-and-forward); subsequent requests take the normal path as soon as Traefik picks up the file. For a browser client (`Accept: text/html`) the waker MAY serve an auto-refreshing waiting page instead of the hold.

```text
States: sleeping ──(1st request)──▶ waking ──(healthy + file swap)──▶ running
             ▲                                                          │
             └──────────────(inactivity TTL, control plane)◀────────────┘
```

### 8.3 Accepted limits

- **Wake timeout**: beyond 60 s (long cold start), `504` + explicit error page; the waker **stops the containers this wake started** (rollback, reverse order) so the resource returns to `sleeping` — never left half-awake in a restart crash-loop the control plane believes asleep.
- **Request body**: hold-and-forward capped at **1 MiB (proposed default)**; beyond, `503` + `Retry-After: 5` (the client replays).
- **WebSockets**: an upgrade during `waking` is held then proxied once the container is healthy, within the timeout limit; long-lived WS moreover prevent inactivity detection — a resource with persistent WS is a poor scale-to-zero candidate (documented).
- The first response byte incurs the full startup latency; scale-to-zero is enabled per resource, previews first, never implicitly in production.

---

## 9. Conformance fixtures

The fixtures (ADR-009, existing **from P0 onwards**) guarantee that Caddy (P2) will reproduce Traefik's behavior exactly: **same fixtures, two providers**.

### 9.1 Format

```text
tests/proxy-conformance/
├── cases/<case_name>/
│   ├── ir.json                  # full server IR (single input, pinned ir_version)
│   ├── expected/traefik/        # expected output of GenerateStatic + GenerateApp
│   │   ├── traefik.yaml
│   │   └── dynamic/<app_uuid>.yaml…
│   ├── expected/caddy/          # filled in at P2 (absent = case not yet ported, CI flags it)
│   └── assertions.yaml          # HTTP/TCP behavioral assertions (§9.2)
└── harness/                     # echo backend + assertion client (DinD, §27.26)
```

Two test levels, both mandatory for a provider to be conformant:

1. **Golden files**: `Generate*(ir.json)` must produce `expected/<provider>/` byte-for-byte (determinism, basis of the checksum).
2. **Behavioral assertions**: the proxy is launched in Docker-in-Docker (§27.26) with the generated config and an echo backend (answers with its received headers + `X-Backend: <name>`); each assertion is a real request. This level is what makes providers interchangeable: two different configs, a single behavior.

### 9.2 Assertion format

```yaml
# assertions.yaml — "multi-domain + path" case
- name: "most specific path first"
  request: { method: GET, url: "https://shop.example/api/users", insecure_tls: true }
  expect:  { status: 200, backend: "api-8081" }
- name: "non-www redirect"
  request: { method: GET, url: "https://www.shop.example/x?y=1", follow_redirects: false, insecure_tls: true }
  expect:  { status: 308, headers: { Location: "https://shop.example/x?y=1" } }
- name: "force HTTPS"
  request: { method: GET, url: "http://shop.example/", follow_redirects: false }
  expect:  { status: 308, headers: { Location: "https://shop.example/" } }
- name: "preview: unauthenticated refused, never indexed"
  request: { method: GET, url: "https://123.preview.example.com/", insecure_tls: true }
  expect:  { status: 401 }
- name: "preview: authenticated, X-Robots-Tag present"
  request: { method: GET, url: "https://123.preview.example.com/", basic_auth: "preview:s3cret", insecure_tls: true }
  expect:  { status: 200, headers: { X-Robots-Tag: "noindex" }, backend: "pr-123" }
- name: "TCP database"
  tcp:     { connect: "127.0.0.1:15432" }
  expect:  { connected: true, backend: "pg-echo" }
```

`insecure_tls: true` accepts the self-signed fallback (the fixtures do not issue real certificates; a dedicated case uses Pebble/staging for the full ACME flow). URLs use the case's ports (`http_port`/`https_port` of the IR) — the "non-standard ports 8080/8443" case is part of the minimal set.

### 9.3 Minimal case set (P0)

simple HTTP app; forced-HTTPS app; multi-domain + paths + `domain:port`; www/non-www redirects (3 directions); protected preview (auth + noindex); materialized `<uuid>.domain` wildcard; middlewares (rate limit 429, ip_whitelist 403 before auth — §4.7 order); custom certificate; self-signed fallback; TCP database; proxy ports 8080/8443; switchover (two sequential `ir.json`: transient IP then stable name — verifies that no request fails during the swap); routing removal (404 after `RemoveApp`).

---

## 10. Caddy mapping (P2, sketch)

Goal: demonstrate that the IR is sufficient — not a complete specification (it will accompany the P2 delivery, validated by the §9 fixtures).

| IR element | Traefik | Caddy |
|---|---|---|
| `(fqdn, path)` route | `Host && PathPrefix` router + priority | site block `fqdn` + matcher `path /api/*`; the matcher order is **emitted sorted** by the §3.1 priority (Caddy evaluates in JSON config order) |
| Service (IP or name endpoint) | `loadBalancer.servers[].url` | `reverse_proxy <endpoint>:<port>` |
| force HTTPS | web router + `redirectScheme` | native (auto-HTTPS redirect); disabled per site if `https.enabled: false` (targeted `auto_https off`) |
| www redirect | `redirectRegex` | `redir` in a site dedicated to the counterpart |
| basic_auth | `basicAuth.usersFile` | `basic_auth` (bcrypt) |
| rate_limit | `rateLimit` | `rate_limit` module (extension embedded in the AkerDock Caddy image) |
| ip_whitelist | `ipAllowList` | `remote_ip` matcher + `abort`/`respond 403` |
| custom_headers / compression | `headers` / `compress` | `header` / `encode gzip br` |
| ACME HTTP-01 / DNS-01 | certResolvers | native / Caddy DNS modules (credentials via env, same `acme.env` file) |
| Custom certificate | `tls.certificates` | `tls /certs/x.pem /certs/x.key` |
| Self-signed fallback | Traefik default certificate | `tls internal` as fallback |
| TCP route | TCP router + entrypoint | `layer4` module (AkerDock image) — supports `idle_timeout` (§5.6) |
| Configurable 80/443 ports | entrypoint addresses | global `http_port` / `https_port` |

Sketch for the §5.3 example (Caddyfile; the real generation will emit Caddy JSON, more deterministic):

```caddyfile
{
  http_port 80
  https_port 443
  email ops@example.com
}
app.example.com {
  reverse_proxy 9f3c2a1e-7b44-4c1d-9e2a-1f0b8c6d5a01:3000
}
```

Accepted operational difference: Caddy does not watch a directory of files — the atomic apply (§6) goes through its local **admin API** (`POST /load`, transactional, native rollback on invalid config), called via `docker exec` like the Traefik API; the §6 contract (generate → checksum → apply → verify → rollback, one scope per application) is unchanged, only the apply transport differs. The per-application files remain the on-disk materialization (assembled before `POST /load`), preserving diagnostics and checksum reconciliation.

---

## 11. PRD traceability

| Section of this spec | PRD sections / specs |
|---|---|
| 1 | §4.1, §16.1(5), §18.1, §20.1, §20.6, §27.1, ADR-001, deployment-engine §1.2 |
| 2 | §4.2–4.3, §6.2, §18.3, §27.9/ADR-009, data dictionary §8.3–8.4, §9.1 (`databases`), deployment-engine §6.1, §7.2 |
| 3 | §4.2, §5.6, §17 (INV-002/011), data dictionary §8.4 |
| 4 | §4.1, §20.4.4, §23.2–23.3, ADR-009 |
| 5 | §4.1–4.3, §6.2, §14.2, §27.1, deployment-engine §5.1, §6.1, §7.1–7.2 |
| 6 | §17 (INV-005), §18.3, §20.6, data dictionary §11.1, deployment-engine §7.2, §9 |
| 7 | §4.3, §7.5, §11, §14.2, §23.2, §24.2, data dictionary §6.7 (`certificates`) |
| 8 | §17 (INV-007), §18.1, §20.4.3, §21.2 |
| 9 | §26.1, §27.9/ADR-009, §27.26 |
| 10 | §4.1, §26.1 (P2), §27.9/ADR-009 |
