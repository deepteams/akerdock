package tunnel

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// The session bounds of §24.4 applied to a client-initiated attach: the tunnel
// must end on its own timers, with the reason the CLI decides re-dial versus
// exit on, and not merely when the peer happens to hang up.
func TestHTTPSessionEnforcesItsBounds(t *testing.T) {
	t.Run("idle timeout", func(t *testing.T) {
		agentControl, _ := newControlPair(t)
		session := NewHTTPSession(agentControl, Options{})
		if reason := session.Run(context.Background(), Options{
			IdleTimeout: 20 * time.Millisecond,
			MaxDuration: time.Minute,
			Heartbeat:   time.Minute,
		}); reason != EndIdleTimeout {
			t.Fatalf("reason = %q, want %q", reason, EndIdleTimeout)
		}
	})

	t.Run("max duration", func(t *testing.T) {
		agentControl, _ := newControlPair(t)
		session := NewHTTPSession(agentControl, Options{})
		if reason := session.Run(context.Background(), Options{
			IdleTimeout: time.Minute,
			MaxDuration: 20 * time.Millisecond,
			Heartbeat:   time.Minute,
		}); reason != EndMaxDuration {
			t.Fatalf("reason = %q, want %q", reason, EndMaxDuration)
		}
	})

	t.Run("operator cut", func(t *testing.T) {
		agentControl, _ := newControlPair(t)
		session := NewHTTPSession(agentControl, Options{})
		cut := make(chan EndReason, 1)
		cut <- EndReason("revoked")
		if reason := session.Run(context.Background(), Options{Cancel: cut}); reason != EndReason("revoked") {
			t.Fatalf("reason = %q, want the cut's own reason", reason)
		}
	})

	// A heartbeat proves the path is alive while no stream carries a byte —
	// most of a port-forward's life. A storage layer that says the session is
	// already closed cuts the tunnel rather than keeping an orphan alive.
	t.Run("heartbeat", func(t *testing.T) {
		agentControl, clientControl := newControlPair(t)
		session := NewHTTPSession(agentControl, Options{})
		// The control pair is a synchronous pipe: keep draining it, or the
		// second ping blocks on a reader that never comes.
		pings := make(chan HTTPControlFrame, 8)
		go func() {
			for {
				frame, err := clientControl.Receive()
				if err != nil {
					close(pings)
					return
				}
				pings <- frame
			}
		}()
		var beats atomic.Int64
		ended := make(chan EndReason, 1)
		go func() {
			ended <- session.Run(context.Background(), Options{
				IdleTimeout: time.Minute,
				MaxDuration: time.Minute,
				Heartbeat:   5 * time.Millisecond,
				// The second beat is the one that finds the row finalized —
				// and it names what finalized it. The rung must carry that
				// word out rather than substituting `disconnect`, which reads
				// as the developer's own connection dropping.
				OnHeartbeat: func(context.Context) EndReason {
					if beats.Add(1) < 2 {
						return ""
					}
					return EndReason("target_stopped")
				},
			})
		}()
		if frame, ok := <-pings; !ok || frame.Type != "ping" {
			t.Fatalf("frame = %+v, delivered = %v", frame, ok)
		}
		select {
		case reason := <-ended:
			if reason != EndReason("target_stopped") {
				t.Fatalf("reason = %q, want the beat's own word once the durable session is gone", reason)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a refused heartbeat must end the session")
		}
	})
}

// Admission is answered, never queued: an egress stream is a TCP connection a
// local client is already holding, and a client told "no" reconnects while a
// client left waiting stalls.
func TestHTTPSessionRefusesBeyondItsAdmissionBound(t *testing.T) {
	agentControl, _ := newControlPair(t)
	session := NewHTTPSession(agentControl, Options{MaxStreams: 2})

	first, firstPeer := net.Pipe()
	defer func() { _ = firstPeer.Close() }()
	second, secondPeer := net.Pipe()
	defer func() { _ = secondPeer.Close() }()
	admittedFirst, err := session.Admit(first)
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if _, err := session.Admit(second); err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if got := session.Streams(); got != 2 {
		t.Fatalf("attached streams = %d, want 2", got)
	}

	third, thirdPeer := net.Pipe()
	defer func() { _ = third.Close(); _ = thirdPeer.Close() }()
	refusedAt := time.Now()
	if _, err := session.Admit(third); !errors.Is(err, ErrSessionStreamLimit) {
		t.Fatalf("third admit = %v, want ErrSessionStreamLimit", err)
	}
	// Refused, not queued: the answer is immediate by construction, and a
	// version that waited would show up here as a delay.
	if waited := time.Since(refusedAt); waited > time.Second {
		t.Fatalf("the refusal took %s — it was queued, not answered", waited)
	}

	// A closed stream gives its slot back, so the next local connection is
	// served rather than refused forever.
	if err := admittedFirst.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := session.Streams(); got != 1 {
		t.Fatalf("attached streams after close = %d, want 1", got)
	}
	fourth, fourthPeer := net.Pipe()
	defer func() { _ = fourthPeer.Close() }()
	if _, err := session.Admit(fourth); err != nil {
		t.Fatalf("admit after a released slot: %v", err)
	}
	// Closing twice must not release a second slot — that would inflate the
	// bound one double-close at a time.
	_ = admittedFirst.Close()
	if got := session.Streams(); got != 2 {
		t.Fatalf("attached streams = %d after a double close, want 2", got)
	}
}

// A stream carrying bytes is what keeps a tunnel alive; heartbeats deliberately
// do not. A forgotten port-forward must still time out, a busy one must not.
func TestHTTPSessionTrafficHoldsTheIdleTimerOpen(t *testing.T) {
	agentControl, _ := newControlPair(t)
	session := NewHTTPSession(agentControl, Options{MaxStreams: 1})
	stream, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	admitted, err := session.Admit(stream)
	if err != nil {
		t.Fatal(err)
	}

	ended := make(chan EndReason, 1)
	go func() {
		ended <- session.Run(context.Background(), Options{
			IdleTimeout: 150 * time.Millisecond,
			MaxDuration: time.Minute,
			Heartbeat:   time.Minute,
		})
	}()

	stop := make(chan struct{})
	go func() { // the peer drains, so writes complete
		buf := make([]byte, 8)
		for {
			if _, err := peer.Read(buf); err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(25 * time.Millisecond):
				if _, err := admitted.Write([]byte("x")); err != nil {
					return
				}
			}
		}
	}()

	select {
	case reason := <-ended:
		t.Fatalf("a session carrying traffic ended as %q", reason)
	case <-time.After(450 * time.Millisecond):
	}

	close(stop)
	select {
	case reason := <-ended:
		if reason != EndIdleTimeout {
			t.Fatalf("reason = %q, want %q once traffic stopped", reason, EndIdleTimeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a silent session must still time out")
	}
}

// The guaranteed teardown of §24.4: no forwarded connection outlives the
// session that authorized it, and nothing may be admitted afterwards.
func TestHTTPSessionTeardownClosesEveryStream(t *testing.T) {
	agentControl, _ := newControlPair(t)
	session := NewHTTPSession(agentControl, Options{MaxStreams: 4})
	stream, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	if _, err := session.Admit(stream); err != nil {
		t.Fatal(err)
	}

	cut := make(chan EndReason, 1)
	cut <- EndReason("revoked")
	if reason := session.Run(context.Background(), Options{Cancel: cut}); reason != EndReason("revoked") {
		t.Fatalf("reason = %q", reason)
	}

	// The forwarded connection is gone: its peer sees the close rather than
	// waiting on a tunnel nobody is authorized to use any more.
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("the forwarded connection outlived its session")
	}
	if session.Streams() != 0 {
		t.Fatalf("%d streams still attached after teardown", session.Streams())
	}
	late, latePeer := net.Pipe()
	defer func() { _ = late.Close(); _ = latePeer.Close() }()
	if _, err := session.Admit(late); !errors.Is(err, ErrOriginClosed) {
		t.Fatalf("admit after teardown = %v, want ErrOriginClosed", err)
	}
	select {
	case <-session.Done():
	default:
		t.Fatal("Done must be closed once the session stopped")
	}
}

// The reason travels before the request ends: it is the only way a developer
// learns why a tunnel they were not using disappeared (ADR-045 §5).
func TestHTTPSessionSendsItsTerminalReason(t *testing.T) {
	agentControl, clientControl := newControlPair(t)
	session := NewHTTPSession(agentControl, Options{})
	go func() {
		_ = session.SendClose(context.Background(), EndMaxDuration, "")
		_ = session.Close()
	}()
	frame, err := clientControl.Receive()
	if err != nil || frame.Type != "session_close" || frame.Reason != string(EndMaxDuration) {
		t.Fatalf("frame = %+v, %v", frame, err)
	}
}

// A reason the developer cannot act on is half a report (ADR-066 §3). Now that
// the attach answers before it dials, "the server's agent is not connected right
// now" arrives here rather than in a 409 body, so the sentence must ride beside
// the reason on the same frame.
func TestHTTPSessionCarriesTheOperatorSentenceBesideTheReason(t *testing.T) {
	agentControl, clientControl := newControlPair(t)
	session := NewHTTPSession(agentControl, Options{})
	const sentence = "the server's agent is not connected right now"
	go func() {
		_ = session.SendClose(context.Background(), EndReason("target_unreachable"), sentence)
		_ = session.Close()
	}()
	frame, err := clientControl.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Reason != "target_unreachable" || frame.Msg != sentence {
		t.Fatalf("frame = %+v — the reason without its sentence names no target", frame)
	}
}

// A client that announces its own close ends the session as a user close, not
// as a disconnect: the CLI re-dials through one and not the other.
func TestHTTPSessionHonoursTheClientsClose(t *testing.T) {
	agentControl, clientControl := newControlPair(t)
	session := NewHTTPSession(agentControl, Options{})
	ended := make(chan EndReason, 1)
	go func() { ended <- session.Run(context.Background(), Options{Heartbeat: time.Minute}) }()
	if err := clientControl.Send(context.Background(), HTTPControlFrame{Type: "session_close"}); err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-ended:
		if reason != EndUserClose {
			t.Fatalf("reason = %q, want %q", reason, EndUserClose)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the client's close must end the session")
	}
}
