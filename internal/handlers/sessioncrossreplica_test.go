// The cross-replica delivery path, driven end to end: a live bridge, attached
// and pumping, whose session row is finalized underneath it by a replica it
// cannot see.
//
// This is the shape both families converge through, and the shape that hid the
// defect. The replica that DECIDES a session is over — the sweep, a revocation, a
// grant that lapsed, the ADR-067 §2 liveness cut — writes the reason on the row
// and never touches the socket, because somebody else holds it. That somebody
// learns of the decision through exactly one signal: its 20-second liveness
// update matching zero rows. It used to answer that signal with `disconnect`, so
// a developer whose container had just been stopped read "the connection to the
// manager dropped" and went to inspect their own network. On a single-replica
// instance the cut reaches the socket directly and this path never runs, which is
// precisely why it survived.
//
// The tests below therefore refuse to call the beat closure directly — that is
// what terminalbeat_test.go and portforwardbeat_test.go do. They stand the real
// bridge up with the real closure over the real wire, take the row away
// underneath it, and assert on the frame a client actually receives. All four
// rungs of the ladder are covered, because ADR-064 §2 forbids a session behaving
// differently for the transport it landed on and the reason a developer reads is
// exactly the kind of difference that goes unnoticed for a release.
//
// Every top-level identifier is prefixed xrep (concurrent-agent rule).
package handlers

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/terminal"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// xrepBeat is short enough that a test does not wait the production cadence and
// long enough that the first beat lands with the client already attached. The
// bridges read it from Options, so no clock is stubbed to obtain it.
const xrepBeat = 10 * time.Millisecond

// xrepDeadline bounds every wait here. Generous on purpose: what these tests
// assert is which word arrives, never how fast.
const xrepDeadline = 10 * time.Second

// xrepRuler is the one thing these tests need of the steerable fakes, which are
// two different types (tbeatDB, pfbeatDB) for two families that must behave the
// same. Taking the capability rather than the type is what lets one arrangement
// helper serve both.
type xrepRuler interface{ rule(netcovRule) }

// xrepFinalizedElsewhere scripts the only database answer a replica that lost its
// session ever sees: the liveness update matches nothing, and the reason lives on
// the row it just failed to update.
func xrepFinalizedElsewhere(db xrepRuler, heartbeat, read string, reason store.TerminalEndReason) {
	db.rule(netcovRule{match: "-- name: " + heartbeat + " ", tag: "UPDATE 0"})
	db.rule(netcovRule{match: "-- name: " + read + " ", typed: []any{ptr(reason)}})
}

// xrepPTY is a shell that is alive and silent — the state a developer is in when
// this matters, reading output rather than typing. It must never end on its own:
// the only thing allowed to end these sessions is the row disappearing.
type xrepPTY struct {
	closed chan struct{}
	once   sync.Once
}

func newXrepPTY() *xrepPTY { return &xrepPTY{closed: make(chan struct{})} }

func (p *xrepPTY) Read([]byte) (int, error) {
	<-p.closed
	return 0, io.EOF
}

func (p *xrepPTY) Write(b []byte) (int, error) {
	if p.isClosed() {
		return 0, errors.New("pty closed")
	}
	return len(b), nil
}

func (p *xrepPTY) Resize(int, int) error { return nil }

func (p *xrepPTY) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func (p *xrepPTY) isClosed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

// A shell held on this replica while another replica stops its container. The
// developer must read `target_stopped` on the WebSocket rung's end frame, and the
// row must be finalized with the same word — the row, the audit entry and the
// last line the developer sees all coming from one value is the property ADR-067
// §2 spent a clause on and this branch quietly broke.
func TestXrepTerminalWebSocketReportsACloseDecidedElsewhere(t *testing.T) {
	a, db := tbeatAPI(t)
	xrepFinalizedElsewhere(db, "HeartbeatTerminalSession", "GetTerminalSessionEndReason",
		store.TerminalEndReasonTargetStopped)

	row := tbeatSession()
	pty := newXrepPTY()
	ended := make(chan terminal.EndReason, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		// The tail of TerminalWebSocket, verbatim apart from the beat interval:
		// the bridge, then the row, then the close status.
		reason := terminal.Bridge(r.Context(), wsConn{conn}, pty, terminal.Options{
			IdleTimeout: time.Minute,
			MaxDuration: time.Minute,
			Heartbeat:   xrepBeat,
			OnHeartbeat: a.terminalHeartbeat(row),
		})
		a.endTerminalSession(row, reason)
		_ = conn.Close(websocket.StatusNormalClosure, string(reason))
		ended <- reason
	}))
	defer srv.Close()

	conn := netcovDialWS(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), xrepDeadline)
	defer cancel()
	// Prove the bridge is genuinely attached before the row is taken away: a
	// reason delivered to a session that was never pumping would prove nothing
	// about what a working developer reads.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("ls\n")); err != nil {
		t.Fatalf("write to a live shell: %v", err)
	}

	if got := tbeatReadEndFrame(t, conn); got != terminalEndReasonTargetStopped {
		t.Fatalf("end frame reason = %q, want %q — a container somebody stopped was reported as a network glitch",
			got, terminalEndReasonTargetStopped)
	}
	if got := xrepAwait(t, ended); got != terminalEndReasonTargetStopped {
		t.Fatalf("bridge returned %q, want %q — the row and the audit entry take this value",
			got, terminalEndReasonTargetStopped)
	}
	if !pty.isClosed() {
		t.Error("the PTY outlived its own durable authorization")
	}
	if !db.ran("EndTerminalSession") {
		t.Error("the socket left without finalizing anything")
	}
}

