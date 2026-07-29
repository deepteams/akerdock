# Specification — AkerDock local CLI (`akerdock`)

> Contract of the command-line client. Upstream sources of truth: PRD (`docs/PRD.md`)
> §12 (API/CLI), §5.7 (operations: logs, terminal), §27.18 (`akerdock up`, v2);
> ADR-018 (local deployment, v2), ADR-021 (single binary), ADR-024 (real time:
> SSE + WebSocket), ADR-027 (access paths), ADR-031 (CLI auth), ADR-032 (TCP tunnel),
> ADR-033 (Cobra). API contract: `docs/specs/openapi-v1.yaml`. Authorizations:
> `docs/specs/rbac-matrix.md`. Threats: `docs/specs/threat-model.md`.
>
> Scope: **v1 "debug"** — `login`, contexts, listing, logs (including `-f`), shell,
> TCP port-forward, basic typed console, and the local MCP bridge (`mcp`, ADR-043).
> Deployment from the workstation (`akerdock up`, ADR-018) and env/domains/keys
> management belong to **v2** and are not specified here.
>
> Defaults not settled by the PRD are marked **(proposed default)**.

---

## 1. Scope and non-goals

**v1 goals.** Give a developer, from their workstation, debug access to their resources
without exposing them: authenticate (including via SSO/OIDC), list resources, read
logs (snapshot and streaming), open a shell in a container, establish a TCP tunnel to
a service (database, redis, …), and a typed console for convenience.

**v1 non-goals.** Deployment (`up`, rollback, `deploy`), management of env variables,
domains, keys, backups, members. The CLI **NEVER reimplements** business logic:
it consumes the public API (PRD §18.2), nothing else.

## 2. Transport invariant

- The CLI **only connects to** the manager FQDN of the active context, **on 443** (80
  only for a possible redirect→HTTPS). No other network destination.
- The CLI **opens no** inbound or loopback port — only outbound requests.
- `shell` and `port-forward` open a `wss://<manager>/…` on 443 (standard WebSocket
  Upgrade headers, like the web terminal, which already traverses proxies and load-balancers).
  The tunnel to the **target server** is established on the manager side (SSH) — never on the client side.
- Everything **MUST** work through an intermediate proxy/LB; heartbeats (20 s)
  keep the WebSockets open despite LB idle-timeouts.

## 3. Commands

`REF` designates a resource: `app/<name|uuid>`, `db/<name|uuid>`, `svc/<name|uuid>`,
`preview/<pr|uuid>`, and — for `port-forward` only — `endpoint/<name|uuid>`, a declared
external endpoint (ADR-045, §7.1). Short forms are accepted (`ep/…`). **The team is the
token's**, not a per-command choice — see the note under the global flags.

### 3.1 Server modes (this binary, not the client)

| Command | Role |
|---|---|
| `akerdock serve [all-in-one\|api\|worker\|scheduler]` | Server modes (ADR-033); the argument falls back to `AKERDOCK_MODE`, then `all-in-one`. |
| `akerdock healthcheck` | Probe for the compose healthcheck. |
| `akerdock waker` | Scale-to-zero waker (ADR-036), deployed as a helper container next to the workload. |
| `akerdock version` | Print the build version. |

### 3.2 Client commands

| Command | Role |
|---|---|
| `akerdock login [--url URL] [--context NAME] [--scopes read,write] [--with-token] [--no-browser]` | Authentication (§5). |
| `akerdock logout [--context NAME] [--revoke]` | Clears the local credential; `--revoke` also deletes the token server-side (needs a resolvable team — see below). |
| `akerdock context list \| current \| use NAME \| remove NAME` | Multi-instances. |
| `akerdock ls [apps\|databases\|services\|servers]` | Listing; default: applications + databases + services. `previews` is **not** a transversal listing in v1 (previews are read per application). |
| `akerdock logs [REF] [--component C] [-n LINES] [-f] [--deployment [UUID]]` | Container logs (snapshot or `-f` streaming) or logs of a deployment. `REF` is optional when `.akerdock` names a default application. |
| `akerdock shell [REF] [--component C]` | Interactive shell in the container (§6). `REF` optional, as for `logs`. |
| `akerdock port-forward [REF] [[LOCAL:]REMOTE] [--component C] [--pr N]` | TCP tunnel (§7); `--pr N` targets the preview instance of PR N instead of production. `REF` may be omitted when `.akerdock` names a default application; the remote port is omitted for an `endpoint/…`, which froze its own host and port at declaration (§7.1). |
| `akerdock db REF [--component C] [--pr N]` | Convenience: opens a forward and launches the local client of the detected engine (§8); accepts a standalone database (`db/…`) or a **database service of a compose stack** (`app/… -c C`), with `--pr N` targeting the preview. |
| `akerdock mcp [--url URL] [--token T]` | Runs the built-in MCP server over **stdio** for a local assistant (ADR-043): a bridge to this instance's `/mcp` endpoint, read-only tools, credentials from the current context by default. The instance-side surface is off unless enabled there. |

