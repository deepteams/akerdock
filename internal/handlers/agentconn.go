// Agent command channel, control-plane side (ADR-052): agentConn wraps one
// live v2 WebSocket and implements dockerruntime.CommandSender — typed
// commands out, results and stream chunks routed back by id. AgentConns is
// the per-process registry the runtime factory asks for a server's channel;
// like AgentPresence it is in-memory, accurate within the supported
// single-api topology (the worker relay is the next slice).
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/coder/websocket"
	cerrdefs "github.com/containerd/errdefs"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// agentStreamBuffer bounds one stream's undelivered chunks. The channel is
// shared by every command to that server: rather than stall it behind one
// slow consumer, an overflowing stream is killed with an explicit error.
const agentStreamBuffer = 512

// errAgentUnavailable is the mandatory-agent failure mode (ADR-051): the
// channel is not there, the operation cannot run, the remedy is the agent's
// reconciliation — never a silent fallback.
func errAgentUnavailable(why string) error {
	return fmt.Errorf("agent channel %s: %w", why, cerrdefs.ErrUnavailable)
}

// agentCall is one command in flight.
type agentCall struct {
	res    chan agentwire.Result
	chunks chan agentwire.StreamChunk // nil for unary commands
}

// agentConn is one live v2 channel.
type agentConn struct {
	ctx  context.Context // the connection handler's lifetime
	conn *websocket.Conn

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	calls   map[int64]*agentCall
}

func newAgentConn(ctx context.Context, conn *websocket.Conn) *agentConn {
	return &agentConn{ctx: ctx, conn: conn, calls: map[int64]*agentCall{}}
}

var _ dockerruntime.CommandSender = (*agentConn)(nil)

// writeFrame serializes one frame onto the socket; 10 s bounds a stalled
// peer, not the command it carries.
func (c *agentConn) writeFrame(f agentwire.Frame) error {
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

func (c *agentConn) start(stream bool) (int64, *agentCall) {
	call := &agentCall{res: make(chan agentwire.Result, 1)}
	if stream {
		call.chunks = make(chan agentwire.StreamChunk, agentStreamBuffer)
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.calls[id] = call
	c.mu.Unlock()
	return id, call
}

func (c *agentConn) finish(id int64) {
	c.mu.Lock()
	delete(c.calls, id)
	c.mu.Unlock()
}

func (c *agentConn) lookup(id int64) *agentCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[id]
}

// cancelRemote tells the agent to abort the command; best-effort — a broken
// socket is its own cancellation.
func (c *agentConn) cancelRemote(id int64) {
	_ = c.writeFrame(agentwire.Frame{Type: agentwire.FrameCancel, Cancel: id})
}

// deliverResult routes a result frame to its waiting call.
func (c *agentConn) deliverResult(res *agentwire.Result) {
	if res == nil {
		return
	}
	if call := c.lookup(res.ID); call != nil {
		select {
		case call.res <- *res:
		default: // duplicate result; the first one won
		}
	}
}

// deliverChunk routes a stream frame. A consumer that cannot keep up loses
// its stream — with an explicit error, and with the agent told to stop — so
// one slow log follower never stalls the whole server's channel.
func (c *agentConn) deliverChunk(chunk *agentwire.StreamChunk) {
	if chunk == nil {
		return
	}
	call := c.lookup(chunk.ID)
	if call == nil || call.chunks == nil {
		return
	}
	select {
	case call.chunks <- *chunk:
	default:
		c.cancelRemote(chunk.ID)
		select {
		case <-call.chunks: // shed one to make room for the error
		default:
		}
		select {
		case call.chunks <- agentwire.StreamChunk{ID: chunk.ID, Err: &agentwire.Error{
			Code: agentwire.CodeUnavailable, Message: "stream consumer too slow",
		}}:
		default:
		}
	}
}

// Command implements dockerruntime.CommandSender: one typed command, one
// result.
func (c *agentConn) Command(ctx context.Context, method string, params any) (json.RawMessage, error) {
	cmd, id, call, err := c.open(method, params, false)
	if err != nil {
		return nil, err
	}
	defer c.finish(id)
	if err := c.writeFrame(agentwire.Frame{Type: agentwire.FrameCommand, Cmd: cmd}); err != nil {
		return nil, errAgentUnavailable("write failed")
	}
	select {
	case <-ctx.Done():
		c.cancelRemote(id)
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, errAgentUnavailable("closed")
	case res := <-call.res:
		if res.Err != nil {
			return nil, res.Err.Err()
		}
		return res.Body, nil
	}
}

// Stream implements dockerruntime.CommandSender: the command's result
// acknowledges the open, then chunks flow until EOF, error or Close.
func (c *agentConn) Stream(ctx context.Context, method string, params any) (io.ReadCloser, error) {
	cmd, id, call, err := c.open(method, params, true)
	if err != nil {
		return nil, err
	}
	if err := c.writeFrame(agentwire.Frame{Type: agentwire.FrameCommand, Cmd: cmd}); err != nil {
		c.finish(id)
		return nil, errAgentUnavailable("write failed")
	}
	select {
	case <-ctx.Done():
		c.cancelRemote(id)
		c.finish(id)
		return nil, ctx.Err()
	case <-c.ctx.Done():
		c.finish(id)
		return nil, errAgentUnavailable("closed")
	case res := <-call.res:
		if res.Err != nil {
			c.finish(id)
			return nil, res.Err.Err()
		}
	}
	return &agentStream{conn: c, id: id, call: call, ctx: ctx}, nil
}

func (c *agentConn) open(method string, params any, stream bool) (*agentwire.Command, int64, *agentCall, error) {
	var raw json.RawMessage
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return nil, 0, nil, err
		}
		raw = data
	}
	id, call := c.start(stream)
	return &agentwire.Command{ID: id, Method: method, Params: raw}, id, call, nil
}

// agentStream adapts a command's chunk flow to io.ReadCloser.
type agentStream struct {
	conn *agentConn
	id   int64
	call *agentCall
	ctx  context.Context

	buf    []byte
	err    error
	closed bool
}

func (s *agentStream) Read(p []byte) (int, error) {
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
			s.err = errAgentUnavailable("closed")
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

func (s *agentStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.err == nil || (s.err != io.EOF && s.ctx.Err() == nil) {
		s.conn.cancelRemote(s.id)
	}
	s.conn.finish(s.id)
	return nil
}

// AgentConns is the per-process registry of live v2 channels, keyed by
// server. The latest connection wins: an agent that reconnects replaces its
// predecessor, whose handler is already on its way out.
type AgentConns struct {
	mu sync.Mutex
	m  map[int64]*agentConn
}

func (r *AgentConns) register(serverID int64, c *agentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[int64]*agentConn{}
	}
	r.m[serverID] = c
}

func (r *AgentConns) unregister(serverID int64, c *agentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[serverID] == c {
		delete(r.m, serverID)
	}
}

// Sender returns the live channel of a server's agent, if this process holds
// one.
func (r *AgentConns) Sender(serverID int64) (dockerruntime.CommandSender, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.m[serverID]
	return c, ok
}

// Runtime returns the Docker runtime executing on the given server through
// its agent channel — the ADR-051 mandatory path for Docker operations.
func (r *AgentConns) Runtime(serverID int64) (dockerruntime.Runtime, error) {
	s, ok := r.Sender(serverID)
	if !ok {
		return nil, errAgentUnavailable("not connected")
	}
	return dockerruntime.NewAgentRuntime(s), nil
}
