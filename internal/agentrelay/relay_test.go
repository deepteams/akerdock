package agentrelay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// scriptedRelayServer stands in for the api-side bridge: it accepts relay
// WebSockets and answers a small scripted vocabulary.
//
//	test.ok       → result {"ok":true}
//	test.notfound → typed not_found error
//	test.stream   → ack, "hello ", "world", EOF
//	test.attach   → ack, then echoes input chunks until stdin EOF
//	test.junk     → malformed and unknown frames, then result {"ok":true}
//	test.close    → slams the connection without answering
//
// accepts, when non-nil, counts accepted connections; it is incremented
// before the handshake response so a returned client dial implies the count.
func scriptedRelayServer(t *testing.T, accepts *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accepts != nil {
			accepts.Add(1)
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentwire.SubprotocolRelay}})
		if err != nil {
			return
		}
		defer func() { _ = ws.Close(websocket.StatusNormalClosure, "") }()
		ws.SetReadLimit(1 << 20)
		ctx := r.Context()
		write := func(f agentwire.Frame) {
			data, err := json.Marshal(f)
			if err != nil {
				return
			}
			_ = ws.Write(ctx, websocket.MessageText, data)
		}
		result := func(res agentwire.Result) {
			write(agentwire.Frame{Type: agentwire.FrameResult, Res: &res})
		}
		chunk := func(c agentwire.StreamChunk) {
			write(agentwire.Frame{Type: agentwire.FrameStream, Chunk: &c})
		}
		attached := map[int64]bool{}
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var f agentwire.Frame
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			switch f.Type {
			case agentwire.FrameCommand:
				cmd := f.Cmd
				if cmd == nil {
					continue
				}
				switch cmd.Method {
				case "test.ok":
					result(agentwire.Result{ID: cmd.ID, Body: json.RawMessage(`{"ok":true}`)})
				case "test.notfound":
					result(agentwire.Result{ID: cmd.ID, Err: &agentwire.Error{Code: agentwire.CodeNotFound, Message: "gone"}})
				case "test.stream":
					result(agentwire.Result{ID: cmd.ID})
					chunk(agentwire.StreamChunk{ID: cmd.ID, Data: []byte("hello ")})
					chunk(agentwire.StreamChunk{ID: cmd.ID, Data: []byte("world")})
					chunk(agentwire.StreamChunk{ID: cmd.ID, EOF: true})
				case "test.attach":
					attached[cmd.ID] = true
					result(agentwire.Result{ID: cmd.ID})
				case "test.junk":
					_ = ws.Write(ctx, websocket.MessageText, []byte("not json")) // malformed frame
					write(agentwire.Frame{Type: agentwire.FrameAck})             // frame type the client does not route
					write(agentwire.Frame{Type: agentwire.FrameResult})          // result frame with no payload
					write(agentwire.Frame{Type: agentwire.FrameStream})          // stream frame with no payload
					result(agentwire.Result{ID: cmd.ID, Body: json.RawMessage(`{"ok":true}`)})
				case "test.close":
					return
				}
			case agentwire.FrameStream: // attach input from the client
				c := f.Chunk
				if c == nil || !attached[c.ID] {
					continue
				}
				if c.EOF {
					chunk(agentwire.StreamChunk{ID: c.ID, EOF: true})
					continue
				}
				chunk(agentwire.StreamChunk{ID: c.ID, Data: c.Data})
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testSource wires a Source at the scripted server. The cleanup order matters:
// the server is registered first, so the source's sockets close before the
// httptest server waits on its hijacked connections.
func testSource(t *testing.T, srv *httptest.Server) *Source {
	t.Helper()
	src := &Source{
		BaseURL: func(context.Context) (string, error) { return srv.URL, nil },
		Token:   func(context.Context, int64) (string, error) { return "akda_test", nil },
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	t.Cleanup(src.Close)
	return src
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestSourceBridgesCommandsStreamsAndAttach(t *testing.T) {
	srv := scriptedRelayServer(t, nil)
	src := testSource(t, srv)
	ctx := testContext(t)

	if rt, err := src.Runtime(ctx, 7); err != nil || rt == nil {
		t.Fatalf("Runtime = (%v, %v), want a live runtime", rt, err)
	}
	if ops, err := src.HostOps(ctx, 7); err != nil || ops == nil {
		t.Fatalf("HostOps = (%v, %v), want live ops", ops, err)
	}

	ds := dynamicSender{s: src, serverID: 7}

	body, err := ds.Command(ctx, "test.ok", map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("Command body = %q, want {\"ok\":true}", body)
	}

	if _, err := ds.Command(ctx, "test.notfound", nil); !dockerruntime.IsNotFound(err) {
		t.Fatalf("typed error = %v, want IsNotFound preserved across the relay", err)
	}

	rc, err := ds.Stream(ctx, "test.stream", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	all, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll(stream): %v", err)
	}
	if string(all) != "hello world" {
		t.Fatalf("stream = %q, want %q", all, "hello world")
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("stream Close: %v", err)
	}

	as, err := ds.Attach(ctx, "test.attach", nil)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if _, err := as.Write([]byte("ping")); err != nil {
		t.Fatalf("attach Write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(as, buf); err != nil {
		t.Fatalf("attach Read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("attach echo = %q, want %q", buf, "ping")
	}
	if err := as.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if n, err := as.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("read after stdin EOF = (%d, %v), want (0, io.EOF)", n, err)
	}
	if err := as.Close(); err != nil {
		t.Fatalf("attach Close: %v", err)
	}
}

func TestReadLoopSkipsMalformedAndUnroutedFrames(t *testing.T) {
	srv := scriptedRelayServer(t, nil)
	src := testSource(t, srv)
	ctx := testContext(t)

	ds := dynamicSender{s: src, serverID: 1}
	body, err := ds.Command(ctx, "test.junk", nil)
	if err != nil {
		t.Fatalf("Command after junk frames: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("Command body = %q, want the result past the junk", body)
	}
}

func TestConnReusesLiveAndRedialsDead(t *testing.T) {
	var accepts atomic.Int32
	srv := scriptedRelayServer(t, &accepts)
	src := testSource(t, srv)
	ctx := testContext(t)

	c1, err := src.conn(ctx, 1)
	if err != nil {
		t.Fatalf("first conn: %v", err)
	}
	c2, err := src.conn(ctx, 1)
	if err != nil {
		t.Fatalf("second conn: %v", err)
	}
	if c1 != c2 {
		t.Fatal("live cached conn was not reused")
	}
	if got := accepts.Load(); got != 1 {
		t.Fatalf("accepts = %d after two conn calls, want 1", got)
	}

	// The server slams the connection instead of answering; the in-flight
	// command surfaces the mandatory-agent unavailable class and the cached
	// conn is dead afterwards.
	if _, err := c1.Command(ctx, "test.close", nil); !dockerruntime.IsUnavailable(err) {
		t.Fatalf("command on slammed conn = %v, want IsUnavailable", err)
	}
	<-c1.Done()

	c3, err := src.conn(ctx, 1)
	if err != nil {
		t.Fatalf("redial: %v", err)
	}
	if c3 == c1 {
		t.Fatal("dead cached conn was returned instead of a fresh dial")
	}
	if _, err := c3.Command(ctx, "test.ok", nil); err != nil {
		t.Fatalf("command on redialed conn: %v", err)
	}
	if got := accepts.Load(); got != 2 {
		t.Fatalf("accepts = %d after redial, want 2", got)
	}
}

func TestCloseTearsDownCachedConns(t *testing.T) {
	srv := scriptedRelayServer(t, nil)
	src := &Source{
		BaseURL: func(context.Context) (string, error) { return srv.URL, nil },
		Token:   func(context.Context, int64) (string, error) { return "akda_test", nil },
	}
	ctx := testContext(t)

	c, err := src.conn(ctx, 3)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	src.Close()
	select {
	case <-c.Done():
	default:
		t.Fatal("Close left the cached conn alive")
	}
	src.mu.Lock()
	n := len(src.conns)
	src.mu.Unlock()
	if n != 0 {
		t.Fatalf("Close left %d cached conns, want 0", n)
	}
	src.Close() // idempotent on an empty cache
}

// TestConcurrentDialsShareOneConn drives both goroutines into dial before
// either stores its connection (the BaseURL resolver is the barrier), so the
// loser of the store race must adopt the winner's live conn and discard its
// own.
func TestConcurrentDialsShareOneConn(t *testing.T) {
	var accepts atomic.Int32
	srv := scriptedRelayServer(t, &accepts)
	var barrier sync.WaitGroup
	barrier.Add(2)
	src := &Source{
		BaseURL: func(context.Context) (string, error) {
			barrier.Done()
			barrier.Wait()
			return srv.URL, nil
		},
		Token: func(context.Context, int64) (string, error) { return "akda_test", nil },
	}
	t.Cleanup(src.Close)
	ctx := testContext(t)

	conns := make(chan *relayConn, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			c, err := src.conn(ctx, 1)
			errs <- err
			conns <- c
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("racing conn: %v", err)
		}
	}
	c1, c2 := <-conns, <-conns
	if c1 != c2 {
		t.Fatal("dial race kept two conns for one server")
	}
	if got := accepts.Load(); got != 2 {
		t.Fatalf("accepts = %d, want 2 (both goroutines dialed)", got)
	}
	src.mu.Lock()
	n := len(src.conns)
	src.mu.Unlock()
	if n != 1 {
		t.Fatalf("cache holds %d conns after the race, want 1", n)
	}
	if _, err := c1.Command(ctx, "test.ok", nil); err != nil {
		t.Fatalf("command on the kept conn: %v", err)
	}
}

// TestDialRaceReplacesDeadExisting covers the store-race branch where the
// concurrently stored conn has already died by the time the slower dial
// finishes: the dead one is discarded, the fresh one kept.
func TestDialRaceReplacesDeadExisting(t *testing.T) {
	srv := scriptedRelayServer(t, nil)
	var dials atomic.Int32
	arrived := make(chan struct{})
	gate := make(chan struct{})
	src := &Source{
		BaseURL: func(context.Context) (string, error) { return srv.URL, nil },
		Token: func(context.Context, int64) (string, error) {
			if dials.Add(1) == 1 { // the slow dial parks here mid-flight
				close(arrived)
				<-gate
			}
			return "akda_test", nil
		},
	}
	t.Cleanup(src.Close)
	ctx := testContext(t)

	type outcome struct {
		c   *relayConn
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		c, err := src.conn(ctx, 1)
		done <- outcome{c, err}
	}()
	<-arrived

	stale, err := src.conn(ctx, 1) // second dial, unblocked, stores first
	if err != nil {
		t.Fatalf("interleaved conn: %v", err)
	}
	stale.close()
	<-stale.Done()

	close(gate) // let the parked dial finish and hit the dead cached conn
	got := <-done
	if got.err != nil {
		t.Fatalf("parked conn: %v", got.err)
	}
	if got.c == stale {
		t.Fatal("dead cached conn was adopted instead of the fresh dial")
	}
	if _, err := got.c.Command(ctx, "test.ok", nil); err != nil {
		t.Fatalf("command on the fresh conn: %v", err)
	}
}

func TestDynamicSenderResolutionFailuresAreUnavailable(t *testing.T) {
	ctx := context.Background()
	src := &Source{
		BaseURL: func(context.Context) (string, error) { return "", errors.New("no settings") },
		Token:   func(context.Context, int64) (string, error) { return "akda_x", nil },
	}
	ds := dynamicSender{s: src, serverID: 1}
	if _, err := ds.Command(ctx, "test.ok", nil); !dockerruntime.IsUnavailable(err) {
		t.Fatalf("Command without a conn = %v, want IsUnavailable", err)
	}
	if _, err := ds.Stream(ctx, "test.stream", nil); !dockerruntime.IsUnavailable(err) {
		t.Fatalf("Stream without a conn = %v, want IsUnavailable", err)
	}
	if _, err := ds.Attach(ctx, "test.attach", nil); !dockerruntime.IsUnavailable(err) {
		t.Fatalf("Attach without a conn = %v, want IsUnavailable", err)
	}
	if _, err := src.HostOps(ctx, 1); !dockerruntime.IsUnavailable(err) {
		t.Fatalf("HostOps without a conn = %v, want IsUnavailable", err)
	}
}
