# ADR-065 — An attach token is spent by the session it opens, not by the request that tries

- **Status**: Accepted
- **Date**: 2026-08-09
- **Scope**: the two **SQL-claimed** attach paths — port-forward/bastion
  (`ClaimPortForwardSession`) and terminal (`ClaimTerminalSession`). The ingress attach is
  deliberately excluded (§2)
- **Revises**: [ADR-032](ADR-032-tcp-tunnel-cli-websocket.md) — the redeem clause's
  "single-use atomic claim", and only in what "single use" counts; and
  [ADR-024](ADR-024-realtime-sse-websocket-terminal.md) — the terminal's redeem in the same
  narrow sense. The `akdp_` prefix, the 60 s TTLs, hash-at-rest, the per-team caps, the
  heartbeat/idle/max-duration bounds, one-mint-one-session-one-audit and the addressless
  wire all stand unchanged
- **Related**: [ADR-064](ADR-064-one-transport-ladder-for-every-cli-tunnel.md) (the ladder
  whose step-down burns the token, and which put the terminal on it), **ADR-066** (the
  attach answers before dialling — the same failure attacked from the other end;
  complementary, not alternative), **ADR-067** (a port-forward may wake a sleeping resource
  — which widens the window this closes),
  [ADR-045](ADR-045-external-endpoint-port-forwards.md) (grant-bound sessions),
  [ADR-060](ADR-060-dev-ingress-tunnels.md)/[ADR-061](ADR-061-ingress-http3-http2-websocket-fallback.md)
  (the ingress mint this mirrors, and does not change),
  [ADR-027](ADR-027-removal-tunnels-provisioning-patching.md) (one wire identity per access path)
- **Related PRD sections**: §5.7 (operations), §12 (CLI), §23.2 (secrets at rest), §24.4
  (real-time sessions)

## Context

Two attach paths redeem a single-use token against a row in PostgreSQL, and both burn it
on the *attempt* rather than on the *session*.

| Path | Claim | Then, before the response head |
|---|---|---|
| Port-forward / bastion | `ClaimPortForwardSession`, at the top of `tunnelAttachSession` and of its WebSocket twin `TunnelWebSocket` | `tunnelTarget` — an agent `ContainerInspect` RPC and a full, unpooled `serverdial.Open` |
| Terminal | `ClaimTerminalSession`, at the top of `terminalAttachSession` and of its WebSocket twin `TerminalWebSocket` | `terminalPTY` — the same unpooled `serverdial.Open` for a server shell, or an agent exec-create/attach for a container shell |

Both claims are strict single-use consumes at the very top of the handler:

```sql
UPDATE …_sessions SET claimed_at = now(), started_at = now(), …
WHERE token_hash = $1 AND claimed_at IS NULL AND ended_at IS NULL AND token_expires_at > now()
```

Everything slow happens *after* the claim, and nothing about it is visible to the client
while it waits.

Meanwhile the client is climbing ADR-064's ladder. When a rung's attach does not answer
inside the open budget, the CLI classifies it as a transport failure and steps down
(`attachRejection.transportRefused`, in `forwardOverHTTP`) — correctly, since from the
client's side an unanswered attach is indistinguishable from a transport that cannot carry
the tunnel. But the server has already claimed. Every remaining rung, and the WebSocket rung
underneath them, then fails with `invalid, expired or already used tunnel token`. The
terminal is in exactly the same position and additionally passes the generic 5 s
`transportAttachTimeout` for its session request (`openTerminalSession`), so its budget does
not even cover the dial it is waiting on.

This is not a thought experiment: it is what a developer hit on
`akerdock port-forward … --pr 4865`. The message compounds the damage by being false in all
three of its terms — nothing was invalid, nothing had expired, and nobody replayed
anything. The client's own first attempt burnt it, and then the client was told it had been
robbed.

A tactical fix ships separately: the session attach gets its own timeout derived from what
the server actually spends, the way `egressDataOpenTimeout` is already derived from
`tunnel.EgressDialTimeout`, plus a bounded client-side re-mint when a claim is refused. That
narrows the window. It does not close it, and cannot:
**any** abandonment after the request reached the server burns a token that never served a
byte — a laptop's wifi handing over, a QUIC path failure, a front that resets the
connection, an operator restarting an API replica mid-attach. The re-mint hides the symptom
at the cost of a second row, a second cap slot and a second audit line for one developer
intent.

