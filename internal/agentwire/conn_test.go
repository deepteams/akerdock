package agentwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	cerrdefs "github.com/containerd/errdefs"
)

// testChannel wires a Conn under test to a scripted peer over a real
// WebSocket pair — the same shape as the api process, which owns the socket,
// runs the read loop and feeds frames back through DeliverResult and
// DeliverChunk.
type testChannel struct {
	t      *testing.T
	conn   *Conn
	ws     *websocket.Conn    // the Conn's own socket, broken on demand
	peer   *websocket.Conn    // the scripted far side
	cancel context.CancelFunc // ends the Conn's lifetime
	frames chan Frame         // every frame the peer received
}

func newTestChannel(t *testing.T) *testChannel {
	t.Helper()
	frames := make(chan Frame, 128)
	peerCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		peerCh <- c
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var f Frame
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			select {
			case frames <- f:
			case <-r.Context().Done():
				return
			}
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	ws, _, err := websocket.Dial(dialCtx, strings.Replace(srv.URL, "http", "ws", 1), nil)
	if err != nil {
		cancel()
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	ws.SetReadLimit(1 << 20)
	conn := NewConn(ctx, ws)
	go func() { // the owner's read loop
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var f Frame
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			switch f.Type {
			case FrameResult:
				conn.DeliverResult(f.Res)
			case FrameStream:
				conn.DeliverChunk(f.Chunk)
			}
		}
	}()
	var peer *websocket.Conn
	select {
	case peer = <-peerCh:
	case <-time.After(5 * time.Second):
		cancel()
		srv.Close()
		t.Fatal("peer never accepted")
	}
	tc := &testChannel{t: t, conn: conn, ws: ws, peer: peer, cancel: cancel, frames: frames}
	t.Cleanup(func() {
		cancel()
		_ = ws.CloseNow()
		_ = peer.CloseNow()
		srv.Close()
	})
	return tc
}

// expectFrame returns the next frame the peer received, failing on a
// different type — the protocol is deterministic, an unexpected frame is a
// bug, not noise to skip.
func (tc *testChannel) expectFrame(want string) Frame {
	tc.t.Helper()
	select {
	case f := <-tc.frames:
		if f.Type != want {
			tc.t.Fatalf("peer received a %q frame, want %q (%+v)", f.Type, want, f)
		}
		return f
	case <-time.After(5 * time.Second):
		tc.t.Fatalf("no %q frame reached the peer", want)
	}
	return Frame{}
}

// reply writes one frame from the peer back to the Conn.
func (tc *testChannel) reply(f Frame) {
	tc.t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		tc.t.Fatalf("marshal reply: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tc.peer.Write(ctx, websocket.MessageText, data); err != nil {
		tc.t.Fatalf("peer write: %v", err)
	}
}

func (tc *testChannel) ack(id int64) {
	tc.t.Helper()
	tc.reply(Frame{Type: FrameResult, Res: &Result{ID: id}})
}

type cmdOutcome struct {
	body json.RawMessage
	err  error
}

// runCommand starts Command in the background so the test goroutine can
// script the peer (and keep t.Fatal legal).
func runCommand(ctx context.Context, tc *testChannel, method string, params any) <-chan cmdOutcome {
	out := make(chan cmdOutcome, 1)
	go func() {
		body, err := tc.conn.Command(ctx, method, params)
		out <- cmdOutcome{body, err}
	}()
	return out
}

func waitOutcome(t *testing.T, out <-chan cmdOutcome) cmdOutcome {
	t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("command never returned")
	}
	return cmdOutcome{}
}

func TestUnavailableIsTyped(t *testing.T) {
	err := Unavailable("closed")
	if !cerrdefs.IsUnavailable(err) {
		t.Fatalf("Unavailable lost its class: %v", err)
	}
	if got := err.Error(); got != "agent channel closed: unavailable" {
		t.Fatalf("message = %q", got)
	}
}

func TestDoneFollowsTheOwnerContext(t *testing.T) {
	tc := newTestChannel(t)
	select {
	case <-tc.conn.Done():
		t.Fatal("Done closed while the owner context is live")
	default:
	}
	tc.cancel()
	select {
	case <-tc.conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done never closed after the owner context ended")
	}
}

