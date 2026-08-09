// Scale-to-zero for the two access paths that never cross the proxy the waker
// sits in (ADR-067): the CLI's TCP tunnel and the container terminal. This file
// is the WAKE half, and it is one implementation for both families because the
// decision is one decision — §4's command knows nothing about which door asked,
// and §7's permission does not depend on it either.
//
// The shape, end to end:
//
//   - the MINT decides (§3, §7, §8) and asks. It never waits for the wake: it
//     answers 201 and the wait is paid inside the session that follows;
//   - the AGENT's waker module performs it (§4) — the same wake-set graph, the
//     same single-flight gate and the same readiness rule an HTTP hit runs, so
//     a browser hit and two mints arriving together join one wake instead of
//     racing three starts through a compose stack;
//   - the SESSION pays the two gates (§5) — gate 1 is the command's own answer,
//     gate 2 is the operation the session exists to perform, retried.
//
// A session that never had a wake never touches any of this: every entry point
// below answers "nothing to do" for the zero sessionWakeSpec, which is what a
// managed database, a Compose service resource, an external endpoint and a
// server shell resolve to (§8).
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/terminal"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// The two budgets of ADR-067 §5, and the ceiling they add up to.
//
// THE WALL CLOCK IS THE CONTROL PLANE'S, and that is not a detail of taste.
// The waker's own WakeTimeout is not a wall clock: it re-arms every time a
// container of the wake set is newly released (internal/agent/waker.go),
// deliberately, so that a five-service stack is not asked to cold-start inside
// one container's budget. A healthy stack can therefore run past 60 s of wall
// clock while still making progress, which means the agent cannot enforce a
// wall clock without growing a second readiness rule beside the waker's — the
// one thing §4 forbids it. What the control plane owes the developer is not a
// readiness verdict but a bound on how long they wait for one, so it puts that
// bound where it belongs: on the command's context. The agent reads the
// cancellation, rolls back what it started, and answers `canceled`.
const (
	// sessionWakeGate1 bounds the WakeResource command — §5's "≤ 60 s", spent
	// on the containers coming up.
	sessionWakeGate1 = 60 * time.Second
	// sessionWakeGate2 bounds the retries of the session's own operation — the
	// TCP dial for a tunnel, the TTY exec attach for a terminal. §5 states it as
	// asserted rather than derived: move it with field data on slow-binding
	// processes, which is tuning and not a new decision.
	sessionWakeGate2 = 15 * time.Second
	// sessionWakeCeiling is §5's number for the developer — 75 s — and it is
	// the SUM of the two gates rather than a third budget superimposed on them,
	// because a third budget is how two of them start disagreeing. The CLI
	// carries the same number on its side (egressWakeOpenTimeout,
	// internal/cli/egress_transport.go): a client budget shorter than this one
	// makes the two deadlines race, and the client then blames the transport
	// for a target that is simply still starting.
	sessionWakeCeiling = sessionWakeGate1 + sessionWakeGate2

	// sessionWakeRefusalWindow is the only thing the mint ever waits for, and it
	// is the one place §3's "answers without waiting" and the compatibility
	// clause's "unimplemented produces the refusal at MINT, not a failure at the
	// first stream" have to be reconciled. They can be, because the answers that
	// must arrive before the mint commits are the ones that cost the agent no
	// work at all: `unimplemented` from a build that predates ADR-067, and
	// `not_found` from a server holding no wake set for that uuid, are one
	// channel round trip away and nothing else.
	//
	// So the window is sized for a round trip and not for a wake — several times
	// what one costs, and a small fraction of what a cold start does. Overrun it
	// and nothing breaks: the session is minted and the same failure reaches the
	// developer on the session's own wire instead, which is the ordinary path.
	sessionWakeRefusalWindow = 750 * time.Millisecond

	// sessionWakeRetention is how long a settled wake stays findable by an
	// attach. It only has to outlast the attach token's TTL plus a climb down
	// ADR-064's ladder: past that no attach can arrive, and the entry is memory
	// held for nobody.
	sessionWakeRetention = 2 * time.Minute

	// Gate 2's backoff. First retry fast — the common case is a listener that
	// binds a beat after its container is declared ready — then back off to
	// avoid hammering a process that is still opening files.
	sessionWakeBackoffFirst = 250 * time.Millisecond
	sessionWakeBackoffMax   = 2 * time.Second
)

