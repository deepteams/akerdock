package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConn is an in-memory Conn: the test scripts inbound frames on a channel
// and collects everything the bridge writes.
type fakeConn struct {
	in chan fakeFrame

	mu      sync.Mutex
	writes  []fakeFrame
	pings   int
	pingErr error
}

type fakeFrame struct {
	typ  MessageType
	data []byte
	err  error
}

func newFakeConn() *fakeConn {
	return &fakeConn{in: make(chan fakeFrame, 16)}
}

func (c *fakeConn) Read(ctx context.Context) (MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case f := <-c.in:
		return f.typ, f.data, f.err
	}
}

func (c *fakeConn) Write(_ context.Context, typ MessageType, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, fakeFrame{typ: typ, data: append([]byte(nil), data...)})
	return nil
}

func (c *fakeConn) Ping(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pings++
	return c.pingErr
}

// binaryOutput concatenates every binary frame written to the client so far.
func (c *fakeConn) binaryOutput() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var output []byte
	for _, f := range c.writes {
		if f.typ == MessageBinary {
			output = append(output, f.data...)
		}
	}
	return string(output)
}

func (c *fakeConn) endMessage(t *testing.T) (EndReason, bool) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.writes {
		if f.typ != MessageText {
			continue
		}
		var msg endMessage
		if err := json.Unmarshal(f.data, &msg); err == nil && msg.Type == "end" {
			return msg.Reason, true
		}
	}
	return "", false
}

// fakePTY records writes and resizes; Read blocks on a channel the test
// feeds, and returns io.EOF once closed. done is a separate channel so a
// test goroutine may keep sending output while the bridge closes the pty.
type fakePTY struct {
	out  chan []byte
	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	written []byte
	resizes [][2]int
	closed  bool
}

func newFakePTY() *fakePTY {
	return &fakePTY{out: make(chan []byte, 16), done: make(chan struct{})}
}

func (p *fakePTY) Read(b []byte) (int, error) {
	// Drain pending output before reporting EOF: a real pty delivers what
	// was written before the command exited.
	select {
	case data := <-p.out:
		return copy(b, data), nil
	default:
	}
	select {
	case data := <-p.out:
		return copy(b, data), nil
	case <-p.done:
		return 0, io.EOF
	}
}

func (p *fakePTY) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errors.New("pty closed")
	}
	p.written = append(p.written, b...)
	return len(b), nil
}

func (p *fakePTY) Resize(cols, rows int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resizes = append(p.resizes, [2]int{cols, rows})
	return nil
}

func (p *fakePTY) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *fakePTY) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *fakePTY) writtenBytes() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.written)
}

// waitFor polls a condition to a deadline — the sanctioned replacement for a
// bare sleep when a test must wait on the bridge's goroutines.
func waitFor(timeout time.Duration, condition func() bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && !condition() {
		time.Sleep(5 * time.Millisecond)
	}
}

// run executes Bridge in a goroutine and returns its reason, failing the test
// if it does not finish in time: a bridge that never ends is precisely the
// bug the timeouts exist to prevent.
func run(ctx context.Context, t *testing.T, conn Conn, pty PTY, opts Options) EndReason {
	t.Helper()
	done := make(chan EndReason, 1)
	go func() { done <- Bridge(ctx, conn, pty, opts) }()
	select {
	case reason := <-done:
		return reason
	case <-time.After(5 * time.Second):
		t.Fatal("Bridge did not terminate")
		return ""
	}
}

// generous keeps a bound out of the way of the scenario under test.
const generous = time.Hour

func TestBridgeShellExitIsUserClose(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()
	pty.out <- []byte("$ ")
	_ = pty.Close() // the shell exits

	reason := run(context.Background(), t, conn, pty, Options{IdleTimeout: generous, MaxDuration: generous})
	if reason != EndUserClose {
		t.Fatalf("reason = %q, want %q", reason, EndUserClose)
	}
	if got, ok := conn.endMessage(t); !ok || got != EndUserClose {
		t.Fatalf("end message = %q (present=%v), want %q", got, ok, EndUserClose)
	}
}

