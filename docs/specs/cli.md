# Specification — AkerDock local CLI (`akerdock`)

> Contract of the command-line client. Upstream sources of truth: PRD (`docs/PRD.md`)
> §12 (API/CLI), §5.7 (operations: logs, terminal), §27.18 (`akerdock up`, v2);
> ADR-018 (local deployment, v2), ADR-021 (single binary), ADR-024 (real time:
> SSE + WebSocket), ADR-027 (access paths), ADR-031 (CLI auth), ADR-032 (TCP tunnel),
> ADR-033 (Cobra). API contract: `docs/specs/openapi-v1.yaml`. Authorizations:
> `docs/specs/rbac-matrix.md`. Threats: `docs/specs/threat-model.md`.
>
> Scope: **v1 "debug"** — `login`, contexts, listing, logs (including `-f`), shell,
> TCP port-forward, bastion tunnels (`tunnel`), basic typed console, and the local MCP
> bridge (`mcp`, ADR-043).
> Deployment from the workstation (`akerdock up`, ADR-018) and env/domains/keys
> management belong to **v2** and are not specified here.
>
> Defaults not settled by the PRD are marked **(proposed default)**.

---

## 1. Scope and non-goals

**v1 goals.** Let a developer work from their workstation without opening the dashboard:
authenticate (including via SSO/OIDC), inspect what the team runs, read logs (snapshot and
streaming), open a shell or a typed console in a container, tunnel a TCP port without
exposing it — and, since ADR-070, **set variables, deploy, roll back, restart, and drive
previews, scheduled tasks and backups**. The split that used to run between "debug from the
terminal" and "everything else from the browser" is gone; what remains in the dashboard is
what belongs there.

**v1 non-goals.** Deploying a **local folder** (`akerdock up` is withdrawn, ADR-070 §3 — a
deployment starts from a source the platform can fetch again), managing domains, keys and
members, restoring a backup, and approving a fork preview: the last two stay in the dashboard
by decision, not by omission. A one-off non-interactive command (`run`) is owed and waits on
an endpoint that does not exist yet. The CLI **NEVER reimplements** business logic: it
consumes the public API (PRD §18.2), nothing else.

## 2. Transport invariant

- The CLI **only connects to** the manager FQDN of the active context, **on 443** (80
  only for a possible redirect→HTTPS). No other network destination.
- The CLI **opens no** inbound or loopback port — only outbound requests.
- `shell`, `port-forward` and `tunnel open` open a `wss://<manager>/…` on 443 (standard WebSocket
  Upgrade headers, like the web terminal, which already traverses proxies and load-balancers).
  The tunnel to the **target server** is established on the manager side (SSH) — never on the client side.
- Everything **MUST** work through an intermediate proxy/LB; heartbeats (20 s)
  keep the WebSockets open despite LB idle-timeouts.

## 3. Commands

