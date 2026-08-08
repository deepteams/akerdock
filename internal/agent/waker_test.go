package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDocker drives the wake decision: containers report their state, and the
// number of Start calls is counted to prove single-flight and idempotence.
type fakeDocker struct {
	mu         sync.Mutex
	running    map[string]bool
	health     map[string]string // per-container health, default "none"
	starts     map[string]int
	seq        []string // Start call order, to prove ordered wake
	stops      []string // Stop call order, to prove rollback
	inspects   int32
	inspectErr error // when set, Inspect fails (e.g. unreachable Docker socket)
	startErr   error // when set, Start fails (daemon refusing the wake)
	stopErr    error // when set, Stop fails (rollback best-effort path)
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{running: map[string]bool{}, health: map[string]string{}, starts: map[string]int{}}
}

func (d *fakeDocker) Start(_ context.Context, c string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.startErr != nil {
		return d.startErr
	}
	d.starts[c]++
	d.seq = append(d.seq, c)
	d.running[c] = true
	return nil
}

func (d *fakeDocker) Stop(_ context.Context, c string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stops = append(d.stops, c)
	if d.stopErr != nil {
		return d.stopErr
	}
	d.running[c] = false
	return nil
}

func (d *fakeDocker) Inspect(_ context.Context, c string) (ContainerState, error) {
	atomic.AddInt32(&d.inspects, 1)
	if d.inspectErr != nil {
		return ContainerState{}, d.inspectErr
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	h := d.health[c]
	if h == "" {
		h = "none"
	}
	return ContainerState{Running: d.running[c], Health: h}, nil
}

type fakeActivity struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (a *fakeActivity) Record(uuid string, at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last == nil {
		a.last = map[string]time.Time{}
	}
	a.last[uuid] = at
	return nil
}

// newTestWakerRes wires a Waker with an explicit wake resource to a backend
// httptest server and fast timings.
func newTestWakerRes(t *testing.T, d *fakeDocker, act Activity, res Resource) (*Waker, *int32, func()) {
	t.Helper()
	var hits int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTeapot)
	}))
	u, _ := url.Parse(backend.URL)
	cfg := Config{
		Routes:    []Route{{Host: "app.example.com", ResourceUUID: res.UUID, Container: u.Hostname(), Port: mustPort(t, u)}},
		Resources: []Resource{res},
	}
	w := New(cfg, d, act, nil)
	w.Poll = time.Millisecond
	w.StableFor = 0
	w.WakeTimeout = 200 * time.Millisecond
	return w, &hits, backend.Close
}

// newTestWaker is newTestWakerRes with the default two-container flat resource.
func newTestWaker(t *testing.T, d *fakeDocker, act Activity) (*Waker, *int32, func()) {
	t.Helper()
	return newTestWakerRes(t, d, act, Resource{UUID: "res-1", Containers: []string{"c1", "c2"}})
}

func mustPort(t *testing.T, u *url.URL) int {
	t.Helper()
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return p
}

func request(host string, body string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", strings.NewReader(body))
	req.Host = host
	if body != "" {
		req.ContentLength = int64(len(body))
	}
	return req
}

func TestWakeFromSleeping(t *testing.T) {
	d := newFakeDocker() // both containers stopped
	act := &fakeActivity{}
	w, hits, closeFn := newTestWaker(t, d, act)
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))

	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want forwarded 418", rr.Code)
	}
	if *hits != 1 {
		t.Fatalf("backend hits = %d, want 1", *hits)
	}
	if d.starts["c1"] != 1 || d.starts["c2"] != 1 {
		t.Fatalf("starts = %v, want each container started once", d.starts)
	}
	if act.last["res-1"].IsZero() {
		t.Fatal("activity not recorded")
	}
}

func TestAlreadyRunningNoStart(t *testing.T) {
	d := newFakeDocker()
	d.running["c1"], d.running["c2"] = true, true
	w, hits, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))

	if *hits != 1 {
		t.Fatalf("backend hits = %d, want 1", *hits)
	}
	if len(d.starts) != 0 {
		t.Fatalf("starts = %v, want none (already running)", d.starts)
	}
}