// endReasonWakeFailed is the one end reason the whole wake half produces
// (§6). A gate-1 timeout, a gate-2 timeout and an operational wake failure are
// the same event for the person reading it — the target did not come up — and
// the message that travels beside it already distinguishes them. It is a member
// of the terminal_end_reason enum both session tables share (migration 00095),
// so the audit row and the last line the developer reads come from one value.
const endReasonWakeFailed tunnel.EndReason = "wake_failed"

// wakeControlFrame is the type of the progress frame both families' control
// wires carry (§6). Per ADR-064 §1 the identifier is added to each family's own
// vocabulary and never pooled; the value is the same string on both because
// they are the same event, not because they share a wire.
const wakeControlFrame = "waking"

// The two codes a `waking` frame carries. A client that models neither still
// works: it drops a frame type it does not know and the session degrades to
// "the first connection takes a while".
const (
	wakeFrameColdStart = "cold_start"
	wakeFrameReady     = "ready"
)

// wakeColdStartNotice is what the developer reads while the target comes up. It
// names scale-to-zero explicitly: they did nothing to stop the target and have
// no reason to suspect it, which is the whole complaint this decision answers.
const wakeColdStartNotice = "the target is asleep (scale-to-zero) — starting it, this can take up to 75 s"

// ---------------------------------------------------------------------------
// §8 — who is woken, and who is not
// ---------------------------------------------------------------------------

// sessionWakeKind is the target kind §8 states its rules per. Only two kinds
// have a scale-to-zero clock, and the same two are the only ones §1 stamps
// activity on: inventing a signal for the others would mean inventing its
// semantics too.
type sessionWakeKind uint8

const (
	// wakeKindNone is every target with no clock: a managed database (ADR-037
	// §2 excludes them by construction), a Compose *service* resource, a
	// declared external endpoint whose far side is not ours to start, and a
	// server shell, which has no container at all. The zero value, so a mint
	// that says nothing about scale-to-zero says exactly the right thing.
	wakeKindNone sessionWakeKind = iota
	wakeKindPreview
	wakeKindApplication
)

// sessionWakeSpec is what a mint resolved about its target's scale-to-zero
// state. Every field comes off rows the mint has already read — no extra query,
// and deliberately NO container inspect: whether the containers are up is the
// waker's question, and answering it here would be the second readiness rule
// §4 forbids. The control plane's own record of "down for scale-to-zero" is
// what decides, and a wake against a resource that turns out to be awake is a
// no-op the module answers at once (an empty Started).
type sessionWakeSpec struct {
	kind     sessionWakeKind
	serverID int64
	teamID   int64
	// uuid keys the wake set on the agent, and it is the container-naming uuid
	// rather than the row's owner: a preview instance names its containers after
	// the PREVIEW (INV-011), an application after its resource.
	uuid pgtype.UUID
	// previewID and applicationID name the row whose last_activity_at the mint
	// stamps (§1). Exactly one is set for a kind that has a clock.
	previewID     *int64
	applicationID *int64
	// armed is the scale-to-zero flag of THIS kind — preview_scale_to_zero for
	// a preview, scale_to_zero for an application.
	armed bool
	// asleep is the control plane's record of the resource being down for
	// scale-to-zero: `sleeping`/`waking` for a preview, scale_slept_at for an
	// application.
	asleep bool
	// desiredRunning is ADR-037 §3's gate, extended by this ADR to the wake:
	// the scheduler may not sleep a manually stopped application, and the
	// symmetric rule is that nothing auto-starts one either. It is read from
	// the APPLICATION's resource row in both cases — a preview has no desired
	// status of its own, and its parent being stopped is what "stopped" means
	// for it.
	desiredRunning bool
}

