# ADR-076 — The non-root server escalates through `sudo -n`, never through a prompt

- **Status**: Accepted
- **Date**: 2026-08-17
- **Related**: PRD §3.1 (non-root is experimental and "requires `sudo NOPASSWD: ALL`"),
  §20.1 step 3 (the validation worker "tests … sudo") and its acceptance line
  ("interactive sudo … produce[s] a distinct error"), [ADR-051](ADR-051-docker-runtime-adapter.md)
  (the agent channel that most operations ride, and that this ADR deliberately does not touch),
  [ADR-054](ADR-054-agent-host-ops.md) (the bootstrap family — the SSH commands this ADR escalates)
- **Related PRD sections**: §3.1, §20.1, §26

## Context

The `use_sudo` column has existed since migration `00006_servers.sql` — and nothing else has.
It was never in the INSERT, never in the UPDATE, absent from the OpenAPI contract, absent from
the dashboard, and `sshexec` has never prefixed a single command with `sudo`. Three normative
statements pointed at a mechanism that did not exist: §3.1 promises an experimental non-root
user against `NOPASSWD: ALL`, §20.1 step 3 says the validation worker tests sudo, and the
acceptance criteria demand a *distinct* error for interactive sudo. The column was the tell
that someone meant to build it; the seventy migrations since are the tell that nobody did.

What a non-root onboarding actually produced, on a real server, was this:

```
agent deploy failed (exit 125): mkdir: impossible de créer le répertoire
«/var/lib/akerdock»: Permission non accordée
```

Three defects in one line. The command was executed bare, so the sudo rights the operator
deliberately gave their SSH user were never used. The failure surfaced in whatever locale the
server speaks, so no error classification — and no web search — behaves the same way twice.
And the remediation (become root, or pre-create the tree by hand) appears nowhere near the
place the operator is looking.

The non-root contract has a second, quieter trap: AkerDock authenticates **exclusively by SSH
key** (§3.1). There is no password anywhere in the system. A sudo configured without
`NOPASSWD` therefore does not fail — it *prompts*, into a session with no terminal and nobody
typing, and hangs until the timeout kills it. Any design that lets sudo prompt is a design
that converts a configuration mistake into a silent stall.

## Decision

### 1. `use_sudo` is opt-in, per server, exposed end-to-end

The column joins the INSERT and UPDATE queries, the `ServerCreate` / `ServerUpdate` / `Server`
schemas (spec-first, ADR-025's toolchain), and both dashboard forms — a checkbox that only
appears when the SSH user is not `root`, whose hint states the sudoers one-liner verbatim.
Default `false`: existing non-root setups that work bare (a pre-created tree, a user in the
`docker` group) keep working bare, and are not suddenly handed a sudo they may not have.

Toggling the flag moves the server back to `pending`, exactly as changing host, port, user or
key does: it is not connectivity in the TCP sense, but every remote command changes its
execution identity, so nothing proven by the last validation still holds.

### 2. The escalation lives in `sshexec`, the one choke point every command crosses

A sudo-enabled client wraps every `Run` / `RunInput` / `RunStream` command as:

```
LC_ALL=C sudo -n -- sh -c '<command, single quotes escaped>'
```

One wrapper, three properties. `sh -c` escalates the **whole** snippet — a `mkdir && docker run`
chain runs entirely as root, instead of a sudo'd first half and a bare second half. `-n` makes
sudo constitutionally unable to prompt: with key-only auth there is no password to type, so
the only honest behaviours are "works" and "fails now, loudly". `LC_ALL=C` pins sudo's own
diagnostics to one locale, so they can be classified — the founding bug report of this ADR
was a French `mkdir` error no substring match would ever have caught.

Deliberately **not** escalated: `StartPTY` (the server terminal is the operator's own session,
§24.4 — they type `sudo` themselves if they mean it) and `DialTCP` (a tunnel carries no
command). The agent channel (ADR-051/054) is untouched: the helper already runs `--user 0`
against the Docker socket, so commands riding it never needed the SSH user's privileges.

### 3. sudo's refusals are typed errors carrying their remediation

A password-required refusal (and its cousins: not in sudoers, command not allowed) returns
`ErrSudoPassword`, whose message says why there is no password to give and names the exact
fix, user included: `echo '<user> ALL=(ALL) NOPASSWD: ALL' | sudo tee /etc/sudoers.d/90-akerdock`.
A missing sudo binary returns `ErrSudoMissing`. Neither is ever handed back as a bare exit
code — "the command failed" is precisely the wrong thing to say about a command that never ran.

### 4. Validation proves the escalation before anything depends on it

A `use_sudo` server gets a `check_sudo` step right after `ssh_connect`: `sudo -n true`, failed
to `pending` on any refusal — including a non-zero exit that classifies as nothing, which is
what a restricted (non-`ALL`) sudoers entry produces. This is the *distinct* interactive-sudo
error §20.1's acceptance line has demanded all along, in the step recorder where the operator
is already looking. The proxy-layout failure message now states all three exits: onboard as
root, enable `use_sudo`, or pre-create `/var/lib/akerdock` for the user.

### What this ADR does not decide

No automatic escalation for non-root users without the flag (bare non-root setups are legal
and existing). No per-command sudo policy — the contract is `NOPASSWD: ALL`, per §3.1, and a
narrower sudoers is reported as a broken one. No `doas`/`su` support. No change to who may
hold the flag: it is a server property under `servers:manage`, not a permission.

## Verification

- `sshexec`: wrap shape and quote escaping asserted against the scripted SSH server; stdin
  passthrough under sudo; password-required and sudo-missing classified with `errors.Is`;
  ordinary non-zero exits pass through unclassified; no wrap before `EnableSudo`.
- `servervalidate`: a use_sudo server is asserted to wrap **every** command of a full
  validation; a bare server to wrap none; a refused escalation fails on `check_sudo` naming
  `NOPASSWD`.
- Handlers: `use_sudo` persisted on create; toggling it on PATCH writes `pending` in the same
  statement.
