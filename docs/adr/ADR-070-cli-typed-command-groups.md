# ADR-070 — The CLI becomes a tree of typed groups, and stops sending developers to the dashboard

- **Status**: Accepted
- **Date**: 2026-08-09
- **Supersedes**: [ADR-018](ADR-018-local-deployment-akerdock-up.md) — `akerdock up`, the push of a
  local folder, leaves the product. Never implemented, and `app deploy run` covers the need
  it was invented for from a source that is actually traceable
- **Revises**: [ADR-033](ADR-033-cli-cobra-migration-run-modes.md) in the **shape** of the
  command tree: the `type/name` REF disappears and every targeted verb moves under the group
  of the type it targets. The Cobra choice, the run modes and the client/server split are
  untouched
- **Revises**: [ADR-069](ADR-069-tunnel-is-its-own-command.md) on two points only — `tunnel ls`
  becomes `tunnel list` (§4 below), and the sentence that refuses an endpoint on `port-forward`
  now names `app port-forward` as the container form. The verb split it decided stands
- **Revises**: [ADR-031](ADR-031-cli-login-poll-code-pkce.md) §CLI surface and
  [ADR-032](ADR-032-tcp-tunnel-cli-websocket.md) in command spelling only — `logs`, `shell` and
  `port-forward` keep their protocols, their mints and their permissions, and gain a group
- **Related**: [ADR-048](ADR-048-apply-config-without-rebuild.md) (`skip_build`, which
  `env set --apply` calls), [ADR-045](ADR-045-external-endpoint-port-forwards.md)/[ADR-060](ADR-060-dev-ingress-tunnels.md)
  (`tunnel` and `ingress`, which target neither an app nor a database and stay top-level),
  [ADR-059](ADR-059-reviewer-inventory-read-access.md) (the reviewer's read path, which
  `app preview list` finally serves from a terminal)
- **Related PRD sections**: §5.7, §12, §26, §27.18

## Context

The CLI was specified as a **debug** client (PRD §12): list, read logs, open a shell, forward
a port. Everything else — setting a variable, triggering a deployment, restarting a container,
seeing why a preview is down — requires a browser. A developer who wants to stay in their
terminal cannot: the product's daily loop is split across two surfaces, and the terminal holds
the smaller half.

Measured against the CLIs of the domain, the gap is not subtle. Heroku, Scalingo, doctl and
flyctl all ship, at minimum: environment variables (`config:set` / `env-set` / `secrets set`),
a deployment trigger, a restart, a per-resource status, and a one-off command. We ship none of
those five. Two of them — variables and deployment — are the ones every tutorial of every
competitor opens with.

The API is not the obstacle. `/{applications,services}/{uuid}/envs`, `/deploy`, `/deployments`,
`/rollback`, `/restart`, `/start`, `/stop`, `/previews/{uuid}/{redeploy,keep}`,
`/databases/{uuid}/backups`, `/scheduled-tasks/{uuid}/run` all exist and are all reachable only
from the dashboard today. The missing half of the product is a client, not a capability.

**And the shape has to be decided before the surface.** Today's spelling is `verb type/name`
(`akerdock logs app/varuna`), a kubectl borrowing that worked while the verbs were four and
transversal. Adding fifteen verbs that are *not* transversal breaks it: `env` and `deploy` mean
nothing for a database, `console` and `backups` mean nothing for an application, and a flat
tree would either accept them everywhere and fail at runtime, or grow a refusal per pair. The
question "which verbs does this resource have?" would have no answer anywhere in `--help`.

## Decision

### 1. The tree is typed: `akerdock <type> <verb> [NAME]`

Every verb that targets one kind of resource lives under that kind's group. The `type/name`
REF is **removed** — the group already names the type, and repeating it was the redundancy
that made `port-forward endpoint/x` ambiguous in the first place (ADR-069 §Context).

```
akerdock app  [NAME] …   list · info · logs · shell · port-forward · open
                         restart · start · stop
                         deploy run|list|cancel|rollback
                         env list|get|set|unset
                         preview list|redeploy|keep
                         tasks list|run
akerdock db   [NAME] …   list · info · shell · console · port-forward
                         restart · start · stop
                         backups list|run
akerdock svc  [NAME] …   list · info
                         restart · start · stop
                         deploy run|list|cancel
                         env list|get|set|unset
```

**A group offers the verbs its type actually has, and not one more.** The asymmetry above is
the API's, not a design choice of this ADR, and inventing client-side verbs on top of missing
endpoints would only move the failure from `--help` to runtime — which is the very thing §1
sets out to fix. What is missing today, recorded so the gaps are visible rather than
mysterious: a database and a compose stack have **no logs endpoint** (the dashboard has no such
view either), a compose stack has **no terminal and no port-forward** (only its parent
application does), only an application has **`rollback`**, and a backup execution exposes its
filename, size and checksum but **no download**. Each of those is a candidate for its own
decision; none is a reason to delay the tree.

