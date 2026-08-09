# ADR-066 — The attach answers before it dials

- **Status**: Accepted
- **Date**: 2026-08-09
- **Revises**: [ADR-032](ADR-032-tcp-tunnel-cli-websocket.md) — its "Server side" clause
  only, and only its timing: the container's IP stops being "resolved by `docker inspect`
  at session opening", and the SSH connection stops being established there. The mint, the
  single-use token, the frozen `(container, port)`, the per-team cap and the "authorization
  boundary is the resource, not the port" statement are untouched, as is one SSH
  `direct-tcpip` channel per stream. That clause also says the streams ride "the existing
  pooled SSH connection": there is no pool and there never was (§7)
- **Extends**: [ADR-064](ADR-064-one-transport-ladder-for-every-cli-tunnel.md) — the ladder,
  its rungs, its bounds and its per-path wire identifiers stand unchanged; this removes the
  reason the egress and terminal rungs' open budgets had to be a guess about someone else's
  SSH latency. ADR-064 §3 put the terminal on the ladder; this decides what its attach may
  do before it answers
- **Related**: [ADR-045](ADR-045-external-endpoint-port-forwards.md) §5 (a tunnel that dies
  must say why), [ADR-052](ADR-052-agent-command-channel.md) (the channel the container
  inspect and the exec attach ride), **ADR-065** (the attach claim becomes idempotent within
  its TTL — the same failure taken from the other end), **ADR-067** (a port-forward may wake
  a sleeping resource)
- **Related PRD sections**: §5.7, §12, §24.4

## Context

Two of the three CLI attach families do their slowest work before writing a single response
byte.

**Egress.** `tunnelAttachSession` (`internal/handlers/portforwardattach.go`) claims the
one-time token, then calls `tunnelTarget` (`internal/handlers/portforward.go`), which
performs an agent `ContainerInspect` RPC and then a full SSH dial through `serverdial.Open`
— unpooled, a fresh handshake per attach, bounded only by `servers.ssh_timeout_seconds`,
whose default is **30 s**. Only then does it flush the response head.

**Terminal.** `terminalAttachSession` (`internal/handlers/terminalattach.go`) claims its
token, then calls `terminalPTY` (`internal/handlers/terminal.go`), which for a *server*
shell performs the same `serverdial.Open` and then `StartPTY`, and for a *container* shell
performs an agent `ContainerExecCreate` followed by `ContainerExecAttach`. Only then does it
flush the response head.

Neither client can wait indefinitely. An unanswered open is indistinguishable from a stalled
transport — the third ingress defect ADR-064 lists — so the ladder bounds it:
`transportAttachTimeout`, **5 s**, one shared constant serving both families
(`internal/cli/httptransport.go`, used at `internal/cli/egress_transport.go` and
`internal/cli/terminal_transport.go`). The two numbers do not merely disagree; they disagree
by a factor of six, in favour of the side that is not waiting. Whatever the client's bound
is, it is a bet on how long an SSH handshake takes on someone else's network, and when the
bet loses the client steps down the ladder while the server is still dialling — with the
token already spent. The next rung presents it, the atomic claim refuses it, and the
developer reads:

```
invalid, expired or already used tunnel token
```

which describes none of what happened. That is the report from a real
`akerdock port-forward … --pr 4865`.

The current ordering is not an accident; it is a stated principle, and the statement is in
the code:

> Resolve the target **BEFORE** committing the response, exactly as the WebSocket rung
> does: an HTTP error is diagnosable, a stream that dies is a mystery.
> — `terminalAttachSession`

That sentence is not wrong. It is incomplete, because it names two outcomes where there are
three. The third — a token spent on a response head the client stopped waiting for — is
neither diagnosable nor a mystery. It is a lie: the platform reports a bad token for a
handshake that was still in progress. This ADR keeps the principle's *goal* (never let a
failure surface as a dead stream) and drops its *mechanism* (make the head wait).

The data-stream half of the egress attach already took the other road.
`tunnelAttachStream` dials with a bounded context (`tun.EgressDialTimeout`, via
`dialTCPContext`) and answers `502 target_unreachable` carrying the dial's own first line,
with a comment saying exactly why: an unbounded dial before the response head "spends the
client's patience and surfaces there as a transport timeout, blaming the tunnel for a target
that never answered." That sentence is true one level up too.

