# ADR-067 — Tunnels and terminals are citizens of scale-to-zero

- **Status**: Accepted
- **Date**: 2026-08-09
- **Revises**: [ADR-036](ADR-036-scale-to-zero-waker.md) §2, in two narrow places. The clause
  that makes the waker's activity file the **sole source of activity**: an attached
  interactive session is activity too, and the control plane records it (§1). And the clause
  that makes a **proxied HTTP request the sole trigger of a wake**: a tunnel or a terminal may
  ask for one (§3). What stands: the waker module remains the only thing that *performs* a
  wake, its readiness rule and its 60 s budget are unchanged, and its file remains the record
  of activity for everything the waker serves. The inactivity window and its 30-minute default
  are untouched
- **Extends**: [ADR-037](ADR-037-scale-to-zero-applications.md) §3 — `desired_status = running`
  already gates putting an application to sleep; it now gates waking it too, which the
  clause implies but does not say
- **Related**: [ADR-024](ADR-024-realtime-sse-websocket-terminal.md) (the terminal, its
  session bounds and its end reasons), [ADR-032](ADR-032-tcp-tunnel-cli-websocket.md) (the
  tunnel and its addressless protocol), [ADR-045](ADR-045-external-endpoint-port-forwards.md)
  §5 (grants, and the tunnel's 30-minute idle timeout),
  [ADR-052](ADR-052-agent-command-channel.md) §2/§5/§6 (the typed channel, the exec attach the
  container terminal rides, and who decides),
  [ADR-054](ADR-054-agent-host-ops.md) (precedent for a second vocabulary on that channel),
  [ADR-056](ADR-056-waker-becomes-the-agent.md) (the waker is a module of the agent),
  [ADR-064](ADR-064-one-transport-ladder-for-every-cli-tunnel.md) (one transport under both
  families), ADR-065 and ADR-066 (the token and attach halves of the same investigation)
- **Related PRD sections**: §5.7, §12, §20.4.3, §24.4

## Context

Scale-to-zero was designed around one protocol. ADR-036 put the waker in the HTTP path,
made a proxied request the unit of activity and the trigger of a wake, and everything else
follows from that: the activity file moves when the waker serves a request, the scheduler
reads that file, and a sleeping resource comes back because somebody loaded a page.

**Two access paths into a container are not HTTP and never touch the proxy.** A CLI tunnel
resolves the container's IP on its Docker network and dials it from the server over SSH
(`tunnelTarget`, `internal/handlers/portforward.go`), which is exactly the point — it
reaches a port that was never published and never routed. A web or CLI terminal opens a TTY
exec on the container through the agent channel (`execTerminal`, `internal/handlers/terminal.go`,
the slice ADR-052 §5 reserved and later shipped). Neither crosses the waker. Both were
therefore invisible to scale-to-zero in both directions, and the two holes are the same hole
seen from its two ends.

**The first hole — an attached session did not keep its target awake.** A developer
connected to a preview's PostgreSQL for thirty minutes produced no activity at all: the
scheduler read a stale file, stopped every container of the preview by label, and the
`psql` session froze on "sending keepalive" — the forwarded connection black-holed into a
destroyed network namespace, so no RST and no FIN ever came back, and nothing surfaced
until the tunnel's own idle timer fired half an hour later. This is a report, not a
hypothesis. A shell open in the same preview would have been stopped just as silently.

**The second hole is the mirror.** Opening a port-forward against a *sleeping* resource
fails with `the target container is not running`, and a terminal against one fails on the
exec, because in both cases the container is the only path to a target and only the waker —
sitting in the HTTP path — ever starts one. ADR-036 §2 and ADR-037 §5 are explicit that the
waker is what wakes; neither of these paths has a waker in it, so neither has a way to ask.
The developer whose preview just went to sleep is told to go and load its URL in a browser
first. That works, which is precisely what is wrong with it: the platform's answer to "your
target is asleep" is a manual workaround through a third protocol.

Both halves revise the same paragraph of ADR-036 — one changes what *counts* as activity,
the other what may *start* a wake — and both apply verbatim to both access paths. One
decision per problem, not one per door.

**Implementation status**, because it is uneven and a reader deserves to know which
sentences describe code. For the **tunnel**, §1 and §2 are built: the heartbeat stamps
activity and cuts a vanished target, and `createPortForward` stamps at mint time
(`recordTunnelActivity`) for previews and applications, with databases and external
endpoints excluded structurally. For the **terminal**, both halves are ahead of their
implementation and §1/§2 name the hooks that do not yet exist. §3 to §9 are ahead of their
implementation on both paths.

## Decision

**An attached interactive session is a first-class participant in scale-to-zero**, whether
it is a TCP tunnel or a terminal. It keeps its target awake while it is attached, it says so
when its target goes away underneath it, and it may wake a sleeping target — the wake being
the session's own act, not a browser detour.

Throughout: *session* means an attached port-forward (ADR-032/045) or an attached container
terminal (ADR-024). A **server shell** — the SSH PTY as `ssh_user`, gated by `terminal:root`
plus a passkey step-up — is not a session in this sense: it has no container, no resource and
no scale-to-zero clock, and it is excluded from every clause below.

### 1. An attached session is activity for its target

An attached session **counts as activity for the resource it targets**, recorded by the
control plane on the session's existing 20-second beat.

- **Previews** — `previews.last_activity_at`, a column that already exists and that the
  sleep decision already reads beside the waker's file.
- **Applications** — `applications.last_activity_at`. Applications had nothing but
  `resources.updated_at`, which moves on a deploy or a configuration change and never on
  use, so it could not carry this.
- **Managed databases, Compose *service* resources, declared external endpoints, server
  shells** — nothing is recorded. None of them has a scale-to-zero clock (ADR-037 §2
  excludes databases by construction), and inventing a signal for them would mean inventing
  its semantics too.

A terminal opened on one *component* of an application deployed as a Compose stack records
against the **application**, which is where the flag and the clock live.

**Why the control plane records it, rather than the agent writing the waker's file.**
The control plane is the only party that knows a session is attached: it terminates both
families (ADR-064 §2), it holds the socket, and it already runs a beat for exactly this
liveness question. The agent cannot observe the session as a session — a tunnel's bytes go
control plane → SSH → container IP and never cross the proxy the waker sits in, and while a
terminal's bytes *do* cross the agent, they cross it as an opaque exec attach on the command
channel, not as something the waker module is in a position to attribute to a resource's
activity clock. Having the agent write a file about either would mean the control plane
telling it so on every beat: a second reporter for one fact, and a round trip to state it.
Nothing here has the server contacting the control plane, so push §18.1 is untouched.

The price is that the sleep decision now reads **two sources**, and that is the honest
revision of ADR-036 §2: the waker's file is no longer the whole truth about activity. It
remains the whole truth about everything the waker serves. Two sources is also the shape
the scheduler already has — for previews it takes the latest of the file,
`last_activity_at` and `last_deployed_at`, because a redeploy is activity the waker never
saw either. This adds writers to a column that already had one, and gives applications the
column previews already had.

**One decision, two bridges.** `internal/tunnel` and `internal/terminal` are separate
session layers with separate `Options`, and only the tunnel's carries an `OnHeartbeat` hook
today; the terminal has the 20-second ping ticker but nothing durable hanging off it, and
`terminal_sessions` has no `last_heartbeat_at` to match `port_forward_sessions`. The rule
above is one rule; giving the terminal the hook — and the beat a row to write to — is work
this ADR obliges, not a symmetry it may assume.

**The consequence a reviewer will challenge**: an attached but *silent* session keeps a
scale-to-zero resource awake. That is the intended reading of "somebody is connected" — a
developer with an idle `psql` session or an open shell is still working, and the alternative
(measuring forwarded bytes) would sleep the container they are one keystroke away from
using. The exposure is bounded, and the bound is the **session's**, not the resource's — and
it differs by path, so it is stated per path rather than rounded:

- a **tunnel** ends after 30 minutes idle (ADR-045 §5) or 4 hours absolute (ADR-032);
- a **terminal** ends after 15 minutes idle or 4 hours absolute
  (`terminal.DefaultIdleTimeout`/`DefaultMaxDuration`, overridable per instance), and its
  idle timer counts **keystrokes only, never output** — a forgotten shell watching a spinner
  dies on schedule.

Worst case, a forgotten session holds a resource up for 4 hours plus its window — the same
worst case a forgotten browser tab has always had against the waker, reached by a path that
at least has a session row, an owner and an audit trail.

**The ordering hazard**, stated because it is the kind of thing found in production rather
than in review: a sleep decided in the window between the mint and the first beat, during
which nothing has yet said the session exists. The mint therefore stamps activity itself, in
**both** branches — necessarily on the wake path (§3), where the resource has just been
started and would otherwise be a candidate for the very next pass, and on the already-awake
path too, where one write closes the same 20-second window for the price of one write. This
is built for the tunnel and owed for the terminal.

### 2. A vanished target ends the session; it does not black-hole

A session whose target container is no longer running is **ended within one beat**, with the
`terminal_end_reason` member `target_stopped` that the client prints.

For the tunnel this converts a hang into an error. The failure mode is *silence*: a
forwarded TCP connection whose container's network namespace has been destroyed receives no
RST and no FIN, so the client sits on a keepalive until the tunnel's own idle timer fires,
and only if nothing else keeps the tunnel busy. For the terminal the gain is narrower and
should not be oversold — the daemon closes a hijacked exec when its container dies, so the
stream does end on its own; what it does not do is say *why*, and a shell that ends as
`disconnect` reads as a network glitch rather than as "your container was stopped". Turning
silence into a reason, and a wrong reason into the right one, is a change in what the
platform promises rather than a bug fix, which is why it is a clause and not a commit.

Three rules, identical on both paths:

- **Only a *definite* "not running" ends a session.** The agent reporting `not_found`, or
  an inspect whose state says not running, is an answer. An agent channel that is merely
  unavailable — `dockerruntime.IsUnavailable`: a helper restart, a relay reconnect, a spec
  reconciliation — is **not** an answer, and neither is a state we cannot read. Reading
  silence as absence would tear down healthy sessions every time an agent blinks, trading a
  rare hang for a routine one.
- **Targets with no container of ours are exempt**: a declared external endpoint (ADR-045),
  whose address was frozen at declaration and whose far side is not ours to inspect, and a
  server shell, which has no container at all. Their liveness is the connection's business.
- **Every rung enforces it.** ADR-064 put the WebSocket bridge and the HTTP session on the
  same bounds for both families, so a session cannot behave differently for having landed on
  one rung rather than another.

The end reason costs no migration: `terminal_end_reason` is one enum shared by
`terminal_sessions` and `port_forward_sessions` since the tunnel's own table reused it, and
`target_stopped` is already a member. What the terminal lacks is the Go constant and,
more substantially, **a way to be cut from outside at all**: the tunnel has a presence
registry whose `Cut` a revocation and this clause both use, while the terminal's only
registry is the HTTP rung's attach-key rendezvous, which exists to pair a data stream with
its session and cannot end one. That registry is work this ADR obliges — and it closes a gap
worth naming on its own: today a live terminal cannot be revoked mid-session either, only
swept as a row.

This clause stands whatever becomes of the wake half. A redeploy, an operator's stop and a
crash produce the identical failure, and not one of them involves scale-to-zero.

### 3. The mint asks for the wake; the attach never waits for it

Both families mint a single-use attach token and then redeem it, and in both the wake
decision belongs to the **mint** (`POST .../port-forwards`, `POST .../terminal-sessions`),
not to the attach. The mint is the only step that carries the caller's identity: the attach
holds a one-shot token and nothing else, deliberately (ADR-024, ADR-032, and whatever
ADR-065 settles about claiming it), so it can neither authorize a state change nor attribute
one to a person. Waking is a state change and must be both.

Concretely: the mint resolves its target as today, and if that target has no running
container **and** is a sleeping scale-to-zero resource (§8), it sends the wake command
(§4), stamps `last_activity_at` (§1) so the scheduler cannot re-sleep the resource in the
window before the first beat, and answers `201` **without waiting**, with the session's
state set to `waking`. The token's 60 s TTL is untouched, because nothing waits before the
attach.

The wait is paid **inside the established session**, on the first stream open for a tunnel
and on the exec attach for a terminal. That matters for ADR-065/066: whichever of them
lands, a 60 s cold start cannot be paid inside a request the client is timing — not the
mint, not the attach, which ADR-066 has answering before it dials at all. Holding the first
*stream* is the transposition of the waker's own hold-and-forward: the local `accept()` (or
the terminal's stream open) succeeds at once, the operation behind it is what waits, and the
client sits in the state it already tolerates from a slow server.

### 4. The agent's waker module performs the wake

The control plane does **not** start the containers itself, even though `dockerruntime`
gives it `ContainerStart` over the agent channel. It sends one typed command —
`WakeResource(resource_uuid)` — and the agent executes it by calling the waker module's
existing wake, the same code an HTTP hit runs: the same wake-set graph, the same
`depends_on` ordering, the same per-resource single-flight gate, the same readiness rule,
the same rollback of what it started when a wake fails. One command serves both access
paths; nothing about it knows which door asked.

Two reasons, and the second is the load-bearing one. First, a control-plane loop of
`ContainerStart` + `ContainerInspect` would re-implement the wake-set graph and the
readiness rule on the far side of a chatty sixty-second poll. Second, and worse, it would
be a **second starter for one resource with no shared gate**: a browser hit, a tunnel mint
and a terminal mint arriving together would race, each starting its own half of a compose
stack in its own order. Going through the module means they join the same in-flight wake.

This is where the second half of ADR-036 §2 is revised, and only here: an HTTP request is
no longer the only thing that starts a wake. What that clause also decided — that the waker
sits in the traffic path, that it reports what it serves through its file, that the control
plane never has the server call it (push §18.1) — is untouched. The control plane initiates,
the agent executes, exactly the model ADR-052 §6 states. `WakeResource` sits outside the
`dockerruntime.Runtime` enumeration that ADR-052 §2 closes, which is not a novelty: ADR-054
already added the host-ops file family and the pipe family to the same channel. This is a
fourth vocabulary of one method.

### 5. Ready means the operation the session is about to perform succeeds

A session must not answer "open" and then hand the developer a socket to a database that has
not finished starting, or a shell that the daemon refuses. For an arbitrary TCP port there
is nothing to lean on: `running` is a fact about a container, not about a listener; a
healthcheck may not be declared, and when it is, it says nothing about *this* port. So
readiness is decided in two gates, each bounding what it can actually observe.

- **Gate 1 — the containers, agent-side, ≤ 60 s.** `WakeResource` returns when every
  container of the wake set is ready **in the waker's own sense**: healthy where a
  healthcheck exists, otherwise running-stable for 10 s (ADR-036 §2). The budget is
  ADR-036 §4's number, unchanged and deliberately not re-litigated. The command is
  cancellable by the channel's `cancel` frame, so a session the developer abandons does not
  leave the agent waiting.
- **Gate 2 — the real operation, ≤ 15 s after gate 1.** Not a synthetic probe: the session
  retries, with backoff, **the exact thing it exists to do** — the TCP dial of `ip:port`
  over the same SSH path a tunnel will carry bytes on, or the TTY exec attach a terminal
  will run on the agent channel. Nothing appears in the target's own logs as a half-open
  connection, and no protocol knowledge is needed — ADR-032's tunnel is protocol-blind and
  stays so.

The two budgets are separate on purpose. They measure different things — a container
becoming ready, and the thing inside it accepting work — and folding them into one number
would silently redefine ADR-036's clause. Ceiling for the developer: **75 s**, after which
the session is refused with a verdict rather than left open.

The 15 s of gate 2 is **asserted, not derived**: no measurement stands behind it, unlike
ADR-036's 60 s. It is deliberately left as an invitation — move it with field data on
slow-binding processes, which is a tuning change and not a new decision. The gate is also
lighter for a terminal than for a tunnel, since a container that runs has a shell, and the
retry only covers the moment just after start when the daemon may still refuse an exec.

Gate 2 is stated as "the operation the session will perform" rather than "a check from the
agent" because a probe from anywhere else proves something other than what the developer is
about to do — and the agent is not guaranteed to be attached to a compose stack's own
network. If ADR-064 §2 is ever revisited and port-forward terminates at the agent, gate 2
moves with the byte path; that is the invariant, not the SSH hop.

### 6. The developer is told, on the wire the session already has

A session that appears to hang for a minute reads as the bug this whole investigation
started from. Two channels carry the news, and both already exist:

- the **mint response** states `waking`, so the client prints one line — the target, and the
  fact that a cold start of up to 75 s is under way — *before* it opens the local listener
  or the terminal's window;
- the **session's control wire** carries progress and the verdict. Both families already
  multiplex text control frames beside their data (`open_err`, `eof`, `close` for a tunnel;
  resize and the end message for a terminal), and `tunnel.HTTPControlFrame` is their shape
  on the ladder's HTTP rungs. A `waking` frame carries the wake's progress; on failure a
  `wake_failed` frame carries the waker's **own** message, which names the container the
  wake stalled on, and the session ends with `wake_failed` as its end reason — the audit row
  and the last line the developer reads coming from one value, as `grant_expired` and §2's
  `target_stopped` already do. `wake_failed` is one `ALTER TYPE ... ADD VALUE` on the enum
  both session tables share.

One end reason, not two: a gate-1 timeout, a gate-2 timeout and an operational wake failure
are the same event for the person reading it — the target did not come up — and the message
already distinguishes them. Splitting the enum would buy analytics nobody asked for.

Per ADR-064 §1 the wire identifiers stay parameterised per access path and are never pooled:
these frames are added to each family's own control vocabulary, not to a shared one.

### 7. Waking is a lifecycle act and is authorized as one

`port-forwards:open` and `terminal:open` are both on the write socle, and each authorizes
opening its own kind of session. Neither, by itself, authorizes starting production
containers — that is `applications:lifecycle`, on the deploy socle, and neither door may
become a side route around it.

- **Previews**: the session's own permission alone — `port-forwards:open` or
  `terminal:open`. A preview's cold start costs nobody, ADR-036 shipped scale-to-zero
  previews-first for exactly that reason, and the same person can already wake the preview
  by opening its URL. Requiring more would make the common case ceremonial for no gain.
- **Applications**: the session's own permission **and `applications:lifecycle`**. Waking a
  production application is starting it; it takes the permission that starting it takes,
  and it takes the same one whichever door asked — a rule that survives the next access
  path. Refusal happens at the mint, `403` naming the missing permission, and **no session
  row is created**: a session minted against something that will never come up is worse than
  a clean refusal.

`applications:exec` is deliberately **not** repurposed here. It is on the deploy socle and it
gates scheduled tasks; the terminal has never required it, and quietly making it the wake
permission for one door would split a rule that §7 exists to keep single.

A **server shell** wakes nothing: `terminal:root` and its passkey step-up are unchanged, and
this ADR neither widens nor narrows them.

**Audit**: a wake emits its own event, `port-forward.wake` or `terminal.wake`, recorded
against the **resource** (application or preview) with the session uuid in its details —
beside, not instead of, the existing `port-forward.open` / `terminal.open` on the session.
Someone reading an application's history must see "X woke this application to get into it"
without having to join the session log to it. The result is recorded too, so a wake that
timed out is legible.

**ADR-045's `sensitive` profile changes nothing here**, in both directions. An external
endpoint has no container of ours to wake (§8), so the grant path never meets this decision;
and no wake happens outside a mint the grant already authorized, so nothing here reaches
anything a grant did not already reach.

### 8. Who is woken, and who is not

| Target | Behaviour |
|---|---|
| Preview, `preview_scale_to_zero` armed, `sleeping` | Woken, by either door. |
| Preview already `waking` | Joins the in-flight wake (§4's gate); no second wake. |
| Application, `scale_to_zero` armed, `scale_slept_at` set, `desired_status = running` | Woken, under §7's permission. |
| Application or preview with `desired_status ≠ running` | **Never woken.** ADR-037 §3 forbids the scheduler from sleeping a manually stopped application; the symmetric rule is that nothing auto-starts one either. Refused with "this application is stopped — start it, then open the session". |
| Resource with scale-to-zero **off** whose containers are stopped | Not woken. They are stopped for some other reason, and guessing which one is not a session's business. Existing `409`. |
| Managed database | Never. ADR-037 §2 excludes databases from scale-to-zero by construction; this ADR does not reopen that. A stopped database is refused with a message pointing at its lifecycle action. |
| Compose *component* of a scale-to-zero application | Wakes **with the application** — the component is inside the wake set — then passes gate 2 on its own container. |
| Compose *service* resource (`services`) | Not woken: it has no scale-to-zero flag. |
| Declared external endpoint (ADR-045) | No wake, ever, on any code path. The address was frozen at declaration and belongs to somebody else's infrastructure; there is nothing of ours to start. |
| Server shell (`terminal:root`) | No wake, ever: no container, no resource, no clock. |

The same list governs §1's activity signal, minus the wake: only the two kinds that have a
scale-to-zero clock are given one.

### 9. No opt-out flag: the permission is the knob

Waking is **unconditional for previews** and **permission-gated for applications** (§7). We
deliberately add no `wake_on_tunnel` column and no instance setting.

The objection is real — an operator armed scale-to-zero to stop paying for an idle machine.
The answer is that scale-to-zero buys savings against *idleness*, and a developer attaching
a session is the definition of not-idle; that is the same reasoning that makes an HTTP
request wake the resource, and the same reasoning that makes §1 count an attached session.
A refusal would save nothing, because the same person wakes the same containers by loading
the same application's URL a second later. All it would buy is making the CLI the one door
that does not work — which is the bug being fixed.

For applications, the operator who does not want sessions waking production already has the
control: withhold `applications:lifecycle` from the people holding `port-forwards:open` or
`terminal:open`. That is an existing, auditable, per-role knob rather than a new flag with
its own default, its own migration and its own UI. Reversal condition, recorded so the next
reader does not have to guess: if a real operator asks for it, it is a **per-application
boolean**, never an instance-wide one — but we do not add a column against a hypothesis.

### 10. What this does not decide

- The **inactivity window** and its default (30 min) — untouched, in both directions. Nor
  the sessions' own idle and duration limits, which stay where ADR-024, ADR-032 and
  ADR-045 §5 put them, at their differing values.
- The **waker's own HTTP wake path**: hold-and-forward, the waiting page, the 1 MiB body
  limit, the 503/504 answers. Unchanged, and not re-argued.
- Whether port-forward should terminate at the **agent** rather than the control plane —
  ADR-064 §2 defers it; §5 above states the invariant that survives either outcome.
- The **token and attach-ordering** decisions of ADR-065 and ADR-066. This ADR only asserts
  that the cold start is not paid inside their requests.
- **Scale-to-zero for databases** (ADR-037 §2 excludes them; still excluded), and therefore
  any activity signal for them.
- Whether an attached session should keep a **non**-scale-to-zero resource from being stopped
  by an operator. It should not: an operator's stop wins, and §2 tells the client so within
  one beat.
- Whether the terminal should gain **mid-session revocation** now that §2 obliges the
  registry that would make it possible. The gap is named in §2; closing it is a separate
  decision about who may cut whose shell.

## Consequences

- A developer opens a tunnel or a shell into a sleeping preview and it works, with a first
  connection that takes up to 75 s and says so, twice, before it does. A developer already
  connected keeps their session, and learns within one beat when its target goes away.
- Scale-to-zero's activity signal has **more than one writer**, and the scheduler merges
  them. The cost is that "why is this still awake?" now has several possible answers; the
  mitigation is that every new one is a session row with an owner, a start time and an audit
  trail.
- **A mint is no longer a pure token issuance**: it can change server state. That is the
  real cost of the wake half and it is stated rather than buried — accepted, with §7's
  permission as the mitigation on the production side, and with the observation that on the
  preview side it grants nothing the same person could not obtain with a browser.
- A mint the developer then abandons (Ctrl-C before the first connection) leaves a woken
  resource. It re-sleeps at the end of the normal inactivity window: the cost is bounded by a
  quantity that already exists, and it is identical to a visitor who loads the URL and closes
  the tab. No cancel-on-abandon path.
- **The terminal costs more than the tunnel to bring in line**, and the work is named rather
  than assumed: an `OnHeartbeat` hook on `internal/terminal.Options`, a durable beat for
  `terminal_sessions` to write to, a presence registry that can cut a live session, and two
  Go end-reason constants. The database enum is already shared, so `target_stopped` needs no
  migration and `wake_failed` needs one value for both families.
- The agent gains one method. The channel gains no frame type; each family's wire gains two
  control-frame types on its own path only.
- **Older clients keep working**: they ignore a `waking` state they do not model and drop
  control-frame types they do not know, so a session degrades to "the first connection takes
  a while" and the end reason still reaches the developer through the close frame each
  family already prints — `target_stopped` included.
- **An older agent** answers `unimplemented` to `WakeResource`. The mint reads that as
  "cannot wake" and refuses with the same actionable message, rather than minting a session
  that would fail at the first stream.
- Scale-to-zero stops being an HTTP-only feature. A third non-HTTP access path, if one is
  ever added, finds an activity signal and a wake trigger to reuse instead of a hole to
  rediscover.

## Alternatives considered

- **Decide the terminal separately, in its own ADR.** Rejected: it is the same revision of
  the same clause of ADR-036, and splitting it would have meant two authorization decisions
  where the honest answer is one rule — the permission that starts an application does not
  depend on which door asked.
- **Have the agent write the waker's activity file for both families**, preserving "one
  file, one truth". Rejected: the agent cannot attribute a tunnel it never sees, nor an
  opaque exec attach, to a resource's activity clock — the control plane would have to tell
  it on every beat, which is a second reporter and a round trip for a fact the control plane
  already holds.
- **Count only forwarded bytes or keystrokes as activity** rather than attachment. Rejected:
  it sleeps the container under an open `psql` prompt, which is the original report with
  extra steps.
- **Cut a session whenever the target's state cannot be confirmed.** Rejected: an agent
  reconnect would kill every healthy session on that server, trading a rare hang for a
  routine one.
- **The control plane starts the containers itself** with the `Runtime` calls it already
  has. Rejected: it re-implements the wake-set graph and the readiness rule, and it creates
  a second starter for one resource with no shared single-flight — a browser hit and two
  mints arriving together would race each other through a compose stack.
- **Wake at the attach instead of the mint.** Rejected: the attach holds a one-shot token
  and no identity, by design, so it can neither authorize nor attribute a state change; and
  ADR-066 has it answering before it dials.
- **Wake lazily on the first stream open, with no mint-time decision.** Rejected: it moves
  an authorization decision onto a frame that carries no identity, and it starts the whole
  cold start *after* the client has already connected instead of overlapping it with the
  client's own setup.
- **Refuse, and tell the developer to load the URL first.** Rejected: it is what happens
  today, and the fact that it works is the defect — a documented workaround through another
  protocol is not a design.
- **Probe readiness from the agent** rather than by performing the session's own operation.
  Rejected: the agent is not guaranteed to be on a compose stack's network, and a probe off
  the real path proves something other than what the developer is about to do.
- **Hold the target awake with a dedicated lease** rather than the beat's activity stamp.
  Rejected as redundant: the beat already runs, for the liveness question, at the cadence
  this needs.

## Verification

Unit tests the implementation owes. Every clause below is asserted **on both families**
unless it names one.

**Activity (§1)**

- Activity is recorded per target kind: a preview session stamps the preview, an application
  session stamps the application, and a managed database, a Compose *service* resource, an
  external endpoint and a server shell stamp **nothing** — asserted as the absence of a
  write, not merely as no error.
- A terminal on one component of a Compose-deployed application stamps the **application**.
- The application sleep decision honours the new signal: an application whose waker file is
  stale but whose `last_activity_at` is fresh is **not** slept, and the merge takes the
  latest of the file, `updated_at` and `last_activity_at`.
- A beat survives a transient database failure: it logs and returns "alive", and only a
  durably finalized row (no rows updated) ends the session. An activity write that fails is
  likewise dropped, never fatal to the session.
- The mint stamps activity on both branches — the wake path and the already-awake path — so
  the window before the first beat cannot be read as idleness.
- The terminal's new beat runs at the same cadence as the tunnel's and writes the same
  signals: the test that would have caught the two bridges drifting apart.

**Liveness (§2)**

- A container the agent definitely reports absent (`not_found`) or not running ends the
  session with `target_stopped`, and the client receives that reason.
- **No** cut when the agent channel is merely unavailable, when the inspect fails with
  anything other than not-found, or when the state cannot be read — the assertion that keeps
  an agent restart from clearing a server's sessions.
- An external-endpoint session and a server shell are never inspected for a target state and
  never cut for one.
- Every rung enforces it: the WebSocket bridge and the HTTP session produce the same end
  reason for the same target state, on both families.

**Wake (§3–§9)**

- A sleeping preview with scale-to-zero armed: the mint issues exactly one `WakeResource`,
  answers `waking`, and the session's own operation — the first stream's dial, or the exec
  attach — is **not** attempted while the wake is in flight, then is, once it returns ready.
- Two mints against the same sleeping resource, including one of each family, issue **one**
  wake and both sessions become usable (single-flight through the module's gate).
- No wake, and no wake command on the channel, when: the flag is off; the kind has none
  (managed database, Compose service resource, server shell); the target is a declared
  external endpoint, whatever its egress server holds.
- `desired_status ≠ running`: never woken, no command sent, and the message names the manual
  start rather than the missing container.
- The bounded wait ends with a verdict, never a hang — gate 1 expiring (the agent returns the
  waker's stalled-container error) and gate 2 expiring (the port never accepts, or the exec
  is still refused) both close the session with `wake_failed` and a message naming the
  container and the operation that failed.
- Authorization: the session's own permission alone wakes a preview; on an application
  without `applications:lifecycle` the mint returns `403` naming the permission and **creates
  no session row** — asserted for `port-forwards:open` and for `terminal:open` separately,
  since they are different handlers.
- The server terminal's `terminal:root` and step-up path is unchanged by any of this, and
  reaches no wake code.
- Audit: `port-forward.wake` / `terminal.wake` is emitted against the resource, with its
  result, on success **and** on failure — and is not emitted when the target was already
  running.
- An agent answering `unimplemented` produces the refusal at mint, not a failure at the
  first stream.
- Compatibility: an unknown control-frame type is ignored by each client's dispatch and the
  session still carries bytes.

The live cold start — a real container graph coming up behind a real dial — stays where
ADR-028 puts it: at most one assertion enriched in the single E2E journey, never a second
journey.
