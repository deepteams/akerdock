package waker

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
	inspects   int32
	inspectErr error // when set, Inspect fails (e.g. unreachable Docker socket)
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{running: map[string]bool{}, health: map[string]string{}, starts: map[string]int{}}
}

func (d *fakeDocker) Start(_ context.Context, c string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starts[c]++
	d.running[c] = true
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

// newTestWaker wires a Waker to a backend httptest server and fast timings.
func newTestWaker(t *testing.T, d *fakeDocker, act Activity) (*Waker, *int32, func()) {
	t.Helper()
	var hits int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTeapot)
	}))
	u, _ := url.Parse(backend.URL)
	cfg := Config{
		Routes:    []Route{{Host: "app.example.com", ResourceUUID: "res-1", Container: u.Hostname(), Port: mustPort(t, u)}},
		Resources: []Resource{{UUID: "res-1", Containers: []string{"c1", "c2"}}},
	}
	w := New(cfg, d, act, nil)
	w.Poll = time.Millisecond
	w.StableFor = 0
	w.WakeTimeout = 200 * time.Millisecond
	return w, &hits, backend.Close
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
