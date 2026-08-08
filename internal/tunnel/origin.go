package tunnel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Origin is the originating side of the mux — the mirror of Bridge. Bridge
// receives "open" and dials a fixed target; Origin SENDS "open" and hands the
// caller a net.Conn for each stream, which is the shape a reverse proxy's
// DialContext wants (ADR-060 §2: the agent originates one stream per visitor
// connection; the laptop side is a plain Bridge dialing 127.0.0.1).
//
// Same wire as Bridge (text control frames + [u32 id][payload] binary), same
// session invariants (idle timeout, max duration, heartbeat, cancel-with-
// reason, guaranteed teardown). Streams share the underlying socket, so a
// stalled consumer stalls its siblings — the protocol's accepted
// head-of-line limitation (ADR-032).
type Origin struct {
	conn Conn

	mu      sync.Mutex
	nextID  uint32
	streams map[uint32]*originStream
	pending map[uint32]chan error

	writeMu  sync.Mutex
	activity chan struct{}

	done     chan struct{} // closed when Run returns; OpenStream fails after
	doneOnce sync.Once
}

// originStream pairs the caller-facing net.Conn with the pipe end the read
// loop feeds. net.Pipe is synchronous: a consumer that stops reading blocks
// the mux read loop, which is the same back-pressure Bridge applies.
type originStream struct {
	local  net.Conn // returned to the caller
	remote net.Conn // fed by the read loop, drained by the pump
}

// NewOrigin wraps an established, already-authenticated connection.
func NewOrigin(conn Conn) *Origin {
	return &Origin{
		conn:     conn,
		streams:  map[uint32]*originStream{},
		pending:  map[uint32]chan error{},
		activity: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// openTimeout bounds the wait for the peer's open_ok — the peer only has a
// loopback dial to perform.
const openTimeout = 15 * time.Second

// ErrOriginClosed is what OpenStream returns once the session ended.
var ErrOriginClosed = errors.New("tunnel: session closed")

// Run pumps the connection until it dies or a bound is hit, then tears every
// stream down and reports why. It mirrors Bridge's loop; the read side is
// started here, so call OpenStream only while Run is running.
func (o *Origin) Run(ctx context.Context, opts Options) EndReason {
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
	defer o.shutdown()

	done := make(chan EndReason, 1)
	go o.readLoop(ctx, done)

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
		case <-o.activity:
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
			if err := o.conn.Ping(ctx); err != nil {
				return EndDisconnect
			}
			if opts.OnHeartbeat != nil && !opts.OnHeartbeat(ctx) {
				return EndDisconnect
			}
		}
	}
}

// OpenStream asks the peer to dial its fixed target and returns the stream as
// a net.Conn. The stream is registered BEFORE the open travels, so data
// racing ahead of the caller lands in the pipe rather than on the floor.
func (o *Origin) OpenStream(ctx context.Context) (net.Conn, error) {
	select {
	case <-o.done:
		return nil, ErrOriginClosed
	default:
	}

	local, remote := net.Pipe()
	wait := make(chan error, 1)
	o.mu.Lock()
	o.nextID++
	id := o.nextID
	o.streams[id] = &originStream{local: local, remote: remote}
	o.pending[id] = wait
	o.mu.Unlock()

	if err := o.sendCtrl(ctx, ctrl{T: "open", ID: id}); err != nil {
		o.dropStream(id)
		return nil, err
	}

	timer := time.NewTimer(openTimeout)
	defer timer.Stop()
	select {
	case err := <-wait:
		if err != nil {
			o.dropStream(id)
			return nil, err
		}
	case <-timer.C:
		o.dropStream(id)
		return nil, fmt.Errorf("tunnel: no answer to open within %s", openTimeout)
	case <-ctx.Done():
		o.dropStream(id)
		return nil, ctx.Err()
	case <-o.done:
		o.dropStream(id)
		return nil, ErrOriginClosed
	}

	// caller → peer: drain what the caller writes into binary frames.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := remote.Read(buf)
			if n > 0 {
				o.touch()
				if werr := o.sendData(context.Background(), id, buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		_ = o.sendCtrl(context.Background(), ctrl{T: "eof", ID: id})
		o.closeStream(id)
	}()
	return local, nil
}

func (o *Origin) touch() {
	select {
	case o.activity <- struct{}{}:
	default:
	}
}

func (o *Origin) sendCtrl(ctx context.Context, c ctrl) error {
	data, _ := json.Marshal(c)
	o.writeMu.Lock()
	defer o.writeMu.Unlock()
	return o.conn.Write(ctx, MessageText, data)
}

func (o *Origin) sendData(ctx context.Context, id uint32, p []byte) error {
	frame := make([]byte, 4+len(p))
	binary.BigEndian.PutUint32(frame, id)
	copy(frame[4:], p)
	o.writeMu.Lock()
	defer o.writeMu.Unlock()
	return o.conn.Write(ctx, MessageBinary, frame)
}

func (o *Origin) readLoop(ctx context.Context, done chan<- EndReason) {
	for {
		typ, data, err := o.conn.Read(ctx)
		if err != nil {
			if errors.Is(err, ErrClientClosed) {
				done <- EndUserClose
			} else {
				done <- EndDisconnect
			}
			return
		}
		o.touch()
		switch typ {
		case MessageText:
			var c ctrl
			if json.Unmarshal(data, &c) != nil {
				continue
			}
			switch c.T {
			case "open_ok":
				o.resolvePending(c.ID, nil)
			case "open_err":
				o.resolvePending(c.ID, fmt.Errorf("tunnel: open refused: %s %s", c.Code, c.Msg))
			case "eof", "close":
				o.closeStream(c.ID)
			}
		case MessageBinary:
			if len(data) < 4 {
				continue
			}
			id := binary.BigEndian.Uint32(data[:4])
			o.mu.Lock()
			s := o.streams[id]
			o.mu.Unlock()
			if s != nil {
				_, _ = s.remote.Write(data[4:])
			}
		}
	}
}

func (o *Origin) resolvePending(id uint32, err error) {
	o.mu.Lock()
	wait := o.pending[id]
	delete(o.pending, id)
	o.mu.Unlock()
	if wait != nil {
		wait <- err
	}
}

// closeStream tears one stream down; the caller-facing conn reads EOF.
func (o *Origin) closeStream(id uint32) {
	o.mu.Lock()
	s := o.streams[id]
	delete(o.streams, id)
	delete(o.pending, id)
	o.mu.Unlock()
	if s != nil {
		_ = s.remote.Close()
		_ = s.local.Close()
	}
}

// dropStream is closeStream for an open that never completed.
func (o *Origin) dropStream(id uint32) { o.closeStream(id) }

func (o *Origin) shutdown() {
	o.doneOnce.Do(func() { close(o.done) })
	o.mu.Lock()
	streams := o.streams
	o.streams = map[uint32]*originStream{}
	pending := o.pending
	o.pending = map[uint32]chan error{}
	o.mu.Unlock()
	for _, s := range streams {
		_ = s.remote.Close()
		_ = s.local.Close()
	}
	for _, wait := range pending {
		select {
		case wait <- ErrOriginClosed:
		default:
		}
	}
}