Two shapes are being confused throughout. A **policy** answer — this token is spent, this
preview is destroyed, this team is at its cap — is a fact the control plane holds locally
and can state in microseconds. A **reachability** answer is a fact about two other machines,
and it is not only slow, it is perishable: a container that is running when the head is
flushed can be gone a second later, and the `409` never protected against that. Both
families pay the price of the second to decorate the first.

Ingress is out of scope, and not by omission: nothing is dialled before its attach answers.
The agent already holds the endpoint it serves, and the control stream exists so the *server*
can ask the client to open — the direction this ADR is about does not occur there.

## Decision

**The attach answers first and resolves lazily, on both the egress and terminal paths.** The
response head stops being a bet on SSH latency; an unreachable target is reported on a
channel that exists for it.

### 1. The session request answers on what is cheap and certain

`tunnelTarget` and `terminalPTY` each split along the line drawn above.

**Local half — stays before the head.** Token claim, attach key decode, protocol echo,
full-duplex capability, geometry, and the store lookups that resolve *what* the session
names:

| | Egress | Terminal |
|---|---|---|
| Session bounds | `sessionBounds(row)` (ADR-045 grant expiry) | idle/max duration from config |
| Target identity | server row; external endpoint (ADR-045) **or** resource + preview status + component → container name | server row; for a container shell, `terminalContainer`: resource + preview status + component → container name |

These are indexed Postgres reads on a pooled connection, and their refusals are the
actionable ones — "the target server no longer exists", "the target resource no longer
exists", "the preview no longer exists — it may have been destroyed". They keep their `409`
and their prose.

**Remote half — moves behind the head.** Every leg that crosses the network to a machine the
control plane does not own: the agent `ContainerInspect` RPC and the SSH handshake for
egress; `serverdial.Open` + `StartPTY`, or `ContainerExecCreate` + `ContainerExecAttach`, for
the terminal.

What the session request holds after the local half is a **target spec**, not a connection.

One ordering requirement comes with the change, and only egress owes it: the live attach must
be registered (`egressRegister`) **before** the head is flushed. The terminal already does
this and says why in its own comment — "the CLI opens its data stream the moment it reads
that head, and a session not yet in the register would answer it *unknown session*". On
egress the window is theoretical today (a WAN round trip against a few nanoseconds of map
insert), and answering earlier is exactly what would widen it. It costs nothing to close, and
the terminal is the precedent.

### 2. The remote half is started eagerly and awaited lazily

Resolution starts in the background immediately after the head is flushed, and whoever needs
its result awaits it rather than performing it.

Eagerly, not on first demand, for one reason: the developer is not waiting at the same place
in the two designs. On egress, between the announcement (`forwarding 127.0.0.1:5432 -> …`)
and the first local connection sits a human typing `psql` — seconds of dead time the
resolution should be spending, not the connection. A fully lazy design would move the whole
`ContainerInspect` + handshake inside the budget of the first stream, which is the one moment
the developer *is* watching, and it would need a singleflight guard anyway since two local
clients can connect at once. It is not simpler, and it is slower where it counts.

The resolution is a **promise owned by the session request**, resolved exactly once:

- It runs under a context derived from the session request's, so a session that ends
  mid-dial cancels the dial. Its bounds are the ones each leg already carries — the agent
  RPC's own timeout and `servers.ssh_timeout_seconds`. **No new tunable** (§7).
- The session request keeps ownership. The `defer client.Close()` (egress) and
  `defer cleanup()` (terminal) that exist today become a `defer` on the promise, which
  releases whatever was established and is idempotent. Ownership does not move — only the
  moment of acquisition does. That is what keeps the SSH client alive across every egress
  data stream, exactly as `tunnelAttachStream` requires.
- A resolution that completes *after* the session tore down releases what it produced
  itself, the same late-arrival discipline `dialTCPContext` already applies to a hung
  `ssh.Client.Dial`. Without it, a session abandoned during a 30 s handshake leaks an SSH
  client — or, on the terminal, a live shell and an exec instance on someone's container.

