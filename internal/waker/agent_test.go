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

	"github.com/coder/websocket"
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
	var frames []wsFrame
	posts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/v1/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer akda_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentSubprotocol}})
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
			var f wsFrame
			_ = json.Unmarshal(data, &f)
			mu.Lock()
			frames = append(frames, f)
			mu.Unlock()
			ack, _ := json.Marshal(wsFrame{Type: "ack", Seq: f.Seq})
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

	a := NewAgent(AgentConfig{InstanceURL: srv.URL, Token: "akda_test"}, nil, nil)
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
		done := len(frames) > 0
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no frame ever reached the channel")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if frames[0].Type != "observations" || len(frames[0].Observations) != 1 ||
		frames[0].Observations[0].ResourceUUID != "res-1" {
		t.Fatalf("frame = %+v, want the observation batch", frames[0])
	}
	if posts != 0 {
		t.Fatalf("POST fallback used %d times while the channel was healthy", posts)
	}
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
