package tunnel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// The session bounds of ADR-060 §6 are the tunnel's guaranteed kill: an
// HTTP v2 session must end on its own timers, with the reason the CLI decides
// re-dial versus exit on, and not merely when the peer happens to hang up.
func TestHTTPOriginEnforcesItsSessionBounds(t *testing.T) {
	t.Run("idle timeout", func(t *testing.T) {
		agentControl, _ := newControlPair(t)
		origin := NewHTTPOrigin(agentControl, Options{})
		if reason := origin.Run(context.Background(), Options{
			IdleTimeout: 20 * time.Millisecond,
			MaxDuration: time.Minute,
			Heartbeat:   time.Minute,
		}); reason != EndIdleTimeout {
			t.Fatalf("reason = %q, want %q", reason, EndIdleTimeout)
		}
	})

	t.Run("max duration", func(t *testing.T) {
		agentControl, _ := newControlPair(t)
		origin := NewHTTPOrigin(agentControl, Options{})
		if reason := origin.Run(context.Background(), Options{
			IdleTimeout: time.Minute,
			MaxDuration: 20 * time.Millisecond,
			Heartbeat:   time.Minute,
		}); reason != EndMaxDuration {
			t.Fatalf("reason = %q, want %q", reason, EndMaxDuration)
		}
	})

	t.Run("operator cut", func(t *testing.T) {
		agentControl, _ := newControlPair(t)
		origin := NewHTTPOrigin(agentControl, Options{})
		cut := make(chan EndReason, 1)
		cut <- EndReason("revoked")
		if reason := origin.Run(context.Background(), Options{Cancel: cut}); reason != EndReason("revoked") {
			t.Fatalf("reason = %q, want the cut's own reason", reason)
		}
	})

	// A heartbeat proves the control path is alive and refreshes the durable
	// session; a storage layer that says the session is already closed cuts the
	// tunnel rather than keeping an orphan alive.
	t.Run("heartbeat", func(t *testing.T) {
		agentControl, clientControl := newControlPair(t)
		origin := NewHTTPOrigin(agentControl, Options{})
		var beats atomic.Int64
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
		ended := make(chan EndReason, 1)
		go func() {
			ended <- origin.Run(context.Background(), Options{
				IdleTimeout: time.Minute,
				MaxDuration: time.Minute,
				Heartbeat:   5 * time.Millisecond,
				OnHeartbeat: func(context.Context) bool { return beats.Add(1) < 2 },
			})
		}()
		if frame, ok := <-pings; !ok || frame.Type != "ping" {
			t.Fatalf("frame = %+v, delivered = %v", frame, ok)
		}
		select {
		case reason := <-ended:
			if reason != EndDisconnect {
				t.Fatalf("reason = %q, want %q once the durable session is gone", reason, EndDisconnect)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a refused heartbeat must end the session")
		}
	})
}

// Admission is Origin's, not net/http's: a burst beyond the active bound waits
// in a bounded queue and is then refused explicitly, with the wait observed so
// hidden latency cannot masquerade as throughput.
func TestHTTPOriginQueueTimeoutIsRefusedAndObserved(t *testing.T) {
	agentControl, clientControl := newControlPair(t)
	type observation struct {
		wait time.Duration
		err  error
	}
	observed := make(chan observation, 8)
	origin := NewHTTPOrigin(agentControl, Options{
		MaxStreams: 1, MaxPendingStreams: 1, StreamQueueTimeout: 20 * time.Millisecond,
		OnStreamWait: func(wait time.Duration, err error) { observed <- observation{wait, err} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go origin.Run(ctx, Options{})

	// Fill the single active slot with a stream the laptop never answers.
	go func() { _, _ = origin.OpenStream(ctx) }()
	if frame, err := clientControl.Receive(); err != nil || frame.Type != "open" {
		t.Fatalf("open frame = %+v, %v", frame, err)
	}
	if first := <-observed; first.err != nil {
		t.Fatalf("the first open must be admitted at once: %v", first.err)
	}

	if _, err := origin.OpenStream(ctx); !errors.Is(err, ErrOriginQueueTimeout) {
		t.Fatalf("queued open = %v, want ErrOriginQueueTimeout", err)
	}
	refused := <-observed
	if !errors.Is(refused.err, ErrOriginQueueTimeout) || refused.wait <= 0 {
		t.Fatalf("observed %+v, want the refusal and the wait it cost", refused)
	}
}

// The multi-lane WebSocket fallback presents several sockets as one Conn: the
// session's liveness check must cover every one of them, and the group must
// report the drop of any single lane.
func TestMultiLaneConnChecksEveryLaneAndReportsDrops(t *testing.T) {
	primary := newFakeConn()
	secondary := newFakeConn()
	lanes := NewMultiLaneConn(primary, nil, 4)
	defer func() { _ = lanes.Close() }()
	if err := lanes.AddLane(1, secondary, nil); err != nil {
		t.Fatal(err)
	}
	if got := lanes.LaneCount(); got != 2 {
		t.Fatalf("lane count = %d, want 2", got)
	}
	if err := lanes.Ping(context.Background()); err != nil {
		t.Fatalf("ping with healthy lanes: %v", err)
	}

	// MultiLaneConn serializes writes per physical lane itself, which is what
	// lets the mux drop its session-wide writer lock.
	if _, ok := any(lanes).(parallelWriteConn); !ok {
		t.Fatal("MultiLaneConn must advertise parallel writes")
	}

	secondary.pingErr = errors.New("lane 1 is gone")
	if err := lanes.Ping(context.Background()); err == nil {
		t.Fatal("a dead secondary lane must fail the session's liveness check")
	}

	select {
	case <-lanes.Done():
		t.Fatal("the group is still whole")
	default:
	}
	close(secondary.in) // the socket drops
	select {
	case <-lanes.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a dropped lane must end the group")
	}
}