func TestWakeTimeout(t *testing.T) {
	d := newFakeDocker()
	// A container that stays "starting" forever: Start marks it running, but
	// health never reaches healthy and StableFor is bypassed by unhealthy path.
	d.health["c1"] = "starting"
	d.health["c2"] = "starting"
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rr.Code)
	}
}

func TestWakeStartsInDeclaredOrder(t *testing.T) {
	// A dependency-free set starts within one pass, in the declared order —
	// like `docker compose up` starting independent services together.
	d := newFakeDocker()
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))

	if len(d.seq) != 2 || d.seq[0] != "c1" || d.seq[1] != "c2" {
		t.Fatalf("start sequence = %v, want [c1 c2] (declared order)", d.seq)
	}
}

func TestWakeGatesOnDependencyReadiness(t *testing.T) {
	// A declared dependency (c2 needs c1) stuck "starting" must HOLD c2's
	// start: it is never booted against a dependency that is not ready, and
	// the failed wake rolls the started dependency back to stopped — otherwise
	// the stack is left half-awake in a crash-loop the control plane believes
	// asleep.
	d := newFakeDocker()
	d.health["c1"] = "starting" // never ready
	w, _, closeFn := newTestWakerRes(t, d, &fakeActivity{}, Resource{
		UUID:       "res-1",
		Containers: []string{"c1", "c2"},
		WakeSet: []WakeContainer{
			{Container: "c1"},
			{Container: "c2", Needs: []string{"c1"}},
		},
	})
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rr.Code)
	}
	if d.starts["c2"] != 0 {
		t.Fatalf("c2 started while its dependency c1 was not ready: %v", d.starts)
	}
	if len(d.stops) != 1 || d.stops[0] != "c1" {
		t.Fatalf("rollback stops = %v, want [c1]", d.stops)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running["c1"] {
		t.Fatal("c1 left running after a failed wake — the resource must return to its slept state")
	}
}

func TestIndependentServiceNotHostageToSibling(t *testing.T) {
	// The worker case of 2026-07-27: `worker` does not depend on `frontend`,
	// so it must start as soon as ITS dependencies are met, even while
	// frontend is still failing its healthcheck — exactly like `docker
	// compose up`. The wake still fails on frontend and rolls BOTH back.
	d := newFakeDocker()
	d.health["frontend"] = "starting" // never ready
	w, _, closeFn := newTestWakerRes(t, d, &fakeActivity{}, Resource{
		UUID:       "res-1",
		Containers: []string{"nats", "frontend", "worker"},
		WakeSet: []WakeContainer{
			{Container: "nats"},
			{Container: "frontend", Needs: []string{"nats"}},
			{Container: "worker", Needs: []string{"nats"}},
		},
	})
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (frontend never ready)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "frontend") {
		t.Fatalf("failure must name the blocker: %q", rr.Body.String())
	}
	if d.starts["worker"] != 1 {
		t.Fatalf("worker must start behind nats without waiting for frontend: %v", d.starts)
	}
	// Rollback stops everything this wake started, in reverse start order.
	if len(d.stops) != 3 {
		t.Fatalf("rollback stops = %v, want the three wake-started containers", d.stops)
	}
}

func TestRollbackOnlyStopsWhatWakeStarted(t *testing.T) {
	// c1 was already awake before the wake: a failed wake must stop ONLY the
	// containers this attempt started (c2), never what was already running.
	d := newFakeDocker()
	d.running["c1"] = true
	d.health["c2"] = "starting" // never ready
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rr.Code)
	}
	if len(d.stops) != 1 || d.stops[0] != "c2" {
		t.Fatalf("rollback stops = %v, want [c2] only", d.stops)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.running["c1"] {
		t.Fatal("rollback stopped c1, which the wake did not start")
	}
}

func TestDockerErrorIs502NotTimeout(t *testing.T) {
	// An unreachable Docker socket (nonroot user) or a too-new API version makes
	// Inspect fail. That must surface as 502 "wake failed: …", never be masked as
	// a 504 timeout — the bug that made a healthy app look like it timed out.
	d := newFakeDocker()
	d.inspectErr = errors.New("permission denied on /var/run/docker.sock")
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for an operational error", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "wake failed") {
		t.Fatalf("body = %q, want the real cause surfaced", rr.Body.String())
	}
}

