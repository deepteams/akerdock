// Agent ingestion (ADR-040 phase 1): the server helper POSTs observation
// batches here, authenticated by its per-server token. Observations are
// untrusted hints scoped to that server: they may refresh observed state and
// emit SSE events, never act — the SSH scans remain the authoritative read.
package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// agentBatchMax bounds one ingestion call; the agent batches at 100, this
// leaves headroom without letting a broken sender stuff megabytes of hints.
const agentBatchMax = 500

// authAgentToken resolves the per-server token of an agent request; ok=false
// means the 401 was already written.
func (a *API) authAgentToken(w http.ResponseWriter, r *http.Request) (store.AgentToken, bool) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(raw, "akda_") {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "agent token required")
		return store.AgentToken{}, false
	}
	sum := sha256.Sum256([]byte(raw))
	token, err := a.Store.GetAgentTokenByHash(r.Context(), hex.EncodeToString(sum[:]))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "unknown agent token")
		return store.AgentToken{}, false
	}
	return token, true
}

// AgentObservations implements POST /agent/v1/observations: authenticate the
// per-server token, then apply each observation best-effort — a malformed or
// out-of-scope entry is skipped, never a reason to fail the batch (delivery
// is at-least-once; the agent would only resend it).
func (a *API) AgentObservations(w http.ResponseWriter, r *http.Request) {
	token, ok := a.authAgentToken(w, r)
	if !ok {
		return
	}
	_ = a.Store.TouchAgentTokenSeen(r.Context(), token.ID)

	var payload struct {
		Observations []agentwire.Observation `json:"observations"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid observation batch")
		return
	}
	if len(payload.Observations) > agentBatchMax {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "observation batch too large")
		return
	}
	for _, o := range payload.Observations {
		a.applyAgentObservation(r.Context(), token.ServerID, o)
	}
	w.WriteHeader(http.StatusAccepted)
}

// AgentPresence tracks the live agent channels per server (ADR-041 §2) —
// in-memory, accurate within the supported single-api topology.
type AgentPresence struct {
	mu   sync.Mutex
	live map[int64]int
}

func (p *AgentPresence) connect(serverID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live == nil {
		p.live = map[int64]int{}
	}
	p.live[serverID]++
}

func (p *AgentPresence) disconnect(serverID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live[serverID]--; p.live[serverID] <= 0 {
		delete(p.live, serverID)
	}
}

// Connected reports whether the server's agent holds a live channel.
func (p *AgentPresence) Connected(serverID int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.live[serverID] > 0
}

// AgentChannel implements GET /agent/v1/ws (ADR-041, ADR-052): the
// persistent outbound channel. Presence is the connection; observation frames
// are acknowledged by sequence — a refused batch is acked `denied` so the
// agent drops it instead of retrying forever. When the agent offers the v2
// subprotocol the same connection carries the typed command traffic, routed
// through the registered agentConn.
func (a *API) AgentChannel(w http.ResponseWriter, r *http.Request) {
	token, ok := a.authAgentToken(w, r)
	if !ok {
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Server preference order: commands whenever the agent can execute
		// them, plain observations otherwise.
		Subprotocols: []string{agentwire.SubprotocolV2, agentwire.SubprotocolV1},
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(1 << 20)

	a.Agents.connect(token.ServerID)
	defer a.Agents.disconnect(token.ServerID)
	_ = a.Store.TouchAgentTokenSeen(r.Context(), token.ID)
	a.Logger.Info("agent channel connected", "server_id", token.ServerID)
	defer a.Logger.Info("agent channel closed", "server_id", token.ServerID)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Dead-peer detection (ADR-041 §2): a ping that cannot round-trip ends
	// the connection — presence flips within seconds, not heartbeat minutes.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pingCtx)
				pingCancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	ac := newAgentConn(ctx, conn)
	v2 := conn.Subprotocol() == agentwire.SubprotocolV2
	if v2 {
		if a.Audit != nil {
			metrics := a.Audit.Metrics
			metricsCtx := context.WithoutCancel(ctx)
			ac.Record = func(method, outcome string) { metrics.RecordDockerOp(metricsCtx, method, outcome) }
		}
		a.AgentRPC.register(token.ServerID, ac)
		defer a.AgentRPC.unregister(token.ServerID, ac)
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
		case agentwire.FrameObservations:
			if len(f.Observations) > agentBatchMax {
				if ac.WriteFrame(agentwire.Frame{Type: agentwire.FrameAck, Seq: f.Seq, Denied: true}) != nil {
					return
				}
				continue
			}
			for _, o := range f.Observations {
				a.applyAgentObservation(ctx, token.ServerID, o)
			}
			_ = a.Store.TouchAgentTokenSeen(ctx, token.ID)
			if ac.WriteFrame(agentwire.Frame{Type: agentwire.FrameAck, Seq: f.Seq}) != nil {
				return
			}
		case agentwire.FrameResult:
			if v2 {
				ac.DeliverResult(f.Res)
			}
		case agentwire.FrameStream:
			if v2 {
				ac.DeliverChunk(f.Chunk)
			}
		}
	}
}

// applyAgentObservation applies one hint, scoped to the sender's server by
// construction: every query carries the server id, so a compromised agent can
// never touch another server's state.
func (a *API) applyAgentObservation(ctx context.Context, serverID int64, o agentwire.Observation) {
	switch o.Type {
	case "heartbeat":
		// The touch on the token row already recorded liveness.
	case "ingress_claimed", "ingress_alive", "ingress_closed":
		a.applyIngressObservation(ctx, serverID, ingressObservation{
			Type: o.Type, SessionUUID: o.ResourceUUID, State: o.State,
		})
	case "stz_woken":
		var u pgtype.UUID
		if err := u.Scan(o.ResourceUUID); err != nil {
			return
		}
		// A sleeping preview on this server: flip it awake and tell the UI —
		// this is the real-time path the SSH scan used to provide a minute
		// late (the scan remains, as reconciliation).
		if p, err := a.Store.GetSleepingPreviewForServer(ctx, store.GetSleepingPreviewForServerParams{
			Uuid: u, ServerID: serverID,
		}); err == nil {
			if err := a.Store.SetPreviewAwake(ctx, p.ID); err != nil {
				return
			}
			a.emitAgentPreviewWoken(ctx, p)
			return
		}
		// Else: a slept scale-to-zero application on this server.
		if id, err := a.Store.WakeSleptApplicationForServer(ctx, store.WakeSleptApplicationForServerParams{
			Uuid: u, ServerID: serverID,
		}); err == nil {
			a.emitAgentApplicationWoken(ctx, id, u)
		}
	case "container_state":
		resourceUUID, component, ok := splitComponentContainer(o.Container)
		if !ok {
			return
		}
		observed, ok := observedFromDockerAction(o.State)
		if !ok {
			return
		}
		_, _ = a.Store.SetServiceComponentObservedByName(ctx, store.SetServiceComponentObservedByNameParams{
			Uuid: resourceUUID, Name: component, ObservedStatus: observed, ServerID: serverID,
		})
	}
}

// emitAgentPreviewWoken publishes the same event the scheduler's scan emits,
// so the previews tab refreshes within a second of the wake.
func (a *API) emitAgentPreviewWoken(ctx context.Context, p store.Preview) {
	app, err := a.Store.GetApplicationByID(ctx, p.ApplicationID)
	if err != nil {
		return
	}
	var teamUUID pgtype.UUID
	if team, err := a.Store.GetTeamByID(ctx, app.Resource.TeamID); err == nil {
		teamUUID = team.Uuid
	}
	a.Audit.Outbox(ctx, a.Store, "application.preview.woken.v1", teamUUID, app.Resource.Uuid,
		"preview:"+pguuid.String(p.Uuid), map[string]any{
			"preview_uuid": pguuid.String(p.Uuid),
			"pr_id":        p.PrID,
		})
	a.Logger.Info("preview woken (agent observation)", "preview", pguuid.String(p.Uuid), "pr", p.PrID)
}

// emitAgentApplicationWoken mirrors the scheduler's application.woken.v1, so
// the application pages refresh within a second of the wake.
func (a *API) emitAgentApplicationWoken(ctx context.Context, resourceID int64, resourceUUID pgtype.UUID) {
	app, err := a.Store.GetApplicationByID(ctx, resourceID)
	if err != nil {
		return
	}
	var teamUUID pgtype.UUID
	if team, err := a.Store.GetTeamByID(ctx, app.Resource.TeamID); err == nil {
		teamUUID = team.Uuid
	}
	a.Audit.Outbox(ctx, a.Store, "application.woken.v1", teamUUID, resourceUUID,
		"application:"+pguuid.String(resourceUUID), map[string]any{"name": app.Resource.Name})
	a.Logger.Info("application woken (agent observation)", "application", pguuid.String(resourceUUID))
}

// splitComponentContainer parses a compose component container name,
// `<resource-uuid>-<service>` (INV-011). Anything else — the single container
// of a plain app, helpers — carries no component to refresh.
func splitComponentContainer(name string) (pgtype.UUID, string, bool) {
	var u pgtype.UUID
	const uuidLen = 36
	if len(name) < uuidLen+2 || name[uuidLen] != '-' {
		return u, "", false
	}
	if err := u.Scan(name[:uuidLen]); err != nil {
		return u, "", false
	}
	return u, name[uuidLen+1:], true
}

// observedFromDockerAction maps what an agent reports for a container to the
// observed-status enum. Agents send a VERIFIED state (read from the daemon
// after the event settled — a Docker action alone is a trigger, not a truth:
// during a zero-downtime replacement the old container dies under the
// service's name while its replacement is renamed into place). The raw action
// names are still accepted so an older agent keeps working; anything without
// an observed meaning is skipped.
func observedFromDockerAction(state string) (store.ResourceObservedStatus, bool) {
	switch state {
	case "healthy":
		return store.ResourceObservedStatusHealthy, true
	case "unhealthy", "health_status: unhealthy":
		return store.ResourceObservedStatusUnhealthy, true
	case "starting", "start", "restart":
		return store.ResourceObservedStatusStarting, true
	case "exited", "die", "stop", "kill", "oom":
		return store.ResourceObservedStatusExited, true
	case "health_status: healthy":
		return store.ResourceObservedStatusHealthy, true
	default:
		return "", false
	}
}
