// Package tunnel bridges a WebSocket to TCP streams multiplexed over it
// (ADR-032, subprotocol akerdock-tunnel-v1). It is the sibling of
// internal/terminal: same Conn abstraction, same session invariants (idle
// timeout, max duration, heartbeat, guaranteed teardown), but instead of one
// PTY it fans out to many TCP connections dialed through a caller-provided
// dialer (in production, an SSH direct-tcpip channel to a fixed container).
//
// Wire protocol:
//   - text frames: JSON control {"t":"open|open_ok|open_err|eof|close","id":N,…};
//   - binary frames: [u32 big-endian stream id][payload].
package tunnel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// MessageType is the frame kind of the underlying WebSocket.
type MessageType int

// Tunnel WebSocket frame types (ADR-032).
const (
	MessageBinary MessageType = iota
	MessageText
)

// ErrClientClosed is what Conn.Read returns on a clean client close.
var ErrClientClosed = errors.New("tunnel: client closed the connection")

// Conn is the subset of a WebSocket the bridge needs (adapts coder/websocket).
type Conn interface {
	Read(ctx context.Context) (MessageType, []byte, error)
	Write(ctx context.Context, typ MessageType, data []byte) error
	Ping(ctx context.Context) error
}

// Dialer opens the fixed target (the container:port frozen at mint time). It
// is called once per stream; in production it is an SSH direct-tcpip dial.
type Dialer func(ctx context.Context) (net.Conn, error)

// EndReason mirrors terminal_end_reason (reused enum, data-dictionary §10.7).
type EndReason string

// Tunnel session end reasons (ADR-032).
const (
	EndUserClose   EndReason = "user_close"
	EndIdleTimeout EndReason = "idle_timeout"
	EndMaxDuration EndReason = "max_duration"
	EndDisconnect  EndReason = "disconnect"
)

// Options bounds a session; zero values fall back to defaults.
type Options struct {
	IdleTimeout time.Duration
	MaxDuration time.Duration
	Heartbeat   time.Duration
	MaxStreams  int
	// OnHeartbeat persists liveness after a successful WebSocket ping. It is
	// best-effort on storage errors (the caller returns true), but returning
	// false means the durable session has already been closed and cuts the
	// socket too.
	OnHeartbeat func(context.Context) bool
	// Cancel ends the session from outside, with the reason to report — a
	// revoked grant, an operator closing the tunnel from the dashboard. A nil
	// channel simply never fires, which is why the zero value is inert.
	//
	// It carries a reason rather than being a bare signal because the ONE
	// thing the developer must get out of an automatic close is why it
	// happened (ADR-045 §5): cancelling the context instead would report
	// `disconnect` and read as a network glitch.
	Cancel <-chan EndReason
}

// Defaults (§24.4, ADR-032).
//
// The idle timeout is 30 min rather than the terminal's 15 (ADR-045 §5): 15 was
// calibrated on HTTP debugging, where traffic is near-continuous, and it cuts
// interactive database work — running a query, reading the result and thinking
// is routinely fifteen minutes of silence. The terminal keeps its own value; a
// shell left open is a different risk from a forwarded port left open.
const (
	DefaultIdleTimeout = 30 * time.Minute
	DefaultMaxDuration = 4 * time.Hour
	defaultHeartbeat   = 20 * time.Second
	defaultMaxStreams  = 32
)