The name is the **last positional and optional**: `akerdock app logs varuna -f`, and
`akerdock app logs -f` inside a repository whose `.akerdock` names a default application. Verb
before name, because the shell completes left to right and the verb is what the reader is
choosing; a name-first form (`app varuna logs`) reads well only when the name is already known.

**Transversal commands keep no group**, because they target no type: `login`, `logout`,
`context`, `whoami`, `list`, `tunnel`, `ingress`, `mcp`, `version`, `completion`. `list` with no
argument still walks applications, databases and services at once — it is the one listing whose
subject is the team, not a kind.

The container terminal keeps one spelling per type that has one (`app shell`, `db shell`) and
the **server** shell stays out: it is an SSH PTY on a host, with its own permission and step-up
(ADR-067 §Scope), and it will get a `server` group when it gets a decision.

### 2. What the developer gains, and what it costs the API: nothing

Everything below calls an endpoint that already exists. **No OpenAPI change, no migration, no
`make generate`.**

- **`env list|get|set|unset [--pr N] [--apply]`** — the value-masking policy is already
  server-side (no `read:sensitive` → masked, `is_locked` → never revealed), so the client
  displays what it is given and decides nothing. `--apply` triggers ADR-048's `skip_build`
  redeployment, because a variable that is set and never applied is a bug the developer writes
  by hand today.
- **`deploy run|list|cancel|rollback`** — a group rather than four top-level verbs: the
  deployment *history* is as often consulted as the deployment is triggered, and `deploy list`
  next to `deploy run` says that in the help. `-f` follows the build. The flags are exactly
  the two the mint body defines, `--skip-build` and `--force-rebuild`, mutually exclusive as
  the spec says; **there is no `--branch`** — no deploy body carries a ref, and a flag the
  server would silently drop is worse than its absence. `--skip-build` exists on `app` only,
  because `POST /services/{uuid}/deploy` takes no body at all: the same asymmetry as
  `rollback`, expressed in flags.
- **`restart|start|stop`** — the same three endpoints exist for the three types, and they are
  spelled the same way under each group.
- **`info`** — desired and observed status, health, components, URL, last deployment. `list`
  answers "what do I have"; nothing answered "what is this one doing".
- **`app preview list|redeploy|keep`** — the reviewer path of ADR-059, from a terminal.
  **`approve` is deliberately absent**: authorizing a fork to run is project governance, and
  this CLI is a runtime and debugging tool. `keep` stays, because holding a preview alive while
  you debug on it *is* debugging.
- **`db console`** — today's `akerdock db <REF>`, moved under the group that now owns the verb
  space of a database. Precedent: `scalingo pgsql-console`, `heroku pg:psql`.
- **`db backups list|run`** — **no `restore`**. Overwriting a production database from
  a one-line command is the one act in this list whose blast radius does not fit in a terminal
  confirmation; it keeps the dashboard's context. Stated as a decision so nobody adds it as an
  oversight. `download` is absent for a different reason — no endpoint serves the file —
  and belongs to the gap list in §1.
- **`app tasks list|run`** — running a scheduled task on demand is how you find out why it
  fails.
- **`whoami`** — context, instance, team and token scopes, with no network call and no new
  endpoint. The question it answers ("where am I pointed, as whom") is the one worth asking
  before `stop`.
- **`app open [--dashboard]`** — the public URL, or the resource's dashboard page. The bridge
  to the UI for the moments the UI is genuinely better, which is the honest complement of this
  whole ADR.

### 3. `akerdock up` leaves the product

ADR-018 decided the CLI **may** push a local folder for prototyping. It was never built, and
this ADR withdraws the promise rather than carrying it as a permanent "v2" line: the paragraph
justifying it — "make it possible to prototype before having created and pushed a repository" —
describes a first-contact journey the dashboard's Git flow now covers, and every safeguard it
had to invent (a context digest instead of a SHA, a deployment that must never enable
auto-deploy) exists only to compensate for a source that cannot be re-fetched.

**A deployment starts from a source the platform can fetch again: a Git ref or an image.** A
local folder is not one. PRD §12 and §27.18 lose the `up` line; ADR-018's index entry becomes
superseded.

### 4. Two conventions settled once, for every command that follows