Two sibling decisions move the same window from the other side. **ADR-066** makes the
attach answer before dialling, shrinking the pre-response work to almost nothing — but it
also creates, deliberately, a session that is open and has dialled nothing, which is
precisely the state that must survive a retry. **ADR-067** lets a port-forward wake a
sleeping resource, which makes the pre-answer work *seconds to tens of seconds* in the one
case where it is longest. Under ADR-067 an idempotent claim stops being a nicety and
becomes a precondition.

## Decision

The claim becomes **idempotent within the token's TTL and bound to the attacher**, instead
of strictly single-use, on both SQL-claimed paths.

### 1. The property being protected is one session per mint, not one request per mint

The single-use rule exists to stop a **replay**: someone who obtains a token must not be
able to open a second, parallel, authenticated session with it. "One HTTP request per mint"
was only ever a proxy for that, and it was an exact proxy for as long as one request *was*
one session — which stopped being true the day the attach acquired retries above it
(ADR-064) and will be less true still with ADR-066's lazy dial.

The property this ADR keeps, restated so it is enforceable rather than incidental:

> A mint authorizes **at most one live authenticated session**, held by **one attacher**,
> for the life of the row; and the window in which an attacher may (re-)take that session
> is the token's TTL and nothing longer.

Under that statement a second *request* from the same attacher, inside the TTL, is not a
replay — it is the same attempt, retried. A request from a different attacher is a replay
and stays refused. "At most one live attach" is enforced exactly where the attach lives,
and converges elsewhere at the speed each path's liveness allows — one heartbeat on
port-forward, and on the terminal a pre-existing limit this ADR names rather than hides
(§5).

### 2. One rule, two paths — and why ingress is not the third

The port-forward and terminal attaches are the same mechanism twice: a row minted with a
60 s token, a hash at rest, a claim that is one `UPDATE` with the consume in its `WHERE`,
a per-team cap counting claimable and claimed rows alike, and now the same ladder above
them. A rule that fixed one and not the other would leave the identical defect standing in
the identical code, which is precisely the outcome ADR-064 §5 exists to prevent.

They are two *tables*, not one, and this ADR does not merge them. What differs, verified
rather than assumed:

- **Separate tables and queries**: `port_forward_sessions` / `terminal_sessions`,
  `db/queries/portforwardsessions.sql` / `db/queries/terminalsessions.sql`, separate token
  hashes and separate claim statements. Each gets its own columns and its own rewritten
  claim; neither grows a foreign key to the other.
- **A shared end-reason enum**: both tables type `end_reason` as `terminal_end_reason` (the
  port-forward table has done so since migration 00056). No new value is added by this
  decision on either path.
- **Separate caps**: `portForwardTeamCap` = 10, `terminalTeamCap` = 20.
- **Different liveness**: `port_forward_sessions` carries `last_heartbeat_at` and a 90 s
  freshness floor used by the cap, the list and the sweep. `terminal_sessions` carries no
  heartbeat at all — its sweep is keyed on `started_at` against the max-duration ceiling.
  This is the one difference with teeth, and §5 and §6 pay for it explicitly.
- **Different stream shape**: an egress session has many data streams; a terminal session
  has exactly one, a second refused by a CAS in `terminalAttachStream`. §6 uses
  that difference rather than working around it.

**Ingress stays out.** Its claim is an in-memory map lookup inside the agent
(`internal/agent/ingress.go`), not a SQL statement: its atomicity argument is a mutex
rather than a row lock, its state is 60 s of agent memory rather than a durable row, and
its rolling-upgrade story is an agent rollout rather than a migration. Transposing this
decision there is plausible and is *not* the same change; it would be its own ADR, with its
own answer to what happens when the agent restarts between two rungs. No ingress failure of
this shape has been reported, and inventing the fix before the failure would fix the wrong
thing.

### 3. The attach key is minted once per token, and travels every rung

Both paths already generate a per-attempt 256-bit attach key (`tun.NewIngressAttachKey`),
present it as the path's `AttachKeyHeader`, and hash it on sight (`decodeAttachKey`) — it is
the credential that binds data streams to the control request that spent the token. It is
exactly the "who is attaching" identity this decision needs, and it costs nothing to reuse.

One change of scope makes it usable: the key was generated **inside the rung loop**
(`forwardOverHTTP`, and its terminal counterpart), so every rung was a different attacher. It
moves up to the mint — **one attach key per mint**, generated beside the token in
`forwardSession` and carried unchanged through the whole ladder climb, WebSocket rung
included. The key stays ephemeral, per-process,
never logged, and never stored in plaintext: the row keeps its SHA-256 only, on the same
footing as the token hash (§23.2).