func TestCommandRoundTrip(t *testing.T) {
	tc := newTestChannel(t)
	obs := make(chan [2]string, 1)
	tc.conn.Record = func(method, outcome string) { obs <- [2]string{method, outcome} }

	out := runCommand(context.Background(), tc, MethodContainerInspect, NameParams{Name: "web"})
	f := tc.expectFrame(FrameCommand)
	if f.Cmd == nil || f.Cmd.Method != MethodContainerInspect {
		t.Fatalf("command frame = %+v", f)
	}
	var p NameParams
	if err := json.Unmarshal(f.Cmd.Params, &p); err != nil || p.Name != "web" {
		t.Fatalf("params = %s (%v)", f.Cmd.Params, err)
	}
	tc.reply(Frame{Type: FrameResult, Res: &Result{ID: f.Cmd.ID, Body: json.RawMessage(`{"id":"abc"}`)}})

	o := waitOutcome(t, out)
	if o.err != nil {
		t.Fatalf("command: %v", o.err)
	}
	if string(o.body) != `{"id":"abc"}` {
		t.Fatalf("body = %s", o.body)
	}
	if got := <-obs; got != [2]string{MethodContainerInspect, "ok"} {
		t.Fatalf("observed %v", got)
	}
}

func TestCommandErrorResultKeepsItsClass(t *testing.T) {
	tc := newTestChannel(t)
	obs := make(chan [2]string, 1)
	tc.conn.Record = func(method, outcome string) { obs <- [2]string{method, outcome} }

	out := runCommand(context.Background(), tc, MethodContainerInspect, NameParams{Name: "gone"})
	f := tc.expectFrame(FrameCommand)
	tc.reply(Frame{Type: FrameResult, Res: &Result{ID: f.Cmd.ID, Err: &Error{Code: CodeNotFound, Message: "no such container"}}})

	o := waitOutcome(t, out)
	if !cerrdefs.IsNotFound(o.err) {
		t.Fatalf("error lost its class: %v", o.err)
	}
	if got := <-obs; got != [2]string{MethodContainerInspect, CodeNotFound} {
		t.Fatalf("observed %v", got)
	}
}

func TestCommandCallerCanceledTellsThePeer(t *testing.T) {
	tc := newTestChannel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := runCommand(ctx, tc, MethodPing, nil)
	f := tc.expectFrame(FrameCommand)
	cancel()

	o := waitOutcome(t, out)
	if !errors.Is(o.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", o.err)
	}
	if c := tc.expectFrame(FrameCancel); c.Cancel != f.Cmd.ID {
		t.Fatalf("cancel frame for id %d, want %d", c.Cancel, f.Cmd.ID)
	}
}

func TestCommandChannelClosedIsUnavailable(t *testing.T) {
	tc := newTestChannel(t)
	out := runCommand(context.Background(), tc, MethodPing, nil)
	tc.expectFrame(FrameCommand)
	tc.cancel()

	o := waitOutcome(t, out)
	if !cerrdefs.IsUnavailable(o.err) {
		t.Fatalf("err = %v, want unavailable", o.err)
	}
}

