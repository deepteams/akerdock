package waker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
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
	a := NewAgent(AgentConfig{InstanceURL: srv.URL, Token: "akda_test"}, nil, nil)
	a.Flush = 10 * time.Millisecond
	a.Backoff = 5 * time.Millisecond
	a.Heartbeat = time.Hour // out of the way unless the test wants it
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	return a, cancel
}

func TestAgentPostsAuthenticatedBatches(t *testing.T) {
	c := &capture{}
	a, cancel := newTestAgent(t, c)
	defer cancel()

	a.Push(Observation{Type: "stz_woken", ResourceUUID: "res-1"})
	a.Push(Observation{Type: "container_state", Container: "res-1-web", State: "start"})

	batches := c.waitBatches(t, 1)
	if got := c.auth[0]; got != "Bearer akda_test" {
		t.Fatalf("authorization = %q, want the agent bearer", got)
	}
	if len(batches[0]) != 2 || batches[0][0].Type != "stz_woken" || batches[0][1].Container != "res-1-web" {
		t.Fatalf("batch = %+v, want both observations in order", batches[0])
	}
}

func TestAgentRetriesThenDelivers(t *testing.T) {
	c := &capture{status: []int{http.StatusInternalServerError}}
	a, cancel := newTestAgent(t, c)
	defer cancel()

	a.Push(Observation{Type: "heartbeat"})
	batches := c.waitBatches(t, 2) // failed attempt + successful retry
	if len(batches[1]) != 1 || batches[1][0].Type != "heartbeat" {
		t.Fatalf("retried batch = %+v, want the same observation redelivered", batches[1])
	}
}

func TestAgentDropsDeniedBatch(t *testing.T) {
	c := &capture{status: []int{http.StatusBadRequest}}
	a, cancel := newTestAgent(t, c)
	defer cancel()

	a.Push(Observation{Type: "stz_woken", ResourceUUID: "denied"})
	c.waitBatches(t, 1)
	a.Push(Observation{Type: "stz_woken", ResourceUUID: "next"})
	batches := c.waitBatches(t, 2)
	// The denied batch is NOT retried: the second POST carries only the new one.
	if len(batches[1]) != 1 || batches[1][0].ResourceUUID != "next" {
		t.Fatalf("post-deny batch = %+v, want only the new observation", batches[1])
	}
}

func TestAgentQueueShedsOldest(t *testing.T) {
	// No flusher running: the queue must shed its OLDEST entries, never block.
	a := NewAgent(AgentConfig{InstanceURL: "http://unused", Token: "akda_x"}, nil, nil)
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

func TestAgentDisabledWithoutEnrollment(t *testing.T) {
	if (AgentConfig{}).Enabled() || (AgentConfig{InstanceURL: "http://x"}).Enabled() {
		t.Fatal("an incomplete enrollment must disable the agent")
	}
	if !(AgentConfig{InstanceURL: "http://x", Token: "akda_y"}).Enabled() {
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