// The terminal's HTTP rung (ADR-064 §3). Its report is a control frame on the
// session request rather than a close status, so it is a genuinely different
// delivery path and earns its own proof.
func TestXrepTerminalHTTPRungReportsACloseDecidedElsewhere(t *testing.T) {
	a, db := tbeatAPI(t)
	xrepFinalizedElsewhere(db, "HeartbeatTerminalSession", "GetTerminalSessionEndReason",
		store.TerminalEndReasonWakeFailed)

	row := tbeatSession()
	pty := newXrepPTY()
	control, wire := xrepControlPair(t)
	frames := xrepReadFrames(wire)

	// The rung presents its control wire and its data stream as one Conn; the
	// data half is a pipe nobody writes to, which is a shell sitting idle.
	dataIn, dataOut := io.Pipe()
	t.Cleanup(func() { _ = dataOut.Close() })
	conn := terminal.NewHTTPConn(control, xrepStream{Reader: dataIn, Writer: io.Discard})

	want := terminal.EndReason(store.TerminalEndReasonWakeFailed)
	ended := make(chan terminal.EndReason, 1)
	go func() {
		reason := terminal.Bridge(context.Background(), conn, pty, terminal.Options{
			IdleTimeout: time.Minute,
			MaxDuration: time.Minute,
			Heartbeat:   xrepBeat,
			OnHeartbeat: a.terminalHeartbeat(row),
		})
		a.endTerminalSession(row, reason)
		_ = conn.Close()
		ended <- reason
	}()

	if got := terminal.EndReason(xrepAwaitFrame(t, frames, "end").Reason); got != want {
		t.Fatalf("end frame reason = %q, want %q on the HTTP rung", got, want)
	}
	if got := xrepAwait(t, ended); got != want {
		t.Fatalf("bridge returned %q, want %q", got, want)
	}
	if !db.ran("GetTerminalSessionEndReason") {
		t.Error("the beat reported a reason without reading the row that holds one")
	}
}

// The tunnel family's HTTP rung, where the word matters most: a port-forward is
// silent for most of its life, so the session-close frame is the ONLY way the
// developer learns why a tunnel they were not using disappeared (ADR-045 §5).
func TestXrepTunnelHTTPSessionReportsACloseDecidedElsewhere(t *testing.T) {
	a, db := pfbeatAPI(t)
	xrepFinalizedElsewhere(db, "HeartbeatPortForwardSession", "GetPortForwardSessionEndReason",
		store.TerminalEndReasonGrantExpired)

	row := pfbeatSession()
	control, wire := xrepControlPair(t)
	frames := xrepReadFrames(wire)

	bounds := sessionBounds(row)
	cancelBridge := a.Tunnels.register(row.ID)
	defer a.Tunnels.unregister(row.ID, cancelBridge)
	bounds.Cancel = cancelBridge
	bounds.Heartbeat = xrepBeat
	bounds.OnHeartbeat = a.portForwardHeartbeat(row)
	session := tunnel.NewHTTPSession(control, bounds)

	ended := make(chan tunnel.EndReason, 1)
	go func() {
		// The tail of tunnelAttachSession: the run, then the reason on the wire,
		// then the row.
		reason := session.Run(context.Background(), bounds)
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = session.SendClose(closeCtx, reason, "")
		cancel()
		_ = session.Close()
		a.endPortForwardAttach(row, reason)
		ended <- reason
	}()

	got := tunnel.EndReason(xrepAwaitFrame(t, frames, "session_close").Reason)
	if got != endReasonGrantExpired {
		t.Fatalf("session_close reason = %q, want %q — the CLI turns it into `request access again`",
			got, endReasonGrantExpired)
	}
	if got := xrepAwait(t, ended); got != endReasonGrantExpired {
		t.Fatalf("session returned %q, want %q", got, endReasonGrantExpired)
	}
	if !db.ran("EndPortForwardSession") {
		t.Error("the socket left without finalizing anything")
	}
}