// previewWakeSpec is §8's preview row: woken by either door, under the
// session's own permission alone (§7). `waking` counts as asleep — a second
// mint against a wake already in flight queues behind the module's gate and
// gets the same verdict, rather than being told the target is up when it is not.
func previewWakeSpec(app store.GetApplicationByUUIDRow, preview store.Preview) sessionWakeSpec {
	previewID := preview.ID
	return sessionWakeSpec{
		kind:           wakeKindPreview,
		serverID:       app.ServerRowID,
		teamID:         app.Resource.TeamID,
		uuid:           preview.Uuid,
		previewID:      &previewID,
		armed:          app.Application.PreviewScaleToZero,
		asleep:         preview.Status == store.PreviewStatusSleeping || preview.Status == store.PreviewStatusWaking,
		desiredRunning: app.Resource.DesiredStatus == store.ResourceDesiredStatusRunning,
	}
}

// applicationWakeSpec is §8's application row: woken under the session's own
// permission AND applications:lifecycle (§7). A Compose *component* of a
// scale-to-zero application resolves to this same spec — the component is
// inside the wake set, so it wakes with the application and then passes gate 2
// on its own container.
func applicationWakeSpec(app store.GetApplicationByUUIDRow) sessionWakeSpec {
	applicationID := app.Resource.ID
	return sessionWakeSpec{
		kind:           wakeKindApplication,
		serverID:       app.ServerRowID,
		teamID:         app.Resource.TeamID,
		uuid:           app.Resource.Uuid,
		applicationID:  &applicationID,
		armed:          app.Application.ScaleToZero,
		asleep:         app.Application.ScaleSleptAt.Valid,
		desiredRunning: app.Resource.DesiredStatus == store.ResourceDesiredStatusRunning,
	}
}

// wakeVerdict is what §8's table answers for one target.
type wakeVerdict uint8

const (
	// wakeSkip is "not this session's business": no command, no refusal, and
	// the mint behaves exactly as it did before this ADR.
	wakeSkip wakeVerdict = iota
	// wakeAsk sends the command.
	wakeAsk
	// wakeRefuse is "never woken, and saying so beats minting a session that
	// will not work".
	wakeRefuse
)

// verdict applies §8 in the order the table reads.
func (s sessionWakeSpec) verdict() (wakeVerdict, string) {
	if s.kind == wakeKindNone {
		return wakeSkip, ""
	}
	if !s.armed {
		// Containers stopped with scale-to-zero off are stopped for some other
		// reason, and guessing which one is not a session's business. The
		// existing 409 at attach stands.
		return wakeSkip, ""
	}
	if !s.desiredRunning {
		// This rule is STRUCTURALLY the control plane's: the routing table the
		// agent holds carries a uuid, a container list and a dependency graph —
		// no desired state — so the agent could not enforce it even if asked.
		// Hence "no command sent", which is the accurate half of §8.
		return wakeRefuse, "this application is stopped — start it, then open the session"
	}
	if !s.asleep {
		return wakeSkip, ""
	}
	return wakeAsk, ""
}

// ---------------------------------------------------------------------------
// The wake in flight
// ---------------------------------------------------------------------------

// sessionWake is one wake asked for at a mint: a promise the session's attach
// awaits as gate 1, plus the record of which of §5's gates produced a verdict.
//
// It lives in this process only, exactly like TunnelPresence and the two attach
// registers beside it: an attach that lands on another replica finds no promise
// and proceeds straight to its target, which is the pre-ADR behaviour and not a
// regression — the wake itself is happening on the server either way.
type sessionWake struct {
	// resource is the uuid asked about, for the log.
	resource string
	done     chan struct{}
	// cancel releases the command's context. The mint calls it only when it
	// REFUSES: a session the developer abandons deliberately leaves the wake
	// running, because the resource re-sleeps at the end of its normal window
	// and cancelling would be a second policy for a cost that already has one.
	cancel context.CancelFunc

	started []string
	err     error

	// gateFailed records that one of §5's gates produced the session's failure,
	// which is what makes wake_failed its end reason. A session that failed
	// AFTER both gates passed — SSH refused, the container was removed a second
	// later — is not a wake failure and must not read as one.
	gateFailed atomic.Bool
}