It is therefore as sensitive as the token for as long as the token lives — 60 s — and no
longer. It is not a session credential; it is a claim credential that also gates data
streams.

PRD §24.4 requires a realtime token to be "short-lived/single-use **or scoped to the
resource**". A token that is short-lived, bound to one session and bound to one attacher
satisfies that requirement on its own terms; this decision does not need the PRD's
normative wording relaxed.

### 4. The claim stays exactly one statement

The claim **must** remain a single statement. Read-then-write would race two rungs of the
same ladder against each other and produce two attaches that both believe they own the
session — the one failure mode strict single-use never had. So the idempotence goes into
the `WHERE`, not into the handler:

```sql
-- name: ClaimPortForwardSession :one
-- Idempotent within the TTL, bound to the attacher (ADR-065): a first claim stamps the
-- attacher's key hash, a re-claim must present the same one, a different one matches zero
-- rows — which is the replay the rule exists to stop.
UPDATE port_forward_sessions
SET claimed_at        = coalesce(claimed_at, now()),
    started_at        = CASE WHEN attach_seq = 0 THEN now() ELSE started_at END,
    last_heartbeat_at = now(),
    attach_key_hash   = $2,
    attach_seq        = attach_seq + 1
WHERE token_hash = $1
  AND ended_at IS NULL
  AND token_expires_at > now()
  AND (attach_key_hash IS NULL OR attach_key_hash = $2)
  AND (authorized_until IS NULL OR authorized_until > now())
RETURNING *;
```

`ClaimTerminalSession` takes the identical shape, minus the two clauses that have no
column on its table (`last_heartbeat_at`, `authorized_until`). Its existing comment —
"`started_at` is reset at claim time so idle/max-duration windows measure the live session"
— survives intact for the *first* claim, which is the only one that can move it.

- `token_expires_at > now()` is unchanged and is the whole of the re-claim window. There is
  no new lifetime concept in this decision: an expired token is refused exactly as before.
- The right-hand sides read pre-update values, so `coalesce(claimed_at, now())` and the
  `CASE WHEN attach_seq = 0` pin `claimed_at` and `started_at` to the **first** claim. A
  re-claim must not restart the ceiling, or a retry loop would buy duration.
- `authorized_until` is checked at claim time as well as at mint. A grant revoked between
  two rungs already ends the row (`endSessionsOfGrant`), so `ended_at IS NULL` catches it;
  the explicit clause is the belt to that brace, because a re-claim is the one path that can
  now arrive *after* an authorization changed.

Columns added, all additive and rolling-upgrade safe — an N-1 replica ignores them, and one
goose migration carries all of them:

| Table | Column | Why |
|---|---|---|
| both | `attach_key_hash bytea` | the first claimant's key, hashed. Nullable only so the migration is additive; the claim never writes NULL (§7) |
| both | `attach_seq bigint NOT NULL DEFAULT 0` | the attach generation — what makes §5's supersession decidable rather than a guess |
| `terminal_sessions` | `streamed_at timestamptz` | stamped once when the session's single data stream joins; used by the sweep only (§6) |

### 5. At most one live attach: supersession, not coexistence

A successful re-claim means the previous attach lost. It must be torn down, not left
running beside the new one — two live attaches on one session is the exact thing §1
forbids, and it would also mean two SSH clients or two PTYs against the target.