type ctrl struct {
	T    string `json:"t"`
	ID   uint32 `json:"id"`
	Code string `json:"code,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// Bridge multiplexes TCP streams between conn and targets dialed by dial,
// until the client disconnects or a bound is hit. It always tears every
// stream down before returning (the guaranteed kill of §24.4) and reports why.
func Bridge(ctx context.Context, conn Conn, dial Dialer, opts Options) EndReason {
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = DefaultMaxDuration
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = defaultHeartbeat
	}
	if opts.MaxStreams <= 0 {
		opts.MaxStreams = defaultMaxStreams
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	m := &mux{
		conn:     conn,
		dial:     dial,
		streams:  map[uint32]net.Conn{},
		activity: make(chan struct{}, 1),
		max:      opts.MaxStreams,
	}
	defer m.closeAll()

	// Reader goroutine turns incoming frames into actions; it signals the end
	// reason when the client goes away.
	done := make(chan EndReason, 1)
	go m.readLoop(ctx, done)

	idle := time.NewTimer(opts.IdleTimeout)
	defer idle.Stop()
	maxTimer := time.NewTimer(opts.MaxDuration)
	defer maxTimer.Stop()
	beat := time.NewTicker(opts.Heartbeat)
	defer beat.Stop()

	for {
		select {
		case <-ctx.Done():
			return EndDisconnect
		case reason := <-done:
			return reason
		case reason := <-opts.Cancel:
			return reason
		case <-m.activity:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(opts.IdleTimeout)
		case <-idle.C:
			return EndIdleTimeout
		case <-maxTimer.C:
			return EndMaxDuration
		case <-beat.C:
			if err := conn.Ping(ctx); err != nil {
				return EndDisconnect
			}
			if opts.OnHeartbeat != nil && !opts.OnHeartbeat(ctx) {
				return EndDisconnect
			}
		}
	}
}

type mux struct {
	conn     Conn
	dial     Dialer
	max      int
	mu       sync.Mutex
	streams  map[uint32]net.Conn
	writeMu  sync.Mutex
	activity chan struct{}
}

func (m *mux) touch() {
	select {
	case m.activity <- struct{}{}:
	default:
	}
}

func (m *mux) sendCtrl(ctx context.Context, c ctrl) error {
	data, _ := json.Marshal(c)
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	return m.conn.Write(ctx, MessageText, data)
}

func (m *mux) sendData(ctx context.Context, id uint32, p []byte) error {
	frame := make([]byte, 4+len(p))
	binary.BigEndian.PutUint32(frame, id)
	copy(frame[4:], p)
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	return m.conn.Write(ctx, MessageBinary, frame)
}

func (m *mux) readLoop(ctx context.Context, done chan<- EndReason) {
	for {
		typ, data, err := m.conn.Read(ctx)
		if err != nil {
			if errors.Is(err, ErrClientClosed) {
				done <- EndUserClose
			} else {
				done <- EndDisconnect
			}
			return
		}
		m.touch()
		switch typ {
		case MessageText:
			var c ctrl
			if json.Unmarshal(data, &c) != nil {
				continue
			}
			switch c.T {
			case "open":
				m.openStream(ctx, c.ID)
			case "eof", "close":
				m.closeStream(c.ID)
			}
		case MessageBinary:
			if len(data) < 4 {
				continue
			}
			id := binary.BigEndian.Uint32(data[:4])
			if s := m.get(id); s != nil {
				_, _ = s.Write(data[4:])
			}
		}
	}
}

func (m *mux) openStream(ctx context.Context, id uint32) {
	m.mu.Lock()
	if len(m.streams) >= m.max {
		m.mu.Unlock()
		_ = m.sendCtrl(ctx, ctrl{T: "open_err", ID: id, Code: "limit", Msg: "too many concurrent streams"})
		return
	}
	m.mu.Unlock()

	target, err := m.dial(ctx)
	if err != nil {
		_ = m.sendCtrl(ctx, ctrl{T: "open_err", ID: id, Code: "dial_failed", Msg: err.Error()})
		return
	}
	m.mu.Lock()
	m.streams[id] = target
	m.mu.Unlock()
	_ = m.sendCtrl(ctx, ctrl{T: "open_ok", ID: id})

	// target → client, as binary frames tagged with the stream id.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := target.Read(buf)
			if n > 0 {
				m.touch()
				if werr := m.sendData(ctx, id, buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		_ = m.sendCtrl(ctx, ctrl{T: "close", ID: id})
		m.closeStream(id)
	}()
}

func (m *mux) get(id uint32) net.Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[id]
}

func (m *mux) closeStream(id uint32) {
	m.mu.Lock()
	s := m.streams[id]
	delete(m.streams, id)
	m.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

func (m *mux) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.streams {
		_ = s.Close()
		delete(m.streams, id)
	}
}

var _ = io.EOF
