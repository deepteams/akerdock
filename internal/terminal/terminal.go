// Package terminal bridges a client connection to a remote PTY (§24.4,
// ADR-024). The wire protocol is deliberately tiny:
//
//   - binary frames carry raw bytes, both ways (keystrokes in, output out);
//   - text frames carry JSON control messages: the client sends
//     {"type":"resize","cols":N,"rows":N}; the server sends
//     {"type":"end","reason":"..."} just before closing.
//
// A frame kind is what the WebSocket rung has; the HTTP rungs of ADR-064 §3
// carry the same two kinds on two wires — the session request and one data
// stream — and HTTPConn presents that pair as the same Conn. The bridge below
// never learns which one carried it.
//
// Keystrokes are never recorded (§24.4): the bridge moves bytes, it does not
// retain them.
package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// MessageType is the frame kind of the underlying WebSocket.
type MessageType int

// The two frame kinds the protocol uses.
const (
	MessageBinary MessageType = iota
	MessageText
)

// ErrClientClosed is what a Conn's Read must return when the client closed
// the connection cleanly — the bridge tells a wanted close (user_close) from
// a vanished peer (disconnect) by this error alone.
var ErrClientClosed = errors.New("terminal: client closed the connection")

// Conn is the subset of a WebSocket connection the bridge needs; the real
// implementation adapts coder/websocket, tests use an in-memory fake.
type Conn interface {
	Read(ctx context.Context) (MessageType, []byte, error)
	Write(ctx context.Context, typ MessageType, data []byte) error
	// Ping is the §24.4 heartbeat: it fails when the peer is gone even if no
	// data is flowing.
	Ping(ctx context.Context) error
}

// PTY is the remote pseudo-terminal end of the bridge (sshexec.PTY in
// production). Read returning io.EOF means the remote command exited.
type PTY interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

// EndReason mirrors the terminal_end_reason enum (data-dictionary §10.6).
type EndReason string

// Why a session ended (§24.4).
const (
	EndUserClose   EndReason = "user_close"
	EndIdleTimeout EndReason = "idle_timeout"
	EndMaxDuration EndReason = "max_duration"
	EndDisconnect  EndReason = "disconnect"
	EndRevoked     EndReason = "revoked"
	// EndTargetUnreachable is the shell that never opened (ADR-066): the
	// attach answers before it dials, so a server that refuses SSH or a
	// container that is gone is no longer a 409 at redeem — it is reported on
	// the session that is already open. The two values that used to carry it
	// were both lies: disconnect blames the developer's own network, and
	// revoked claims an administrator acted when nobody did.
	EndTargetUnreachable EndReason = "target_unreachable"
)

// Options bounds a session (§24.4). Zero values fall back to defaults.
type Options struct {
	// IdleTimeout ends the session after that long without a keystroke.
	// Output does not count as activity: a spinner left running must not
	// keep a forgotten root shell alive.
	IdleTimeout time.Duration
	// MaxDuration ends the session regardless of activity.
	MaxDuration time.Duration
	// Heartbeat is the ping interval detecting a silently vanished peer.
	Heartbeat time.Duration
	// OnHeartbeat rides that same beat to persist what only the control plane
	// knows: that this session is still attached — and therefore, since
	// ADR-067 §1, that its target is not idle. A beat is the only moment an
	// attached session speaks to the control plane while a developer sits and
	// reads, which is why the durable liveness stamp and the activity signal
	// share one hook rather than growing a timer each.
	//
	// A storage error is best effort: the caller logs it and returns true,
	// because the socket is the source of truth while this process lives.
	// Returning FALSE is reserved for a session that is durably over — another
	// replica or the sweep finalized the row, or a re-claim superseded this
	// attach — and it cuts the socket, which must not outlive its own
	// authorization. That is tunnel.Options.OnHeartbeat's contract, word for
	// word and on purpose: two bridges, one rule about what a beat means.
	OnHeartbeat func(context.Context) bool
	// Cancel ends the session from outside, naming the reason to report. A nil
	// channel simply never fires, which is why the zero value is inert.
	//
	// It carries a reason rather than being a bare signal for the same reason
	// the tunnel's does: cancelling the session's context also ends it, but it
	// ends it as EndRevoked — which tells the developer an administrator acted
	// when nobody did. A target that stopped under a live shell (ADR-067 §2)
	// and an attach displaced by a re-claim (ADR-065 §5) are both cuts from
	// outside, and neither is a revocation.
	Cancel <-chan EndReason
}

// Defaults for Options; the instance configuration overrides them
// (AKERDOCK_TERMINAL_IDLE_TIMEOUT / AKERDOCK_TERMINAL_MAX_DURATION).
const (
	DefaultIdleTimeout = 15 * time.Minute
	DefaultMaxDuration = 4 * time.Hour
	defaultHeartbeat   = 20 * time.Second
)

