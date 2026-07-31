package agentwire

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/coder/websocket"
	cerrdefs "github.com/containerd/errdefs"
)

// StreamBuffer bounds one stream's undelivered chunks. A channel is shared by
// every command to a server: rather than stall it behind one slow consumer,
// an overflowing stream is killed with an explicit error.
const StreamBuffer = 512

// ChunkSize bounds one stream chunk; small enough to interleave fairly with
// other traffic on a shared channel, large enough to keep log following
// cheap.
const ChunkSize = 32 << 10

// Unavailable is the mandatory-agent failure mode (ADR-051): the channel is
// not there, the operation cannot run, and the remedy is the agent's
// reconciliation — never a silent fallback.
func Unavailable(why string) error {
	return fmt.Errorf("agent channel %s: %w", why, cerrdefs.ErrUnavailable)
}

// call is one command in flight.
type call struct {
	res    chan Result
	chunks chan StreamChunk // nil for unary commands
}

// Conn routes typed commands over one live channel and matches results and
// stream chunks back by id. It is side-agnostic: the api process runs one per
// agent WebSocket, the relay client (ADR-052 §8) one per bridged server. The
// OWNER runs the read loop and feeds received frames in through
// DeliverResult/DeliverChunk; writes are serialized here.
type Conn struct {
	ctx  context.Context // the owning connection's lifetime
	conn *websocket.Conn
	// Record, when set, feeds the docker-ops counter: one increment per
	// command or stream open, by method and outcome.
	Record func(method, outcome string)

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	calls   map[int64]*call
}

// NewConn wraps one live WebSocket whose lifetime is ctx.
func NewConn(ctx context.Context, conn *websocket.Conn) *Conn {
	return &Conn{ctx: ctx, conn: conn, calls: map[int64]*call{}}
}

// Done reports the connection's end — the owner's ctx.
func (c *Conn) Done() <-chan struct{} { return c.ctx.Done() }

// WriteFrame serializes one frame onto the socket; 10 s bounds a stalled
// peer, not the command it carries.
func (c *Conn) WriteFrame(f Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ioCtx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	return c.conn.Write(ioCtx, websocket.MessageText, data)
}

func (c *Conn) start(stream bool) (int64, *call) {
	cl := &call{res: make(chan Result, 1)}
	if stream {
		cl.chunks = make(chan StreamChunk, StreamBuffer)
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.calls[id] = cl
	c.mu.Unlock()
	return id, cl
}

func (c *Conn) finish(id int64) {
	c.mu.Lock()
	delete(c.calls, id)
	c.mu.Unlock()
}

func (c *Conn) lookup(id int64) *call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[id]
}

// CancelRemote tells the peer to abort the command; best-effort — a broken
// socket is its own cancellation.
func (c *Conn) CancelRemote(id int64) {
	_ = c.WriteFrame(Frame{Type: FrameCancel, Cancel: id})
}

func (c *Conn) observe(method, outcome string) {
	if c.Record != nil {
		c.Record(method, outcome)
	}
}

// DeliverResult routes a result frame to its waiting call.
func (c *Conn) DeliverResult(res *Result) {
	if res == nil {
		return
	}
	if cl := c.lookup(res.ID); cl != nil {
		select {
		case cl.res <- *res:
		default: // duplicate result; the first one won
		}
	}
}

// DeliverChunk routes a stream frame. A consumer that cannot keep up loses
// its stream — with an explicit error, and with the peer told to stop — so
// one slow log follower never stalls the whole channel.
func (c *Conn) DeliverChunk(chunk *StreamChunk) {
	if chunk == nil {
		return
	}
	cl := c.lookup(chunk.ID)
	if cl == nil || cl.chunks == nil {
		return
	}
	select {
	case cl.chunks <- *chunk:
	default:
		c.CancelRemote(chunk.ID)
		select {
		case <-cl.chunks: // shed one to make room for the error
		default:
		}
		select {
		case cl.chunks <- StreamChunk{ID: chunk.ID, Err: &Error{
			Code: CodeUnavailable, Message: "stream consumer too slow",
		}}:
		default:
		}
	}
}

