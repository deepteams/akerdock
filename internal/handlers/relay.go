// Agent relay, api side (ADR-052 §8): a worker or scheduler process does not
// terminate agent WebSockets, so it bridges its typed commands through the
// api process that does. Auth is the TARGET SERVER's own agent token — the
// caller reads it from the store exactly like agent provisioning does, and
// the token's scope (one server) is precisely the relay's authorization.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// AgentRelay implements GET /agent/v1/relay: one WebSocket per (process,
// server), speaking the same typed frames as the agent channel.
func (a *API) AgentRelay(w http.ResponseWriter, r *http.Request) {
	token, ok := a.authAgentToken(w, r)
	if !ok {
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentwire.SubprotocolRelay}})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	a.Logger.Info("agent relay connected", "server_id", token.ServerID)
	defer a.Logger.Info("agent relay closed", "server_id", token.ServerID)

	wc := agentwire.NewConn(ctx, conn)
	// The relay's own dead-peer detection: a worker process that died mid-job
	// otherwise leaves this loop reading a socket nobody will ever write to.
	go func() {
		wc.Keepalive(30*time.Second, 10*time.Second)
		cancel()
	}()
	senderFor := func() (dockerruntime.CommandSender, bool) { return a.AgentRPC.Sender(token.ServerID) }
	relayLoop(ctx, conn, wc, senderFor)
}

// relayLoop bridges relay frames onto the server's live channel: each cmd
// becomes a Command, Stream or Attach call on THIS process's agent
// connection — so command id spaces never collide, and telemetry counts
// every operation exactly once, where the channel is. Input chunks of a
// bridged attach route to its stream by id.
func relayLoop(ctx context.Context, conn *websocket.Conn, wc *agentwire.Conn, senderFor func() (dockerruntime.CommandSender, bool)) {
	var mu sync.Mutex
	inflight := map[int64]context.CancelFunc{}
	attaches := map[int64]dockerruntime.AttachStream{}
	registerAttach := func(id int64, att dockerruntime.AttachStream) func() {
		mu.Lock()
		attaches[id] = att
		mu.Unlock()
		return func() {
			mu.Lock()
			delete(attaches, id)
			mu.Unlock()
		}
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var f agentwire.Frame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch f.Type {
		case agentwire.FrameCommand:
			if f.Cmd == nil {
				continue
			}
			cmd := *f.Cmd
			cmdCtx, cancelCmd := context.WithCancel(ctx)
			mu.Lock()
			inflight[cmd.ID] = cancelCmd
			mu.Unlock()
			go func() {
				defer func() {
					cancelCmd()
					mu.Lock()
					delete(inflight, cmd.ID)
					mu.Unlock()
				}()
				bridgeCommand(cmdCtx, wc, cmd, senderFor, registerAttach)
			}()
		case agentwire.FrameStream:
			if f.Chunk == nil {
				continue
			}
			mu.Lock()
			att := attaches[f.Chunk.ID]
			mu.Unlock()
			if att == nil {
				continue
			}
			if len(f.Chunk.Data) > 0 {
				_, _ = att.Write(f.Chunk.Data)
			}
			if f.Chunk.EOF {
				_ = att.CloseWrite()
			}
		case agentwire.FrameCancel:
			mu.Lock()
			cancelCmd := inflight[f.Cancel]
			mu.Unlock()
			if cancelCmd != nil {
				cancelCmd()
			}
		}
	}
}

// bridgeCommand forwards one relayed command and writes its answer back. The
// error round-trips intact: rebuilt from the agent's wire code by the
// channel, re-flattened here — the worker's IsNotFound sees what the daemon
// said, two hops away.
func bridgeCommand(ctx context.Context, wc *agentwire.Conn, cmd agentwire.Command, senderFor func() (dockerruntime.CommandSender, bool), registerAttach func(int64, dockerruntime.AttachStream) func()) {
	fail := func(err error) {
		_ = wc.WriteFrame(agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID, Err: agentwire.WireError(err)}})
	}
	sender, ok := senderFor()
	if !ok {
		fail(agentwire.Unavailable("not connected"))
		return
	}
	if cmd.Method == agentwire.MethodContainerExecAttach {
		att, err := sender.Attach(ctx, cmd.Method, cmd.Params)
		if err != nil {
			fail(err)
			return
		}
		unregister := registerAttach(cmd.ID, att)
		defer func() {
			unregister()
			_ = att.Close()
		}()
		if wc.WriteFrame(agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID}}) != nil {
			return
		}
		agentwire.PumpReader(ctx, cmd.ID, att, wc.WriteFrame)
		return
	}
	if agentwire.IsStreamMethod(cmd.Method) {
		rc, err := sender.Stream(ctx, cmd.Method, cmd.Params)
		if err != nil {
			fail(err)
			return
		}
		defer func() { _ = rc.Close() }()
		if wc.WriteFrame(agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID}}) != nil {
			return
		}
		agentwire.PumpReader(ctx, cmd.ID, rc, wc.WriteFrame)
		return
	}
	body, err := sender.Command(ctx, cmd.Method, cmd.Params)
	if err != nil {
		fail(err)
		return
	}
	_ = wc.WriteFrame(agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID, Body: body}})
}