func TestBridgeClientCloseIsUserClose(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()
	conn.in <- fakeFrame{err: ErrClientClosed}

	reason := run(context.Background(), t, conn, pty, Options{IdleTimeout: generous, MaxDuration: generous})
	if reason != EndUserClose {
		t.Fatalf("reason = %q, want %q", reason, EndUserClose)
	}
	if !pty.isClosed() {
		t.Fatal("pty must be closed when the session ends — that is the guaranteed kill")
	}
}

func TestBridgeAbruptClientErrorIsDisconnect(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()
	conn.in <- fakeFrame{err: errors.New("connection reset")}

	reason := run(context.Background(), t, conn, pty, Options{IdleTimeout: generous, MaxDuration: generous})
	if reason != EndDisconnect {
		t.Fatalf("reason = %q, want %q", reason, EndDisconnect)
	}
	if !pty.isClosed() {
		t.Fatal("pty must be closed on disconnect")
	}
}

func TestBridgeIdleTimeout(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()

	reason := run(context.Background(), t, conn, pty, Options{IdleTimeout: 50 * time.Millisecond, MaxDuration: generous})
	if reason != EndIdleTimeout {
		t.Fatalf("reason = %q, want %q", reason, EndIdleTimeout)
	}
	if !pty.isClosed() {
		t.Fatal("pty must be closed on idle timeout")
	}
}

func TestBridgeKeystrokesResetIdle(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()

	// Keystrokes at half the idle window: the session must outlive several
	// idle windows, then hit max duration.
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				select {
				case conn.in <- fakeFrame{typ: MessageBinary, data: []byte("k")}:
				default:
				}
			}
		}
	}()
	defer close(stop)

	reason := run(context.Background(), t, conn, pty, Options{IdleTimeout: 80 * time.Millisecond, MaxDuration: 400 * time.Millisecond})
	if reason != EndMaxDuration {
		t.Fatalf("reason = %q, want %q (idle must have been reset by keystrokes)", reason, EndMaxDuration)
	}
}

func TestBridgeOutputDoesNotResetIdle(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()

	// A chatty program with no keystrokes: idle must still fire — output is
	// not activity, or a spinner would keep a forgotten shell alive forever.
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				select {
				case pty.out <- []byte("spinner\n"):
				default:
				}
			}
		}
	}()
	defer close(stop)

	reason := run(context.Background(), t, conn, pty, Options{IdleTimeout: 100 * time.Millisecond, MaxDuration: generous})
	if reason != EndIdleTimeout {
		t.Fatalf("reason = %q, want %q", reason, EndIdleTimeout)
	}
}

func TestBridgeMaxDuration(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()

	reason := run(context.Background(), t, conn, pty, Options{IdleTimeout: generous, MaxDuration: 50 * time.Millisecond})
	if reason != EndMaxDuration {
		t.Fatalf("reason = %q, want %q", reason, EndMaxDuration)
	}
}

func TestBridgeContextCancelIsRevoked(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	reason := run(ctx, t, conn, pty, Options{IdleTimeout: generous, MaxDuration: generous})
	if reason != EndRevoked {
		t.Fatalf("reason = %q, want %q", reason, EndRevoked)
	}
	if !pty.isClosed() {
		t.Fatal("pty must be closed on revocation")
	}
}

func TestBridgeZeroOptionsUseDefaults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn := newFakeConn()
	pty := newFakePTY()
	if got := Bridge(ctx, conn, pty, Options{}); got != EndRevoked {
		t.Fatalf("Bridge() = %s, want %s", got, EndRevoked)
	}
}

func TestBridgeHeartbeatFailureIsDisconnect(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()
	conn.pingErr = errors.New("peer gone")

	reason := run(context.Background(), t, conn, pty, Options{
		IdleTimeout: generous, MaxDuration: generous, Heartbeat: 20 * time.Millisecond,
	})
	if reason != EndDisconnect {
		t.Fatalf("reason = %q, want %q", reason, EndDisconnect)
	}
}

