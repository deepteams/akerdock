package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
)

func wsWriteFrame(ctx context.Context, conn *websocket.Conn, f agentwire.Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

// waitObservation reads the agent's queue until an observation matches, or
// fails at the deadline.
func waitObservation(t *testing.T, a *Agent, match func(Observation) bool) Observation {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case o := <-a.queue:
			if match(o) {
				return o
			}
		case <-deadline:
			t.Fatal("expected observation never queued")
		}
	}
}

// TestAgentRunReturnsWithoutEnrollment pins the waker-only mode: no
// enrollment, no loops — Run returns synchronously.
func TestAgentRunReturnsWithoutEnrollment(t *testing.T) {
	a := NewAgent(Enrollment{}, nil, nil)
	a.Run(context.Background()) // must return, not block
}

// TestContainTurnsAPanicIntoALogLine pins the isolation contract (ADR-040):
// a bug in one loop must never take the wake path down.
func TestContainTurnsAPanicIntoALogLine(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	contain(logger, "test loop", func() { panic("boom") })
}

func TestDenyErrorNamesTheStatus(t *testing.T) {
	if got := (&denyError{status: 403}).Error(); got != "denied: 403" {
		t.Fatalf("denyError = %q", got)
	}
}

// TestAgentPostStatusMapping pins the POST contract: 202 is the only success
// (a generic 200 is a misroute, not a delivery), 4xx is a non-retryable deny
// except 429, and everything else retries.
func TestAgentPostStatusMapping(t *testing.T) {
	var mu sync.Mutex
	codes := []int{
		http.StatusAccepted, http.StatusBadRequest, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusOK,
	}
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		code := codes[call]
		call++
		mu.Unlock()
		w.WriteHeader(code)
	}))
	defer srv.Close()
	a := NewAgent(Enrollment{InstanceURL: srv.URL, Token: "akda_test"}, nil, nil)
	batch := []Observation{{Type: "heartbeat"}}

	if err := a.post(context.Background(), batch); err != nil {
		t.Fatalf("202: %v, want success", err)
	}
	var deny *denyError
	if err := a.post(context.Background(), batch); !errors.As(err, &deny) {
		t.Fatalf("400: %v, want a deny", err)
	}
	if err := a.post(context.Background(), batch); err == nil || errors.As(err, &deny) {
		t.Fatalf("429: %v, want a retryable error", err)
	}
	if err := a.post(context.Background(), batch); err == nil || errors.As(err, &deny) {
		t.Fatalf("500: %v, want a retryable error", err)
	}
	if err := a.post(context.Background(), batch); err == nil {
		t.Fatal("200: want an error — a misrouted SPA answer is NOT a delivery")
	}
}

// TestAgentPostTransportErrors pins the request-building and dialing errors.
func TestAgentPostTransportErrors(t *testing.T) {
	bad := NewAgent(Enrollment{InstanceURL: "://not-a-url", Token: "t"}, nil, nil)
	if err := bad.post(context.Background(), nil); err == nil {
		t.Fatal("an unusable instance URL must fail the post")
	}
	// A loopback port nobody listens on: the Do error path.
	refused := NewAgent(Enrollment{InstanceURL: "http://127.0.0.1:1", Token: "t"}, nil, nil)
	if err := refused.post(context.Background(), nil); err == nil {
		t.Fatal("an unreachable control plane must fail the post")
	}
}

// TestAgentSendDialFailureArmsCooldownAndFallsBack pins the ADR-041 ladder's
// first rung: a failed channel dial does not fail the batch — it arms the
// cooldown and delivers over POST, and the next batch skips the dial.
func TestAgentSendDialFailureArmsCooldownAndFallsBack(t *testing.T) {
	posts := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/v1/observations" {
			mu.Lock()
			posts++
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.NotFound(w, r) // no WS endpoint: the dial fails
	}))
	defer srv.Close()

	a := NewAgent(Enrollment{InstanceURL: srv.URL, Token: "akda_test"}, nil, nil)
	batch := []Observation{{Type: "heartbeat"}}
	if err := a.send(context.Background(), batch); err != nil {
		t.Fatalf("send with a broken channel: %v, want POST delivery", err)
	}
	if a.ws != nil {
		t.Fatal("a failed dial must not leave a channel behind")
	}
	if !a.wsRetryAt.After(time.Now()) {
		t.Fatal("a failed dial must arm the re-dial cooldown")
	}
	// Within the cooldown: no dial attempt, straight to POST.
	if err := a.send(context.Background(), batch); err != nil {
		t.Fatalf("second send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 2 {
		t.Fatalf("posts = %d, want both batches on the fallback", posts)
	}
}