- **Same replica** — the common case, since the ladder's rungs reach whatever the front
  routes them to and that is usually one process. The displaced attach is cut with the
  wire-only reason `superseded`, gated on `attach_seq > 1`, which is what makes supersession
  decidable rather than a guess: anything above the first claim is a re-claim.

  The cut travels the register that carries a **reason** and is keyed on the session row
  rather than on a transport, so it reaches an incumbent on either rung: `TunnelPresence.Cut`
  on port-forward — the same register a revocation uses, and the only one that reaches a
  WebSocket incumbent, which the HTTP path's in-process attach map never sees — and, on the
  terminal, which has no such register, `terminalRegister`/`terminalSupersede` returning the
  displaced attach so `terminalCut` can fire its `finish()` and its context cancel. One
  mechanism per path rather than two, and no register is asked to both publish and tear down
  on the egress side.

  **`superseded` is wire-only and is deliberately not a `terminal_end_reason` value.** It is
  a fact about a socket, not about the session — the session it names is still open, for the
  attach that won — so the loser must never finalize the row. Had that reason reached the
  database, PostgreSQL would have refused the enum cast whatever the generation guard did,
  which is a crash where a silent no-op was intended.

  One correction to what this ADR originally asserted, kept visible because it is exactly
  the kind of thing a future reader would re-break: **`TunnelPresence.unregister` did not
  release by pointer comparison** — it deleted unconditionally by session id. Implemented as
  first written, a superseded attach leaving would have evicted the *winner's* cancel
  channel, leaving a live tunnel that ADR-045 revocation could no longer reach and that
  shutdown no longer waited for. `unregister` now takes the caller's own channel and removes
  only its own entry. Identity-aware release is a precondition of supersession, not a
  detail of it.
- **Another replica, port-forward.** The heartbeat becomes generation-aware:
  `HeartbeatPortForwardSession` matches `attach_seq = $2` in addition to `id`, and the
  bridge already ends itself when the heartbeat updates zero rows — "another replica or the
  scheduler finalized this" was always its meaning, and "another attach superseded me" is
  the same sentence. The superseded attach dies within one 20 s beat. This is the ADR-045
  convergence layering (socket-local truth, reported heartbeat, leader sweep) with no new
  mechanism; it is eventually consistent by construction and the ADR says so rather than
  implying a synchronous cut it cannot deliver.
- **Another replica, terminal.** `terminal_sessions` has no heartbeat column and the
  terminal has no cut registry, so there is nothing for a generation to travel on: a
  superseded PTY on a foreign replica survives until its own idle timeout or max duration.
  This ADR does not create that gap — a revoked or operator-cut terminal on a foreign
  replica is unreachable today for the same reason — and it does not close it, because
  giving the terminal a durable heartbeat is a change to its liveness model and belongs in
  the ADR that fixes cross-replica terminal revocation generally. Until then, "at most one
  live attach" is exact per replica on the terminal and eventually consistent on
  port-forward. Stated here so that a later reader finds it recorded rather than discovers
  it.
- Both `EndPortForwardSession` and `EndTerminalSession` gain the same optional guard: an
  attach finalizes the session only while it is still *the* attach. Revocation, operator cut
  and the sweep pass no generation and finalize unconditionally, because their verdict is
  about the session and not about whichever socket happens to hold it.

**A re-claim is permitted until the token's TTL expires, whether or not a data stream has
already been served.** The narrower rule — "only while nothing has been forwarded" — was
rejected: it needs a durable "served" flag written on the egress data-stream hot path, and
its extra security is illusory, since an attacker who holds both the token and the 256-bit
attach key inside the same 60 s is already reading the legitimate client's request headers.
The bound that matters is the TTL, and it is unchanged.

### 6. An abandoned attach does not finalize the session; a refused one does

Idempotence in the claim is worthless if the abandoned attach kills the row on its way out
— and today both paths do. When the client vanishes mid-`tunnelTarget` or mid-`terminalPTY`,
the request context cancels, the dial fails, and the handler calls
`endPortForwardSession(…, EndDisconnect)` or `endTerminalSession(…, EndRevoked)`. The next
rung is then refused by `ended_at IS NULL`, having gained nothing.

So the server draws the same line ADR-060 §6 draws on the client: **transport versus
policy**.

- **The client went away before anything was served** — request context canceled, the
  response head never flushed, or (terminal) `awaitTerminalStream` timing out because the
  client died between the two requests: release the attach, close the PTY or the SSH
  client, and leave the row claimed, un-ended and re-claimable for the remainder of its TTL.
  Nobody is told anything, because nobody is listening.

  **Bounded by `token_expires_at`, and that bound is load-bearing**: "the request context was
  canceled" also describes a four-hour session ending perfectly normally, and a rule without
  the bound would leave every such row un-finalized. Past the TTL there is nothing left to
  rescue — no key can re-claim an expired token — so the attach finalizes as the
  `disconnect` it was. Port-forward enforces it in the handler
  (`endAbandonedPortForwardAttach`); the terminal reaches the same instant through the sweep
  clause below, its abandonment path simply returning.
- **The attach was refused on its merits** — target no longer exists, container not running,
  agent not connected, full duplex unavailable, grant expired: finalize as today. A re-claim
  would only reproduce the same verdict, and leaving the row open would invite the CLI to
  spend its whole ladder discovering it three more times.