func TestBridgeMovesBytesBothWays(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()
	conn.in <- fakeFrame{typ: MessageBinary, data: []byte("ls -la\r")}
	pty.out <- []byte("total 0\r\n")

	go func() {
		// Close from the client only once both pumps have provably moved the
		// seeded bytes — a wall-clock guess loses on a loaded runner.
		waitFor(2*time.Second, func() bool {
			return pty.writtenBytes() != "" && conn.binaryOutput() != ""
		})
		conn.in <- fakeFrame{err: ErrClientClosed}
	}()
	reason := run(context.Background(), t, conn, pty, Options{IdleTimeout: generous, MaxDuration: generous})
	if reason != EndUserClose {
		t.Fatalf("reason = %q, want %q", reason, EndUserClose)
	}

	if written := pty.writtenBytes(); written != "ls -la\r" {
		t.Fatalf("pty received %q, want %q", written, "ls -la\r")
	}
	if output := conn.binaryOutput(); output != "total 0\r\n" {
		t.Fatalf("client received %q, want %q", output, "total 0\r\n")
	}
}

// failWritePTY refuses every write while its embedded fakePTY keeps Read
// blocking: the keystroke pump hits the write error in isolation.
type failWritePTY struct {
	*fakePTY
}

func (p *failWritePTY) Write([]byte) (int, error) {
	return 0, errors.New("pty write failed")
}

func TestBridgePTYWriteErrorIsDisconnect(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()
	conn.in <- fakeFrame{typ: MessageBinary, data: []byte("k")}

	reason := run(context.Background(), t, conn, &failWritePTY{pty}, Options{IdleTimeout: generous, MaxDuration: generous})
	if reason != EndDisconnect {
		t.Fatalf("reason = %q, want %q", reason, EndDisconnect)
	}
	if !pty.isClosed() {
		t.Fatal("pty must be closed when its write path breaks")
	}
}

// failBinaryWriteConn drops binary frames on the floor with an error but lets
// text frames through, so the bridge can still deliver its end message.
type failBinaryWriteConn struct {
	*fakeConn
}

func (c *failBinaryWriteConn) Write(ctx context.Context, typ MessageType, data []byte) error {
	if typ == MessageBinary {
		return errors.New("client write failed")
	}
	return c.fakeConn.Write(ctx, typ, data)
}

func TestBridgeClientWriteErrorIsDisconnect(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()
	pty.out <- []byte("output the client will never get")

	reason := run(context.Background(), t, &failBinaryWriteConn{conn}, pty, Options{IdleTimeout: generous, MaxDuration: generous})
	if reason != EndDisconnect {
		t.Fatalf("reason = %q, want %q", reason, EndDisconnect)
	}
	if got, ok := conn.endMessage(t); !ok || got != EndDisconnect {
		t.Fatalf("end message = %q (present=%v), want %q", got, ok, EndDisconnect)
	}
}

// errReadPTY fails Read outright — an SSH channel torn down under the bridge,
// not a shell exiting.
type errReadPTY struct {
	*fakePTY
}

func (p *errReadPTY) Read([]byte) (int, error) {
	return 0, errors.New("pty read failed")
}

func TestBridgePTYReadErrorIsDisconnect(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()

	reason := run(context.Background(), t, conn, &errReadPTY{pty}, Options{IdleTimeout: generous, MaxDuration: generous})
	if reason != EndDisconnect {
		t.Fatalf("reason = %q, want %q", reason, EndDisconnect)
	}
	if !pty.isClosed() {
		t.Fatal("pty must be closed when its read path breaks")
	}
}

// gatedPingConn parks the bridge's control loop inside Ping: started is
// closed when the first Ping begins, and Ping only returns (with err) once
// release is closed. That lets a test line up events — a cancellation, an
// expired timer, a queued keystroke — while the loop provably is not in its
// select, then observe exactly which case it takes on re-entry.
type gatedPingConn struct {
	*fakeConn
	started   chan struct{}
	release   chan struct{}
	err       error
	startOnce sync.Once
}

func newGatedPingConn(err error) *gatedPingConn {
	return &gatedPingConn{
		fakeConn: newFakeConn(),
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		err:      err,
	}
}

