package tunnel

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The attach key binds every data request to the one control request that
// consumed the mint. A key that repeated, or that was not a full 256 bits,
// would let a second laptop's data stream join this session.
func TestNewIngressAttachKeyIsFreshAndFullWidth(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		key, err := NewIngressAttachKey()
		if err != nil {
			t.Fatal(err)
		}
		raw, decodeErr := base64.RawURLEncoding.DecodeString(key)
		if decodeErr != nil || len(raw) != 32 {
			t.Fatalf("key %q decodes to %d bytes: %v", key, len(raw), decodeErr)
		}
		if seen[key] {
			t.Fatal("attach keys must never repeat")
		}
		seen[key] = true
	}
}

// A control frame is bounded on purpose: the reader holds one buffer for the
// whole session, and an unbounded line is a peer that can grow it at will.
func TestLineControlRejectsUnboundedFrame(t *testing.T) {
	oversized := `{"t":"open","msg":"` + strings.Repeat("a", maxControlFrame) + `"}` + "\n"
	control := NewLineControl(strings.NewReader(oversized), io.Discard, nil, nil)
	if _, err := control.Receive(); err == nil || !strings.Contains(err.Error(), "16 KiB") {
		t.Fatalf("oversized frame = %v, want the bound spelled out", err)
	}
}

func TestLineControlRejectsMalformedFrames(t *testing.T) {
	for name, wire := range map[string]string{
		"not json":  "{oops\n",
		"no type":   `{"id":3}` + "\n",
		"truncated": `{"t":"ping"}`, // no newline: the peer went away mid-frame
	} {
		t.Run(name, func(t *testing.T) {
			control := NewLineControl(strings.NewReader(wire), io.Discard, nil, nil)
			if frame, err := control.Receive(); err == nil {
				t.Fatalf("accepted %+v", frame)
			}
		})
	}
}

// Send must not write half a frame after the session is gone, and must flush
// what it wrote: the control stream stays open for the whole tunnel, so an
// unflushed ping is a heartbeat the peer never sees.
func TestLineControlSendHonoursContextAndFlushes(t *testing.T) {
	var out strings.Builder
	var flushes atomic.Int64
	control := NewLineControl(strings.NewReader(""), &out, func() error {
		flushes.Add(1)
		return nil
	}, nil)

	if err := control.Send(context.Background(), HTTPControlFrame{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"t":"ping"}`+"\n" || flushes.Load() != 1 {
		t.Fatalf("wire = %q, flushes = %d", out.String(), flushes.Load())
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := control.Send(cancelled, HTTPControlFrame{Type: "pong"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("send on a cancelled context = %v", err)
	}
	if strings.Contains(out.String(), "pong") {
		t.Fatal("a cancelled send must not reach the wire")
	}
}

func TestLineControlCloseRunsOnce(t *testing.T) {
	var closes atomic.Int64
	control := NewLineControl(strings.NewReader(""), io.Discard, nil, func() error {
		closes.Add(1)
		return errors.New("already gone")
	})
	first := control.Close()
	if first == nil || control.Close() != first {
		t.Fatal("Close must memoize its result")
	}
	if closes.Load() != 1 {
		t.Fatalf("closed %d times, want once", closes.Load())
	}
}

// The terminal reason travels on the control stream before the response ends:
// it is the one thing the developer must get out of an automatic close, and
// the CLI decides re-dial versus exit on it (ADR-045 §5).
func TestHTTPOriginSendsTerminalReasonBeforeClosing(t *testing.T) {
	agentControl, clientControl := newControlPair(t)
	origin := NewHTTPOrigin(agentControl, Options{})
	go func() {
		_ = origin.SendClose(context.Background(), EndIdleTimeout)
		_ = origin.Close()
	}()
	frame, err := clientControl.Receive()
	if err != nil || frame.Type != "session_close" || frame.Reason != string(EndIdleTimeout) {
		t.Fatalf("frame = %+v, %v", frame, err)
	}
}

// Once the session is over, an in-flight visitor request must be refused
// outright rather than wait out the open timeout on a peer that is gone.
func TestHTTPOriginRefusesStreamsAfterShutdown(t *testing.T) {
	agentControl, clientControl := newControlPair(t)
	origin := NewHTTPOrigin(agentControl, Options{MaxStreams: 1})
	ended := make(chan EndReason, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { ended <- origin.Run(ctx, Options{}) }()

	if err := clientControl.Send(ctx, HTTPControlFrame{Type: "session_close", Reason: "revoked"}); err != nil {
		t.Fatal(err)
	}
	if reason := <-ended; reason != EndReason("revoked") {
		t.Fatalf("end reason = %q", reason)
	}
	if _, err := origin.OpenStream(ctx); !errors.Is(err, ErrOriginClosed) {
		t.Fatalf("open after shutdown = %v, want ErrOriginClosed", err)
	}
	// The slot the closed session held must be back: a shutdown that leaked
	// admission would wedge the next session at zero available streams.
	if len(origin.streamSlots) != 0 {
		t.Fatalf("%d stream slots still held after shutdown", len(origin.streamSlots))
	}
}

// A visitor that gives up before the laptop answers must release its slot,
// otherwise every abandoned page load permanently shrinks the tunnel.
func TestHTTPOriginReleasesSlotWhenTheVisitorGivesUp(t *testing.T) {
	agentControl, clientControl := newControlPair(t)
	origin := NewHTTPOrigin(agentControl, Options{MaxStreams: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go origin.Run(ctx, Options{})

	abandoned, abandon := context.WithCancel(ctx)
	failed := make(chan error, 1)
	go func() {
		_, err := origin.OpenStream(abandoned)
		failed <- err
	}()
	if frame, err := clientControl.Receive(); err != nil || frame.Type != "open" {
		t.Fatalf("open frame = %+v, %v", frame, err)
	}
	abandon()
	if err := <-failed; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned open = %v", err)
	}

	// The slot is free again: the next visitor is served rather than queued.
	served := make(chan error, 1)
	go func() {
		_, err := origin.OpenStream(ctx)
		served <- err
	}()
	next, err := clientControl.Receive()
	if err != nil || next.Type != "open" {
		t.Fatalf("second open frame = %+v, %v", next, err)
	}
	agentSide, clientSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()
	if err := origin.AttachStream(next.ID, agentSide); err != nil {
		t.Fatalf("AttachStream: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("second open = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the released slot was never reused")
	}
}
