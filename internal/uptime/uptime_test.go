package uptime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTransitionThresholds(t *testing.T) {
	// A fresh check goes up on the FIRST success…
	s, changed := Transition(State{Status: StatusUnknown}, true, 3, 2)
	if !changed || s.Status != StatusUp {
		t.Fatalf("first success must establish up: %+v changed=%v", s, changed)
	}
	// …but down needs the full threshold, from unknown as from up.
	s = State{Status: StatusUp}
	for i := 1; i <= 2; i++ {
		var ch bool
		s, ch = Transition(s, false, 3, 2)
		if ch {
			t.Fatalf("failure %d must not flip yet (threshold 3)", i)
		}
	}
	s, changed = Transition(s, false, 3, 2)
	if !changed || s.Status != StatusDown || s.ConsecutiveFailures != 3 {
		t.Fatalf("third failure must flip to down: %+v", s)
	}
	// Recovery needs success_threshold consecutive successes.
	s, changed = Transition(s, true, 3, 2)
	if changed || s.Status != StatusDown {
		t.Fatalf("one success must not clear a real outage: %+v", s)
	}
	s, changed = Transition(s, true, 3, 2)
	if !changed || s.Status != StatusUp {
		t.Fatalf("second success must recover: %+v", s)
	}
	// A blip resets the success streak.
	s = State{Status: StatusDown, ConsecutiveSuccesses: 1}
	s, _ = Transition(s, false, 3, 2)
	if s.ConsecutiveSuccesses != 0 {
		t.Fatalf("a failure must reset the success streak: %+v", s)
	}
}

func TestProbeHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := Probe(context.Background(), "http", srv.URL+"/", 2*time.Second)
	if !res.OK || res.StatusCode != 200 {
		t.Fatalf("healthy endpoint must be up: %+v", res)
	}
	// A 500 is reachable but NOT up — the outage this feature catches.
	res = Probe(context.Background(), "http", srv.URL+"/broken", 2*time.Second)
	if res.OK || res.StatusCode != 500 || !strings.Contains(res.Error, "500") {
		t.Fatalf("a 500 must be down: %+v", res)
	}
	res = Probe(context.Background(), "http", "http://127.0.0.1:1/", 500*time.Millisecond)
	if res.OK || res.Error == "" {
		t.Fatalf("a refused connection must be down with a reason: %+v", res)
	}
}

func TestProbeTCP(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	res := Probe(context.Background(), "tcp", addr, 2*time.Second)
	if !res.OK {
		t.Fatalf("open port must be up: %+v", res)
	}
	res = Probe(context.Background(), "tcp", "127.0.0.1:1", 500*time.Millisecond)
	if res.OK {
		t.Fatalf("closed port must be down: %+v", res)
	}
}