func TestHealthyReady(t *testing.T) {
	d := newFakeDocker()
	d.health["c1"] = "healthy"
	d.health["c2"] = "healthy"
	w, hits, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))
	if rr.Code != http.StatusTeapot || *hits != 1 {
		t.Fatalf("status=%d hits=%d, want forwarded once", rr.Code, *hits)
	}
}

func TestUptimeProbeAsleepAnswersDirectly(t *testing.T) {
	// ADR-037: an uptime probe on a SLEEPING app must be answered directly (200)
	// without waking it — otherwise every check cold-starts the whole stack.
	d := newFakeDocker() // sleeping
	act := &fakeActivity{}
	w, hits, closeFn := newTestWaker(t, d, act)
	defer closeFn()

	req := request("app.example.com", "")
	req.Header.Set(UptimeProbeHeader, "1")
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (available while asleep)", rr.Code)
	}
	if rr.Header().Get("X-AkerDock-Scale") != "asleep" {
		t.Fatalf("missing asleep marker: %v", rr.Header())
	}
	if len(d.starts) != 0 {
		t.Fatalf("uptime probe must NOT wake a sleeping app, starts=%v", d.starts)
	}
	if *hits != 0 {
		t.Fatalf("sleeping probe must not reach the backend, hits=%d", *hits)
	}
	if !act.last["res-1"].IsZero() {
		t.Fatal("uptime probe must not count as activity")
	}
}

func TestUptimeProbeAwakeForwardsNotActivity(t *testing.T) {
	// An uptime probe on an already-awake app IS forwarded (real health) but must
	// not count as activity.
	d := newFakeDocker()
	d.running["c1"], d.running["c2"] = true, true
	act := &fakeActivity{}
	w, hits, closeFn := newTestWaker(t, d, act)
	defer closeFn()

	req := request("app.example.com", "")
	req.Header.Set(UptimeProbeHeader, "1")
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, req)

	if rr.Code != http.StatusTeapot || *hits != 1 {
		t.Fatalf("status=%d hits=%d, want forwarded to the awake app", rr.Code, *hits)
	}
	if !act.last["res-1"].IsZero() {
		t.Fatal("uptime probe must not count as activity even when awake")
	}
}

func TestUnknownHost404(t *testing.T) {
	w, _, closeFn := newTestWaker(t, newFakeDocker(), &fakeActivity{})
	defer closeFn()
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("nope.example.com", ""))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestLargeBodyWhileSleeping503(t *testing.T) {
	d := newFakeDocker() // sleeping
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	req := httptest.NewRequest(http.MethodPost, "http://app.example.com/", strings.NewReader("x"))
	req.Host = "app.example.com"
	req.ContentLength = MaxHoldBody + 1

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
	if len(d.starts) != 0 {
		t.Fatal("must not wake for an oversized body")
	}
}

func TestLargeBodyWhileRunningForwards(t *testing.T) {
	d := newFakeDocker()
	d.running["c1"], d.running["c2"] = true, true // already awake
	w, hits, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	big := strings.Repeat("x", MaxHoldBody+1)
	req := httptest.NewRequest(http.MethodPost, "http://app.example.com/", strings.NewReader(big))
	req.Host = "app.example.com"
	req.ContentLength = int64(len(big))

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, req)
	// Awake target: a big body is fine, nothing is held.
	if rr.Code != http.StatusTeapot || *hits != 1 {
		t.Fatalf("status=%d hits=%d, want forwarded", rr.Code, *hits)
	}
}

func TestConcurrentWakeSingleStart(t *testing.T) {
	d := newFakeDocker()
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := httptest.NewRecorder()
			w.ServeHTTP(rr, request("app.example.com", ""))
		}()
	}
	wg.Wait()

	// Single-flight: the first wake starts the containers, the rest find them
	// running. Each container started exactly once.
	if d.starts["c1"] != 1 || d.starts["c2"] != 1 {
		t.Fatalf("starts = %v, want each started exactly once under concurrency", d.starts)
	}
}

