package agent

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// newPendingWaker builds a waker whose only knowledge of the host is a pending
// preview entry — the state of a preview between the PR opening and its first
// deployment (ADR-073 §1).
func newPendingWaker(pending ...PendingRoute) *Waker {
	return New(Config{Pending: pending}, newFakeDocker(), &fakeActivity{}, nil)
}

func getPending(t *testing.T, w *Waker, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, req)
	return rr
}

// A reserved preview answers with its state instead of the 404 the proxy used
// to return, and says so in the words the dashboard uses.
func TestPendingPagePerState(t *testing.T) {
	cases := []struct {
		state     string
		wants     string
		refreshes bool
	}{
		{PreviewStateQueued, "queued", true},
		{PreviewStateDeploying, "deploying", true},
		{PreviewStateFailed, "failed", false},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			w := newPendingWaker(PendingRoute{
				Host: "pr-7.preview.example.com", ResourceUUID: "prev-1", State: tc.state, PRNumber: 7,
			})
			rr := getPending(t, w, "pr-7.preview.example.com")

			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rr.Code)
			}
			if got := rr.Header().Get(PreviewStateHeader); got != tc.state {
				t.Fatalf("%s = %q, want %q", PreviewStateHeader, got, tc.state)
			}
			if got := rr.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Fatalf("Content-Type = %q, want text/html", ct)
			}

			body := rr.Body.String()
			if !strings.Contains(body, tc.wants) {
				t.Fatalf("body does not say %q: %s", tc.wants, body)
			}
			if !strings.Contains(body, "pull request #7") {
				t.Fatalf("body omits the PR number: %s", body)
			}

			refreshTag := "<meta http-equiv=\"refresh\" content=\"" + strconv.Itoa(pendingRefreshSeconds) + "\">"
			if got := strings.Contains(body, refreshTag); got != tc.refreshes {
				t.Fatalf("auto-refresh = %v, want %v (state %s)", got, tc.refreshes, tc.state)
			}
			if got := rr.Header().Get("Retry-After") != ""; got != tc.refreshes {
				t.Fatalf("Retry-After present = %v, want %v (state %s)", got, tc.refreshes, tc.state)
			}
		})
	}
}

// The page is safe behind an open wall (ADR-073 §4): the state and the PR
// number, nothing else — no repository, no branch, no uuid, no error text.
func TestPendingPageLeaksNothingBeyondState(t *testing.T) {
	w := newPendingWaker(PendingRoute{
		Host: "pr-482.preview.example.com", ResourceUUID: "9f1c-secret-uuid", State: PreviewStateFailed, PRNumber: 482,
	})
	body := getPending(t, w, "pr-482.preview.example.com").Body.String()

	if strings.Contains(body, "9f1c-secret-uuid") {
		t.Fatalf("body exposes the resource uuid: %s", body)
	}
	if !strings.Contains(body, "pull request #482") {
		t.Fatalf("body omits the PR number: %s", body)
	}
}

// Without a PR number the page still renders, and invents none.
func TestPendingPageWithoutPRNumber(t *testing.T) {
	w := newPendingWaker(PendingRoute{Host: "prev.example.com", ResourceUUID: "prev-2", State: PreviewStateQueued})
	body := getPending(t, w, "prev.example.com").Body.String()

	if strings.Contains(body, "pull request #") {
		t.Fatalf("body names a pull request it was not given: %s", body)
	}
	if !strings.Contains(body, "queued") {
		t.Fatalf("body does not say queued: %s", body)
	}
}

// The host is escaped in the title like every other agent page.
func TestPendingPageEscapesHost(t *testing.T) {
	w := newPendingWaker(PendingRoute{Host: "evil<script>.example.com", State: PreviewStateDeploying})
	body := renderPendingPage("evil<script>.example.com", PendingRoute{}, PreviewStateDeploying)

	if strings.Contains(body, "<script>") {
		t.Fatalf("host not escaped: %s", body)
	}
	if !strings.Contains(body, "evil&lt;script&gt;.example.com") {
		t.Fatalf("escaped host missing: %s", body)
	}
	if _, ok := w.pendingRoute("evil<script>.example.com"); !ok {
		t.Fatalf("pending host not registered")
	}
}