The cost is a row that is claimed, unattached and still counted as open. On **port-forward**
that is bounded at 90 s by the sweep's existing heartbeat floor — precisely the state a
crashed CLI already leaves behind, bounded by machinery that already exists, and accepted
as-is (§8).

On the **terminal** the same rule would be unbounded: `CountOpenTerminalSessions` counts
`claimed_at IS NOT NULL` with no freshness test, and `SweepTerminalSessions` finalizes a
claimed row only at the max-duration ceiling — so an abandoned attach would hold one of the
20 slots for hours, which is not the same accepted magnitude at all. The terminal therefore
stamps **`streamed_at`** when its single data stream joins — one write per session, not per
stream, which is only affordable because a terminal has exactly one (§2) — and its sweep
gains one clause:

```sql
OR (claimed_at IS NOT NULL AND streamed_at IS NULL AND token_expires_at < now())
```

finalized as `disconnect`. A claimed terminal row that never carried its PTY is closed once
its token dies, so the slot is held for at most the TTL plus one sweep interval.

`streamed_at` is read by the sweep and by nothing else. It is **not** a re-claim condition:
§5's rule is that the TTL is the bound, and a session that has served bytes is re-claimable
exactly like one that has not. Recorded emphatically because the column is otherwise
one careless `AND` away from becoming the rule this ADR rejected.

### 7. The WebSocket rung carries the key too, and a keyless claim stays strictly single-use

Both WebSocket rungs present the token in a query parameter and carry no attach key — and a
WebSocket attach arriving after an HTTP rung burnt the token is *exactly* the failure the
developer reported. Leaving the bottom rung out would fix the ladder everywhere except where
it lands.

The CLI therefore sends the same per-mint key on the WebSocket dial, in the **same header**
each path already uses (`Akerdock-Egress-Key`, `Akerdock-Terminal-Key` — one wire identity
per access path, ADR-027 and ADR-064 §1), not in the query string: a CLI WebSocket dial can
set arbitrary headers, and there is no reason to widen what lands in an intermediary's
access log. The token stays where ADR-024 and ADR-032 put it.

An attach that presents **no** key — an N-1 CLI during a rolling upgrade, or the dashboard's
browser terminal, which is not on the ladder and has no retry to rescue — is still accepted,
and the claim stores a **server-generated random 32 bytes** in `attach_key_hash` rather than
a NULL or a fixed sentinel. No presented key hashes to it, so such a session is strictly
single-use, exactly as today. This is deliberate and worth stating plainly: the column must
never be left in a state that matches *anything*, or an old client's token would become
freely re-claimable for 60 s by whoever holds it — turning a compatibility shim into the
replay hole this ADR is supposed to keep shut.

### 8. The caps, the grant, and the audit

- **Per-team caps** (`portForwardTeamCap` = 10, `terminalTeamCap` = 20). Unchanged in value
  and in meaning, and materially *more* accurate. Today a three-rung climb with the tactical
  re-mint produces up to four rows, and each unclaimed one counts until its TTL lapses — the
  cap measures transport attempts. One mint, one row, one slot restores it to measuring
  concurrent sessions. Neither `CountOpenPortForwardSessions` nor `CountOpenTerminalSessions`
  changes: a claimed row counts once, regardless of how many times it was attached.
- **ADR-045 grant-bound sessions.** A grant authorizes *minting*; this ADR touches nothing
  there. `authorized_until` is frozen on the row at mint, a re-claim cannot extend it (§4),
  and a revocation between two rungs is refused by both `ended_at` and the explicit clause.
- **Audit.** `port-forward.open` and its terminal counterpart are emitted at **mint**, so
  the fix is to what the mint count means: one
  developer intent produces one mint, one open line and one close line, instead of one open
  line per rung. **A re-claim emits no audit event** — it is not a second open, and recording
  it as one would restate the very confusion this ADR removes. Nothing is lost to forensics:
  `attach_seq` on the row answers "how many times was this session attached", and a
  supersession is a structured log line on the session, where operational noise belongs.

### 9. What this does not decide

The token TTLs (60 s, `portForwardTokenTTL` / `terminalTokenTTL`), who may mint and under
what permission, the transport ladder and its order, the mint prefixes and hash-at-rest
discipline, the terminal's idle/max-duration bounds, and what terminates each tunnel — all
unchanged. The ingress attach keeps its agent-side in-memory single-use claim (§2). Giving
the terminal a durable heartbeat, and with it cross-replica revocation and cross-replica
supersession, is named in §5 and left to the ADR that takes on terminal liveness as a
subject.

