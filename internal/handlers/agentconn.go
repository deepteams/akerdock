// Agent command channel, control-plane side (ADR-052): the call routing
// lives in agentwire.Conn — typed commands out, results and stream chunks
// matched back by id. AgentConns is the per-process registry the runtime
// asks for a server's channel; like AgentPresence it is in-memory, accurate
// within the process that terminates the WebSocket (a worker reaches these
// through the relay, ADR-052 §8).
package handlers

import (
	"context"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
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

// Runtime returns the Docker runtime executing on the given server through
// its agent channel — the ADR-051 mandatory path for Docker operations. The
// ctx is unused here (the registry is in-memory) but keeps the signature
// shared with the relay-backed source workers use.
func (r *AgentConns) Runtime(_ context.Context, serverID int64) (dockerruntime.Runtime, error) {
	s, ok := r.Sender(serverID)
	if !ok {
		return nil, agentwire.Unavailable("not connected")
	}
	return dockerruntime.NewAgentRuntime(s), nil
}

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