// A state this agent does not know (an older or newer control plane) is served
// as queued: not serving yet, and still refreshing.
func TestPendingUnknownStateFallsBackToQueued(t *testing.T) {
	for _, state := range []string{"", "  DEPLOYING ", "wat"} {
		w := newPendingWaker(PendingRoute{Host: "prev.example.com", State: state})
		rr := getPending(t, w, "prev.example.com")
		want := PreviewStateQueued
		if strings.TrimSpace(strings.ToLower(state)) == PreviewStateDeploying {
			want = PreviewStateDeploying
		}
		if got := rr.Header().Get(PreviewStateHeader); got != want {
			t.Fatalf("state %q → header %q, want %q", state, got, want)
		}
		if !strings.Contains(rr.Body.String(), "<meta http-equiv=\"refresh\"") {
			t.Fatalf("state %q should keep refreshing: %s", state, rr.Body.String())
		}
	}
}

// A HEAD gets the headers and no body, as the waking page does.
func TestPendingPageHeadHasNoBody(t *testing.T) {
	w := newPendingWaker(PendingRoute{Host: "prev.example.com", State: PreviewStateDeploying})
	req := httptest.NewRequest(http.MethodHead, "http://prev.example.com/", nil)
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if rr.Header().Get(PreviewStateHeader) != PreviewStateDeploying {
		t.Fatalf("marker header missing on HEAD")
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("HEAD returned a body: %q", rr.Body.String())
	}
}

// An API client gets the page too — it is the only answer the agent has for a
// host with no container. What matters is that it is not a 404 and not a
// success.
func TestPendingPageAlsoAnswersNonBrowserRequests(t *testing.T) {
	w := newPendingWaker(PendingRoute{Host: "prev.example.com", State: PreviewStateQueued})
	req := httptest.NewRequest(http.MethodPost, "http://prev.example.com/api", strings.NewReader("{}"))
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

// A host with no route and no pending entry is still a 404.
func TestUnknownHostStillNotFound(t *testing.T) {
	w := newPendingWaker(PendingRoute{Host: "prev.example.com", State: PreviewStateQueued})
	rr := getPending(t, w, "other.example.com")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// The regression test of ADR-073 §3: a real route wins over a pending entry
// that somehow survived the switch. The holding page must never sit in front
// of a working preview.
func TestRealRouteWinsOverStalePendingEntry(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusTeapot)
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)

	d := newFakeDocker()
	d.running["c1"] = true
	d.health["c1"] = "healthy"
	w := New(Config{
		Routes:    []Route{{Host: "prev.example.com", ResourceUUID: "prev-1", Container: u.Hostname(), Port: mustPort(t, u)}},
		Resources: []Resource{{UUID: "prev-1", Containers: []string{"c1"}}},
		Pending:   []PendingRoute{{Host: "prev.example.com", ResourceUUID: "prev-1", State: PreviewStateDeploying}},
	}, d, &fakeActivity{}, nil)
	w.Poll, w.StableFor = 0, 0

	rr := getPending(t, w, "prev.example.com")
	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 from the container", rr.Code)
	}
	if rr.Header().Get(PreviewStateHeader) != "" {
		t.Fatalf("holding page served in front of a live container")
	}
}

// A pending host carries its port like any other Host header value.
func TestPendingMatchesHostWithPort(t *testing.T) {
	w := newPendingWaker(PendingRoute{Host: "prev.example.com", State: PreviewStateQueued})
	req := httptest.NewRequest(http.MethodGet, "http://prev.example.com/", nil)
	req.Host = "prev.example.com:8080"
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

// The deposited routing document round-trips the pending entries: the control
// plane writes them, the agent reads them back unchanged.
func TestConfigRoundTripsPendingEntries(t *testing.T) {
	dir := t.TempDir()
	raw, err := MarshalConfig(Config{
		Pending: []PendingRoute{{Host: "prev.example.com", ResourceUUID: "prev-1", State: PreviewStateQueued, PRNumber: 12}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, RoutesFile), raw, 0o644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Pending) != 1 {
		t.Fatalf("pending entries = %d, want 1", len(cfg.Pending))
	}
	got := cfg.Pending[0]
	want := PendingRoute{Host: "prev.example.com", ResourceUUID: "prev-1", State: PreviewStateQueued, PRNumber: 12}
	if got != want {
		t.Fatalf("pending = %+v, want %+v", got, want)
	}
	if !strings.Contains(string(raw), `"pr_number": 12`) {
		t.Fatalf("pr_number tag missing from the document: %s", raw)
	}
}
