// Agent command channel, control-plane side (ADR-052): the call routing
// lives in agentwire.Conn — typed commands out, results and stream chunks
// matched back by id. AgentConns is the per-process registry the runtime
// asks for a server's channel; like AgentPresence it is in-memory, accurate
// within the process that terminates the WebSocket (a worker reaches these
// through the relay, ADR-052 §8).
package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/httpapi"
)

// agentConn is one live v2 channel: agentwire.Conn plus the Attach signature
// dockerruntime.CommandSender asks for.
type agentConn struct {
	*agentwire.Conn
}

func newAgentConn(ctx context.Context, conn *websocket.Conn) *agentConn {
	return &agentConn{Conn: agentwire.NewConn(ctx, conn)}
}

// Attach narrows agentwire's concrete stream to the CommandSender interface.
func (c *agentConn) Attach(ctx context.Context, method string, params any) (dockerruntime.AttachStream, error) {
	return c.Conn.Attach(ctx, method, params)
}

var _ dockerruntime.CommandSender = (*agentConn)(nil)

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
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.m[serverID]
	return c, ok
}

// dynamicSender resolves the server's CURRENT channel at every call: a
// runtime handed to a long job (a deployment holds one for many minutes)
// survives an agent reconnect mid-run — the next call rides the fresh
// connection instead of the corpse the job started with. The call in flight
// when the channel died still fails, which is correct: its outcome is
// unknown, and the caller's step/retry semantics own that.
type dynamicSender struct {
	conns    *AgentConns
	serverID int64
}

func (d dynamicSender) current() (dockerruntime.CommandSender, error) {
	s, ok := d.conns.Sender(d.serverID)
	if !ok {
		return nil, agentwire.Unavailable("not connected")
	}
	return s, nil
}

func (d dynamicSender) Command(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s, err := d.current()
	if err != nil {
		return nil, err
	}
	return s.Command(ctx, method, params)
}

func (d dynamicSender) Stream(ctx context.Context, method string, params any) (io.ReadCloser, error) {
	s, err := d.current()
	if err != nil {
		return nil, err
	}
	return s.Stream(ctx, method, params)
}

func (d dynamicSender) Attach(ctx context.Context, method string, params any) (dockerruntime.AttachStream, error) {
	s, err := d.current()
	if err != nil {
		return nil, err
	}
	return s.Attach(ctx, method, params)
}

var _ dockerruntime.CommandSender = dynamicSender{}

// Runtime returns the Docker runtime executing on the given server through
// its agent channel — the ADR-051 mandatory path for Docker operations. The
// resolve-time check keeps the fail-fast contract ("is the agent there at
// all?"); the runtime itself re-resolves the live channel on every call. The
// ctx is unused here (the registry is in-memory) but keeps the signature
// shared with the relay-backed source workers use.
func (r *AgentConns) Runtime(_ context.Context, serverID int64) (dockerruntime.Runtime, error) {
	if _, ok := r.Sender(serverID); !ok {
		return nil, agentwire.Unavailable("not connected")
	}
	return dockerruntime.NewAgentRuntime(dynamicSender{conns: r, serverID: serverID}), nil
}

// HostOps returns the ADR-054 file primitives executing on the given server
// through the same channel, with the same per-call re-resolution.
func (r *AgentConns) HostOps(_ context.Context, serverID int64) (hostops.Ops, error) {
	if _, ok := r.Sender(serverID); !ok {
		return nil, agentwire.Unavailable("not connected")
	}
	return hostops.NewClient(dynamicSender{conns: r, serverID: serverID}), nil
}

var _ hostops.Source = (*AgentConns)(nil)

// agentRuntime resolves the server's Docker runtime over its command channel
// (ADR-051: the agent is the only Docker path — no fallback hides a missing
// channel); ok=false means the 409 was already written.
func (a *API) agentRuntime(w http.ResponseWriter, r *http.Request, serverID int64) (dockerruntime.Runtime, bool) {
	rt, err := a.AgentRPC.Runtime(r.Context(), serverID)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
			"the server's agent is not connected — it reconnects on its own; check the server page if this persists")
		return nil, false
	}
	return rt, true
}
