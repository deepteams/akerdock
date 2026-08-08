package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestWakerLogDefaultsToSlog pins the logger fallback.
func TestWakerLogDefaultsToSlog(t *testing.T) {
	w := New(Config{}, newFakeDocker(), nil, nil)
	if w.log() != slog.Default() {
		t.Fatal("a nil Logger must fall back to slog.Default")
	}
	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	w.Logger = custom
	if w.log() != custom {
		t.Fatal("an explicit Logger must be used")
	}
}

// TestWakeUnknownResourceIs502 pins the config-mismatch path: a route naming
// a resource absent from the table is an operational failure, reported as
// such — never a timeout.
func TestWakeUnknownResourceIs502(t *testing.T) {
	cfg := Config{
		Routes: []Route{{Host: "app.example.com", ResourceUUID: "ghost", Container: "c1", Port: 3000}},
	}
	w := New(cfg, newFakeDocker(), &fakeActivity{}, nil)
	w.Poll, w.StableFor, w.WakeTimeout = time.Millisecond, 0, 50*time.Millisecond

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unknown resource") {
		t.Fatalf("body = %q, want the unknown resource named", rr.Body.String())
	}
}

// TestGateForCreatesMissingGates pins the lazily-created gate: a resource the
// config did not pre-register still serializes on one mutex.
func TestGateForCreatesMissingGates(t *testing.T) {
	w := New(Config{}, newFakeDocker(), nil, nil)
	g := w.gateFor("late")
	if g == nil {
		t.Fatal("gateFor must create a gate on demand")
	}
	if w.gateFor("late") != g {
		t.Fatal("gateFor must return the same gate for the same resource")
	}
}

// TestIsRunningUnknownResource pins the fast path's guard.
func TestIsRunningUnknownResource(t *testing.T) {
	w := New(Config{}, newFakeDocker(), nil, nil)
	if w.isRunning(context.Background(), "nope") {
		t.Fatal("an unknown resource is never running")
	}
}

// TestWakeDisplayStates pins the waiting page's snapshot: every observable
// state renders, the resource prefix is trimmed, a bare container name shows
// as "app", and an inspect failure degrades to waiting.
func TestWakeDisplayStates(t *testing.T) {
	d := newFakeDocker()
	d.running["res-1-db"] = true
	d.health["res-1-db"] = "unhealthy"
	d.running["res-1-api"] = true
	d.health["res-1-api"] = "healthy"
	d.running["res-1-worker"] = true
	d.health["res-1-worker"] = "starting"
	d.running["other"] = true // no healthcheck: running is all we can observe
	cfg := Config{Resources: []Resource{{
		UUID:       "res-1",
		Containers: []string{"res-1", "res-1-db", "res-1-api", "res-1-worker", "other"},
	}}}
	w := New(cfg, d, nil, nil)

	got := w.wakeDisplayStates(context.Background(), "res-1")
	want := []containerDisplayState{
		{Name: "app", State: "waiting"}, // stopped, name equal to the uuid
		{Name: "db", State: "unhealthy"},
		{Name: "api", State: "ready"},
		{Name: "worker", State: "starting"},
		{Name: "other", State: "running"},
	}
	if len(got) != len(want) {
		t.Fatalf("states = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("state[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	if states := w.wakeDisplayStates(context.Background(), "unknown"); states != nil {
		t.Fatalf("unknown resource states = %+v, want nil", states)
	}

	d.inspectErr = errors.New("socket gone")
	for _, st := range w.wakeDisplayStates(context.Background(), "res-1") {
		if st.State != "waiting" {
			t.Fatalf("state with a failing inspect = %+v, want waiting", st)
		}
	}
}

// TestWaitingPageHeadHasNoBody pins the HEAD arm: the browser preflight gets
// the status and headers, never the page.
func TestWaitingPageHeadHasNoBody(t *testing.T) {
	d := newFakeDocker() // sleeping
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	req := httptest.NewRequest(http.MethodHead, "http://app.example.com/", nil)
	req.Host = "app.example.com"
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q, want empty", rr.Body.String())
	}
}

// TestWakeStartFailureIs502 pins the docker-start error path: the daemon
// refusing the start is an operational failure with its cause surfaced.
func TestWakeStartFailureIs502(t *testing.T) {
	d := newFakeDocker()
	d.startErr = errors.New("cannot start: disk full")
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "disk full") {
		t.Fatalf("body = %q, want the daemon's cause", rr.Body.String())
	}
}

// TestRollbackSurvivesAStopFailure pins the best-effort rollback: a stop that
// fails is logged, the wake still reports its own failure.
func TestRollbackSurvivesAStopFailure(t *testing.T) {
	d := newFakeDocker()
	d.health["c1"] = "starting" // never ready → the wake times out
	d.stopErr = errors.New("stop failed")
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	w.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want the 504 despite the failed rollback", rr.Code)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.stops) == 0 {
		t.Fatal("the rollback must still attempt every stop")
	}
}

// TestSleepCtxHonorsCancellation pins the context arm.
func TestSleepCtxHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepCtx = %v, want context.Canceled without waiting", err)
	}
	if err := sleepCtx(context.Background(), time.Nanosecond); err != nil {
		t.Fatalf("sleepCtx = %v, want a completed sleep", err)
	}
}