// browserRequest is a page navigation as a browser would send it.
func browserRequest(host, path string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	req.Host = host
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	return req
}

func TestBrowserNavigationGetsWaitingPage(t *testing.T) {
	d := newFakeDocker() // sleeping
	act := &fakeActivity{}
	w, hits, closeFn := newTestWaker(t, d, act)
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, browserRequest("app.example.com", "/"))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 waiting page", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Waking up") || !strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Fatalf("body is not an auto-refreshing waiting page: %q", body)
	}
	if !strings.Contains(body, "c1") || !strings.Contains(body, "c2") {
		t.Fatalf("waiting page must list the stack's containers: %q", body)
	}
	if rr.Header().Get("X-AkerDock-Scale") != "waking" {
		t.Fatalf("missing waking marker: %v", rr.Header())
	}
	if *hits != 0 {
		t.Fatal("the navigation must not reach the backend while asleep")
	}
	// The wake runs in the background: the containers end up started and the
	// triggering navigation counts as activity.
	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		woken := d.starts["c1"] == 1 && d.starts["c2"] == 1
		d.mu.Unlock()
		act.mu.Lock()
		recorded := !act.last["res-1"].IsZero()
		act.mu.Unlock()
		if woken && recorded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background wake did not complete: starts=%v activity=%v", d.starts, act.last)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBrowserNavigationForwardsWhenAwake(t *testing.T) {
	d := newFakeDocker()
	d.running["c1"], d.running["c2"] = true, true
	w, hits, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, browserRequest("app.example.com", "/"))
	if rr.Code != http.StatusTeapot || *hits != 1 {
		t.Fatalf("status=%d hits=%d, want the awake app to answer, not the waiting page", rr.Code, *hits)
	}
}

func TestWaitingPageFailureThenRetry(t *testing.T) {
	d := newFakeDocker()
	d.health["c1"] = "starting" // never ready → the background wake fails
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, browserRequest("app.example.com", "/"))
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "Waking up") {
		t.Fatalf("first navigation: status=%d body=%q, want the waiting page", rr.Code, rr.Body.String())
	}

	// Poll until the page reports the failure (the wake times out at 200ms).
	deadline := time.Now().Add(2 * time.Second)
	var body string
	for {
		rr = httptest.NewRecorder()
		w.ServeHTTP(rr, browserRequest("app.example.com", "/"))
		body = rr.Body.String()
		if strings.Contains(body, "could not wake up") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failure never reported, last body: %q", body)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Fatal("the failure page must stop auto-refreshing")
	}
	if !strings.Contains(body, retryParam) {
		t.Fatal("the failure page must offer a retry link")
	}
	if !strings.Contains(body, "waiting for c1") {
		t.Fatalf("the failure must name the blocking container: %q", body)
	}
	// The rollback stopped c1, and a plain reload must NOT retrigger a wake.
	d.mu.Lock()
	startsAfterFailure := d.starts["c1"]
	d.mu.Unlock()
	rr = httptest.NewRecorder()
	w.ServeHTTP(rr, browserRequest("app.example.com", "/"))
	d.mu.Lock()
	startsAfterReload := d.starts["c1"]
	d.mu.Unlock()
	if startsAfterReload != startsAfterFailure {
		t.Fatalf("a reload of the failure page retriggered a wake: %d -> %d", startsAfterFailure, startsAfterReload)
	}

	// The retry link starts a fresh attempt.
	rr = httptest.NewRecorder()
	w.ServeHTTP(rr, browserRequest("app.example.com", "/?"+retryParam+"=1"))
	if !strings.Contains(rr.Body.String(), "Waking up") {
		t.Fatalf("retry must render the waiting page again: %q", rr.Body.String())
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		retried := d.starts["c1"] > startsAfterFailure
		d.mu.Unlock()
		if retried {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry did not start a new wake attempt")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAwakeButUnhealthyForwardsImmediately(t *testing.T) {
	// Regression for the production hang of 2026-07-27: every container of an
	// AWAKE stack was running but one healthcheck was failing; the waker held
	// each request 60 s on the single-flight gate waiting for readiness, and
	// the queue never drained — infinite loading with no response at all. A
	// container the wake did not start is not its to gate: forward, and let
	// the app answer for its own health.
	d := newFakeDocker()
	d.running["c1"], d.running["c2"] = true, true
	d.health["c2"] = "unhealthy"
	w, hits, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	start := time.Now()
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))

	if rr.Code != http.StatusTeapot || *hits != 1 {
		t.Fatalf("status=%d hits=%d, want forwarded to the running app", rr.Code, *hits)
	}
	if elapsed := time.Since(start); elapsed > w.WakeTimeout/2 {
		t.Fatalf("request held %s on a running stack — must forward immediately", elapsed)
	}
	if len(d.starts) != 0 || len(d.stops) != 0 {
		t.Fatalf("no start/stop expected on a running stack: starts=%v stops=%v", d.starts, d.stops)
	}
}

