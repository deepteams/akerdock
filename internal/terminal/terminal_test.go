package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
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