**What is awaited differs per family, and the difference is the point.**

- **Egress** awaits an SSH client that must outlive every data stream. A stream's dial
  becomes: await the promise, then `dialTCPContext`, both inside the single
  `tun.EgressDialTimeout` context the stream already builds. The server-side bound the client
  derives its budget from does not move, so `egressDataOpenTimeout` and the contract stated
  in `EgressDialTimeout`'s doc comment stay exactly as they are.
- **Terminal, server shell** awaits an SSH client *and* the PTY started on it — `StartPTY` is
  part of the remote half, not of the bridge.
- **Terminal, container shell** awaits **no SSH client at all**. Its remote half is two typed
  commands on the agent channel (ADR-052), and its `cleanup` is `func() {}` because there is
  no transport to close — the `execPTY` owns the hijacked stream. Smoothing this into "the
  SSH client is dialled lazily" would describe a connection that this shell kind never opens.
  What the two kinds share is not SSH; it is that the last thing the control plane holds
  before bridging is the product of a network round trip to another machine.

The terminal needs no new waiting primitive. Its session request already blocks on
`awaitTerminalStream` for its one data stream (bounded, `terminalStreamOpenTimeout`, 15 s), so
the resolution and the data-stream open now overlap naturally; the promise is awaited
immediately after the stream arrives and immediately before `terminal.Bridge`. Its data-stream
handler is unmodified: it carries no dial, hands its conn to the session and blocks on
`attach.done`.

### 3. A stream that arrives early waits; it is not refused

A data stream that opens while the resolution is still in flight **waits for it**.

This needs no client to be convinced of anything, and the wait is already bounded by the
property §2 establishes: the await-plus-dial happens inside the one `EgressDialTimeout`
context the stream already builds, so a stream that waits cannot hang longer than a stream
that dials, and the client's derived budget keeps the margin it has today. Refusing early
streams and asking the caller to come back would make the failure explicit at the cost of a
bet on what `psql` and `redis-cli` do with a refused connection (see *Alternatives*). No new
status code, no new error code, and nothing in the CLI to teach.

**Two answers on a data stream, both already in the vocabulary:**

- **Resolution failed** → `502 target_unreachable` carrying the resolver's sentence.
- **TCP dial to the target refused** → `502 target_unreachable` carrying the dial's own
  words. Unchanged.

**And one on the session.** A failure that has nothing to do with any particular stream —
server gone, preview destroyed between mint and resolution, agent channel down, SSH refused,
container not running — is reported on the session stream before it closes:
`tunnel.HTTPSession.SendClose` on egress, the `{"type":"end","reason":…}` message on the
terminal. ADR-045 §5 already names this "the only way the developer learns why a tunnel they
were not using disappeared". On the terminal it is the *only* report channel, because that
family's data stream carries no dial and so has nothing to answer `502` about.

**The terminal reason enum gains one member: `target_unreachable`.** Not because the wire
needs it — `HTTPControlFrame` already carries `Reason` and an unused `Msg`, so the operator
sentence travels beside the reason with no wire change — but because the value is persisted,
and both families persist into the same `terminal_end_reason` type. The members that carry it
today are wrong in two different ways:

- egress writes `disconnect`, which means "the peer vanished"; the CLI renders it *"tunnel
  closed: the connection to the manager dropped"*, sending a developer to inspect their own
  network for a preview that was destroyed on the server;
- the terminal writes `terminal.EndRevoked`, which renders as an administrator having
  revoked the session — a statement about a human act that nobody performed.

ADR-045 set the precedent in both directions: a new member (`grant_expired`, migration 00079)
so that the audit row and the sentence the developer reads come from the same value. The
migration is additive — `ALTER TYPE terminal_end_reason ADD VALUE IF NOT EXISTS
'target_unreachable'`, last statement of its file, because PostgreSQL forbids using a new
enum value in the transaction that adds it — and rolling-upgrade safe: an older replica never
writes the value and reads the column as text, so it displays a reason it does not know
rather than failing.