## Alternatives considered

- **Only fix the budgets and re-mint on refusal** (the tactical change already in flight).
  It was the right thing to ship first — it was small, it was safe, and it bought back most
  of the failures while this decision was being written. But it is a stopgap this decision
  **retires**, not a behaviour it keeps. The re-mint existed to remedy a token burnt by an
  abandoned attempt; once an abandoned attempt burns nothing, there is nothing left for a
  second mint to remedy, and keeping it would contradict §8 outright — the cap argument is
  that **one climb is one row**, and a client that mints again on refusal makes one climb up
  to four rows again. The budget derivation stays (a client that waits long enough for the
  server's own work is right for reasons that have nothing to do with claims); the re-mint
  goes.
- **Make the token multi-use for its TTL, full stop.** Rejected: it drops the replay property
  entirely. Two laptops could hold two tunnels into a production database from one mint, and
  the audit trail would name one open for two sessions.
- **Fix port-forward only, and leave the terminal.** Rejected: the terminal claims the same
  way, dials something slower on the same code path, climbs the same ladder, and passes a
  budget that does not even cover its own dial. Fixing one is how the three transport defects
  of ADR-064's context got paid for three times.
- **Extend the rule to ingress in the same pass.** Rejected for now — §2: a mutex over agent
  memory is a different atomicity argument and a different upgrade story, and no failure of
  this shape has been observed there.
- **Claim after the dial instead of before it.** Rejected as the primary lever, though it is
  the right instinct and is what ADR-066 does for other reasons: it moves the window rather
  than removing it — a client that abandons *after* the dial and before the response head
  still burns the token — and doing the target work before authenticating the attach means an
  unauthenticated request can cost an SSH connection or a PTY.
- **Bind the re-claim to the client IP instead of an attach key.** Rejected: both too loose
  and too tight. Too loose, because everyone behind one corporate NAT shares it; too tight,
  because the failure this fixes is frequently *caused* by a network change, and a laptop
  moving from wifi to a hotspot mid-ladder would be refused precisely when it most needs the
  retry.
- **Keep single use and let the CLI re-mint silently on every refusal.** Rejected: a client
  that answers "already used" by minting another one has turned a single-use rule into a
  suggestion, the same argument ADR-060 §6 makes about reconnecting through a revocation. It
  would also make a genuine replay indistinguishable from a retry in the audit log.
- **Allow the re-claim only before the first data stream.** Rejected — §5: a durable flag on
  the egress hot path, buying security that the 60 s TTL and a 256-bit key already provide.
- **Revert `claimed_at` to NULL on abandonment** instead of leaving the row claimed. Rejected:
  it would make both sweeps finalize the row as `revoked`, which is a lie about what happened,
  and it would erase from the row the fact that an attach occurred — the one thing the audit
  trail cannot reconstruct afterwards.
- **A monotonic attach counter carried by the client instead of a stored key hash.** Rejected:
  a counter is guessable, so it identifies the attempt without authenticating the attacher,
  which is the whole job.

## Consequences

- **Positive**: the ladder's step-down stops destroying the session it is trying to save,
  which is what makes ADR-064 usable on a flaky network, on both paths it put there; the
  failure that produced `invalid, expired or already used tunnel token` on a first attempt
  disappears, and the message becomes true whenever it is printed; one mint means one row,
  one cap slot and one audit pair again; ADR-067's wake, the slowest pre-answer work the
  product will ever have on this path, becomes survivable rather than a guaranteed burn; the
  replay property is stated as an invariant instead of being an accident of request counting;
  and the terminal gets the first durable trace it has ever had of whether its PTY actually
  attached.
- **Negative**: two columns on `port_forward_sessions`, three on `terminal_sessions`, and a
  rewritten claim on both hot paths; the attach key gains a durable hash and a security role
  it did not have (it now gates re-claiming, not only data streams); an abandoned attach
  leaves a row counted as open for up to 90 s on port-forward and for the remainder of the TTL
  plus one sweep interval on the terminal, instead of being finalized at once; supersession
  across replicas converges within one heartbeat on port-forward and not at all on the
  terminal (§5), and the ADR owns that rather than pretending otherwise; both WebSocket rungs
  grow a header, so CLI and server versions must be reasoned about during a rolling upgrade
  (§7 makes both directions safe).
- **Accepted risks**: an attacker holding **both** the token and the attach key within the
  same 60 s can supersede a live session rather than merely being refused. Obtaining both
  means reading the legitimate client's request headers, at which point the session's bytes
  are already exposed; the exchange buys a materially better failure mode for every honest
  client. A client that re-claims in a loop can flap its own session — its own session, its
  own 60 s, and the caps are unaffected. The cap-slot imprecision above is accepted as stated,
  on both paths.

## Verification

Unit level (ADR-028 — no new E2E journey), owed by the implementation. Every claim assertion
below is owed **twice**, once per path, because the two claims are two statements over two
tables and a table-driven test that only covers one proves nothing about the other:

- **Same-key re-claim accepted inside the TTL**: a second claim with the same token and the
  same attach key returns the same row, with `claimed_at` and `started_at` unmoved and
  `attach_seq` incremented.
- **Different-key re-claim refused**: the same token with a different attach key matches zero
  rows and answers `401`, leaving the row and its stored hash untouched — the replay case,
  asserted as such.
- **Re-claim after a stream has been served is accepted**, and the previously served attach is
  superseded rather than left running. On the terminal, the superseded attach's data-stream
  handler is released by `finish()` rather than left parked on `attach.done`.
- **The previous half-open attach is torn down**: in-process, the displaced attach is closed
  with `superseded` and its SSH client or PTY released; on port-forward across processes, its
  next heartbeat matches zero rows on `attach_seq` and it ends itself.
- **A superseded attach never finalizes the session**: `EndPortForwardSession` /
  `EndTerminalSession` guarded by a stale `attach_seq` update zero rows, the row stays open
  for the winner, and no close audit is emitted for the loser.
- **Client abandonment before anything is served leaves the row re-claimable** — including the
  terminal's `awaitTerminalStream` timeout path — while a refusal on the merits (target gone,
  agent disconnected, PTY refused) finalizes it and a subsequent same-key claim is refused.
