package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
)

// wakeFrames runs one WakeResource command through the executor and returns
// everything it sent back. It asserts nothing, so the concurrency tests can run
// it on their own goroutine and keep every assertion on the test's.
func wakeFrames(e *Executor, id int64, uuid string) []agentwire.Frame {
	sink := &frameSink{}
	params, _ := json.Marshal(agentwire.WakeResourceParams{ResourceUUID: uuid})
	e.Execute(context.Background(), agentwire.Command{
		ID: id, Method: agentwire.MethodWakeResource, Params: params,
	}, sink.send)
	return sink.all()
}

// onlyResult is the shape the control plane reads: a unary command answers with
// exactly one result frame and never a stream.
func onlyResult(t *testing.T, frames []agentwire.Frame) *agentwire.Result {
	t.Helper()
	if len(frames) != 1 || frames[0].Type != agentwire.FrameResult || frames[0].Res == nil {
		t.Fatalf("frames = %+v, want exactly one result", frames)
	}
	return frames[0].Res
}

func wakeCommand(t *testing.T, e *Executor, id int64, uuid string) *agentwire.Result {
	t.Helper()
	return onlyResult(t, wakeFrames(e, id, uuid))
}

// wakeExecutor is an executor whose only vocabulary under test is the wake: it
// talks to no daemon of its own, since WakeResource goes through the waker
// module and never through the Runtime.
func wakeExecutor(w *Waker) *Executor {
	e := NewExecutor(&fake.Runtime{}, nil, nil)
	e.Waker = func() *Waker { return w }
	return e
}

func wakeStarted(t *testing.T, res *agentwire.Result) []string {
	t.Helper()
	if res.Err != nil {
		t.Fatalf("wake failed: %+v", res.Err)
	}
	var out agentwire.WakeResourceResult
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatalf("result body %s: %v", res.Body, err)
	}
	return out.Started
}

// TestWakeCommandWakesTheWholeWakeSet is ADR-067 §4's first promise: one
// command wakes the resource's whole set along its depends_on graph, not the
// single container the session happens to target — a tunnel into a compose
// stack's database needs the stack, and the control plane never sees the graph.
func TestWakeCommandWakesTheWholeWakeSet(t *testing.T) {
	d := newFakeDocker()
	res := Resource{
		UUID:       "res-1",
		Containers: []string{"db", "web"},
		WakeSet: []WakeContainer{
			{Container: "db"},
			{Container: "web", Needs: []string{"db"}},
		},
	}
	w, _, closeFn := newTestWakerRes(t, d, &fakeActivity{}, res)
	defer closeFn()

	started := wakeStarted(t, wakeCommand(t, wakeExecutor(w), 1, "res-1"))

	if len(started) != 2 || started[0] != "db" || started[1] != "web" {
		t.Fatalf("started = %v, want the whole set in dependency order", started)
	}
	if d.starts["db"] != 1 || d.starts["web"] != 1 {
		t.Fatalf("starts = %v, want each container started once", d.starts)
	}
	if len(d.seq) != 2 || d.seq[0] != "db" {
		t.Fatalf("start order = %v, want the dependency first", d.seq)
	}
}

// TestWakeCommandOnAnAwakeResourceStartsNothing pins the answer the mint gets
// when it raced someone else to the wake: ready, with an empty Started, which
// is how the control plane tells "I woke this" from "it was already up" without
// a second round trip.
func TestWakeCommandOnAnAwakeResourceStartsNothing(t *testing.T) {
	d := newFakeDocker()
	d.running["c1"], d.running["c2"] = true, true
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	if started := wakeStarted(t, wakeCommand(t, wakeExecutor(w), 1, "res-1")); len(started) != 0 {
		t.Fatalf("started = %v, want nothing on an awake resource", started)
	}
	if len(d.starts) != 0 {
		t.Fatalf("starts = %v, want none", d.starts)
	}
}