Carrying the *sentence* costs one field per family: the CLI's `forwardCloseMessage` gains the
case and prefers the frame's `Msg`; on the terminal, `terminal.endMessage` and the
`HTTPControlFrame` mapping in `terminal.HTTPConn` — which today copies `Type`, `Cols`, `Rows`
and `Reason` — both gain `msg`, so the two families say the same thing rather than one saying
`session ended: target_unreachable`.

### 4. What is lost: the `409` at open, for five messages

Today an unreachable target is a `409` at redeem whose body the CLI prints immediately —
`handshakeReason` on the WebSocket rung, `attachRejection.message` on the HTTP rungs, both
rendered as `cannot open tunnel: <sentence>`. After this change the session opens and those
failures surface afterwards. Precisely five sentences move:

> *Egress* — "the server's agent is not connected right now" · "the target container is not
> running" · "the server is not reachable over SSH right now"
>
> *Terminal* — the `execTerminal` family ("the server's agent is not connected — it
> reconnects on its own…", "the container does not exist on the server — deploy it first",
> "the container is not running — start it first", "could not start the remote terminal") ·
> "the server is not reachable over SSH right now"

Every other refusal in `tunnelTarget`, `terminalPTY` and `terminalContainer` is a store lookup
and stays where it is (§1).

This is acceptable, and the argument is not "the cost is small". It is that the `409` was **a
pre-flight check whose validity expired the instant it was performed**: it proves the
container was running at redeem, which is not the question the developer's first connection
asks. A container that stops one second later already produced a `502` on the first egress
stream, and always did.

What replaces it is not "wait until you waste a `psql` attempt". The eager resolution (§2)
reports on its own schedule, on a session request that is already open, and the CLI prints the
sentence on stderr next to the `forwarding …` line it has just printed. In the common
failures — agent disconnected, container not running — that is a channel presence check and
one RPC: milliseconds, before the developer has finished typing. The genuinely degraded case
is a **slow** failure, an SSH handshake that times out at `ssh_timeout_seconds`: the listener
existed for up to 30 s, and a local client that connected inside that window waited (§3) and
then saw its connection fail with the reason. That is worse than a `409` by a few seconds, and
better than a spent token and a message about a token that was never the problem. On the
terminal the degradation is smaller still: the session announces nothing until it bridges, so
a failed resolution reaches the developer as an `end` reason before any shell was drawn.

### 5. The WebSocket rungs keep dialling before the upgrade

`TunnelWebSocket` and `TerminalWebSocket` have the same shape — claim, resolve, `409` before
`websocket.Accept` — and both deliberately stay as they are.

The defect this ADR removes is manufactured by a **bounded** open. The WebSocket rung has
none: the CLI's `websocket.Dial` is bounded only by the command's context, so the server may
take its whole handshake and still answer, and `handshakeReason` renders the `409`'s body
verbatim. There is no bet to lose. Making those rungs answer-first would trade the best
diagnosis on the ladder for a uniform one, and would need the pre-session failure to travel
through a close frame on bridges (`tunnel.Bridge`, `terminal.Bridge`) that take their dial or
their PTY at construction — real work, to make an experience worse.

ADR-064 already permits this: it homogenises the transport, not the choreography. The cost is
honest — the same failure reads differently on the HTTP rungs and on WebSocket, and a support
answer must know which — and it is bounded by the rung's own trajectory: WebSocket is the
compatibility floor, and the day it goes, the divergence goes with it. The CLI already prints
`tunnel transport: …` at attach, so the transcript names the rung.

The two in-code comments that state the opposite principle — "Resolve the target BEFORE
committing the response" in `terminalAttachSession` and "Resolve the target BEFORE upgrading"
in `TerminalWebSocket` — are now true of exactly one of them. The WebSocket one stands as
written; the session one is replaced by the reasoning of §1–§3.

### 6. The open budget stops being a bet

The second benefit is independent of the diagnosis and arguably larger.
`transportAttachTimeout` is one shared 5 s across every access path, and only egress and the
terminal needed it to cover an agent RPC or a fresh SSH handshake; the *data* budgets have
just been differentiated tactically (`egressDataOpenTimeout = EgressDialTimeout + 5 s`), and
the session budget was next in line for the same treatment. After this change both session
opens cover a token claim and a handful of indexed reads, so a shared 5 s stops being a bet
about someone else's network and the per-path question largely dissolves. What remains —
`egressDataOpenTimeout` — is derived from a real server-side bound that the server enforces,
which is a contract between two processes rather than a guess.