// settle records gate 1's verdict exactly once.
func (k *sessionWake) settle(started []string, err error) {
	k.started, k.err = started, err
	close(k.done)
}

// ready reports whether the wake has already answered.
func (k *sessionWake) ready() bool {
	select {
	case <-k.done:
		return true
	default:
		return false
	}
}

// await is gate 1 as the session pays it (§5): the command's answer IS the
// verdict — every container of the wake set passed the waker's own readiness
// rule — so success is the absence of an error and nothing subtler.
//
// ok=false with an empty message is an abandonment: the session went away
// before the wake answered, and there is nobody left to tell.
func (k *sessionWake) await(ctx context.Context) (string, bool) {
	if k == nil {
		return "", true
	}
	select {
	case <-k.done:
	case <-ctx.Done():
		return "", false
	}
	if k.err == nil {
		return "", true
	}
	k.gateFailed.Store(true)
	return wakeFailureMessage(k.err), false
}

// failedGate reports that the session's failure came from §5's gates, so the
// caller ends it with wake_failed rather than target_unreachable.
func (k *sessionWake) failedGate() bool { return k != nil && k.gateFailed.Load() }

// sessionEndReason picks between the two verdicts a session that never reached
// its target can carry.
func sessionEndReason(wake *sessionWake) tunnel.EndReason {
	if wake.failedGate() {
		return endReasonWakeFailed
	}
	return endReasonTargetUnreachable
}

// terminalSessionEndReason is the same verdict in the terminal family's own Go
// type. The VALUE is identical on both paths — terminal_end_reason is one
// database enum shared by both session tables — and only the type differs,
// which is ADR-064 §1's rule about wire identifiers landing where it costs
// nothing: two constants, one meaning, no shared vocabulary invented for it.
func terminalSessionEndReason(wake *sessionWake) terminal.EndReason {
	return terminal.EndReason(sessionEndReason(wake))
}

// rememberWake publishes a wake under the session it was asked for, so the
// attach that follows can await it. Called once the row exists, which is the
// first moment there is a uuid to key on — and the first moment any client
// could possibly attach, since the uuid only reaches it in the mint response.
func (a *API) rememberWake(sessionUUID string, wake *sessionWake) {
	if wake == nil {
		return
	}
	a.wakeMu.Lock()
	if a.wakesLive == nil {
		a.wakesLive = map[string]*sessionWake{}
	}
	a.wakesLive[sessionUUID] = wake
	a.wakeMu.Unlock()
	time.AfterFunc(sessionWakeRetention, func() { a.forgetWake(sessionUUID, wake) })
}

func (a *API) forgetWake(sessionUUID string, wake *sessionWake) {
	a.wakeMu.Lock()
	defer a.wakeMu.Unlock()
	if a.wakesLive[sessionUUID] == wake {
		delete(a.wakesLive, sessionUUID)
	}
}

// lookupWake finds the wake a session was minted over, or nil — which every
// caller treats as "there was nothing to wait for".
func (a *API) lookupWake(sessionUUID string) *sessionWake {
	a.wakeMu.Lock()
	defer a.wakeMu.Unlock()
	return a.wakesLive[sessionUUID]
}

// ---------------------------------------------------------------------------
// §3 / §7 — the mint asks, and refuses rather than minting a doomed session
// ---------------------------------------------------------------------------

// sessionFamily is everything the wake needs to know about which door asked,
// which is the audit action and a noun for the log. The command itself carries
// none of it (§4).
type sessionFamily struct {
	wakeAction string
	noun       string
}

var (
	portForwardFamily = sessionFamily{wakeAction: "port-forward.wake", noun: "port-forward"}
	terminalFamily    = sessionFamily{wakeAction: "terminal.wake", noun: "terminal"}
)