// controlMessage is a client→server text frame.
type controlMessage struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// endMessage is the server→client text frame sent before closing. Msg carries
// the operator sentence beside the machine-readable reason (ADR-066 §3): the
// egress wire has always had a message field, and without one here a developer
// whose shell never opened reads `session ended: target_unreachable` where a
// port-forward prints what actually went wrong.
type endMessage struct {
	Type   string    `json:"type"`
	Reason EndReason `json:"reason"`
	Msg    string    `json:"msg,omitempty"`
}

// SendEnd writes the session's final text frame, best effort and on a fresh
// context: whoever calls it is tearing down, and ctx may already be dead.
//
// It is exported because a session can end BEFORE it has a bridge. Since
// ADR-066 the remote half runs behind the response head, so a resolution that
// fails has an open session request and no PTY — and this frame is the only
// channel that failure has, the terminal's data stream carrying no dial and so
// having nothing to answer 502 about.
func SendEnd(conn Conn, reason EndReason, msg string) {
	payload, err := json.Marshal(endMessage{Type: "end", Reason: reason, Msg: msg})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Write(ctx, MessageText, payload)
}

// Bridge pumps bytes between conn and pty until one side ends the session,
// then closes the pty — the guaranteed kill of §24.4 — and reports why. The
// caller owns the WebSocket close handshake and the session row.
func Bridge(ctx context.Context, conn Conn, pty PTY, opts Options) EndReason {
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = DefaultMaxDuration
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = defaultHeartbeat
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// First reason wins: both pumps and all three timers race to it, and the
	// buffered channel lets losers report without a receiver.
	reasons := make(chan EndReason, 2)

	// Client → PTY: keystrokes and control messages. Only keystrokes reset
	// the idle timer.
	activity := make(chan struct{}, 1)
	go func() {
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				if errors.Is(err, ErrClientClosed) {
					reasons <- EndUserClose
				} else {
					reasons <- EndDisconnect
				}
				return
			}
			switch typ {
			case MessageBinary:
				select {
				case activity <- struct{}{}:
				default:
				}
				if _, err := pty.Write(data); err != nil {
					reasons <- EndDisconnect
					return
				}
			case MessageText:
				var msg controlMessage
				if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" &&
					msg.Cols > 0 && msg.Cols <= 1000 && msg.Rows > 0 && msg.Rows <= 1000 {
					_ = pty.Resize(msg.Cols, msg.Rows)
				}
			}
		}
	}()

	// PTY → client: raw output. EOF is the shell exiting — the normal end.
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := pty.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, MessageBinary, buf[:n]); werr != nil {
					reasons <- EndDisconnect
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					reasons <- EndUserClose
				} else {
					reasons <- EndDisconnect
				}
				return
			}
		}
	}()

	idle := time.NewTimer(opts.IdleTimeout)
	defer idle.Stop()
	maxDur := time.NewTimer(opts.MaxDuration)
	defer maxDur.Stop()
	heartbeat := time.NewTicker(opts.Heartbeat)
	defer heartbeat.Stop()

	reason := EndDisconnect
loop:
	for {
		select {
		case <-ctx.Done():
			// The control plane is shutting down or the handler was torn
			// away: the session is being revoked, not abandoned by its user.
			reason = EndRevoked
			break loop
		case reason = <-reasons:
			break loop
		case reason = <-opts.Cancel:
			// Cut from outside, with its own word for it.
			break loop
		case <-activity:
			if !idle.Stop() {
				<-idle.C
			}
			idle.Reset(opts.IdleTimeout)
		case <-idle.C:
			reason = EndIdleTimeout
			break loop
		case <-maxDur.C:
			reason = EndMaxDuration
			break loop
		case <-heartbeat.C:
			if err := conn.Ping(ctx); err != nil {
				reason = EndDisconnect
				break loop
			}
			if opts.OnHeartbeat != nil && !opts.OnHeartbeat(ctx) {
				// The durable session is already closed; only the socket is
				// left, and the reason it ended with is on the row.
				reason = EndDisconnect
				break loop
			}
		}
	}

	// A disconnect produced while we were cancelling is a revocation: the
	// pump's I/O failed BECAUSE the context died, and it can beat ctx.Done()
	// to the select above. Arbitrating once here covers every error path —
	// pumps and heartbeat alike — without each site classifying for itself.
	if reason == EndDisconnect && ctx.Err() != nil {
		reason = EndRevoked
	}
	// …and a cut that named its reason wins over that default. A cutter queues
	// the reason and THEN cancels — it has to cancel, because the same cut must
	// also reach a session still resolving its PTY, which no bridge is watching
	// yet — so by the time cancellation surfaces here the value is already
	// buffered and this receive cannot miss it. Without this arbitration the
	// container that stopped under a live shell would be reported as `revoked`:
	// the one word ADR-067 §2 exists to stop the developer reading.
	if reason == EndRevoked {
		select {
		case r := <-opts.Cancel:
			reason = r
		default:
		}
	}

	// Kill first (§24.4 — the pty must not survive the socket), then tell the
	// client why. A bridge that ends has no sentence to add: the reason IS the
	// whole report, and the message field exists for the failures that never
	// reached a bridge at all.
	_ = pty.Close()
	SendEnd(conn, reason, "")
	return reason
}
