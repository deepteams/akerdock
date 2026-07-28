// Agent ingestion (ADR-040 phase 1): the server helper POSTs observation
// batches here, authenticated by its per-server token. Observations are
// untrusted hints scoped to that server: they may refresh observed state and
// emit SSE events, never act — the SSH scans remain the authoritative read.
package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// agentObservation mirrors waker.Observation on the wire.
type agentObservation struct {
	Type         string    `json:"type"`
	At           time.Time `json:"at"`
	Container    string    `json:"container"`
	State        string    `json:"state"`
	ResourceUUID string    `json:"resource_uuid"`
}

// agentBatchMax bounds one ingestion call; the agent batches at 100, this
// leaves headroom without letting a broken sender stuff megabytes of hints.
const agentBatchMax = 500

// AgentObservations implements POST /agent/v1/observations: authenticate the
// per-server token, then apply each observation best-effort — a malformed or
// out-of-scope entry is skipped, never a reason to fail the batch (delivery
// is at-least-once; the agent would only resend it).
func (a *API) AgentObservations(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(raw, "akda_") {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "agent token required")
		return
	}
	sum := sha256.Sum256([]byte(raw))
	token, err := a.Store.GetAgentTokenByHash(r.Context(), hex.EncodeToString(sum[:]))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "unknown agent token")
		return
	}
	_ = a.Store.TouchAgentTokenSeen(r.Context(), token.ID)

	var payload struct {
		Observations []agentObservation `json:"observations"`
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
		a.applyAgentObservation(r, token.ServerID, o)
	}
	w.WriteHeader(http.StatusAccepted)
}

// applyAgentObservation applies one hint, scoped to the sender's server by
// construction: every query carries the server id, so a compromised agent can
// never touch another server's state.
func (a *API) applyAgentObservation(r *http.Request, serverID int64, o agentObservation) {
	ctx := r.Context()
	switch o.Type {
	case "heartbeat":
		// The touch on the token row already recorded liveness.
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
			a.emitAgentPreviewWoken(r, p)
			return
		}
		// Else: a slept scale-to-zero application on this server.
		if n, err := a.Store.WakeSleptApplicationForServer(ctx, store.WakeSleptApplicationForServerParams{
			Uuid: u, ServerID: serverID,
		}); err == nil && n > 0 {
			a.Logger.Info("application woken (agent observation)", "application", o.ResourceUUID, "server_id", serverID)
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
func (a *API) emitAgentPreviewWoken(r *http.Request, p store.Preview) {
	ctx := r.Context()
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

// observedFromDockerAction maps a Docker event action to the observed-status
// enum; actions with no observed meaning (pause, destroy, …) are skipped.
func observedFromDockerAction(action string) (store.ResourceObservedStatus, bool) {
	switch action {
	case "start", "restart":
		return store.ResourceObservedStatusStarting, true
	case "health_status: healthy":
		return store.ResourceObservedStatusHealthy, true
	case "health_status: unhealthy":
		return store.ResourceObservedStatusUnhealthy, true
	case "die", "stop", "kill", "oom":
		return store.ResourceObservedStatusExited, true
	default:
		return "", false
	}
}