// TestAgentSendWSV1DenyIsNotRetried drives the v1 inline-ack path: garbage
// and stale acks are skipped, and a denied ack surfaces as a deny the flusher
// drops — never a retry.
func TestAgentSendWSV1DenyIsNotRetried(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/v1/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentwire.SubprotocolV1}})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var f agentwire.Frame
		_ = json.Unmarshal(data, &f)
		_ = conn.Write(ctx, websocket.MessageText, []byte("not json"))
		_ = wsWriteFrame(ctx, conn, agentwire.Frame{Type: agentwire.FrameAck, Seq: f.Seq + 99})
		_ = wsWriteFrame(ctx, conn, agentwire.Frame{Type: agentwire.FrameAck, Seq: f.Seq, Denied: true})
		_, _, _ = conn.Read(ctx) // hold the connection until the client is done
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := NewAgent(Enrollment{InstanceURL: srv.URL, Token: "akda_test"}, nil, nil)
	ctx := context.Background()
	if err := a.dialWS(ctx); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if a.ws.v2 {
		t.Fatal("without an executor the agent must not land on v2")
	}
	var deny *denyError
	if err := a.send(ctx, []Observation{{Type: "heartbeat"}}); !errors.As(err, &deny) {
		t.Fatalf("send = %v, want the deny surfaced", err)
	}
	a.closeWS()
	if a.ws != nil {
		t.Fatal("closeWS must drop the channel")
	}
}

// TestAgentSendWSV2DenyAndReadLoopDispatch drives the v2 read loop with the
// full frame zoo — input chunks, cancels, a nil command, garbage, a stale ack
// — before the denied ack that answers the batch.
func TestAgentSendWSV2DenyAndReadLoopDispatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/v1/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{agentwire.SubprotocolV2, agentwire.SubprotocolV1},
		})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var f agentwire.Frame
		_ = json.Unmarshal(data, &f)
		// Everything the read loop must shrug off before the real ack.
		_ = wsWriteFrame(ctx, conn, agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: 999, Data: []byte("x")}})
		_ = wsWriteFrame(ctx, conn, agentwire.Frame{Type: agentwire.FrameCancel, Cancel: 123})
		_ = wsWriteFrame(ctx, conn, agentwire.Frame{Type: agentwire.FrameCommand}) // nil Cmd
		_ = conn.Write(ctx, websocket.MessageText, []byte("not json"))
		_ = wsWriteFrame(ctx, conn, agentwire.Frame{Type: agentwire.FrameAck, Seq: f.Seq + 99})
		_ = wsWriteFrame(ctx, conn, agentwire.Frame{Type: agentwire.FrameAck, Seq: f.Seq, Denied: true})
		_, _, _ = conn.Read(ctx)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := NewAgent(Enrollment{InstanceURL: srv.URL, Token: "akda_test"}, nil, nil)
	a.Executor = NewExecutor(&fake.Runtime{}, nil, nil)
	ctx := context.Background()
	if err := a.dialWS(ctx); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if !a.ws.v2 {
		t.Fatal("with an executor the agent must offer and land on v2")
	}
	var deny *denyError
	if err := a.send(ctx, []Observation{{Type: "heartbeat"}}); !errors.As(err, &deny) {
		t.Fatalf("send = %v, want the deny surfaced", err)
	}
	a.closeWS()
}

// TestAgentSendWSDeadChannelFallsBackToPost pins the degradation on a channel
// dying mid-batch: the read loop's death surfaces, the channel is torn down
// and THIS batch still delivers over POST.
func TestAgentSendWSDeadChannelFallsBackToPost(t *testing.T) {
	posted := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/v1/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{agentwire.SubprotocolV2, agentwire.SubprotocolV1},
		})
		if err != nil {
			return
		}
		_, _, _ = conn.Read(r.Context()) // take the batch, never ack
		_ = conn.Close(websocket.StatusInternalError, "dying")
	})
	mux.HandleFunc("/agent/v1/observations", func(w http.ResponseWriter, _ *http.Request) {
		posted <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := NewAgent(Enrollment{InstanceURL: srv.URL, Token: "akda_test"}, nil, nil)
	a.Executor = NewExecutor(&fake.Runtime{}, nil, nil)
	ctx := context.Background()
	if err := a.dialWS(ctx); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := a.send(ctx, []Observation{{Type: "heartbeat"}}); err != nil {
		t.Fatalf("send over a dying channel = %v, want POST delivery", err)
	}
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("the batch never reached the POST fallback")
	}
	if a.ws != nil {
		t.Fatal("a dead channel must be torn down")
	}
}

// TestAgentHeartbeatLoopTicks pins the periodic heartbeat.
func TestAgentHeartbeatLoopTicks(t *testing.T) {
	a := NewAgent(Enrollment{InstanceURL: "http://unused", Token: "t"}, nil, nil)
	a.Heartbeat = 2 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.heartbeatLoop(ctx)
	waitObservation(t, a, func(o Observation) bool { return o.Type == "heartbeat" })
}

// failingLister makes the first resync listing fail — the loop must log and
// try again on the next tick, never die.
type failingLister struct {
	*eventDocker
	mu    sync.Mutex
	calls int
}

func (l *failingLister) ListManaged(ctx context.Context) ([]string, error) {
	l.mu.Lock()
	l.calls++
	first := l.calls == 1
	l.mu.Unlock()
	if first {
		return nil, errors.New("daemon busy")
	}
	return l.eventDocker.ListManaged(ctx)
}

