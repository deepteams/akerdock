package tunnel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// fakeConn is an in-memory Conn: frames written by the bridge land on out,
// frames the test injects arrive on in.
type fakeConn struct {
	in  chan frame
	out chan frame
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
	cp := append([]byte(nil), data...)
	select {
	case f.out <- frame{typ, cp}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeConn) Ping(context.Context) error { return nil }

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