// wakeForSession is §3's step at the mint. It answers before the session row is
// created, because every refusal it can produce must leave no row behind: a
// session minted against something that will never come up is worse than a
// clean refusal.
//
// ok=false means the response has already been written.
func (a *API) wakeForSession(w http.ResponseWriter, r *http.Request, id *auth.Identity, spec sessionWakeSpec, family sessionFamily) (*sessionWake, bool) {
	verdict, refusal := spec.verdict()
	switch verdict {
	case wakeSkip:
		return nil, true
	case wakeRefuse:
		a.auditWake(r, id, spec, family, store.AuditResultDenied, refusal, nil)
		httpapi.WriteError(w, r, http.StatusConflict, "resource_stopped", refusal)
		return nil, false
	}

	// §7: waking is a lifecycle act. `port-forwards:open` and `terminal:open`
	// each authorize opening their own kind of session; neither authorizes
	// starting production containers, and neither door may become a side route
	// around the permission that does. A preview costs nobody and the same
	// person can already wake it by loading its URL, so its own permission is
	// the whole gate there.
	if spec.kind == wakeKindApplication && !auth.Has(id.Permissions, auth.PermApplicationsLifecycle) {
		a.auditWake(r, id, spec, family, store.AuditResultDenied,
			"missing "+string(auth.PermApplicationsLifecycle), nil)
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden,
			"this application is asleep and waking it requires the "+
				string(auth.PermApplicationsLifecycle)+" permission")
		return nil, false
	}

	wake := a.startSessionWake(r, id, spec, family)
	// The mint waits for a refusal, never for the wake. Anything that has
	// already failed by the time the mint would answer is a refusal on the
	// merits — the agent is too old, the server has no wake set for this uuid,
	// the channel is down, or the waker failed outright — and refusing beats
	// handing back a session whose first stream is already doomed.
	select {
	case <-wake.done:
	case <-time.After(sessionWakeRefusalWindow):
		return wake, true
	}
	if wake.err == nil {
		// Already awake, answered at once: nothing is in flight, so the session
		// pays no gate at all.
		return nil, true
	}
	wake.cancel()
	httpapi.WriteError(w, r, http.StatusConflict, wakeRefusalCode(wake.err), wakeFailureMessage(wake.err))
	return nil, false
}

// startSessionWake sends the one typed command §4 reserves for this and hands
// back the promise the session will await.
//
// The command runs on a context DETACHED from the request — the mint answers
// 201 immediately, so tying the wake to the request would cancel it the instant
// it was asked for — and bounded by the wall clock the control plane owes the
// developer (sessionWakeGate1).
func (a *API) startSessionWake(r *http.Request, id *auth.Identity, spec sessionWakeSpec, family sessionFamily) *sessionWake {
	detached := context.WithoutCancel(r.Context())
	ctx, cancel := context.WithTimeout(detached, sessionWakeGate1)
	wake := &sessionWake{resource: uuidString(spec.uuid), done: make(chan struct{}), cancel: cancel}
	// The audit is written when the wake SETTLES, because §7 wants its result in
	// the trail and the request is long gone by then. A detached clone carries
	// the actor and the correlation ids that far: "X woke this application to
	// get into it" is the sentence the trail owes an operator, and a system row
	// would not carry it.
	audited := r.Clone(detached)
	go func() {
		defer cancel()
		started, err := a.sendWakeResource(ctx, spec)
		wake.settle(started, err)
		result := store.AuditResultSuccess
		detail := ""
		if err != nil {
			result, detail = store.AuditResultFailure, wakeFailureMessage(err)
			a.Logger.Warn(family.noun+" wake failed",
				"resource", wake.resource, "error", err)
		}
		a.auditWake(audited, id, spec, family, result, detail, started)
	}()
	return wake
}