func (c *gatedPingConn) Ping(context.Context) error {
	c.startOnce.Do(func() { close(c.started) })
	<-c.release
	return c.err
}

func TestBridgeDisconnectDuringCancellationIsRevoked(t *testing.T) {
	// A revocation can surface as an I/O error before the loop ever sees
	// ctx.Done: here the heartbeat is in flight when the context dies, and
	// its failure must be arbitrated back to revoked, not disconnect.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := newGatedPingConn(errors.New("peer vanished mid-shutdown"))
	pty := newFakePTY()

	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(ctx, conn, pty, Options{IdleTimeout: generous, MaxDuration: generous, Heartbeat: time.Millisecond})
	}()

	<-conn.started      // the loop is inside Ping, not watching ctx.Done
	cancel()            // the revocation lands while the ping is in flight
	close(conn.release) // the ping now fails: the loop sees an error first

	select {
	case reason := <-done:
		if reason != EndRevoked {
			t.Fatalf("reason = %q, want %q (disconnect during cancellation is a revocation)", reason, EndRevoked)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Bridge did not terminate")
	}
	if !pty.isClosed() {
		t.Fatal("pty must be closed on revocation")
	}
}

func TestBridgeKeystrokeRacingExpiredIdleStillTimesOut(t *testing.T) {
	// A keystroke consumed after the idle timer has already expired must not
	// resurrect the session indefinitely: whichever ready case the select
	// picks, the session still ends in idle_timeout. Note the drain branch
	// (`if !idle.Stop() { <-idle.C }`) cannot fire on Go >= 1.23: with
	// synchronous timer channels, Stop on an expired-but-undelivered timer
	// returns true and removes the pending tick, so the false path is
	// unreachable — this test pins the behavior, not that branch.
	const idleTimeout = 10 * time.Millisecond
	conn := newGatedPingConn(nil)
	pty := newFakePTY()

	done := make(chan EndReason, 1)
	go func() {
		done <- Bridge(context.Background(), conn, pty, Options{IdleTimeout: idleTimeout, MaxDuration: generous, Heartbeat: time.Millisecond})
	}()

	<-conn.started // the loop is parked inside Ping
	conn.in <- fakeFrame{typ: MessageBinary, data: []byte("k")}
	// The pump recording the keystroke proves the activity token was queued
	// first — the loop, still parked, has not consumed it.
	waitFor(2*time.Second, func() bool { return pty.writtenBytes() == "k" })
	time.Sleep(3 * idleTimeout) // the idle timer has certainly expired
	close(conn.release)         // now the keystroke and the expired timer race

	select {
	case reason := <-done:
		if reason != EndIdleTimeout {
			t.Fatalf("reason = %q, want %q", reason, EndIdleTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Bridge did not terminate")
	}
}

func TestBridgeResizeControl(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()
	conn.in <- fakeFrame{typ: MessageText, data: []byte(`{"type":"resize","cols":132,"rows":43}`)}
	// Out-of-range and malformed control messages must be ignored, not crash.
	conn.in <- fakeFrame{typ: MessageText, data: []byte(`{"type":"resize","cols":0,"rows":43}`)}
	conn.in <- fakeFrame{typ: MessageText, data: []byte(`{"type":"resize","cols":5000,"rows":43}`)}
	conn.in <- fakeFrame{typ: MessageText, data: []byte(`not json`)}
	conn.in <- fakeFrame{err: ErrClientClosed}

	run(context.Background(), t, conn, pty, Options{IdleTimeout: generous, MaxDuration: generous})

	pty.mu.Lock()
	defer pty.mu.Unlock()
	if len(pty.resizes) != 1 || pty.resizes[0] != [2]int{132, 43} {
		t.Fatalf("resizes = %v, want exactly [[132 43]]", pty.resizes)
	}
}

// The failure ADR-066 §3 gives the terminal a vocabulary for: since the attach
// answers before it dials, a shell that never opens is reported on a session
// that is already up, and the report has to say more than a bare reason. A
// port-forward prints a sentence; without the message field the terminal
// printed "session ended: target_unreachable" and the developer went looking
// for an administrator who never acted.
func TestSendEndCarriesTheOperatorSentence(t *testing.T) {
	conn := newFakeConn()
	SendEnd(conn, EndTargetUnreachable, "the container is not running — start it first")

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.writes) != 1 || conn.writes[0].typ != MessageText {
		t.Fatalf("writes = %+v — the end frame is one text message", conn.writes)
	}
	var msg endMessage
	if err := json.Unmarshal(conn.writes[0].data, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "end" || msg.Reason != EndTargetUnreachable {
		t.Fatalf("end = %+v, want a target_unreachable end", msg)
	}
	if msg.Msg != "the container is not running — start it first" {
		t.Fatalf("msg = %q — the reason alone is not a report", msg.Msg)
	}
}

// A bridge that ends has no sentence to add, and must not invent one: the
// reason IS the whole report there, and an empty message stays off the wire.
func TestBridgeEndMessageCarriesNoSentence(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()
	conn.in <- fakeFrame{err: ErrClientClosed}
	run(context.Background(), t, conn, pty, Options{IdleTimeout: generous, MaxDuration: generous})

	conn.mu.Lock()
	defer conn.mu.Unlock()
	for _, f := range conn.writes {
		if f.typ != MessageText {
			continue
		}
		if strings.Contains(string(f.data), `"msg"`) {
			t.Fatalf("end frame = %s — an empty message must be omitted", f.data)
		}
	}
}

// ---------------------------------------------------------------------------
// ADR-067 §1 — the beat is durable, and §2 — a cut carries its reason
// ---------------------------------------------------------------------------

// The beat is what makes an attached shell visible to anything outside this
// process: to scale-to-zero, which would otherwise read a developer at work as
// perfect idleness, and to the next control plane after a crash.
func TestBridgeHeartbeatPersistsLivenessAfterEachPing(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()
	var beats atomic.Int32

	go func() {
		waitFor(2*time.Second, func() bool { return beats.Load() >= 3 })
		conn.in <- fakeFrame{err: ErrClientClosed}
	}()
	reason := run(context.Background(), t, conn, pty, Options{
		IdleTimeout: generous, MaxDuration: generous, Heartbeat: time.Millisecond,
		OnHeartbeat: func(context.Context) EndReason { beats.Add(1); return "" },
	})
	if reason != EndUserClose {
		t.Fatalf("reason = %q, want %q — a beat that answers the empty reason changes nothing", reason, EndUserClose)
	}
	if got := beats.Load(); got < 3 {
		t.Fatalf("beats = %d — the hook must ride the ping ticker, not fire once", got)
	}
	conn.mu.Lock()
	pings := conn.pings
	conn.mu.Unlock()
	if pings < int(beats.Load()) {
		t.Fatalf("beats = %d for %d pings — liveness is persisted only after the peer answered",
			beats.Load(), pings)
	}
}

// The only durable answer that ends a socket: the row is already finalized —
// another replica, the sweep, or a re-claim that superseded this attach — and a
// PTY must not outlive its own authorization.
//
// What the bridge reports is whatever the beat hands it, verbatim. The beat is
// the only party that can read the row it just failed to update, so the bridge
// classifies nothing here: a session finalized on another replica as
// `target_stopped` must not reach the developer as `disconnect`, which is a
// network glitch and sends them to inspect their own laptop.
func TestBridgeHeartbeatEndsTheSessionWithTheReasonTheBeatReports(t *testing.T) {
	for _, want := range []EndReason{EndDisconnect, EndRevoked, "target_stopped", "wake_failed"} {
		t.Run(string(want), func(t *testing.T) {
			conn, pty := newFakeConn(), newFakePTY()
			reason := run(context.Background(), t, conn, pty, Options{
				IdleTimeout: generous, MaxDuration: generous, Heartbeat: time.Millisecond,
				OnHeartbeat: func(context.Context) EndReason { return want },
			})
			if reason != want {
				t.Fatalf("reason = %q, want %q", reason, want)
			}
			if !pty.isClosed() {
				t.Fatal("pty must be closed when the durable session is gone")
			}
			// The end frame is where the developer meets it, and it is the whole
			// of the report: a reason the bridge returned but never wrote would
			// reach the row and the audit trail and nobody else.
			got, ok := conn.endMessage(t)
			if !ok || got != want {
				t.Fatalf("end frame reason = %q (present=%v), want %q", got, ok, want)
			}
		})
	}
}

// A peer that has already vanished is not asked to carry a beat: the ping is
// the cheaper test and it runs first, so a dead socket costs no database write.
func TestBridgeHeartbeatIsNotRunAfterAFailedPing(t *testing.T) {
	conn, pty := newFakeConn(), newFakePTY()
	conn.pingErr = errors.New("peer gone")
	var beats atomic.Int32

	reason := run(context.Background(), t, conn, pty, Options{
		IdleTimeout: generous, MaxDuration: generous, Heartbeat: time.Millisecond,
		OnHeartbeat: func(context.Context) EndReason { beats.Add(1); return "" },
	})
	if reason != EndDisconnect {
		t.Fatalf("reason = %q, want %q", reason, EndDisconnect)
	}
	if got := beats.Load(); got != 0 {
		t.Fatalf("beats = %d — a vanished peer must not be recorded as alive", got)
	}
}

// The whole of §2 as the developer meets it: the container went away, and the
// session says so. `revoked` is the specific lie this exists to stop — nobody
// revoked anything.
func TestBridgeCutCarriesItsReason(t *testing.T) {
	const targetStopped EndReason = "target_stopped"
	conn, pty := newFakeConn(), newFakePTY()
	cut := make(chan EndReason, 1)
	cut <- targetStopped

	reason := run(context.Background(), t, conn, pty, Options{
		IdleTimeout: generous, MaxDuration: generous, Cancel: cut,
	})
	if reason != targetStopped {
		t.Fatalf("reason = %q, want %q", reason, targetStopped)
	}
	if got, ok := conn.endMessage(t); !ok || got != targetStopped {
		t.Fatalf("end frame = %q (present=%v), want %q", got, ok, targetStopped)
	}
	if !pty.isClosed() {
		t.Fatal("pty must be closed when the session is cut")
	}
}

// The arbitration a cutter depends on. A cut that must also reach a session
// which is not yet bridging cancels the context as well, and that cancellation
// can be the branch the loop wakes on: the queued reason still wins, because it
// was queued first.
func TestBridgeCutBeatsTheCancellationThatAccompaniesIt(t *testing.T) {
	const targetStopped EndReason = "target_stopped"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, pty := newFakeConn(), newFakePTY()
	cut := make(chan EndReason, 1)

	// The order a cutter uses: the word, then the cancellation.
	cut <- targetStopped
	cancel()

	reason := Bridge(ctx, conn, pty, Options{IdleTimeout: generous, MaxDuration: generous, Cancel: cut})
	if reason == EndRevoked {
		t.Fatal("a cancelled cut reported a revocation nobody performed")
	}
	if reason != targetStopped {
		t.Fatalf("reason = %q, want %q", reason, targetStopped)
	}
}

// A cancellation with nothing queued is still a revocation: the control plane
// is shutting down, or the handler was torn away.
func TestBridgeBareCancellationIsStillRevoked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, pty := newFakeConn(), newFakePTY()
	if got := Bridge(ctx, conn, pty, Options{Cancel: make(chan EndReason, 1)}); got != EndRevoked {
		t.Fatalf("Bridge() = %s, want %s", got, EndRevoked)
	}
}

// One cadence for two bridges (ADR-067 §1): internal/tunnel beats at the same
// 20 s, and the two are separate constants in separate packages with nothing
// but this assertion between them.
func TestDefaultHeartbeatMatchesTheTunnelsCadence(t *testing.T) {
	if defaultHeartbeat != 20*time.Second {
		t.Fatalf("defaultHeartbeat = %s, want 20s — the tunnel's beat (internal/tunnel) runs at that "+
			"cadence and ADR-067 §1 records activity on both", defaultHeartbeat)
	}
}
