package tunnel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeConn is an in-memory Conn: frames written by the bridge land on out,
// frames the test injects arrive on in. pingErr (set before the session
// starts) makes every Ping fail; closing failWrites makes every later Write
// fail, standing in for a socket that died mid-session.
type fakeConn struct {
	in         chan frame
	out        chan frame
	pingErr    error
	failWrites chan struct{}
}

type frame struct {
	typ  MessageType
	data []byte
}

func newFakeConn() *fakeConn {
	return &fakeConn{in: make(chan frame, 64), out: make(chan frame, 64)}
}

func (f *fakeConn) Read(ctx context.Context) (MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case fr, ok := <-f.in:
		if !ok {
			return 0, nil, ErrClientClosed
		}
		return fr.typ, fr.data, nil
	}
}

func (f *fakeConn) Write(ctx context.Context, typ MessageType, data []byte) error {
	if f.failWrites != nil {
		select {
		case <-f.failWrites:
			return errWriteFailed
		default:
		}
	}
	cp := append([]byte(nil), data...)
	select {
	case f.out <- frame{typ, cp}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeConn) Ping(context.Context) error { return f.pingErr }

var errWriteFailed = errors.New("tunnel test: the socket is gone")

// TestBridgeStreamRoundTrip drives one stream through the mux: open → open_ok,
// bytes to the target and back, then client close.
func TestBridgeStreamRoundTrip(t *testing.T) {
	fc := newFakeConn()

	// The dialer hands the bridge one end of a pipe; the test drives the other,
	// standing in for the container.
	serverEnd, targetEnd := net.Pipe()
	dial := func(context.Context) (net.Conn, error) { return serverEnd, nil }

	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(context.Background(), fc, dial, Options{})
	}()

	// Client opens stream 1.
	openMsg, _ := json.Marshal(ctrl{T: "open", ID: 1})
	fc.in <- frame{MessageText, openMsg}

	// Expect open_ok.
	if got := waitCtrl(t, fc); got.T != "open_ok" || got.ID != 1 {
		t.Fatalf("want open_ok id=1, got %+v", got)
	}

	// Client → target.
	fc.in <- frame{MessageBinary, dataFrame(1, []byte("ping"))}
	buf := make([]byte, 4)
	_ = targetEnd.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := targetEnd.Read(buf); err != nil || string(buf) != "ping" {
		t.Fatalf("target read = %q, %v", buf, err)
	}

	// Target → client.
	_, _ = targetEnd.Write([]byte("pong"))
	if id, payload := waitData(t, fc); id != 1 || string(payload) != "pong" {
		t.Fatalf("client data = id %d %q", id, payload)
	}

	// Client closes the connection cleanly.
	close(fc.in)
	select {
	case reason := <-done:
		if reason != EndUserClose {
			t.Fatalf("end reason = %q, want user_close", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not return after client close")
	}
}

// A local target that stops reading may consume its bounded per-stream queue,
// but it must not block the socket decoder or a second target.
func TestBridgeStalledTargetDoesNotBlockSibling(t *testing.T) {
	fc := newFakeConn()
	stalledBridge, stalledTarget := net.Pipe()
	siblingBridge, siblingTarget := net.Pipe()
	defer func() { _ = stalledTarget.Close() }()
	defer func() { _ = siblingTarget.Close() }()

	var dialMu sync.Mutex
	dials := []net.Conn{stalledBridge, siblingBridge}
	dial := func(context.Context) (net.Conn, error) {
		dialMu.Lock()
		defer dialMu.Unlock()
		conn := dials[0]
		dials = dials[1:]
		return conn, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Bridge(ctx, fc, dial, Options{MaxStreams: 2})

	for _, id := range []uint32{1, 2} {
		open, _ := json.Marshal(ctrl{T: "open", ID: id})
		fc.in <- frame{MessageText, open}
		if got := waitCtrl(t, fc); got.T != "open_ok" || got.ID != id {
			t.Fatalf("open %d = %+v", id, got)
		}
	}

	payload := make([]byte, streamFramePayload)
	for range streamQueueChunks + 4 {
		fc.in <- frame{MessageBinary, dataFrame(1, payload)}
	}
	fc.in <- frame{MessageBinary, dataFrame(2, []byte("still-moving"))}

	_ = siblingTarget.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len("still-moving"))
	if _, err := io.ReadFull(siblingTarget, got); err != nil {
		t.Fatalf("sibling was blocked by stalled target: %v", err)
	}
	if string(got) != "still-moving" {
		t.Fatalf("sibling got %q", got)
	}
}

func dataFrame(id uint32, p []byte) []byte {
	f := make([]byte, 4+len(p))
	binary.BigEndian.PutUint32(f, id)
	copy(f[4:], p)
	return f
}

func waitCtrl(t *testing.T, fc *fakeConn) ctrl {
	t.Helper()
	for {
		select {
		case fr := <-fc.out:
			if fr.typ == MessageText {
				var c ctrl
				_ = json.Unmarshal(fr.data, &c)
				return c
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for a control frame")
		}
	}
}

func waitData(t *testing.T, fc *fakeConn) (uint32, []byte) {
	t.Helper()
	for {
		select {
		case fr := <-fc.out:
			if fr.typ == MessageBinary && len(fr.data) >= 4 {
				return binary.BigEndian.Uint32(fr.data[:4]), fr.data[4:]
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for a data frame")
		}
	}
}

// A tunnel cut from outside — a revoked grant, an operator closing it in the
// dashboard — must report WHY, not merely stop. The reason is what the CLI
// prints to the developer, and a socket that dies without a word is read as a
// bug in the platform rather than as the control it is (ADR-045 §5).
func TestBridgeCancelReportsTheReasonItWasGiven(t *testing.T) {
	fc := newFakeConn()
	serverEnd, _ := net.Pipe()
	dial := func(context.Context) (net.Conn, error) { return serverEnd, nil }

	cancel := make(chan EndReason, 1)
	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(context.Background(), fc, dial, Options{Cancel: cancel})
	}()

	cancel <- EndReason("revoked")

	select {
	case got := <-done:
		if got != EndReason("revoked") {
			t.Fatalf("end reason = %q, want the reason the canceller gave", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge ignored its cancel channel")
	}
}

// The zero Options must stay inert: a nil cancel channel never fires, which is
// what lets every existing caller keep passing Options{}.
func TestBridgeWithoutCancelChannelStillEndsNormally(t *testing.T) {
	fc := newFakeConn()
	serverEnd, _ := net.Pipe()
	dial := func(context.Context) (net.Conn, error) { return serverEnd, nil }

	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(context.Background(), fc, dial, Options{})
	}()

	close(fc.in) // the client hangs up
	select {
	case got := <-done:
		if got != EndUserClose {
			t.Fatalf("end reason = %q, want user_close", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not return after client close")
	}
}

func TestBridgePersistsSuccessfulHeartbeats(t *testing.T) {
	fc := newFakeConn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	heartbeat := make(chan struct{}, 1)
	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(ctx, fc, nil, Options{
			Heartbeat: 5 * time.Millisecond,
			OnHeartbeat: func(context.Context) EndReason {
				select {
				case heartbeat <- struct{}{}:
				default:
				}
				return ""
			},
		})
	}()

	select {
	case <-heartbeat:
	case <-time.After(time.Second):
		t.Fatal("successful WebSocket ping did not trigger its persistence hook")
	}
	cancel()
	select {
	case got := <-done:
		if got != EndDisconnect {
			t.Fatalf("end reason = %q, want disconnect after context cancellation", got)
		}
	case <-time.After(time.Second):
		t.Fatal("bridge did not stop after cancellation")
	}
}

// A session with no traffic must end on the idle timer, and malformed frames
// (unparseable control JSON, a binary frame too short to carry a stream id)
// must be ignored rather than kill the session — they still count as activity,
// which is what resets the idle timer before it finally fires.
func TestBridgeIdleTimeoutAfterIgnoredJunkFrames(t *testing.T) {
	fc := newFakeConn()
	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(context.Background(), fc, nil, Options{IdleTimeout: 150 * time.Millisecond})
	}()

	fc.in <- frame{MessageText, []byte("{not json")}
	fc.in <- frame{MessageBinary, []byte{0x00, 0x01}}

	select {
	case got := <-done:
		if got != EndIdleTimeout {
			t.Fatalf("end reason = %q, want idle_timeout", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle timer never fired")
	}
}

func TestBridgeMaxDuration(t *testing.T) {
	fc := newFakeConn()
	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(context.Background(), fc, nil, Options{MaxDuration: 30 * time.Millisecond})
	}()
	select {
	case got := <-done:
		if got != EndMaxDuration {
			t.Fatalf("end reason = %q, want max_duration", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("max duration timer never fired")
	}
}

func TestBridgeEndsWhenPingFails(t *testing.T) {
	fc := newFakeConn()
	fc.pingErr = errors.New("peer gone")
	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(context.Background(), fc, nil, Options{Heartbeat: 5 * time.Millisecond})
	}()
	select {
	case got := <-done:
		if got != EndDisconnect {
			t.Fatalf("end reason = %q, want disconnect after a failed ping", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge survived a dead WebSocket")
	}
}

// The stream cap is a hard bound (§24.4): the open above the limit is refused
// with a code the client can show, and the streams already open keep working.
func TestBridgeRefusesStreamsAboveTheLimit(t *testing.T) {
	fc := newFakeConn()
	serverEnd, _ := net.Pipe()
	dial := func(context.Context) (net.Conn, error) { return serverEnd, nil }

	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(context.Background(), fc, dial, Options{MaxStreams: 1})
	}()

	openMsg, _ := json.Marshal(ctrl{T: "open", ID: 1})
	fc.in <- frame{MessageText, openMsg}
	if got := waitCtrl(t, fc); got.T != "open_ok" || got.ID != 1 {
		t.Fatalf("want open_ok id=1, got %+v", got)
	}

	openMsg2, _ := json.Marshal(ctrl{T: "open", ID: 2})
	fc.in <- frame{MessageText, openMsg2}
	if got := waitCtrl(t, fc); got.T != "open_err" || got.ID != 2 || got.Code != "limit" {
		t.Fatalf("want open_err id=2 code=limit, got %+v", got)
	}

	close(fc.in)
	<-done
}

func TestBridgeReportsDialFailure(t *testing.T) {
	fc := newFakeConn()
	dial := func(context.Context) (net.Conn, error) { return nil, errors.New("no route to container") }

	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(context.Background(), fc, dial, Options{})
	}()

	openMsg, _ := json.Marshal(ctrl{T: "open", ID: 7})
	fc.in <- frame{MessageText, openMsg}
	got := waitCtrl(t, fc)
	if got.T != "open_err" || got.ID != 7 || got.Code != "dial_failed" {
		t.Fatalf("want open_err id=7 code=dial_failed, got %+v", got)
	}

	close(fc.in)
	<-done
}

// An "eof" from the client must close the target connection, not just forget
// the id — the container side is what would otherwise leak.
func TestBridgeClientEofClosesTheTarget(t *testing.T) {
	fc := newFakeConn()
	serverEnd, targetEnd := net.Pipe()
	dial := func(context.Context) (net.Conn, error) { return serverEnd, nil }

	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(context.Background(), fc, dial, Options{})
	}()

	openMsg, _ := json.Marshal(ctrl{T: "open", ID: 1})
	fc.in <- frame{MessageText, openMsg}
	if got := waitCtrl(t, fc); got.T != "open_ok" {
		t.Fatalf("want open_ok, got %+v", got)
	}

	eofMsg, _ := json.Marshal(ctrl{T: "eof", ID: 1})
	fc.in <- frame{MessageText, eofMsg}

	_ = targetEnd.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := targetEnd.Read(buf); err == nil {
		t.Fatal("the target end should be closed after the client's eof")
	}

	close(fc.in)
	<-done
}

// When the WebSocket dies mid-relay, the target→client pump must stop and tear
// its stream down instead of spinning on a dead socket.
func TestBridgeStreamTeardownWhenTheSocketDies(t *testing.T) {
	fc := newFakeConn()
	fc.failWrites = make(chan struct{})
	serverEnd, targetEnd := net.Pipe()
	dial := func(context.Context) (net.Conn, error) { return serverEnd, nil }

	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(context.Background(), fc, dial, Options{})
	}()

	openMsg, _ := json.Marshal(ctrl{T: "open", ID: 1})
	fc.in <- frame{MessageText, openMsg}
	if got := waitCtrl(t, fc); got.T != "open_ok" {
		t.Fatalf("want open_ok, got %+v", got)
	}

	// The socket dies; the next relayed chunk cannot be written and the pump
	// must close the target, which the test observes as a failing read.
	close(fc.failWrites)
	if _, err := targetEnd.Write([]byte("x")); err != nil {
		t.Fatalf("target write: %v", err)
	}
	_ = targetEnd.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := targetEnd.Read(buf); err == nil {
		t.Fatal("the target should be closed once its data cannot be relayed")
	}

	close(fc.in)
	<-done
}

// The bridge reports whatever the beat hands it, verbatim. Only the beat can
// read the row it just failed to update, so a session finalized on another
// replica as `target_stopped` or `grant_expired` must arrive at the developer
// with that word — `disconnect` is a network glitch and sends them to inspect
// their own laptop.
func TestBridgeStopsWithTheReasonTheBeatReports(t *testing.T) {
	for _, want := range []EndReason{EndDisconnect, "target_stopped", "grant_expired", "wake_failed"} {
		t.Run(string(want), func(t *testing.T) {
			fc := newFakeConn()
			done := make(chan EndReason, 1)
			go func() {
				done <- Bridge(context.Background(), fc, nil, Options{
					Heartbeat:   5 * time.Millisecond,
					OnHeartbeat: func(context.Context) EndReason { return want },
				})
			}()

			select {
			case got := <-done:
				if got != want {
					t.Fatalf("end reason = %q, want %q for a row finalized elsewhere", got, want)
				}
			case <-time.After(time.Second):
				t.Fatal("bridge remained open after its durable session was finalized")
			}
		})
	}
}