- **`list`, never `ls`.** `akerdock list`, `app list`, `db backups list`, `context list`,
  `tunnel list` (revising ADR-069's spelling). `ls` stays as an alias on each of them —
  registered, tested, and never the name shown in help — because muscle memory is real and an
  alias costs one line.
- **`-a` / `-e` / `-p`** are the short forms of `--application`, `--environment`, `--project`.
  `-a` is the domain's universal spelling (heroku, scalingo, fly); the other two follow it
  rather than leaving a lone exception. They carry a *default*, which the positional name
  overrides when given — the precedence of the spec (§4) is unchanged.

### 5. No compatibility aliases, and a refusal that teaches

`akerdock logs app/varuna` does not work and is not silently translated. It fails with
`use: akerdock app logs varuna`, exactly as ADR-069 §3 decided for the bastion, for the same
reason: an alias that keeps working teaches nothing and leaves two spellings in the help, the
docs and every support answer. A REF-shaped argument is detected by its slash, so the refusal
is precise and never fires on a legitimate name.

The old spelling appears in the README, the in-app documentation (PRD §25.4) and the
dashboard's copyable snippets. Those move with this ADR, not after it.

## Consequences

- **The tree is rewritten, not extended.** `logs`, `shell`, `port-forward`, `db` and `ls` all
  change spelling; `internal/cli/ref.go`'s REF parsing collapses into a per-group resolver, and
  `--pr N` stays a flag of the app group's verbs.
- **`--application` now overlaps the positional name.** It survives because it is also how
  `.akerdock` and `AKERDOCK_APPLICATION` speak, but it is no longer the primary way to name a
  target, and the spec must say which wins (the positional).
- **The help becomes the map.** `akerdock db --help` lists exactly what a database can do —
  the question that had no answer before this ADR.
- **`run` (one-off, non-interactive) is deliberately not here.** All four competitors have it
  and we should, but it needs an endpoint that does not exist; it is its own decision, taken
  later, and it will land as `app run` in the tree this ADR sets up.
- **The documentation is rewritten, not patched.** Every spelling in this repository changes,
  and the affected documents are named here so none is missed: `docs/specs/cli.md` (§3's REF
  paragraph, the §3.2 command table, and §6/§7/§8, all written around `verb type/name`),
  PRD §12 and §27.18 (the `up` promise) plus the §26 grid, `README.md`'s CLI section, the
  in-app documentation page (PRD §25.4, `web/src/app/pages/docs/docs.content.ts` — the CLI
  chapters and every code block), and the dashboard's copyable command snippets
  (`web/src/ui/stack-components/`). The spec is the one to write **first**: it is the contract
  the rest quotes.
- **Two contract defects block on nothing and are fixed in the same slice, without an ADR**
  because they are gaps against a spec already written: `docs/specs/cli.md` §3.2 promises exit
  code `2` for usage errors and the binary always exits `1`; `-o bogus` is accepted silently
  and falls back to a table. Both are one test each.

## Alternatives rejected

- **Keep the flat `verb type/name` tree and just add the verbs**: rejected — `env` on a
  database and `console` on an application are meaningless, so a flat tree pays for every
  (verb, type) pair with a runtime refusal, and `--help` can never state what a given resource
  supports.
- **Groups for the lifecycle only, REF for the rest**: rejected — two models in one CLI means
  every future command re-opens the question, and the rule cannot be stated in a sentence.
- **`app` as the only group, databases and services left flat**: rejected — the three types
  have the same three lifecycle endpoints; asymmetry in the client would be ours alone.
- **Deprecation aliases for the old spellings**: rejected, per §5.
- **A `restore` command with a typed confirmation**: rejected for now — see §2. The confirmation
  design is not the hard part; deciding that a terminal is the right place for that act is.
- **Keeping `akerdock up` as a v2 line**: rejected — a promise nobody is building is a promise
  the roadmap pays for at every review.

## Verification

Unit tests, per the pyramid (ADR-026/028):

- The tree: every group exposes exactly the verbs listed in §1, asserted against the Cobra tree
  so a verb added to one type does not silently appear on another.
- Target resolution: positional name, `.akerdock` default, `-a` override, and the precedence
  between them; a name containing a slash produces the §5 refusal naming the new form.
- `env`: `set` sends the variable, `--apply` triggers the `skip_build` deployment and plain
  `set` does not; a masked value renders as masked and is never echoed back on `set`.
- `deploy`: `run` mints and follows, `list` paginates, `cancel` and `rollback` hit their
  endpoints, `-f` streams the build log.
- Lifecycle: the three verbs under each of the three groups reach the right path.
- `preview keep`/`redeploy` reach the preview endpoints; **no `approve` verb exists** in the
  tree (asserted, because its absence is a decision).
- `db backups`: `list`/`run`; **no `restore` and no `download` verb exists** (the first by decision, the second for want of an endpoint).
- `whoami` performs no HTTP request.
- Contract: an unknown command, an unknown flag and a missing argument all exit `2`; an
  invalid `-o` value is refused with the accepted values named.