// sendWakeResource is the whole of the control plane's part in §4: one typed
// command, and no ContainerStart loop of its own. The result body is gate 1's
// verdict; Started is informational (empty means the resource was already awake)
// and is never the success test.
func (a *API) sendWakeResource(ctx context.Context, spec sessionWakeSpec) ([]string, error) {
	if a.AgentRPC == nil {
		return nil, agentwire.Unavailable("not connected")
	}
	sender, ok := a.AgentRPC.Sender(spec.serverID)
	if !ok {
		return nil, agentwire.Unavailable("not connected")
	}
	raw, err := sender.Command(ctx, agentwire.MethodWakeResource,
		agentwire.WakeResourceParams{ResourceUUID: uuidString(spec.uuid)})
	if err != nil {
		return nil, err
	}
	var res agentwire.WakeResourceResult
	if len(raw) > 0 {
		// A result that will not decode is still a result, and a result at all
		// is the verdict: the wake is ready. Only the names of what it started
		// are lost, and they are for the log.
		_ = json.Unmarshal(raw, &res)
	}
	return res.Started, nil
}

// wakeRefusalCode types a wake that failed before the mint answered. The status
// is 409 for every one of them and is therefore not returned: the request was
// well-formed and authorized, the target is simply not in a state this session
// can use.
func wakeRefusalCode(err error) string {
	if dockerruntime.IsUnavailable(err) {
		// Genuinely transient — a helper restart, a relay reconnect — and worth
		// typing apart, because "try again shortly" is the right advice here and
		// wrong for every other code.
		return "agent_unavailable"
	}
	return string(endReasonWakeFailed)
}

// wakeFailureMessage phrases a failed wake for the developer, per typed code.
//
// The default branch is the load-bearing one: an `internal` answer IS the wake
// failing, and its message is the WAKER'S OWN text, which names the container
// the wake stalled on. Nothing here can reconstruct that, so nothing here tries
// — the message is passed through, minus the channel's own "agent: " prefix.
func wakeFailureMessage(err error) string {
	switch {
	case err == nil:
		return ""
	case cerrdefs.IsNotImplemented(err):
		return "the server's agent is too old to wake a sleeping target — " +
			"update the agent, or open the application's URL once to wake it"
	case dockerruntime.IsNotFound(err):
		return "the server has no wake set for this resource — deploy it again, " +
			"or open its URL once to wake it"
	case dockerruntime.IsUnavailable(err):
		return "the server's agent is not connected right now — it reconnects on its own; try again shortly"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// Our own ceiling ran out. The agent read the cancellation and rolled
		// back what it had started, so nothing is left half-up.
		return "the target did not finish starting within " +
			sessionWakeGate1.String() + " — the wake was rolled back; run the command again to retry"
	case cerrdefs.IsInvalidArgument(err):
		return "the wake was refused as malformed — this is a bug in AkerDock, please report it"
	default:
		return strings.TrimPrefix(err.Error(), "agent: ")
	}
}

// auditWake emits §7's event: against the RESOURCE, not against the session, so
// that someone reading an application's history sees "X woke this application to
// get into it" without having to join the session log to it. It sits beside the
// existing port-forward.open / terminal.open, never instead of them, and it is
// not emitted at all when nothing was woken.
func (a *API) auditWake(r *http.Request, id *auth.Identity, spec sessionWakeSpec, family sessionFamily, result store.AuditResult, detail string, started []string) {
	targetKind := "application"
	if spec.kind == wakeKindPreview {
		targetKind = "preview"
	}
	diff := map[string]any{"resource": uuidString(spec.uuid)}
	if detail != "" {
		diff["detail"] = detail
	}
	if len(started) > 0 {
		diff["started"] = started
	}
	a.Audit.Record(r, id, audit.Event{
		Action: family.wakeAction, TargetKind: targetKind, TargetUUID: spec.uuid,
		Result: result, Diff: diff,
	})
}

// stampSessionActivity closes §1's ordering hazard at the mint: a sleep decided
// in the window between the mint and the session's first beat, during which
// nothing has yet said the session exists. It runs on BOTH branches —
// necessarily on the wake path, where the resource has just been started and
// would otherwise be a candidate for the very next pass, and on the
// already-awake path too, where one write closes the same 20-second window for
// the price of one write.
func (a *API) stampSessionActivity(ctx context.Context, spec sessionWakeSpec, session string) {
	a.recordSessionActivity(ctx, spec.previewID, spec.applicationID, session)
}