This **composes with ADR-065 rather than competing**. ADR-065 makes a lost race recoverable:
the claim becomes idempotent within the token's TTL and bound to the attacher's key, so a
client that gave up can present the same token on the next rung. This ADR makes the race not
happen. Neither subsumes the other — a bounded open can still be lost to a genuinely slow
network, and ADR-065 is what saves it; the control plane's own SSH handshake should never
have been the reason, and that is this ADR.

### 7. There is no SSH pool, and this ADR is where that is said

ADR-032's "Server side" clause says the streams ride "the existing pooled SSH connection to
the server", and `dialSessionServer`'s doc comment says "opens the pooled SSH connection".
Neither is true: `serverdial.Open` performs a full handshake — key fetch, envelope decrypt,
`sshexec.Dial` — on every attach, on both families. An accepted ADR is immutable, so ADR-032's
text stays as written; **this ADR supersedes that claim**, and the code comment is corrected
alongside it.

The correction is stated here because the ADR depends on it. If a pool existed, the handshake
would usually be free and the ordering would rarely matter; because none exists, every attach
pays it, which is why the defect is reproducible rather than rare.

Building a pool remains a separate decision and is not taken here. It would reduce how often
this is felt, never its shape: a cold pool still pays the handshake, and the first attach after
a control-plane restart is exactly when a developer is most likely to be debugging something
else.

### What this ADR does not decide

- **A dedicated resolution budget.** `ssh_timeout_seconds` is the bound — per server,
  adjustable where the problem actually occurs. A second budget superimposed on it would give
  two places to look when a tunnel gets cut, and the shorter one would silently win.
- **SSH connection pooling** — §7: the false claim is corrected, the pool is not built.
- **Dialling through the agent channel instead of SSH.** ADR-051/052 point there and ADR-052
  defers exec-attach; the egress target set includes ADR-045 external endpoints that are not
  containers at all.
- **The ladder's order, its rungs and its failure budget** — ADR-064's, unchanged.
- **The token's TTL and the semantics of the claim** — ADR-065's.
- **Ingress** — nothing is dialled before its attach answers; there is no defect to fix.

## Alternatives considered

- **Refuse an early data stream with `503 target_not_ready` + `Retry-After: 1`** instead of
  waiting (§3). Rejected. In its favour: it makes the state explicit rather than absorbing it
  into a wait, and it reuses the vocabulary `ErrSessionStreamLimit` already answers with.
  Against it, decisively: it converts a bounded server-side wait into an empirical bet on how
  `psql`, `redis-cli` and every other local client treat a connection that is accepted
  locally and then refused — a bet nobody wanted to take, for a wait that §2 already bounds by
  the same deadline the stream would have spent dialling. It also adds a status code and an
  error code to a path whose whole point is that it needs no new vocabulary.
- **Raise the client's session-open budget above `ssh_timeout_seconds`.** Rejected: a bigger
  bet is still a bet, and a budget long enough to absorb a worst-case handshake is long enough
  to be indistinguishable from the hung transport the budget exists to detect. It also makes
  the CLI's timeout depend on a per-server column it cannot read.
- **Fully lazy: resolve nothing until the first data stream.** Rejected — §2. It moves the
  handshake into the one moment the developer is watching, and needs a singleflight guard
  anyway, so it does not even buy simplicity.
- **Defer only the SSH handshake, keep the `ContainerInspect` / `ContainerExecCreate` before
  the head.** Rejected: those are RPCs over a WebSocket to a machine that may be offline or
  busy. They have the same failure shape and no better latency guarantee, so splitting the
  deferral by leg leaves half the bet on the table for half the benefit — and on a container
  shell it would leave *all* of it, since that kind has no SSH leg at all.
- **Answer `202`, then a `ready` control frame before the CLI announces the listener.**
  Rejected: it reinstates the wait on the control wire, where it is no longer bounded by
  anything, and it makes the announcement a promise of reachability — which is what the `409`
  pretended to be and could not be.