// TestAgentResyncLoopObservesManagedContainers pins the self-healing net: on
// each tick every managed container's REAL state is pushed — even after a
// failed listing.
func TestAgentResyncLoopObservesManagedContainers(t *testing.T) {
	d := newEventDocker()
	d.managed = []string{"m1"}
	d.running["m1"] = true
	d.health["m1"] = "healthy"
	l := &failingLister{eventDocker: d}

	a := NewAgent(Enrollment{InstanceURL: "http://unused", Token: "t"}, l, nil)
	a.Resync = 2 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.resyncLoop(ctx)

	o := waitObservation(t, a, func(o Observation) bool {
		return o.Type == "container_state" && o.Container == "m1"
	})
	if o.State != "healthy" {
		t.Fatalf("resync observation = %+v, want the verified healthy state", o)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls < 2 {
		t.Fatalf("listings = %d, want the loop to survive the first failure", l.calls)
	}
}

// flakyStreamer breaks its first stream — the loop must reconnect with
// backoff — then delivers one noise event and one interesting event.
type flakyStreamer struct {
	mu    sync.Mutex
	calls int
}

func (s *flakyStreamer) StreamEvents(ctx context.Context, handler func(ContainerEvent)) error {
	s.mu.Lock()
	s.calls++
	first := s.calls == 1
	s.mu.Unlock()
	if first {
		return errors.New("stream broke")
	}
	handler(ContainerEvent{Container: "c1", Action: "exec_start"}) // noise: filtered
	handler(ContainerEvent{Container: "c1", Action: "start"})
	<-ctx.Done()
	return ctx.Err()
}

// TestAgentStreamLoopReconnectsAndFilters pins the event loop: a broken
// stream reconnects after the backoff, noise actions are dropped, and —
// without a state reader — the event is pushed as-is with state unknown.
func TestAgentStreamLoopReconnectsAndFilters(t *testing.T) {
	s := &flakyStreamer{}
	a := NewAgent(Enrollment{InstanceURL: "http://unused", Token: "t"}, s, nil)
	a.Backoff = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.streamLoop(ctx)

	o := waitObservation(t, a, func(o Observation) bool { return o.Type == "container_state" })
	if o.Container != "c1" || o.State != "unknown" {
		t.Fatalf("observation = %+v, want the raw event with state unknown", o)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls < 2 {
		t.Fatalf("stream connections = %d, want a reconnect after the break", s.calls)
	}
}

// TestVerifyLaterEdges pins the verification scheduler's guards: an empty
// container name is ignored, a duplicate event collapses into the pending
// verification, and cancellation abandons the reading.
func TestVerifyLaterEdges(t *testing.T) {
	d := newEventDocker()
	d.running["x"] = true
	d.health["x"] = "healthy"
	a := NewAgent(Enrollment{InstanceURL: "http://unused", Token: "t"}, d, nil)
	a.Settle = 5 * time.Millisecond
	ctx := context.Background()

	a.verifyLater(ctx, "")
	if len(a.queue) != 0 {
		t.Fatal("an empty container name must not queue anything")
	}

	// Two events in a burst: ONE pending verification, one observation.
	a.verifyLater(ctx, "x")
	a.verifyLater(ctx, "x")
	a.mu.Lock()
	pending := len(a.pending)
	a.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending verifications = %d, want the burst collapsed to 1", pending)
	}
	waitObservation(t, a, func(o Observation) bool { return o.Container == "x" && o.State == "healthy" })

	// A canceled context abandons the settled reading silently.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	a.verifyLater(canceled, "y")
	if len(a.queue) != 0 {
		t.Fatal("a canceled verification must not push")
	}
}

// TestObserveSkipsAVanishedContainer pins the mid-replacement rule: a
// container that no longer inspects pushes NOTHING — its absence says nothing
// about the service.
func TestObserveSkipsAVanishedContainer(t *testing.T) {
	d := newEventDocker()
	d.inspectErr = errors.New("no such container")
	a := NewAgent(Enrollment{InstanceURL: "http://unused", Token: "t"}, d, nil)
	a.observe(context.Background(), "gone")
	if len(a.queue) != 0 {
		t.Fatalf("queue = %d observations, want none for a vanished container", len(a.queue))
	}
}

// TestAgentFlushLoopStopsOnCancelDuringRetry pins the shutdown path: a batch
// stuck in retry lets go when the context ends, and Run returns.
func TestAgentFlushLoopStopsOnCancelDuringRetry(t *testing.T) {
	attempted := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case attempted <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusInternalServerError) // always retryable
	}))
	defer srv.Close()

	a := NewAgent(Enrollment{InstanceURL: srv.URL, Token: "akda_test"}, nil, nil)
	a.Flush = 2 * time.Millisecond
	a.Backoff = time.Hour // park the retry in its backoff sleep
	a.Heartbeat = time.Hour
	a.DisableWS = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx) // the hello heartbeat fails and enters retry
		close(done)
	}()

	// Wait until the failing delivery happened, then cancel mid-backoff.
	select {
	case <-attempted:
	case <-time.After(2 * time.Second):
		t.Fatal("the hello batch never reached the control plane")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run never returned after cancellation during a retry")
	}
}
