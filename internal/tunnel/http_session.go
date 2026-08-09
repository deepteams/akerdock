package tunnel

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// ErrSessionStreamLimit refuses a stream beyond the session's admission bound.
// It is answered immediately rather than queued: unlike ingress, where the
// queue absorbs a page load's burst before the browser gives up, an egress
// stream is one TCP connection a local client is already holding open — and a
// client that is told "no" reconnects, while a client left waiting stalls.
var ErrSessionStreamLimit = errors.New("tunnel: too many concurrent streams")

// HTTPSession is the server end of a CLIENT-initiated HTTP attach (ADR-064
// §4). It is the mirror image of HTTPOrigin: there, the server asks the client
// to open a stream when a visitor arrives, so the control stream carries opens;
// here the client opens on demand — a local accept — and the session request
// carries nothing but the session itself: its bounds, its liveness and, at the
// end, why it stopped.
//
// It exists because the session must outlive the moments when no stream is
// open. Without it, the attach token (60 s) would have to survive until the
// first local connection, nothing would detect a laptop that vanished, and the
// developer would learn a revoked grant only on their next connection attempt.
type HTTPSession struct {
	control *LineControl

	mu      sync.Mutex
	streams map[*sessionStream]struct{}
	closed  bool

	slots    chan struct{}
	activity chan struct{}
	done     chan struct{}
	doneOnce sync.Once
}

// NewHTTPSession configures admission before the first data stream attaches.
func NewHTTPSession(control *LineControl, opts Options) *HTTPSession {
	if opts.MaxStreams <= 0 {
		opts.MaxStreams = DefaultMaxStreams
	}
	return &HTTPSession{
		control:  control,
		streams:  make(map[*sessionStream]struct{}),
		slots:    make(chan struct{}, opts.MaxStreams),
		activity: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// Run owns the session request and its timers until either side ends it. The
// bounds are the ones the WebSocket bridge enforces, applied to the same
// session — only the transport changed.
func (s *HTTPSession) Run(ctx context.Context, opts Options) EndReason {
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
	defer s.shutdown()

	controlDone := make(chan EndReason, 1)
	go s.readControl(controlDone)
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
		case <-s.activity:
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
			// The ping proves the path is alive even while no stream carries a
			// byte, which is most of a port-forward's life.
			if err := s.control.Send(ctx, HTTPControlFrame{Type: "ping"}); err != nil {
				return EndDisconnect
			}
			if opts.OnHeartbeat != nil && !opts.OnHeartbeat(ctx) {
				return EndDisconnect
			}
		}
	}
}

// Admit takes one admission slot and returns conn wrapped so that its traffic
// keeps the session alive and its close gives the slot back. The caller owns
// the returned conn and must Close it.
func (s *HTTPSession) Admit(conn net.Conn) (net.Conn, error) {
	select {
	case <-s.done:
		return nil, ErrOriginClosed
	default:
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return nil, ErrSessionStreamLimit
	}
	tracked := &sessionStream{Conn: conn, session: s}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.slots
		return nil, ErrOriginClosed
	}
	s.streams[tracked] = struct{}{}
	s.mu.Unlock()
	s.touch()
	return tracked, nil
}

// Streams reports how many data streams are attached right now.
func (s *HTTPSession) Streams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

// Done is closed once the session stopped, so a data stream blocked on its
// peer can be woken.
func (s *HTTPSession) Done() <-chan struct{} { return s.done }

// SendClose delivers the end reason before the session request ends. It is the
// HTTP equivalent of the WebSocket close frame's Reason, and the only way the
// developer learns why a tunnel they were not using disappeared (ADR-045 §5).
func (s *HTTPSession) SendClose(ctx context.Context, reason EndReason) error {
	return s.control.Send(ctx, HTTPControlFrame{Type: "session_close", Reason: string(reason)})
}

// Close ends the session request after its terminal reason was flushed.
func (s *HTTPSession) Close() error { return s.control.Close() }

func (s *HTTPSession) readControl(done chan<- EndReason) {
	for {
		frame, err := s.control.Receive()
		if err != nil {
			// A clean end of the request body is a client that finished; any
			// other read failure is a peer that vanished. The CLI announces the
			// former explicitly, so this only has to classify what is left.
			done <- EndDisconnect
			return
		}
		switch frame.Type {
		case "pong":
			// Liveness only: a heartbeat is not traffic, and must not hold the
			// idle timeout open on a forgotten tunnel.
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

func (s *HTTPSession) touch() {
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

func (s *HTTPSession) remove(stream *sessionStream) {
	s.mu.Lock()
	_, ok := s.streams[stream]
	delete(s.streams, stream)
	s.mu.Unlock()
	if ok {
		<-s.slots
	}
}

func (s *HTTPSession) shutdown() {
	s.doneOnce.Do(func() { close(s.done) })
	s.mu.Lock()
	s.closed = true
	streams := s.streams
	s.streams = make(map[*sessionStream]struct{})
	s.mu.Unlock()
	// The guaranteed teardown of §24.4: no forwarded connection survives the
	// session that authorized it.
	for stream := range streams {
		_ = stream.Conn.Close()
		<-s.slots
	}
}

// sessionStream is one attached data stream: it keeps the session's idle timer
// honest and returns its admission slot exactly once.
type sessionStream struct {
	net.Conn
	session *HTTPSession
	once    sync.Once
	err     error
}

func (c *sessionStream) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.session.touch()
	}
	return n, err
}

func (c *sessionStream) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.session.touch()
	}
	return n, err
}

func (c *sessionStream) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.session.remove(c)
	})
	return c.err
}
