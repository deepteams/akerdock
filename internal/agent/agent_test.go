package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/deepteams/akerdock/internal/agentwire"
)

// capture records every batch the agent posts.
type capture struct {
	mu      sync.Mutex
	auth    []string
	batches [][]Observation
	status  []int // per-request response code, defaults to 202
	calls   int
}

func (c *capture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		var payload struct {
			Observations []Observation `json:"observations"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		c.auth = append(c.auth, r.Header.Get("Authorization"))
		c.batches = append(c.batches, payload.Observations)
		code := http.StatusAccepted
		if c.calls < len(c.status) {
			code = c.status[c.calls]
		}
		c.calls++
		w.WriteHeader(code)
	}
}

func (c *capture) waitBatches(t *testing.T, n int) [][]Observation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		if len(c.batches) >= n {
			out := append([][]Observation(nil), c.batches...)
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("expected %d batches, server never received them", n)
		}
		time.Sleep(time.Millisecond)
	}
}

func newTestAgent(t *testing.T, c *capture) (*Agent, context.CancelFunc) {
	t.Helper()
	srv := httptest.NewServer(c.handler())
	t.Cleanup(srv.Close)
	a := NewAgent(Enrollment{InstanceURL: srv.URL, Token: "akda_test"}, nil, nil)
	a.Flush = 10 * time.Millisecond
	a.Backoff = 5 * time.Millisecond
	a.Heartbeat = time.Hour // out of the way unless the test wants it
	a.DisableWS = true      // these tests exercise the POST fallback path
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	return a, cancel
}

// TestAgentPrefersChannel proves the ADR-041 ladder: with a healthy WebSocket
// endpoint, batches ride the channel as acknowledged frames and the POST
// fallback is never touched.
func TestAgentPrefersChannel(t *testing.T) {
	var mu sync.Mutex
	var frames []agentwire.Frame
	posts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/v1/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer akda_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentwire.SubprotocolV1}})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var f agentwire.Frame
			_ = json.Unmarshal(data, &f)
			mu.Lock()
			frames = append(frames, f)
			mu.Unlock()
			ack, _ := json.Marshal(agentwire.Frame{Type: "ack", Seq: f.Seq})
			if conn.Write(ctx, websocket.MessageText, ack) != nil {
				return
			}
		}
	})
	mux.HandleFunc("/agent/v1/observations", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		posts++
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := NewAgent(Enrollment{InstanceURL: srv.URL, Token: "akda_test"}, nil, nil)
	a.Flush = 10 * time.Millisecond
	a.Backoff = 5 * time.Millisecond
	a.Heartbeat = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Push(Observation{Type: "stz_woken", ResourceUUID: "res-1"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		delivered := false
		for _, f := range frames {
			for _, o := range f.Observations {
				delivered = delivered || (f.Type == "observations" && o.ResourceUUID == "res-1")
			}
		}
		mu.Unlock()
		if delivered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the observation never reached the channel")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 0 {
		t.Fatalf("POST fallback used %d times while the channel was healthy", posts)
	}
}

// flatten gathers every delivered observation across batches.
func flatten(batches [][]Observation) []Observation {
	var out []Observation
	for _, b := range batches {
		out = append(out, b...)
	}
	return out
}

func TestAgentPostsAuthenticatedBatches(t *testing.T) {
	c := &capture{}
	a, cancel := newTestAgent(t, c)
	defer cancel()

	a.Push(Observation{Type: "stz_woken", ResourceUUID: "res-1"})
	a.Push(Observation{Type: "container_state", Container: "res-1-web", State: "start"})

	// The startup hello heartbeat may ride the same batch or its own: assert
	// on delivered content, not batch layout.
	deadline := time.Now().Add(2 * time.Second)
	for {
		all := flatten(c.waitBatches(t, 1))
		woken, state := false, false
		for _, o := range all {
			woken = woken || o.ResourceUUID == "res-1"
			state = state || o.Container == "res-1-web"
		}
		if woken && state {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("observations never delivered: %+v", all)
		}
		time.Sleep(time.Millisecond)
	}
	if got := c.auth[0]; got != "Bearer akda_test" {
		t.Fatalf("authorization = %q, want the agent bearer", got)
	}
}

func TestAgentRetriesThenDelivers(t *testing.T) {
	// The first delivery (the startup hello) fails with a 500: the SAME batch
	// must be redelivered and accepted.
	c := &capture{status: []int{http.StatusInternalServerError}}
	_, cancel := newTestAgent(t, c)
	defer cancel()

	batches := c.waitBatches(t, 2) // failed attempt + successful retry
	if len(batches[1]) != len(batches[0]) || len(batches[1]) == 0 || batches[1][0].Type != "heartbeat" {
		t.Fatalf("retried batch = %+v, want the failed batch redelivered", batches[1])
	}
}

func TestAgentDropsDeniedBatch(t *testing.T) {
	// Batch 1 (the hello) passes, batch 2 is denied and must be dropped —
	// batch 3 then carries only the new observation.
	c := &capture{status: []int{http.StatusAccepted, http.StatusBadRequest}}
	a, cancel := newTestAgent(t, c)
	defer cancel()

	c.waitBatches(t, 1)
	a.Push(Observation{Type: "stz_woken", ResourceUUID: "denied"})
	c.waitBatches(t, 2)
	a.Push(Observation{Type: "stz_woken", ResourceUUID: "next"})
	batches := c.waitBatches(t, 3)
	if len(batches[2]) != 1 || batches[2][0].ResourceUUID != "next" {
		t.Fatalf("post-deny batch = %+v, want only the new observation", batches[2])
	}
}

func TestAgentQueueShedsOldest(t *testing.T) {
	// No flusher running: the queue must shed its OLDEST entries, never block.
	a := NewAgent(Enrollment{InstanceURL: "http://unused", Token: "akda_x"}, nil, nil)
	for i := 0; i < agentQueueCap+25; i++ {
		a.Push(Observation{Type: "container_state", ResourceUUID: fmt.Sprint(i)})
	}
	if len(a.queue) != agentQueueCap {
		t.Fatalf("queue length = %d, want the cap %d", len(a.queue), agentQueueCap)
	}
	first := <-a.queue
	if first.ResourceUUID == "0" {
		t.Fatal("oldest observation survived a full queue — overflow must shed the oldest")
	}
}

// eventDocker is a fake Docker exposing the event stream, state reads and the
// managed listing — the three the agent uses to observe.
type eventDocker struct {
	*fakeDocker
	events  chan ContainerEvent
	managed []string
}

func newEventDocker() *eventDocker {
	return &eventDocker{fakeDocker: newFakeDocker(), events: make(chan ContainerEvent, 8)}
}

func (d *eventDocker) StreamEvents(ctx context.Context, handler func(ContainerEvent)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-d.events:
			handler(ev)
		}
	}
}

func (d *eventDocker) ListManaged(context.Context) ([]string, error) { return d.managed, nil }

// Regression for the "exited while the service is up" report: during a
// zero-downtime replacement the OLD container dies under the service's name
// while its replacement is renamed into place. The event is a trigger — the
// agent must report what the daemon says AFTERWARDS, not the action itself.
func TestAgentVerifiesStateAfterEventInsteadOfTrustingIt(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(c.handler())
	defer srv.Close()

	d := newEventDocker()
	// The replacement is already running and healthy when the settle delay
	// expires — exactly the state the deploy leaves behind.
	d.running["app-1-frontend"] = true
	d.health["app-1-frontend"] = "healthy"

	a := NewAgent(Enrollment{InstanceURL: srv.URL, Token: "akda_test"}, d, nil)
	a.Flush, a.Backoff, a.Heartbeat = 10*time.Millisecond, 5*time.Millisecond, time.Hour
	a.Settle, a.Resync, a.DisableWS = 5*time.Millisecond, time.Hour, true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	d.events <- ContainerEvent{Container: "app-1-frontend", Action: "die", At: time.Now()}

	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, o := range flatten(c.waitBatches(t, 1)) {
			if o.Type != "container_state" {
				continue
			}
			if o.Container != "app-1-frontend" {
				t.Fatalf("unexpected container in observation: %+v", o)
			}
			if o.State == "exited" || o.State == "die" {
				t.Fatalf("the agent reported the raw event instead of the verified state: %+v", o)
			}
			if o.State == "healthy" {
				return // the running replacement is what got reported
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no verified container_state observation was delivered")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestObservedState(t *testing.T) {
	cases := []struct {
		st   ContainerState
		want string
	}{
		{ContainerState{Running: false, Health: "none"}, "exited"},
		{ContainerState{Running: true, Health: "healthy"}, "healthy"},
		{ContainerState{Running: true, Health: "unhealthy"}, "unhealthy"},
		{ContainerState{Running: true, Health: "starting"}, "starting"},
		// No healthcheck: running IS up — the same call the deploy makes.
		{ContainerState{Running: true, Health: "none"}, "healthy"},
	}
	for _, c := range cases {
		if got := observedState(c.st); got != c.want {
			t.Fatalf("observedState(%+v) = %q, want %q", c.st, got, c.want)
		}
	}
}

func TestAgentDisabledWithoutEnrollment(t *testing.T) {
	if (Enrollment{}).Enabled() || (Enrollment{InstanceURL: "http://x"}).Enabled() {
		t.Fatal("an incomplete enrollment must disable the agent")
	}
	if !(Enrollment{InstanceURL: "http://x", Token: "akda_y"}).Enabled() {
		t.Fatal("a complete enrollment must enable the agent")
	}
}

func TestWakeNotifiesOnWake(t *testing.T) {
	d := newFakeDocker() // both containers stopped: the wake starts them
	w, _, closeFn := newTestWaker(t, d, &fakeActivity{})
	defer closeFn()
	var woken []string
	w.OnWake = func(uuid string) { woken = append(woken, uuid) }

	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))
	if len(woken) != 1 || woken[0] != "res-1" {
		t.Fatalf("OnWake calls = %v, want one for res-1", woken)
	}

	// Already awake: no containers started, no notification.
	woken = nil
	rr = httptest.NewRecorder()
	w.ServeHTTP(rr, request("app.example.com", ""))
	if len(woken) != 0 {
		t.Fatalf("OnWake called on an already-awake resource: %v", woken)
	}
}