- **Fix egress only, leave the terminal.** Rejected: it is the same defect, the same 5 s
  constant and the same spent token, and the terminal's server shell dials the same
  `serverdial.Open`. Fixing one would leave the other to be rediscovered from a support
  ticket — the precise failure mode ADR-064 exists to stop.

## Consequences

- **Positive**: on both families the response head is produced from local state alone, so the
  session open is fast and predictable regardless of the target server's network; a spent
  token is no longer the report of a slow SSH handshake; every reachability failure is
  diagnosable in the developer's own words — `502` on the egress stream that asked, a terminal
  reason plus message for the session as a whole; two wrong persisted end reasons
  (`disconnect` on egress, `revoked` on the terminal) are replaced by one that is true; the
  per-path client-budget question narrows to one constant derived from a server-side bound;
  and answer-first is a **precondition for ADR-067**, since a resource that must be woken
  takes tens of seconds to become dialable and no response head can be held open that long.
- **Negative**: a port-forward's listener can now open onto a target that turns out to be
  unreachable, and five refusals that used to arrive before the announcement now arrive after
  it; both handlers grow a resolution promise with a real lifetime, which is a concurrency
  object where there was a straight line; the same failure reads differently on the WebSocket
  rungs (§5); and one additive enum migration, one `msg` field on the terminal's end message,
  and two CLI message cases ship with it.
- **Accepted risks**: the promise's late-arrival release is the only thing standing between a
  30 s handshake and a leaked SSH client — or a leaked shell and exec instance on a customer's
  container — per abandoned session, so it is guarded by a test rather than by the shape of the
  code; and a data stream that arrives during resolution now spends part of its budget waiting
  rather than dialling, which is invisible when resolution is fast and is exactly the
  degradation §4 accepts when it is slow.

## Verification

- Unit: each session request flushes its `200` **before any remote leg is attempted** — the
  test seam (`tcpDialer`, already in `portforwardattach_test.go`, and the terminal's PTY
  resolver) records that resolution had not started when the head arrived, and a resolver
  blocked for longer than the whole test still yields a head immediately.
- Unit: the egress attach is registered before the head is flushed — a data stream presented
  the instant the head is read finds its session rather than `401`. The terminal's existing
  ordering test stands as the precedent.
- Unit: a data stream that opens while resolution is in flight **waits and is then served**,
  and the whole wait-plus-dial stays inside one `EgressDialTimeout` — a resolution that
  completes within the budget yields a `200`, and one that never completes yields the same
  answer as a dial that never completes, not a distinct code.
- Unit: a resolution that fails surfaces on both channels — `502 target_unreachable` on an
  egress data stream opened afterwards, carrying the resolver's sentence, and a terminal
  reason on the session stream (`session_close` on egress, `{"type":"end"}` on the terminal)
  with `target_unreachable` and the operator message in `msg`, sent before the session request
  ends; the row is finalized with the matching enum value, and never with `disconnect` or
  `revoked`.
- Unit: the SSH client outlives every egress data stream — a stream that opens, splices and
  closes leaves the client usable for the next one, and the client is closed exactly once,
  when the session ends.
- Unit: no leak when a session ends before it is used — an egress session that ends before any
  stream opens, and a terminal session whose data stream never arrives
  (`terminalStreamOpenTimeout`), both release what the resolution produced; a resolution that
  completes after teardown releases it itself (the `dialTCPContext` late-arrival discipline,
  applied one level up); a session cancelled mid-handshake cancels the dial.
- Unit: the container-shell path opens no SSH connection at all — the resolver reaches only
  the agent channel, and its promise release closes the exec attach rather than a client.
- Unit: the local half still refuses before the head, on both families — destroyed preview,
  missing resource, missing server each answer `409` with their existing sentence, and the
  token is finalized.
- Unit: both WebSocket rungs still resolve before `websocket.Accept` and still answer `409`
  with the operator sentence — the existing tests are the assertion that §5 was a decision and
  not an omission.
- Unit (CLI): `forwardCloseMessage` renders `target_unreachable` from the frame's `Msg` and
  falls back to a phrased default when absent; the terminal's end-frame rendering carries the
  message rather than printing the bare reason.
- The single E2E journey (ADR-026/028) is untouched.