**Global (persistent) flags.** `--context NAME`, `--team`, `--project`, `--application`,
`--environment`, `-o table|json` (`json` = raw API objects, for scripting), `--quiet`.
`NO_COLOR` honored. **Exit codes**: `0` success, `1` error, `2` usage.

> **`--team` does not switch teams.** An API token is bound to one team at creation
> (rbac-matrix §4.1), so every command acts in the token's team whatever this flag
> says; it is only used to locate the token to delete in `logout --revoke`. To work
> in another team, log in again into a separate context with a token of that team.
> This is the CLI counterpart of the dashboard's team switcher (rbac-matrix §3.6),
> which moves a *session* — the CLI holds a token, and tokens do not move.

## 4. Contexts and storage

`~/.akerdock/` (directory `0700`):
- `config.yaml` (`0600`) — `current_context` + `contexts: {name → {url, fqdn, team_uuid}}`.
- `credentials.yaml` (`0600`) — `{context → token}`, kept separate so the config can be
  inspected/shared without exposing tokens.

A context = one instance + the team its token belongs to. `login` creates or updates the
current context. The OS keychain is a **SHOULD (v1.x)** (see ADR-031 for the accepted gap).

**Per-directory config (`.akerdock`).** A `.akerdock` file (YAML) placed in a repository
sets the CLI defaults for that directory and its subdirectories — found by walking up
the tree from the current directory, `.git`-style (a `.akerdock` directory, like the
global `~/.akerdock`, is ignored: only a **file** counts). It **never contains a
token** (those stay in `~/.akerdock/credentials.yaml`), so it is committable. Fields,
all optional: `context` (name of a global context), `team`, `project`, `application`
(default target), `environment`, `component` (default compose service for `logs`,
`shell`, `port-forward` and `db`).

**Precedence (MUST).** Each parameter resolves in this order, from strongest to weakest:

```
CLI flag  >  AKERDOCK_* env variable  >  .akerdock (directory)  >  ~/.akerdock (global)
```

Env variables: `AKERDOCK_CONTEXT`, `AKERDOCK_TEAM`, `AKERDOCK_PROJECT`,
`AKERDOCK_APPLICATION`, `AKERDOCK_ENVIRONMENT`, `AKERDOCK_COMPONENT`. Thus, from a repository
with a `.akerdock` that points to `context:` and `application:`, `akerdock logs` (without REF, without
`--context`) targets the default app of the configured instance; an explicit `--context`/`REF`
always wins.

## 5. Login (ADR-031)

**Poll + confirmation code bound by PKCE** flow — no open port, everything outbound over HTTPS.

1. The CLI generates `verifier` + `challenge = SHA-256(verifier)`, `POST /auth/cli/start
   {challenge, name}` → `{request_id, user_code, verify_url, interval, expires_in}`.
2. Displays `user_code` + the URL, opens the browser on `/cli/authorize?request_id=…`
   (or prints it if `--no-browser`).
3. Browser consent: login (password/passkey/OIDC), team, permissions, **and
   confrontation of the `user_code`**; approval → `POST /auth/cli/approve` (session + CSRF).
4. The CLI polls `POST /auth/cli/token {request_id, verifier}` → upon approval, an `akd_`
   token (TTL 30 d, name `cli — <user>@<host>`) is written with `0600`.

**Requirements** (normative detail in ADR-031): single-use, hashed codes; `verifier`
never transmitted to the browser; `SHA-256(verifier) == challenge` checked at exchange;
POST+CSRF approval; `user_code` match required; permissions ⊆ session;
default `read,write`, never `root`/`deploy`/`read:sensitive` by default; everything audited.
`--with-token` fallback for machines without a browser.

## 6. Shell

**Full** reuse of the existing terminal sessions (§5.7, §24.4, ADR-024): `POST
/applications/{uuid}/terminal-sessions` (+ `component`) mints a single-use attach
token, the CLI opens `wss://<manager>/terminal/ws?token=…&cols=&rows=`, puts the local TTY in
raw mode and bridges the binary stream ↔ PTY, forwarding window size
changes. Idle timeout, max duration, heartbeat and guaranteed kill apply unchanged. The CLI
**neither defines nor specifies** a new protocol here.

## 7. Port-forward (ADR-032)

`akerdock port-forward db/varuna 15432:5432` establishes a tunnel from `127.0.0.1:15432` **local
to the CLI process** (CLI loopback listener, outside the §2 invariant which only concerns
outbound network connections) to port `5432` of the target container, via the manager.

- **Mint**: `POST /{applications|databases|services}/{uuid}/port-forwards` (+ previews),
  `x-required-permission: write`, body `{port}`, target frozen and authorized at mint time, cap
  `port_forward_limit` (default **10**) → `PortForwardSession{uuid, token akdp_, websocket_path
  "/tunnel/ws", expires_at}`.