// Command sends one typed command and waits for its result.
func (c *Conn) Command(ctx context.Context, method string, params any) (json.RawMessage, error) {
	cmd, id, cl, err := c.open(method, params, false)
	if err != nil {
		return nil, err
	}
	defer c.finish(id)
	if err := c.WriteFrame(Frame{Type: FrameCommand, Cmd: cmd}); err != nil {
		c.observe(method, CodeUnavailable)
		return nil, Unavailable("write failed")
	}
	select {
	case <-ctx.Done():
		c.CancelRemote(id)
		c.observe(method, CodeCanceled)
		return nil, ctx.Err()
	case <-c.ctx.Done():
		c.observe(method, CodeUnavailable)
		return nil, Unavailable("closed")
	case res := <-cl.res:
		if res.Err != nil {
			c.observe(method, res.Err.Code)
			return nil, res.Err.Err()
		}
		c.observe(method, "ok")
		return res.Body, nil
	}
}

// Stream sends one streaming command: the result acknowledges the open, then
// chunks flow until EOF, error or Close.
func (c *Conn) Stream(ctx context.Context, method string, params any) (io.ReadCloser, error) {
	cmd, id, cl, err := c.open(method, params, true)
	if err != nil {
		return nil, err
	}
	if err := c.WriteFrame(Frame{Type: FrameCommand, Cmd: cmd}); err != nil {
		c.finish(id)
		c.observe(method, CodeUnavailable)
		return nil, Unavailable("write failed")
	}
	select {
	case <-ctx.Done():
		c.CancelRemote(id)
		c.finish(id)
		c.observe(method, CodeCanceled)
		return nil, ctx.Err()
	case <-c.ctx.Done():
		c.finish(id)
		c.observe(method, CodeUnavailable)
		return nil, Unavailable("closed")
	case res := <-cl.res:
		if res.Err != nil {
			c.finish(id)
			c.observe(method, res.Err.Code)
			return nil, res.Err.Err()
		}
	}
	c.observe(method, "ok")
	return &stream{conn: c, id: id, call: cl, ctx: ctx}, nil
}

func (c *Conn) open(method string, params any, streamed bool) (*Command, int64, *call, error) {
	var raw json.RawMessage
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return nil, 0, nil, err
		}
		raw = data
	}
	id, cl := c.start(streamed)
	return &Command{ID: id, Method: method, Params: raw}, id, cl, nil
}

// stream adapts a command's chunk flow to io.ReadCloser.
type stream struct {
	conn *Conn
	id   int64
	call *call
	ctx  context.Context

	buf    []byte
	err    error
	closed bool
}

func (s *stream) Read(p []byte) (int, error) {
	for {
		if len(s.buf) > 0 {
			n := copy(p, s.buf)
			s.buf = s.buf[n:]
			return n, nil
		}
		if s.err != nil {
			return 0, s.err
		}
		select {
		case <-s.ctx.Done():
			s.err = s.ctx.Err()
			return 0, s.err
		case <-s.conn.ctx.Done():
			s.err = Unavailable("closed")
			return 0, s.err
		case chunk := <-s.call.chunks:
			switch {
			case chunk.Err != nil:
				s.err = chunk.Err.Err()
			case chunk.EOF:
				s.err = io.EOF
			}
			s.buf = chunk.Data
		}
	}
}

func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.err == nil || (s.err != io.EOF && s.ctx.Err() == nil) {
		s.conn.CancelRemote(s.id)
	}
	s.conn.finish(s.id)
	return nil
}

// IsStreamMethod reports whether the method answers with a chunk stream after
// its acknowledging result — what a relay must know to bridge it.
func IsStreamMethod(method string) bool {
	switch method {
	case MethodContainerLogs, MethodImagePull, MethodImagePush, MethodEvents:
		return true
	}
	return false
}

// PumpReader forwards a reader as stream chunks for command id until EOF or
// error, then sends the terminal chunk. brokeCleanly is decided by ctx: a
// canceled pump reports EOF-less silence, not a daemon error.
func PumpReader(ctx context.Context, id int64, r io.Reader, write func(Frame) error) {
	buf := make([]byte, ChunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			if write(Frame{Type: FrameStream, Chunk: &StreamChunk{ID: id, Data: data}}) != nil {
				return
			}
		}
		if err != nil {
			chunk := StreamChunk{ID: id, EOF: true}
			if err != io.EOF && ctx.Err() == nil {
				chunk = StreamChunk{ID: id, Err: WireError(err)}
			}
			_ = write(Frame{Type: FrameStream, Chunk: &chunk})
			return
		}
	}
}
