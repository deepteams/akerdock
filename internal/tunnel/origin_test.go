package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// pipeConn is an in-memory Conn pair for wiring an Origin to a Bridge without a
// real WebSocket: whatever one side writes, the other reads, framed by type.
type pipeConn struct {
	in     <-chan frame
	out    chan<- frame
	closed chan struct{}
}

func newPipe() (*pipeConn, *pipeConn) {
	a2b := make(chan frame, 64)
	b2a := make(chan frame, 64)
	closed := make(chan struct{})
	a := &pipeConn{in: b2a, out: a2b, closed: closed}
	b := &pipeConn{in: a2b, out: b2a, closed: closed}
	return a, b
}

func (p *pipeConn) Read(ctx context.Context) (MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-p.closed:
		return 0, nil, ErrClientClosed
	case f, ok := <-p.in:
		if !ok {
			return 0, nil, ErrClientClosed
		}
		return f.typ, f.data, nil
	}
}

func (p *pipeConn) Write(ctx context.Context, typ MessageType, data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closed:
		return io.ErrClosedPipe
	case p.out <- frame{typ, cp}:
		return nil
	}
}

func (p *pipeConn) Ping(context.Context) error { return nil }

type originOpenResult struct {
	conn net.Conn
	err  error
}

func openOriginTestStream(ctx context.Context, t *testing.T, origin *Origin, fc *fakeConn) (net.Conn, ctrl) {
	t.Helper()
	result := make(chan originOpenResult, 1)
	go func() {
		conn, err := origin.OpenStream(ctx)
		result <- originOpenResult{conn: conn, err: err}
	}()
	open := waitCtrl(t, fc)
	if open.T != "open" {
		t.Fatalf("control = %+v, want open", open)
	}
	ok, _ := json.Marshal(ctrl{T: "open_ok", ID: open.ID})
	fc.in <- frame{MessageText, ok}
	got := <-result
	if got.err != nil {
		t.Fatalf("OpenStream: %v", got.err)
	}
	return got.conn, open
}

func waitOriginQueued(t *testing.T, origin *Origin, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		origin.admissionMu.Lock()
		got := origin.queued
		origin.admissionMu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queued streams did not reach %d", want)
}

// TestOriginBridgeRoundTrip wires an Origin (agent side) to a Bridge (laptop
// side) through the pipe, then opens a stream and checks bytes flow both ways
// through a real local echo listener — the exact shape of the ingress relay.
func TestOriginBridgeRoundTrip(t *testing.T) {
	// A local echo server stands in for the developer's app.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 1024)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						// echo, uppercased so we know it round-tripped
						out := make([]byte, n)
						for i := 0; i < n; i++ {
							b := buf[i]
							if b >= 'a' && b <= 'z' {
								b -= 32
							}
							out[i] = b
						}
						_, _ = conn.Write(out)
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()

	originConn, bridgeConn := newPipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	origin := NewOrigin(originConn)
	go origin.Run(ctx, Options{})

	// The Bridge (laptop) dials the echo server for every opened stream.
	dial := func(context.Context) (net.Conn, error) {
		return net.Dial("tcp", ln.Addr().String())
	}
	go Bridge(ctx, bridgeConn, dial, Options{})

	stream, err := origin.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := stream.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 5)
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "HELLO" {
		t.Fatalf("round trip got %q, want HELLO", got)
	}
}