- **`streamed_at` is stamped exactly once**, on the stream that joins, and never read by the
  claim: a session whose `streamed_at` is set is still re-claimable inside its TTL.
- **The terminal sweep closes an abandoned claimed row at token expiry** as `disconnect`, and
  leaves a claimed row that did stream alone until its own ceiling.
- **Expired token still refused**: a claim one millisecond past `token_expires_at` fails with
  the same key that would have succeeded before it.
- **Concurrent double claim resolved by the single statement**: two claims with the same key
  issued in parallel both succeed and receive *distinct* `attach_seq` values, exactly one
  survives, and no interleaving produces two rows or two live attaches. The same test with two
  different keys yields exactly one success.
- **Revocation beats re-claim**: a grant revoked between two claims refuses the second, via
  `ended_at` and via `authorized_until` independently.
- **Keyless (N-1, and the browser terminal) attach stays strictly single-use**: a claim
  presenting no key succeeds once, a second keyless claim on the same token is refused, and
  the stored value matches no presentable key.
- **The WebSocket rungs participate**: an HTTP rung that claims and is abandoned is followed by
  a successful WebSocket attach carrying the same key — the reported failure, as a regression
  test, on both paths — and by a refused one carrying a different key.
- **The key is one per mint, not one per rung**: the CLI generates it before the ladder loop
  and every rung, WebSocket included, presents the same value.
- **Nothing is logged**: neither attach key nor token appears in any log line, error message or
  audit payload, on any of these paths.
- **Cap accounting**: one mint attached over three rungs counts as one open session throughout,
  on both caps, and returns to zero when the session ends.

## Deliberately deferred

Neither of these is an open question about *this* decision; both are named so that the next
person finds them recorded rather than rediscovers them.

- **A durable heartbeat for `terminal_sessions`.** Its absence is why cross-replica
  supersession does not converge on the terminal (§5), and it is the same absence that makes
  cross-replica terminal revocation ineffective today, independently of this ADR. Fixing it
  changes the terminal's liveness model — a heartbeat column, a periodic write from
  `terminal.Bridge`, a freshness term in the cap and the sweep — and belongs in the ADR that
  takes that on, where the port-forward path's 90 s floor is the obvious template.
- **The ingress attach.** §2 states why it is a different change; if a claim-burnt-by-a-rung
  failure is ever observed there, this ADR is the shape to copy, not to extend.