// The tunnel's WebSocket rung, whose report is the close status the CLI reads.
func TestXrepTunnelWebSocketReportsACloseDecidedElsewhere(t *testing.T) {
	a, db := pfbeatAPI(t)
	xrepFinalizedElsewhere(db, "HeartbeatPortForwardSession", "GetPortForwardSessionEndReason",
		store.TerminalEndReasonTargetStopped)

	row := pfbeatSession()
	ended := make(chan tunnel.EndReason, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"akerdock-tunnel-v1"},
		})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		bounds := sessionBounds(row)
		bounds.Heartbeat = xrepBeat
		bounds.OnHeartbeat = a.portForwardHeartbeat(row)
		// No stream is opened: a tunnel with nothing forwarded through it right
		// now is the case this whole report exists for.
		dial := func(context.Context) (net.Conn, error) { return nil, errors.New("no stream in this test") }
		reason := tunnel.Bridge(r.Context(), tunnelConn{conn}, dial, bounds)
		a.endPortForwardAttach(row, reason)
		_ = conn.Close(websocket.StatusNormalClosure, string(reason))
		ended <- reason
	}))
	defer srv.Close()

	conn := netcovDialWS(t, srv.URL, "akerdock-tunnel-v1")
	if got := xrepAwaitCloseStatus(t, conn); got != string(endReasonTargetStopped) {
		t.Fatalf("close reason = %q, want %q", got, endReasonTargetStopped)
	}
	if got := xrepAwait(t, ended); got != endReasonTargetStopped {
		t.Fatalf("bridge returned %q, want %q", got, endReasonTargetStopped)
	}
	if !db.ran("GetPortForwardSessionEndReason") {
		t.Error("the beat reported a reason without reading the row that holds one")
	}
}

// xrepControlPair wires a server-side LineControl to the client's end of the same
// full-duplex request, over two pipes and without net/http. It returns the
// server's control wire and the reader a client would hold.
func xrepControlPair(t *testing.T) (*tunnel.LineControl, io.Reader) {
	t.Helper()
	fromClient, toServer := io.Pipe()
	fromServer, toClient := io.Pipe()
	t.Cleanup(func() {
		_ = toServer.Close()
		_ = toClient.Close()
	})
	return tunnel.NewLineControl(fromClient, responseWriter{toClient}, nil, fromClient.Close), fromServer
}

// xrepReadFrames drains the control wire the way a client does. Draining is not
// decoration: a pipe blocks its writer, so a test that stopped reading would
// stall the very heartbeat it is waiting on.
func xrepReadFrames(r io.Reader) <-chan tunnel.HTTPControlFrame {
	frames := make(chan tunnel.HTTPControlFrame, 64)
	control := tunnel.NewLineControl(r, io.Discard, nil, nil)
	go func() {
		defer close(frames)
		for {
			frame, err := control.Receive()
			if err != nil {
				return
			}
			frames <- frame
		}
	}()
	return frames
}

// xrepAwaitFrame returns the first frame of the given type, skipping the pings
// that keep the wire warm.
func xrepAwaitFrame(t *testing.T, frames <-chan tunnel.HTTPControlFrame, want string) tunnel.HTTPControlFrame {
	t.Helper()
	deadline := time.After(xrepDeadline)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatalf("the control wire ended without a %q frame: the reason never reached the client", want)
			}
			if frame.Type == want {
				return frame
			}
		case <-deadline:
			t.Fatalf("no %q frame", want)
		}
	}
}

// xrepAwaitCloseStatus reads until the server closes and returns the reason it
// closed with — the string the CLI turns into its sentence.
func xrepAwaitCloseStatus(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), xrepDeadline)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			var closeErr websocket.CloseError
			if errors.As(err, &closeErr) {
				return closeErr.Reason
			}
			t.Fatalf("socket ended without a close frame: %v", err)
		}
	}
}

// xrepAwait takes the reason a bridge returned, or fails rather than hanging.
func xrepAwait[R ~string](t *testing.T, ended <-chan R) R {
	t.Helper()
	select {
	case reason := <-ended:
		return reason
	case <-time.After(xrepDeadline):
		t.Fatal("the bridge never returned after its row was finalized")
		return ""
	}
}

// xrepStream is the terminal HTTP rung's data half: one reader, one writer, and
// nothing else the bridge asks of it.
type xrepStream struct {
	io.Reader
	io.Writer
}

func (xrepStream) Close() error { return nil }