func TestWaitingPageStaysWhileWakeInProgress(t *testing.T) {
	// The graph wake starts every container within seconds, so everything is
	// Running long before it is ready. A navigation arriving mid-wake must
	// keep getting the waiting page — immediately, without queuing on the
	// wake gate — so the user sees the starting→ready progression instead of
	// a tab stuck loading.
	d := newFakeDocker()
	d.health["c1"] = "starting" // wake stays in flight until its budget
	w, hits, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, browserRequest("app.example.com", "/"))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("first navigation: status = %d, want the waiting page", rr.Code)
	}

	// Wait until the background wake has started both containers: they are
	// all Running, yet the wake is still in flight (c1 never ready).
	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		allRunning := d.running["c1"] && d.running["c2"]
		d.mu.Unlock()
		if allRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background wake never started the containers")
		}
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	rr = httptest.NewRecorder()
	w.ServeHTTP(rr, browserRequest("app.example.com", "/"))
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "Waking up") {
		t.Fatalf("mid-wake navigation: status=%d body=%q, want the waiting page", rr.Code, rr.Body.String())
	}
	if elapsed := time.Since(start); elapsed > w.WakeTimeout/2 {
		t.Fatalf("mid-wake navigation held %s — must answer immediately, never queue on the gate", elapsed)
	}
	if *hits != 0 {
		t.Fatal("nothing must reach the backend while the wake is in flight")
	}
}

func TestPostWithHTMLAcceptKeepsHoldAndForward(t *testing.T) {
	// A form submission must never be answered with a waiting page its body
	// cannot survive: non-GET requests keep the hold-and-forward path.
	d := newFakeDocker() // sleeping, wakes instantly
	w, hits, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()

	req := httptest.NewRequest(http.MethodPost, "http://app.example.com/submit", strings.NewReader("a=1"))
	req.Host = "app.example.com"
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, req)

	if rr.Code != http.StatusTeapot || *hits != 1 {
		t.Fatalf("status=%d hits=%d, want the POST held and forwarded", rr.Code, *hits)
	}
}

func TestRetryParamStrippedBeforeProxy(t *testing.T) {
	// The retry flag is the waiting page's own; the application never sees it.
	var gotQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusTeapot)
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)

	d := newFakeDocker()
	d.running["c1"], d.running["c2"] = true, true
	cfg := Config{
		Routes:    []Route{{Host: "app.example.com", ResourceUUID: "res-1", Container: u.Hostname(), Port: mustPort(t, u)}},
		Resources: []Resource{{UUID: "res-1", Containers: []string{"c1", "c2"}}},
	}
	w := New(cfg, d, &fakeActivity{}, nil)
	w.Poll, w.StableFor, w.WakeTimeout = time.Millisecond, 0, 200*time.Millisecond

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, browserRequest("app.example.com", "/?"+retryParam+"=1&keep=yes"))

	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want forwarded", rr.Code)
	}
	if strings.Contains(gotQuery, retryParam) || !strings.Contains(gotQuery, "keep=yes") {
		t.Fatalf("proxied query = %q, want retry flag stripped and the rest kept", gotQuery)
	}
}

func TestActivityRoundTrip(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	got, err := ParseActivity("  1700000000\n")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(at) {
		t.Fatalf("parsed %v, want %v", got, at)
	}
	if _, err := ParseActivity("garbage"); err == nil {
		t.Fatal("expected error on garbage activity value")
	}
}
