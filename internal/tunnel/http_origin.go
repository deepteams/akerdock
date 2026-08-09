package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

type httpOpenResult struct {
	conn net.Conn
	err  error
}

// HTTPOrigin is the agent side of the ADR-061 HTTP v2 wire. It asks the CLI
// to open a data request on the control stream, then hands that independent
// HTTP/2 or HTTP/3 stream to the reverse proxy as a net.Conn.
type HTTPOrigin struct {
	control *LineControl

	mu      sync.Mutex
	nextID  uint32
	pending map[uint32]chan httpOpenResult
	streams map[uint32]*trackedHTTPConn
	closed  bool

	activity chan struct{}
	done     chan struct{}
	doneOnce sync.Once

	admissionMu  sync.Mutex
	streamSlots  chan struct{}
	maxQueued    int
	queued       int
	queueWait    time.Duration
	onStreamWait func(time.Duration, error)
}

// NewHTTPOrigin configures admission before the first HTTP data stream opens.
func NewHTTPOrigin(control *LineControl, opts Options) *HTTPOrigin {
	if opts.MaxStreams <= 0 {
		opts.MaxStreams = DefaultMaxStreams
	}
	if opts.StreamQueueTimeout <= 0 {
		opts.StreamQueueTimeout = defaultStreamQueueTimeout
	}
	return &HTTPOrigin{
		control:      control,
		pending:      make(map[uint32]chan httpOpenResult),
		streams:      make(map[uint32]*trackedHTTPConn),
		activity:     make(chan struct{}, 1),
		done:         make(chan struct{}),
		streamSlots:  make(chan struct{}, opts.MaxStreams),
		maxQueued:    max(opts.MaxPendingStreams, 0),
		queueWait:    opts.StreamQueueTimeout,
		onStreamWait: opts.OnStreamWait,
	}
}

// Run owns the control reader and session timers until either side closes.
func (o *HTTPOrigin) Run(ctx context.Context, opts Options) EndReason {
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

	controlDone := make(chan EndReason, 1)
	go o.readControl(controlDone)
	idle := time.NewTimer(opts.IdleTimeout)
	defer idle.Stop()
	maximum := time.NewTimer(opts.MaxDuration)
	defer maximum.Stop()
	heartbeat := time.NewTicker(opts.Heartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return EndDisconnect
		case reason := <-controlDone:
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
		case <-maximum.C:
			return EndMaxDuration
		case <-heartbeat.C:
			if err := o.control.Send(ctx, HTTPControlFrame{Type: "ping"}); err != nil {
				return EndDisconnect
			}
			// Same rule as the WebSocket rung's: the durable session ended
			// elsewhere, and the beat brings back the word it ended with.
			if ended := sessionBeat(ctx, opts); ended != "" {
				return ended
			}
		}
	}
}

// OpenStream requests one client-initiated HTTP data stream.
func (o *HTTPOrigin) OpenStream(ctx context.Context) (net.Conn, error) {
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
	wait := make(chan httpOpenResult, 1)
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		o.releaseStream()
		return nil, ErrOriginClosed
	}
	o.nextID++
	id := o.nextID
	o.pending[id] = wait
	o.mu.Unlock()

	if err := o.control.Send(ctx, HTTPControlFrame{Type: "open", ID: id}); err != nil {
		o.dropPending(id)
		return nil, err
	}
	timer := time.NewTimer(openTimeout)
	defer timer.Stop()
	select {
	case result := <-wait:
		return result.conn, result.err
	case <-timer.C:
		o.dropPending(id)
		return nil, fmt.Errorf("tunnel: no HTTP stream within %s", openTimeout)
	case <-ctx.Done():
		o.dropPending(id)
		return nil, ctx.Err()
	case <-o.done:
		o.dropPending(id)
		return nil, ErrOriginClosed
	}
}

// WantsStream checks whether id is an outstanding open before an HTTP handler
// commits its successful streaming response.
func (o *HTTPOrigin) WantsStream(id uint32) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.pending[id]
	return ok && !o.closed
}