- **Redeem**: `GET /tunnel/ws?token=akdp_…` (outside the contract), subprotocol
  `akerdock-tunnel-v1`: one multiplexed WS per session, text control frames
  (`open`/`open_ok`/`open_err`/`eof`/`close` by `id`) + binary frames `[u32 id][payload]`.
- **Limits**: 32 streams/session, 1 MiB buffer/stream, idle 15 min, max 4 h, heartbeat
  20 s, guaranteed teardown, open/close audited.
- **Authorization boundary = the resource, not the port**: the target is a container
  authorized at mint time; every port of that container is reachable (Docker does not filter
  host→container), just as `shell` gives the whole container. Stated, not hidden.
- **Servers excluded**: no server-level `port-forward` (= `ssh -L`).

### 7.1 External endpoints (bastion, ADR-045)

`akerdock port-forward endpoint/prod-replica` tunnels to a target **outside** the
server — a managed database, an internal API — that an admin declared as an
`external_endpoint`. Three differences with a container forward:

- **No remote port argument, and no body on the mint**: the endpoint froze its host and
  port at declaration, so the client cannot name an address. Only the local port is the
  caller's to choose — and it may be omitted too, in which case the OS picks a free port
  and the CLI announces it. `akerdock port-forward endpoint/<name>` is therefore a
  complete command, which is why the REF/ports arguments are told apart by shape (a REF
  contains a slash) rather than by position.
- **`sensitive` endpoints require a live access grant**. The mint answers
  `access_request_required`; the CLI then opens the dashboard page that issues the grant
  (reason + fresh second factor) and **keeps replaying the mint** every 2 s until it goes
  through (10 min ceiling, Ctrl-C to give up) — the same choreography as `login`, so the
  developer neither hunts for a URL nor runs the command a second time. Only
  `access_request_required` is retried; any other error ends the wait at once.
- **The grant belongs to the human, and the CLI token spends it.** The request is made from
  a browser session (a token cannot re-authenticate, rbac-matrix §5), but the mint that
  follows comes from the CLI, authenticated by the token whose creator is that same person
  (`api_tokens.created_by`). Within the window, tunnels reopen after a reboot or a network
  change without another ceremony (ADR-045 §5).
- **The grant's expiry is the session deadline**: `authorized_until` is announced at open,
  and an automatic close reports its reason (`grant_expired` among them) instead of a bare
  disconnection.

## 8. Typed console (`akerdock db`)

Convenience on top of §7. `akerdock db REF` detects the resource's engine (postgres /
mysql / redis / mongo), opens an ephemeral port-forward and launches the corresponding local client
(`psql`, `mysql`, `redis-cli`, `mongosh`) preconfigured with the resource's credentials.

**Targets.** A standalone database (`db/<name>`) **or** a database service of a compose stack
(`app/<name> -c <service>`, engine read from the component, §9.2); `--pr N` targets the service of
the preview instance of PR N. For a compose service, credentials are read on a best-effort basis
from the **generated magic variables** (`SERVICE_USER_<ID>` / `SERVICE_PASSWORD_<ID>`,
§5.4): without `read:sensitive` they are redacted (`value: null`), in which case the CLI prints the
connection command and leaves the forward open rather than launching a client without
credentials. If the local client is missing, same fallback. The CLI **neither stores nor relays** a
cleartext password beyond launching the child process.

## 9. Security (delta to the threat model)

- **T — loopback interception**: neutralized by PKCE (the `verifier` never leaves the CLI),
  cf. ADR-031.
- **S — phishing of the consent page**: the confrontation of the `user_code` (generated by
  the CLI, displayed on both sides) breaks the classic device-flow vector.
- **E — tunnel/shell to an unauthorized container**: authorization at mint time (`write` on the
  resource), team-scoping (INV-001), target frozen at session creation.
- **Repudiation**: `start`/`approve`/`token`, shell and port-forward open/close,
  token creation and revocation — all audited (§23.4).
- **Storage**: `akd_` token at rest with `0600`, TTL 30 d, revocable (accepted keychain gap,
  ADR-031).

## 10. Audit and observability

Every action with a remote effect is audited with actor/token, IP, timestamp (§23.4): login
(success/failure), terminal session and port-forward open/close, revocation.
Shell keystrokes and tunnel bytes are **never** logged (§24.4).

## 11. Tests

In accordance with the test pyramid (ADR-026/028, test plan §2): deterministic logic
is proven with **unit/module tests** — `REF` parsing, context resolution, login state
machine (start/poll/approve/exchange, PKCE verification), tunnel multiplexing
(`open`/`eof`/`close` framing, stream cap, buffer). End-to-end shell and port-forward
are validated **manually** on an ad-hoc basis; the single E2E product journey
(Docker-in-Docker) is **not** extended for the CLI (ADR-028).