// A burst beyond the active-stream bound waits locally at Origin. Closing an
// active stream sends its EOF before admitting the replacement, so Bridge has
// released the matching target by the time the next open arrives.
func TestOriginQueuesUntilAnActiveStreamCloses(t *testing.T) {
	fc := newFakeConn()
	origin := NewOriginWithOptions(fc, Options{
		MaxStreams: 1, MaxPendingStreams: 1, StreamQueueTimeout: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go origin.Run(ctx, Options{})

	first, firstOpen := openOriginTestStream(ctx, t, origin, fc)
	secondResult := make(chan originOpenResult, 1)
	go func() {
		conn, err := origin.OpenStream(ctx)
		secondResult <- originOpenResult{conn: conn, err: err}
	}()
	waitOriginQueued(t, origin, 1)
	select {
	case unexpected := <-fc.out:
		t.Fatalf("queued stream wrote frame before a slot opened: %+v", unexpected)
	default:
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if eof := waitCtrl(t, fc); eof.T != "eof" || eof.ID != firstOpen.ID {
		t.Fatalf("first close = %+v, want eof for %d", eof, firstOpen.ID)
	}
	secondOpen := waitCtrl(t, fc)
	if secondOpen.T != "open" {
		t.Fatalf("queued control = %+v, want open", secondOpen)
	}
	ok, _ := json.Marshal(ctrl{T: "open_ok", ID: secondOpen.ID})
	fc.in <- frame{MessageText, ok}
	second := <-secondResult
	if second.err != nil {
		t.Fatalf("queued OpenStream: %v", second.err)
	}
	_ = second.conn.Close()
}

func TestOriginQueueBoundAndCancellation(t *testing.T) {
	fc := newFakeConn()
	origin := NewOriginWithOptions(fc, Options{
		MaxStreams: 1, MaxPendingStreams: 1, StreamQueueTimeout: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go origin.Run(ctx, Options{})

	first, _ := openOriginTestStream(ctx, t, origin, fc)
	queuedCtx, cancelQueued := context.WithCancel(ctx)
	queuedResult := make(chan error, 1)
	go func() {
		_, err := origin.OpenStream(queuedCtx)
		queuedResult <- err
	}()
	waitOriginQueued(t, origin, 1)
	if _, err := origin.OpenStream(ctx); !errors.Is(err, ErrOriginQueueFull) {
		t.Fatalf("open beyond queue bound = %v, want ErrOriginQueueFull", err)
	}

	cancelQueued()
	if err := <-queuedResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled queued open = %v, want context.Canceled", err)
	}
	waitOriginQueued(t, origin, 0)
	_ = first.Close()
}

func TestOriginQueueTimeout(t *testing.T) {
	fc := newFakeConn()
	origin := NewOriginWithOptions(fc, Options{
		MaxStreams: 1, MaxPendingStreams: 1, StreamQueueTimeout: 20 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go origin.Run(ctx, Options{})

	first, _ := openOriginTestStream(ctx, t, origin, fc)
	if _, err := origin.OpenStream(ctx); !errors.Is(err, ErrOriginQueueTimeout) {
		t.Fatalf("timed-out queued open = %v, want ErrOriginQueueTimeout", err)
	}
	waitOriginQueued(t, origin, 0)
	_ = first.Close()
}

// TestOriginOpenStreamAfterClose ensures OpenStream fails once the session ends
// rather than hanging — a re-dial must get a clean error, not a stall.
func TestOriginOpenStreamAfterClose(t *testing.T) {
	originConn, _ := newPipe()
	origin := NewOrigin(originConn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { origin.Run(ctx, Options{}); close(done) }()
	cancel()
	<-done
	if _, err := origin.OpenStream(context.Background()); err != ErrOriginClosed {
		t.Fatalf("OpenStream after close: got %v, want ErrOriginClosed", err)
	}
}

// readCtrl reads one JSON control frame from a pipe end (the peer's view of what
// the Origin sent).
func readCtrl(t *testing.T, c *pipeConn) ctrl {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil || typ != MessageText {
		t.Fatalf("readCtrl: typ=%v err=%v", typ, err)
	}
	var m ctrl
	if json.Unmarshal(data, &m) != nil {
		t.Fatalf("readCtrl: bad json %q", data)
	}
	return m
}

func writeCtrl(t *testing.T, c *pipeConn, m ctrl) {
	t.Helper()
	data, _ := json.Marshal(m)
	if err := c.Write(context.Background(), MessageText, data); err != nil {
		t.Fatalf("writeCtrl: %v", err)
	}
}

// TestOriginOpenStreamRefused checks the peer answering open_err surfaces as an
// error from OpenStream, not a hang.
func TestOriginOpenStreamRefused(t *testing.T) {
	originConn, peer := newPipe()
	origin := NewOrigin(originConn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go origin.Run(ctx, Options{})

	errCh := make(chan error, 1)
	go func() {
		_, err := origin.OpenStream(ctx)
		errCh <- err
	}()

	open := readCtrl(t, peer)
	if open.T != "open" {
		t.Fatalf("expected open, got %q", open.T)
	}
	writeCtrl(t, peer, ctrl{T: "open_err", ID: open.ID, Code: "dial_failed", Msg: "nope"})

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("OpenStream should return the peer's refusal")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OpenStream hung on open_err")
	}
}

// TestOriginCancelReportsReason checks Run returns the reason pushed on the
// Cancel channel — the operator-cut / revoked path.
func TestOriginCancelReportsReason(t *testing.T) {
	originConn, _ := newPipe()
	origin := NewOrigin(originConn)
	cancelCh := make(chan EndReason, 1)
	done := make(chan EndReason, 1)
	go func() { done <- origin.Run(context.Background(), Options{Cancel: cancelCh}) }()

	cancelCh <- endReasonRevokedTest
	select {
	case r := <-done:
		if r != endReasonRevokedTest {
			t.Fatalf("Run returned %q, want %q", r, endReasonRevokedTest)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not honour the cancel reason")
	}
}

const endReasonRevokedTest EndReason = "revoked"

// TestOriginOpenStreamContextCancelled checks OpenStream returns when its own
// ctx is cancelled while waiting for the peer's open_ok — the stream is dropped,
// not leaked.
func TestOriginOpenStreamContextCancelled(t *testing.T) {
	originConn, peer := newPipe()
	origin := NewOrigin(originConn)
	go origin.Run(context.Background(), Options{})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := origin.OpenStream(ctx)
		errCh <- err
	}()
	// The peer reads the open but never answers; cancelling the caller's ctx
	// must unblock OpenStream.
	_ = readCtrl(t, peer)
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("OpenStream should fail when its context is cancelled")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OpenStream ignored context cancellation")
	}
}

// TestOriginIdleTimeout checks a session with no traffic ends on the idle timer.
func TestOriginIdleTimeout(t *testing.T) {
	originConn, _ := newPipe()
	origin := NewOrigin(originConn)
	done := make(chan EndReason, 1)
	go func() {
		done <- origin.Run(context.Background(), Options{IdleTimeout: 30 * time.Millisecond})
	}()
	select {
	case r := <-done:
		if r != EndIdleTimeout {
			t.Fatalf("got %q, want idle_timeout", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle timer never fired")
	}
}

// TestOriginRunReportsUserClose checks a clean hangup from the peer surfaces as
// user_close, not disconnect — the CLI prints this to the developer.
func TestOriginRunReportsUserClose(t *testing.T) {
	fc := newFakeConn()
	origin := NewOrigin(fc)
	done := make(chan EndReason, 1)
	go func() { done <- origin.Run(context.Background(), Options{}) }()
	close(fc.in)
	select {
	case r := <-done:
		if r != EndUserClose {
			t.Fatalf("end reason = %q, want user_close", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the peer hung up")
	}
}

func TestOriginMaxDuration(t *testing.T) {
	fc := newFakeConn()
	origin := NewOrigin(fc)
	done := make(chan EndReason, 1)
	go func() {
		done <- origin.Run(context.Background(), Options{MaxDuration: 30 * time.Millisecond})
	}()
	select {
	case r := <-done:
		if r != EndMaxDuration {
			t.Fatalf("end reason = %q, want max_duration", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("max duration timer never fired")
	}
}

func TestOriginEndsWhenPingFails(t *testing.T) {
	fc := newFakeConn()
	fc.pingErr = errWriteFailed
	origin := NewOrigin(fc)
	done := make(chan EndReason, 1)
	go func() {
		done <- origin.Run(context.Background(), Options{Heartbeat: 5 * time.Millisecond})
	}()
	select {
	case r := <-done:
		if r != EndDisconnect {
			t.Fatalf("end reason = %q, want disconnect after a failed ping", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run survived a dead WebSocket")
	}
}

func TestOriginStopsWhenTheDurableSessionWasClosed(t *testing.T) {
	fc := newFakeConn()
	origin := NewOrigin(fc)
	done := make(chan EndReason, 1)
	go func() {
		done <- origin.Run(context.Background(), Options{
			Heartbeat:   5 * time.Millisecond,
			OnHeartbeat: func(context.Context) bool { return false },
		})
	}()
	select {
	case r := <-done:
		if r != EndDisconnect {
			t.Fatalf("end reason = %q, want disconnect for an already-finalized row", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run remained open after its durable session was finalized")
	}
}

// Malformed frames (unparseable control JSON, a binary frame too short to
// carry a stream id) are ignored but still count as activity; the session then
// ends on the idle timer as if nothing had been received.
func TestOriginIdleTimeoutAfterIgnoredJunkFrames(t *testing.T) {
	fc := newFakeConn()
	origin := NewOrigin(fc)
	done := make(chan EndReason, 1)
	go func() {
		done <- origin.Run(context.Background(), Options{IdleTimeout: 150 * time.Millisecond})
	}()

	fc.in <- frame{MessageText, []byte("{not json")}
	fc.in <- frame{MessageBinary, []byte{0x00, 0x01}}

	select {
	case r := <-done:
		if r != EndIdleTimeout {
			t.Fatalf("end reason = %q, want idle_timeout", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle timer never fired")
	}
}

// TestOriginOpenStreamSurfacesSendFailure checks a dead socket fails the open
// immediately — the reverse proxy gets an error, not a 15-second stall.
func TestOriginOpenStreamSurfacesSendFailure(t *testing.T) {
	fc := newFakeConn()
	fc.failWrites = make(chan struct{})
	close(fc.failWrites)
	origin := NewOrigin(fc)
	if _, err := origin.OpenStream(context.Background()); err == nil {
		t.Fatal("OpenStream must fail when the open frame cannot be written")
	}
}

// TestOriginOpenStreamUnblocksWhenSessionEnds pins the done-channel arm of
// OpenStream's wait. Run is deliberately not started and the peer never
// answers, so closing done — exactly what shutdown does first — is the only
// way out; going through shutdown itself would also resolve the pending wait
// and make which arm fires a coin toss.
func TestOriginOpenStreamUnblocksWhenSessionEnds(t *testing.T) {
	originConn, peer := newPipe()
	origin := NewOrigin(originConn)

	errCh := make(chan error, 1)
	go func() {
		_, err := origin.OpenStream(context.Background())
		errCh <- err
	}()
	_ = readCtrl(t, peer) // the open travelled; the peer stays silent
	origin.doneOnce.Do(func() { close(origin.done) })

	select {
	case err := <-errCh:
		if err != ErrOriginClosed {
			t.Fatalf("got %v, want ErrOriginClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OpenStream ignored the session ending")
	}
}

// TestOriginShutdownTearsDownStreamsAndPendingOpens ends a session that has
// one live stream and one open still waiting for its answer: the stream's
// caller must read an error and the waiting open must resolve, not leak.
func TestOriginShutdownTearsDownStreamsAndPendingOpens(t *testing.T) {
	fc := newFakeConn()
	origin := NewOrigin(fc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan EndReason, 1)
	go func() { done <- origin.Run(ctx, Options{}) }()

	// First stream opens fully.
	streamCh := make(chan net.Conn, 1)
	go func() {
		s, err := origin.OpenStream(ctx)
		if err != nil {
			t.Errorf("OpenStream: %v", err)
		}
		streamCh <- s
	}()
	open := waitCtrl(t, fc)
	okMsg, _ := json.Marshal(ctrl{T: "open_ok", ID: open.ID})
	fc.in <- frame{MessageText, okMsg}
	stream := <-streamCh
	if stream == nil {
		return
	}

	// Second open goes out but the peer never answers.
	errCh := make(chan error, 1)
	go func() {
		_, err := origin.OpenStream(ctx)
		errCh <- err
	}()
	_ = waitCtrl(t, fc)

	// The peer hangs up: Run returns and shutdown must sweep both.
	close(fc.in)
	select {
	case r := <-done:
		if r != EndUserClose {
			t.Fatalf("end reason = %q, want user_close", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the peer hung up")
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("the pending open must resolve with an error at shutdown")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the pending open leaked past shutdown")
	}

	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := stream.Read(buf); err == nil {
		t.Fatal("the live stream must be closed at shutdown")
	}
}

// When the WebSocket dies mid-relay, the caller→peer pump must stop and tear
// its stream down instead of spinning on a dead socket.
func TestOriginStreamTeardownWhenTheSocketDies(t *testing.T) {
	fc := newFakeConn()
	fc.failWrites = make(chan struct{})
	origin := NewOrigin(fc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go origin.Run(ctx, Options{})

	streamCh := make(chan net.Conn, 1)
	go func() {
		s, err := origin.OpenStream(ctx)
		if err != nil {
			t.Errorf("OpenStream: %v", err)
		}
		streamCh <- s
	}()
	open := waitCtrl(t, fc)
	okMsg, _ := json.Marshal(ctrl{T: "open_ok", ID: open.ID})
	fc.in <- frame{MessageText, okMsg}
	stream := <-streamCh
	if stream == nil {
		return
	}

	// The socket dies; the next chunk the caller writes cannot be relayed and
	// the pump must close the stream, which the caller observes as a failing
	// read.
	close(fc.failWrites)
	if _, err := stream.Write([]byte("x")); err != nil {
		t.Fatalf("stream write: %v", err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := stream.Read(buf); err == nil {
		t.Fatal("the stream should be closed once its data cannot be relayed")
	}
}

// TestOriginPeerEofClosesStream checks a "close" from the peer tears the stream
// down so the caller reads EOF, exercising readLoop's eof/close branch.
func TestOriginPeerEofClosesStream(t *testing.T) {
	originConn, peer := newPipe()
	origin := NewOrigin(originConn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go origin.Run(ctx, Options{})

	streamCh := make(chan net.Conn, 1)
	go func() {
		s, err := origin.OpenStream(ctx)
		if err != nil {
			t.Errorf("OpenStream: %v", err)
			streamCh <- nil
			return
		}
		streamCh <- s
	}()
	open := readCtrl(t, peer)
	writeCtrl(t, peer, ctrl{T: "open_ok", ID: open.ID})
	stream := <-streamCh
	if stream == nil {
		return
	}

	// The peer closes the stream; the caller-facing conn must read EOF.
	writeCtrl(t, peer, ctrl{T: "close", ID: open.ID})
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := stream.Read(buf); err == nil {
		t.Fatal("expected the stream to close after the peer's close frame")
	}
}