func TestCommandWriteFailureIsUnavailable(t *testing.T) {
	tc := newTestChannel(t)
	_ = tc.ws.CloseNow()
	_, err := tc.conn.Command(context.Background(), MethodPing, nil)
	if !cerrdefs.IsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

func TestCommandRejectsUnmarshalableParams(t *testing.T) {
	tc := newTestChannel(t)
	if _, err := tc.conn.Command(context.Background(), MethodPing, make(chan int)); err == nil {
		t.Fatal("a func/chan param must fail before it reaches the wire")
	}
}

func TestDeliverResultRouting(t *testing.T) {
	c := NewConn(context.Background(), nil)
	c.DeliverResult(nil)              // nil frame: ignored
	c.DeliverResult(&Result{ID: 999}) // unknown id: ignored
	id, cl := c.start(false)
	c.DeliverResult(&Result{ID: id, Body: json.RawMessage(`"first"`)})
	c.DeliverResult(&Result{ID: id, Body: json.RawMessage(`"duplicate"`)})
	res := <-cl.res
	if string(res.Body) != `"first"` {
		t.Fatalf("body = %s, want the first result to win", res.Body)
	}
	select {
	case extra := <-cl.res:
		t.Fatalf("duplicate result was queued: %+v", extra)
	default:
	}
	c.finish(id)
	if c.lookup(id) != nil {
		t.Fatal("finished call still routable")
	}
}

func TestDeliverChunkIgnoresUnknownAndUnary(t *testing.T) {
	c := NewConn(context.Background(), nil)
	c.DeliverChunk(nil)                                     // nil frame: ignored
	c.DeliverChunk(&StreamChunk{ID: 999})                   // unknown id: ignored
	id, _ := c.start(false)                                 // unary call: no chunk channel
	c.DeliverChunk(&StreamChunk{ID: id, Data: []byte("x")}) // must not panic
}

func TestDeliverChunkOverflowKillsTheStream(t *testing.T) {
	tc := newTestChannel(t)
	id, cl := tc.conn.start(true)
	for i := 0; i < StreamBuffer; i++ {
		tc.conn.DeliverChunk(&StreamChunk{ID: id, Data: []byte("d")})
	}
	tc.conn.DeliverChunk(&StreamChunk{ID: id, Data: []byte("overflow")})

	// The peer is told to stop the stream it can no longer deliver.
	if f := tc.expectFrame(FrameCancel); f.Cancel != id {
		t.Fatalf("cancel frame for id %d, want %d", f.Cancel, id)
	}
	// One chunk was shed to make room for the explicit error.
	var last StreamChunk
	count := 0
	for {
		select {
		case chunk := <-cl.chunks:
			last = chunk
			count++
			continue
		default:
		}
		break
	}
	if count != StreamBuffer {
		t.Fatalf("buffered chunks = %d, want %d", count, StreamBuffer)
	}
	if last.Err == nil || last.Err.Code != CodeUnavailable {
		t.Fatalf("last chunk = %+v, want the slow-consumer error", last)
	}
}

func TestStreamRoundTrip(t *testing.T) {
	tc := newTestChannel(t)
	type streamOutcome struct {
		rc  io.ReadCloser
		err error
	}
	out := make(chan streamOutcome, 1)
	go func() {
		rc, err := tc.conn.Stream(context.Background(), MethodContainerLogs, ContainerLogsParams{Name: "web"})
		out <- streamOutcome{rc, err}
	}()
	f := tc.expectFrame(FrameCommand)
	if f.Cmd.Method != MethodContainerLogs {
		t.Fatalf("method = %q", f.Cmd.Method)
	}
	tc.ack(f.Cmd.ID)
	var o streamOutcome
	select {
	case o = <-out:
	case <-time.After(5 * time.Second):
		t.Fatal("stream open never returned")
	}
	if o.err != nil {
		t.Fatalf("stream: %v", o.err)
	}
	tc.reply(Frame{Type: FrameStream, Chunk: &StreamChunk{ID: f.Cmd.ID, Data: []byte("log ")}})
	tc.reply(Frame{Type: FrameStream, Chunk: &StreamChunk{ID: f.Cmd.ID, Data: []byte("line")}})
	tc.reply(Frame{Type: FrameStream, Chunk: &StreamChunk{ID: f.Cmd.ID, EOF: true}})

	data, err := io.ReadAll(o.rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "log line" {
		t.Fatalf("data = %q", data)
	}
	if err := o.rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A stream that ended in EOF must NOT cancel the command on Close: the
	// next frame the peer sees is the next command, not a stray cancel.
	done := runCommand(context.Background(), tc, MethodPing, nil)
	next := tc.expectFrame(FrameCommand)
	tc.ack(next.Cmd.ID)
	waitOutcome(t, done)
}

func TestStreamChunkErrorKeepsItsClass(t *testing.T) {
	tc := newTestChannel(t)
	out := make(chan cmdOutcome, 1)
	var rc io.ReadCloser
	go func() {
		var err error
		rc2, err := tc.conn.Stream(context.Background(), MethodImagePull, ImagePullParams{Ref: "img"})
		rc = rc2
		out <- cmdOutcome{err: err}
	}()
	f := tc.expectFrame(FrameCommand)
	tc.ack(f.Cmd.ID)
	if o := waitOutcome(t, out); o.err != nil {
		t.Fatalf("stream: %v", o.err)
	}
	tc.reply(Frame{Type: FrameStream, Chunk: &StreamChunk{ID: f.Cmd.ID, Err: &Error{Code: CodeNotFound, Message: "manifest unknown"}}})
	_, err := io.ReadAll(rc)
	if !cerrdefs.IsNotFound(err) {
		t.Fatalf("read err = %v, want not found", err)
	}
	// A broken stream cancels the command on Close.
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if c := tc.expectFrame(FrameCancel); c.Cancel != f.Cmd.ID {
		t.Fatalf("cancel for %d, want %d", c.Cancel, f.Cmd.ID)
	}
}

func TestStreamCloseBeforeEOFCancelsRemote(t *testing.T) {
	tc := newTestChannel(t)
	out := make(chan cmdOutcome, 1)
	var rc io.ReadCloser
	go func() {
		rc2, err := tc.conn.Stream(context.Background(), MethodEvents, nil)
		rc = rc2
		out <- cmdOutcome{err: err}
	}()
	f := tc.expectFrame(FrameCommand)
	tc.ack(f.Cmd.ID)
	if o := waitOutcome(t, out); o.err != nil {
		t.Fatalf("stream: %v", o.err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if c := tc.expectFrame(FrameCancel); c.Cancel != f.Cmd.ID {
		t.Fatalf("cancel for %d, want %d", c.Cancel, f.Cmd.ID)
	}
	// Close is idempotent: no second cancel rides the wire.
	if err := rc.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	done := runCommand(context.Background(), tc, MethodPing, nil)
	next := tc.expectFrame(FrameCommand)
	tc.ack(next.Cmd.ID)
	waitOutcome(t, done)
}

// TestOpenFailuresBeforeAck drives Stream and Attach through every way the
// open can fail before the acknowledging result: an error result, the
// caller's cancellation, the channel's end, and a dead socket.
func TestOpenFailuresBeforeAck(t *testing.T) {
	openers := []struct {
		name string
		open func(tc *testChannel, ctx context.Context) error
	}{
		{"stream", func(tc *testChannel, ctx context.Context) error {
			_, err := tc.conn.Stream(ctx, MethodContainerLogs, nil)
			return err
		}},
		{"attach", func(tc *testChannel, ctx context.Context) error {
			_, err := tc.conn.Attach(ctx, MethodContainerExecAttach, nil)
			return err
		}},
	}
	for _, op := range openers {
		t.Run(op.name+" error result", func(t *testing.T) {
			tc := newTestChannel(t)
			errCh := make(chan error, 1)
			go func() { errCh <- op.open(tc, context.Background()) }()
			f := tc.expectFrame(FrameCommand)
			tc.reply(Frame{Type: FrameResult, Res: &Result{ID: f.Cmd.ID, Err: &Error{Code: CodeConflict, Message: "busy"}}})
			if err := <-errCh; !cerrdefs.IsConflict(err) {
				t.Fatalf("err = %v, want conflict", err)
			}
		})
		t.Run(op.name+" caller canceled", func(t *testing.T) {
			tc := newTestChannel(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- op.open(tc, ctx) }()
			f := tc.expectFrame(FrameCommand)
			cancel()
			if err := <-errCh; !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if c := tc.expectFrame(FrameCancel); c.Cancel != f.Cmd.ID {
				t.Fatalf("cancel for %d, want %d", c.Cancel, f.Cmd.ID)
			}
		})
		t.Run(op.name+" channel closed", func(t *testing.T) {
			tc := newTestChannel(t)
			errCh := make(chan error, 1)
			go func() { errCh <- op.open(tc, context.Background()) }()
			tc.expectFrame(FrameCommand)
			tc.cancel()
			if err := <-errCh; !cerrdefs.IsUnavailable(err) {
				t.Fatalf("err = %v, want unavailable", err)
			}
		})
		t.Run(op.name+" write failed", func(t *testing.T) {
			tc := newTestChannel(t)
			_ = tc.ws.CloseNow()
			if err := op.open(tc, context.Background()); !cerrdefs.IsUnavailable(err) {
				t.Fatalf("err = %v, want unavailable", err)
			}
		})
		t.Run(op.name+" bad params", func(t *testing.T) {
			tc := newTestChannel(t)
			var err error
			if op.name == "stream" {
				_, err = tc.conn.Stream(context.Background(), MethodContainerLogs, make(chan int))
			} else {
				_, err = tc.conn.Attach(context.Background(), MethodContainerExecAttach, make(chan int))
			}
			if err == nil {
				t.Fatal("unmarshalable params must fail before the wire")
			}
		})
	}
}

func TestAttachRoundTrip(t *testing.T) {
	tc := newTestChannel(t)
	out := make(chan cmdOutcome, 1)
	var att *Attached
	go func() {
		a, err := tc.conn.Attach(context.Background(), MethodContainerExecAttach, ContainerExecAttachParams{ExecID: "e1"})
		att = a
		out <- cmdOutcome{err: err}
	}()
	f := tc.expectFrame(FrameCommand)
	if f.Cmd.Method != MethodContainerExecAttach {
		t.Fatalf("method = %q", f.Cmd.Method)
	}
	tc.ack(f.Cmd.ID)
	if o := waitOutcome(t, out); o.err != nil {
		t.Fatalf("attach: %v", o.err)
	}

	// Writes split at ChunkSize and travel as input chunks under the same id.
	payload := bytes.Repeat([]byte("a"), ChunkSize+3)
	n, err := att.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("write = %d, %v", n, err)
	}
	first := tc.expectFrame(FrameStream)
	if first.Chunk.ID != f.Cmd.ID || len(first.Chunk.Data) != ChunkSize {
		t.Fatalf("first input chunk = id %d, %d bytes", first.Chunk.ID, len(first.Chunk.Data))
	}
	second := tc.expectFrame(FrameStream)
	if len(second.Chunk.Data) != 3 {
		t.Fatalf("second input chunk = %d bytes", len(second.Chunk.Data))
	}

	// CloseWrite marks stdin closed without ending reads.
	if err := att.CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}
	if eof := tc.expectFrame(FrameStream); !eof.Chunk.EOF {
		t.Fatalf("expected an EOF input chunk, got %+v", eof.Chunk)
	}

	tc.reply(Frame{Type: FrameStream, Chunk: &StreamChunk{ID: f.Cmd.ID, Data: []byte("pong")}})
	tc.reply(Frame{Type: FrameStream, Chunk: &StreamChunk{ID: f.Cmd.ID, EOF: true}})
	got, err := io.ReadAll(att)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("output = %q", got)
	}
	if err := att.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestAttachedWriteFailsOnDeadSocket(t *testing.T) {
	tc := newTestChannel(t)
	out := make(chan cmdOutcome, 1)
	var att *Attached
	go func() {
		a, err := tc.conn.Attach(context.Background(), MethodContainerExecAttach, nil)
		att = a
		out <- cmdOutcome{err: err}
	}()
	f := tc.expectFrame(FrameCommand)
	tc.ack(f.Cmd.ID)
	if o := waitOutcome(t, out); o.err != nil {
		t.Fatalf("attach: %v", o.err)
	}
	_ = tc.ws.CloseNow()
	if n, err := att.Write([]byte("in")); err == nil {
		t.Fatalf("write on a dead socket returned %d, nil", n)
	}
	if err := att.CloseWrite(); err == nil {
		t.Fatal("close write on a dead socket must fail")
	}
}

// TestStreamReadBuffering exercises the io.Reader adaptation directly: partial
// reads drain the buffered chunk before the next one is taken, an EOF chunk
// delivers its data before the EOF, and the terminal error is sticky.
func TestStreamReadBuffering(t *testing.T) {
	c := NewConn(context.Background(), nil)
	id, cl := c.start(true)
	s := &stream{conn: c, id: id, call: cl, ctx: context.Background()}

	cl.chunks <- StreamChunk{ID: id, Data: []byte("abcd")}
	cl.chunks <- StreamChunk{ID: id, Data: []byte("e"), EOF: true}

	buf := make([]byte, 2)
	var got []byte
	for {
		n, err := s.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if string(got) != "abcde" {
		t.Fatalf("data = %q", got)
	}
	if _, err := s.Read(buf); err != io.EOF {
		t.Fatalf("terminal error not sticky: %v", err)
	}
	// EOF + no cancellation makes Close safe without a socket.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestStreamReadCallerCanceled(t *testing.T) {
	c := NewConn(context.Background(), nil)
	id, cl := c.start(true)
	ctx, cancel := context.WithCancel(context.Background())
	s := &stream{conn: c, id: id, call: cl, ctx: ctx}
	cancel()
	if _, err := s.Read(make([]byte, 4)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// The caller's own cancellation needs no remote cancel on Close.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestStreamReadChannelClosed(t *testing.T) {
	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	c := NewConn(ownerCtx, nil)
	id, cl := c.start(true)
	s := &stream{conn: c, id: id, call: cl, ctx: context.Background()}
	ownerCancel()
	if _, err := s.Read(make([]byte, 4)); !cerrdefs.IsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

func TestIsStreamMethod(t *testing.T) {
	streaming := []string{MethodContainerLogs, MethodImagePull, MethodImagePush, MethodEvents, MethodImageBuild}
	for _, m := range streaming {
		if !IsStreamMethod(m) {
			t.Errorf("%s must be a stream method", m)
		}
	}
	// ContainerExecAttach is BIDIRECTIONAL, not a plain output stream — a
	// relay bridges it differently, so it must stay out of this list.
	for _, m := range []string{MethodPing, MethodContainerInspect, MethodContainerExecAttach, ""} {
		if IsStreamMethod(m) {
			t.Errorf("%s must not be a stream method", m)
		}
	}
}

// readThenFail returns its payload and the error in one Read call, the way a
// real pipe often ends.
type readThenFail struct {
	data []byte
	err  error
	done bool
}

func (r *readThenFail) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.data)
	return n, r.err
}

func TestPumpReaderCleanEOF(t *testing.T) {
	var frames []Frame
	write := func(f Frame) error { frames = append(frames, f); return nil }
	PumpReader(context.Background(), 7, strings.NewReader("hello"), write)
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want data + terminal", len(frames))
	}
	if string(frames[0].Chunk.Data) != "hello" {
		t.Fatalf("data chunk = %q", frames[0].Chunk.Data)
	}
	last := frames[1].Chunk
	if !last.EOF || last.Err != nil || last.ID != 7 {
		t.Fatalf("terminal chunk = %+v, want clean EOF", last)
	}
}

func TestPumpReaderDaemonError(t *testing.T) {
	var frames []Frame
	write := func(f Frame) error { frames = append(frames, f); return nil }
	r := &readThenFail{data: []byte("partial"), err: fmt.Errorf("logs: %w", cerrdefs.ErrConflict)}
	PumpReader(context.Background(), 3, r, write)
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want data + terminal", len(frames))
	}
	last := frames[1].Chunk
	if last.EOF || last.Err == nil || last.Err.Code != CodeConflict {
		t.Fatalf("terminal chunk = %+v, want a conflict error", last)
	}
}

func TestPumpReaderCanceledReportsSilence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var frames []Frame
	write := func(f Frame) error { frames = append(frames, f); return nil }
	PumpReader(ctx, 5, &readThenFail{err: errors.New("read after cancel")}, write)
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want the terminal only", len(frames))
	}
	last := frames[0].Chunk
	if !last.EOF || last.Err != nil {
		t.Fatalf("terminal chunk = %+v: a canceled pump reports EOF, not a daemon error", last)
	}
}

func TestPumpReaderStopsOnWriteFailure(t *testing.T) {
	calls := 0
	write := func(Frame) error { calls++; return errors.New("socket gone") }
	PumpReader(context.Background(), 9, strings.NewReader("data that would keep flowing"), write)
	if calls != 1 {
		t.Fatalf("write calls = %d, want the pump to stop at the first failure", calls)
	}
}