The tree is **typed** (ADR-070): `akerdock <type> <verb> [NAME]`. A verb that acts on one
kind of resource lives under that kind's group — `app`, `db`, `svc` — and the **NAME is the
last positional argument**, optional wherever a default can stand in for it (an
application's `.akerdock`, §4). The `type/name` REF of earlier versions is **gone**: the old
spelling is refused with the command that replaced it, never resolved as a literal name.

A group offers **the verbs its type actually has**, and the API is not symmetric: a database
and a compose stack have no logs endpoint, a stack has neither terminal nor port-forward,
only an application has `rollback`, and a backup execution has no download. Each group's
`--help` is the authoritative list.

Commands that target no type stay at the top level: `login`, `logout`, `context`,
`whoami`, `list`, `tunnel`, `ingress`, `mcp`, `version`, `completion`. **The team is the
token's**, not a per-command choice — see the note under the global flags.

### 3.1 Server modes (this binary, not the client)

| Command | Role |
|---|---|
| `akerdock serve [all-in-one\|api\|worker\|scheduler]` | Server modes (ADR-033); the argument falls back to `AKERDOCK_MODE`, then `all-in-one`. |
| `akerdock healthcheck` | Probe for the compose healthcheck. |
| `akerdock agent` | Server agent (command channel, host-ops, scale-to-zero waker — ADR-036/052/056), deployed as a helper container next to the workload. `waker` remains as a deprecated alias. |
| `akerdock version` | Print the build version. |

### 3.2 Client commands

| Command | Role |
|---|---|
| `akerdock login [--url URL] [--context NAME] [--scopes read,write] [--with-token] [--no-browser]` | Authentication (§5). |
| `akerdock logout [--context NAME] [--revoke]` | Clears the local credential; `--revoke` also deletes the token server-side (needs a resolvable team — see below). |
| `akerdock context list \| current \| use NAME \| remove NAME` | Multi-instances. |
| `akerdock whoami` | Where this terminal is pointed and as whom: context, instance, team, stored scopes. **No network call**, and the token is never printed — the question is worth answering before a command that changes something. |
| `akerdock list [apps\|databases\|services\|servers]` | The one listing whose subject is the team rather than a kind: applications + databases + services by default. `ls` is an accepted alias, here and on every other `list` (§3). |
| `akerdock tunnel open ENDPOINT [LOCAL_PORT]` | TCP tunnel to a **declared external endpoint** — a target AkerDock does not run (§7.1). |
| `akerdock tunnel list [--endpoint NAME] [--all]` | The team's tunnel sessions, every target kind (§7.2). |
| `akerdock tunnel close SESSION_UUID` | Cuts a live tunnel session (§7.2). |
| `akerdock ingress ENDPOINT LOCAL_PORT` | Relays a declared public URL to a port on this machine (ADR-060). |
| `akerdock mcp [--url URL] [--token T]` | Runs the built-in MCP server over **stdio** for a local assistant (ADR-043): a bridge to this instance's `/mcp` endpoint, read-only tools, credentials from the current context by default. The instance-side surface is off unless enabled there. |

#### The `app` group

| Command | Role |
|---|---|
| `akerdock app list` | The team's applications. |
| `akerdock app info [NAME]` | One application: desired/observed status, health, components, last deployment (§9). |
| `akerdock app logs [NAME] [-c C] [-n LINES] [-f] [--deployment [UUID]] [--pr N]` | Container logs (snapshot or `-f`) or a deployment's build log. `--pr N` reads the preview instance of PR N instead of production. |
| `akerdock app shell [NAME] [-c C]` | Interactive shell in the container (§6). |
| `akerdock app port-forward [LOCAL:]REMOTE [NAME] [-c C] [--pr N]` | TCP tunnel to a container port (§7). The ports come first, the name last. |
| `akerdock app open [NAME] [--dashboard]` | Opens the public URL, or the resource's dashboard page. |
| `akerdock app restart\|start\|stop [NAME]` | Lifecycle (§10). |
| `akerdock app deploy run [NAME] [--skip-build\|--force-rebuild] [-f]` | Triggers a deployment; `--skip-build` applies the configuration without rebuilding (ADR-048), `--force-rebuild` is its opposite and the two are mutually exclusive; `-f` follows the build log. No `--branch`: no deploy body carries a ref. |
| `akerdock app deploy list [NAME]` | Deployment history. |
| `akerdock app deploy cancel DEPLOYMENT_UUID` | Cancels a running deployment. |
| `akerdock app deploy rollback [NAME] [--to UUID]` | Rolls back to a previous deployment. **Applications only** — no such endpoint exists for a stack. |
| `akerdock app env list\|get\|set\|unset KEY… [NAME] [--pr N] [--apply]` | Environment variables (§11). The keys come first, the application name last; `--apply` redeploys without rebuilding. |
| `akerdock app preview list [NAME]` | The application's PR previews. |
| `akerdock app preview redeploy\|keep --pr N [NAME]` | Redeploys a preview, or holds it against automatic destruction. **No `approve`**: authorizing a fork to run is project governance and stays in the dashboard (ADR-070 §2). |
| `akerdock app tasks list [NAME]` / `akerdock app tasks run TASK [NAME]` | Scheduled tasks, and running one on demand. |

#### The `db` group

| Command | Role |
|---|---|
| `akerdock db list` / `akerdock db info [NAME]` | The team's databases; one database. |
| `akerdock db console [NAME] [--app A -c C] [--pr N]` | Opens the engine's own client over an ephemeral forward (§8). With `--app`/`-c`, a database **service of a compose stack**. |
| `akerdock db shell [NAME]` | Interactive shell in the database container (§6). |
| `akerdock db port-forward [LOCAL:]REMOTE [NAME]` | TCP tunnel to the database port (§7). |
| `akerdock db restart\|start\|stop [NAME]` | Lifecycle (§10). |
| `akerdock db backups list [NAME]` / `akerdock db backups run [NAME] [--plan P]` | Backup plans and their executions; triggers one now. **No `restore`** (a production overwrite does not belong behind a one-line terminal confirmation) and no `download` (no endpoint serves the file). |

#### The `svc` group (compose stacks)

| Command | Role |
|---|---|
| `akerdock svc list` / `akerdock svc info [NAME]` | The team's stacks; one stack and its components. |
| `akerdock svc restart\|start\|stop [NAME]` | Lifecycle (§10). |
| `akerdock svc deploy run\|list\|cancel [NAME]` | Deployment and history. No `rollback` (the endpoint exists for applications only) and no `--skip-build` (`POST /services/{uuid}/deploy` takes no body). |
| `akerdock svc env list\|get\|set\|unset KEY… [NAME] [--apply]` | Environment variables (§11). `--apply` redeploys the stack in full: `POST /services/{uuid}/deploy` takes no body, so there is no `skip_build` to send. |

> A stack has **no `logs`, `shell` or `port-forward`** of its own: those endpoints exist for
> applications and databases only. Debugging a stack's container goes through the
> application that owns it.

**Global (persistent) flags.** `--context NAME`, `--team`, `--project`, `--application`,
`--environment`, `-o table|json` (`json` = raw API objects, for scripting), `--quiet`.
`NO_COLOR` honored. **Exit codes**: `0` success, `1` error, `2` usage — a usage failure is an
unknown command, an unknown or malformed flag, a missing or excess argument, or a flag value
outside its enumerated set (`-o` accepts `table` and `json`, and refuses anything else instead
of falling back). The distinction is what lets a script tell a typo from a deployment that
actually failed.

> **`logs --pr N -f` polls, it does not stream.** The API exposes a live stream for an
> application's containers (`/logs/stream`) but only a snapshot for a preview's
> (`docker logs --tail`, read on demand). With `--pr`, the CLI therefore repolls every
> 3 s and prints only the lines the previous window did not already end with. A line may
> be reprinted when the window jumps by more than one page; none is dropped.

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
(the default an `app` verb uses when no name is typed), `environment`, `component` (default
compose service for `logs`, `shell`, `port-forward` and the console).

Only the **application** has such a default: a repository declares the app it deploys, never
the database it talks to, so `db` and `svc` verbs always take a name.

**Precedence (MUST).** Each parameter resolves in this order, from strongest to weakest:

```
CLI flag  >  AKERDOCK_* env variable  >  .akerdock (directory)  >  ~/.akerdock (global)
```

Env variables: `AKERDOCK_CONTEXT`, `AKERDOCK_TEAM`, `AKERDOCK_PROJECT`,
`AKERDOCK_APPLICATION`, `AKERDOCK_ENVIRONMENT`, `AKERDOCK_COMPONENT`. Thus, from a repository
with a `.akerdock` that points to `context:` and `application:`, `akerdock app logs` (no name,
no `--context`) targets the default app of the configured instance. **A positional name always
wins over `-a/--application`**, which carries the default rather than the target;
`--context` likewise overrides the file.

Short forms: `-a` (application), `-e` (environment), `-p` (project) — the spellings every CLI
of the domain uses (ADR-070 §4).

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

`akerdock app shell [NAME]` and `akerdock db shell [NAME]` — the two types whose API has a
`terminal-sessions` endpoint. **Full** reuse of the existing terminal sessions (§5.7,
§24.4, ADR-024): `POST /{applications|databases}/{uuid}/terminal-sessions` (+ `component`
for an application) mints a single-use attach
token, the CLI opens `wss://<manager>/terminal/ws?token=…&cols=&rows=`, puts the local TTY in
raw mode and bridges the binary stream ↔ PTY, forwarding window size
changes. Idle timeout, max duration, heartbeat and guaranteed kill apply unchanged. The CLI
**neither defines nor specifies** a new protocol here.

## 7. Port-forward (ADR-032)

`akerdock db port-forward 15432:5432 varuna` establishes a tunnel from `127.0.0.1:15432` **local
to the CLI process** (CLI loopback listener, outside the §2 invariant which only concerns
outbound network connections) to port `5432` of the target container, via the manager.

- **Positional order**: the ports come first and the resource name last, so the two
  optional positionals are told apart by their place. With the type carried by the group,
  `15432:5432` can no longer be mistaken for a target (ADR-070 §1).
- **Mint**: `POST /{applications|databases}/{uuid}/port-forwards` (+ previews),
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

### 7.1 External endpoints (bastion, ADR-045) — `akerdock tunnel open`

`akerdock tunnel open prod-replica` tunnels to a target **outside** the
server — a managed database, an internal API — that an admin declared as an
`external_endpoint`. It is its own command, not a `port-forward` target (ADR-069): the two
share the transport ladder (ADR-064) and the session table, and nothing else.
`port-forward endpoint/…` is **refused**, with an error naming this command. Three
differences with a container forward:

- **No remote port argument, and no body on the mint**: the endpoint froze its host and
  port at declaration, so the client cannot name an address. Only the local port is the
  caller's to choose — and it may be omitted too, in which case the OS picks a free port
  and the CLI announces it. The endpoint is named **bare** (`prod-replica`, not
  `endpoint/prod-replica`): a `type/` prefix disambiguates a target among the kinds a
  command accepts several of, and this one accepts exactly one — the same reasoning that
  gives `akerdock ingress dev-kedric 3000` a bare name.
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
  change without another ceremony (ADR-045 §5). A token minted before that creator was
  recorded names nobody, so no grant is ever spendable through it: the mint refuses it with
  `token_without_creator` — **not** `access_request_required`, which would send the
  developer through the ceremony and then poll for ten minutes on a call that cannot
  succeed — and the message names the fix, `akerdock login` again.
- **The grant's expiry is the session deadline**: `authorized_until` is announced at open,
  and an automatic close reports its reason (`grant_expired` among them) instead of a bare
  disconnection.

### 7.2 Seeing and cutting sessions (`akerdock tunnel list|close`)

`GET /port-forward-sessions` and `DELETE /port-forward-sessions/{uuid}` shipped with
ADR-045 and, until ADR-069, had no CLI client: a tunnel could be opened from the terminal
but only seen or closed from a browser. Both are now reachable.

- **`akerdock tunnel list`** lists the team's sessions — **every target kind**, application
  and database and preview included, because "what is currently forwarded out of this
  team" is the operational question and hiding the container forwards would answer a
  different one invisibly (ADR-045's own reasoning for the dashboard view). Columns:
  target (`kind/name[:component]`), remote port (`-` for an endpoint, which named none),
  user, state, opened-at, UUID. State folds three fields into one word: `pending` (minted,
  not yet attached), `attached`, or the end reason (`idle_timeout`, `grant_expired`,
  `target_stopped`, …). `--endpoint NAME` sends the API's `external_endpoint_uuid` filter,
  `--all` its `active=false`; `-o json` emits the API objects unaltered.
- **`akerdock tunnel close SESSION_UUID`** issues the `DELETE`. The rule is the endpoint's,
  not the CLI's: your own session needs only the permission that opened it, someone
  else's is an administrative act (ADR-068 decides which permission). Closing an
  already-closed session is a no-op.

No token is readable back from either surface (§23.2), here as anywhere else.

## 8. Typed console (`akerdock db console`)

Convenience on top of §7. `akerdock db console [NAME]` detects the resource's engine (postgres /
mysql / redis / mongo), opens an ephemeral port-forward and launches the corresponding local client
(`psql`, `mysql`, `redis-cli`, `mongosh`) preconfigured with the resource's credentials.

**Targets.** A standalone database (the positional name) **or** a database service of a compose
stack (`--app <name> -c <service>`, engine read from the component, §9.2 of the compose spec) —
the compose form names the application because the container belongs to it; `--pr N` targets the service of
the preview instance of PR N. For a compose service, credentials are read on a best-effort basis
from the **generated magic variables** (`SERVICE_USER_<ID>` / `SERVICE_PASSWORD_<ID>`,
§5.4): without `read:sensitive` they are redacted (`value: null`), in which case the CLI prints the
connection command and leaves the forward open rather than launching a client without
credentials. If the local client is missing, same fallback. The CLI **neither stores nor relays** a
cleartext password beyond launching the child process.

## 9. Inspecting one resource (`info`)

`akerdock <type> info [NAME]` answers "what is this one doing", which `list` never did:
desired and observed status, health, the components of an application or a stack, and the
last deployment. Read-only, `GET` on the resource (plus `/components` where the type has
them). `-o json` returns the API objects unaltered, so a script never parses the table.

A field the API does not serve is **left out**, never filled with a placeholder: an
application's public URL, for instance, lives on `GET /servers/{uuid}/domains` and is
resolved through it or omitted.

## 10. Lifecycle (`restart` / `start` / `stop`)

`POST /{applications|databases|services}/{uuid}/{restart,start,stop}` — the three verbs
exist for the three types and are spelled identically under each group. No confirmation
prompt: stopping a resource the platform can start again is an ordinary act, and scale-to-zero
already stops containers on its own (ADR-036).

**No `-c` and no `--pr`**: the nine endpoints take no body, no query and no component, and
there is no preview lifecycle endpoint at all. A flag the server ignores would let the caller
believe one container — or the PR instance — had been restarted when the whole resource was.

The answer is a job, not a result: the API replies `202` with a job uuid, so the CLI says
`accepted`, never `restarted`.

Interaction with scale-to-zero is unchanged: an explicit `stop` sets the desired status, which
is exactly the gate ADR-037 §3 checks before waking anything.

## 11. Environment variables (`env`)

`akerdock <app|svc> env list|get|set|unset [--pr N] [--apply]` over
`/{applications|services}/{uuid}/envs` (and the preview collection with `--pr`).

- **The masking policy is the server's.** Without `read:sensitive` a value comes back masked,
  and an `is_locked` variable never reveals its value at all (§20.4.4 of the PRD). The client
  prints what it is given and re-implements none of that.
- **`--apply`** redeploys **without rebuilding** (`skip_build`, ADR-048) once the variables are
  written. Without it, `set` writes and says nothing further: a variable that is set and never
  applied is the mistake this flag exists to prevent, not a state the CLI hides.
- `set` accepts several `KEY=VALUE` pairs in one call; a malformed pair is refused **before**
  any request is sent (exit code 2). `--secret` marks them as **build
  secrets**: mounted by BuildKit for one `RUN` rather than passed as a build ARG, which
  `docker history` would expose (§5.2). `KEY=` (an empty value) is refused and points at `unset`, which also means the empty
  string is not expressible from the CLI: the shell strips the quotes of `KEY=""` before the
  process sees them, so the two are indistinguishable.
- With `--pr N` the collection is the preview's **effective** set. Setting an inherited key
  creates an override for that preview; **unsetting one is refused**, because the API has no
  per-preview delete and removing it would change every open PR.

## 12. Deployments (`deploy`)

`akerdock <app|svc> deploy run|list|cancel` and, for an application only,
`deploy rollback`. A group rather than four top-level verbs, because the history is consulted
about as often as a deployment is triggered.

- **`run`** — `POST /{applications|services}/{uuid}/deploy`. On an application the body takes
  `skip_build` (apply the configuration without rebuilding, ADR-048) or `force_rebuild`,
  never both; on a **service the endpoint takes no body**, so neither flag is offered there.
  `-f` follows the build log of the deployment the mint just returned, through the same SSE
  stream `app logs --deployment` reads, rather than polling. There is **no `--branch`**: no
  deploy body carries a ref, and a flag the server drops would be a lie in the help.
- **`list`** is bounded (`-n`, default 20): the history is unbounded server-side and walking
  it whole is not a default anyone asked for.
- **`list`** — the paginated history: status, trigger, commit or image, timings.
- **`cancel`** — `POST /deployments/{uuid}/cancel`, transversal because a deployment uuid
  identifies itself without its parent.
- **`rollback`** — `POST /applications/{uuid}/rollback`. Absent from the `svc` group because
  the endpoint does not exist there, not as a policy.

## 13. Security (delta to the threat model)

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

## 14. Audit and observability

Every action with a remote effect is audited with actor/token, IP, timestamp (§23.4): login
(success/failure), terminal session and port-forward open/close, revocation.
Shell keystrokes and tunnel bytes are **never** logged (§24.4).

## 15. Tests

In accordance with the test pyramid (ADR-026/028, test plan §2): deterministic logic
is proven with **unit/module tests** — target resolution (positional name, directory default,
and the refusal of the removed `type/name` form), the shape of the command tree itself (each
group exposing exactly the verbs its type has, so a verb cannot silently appear on another
type — and so the two decided absences, `preview approve` and `backups restore`, stay
absences), context resolution, login state
machine (start/poll/approve/exchange, PKCE verification), tunnel multiplexing
(`open`/`eof`/`close` framing, stream cap, buffer), overlap detection between two
preview log snapshots (`logs --pr -f`). End-to-end shell and port-forward
are validated **manually** on an ad-hoc basis; the single E2E product journey
(Docker-in-Docker) is **not** extended for the CLI (ADR-028).