// gatedDocker blocks the very first Inspect until release is closed, so a test
// can hold one wake inside the single-flight gate and let a second caller try
// to overtake it.
type gatedDocker struct {
	*fakeDocker
	first   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func newGatedDocker() *gatedDocker {
	return &gatedDocker{
		fakeDocker: newFakeDocker(),
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
}

func (d *gatedDocker) Inspect(ctx context.Context, c string) (ContainerState, error) {
	if d.first.CompareAndSwap(false, true) {
		close(d.entered)
		<-d.release
	}
	return d.fakeDocker.Inspect(ctx, c)
}

// TestWakeCommandSharesTheSingleFlightGateWithTheHTTPPath is the load-bearing
// assertion of ADR-067 §4, and the reason the wake is a command to the waker
// module rather than a ContainerStart loop on the control plane. A session mint
// and a browser hit arriving together must join ONE wake: two starters for one
// resource with no shared gate would each start their own half of a compose
// stack, in their own order.
//
// The proof is the window: the command holds the gate, blocked before it has
// started anything, while the HTTP request is admitted. A second starter would
// have started both containers in that window; the shared gate leaves the count
// at zero, and at exactly one per container once both have run.
func TestWakeCommandSharesTheSingleFlightGateWithTheHTTPPath(t *testing.T) {
	d := newGatedDocker()
	w, hits, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	cmdDone := make(chan []agentwire.Frame, 1)
	go func() { cmdDone <- wakeFrames(wakeExecutor(w), 1, "res-1") }()
	<-d.entered // the command holds the gate, before any start

	httpDone := make(chan int, 1)
	go func() {
		rr := httptest.NewRecorder()
		w.ServeHTTP(rr, request("app.example.com", ""))
		httpDone <- rr.Code
	}()
	// Long enough for an unguarded HTTP wake to have started the stack.
	time.Sleep(20 * time.Millisecond)
	d.mu.Lock()
	overtook := len(d.starts)
	d.mu.Unlock()
	if overtook != 0 {
		t.Fatalf("the HTTP wake started %d containers while the command held the gate — the two paths do not share it", overtook)
	}

	close(d.release)
	started := wakeStarted(t, onlyResult(t, <-cmdDone))
	code := <-httpDone

	if len(started) != 2 {
		t.Fatalf("command started = %v, want the whole set", started)
	}
	if d.starts["c1"] != 1 || d.starts["c2"] != 1 {
		t.Fatalf("starts = %v, want each container started exactly once across both paths", d.starts)
	}
	if code != http.StatusTeapot || *hits != 1 {
		t.Fatalf("http status = %d, backend hits = %d, want the request forwarded once", code, *hits)
	}
}

// TestWakeCommandUnknownResourceIsNotFound separates a stale routing deposit
// from a cold start that failed: not-found tells the mint this server cannot
// wake that resource at all, which is a refusal, not a wake_failed the
// developer should read as "your container did not come up".
func TestWakeCommandUnknownResourceIsNotFound(t *testing.T) {
	d := newFakeDocker()
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	res := wakeCommand(t, wakeExecutor(w), 1, "res-unknown")

	if res.Err == nil || res.Err.Code != agentwire.CodeNotFound {
		t.Fatalf("error = %+v, want not_found", res.Err)
	}
	if len(d.starts) != 0 {
		t.Fatalf("starts = %v, want none for a resource this waker does not know", d.starts)
	}
}

// TestWakeCommandWithoutAWakerIsUnavailable covers the helper that runs no
// waker, and the one whose routing table has not landed yet: nothing was
// started, so the answer must be the transient code, never a wake failure.
func TestWakeCommandWithoutAWakerIsUnavailable(t *testing.T) {
	for name, e := range map[string]*Executor{
		"no waker at all":  NewExecutor(&fake.Runtime{}, nil, nil),
		"no routing table": wakeExecutor(nil),
	} {
		t.Run(name, func(t *testing.T) {
			res := wakeCommand(t, e, 1, "res-1")
			if res.Err == nil || res.Err.Code != agentwire.CodeUnavailable {
				t.Fatalf("error = %+v, want unavailable", res.Err)
			}
		})
	}
}

// TestWakeCommandBudgetExpiryNamesTheStalledContainer is what §6 puts on the
// wire: a wake that runs out of budget answers with the waker's OWN message, so
// the wake_failed frame the developer reads names the container that stalled
// rather than a generic timeout.
func TestWakeCommandBudgetExpiryNamesTheStalledContainer(t *testing.T) {
	d := newFakeDocker()
	d.health["c1"], d.health["c2"] = "starting", "starting"
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	res := wakeCommand(t, wakeExecutor(w), 1, "res-1")

	if res.Err == nil || res.Err.Code != agentwire.CodeInternal {
		t.Fatalf("error = %+v, want an internal wake failure", res.Err)
	}
	if !strings.Contains(res.Err.Message, "c1") || !strings.Contains(res.Err.Message, "wake timed out") {
		t.Fatalf("message = %q, want the waker's own text naming the stalled container", res.Err.Message)
	}
	// The failed wake rolled back what it started: the resource goes back to
	// sleep instead of crash-looping half-awake behind a session that failed.
	if len(d.stops) == 0 {
		t.Fatal("no rollback after a failed wake")
	}
}

// TestWakeCommandIsCancellable is §5's clause on gate 1: the developer who
// abandons the session (Ctrl-C, a closed mint) must not leave the agent waiting
// out a 60 s budget. The channel's cancel frame reaches the wake through the
// command's context, and the rollback runs as it does for any failed wake.
func TestWakeCommandIsCancellable(t *testing.T) {
	d := newFakeDocker()
	d.health["c1"], d.health["c2"] = "starting", "starting" // never becomes ready
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()
	w.WakeTimeout = time.Minute // the budget must not be what ends this wake

	e := wakeExecutor(w)
	done := make(chan []agentwire.Frame, 1)
	go func() { done <- wakeFrames(e, 7, "res-1") }()

	// Cancel once the wake is demonstrably under way.
	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		running := len(d.starts) > 0
		d.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the wake never started a container")
		}
		time.Sleep(time.Millisecond)
	}
	e.Cancel(7)

	select {
	case frames := <-done:
		res := onlyResult(t, frames)
		if res.Err == nil || res.Err.Code != agentwire.CodeCanceled {
			t.Fatalf("error = %+v, want canceled", res.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel frame did not end the wake")
	}
}

// TestWakeCommandRejectsAnEmptyResourceUUID: a wake with nothing to wake is a
// malformed command, not a resource that happens to be missing — invalid says
// "this build cannot read what you sent", and retrying will not change it.
func TestWakeCommandRejectsAnEmptyResourceUUID(t *testing.T) {
	d := newFakeDocker()
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	res := wakeCommand(t, wakeExecutor(w), 1, "")

	if res.Err == nil || res.Err.Code != agentwire.CodeInvalid {
		t.Fatalf("error = %+v, want invalid", res.Err)
	}
}

// TestWakeCommandFollowsTheReloadedWaker pins why the executor holds a getter
// and not a pointer: the control plane deposits a new routing table whenever a
// resource is added or dropped, and serve.go answers by REBUILDING the waker.
// A pointer captured at enrollment would keep waking against a table the HTTP
// path stopped serving — and would gate against nobody.
func TestWakeCommandFollowsTheReloadedWaker(t *testing.T) {
	d := newFakeDocker()
	first, _, closeFirst := newTestWakerRes(t, d, &fakeActivity{}, Resource{UUID: "res-old", Containers: []string{"c1"}})
	defer closeFirst()
	second, _, closeSecond := newTestWakerRes(t, d, &fakeActivity{}, Resource{UUID: "res-new", Containers: []string{"c2"}})
	defer closeSecond()

	var current atomic.Pointer[Waker]
	current.Store(first)
	e := NewExecutor(&fake.Runtime{}, nil, nil)
	e.Waker = current.Load

	if started := wakeStarted(t, wakeCommand(t, e, 1, "res-old")); len(started) != 1 {
		t.Fatalf("started = %v, want the resource of the loaded table", started)
	}

	current.Store(second) // a routing reload

	if res := wakeCommand(t, e, 2, "res-old"); res.Err == nil || res.Err.Code != agentwire.CodeNotFound {
		t.Fatalf("stale resource = %+v, want not_found after the reload", res.Err)
	}
	if started := wakeStarted(t, wakeCommand(t, e, 3, "res-new")); len(started) != 1 {
		t.Fatalf("started = %v, want the reloaded table's resource", started)
	}
}

// TestWakeCommandIsDispatchedWhileUnknownMethodsAreNot pins the compatibility
// signal ADR-067 rests on: an agent that predates the decision has no arm for
// this method and falls through to `unimplemented`, which the mint reads as
// "cannot wake" and answers with a refusal instead of a session that would die
// at its first stream. Deleting the dispatch arm would silently reinstate that
// refusal on every server.
func TestWakeCommandIsDispatchedWhileUnknownMethodsAreNot(t *testing.T) {
	d := newFakeDocker()
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()
	e := wakeExecutor(w)

	if res := wakeCommand(t, e, 1, "res-1"); res.Err != nil {
		t.Fatalf("WakeResource answered %+v — this build must know the method", res.Err)
	}

	sink := &frameSink{}
	e.Execute(context.Background(), agentwire.Command{ID: 2, Method: "WakeResourceV2"}, sink.send)
	res := sink.all()[0].Res
	if res.Err == nil || res.Err.Code != agentwire.CodeUnimplemented {
		t.Fatalf("unknown wake method = %+v, want unimplemented", res.Err)
	}
}

// TestWakeCommandReportsWokenResourcesAsObservations: the wake command must
// keep the ADR-040 push the HTTP path produces, so the control plane flips the
// resource out of `sleeping` without waiting for its next SSH scan. A session
// that woke a preview leaves exactly the same trace a browser hit would.
func TestWakeCommandReportsWokenResourcesAsObservations(t *testing.T) {
	d := newFakeDocker()
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()
	var mu sync.Mutex
	var woken []string
	w.OnWake = func(uuid string) {
		mu.Lock()
		defer mu.Unlock()
		woken = append(woken, uuid)
	}

	wakeStarted(t, wakeCommand(t, wakeExecutor(w), 1, "res-1"))
	// Second wake: nothing to start, so nothing to report either.
	wakeStarted(t, wakeCommand(t, wakeExecutor(w), 2, "res-1"))

	mu.Lock()
	defer mu.Unlock()
	if len(woken) != 1 || woken[0] != "res-1" {
		t.Fatalf("observations = %v, want exactly one for the wake that started containers", woken)
	}
}