// ---------------------------------------------------------------------------
// §5 — gate 2: the operation the session is about to perform
// ---------------------------------------------------------------------------

// retryUntilReady runs gate 2: not a synthetic probe but the exact thing the
// session exists to do, retried with backoff until it succeeds or the budget
// runs out. attempt returns an empty string on success and the operator
// sentence of its last failure otherwise.
//
// The budget is separate from gate 1's on purpose: they measure different
// things — a container becoming ready, and the thing inside it accepting work —
// and folding them into one number would silently redefine ADR-036's clause.
//
// It is a WALL CLOCK and deliberately NOT a derived context: on the terminal
// path the context that opens the exec is the one the shell then lives on, so a
// deadline pushed down into attempt would kill the very session it had just
// established, fifteen seconds in. An attempt that needs its own bound gives
// itself one.
func retryUntilReady(ctx context.Context, attempt func(context.Context) string) string {
	deadline := time.Now().Add(sessionWakeGate2)
	wait := sessionWakeBackoffFirst
	for {
		msg := attempt(ctx)
		if msg == "" {
			return ""
		}
		if time.Now().Add(wait).After(deadline) || ctx.Err() != nil {
			return msg
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return msg
		}
		timer.Stop()
		if wait *= 2; wait > sessionWakeBackoffMax {
			wait = sessionWakeBackoffMax
		}
	}
}

// announceWake tells the developer, on the wire the session already has, that
// the wait they are about to sit through is a cold start (§6) — and tells them
// again when gate 1 clears, so a client can stop widening its budgets once the
// target is up.
//
// Both frames are best effort: a frame that cannot be written is not a reason
// to fail a session that is otherwise fine, and a client that models no such
// frame drops it and degrades to "the first connection takes a while". It
// returns at once; the follow-up rides a goroutine bounded by the session's own
// context.
func announceWake(ctx context.Context, control *tunnel.LineControl, wake *sessionWake) {
	if wake == nil || wake.ready() {
		return
	}
	send := func(code, msg string) {
		_ = control.Send(ctx, tunnel.HTTPControlFrame{Type: wakeControlFrame, Code: code, Msg: msg})
	}
	send(wakeFrameColdStart, wakeColdStartNotice)
	go func() {
		select {
		case <-wake.done:
		case <-ctx.Done():
			return
		}
		if wake.err == nil {
			send(wakeFrameReady, "the target is ready")
		}
	}()
}

// wakeMintState is §6's first channel: the mint response states whether the
// target was already up or is being started for this session.
//
// It says what the mint ASKED FOR, at the instant it answered, and nothing
// about how the wake turns out — a `waking` session that then fails ends with
// `wake_failed`, and the two do not contradict each other. The refusal window
// is what keeps that honest in the other direction: a wake that has already
// failed by then is refused outright, so no session is ever announced as
// `waking` when its verdict is already in.
//
// Absent means ready, and that is the natural reading rather than a special
// case: an older manager sends no field, a session that woke nothing has a nil
// wake, and both produce the same nil pointer here. One nil, one meaning, no
// branch anywhere that has to know which of the two produced it.
//
// It is generic over the two generated enum types because the two families
// carry their own — ADR-064 §1's rule that identifiers are parameterised per
// access path and never pooled, applied where the contract already applies it.
func wakeMintState[T ~string](wake *sessionWake, waking T) *T {
	if wake == nil {
		return nil
	}
	return &waking
}

// wakeStreamBudget is what a data stream may spend waiting for a session whose
// target is still cold-starting (§3: the local accept() succeeds at once and
// the operation behind it is what waits). Without it the first connection would
// be refused by the ordinary dial budget while the platform was doing exactly
// what it promised.
func wakeStreamBudget(wake *sessionWake) time.Duration {
	if wake == nil {
		return tunnel.EgressDialTimeout
	}
	return sessionWakeCeiling + tunnel.EgressDialTimeout
}