// AttachStream resolves an outstanding open with the accepted HTTP request.
func (o *HTTPOrigin) AttachStream(id uint32, conn net.Conn) error {
	o.mu.Lock()
	wait := o.pending[id]
	if wait == nil || o.closed {
		o.mu.Unlock()
		return errors.New("tunnel: HTTP stream is not pending")
	}
	delete(o.pending, id)
	tracked := &trackedHTTPConn{Conn: conn, id: id, origin: o}
	o.streams[id] = tracked
	o.mu.Unlock()
	wait <- httpOpenResult{conn: tracked}
	return nil
}

func (o *HTTPOrigin) readControl(done chan<- EndReason) {
	for {
		frame, err := o.control.Receive()
		if err != nil {
			done <- EndDisconnect
			return
		}
		switch frame.Type {
		case "open_err":
			o.resolvePending(frame.ID, fmt.Errorf("tunnel: open refused: %s %s", frame.Code, frame.Msg))
		case "pong":
			// Receipt itself proves the control path is alive. Heartbeats do not
			// count as visitor traffic for the idle timeout.
		case "session_close":
			if frame.Reason == "" {
				done <- EndUserClose
			} else {
				done <- EndReason(frame.Reason)
			}
			return
		}
	}
}

func (o *HTTPOrigin) resolvePending(id uint32, err error) {
	o.mu.Lock()
	wait := o.pending[id]
	if wait != nil {
		delete(o.pending, id)
	}
	o.mu.Unlock()
	if wait != nil {
		o.releaseStream()
		wait <- httpOpenResult{err: err}
	}
}

func (o *HTTPOrigin) dropPending(id uint32) {
	o.mu.Lock()
	_, ok := o.pending[id]
	delete(o.pending, id)
	o.mu.Unlock()
	if ok {
		o.releaseStream()
	}
}

func (o *HTTPOrigin) removeStream(id uint32) {
	o.mu.Lock()
	_, ok := o.streams[id]
	delete(o.streams, id)
	o.mu.Unlock()
	if ok {
		o.releaseStream()
	}
}

func (o *HTTPOrigin) touch() {
	select {
	case o.activity <- struct{}{}:
	default:
	}
}

func (o *HTTPOrigin) acquireStream(ctx context.Context) error {
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

func (o *HTTPOrigin) releaseStream() { <-o.streamSlots }

func (o *HTTPOrigin) observeStreamWait(wait time.Duration, err error) {
	if o.onStreamWait != nil {
		o.onStreamWait(wait, err)
	}
}

// SendClose delivers the policy reason before the HTTP response ends.
func (o *HTTPOrigin) SendClose(ctx context.Context, reason EndReason) error {
	return o.control.Send(ctx, HTTPControlFrame{Type: "session_close", Reason: string(reason)})
}

// Close ends the control request after its terminal reason was flushed.
func (o *HTTPOrigin) Close() error { return o.control.Close() }

func (o *HTTPOrigin) shutdown() {
	o.doneOnce.Do(func() { close(o.done) })
	o.mu.Lock()
	o.closed = true
	streams := o.streams
	o.streams = make(map[uint32]*trackedHTTPConn)
	pending := o.pending
	o.pending = make(map[uint32]chan httpOpenResult)
	o.mu.Unlock()
	for _, stream := range streams {
		_ = stream.Close()
		o.releaseStream()
	}
	for _, wait := range pending {
		o.releaseStream()
		wait <- httpOpenResult{err: ErrOriginClosed}
	}
}

type trackedHTTPConn struct {
	net.Conn
	id     uint32
	origin *HTTPOrigin
	once   sync.Once
	err    error
}

func (c *trackedHTTPConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.origin.touch()
	}
	return n, err
}

func (c *trackedHTTPConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.origin.touch()
	}
	return n, err
}

func (c *trackedHTTPConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.origin.removeStream(c.id)
	})
	return c.err
}
