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
// reason, guaranteed teardown). Each stream has a bounded receive queue so a
// stalled consumer can lose only its own stream, never block sibling data or
// control frames (ADR-061).
type Origin struct {
	conn Conn

	mu      sync.Mutex
	nextID  uint32
	streams map[uint32]*originStream
	pending map[uint32]chan error
	closed  bool

	writeMu  sync.Mutex
	activity chan struct{}

	done     chan struct{} // closed when Run returns; OpenStream fails after
	doneOnce sync.Once

	admissionMu  sync.Mutex
	streamSlots  chan struct{}
	maxQueued    int
	queued       int
	queueWait    time.Duration
	onStreamWait func(time.Duration, error)
}

// originStream pairs the caller-facing net.Conn with the pipe end fed by its
// own bounded queue. net.Pipe remains synchronous, but only this stream's pump
// can block on it.
type originStream struct {
	local  net.Conn // returned to the caller
	remote net.Conn // fed by the queue pump, drained by the caller
	inbox  *streamQueue
}

// NewOrigin wraps an established, already-authenticated connection with the
// default active-stream bound and no waiting queue.
func NewOrigin(conn Conn) *Origin {
	return NewOriginWithOptions(conn, Options{})
}

// NewOriginWithOptions wraps a connection and configures admission before the
// first stream can be opened. MaxStreams bounds active streams;
// MaxPendingStreams bounds callers waiting for one of those slots.
func NewOriginWithOptions(conn Conn, opts Options) *Origin {
	if opts.MaxStreams <= 0 {
		opts.MaxStreams = DefaultMaxStreams
	}
	if opts.StreamQueueTimeout <= 0 {
		opts.StreamQueueTimeout = defaultStreamQueueTimeout
	}
	return &Origin{
		conn:         conn,
		streams:      map[uint32]*originStream{},
		pending:      map[uint32]chan error{},
		activity:     make(chan struct{}, 1),
		done:         make(chan struct{}),
		streamSlots:  make(chan struct{}, opts.MaxStreams),
		maxQueued:    max(opts.MaxPendingStreams, 0),
		queueWait:    opts.StreamQueueTimeout,
		onStreamWait: opts.OnStreamWait,
	}
}

// openTimeout bounds the wait for the peer's open_ok — the peer only has a
// loopback dial to perform.
const (
	openTimeout               = 15 * time.Second
	defaultStreamQueueTimeout = 30 * time.Second
)

var (
	// ErrOriginClosed is what OpenStream returns once the session ended.
	ErrOriginClosed = errors.New("tunnel: session closed")
	// ErrOriginQueueFull refuses work beyond the configured pending bound.
	ErrOriginQueueFull = errors.New("tunnel: stream queue full")
	// ErrOriginQueueTimeout reports a request that could not acquire an active
	// stream slot before its queue deadline.
	ErrOriginQueueTimeout = errors.New("tunnel: stream queue timeout")
)

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
			// A close decided on another replica reaches this rung as a beat that
			// matched no row, and the reason is the one the row carries.
			if ended := sessionBeat(ctx, opts); ended != "" {
				return ended
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
	waitStarted := time.Now()
	if err := o.acquireStream(ctx); err != nil {
		o.observeStreamWait(time.Since(waitStarted), err)
		return nil, err
	}
	o.observeStreamWait(time.Since(waitStarted), nil)
	// A slot and session shutdown may become ready together. Never register
	// new work after shutdown won the race.
	select {
	case <-o.done:
		o.releaseStream()
		return nil, ErrOriginClosed
	default:
	}

	local, remote := net.Pipe()
	wait := make(chan error, 1)
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		_ = remote.Close()
		_ = local.Close()
		o.releaseStream()
		return nil, ErrOriginClosed
	}
	o.nextID++
	id := o.nextID
	stream := &originStream{local: local, remote: remote, inbox: newStreamQueue()}
	o.streams[id] = stream
	o.pending[id] = wait
	o.mu.Unlock()
	go func() {
		if err := stream.inbox.pump(remote); err != nil {
			o.closeStream(id)
		}
	}()

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

// acquireStream either takes an active slot immediately or joins the bounded
// wait set. New arrivals cannot bypass existing waiters, which keeps a burst
// from starving its oldest requests.
func (o *Origin) acquireStream(ctx context.Context) error {
	o.admissionMu.Lock()
	if o.queued == 0 {
		select {
		case o.streamSlots <- struct{}{}:
			o.admissionMu.Unlock()
			return nil
		default:
		}
	}
	if o.queued >= o.maxQueued {
		o.admissionMu.Unlock()
		return ErrOriginQueueFull
	}
	o.queued++
	o.admissionMu.Unlock()

	defer func() {
		o.admissionMu.Lock()
		o.queued--
		o.admissionMu.Unlock()
	}()
	timer := time.NewTimer(o.queueWait)
	defer timer.Stop()
	select {
	case o.streamSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-o.done:
		return ErrOriginClosed
	case <-timer.C:
		return ErrOriginQueueTimeout
	}
}

func (o *Origin) releaseStream() { <-o.streamSlots }

func (o *Origin) observeStreamWait(wait time.Duration, err error) {
	if o.onStreamWait != nil {
		o.onStreamWait(wait, err)
	}
}

func (o *Origin) touch() {
	select {
	case o.activity <- struct{}{}:
	default:
	}
}

func (o *Origin) sendCtrl(ctx context.Context, c ctrl) error {
	data, _ := json.Marshal(c)
	return writeTunnelMessage(ctx, o.conn, &o.writeMu, MessageText, data)
}

func (o *Origin) sendData(ctx context.Context, id uint32, p []byte) error {
	frame := make([]byte, 4+len(p))
	binary.BigEndian.PutUint32(frame, id)
	copy(frame[4:], p)
	return writeTunnelMessage(ctx, o.conn, &o.writeMu, MessageBinary, frame)
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
			if s != nil && !s.inbox.enqueue(data[4:]) {
				// A peer that outruns this stream's bounded receive queue loses
				// only this stream. Keep reading so siblings and close frames move.
				o.closeStream(id)
				go func() { _ = o.sendCtrl(context.Background(), ctrl{T: "close", ID: id}) }()
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
		s.inbox.close()
		_ = s.remote.Close()
		_ = s.local.Close()
		o.releaseStream()
	}
}

// dropStream is closeStream for an open that never completed.
func (o *Origin) dropStream(id uint32) { o.closeStream(id) }

func (o *Origin) shutdown() {
	o.doneOnce.Do(func() { close(o.done) })
	o.mu.Lock()
	o.closed = true
	streams := o.streams
	o.streams = map[uint32]*originStream{}
	pending := o.pending
	o.pending = map[uint32]chan error{}
	o.mu.Unlock()
	for _, s := range streams {
		s.inbox.close()
		_ = s.remote.Close()
		_ = s.local.Close()
		o.releaseStream()
	}
	for _, wait := range pending {
		select {
		case wait <- ErrOriginClosed:
		default:
		}
	}
}
