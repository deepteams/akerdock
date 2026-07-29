// The operator's view of the tunnels of ADR-032/ADR-045: what is forwarded out
// of this team right now, by whom, onto what — and the means to cut one.
//
// Listing and closing live here rather than next to the mint because they are a
// different act: minting is a developer opening their own tunnel, this is
// somebody looking at (and interrupting) the fleet's.
package handlers

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// TunnelPresence tracks the bridges THIS process is currently running, keyed by
// session id, so that closing a session cuts the socket now instead of leaving
// it alive until its own idle timeout — which is what "revoking tears down the
// sessions" has to mean to somebody already connected (ADR-045 §5).
//
// Deliberately in-memory: a tunnel is a live socket held by one process, and
// the row in the database is the record, not the connection. In a multi-replica
// deployment a cut therefore reaches the socket only when it is served by the
// replica handling the request; the session is marked ended either way, so the
// attach token can no longer be redeemed and the next dial fails. Making this
// exact across replicas is a LISTEN/NOTIFY question, not a bookkeeping one.
//
// Zero value ready, like AgentPresence.
type TunnelPresence struct {
	mu          sync.Mutex
	live        map[int64]chan tunnel.EndReason
	closing     bool
	closeReason tunnel.EndReason
}

// register hands the bridge its cancel channel and returns it. Buffered so a
// cut never blocks on a bridge that is between selects.
func (p *TunnelPresence) register(sessionID int64) <-chan tunnel.EndReason {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live == nil {
		p.live = map[int64]chan tunnel.EndReason{}
	}
	ch := make(chan tunnel.EndReason, 1)
	p.live[sessionID] = ch
	// Shutdown starts before http.Server.Shutdown: a WebSocket that raced with
	// it may register after CloseAll took its snapshot. Remembering the state
	// makes that bridge leave immediately and Wait still observes it until its
	// database row has been finalized.
	if p.closing {
		ch <- p.closeReason
	}
	return ch
}

func (p *TunnelPresence) unregister(sessionID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.live, sessionID)
}

// Cut asks the bridge of this session to stop, reporting reason to the client.
// Reports whether a live bridge was reached here. A second cut on the same
// session is a no-op: the channel holds one value and the bridge is leaving.
func (p *TunnelPresence) Cut(sessionID int64, reason tunnel.EndReason) bool {
	p.mu.Lock()
	ch, ok := p.live[sessionID]
	p.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- reason:
	default:
	}
	return true
}

// CloseAll asks every bridge owned by this process to stop. It also closes any
// bridge that races with shutdown and registers afterwards.
func (p *TunnelPresence) CloseAll(reason tunnel.EndReason) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closing {
		p.closing = true
		p.closeReason = reason
	}
	for _, ch := range p.live {
		select {
		case ch <- p.closeReason:
		default:
		}
	}
	return len(p.live)
}

// Wait blocks until every bridge has returned and unregistered, or ctx expires.
// The unregister happens only after endPortForwardSession, so a successful wait
// also means the open rows have been finalized before the process exits.
func (p *TunnelPresence) Wait(ctx context.Context) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		p.mu.Lock()
		n := len(p.live)
		p.mu.Unlock()
		if n == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// ListPortForwardSessions implements GET /port-forward-sessions.
func (a *API) ListPortForwardSessions(w http.ResponseWriter, r *http.Request, params api.ListPortForwardSessionsParams) {
	id, ok := a.require(w, r, auth.PermPortForwardsOpen)
	if !ok {
		return
	}
	limit, ok := pageLimit(w, r, params.Limit)
	if !ok {
		return
	}
	before, ok := afterID(w, r, params.Cursor)
	if !ok {
		return
	}
	// Newest first, so an absent cursor starts above every row rather than
	// below it (same walk as the grant list).
	if before == 0 {
		before = int64(^uint64(0) >> 1)
	}
	var endpointID *int64
	if params.ExternalEndpointUuid != nil && *params.ExternalEndpointUuid != "" {
		endpoint, found := a.resolveExternalEndpoint(w, r, id, *params.ExternalEndpointUuid)
		if !found {
			return
		}
		endpointID = &endpoint.ID
	}
	rows, err := a.Store.ListPortForwardSessionsPage(r.Context(), store.ListPortForwardSessionsPageParams{
		TeamID:     id.TeamID,
		BeforeID:   before,
		EndpointID: endpointID,
		// The live sessions are the operational question; the history is
		// available but asked for.
		ActiveOnly: params.Active == nil || *params.Active,
		PageLimit:  limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list port-forward sessions", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(s store.ListPortForwardSessionsPageRow) int64 { return s.ID })

	data := make([]api.PortForwardSessionInfo, 0, len(rows))
	for _, row := range rows {
		data = append(data, a.portForwardSessionToAPI(r, row))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.PortForwardSessionInfo `json:"data"`
		NextCursor *string                      `json:"next_cursor"`
	}{data, cursor})
}

// portForwardSessionToAPI renders one session. The target kind is derived from
// which target column is set rather than stored: the columns are the truth, and
// a session whose target was deleted afterwards reads as `unknown` instead of
// disappearing from the trail.
func (a *API) portForwardSessionToAPI(r *http.Request, row store.ListPortForwardSessionsPageRow) api.PortForwardSessionInfo {
	out := api.PortForwardSessionInfo{
		Uuid:            uuidString(row.Uuid),
		TargetKind:      api.PortForwardSessionInfoTargetKindUnknown,
		TargetName:      row.TargetName,
		TargetComponent: row.TargetComponent,
		TargetPort:      int(row.TargetPort),
		UserEmail:       row.UserEmail,
		Active:          portForwardSessionActive(row),
		CreatedAt:       row.CreatedAt.Time,
	}
	if row.ClientIp != nil {
		out.ClientIp = ptr(row.ClientIp.String())
	}
	if row.StartedAt.Valid {
		out.StartedAt = &row.StartedAt.Time
	}
	if row.EndedAt.Valid {
		out.EndedAt = &row.EndedAt.Time
	}
	if row.EndReason != nil {
		out.EndReason = ptr(api.PortForwardSessionInfoEndReason(*row.EndReason))
	}
	if row.AuthorizedUntil.Valid {
		out.AuthorizedUntil = &row.AuthorizedUntil.Time
	}
	switch {
	case row.ExternalEndpointID != nil:
		out.TargetKind = api.PortForwardSessionInfoTargetKindExternalEndpoint
		if row.EndpointUuid.Valid {
			out.ExternalEndpointUuid = ptr(uuidString(row.EndpointUuid))
		}
	case row.PreviewID != nil:
		out.TargetKind = api.PortForwardSessionInfoTargetKindPreview
	case row.ResourceID != nil:
		if res, err := a.Store.GetResourceByID(r.Context(), *row.ResourceID); err == nil {
			out.TargetKind = api.PortForwardSessionInfoTargetKind(res.ResourceType)
		}
	}
	return out
}

// portForwardSessionActive is the same definition of "open" as the team cap:
// a redeemable token, or an attached bridge whose authorization, hard duration
// and persisted heartbeat are still valid.
func portForwardSessionActive(row store.ListPortForwardSessionsPageRow) bool {
	if row.EndedAt.Valid {
		return false
	}
	now := time.Now()
	if !row.ClaimedAt.Valid {
		return row.TokenExpiresAt.Time.After(now)
	}
	if !row.StartedAt.Valid ||
		!row.StartedAt.Time.After(now.Add(-tunnel.DefaultMaxDuration)) {
		return false
	}
	if row.AuthorizedUntil.Valid && !row.AuthorizedUntil.Time.After(now) {
		return false
	}
	// NULL is a bridge served by the previous release during a rolling
	// upgrade. It cannot heartbeat, so retain it until the four-hour ceiling.
	return !row.LastHeartbeatAt.Valid ||
		row.LastHeartbeatAt.Time.After(now.Add(-portForwardHeartbeatStaleAfter))
}

const portForwardHeartbeatStaleAfter = 90 * time.Second

// ClosePortForwardSession implements DELETE /port-forward-sessions/{uuid}.
//
// Closing one's own tunnel needs nothing beyond the permission that opened it;
// closing somebody else's is the administrative act that revoking a grant is,
// and carries the same permission. The two cases also close for different
// reasons, because the message the developer reads comes from that value
// (ADR-045 §5): a tunnel that vanishes without a word is read as a bug.
func (a *API) ClosePortForwardSession(w http.ResponseWriter, r *http.Request, sessionUUID api.PortForwardSessionUuid) {
	id, ok := a.require(w, r, auth.PermPortForwardsOpen)
	if !ok {
		return
	}
	u, ok := a.scanUUID(w, r, sessionUUID, "session")
	if !ok {
		return
	}
	row, err := a.Store.GetPortForwardSessionByUUID(r.Context(), store.GetPortForwardSessionByUUIDParams{
		Uuid: u, TeamID: id.TeamID,
	})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "tunnel session not found")
		return
	}

	reason := tunnel.EndUserClose
	if !a.ownsPortForwardSession(r, id, row) {
		if _, ok := a.require(w, r, auth.PermExternalEndpointsManage); !ok {
			return
		}
		reason = tunnelEndReasonRevoked
	}

	if row.EndedAt.Valid {
		// Already closed: the tunnel is gone either way, and answering 404 here
		// would make a double click look like a failure.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	a.Tunnels.Cut(row.ID, reason)
	a.endPortForwardSession(row, reason)
	a.recordAudit(r, id, "port-forward.close", "port_forward_session", row.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// ownsPortForwardSession reports whether the caller is the human who opened
// this session. An API token owns nothing here: it has no user, so a token
// closing a session is always doing it to somebody else's.
func (a *API) ownsPortForwardSession(r *http.Request, id *auth.Identity, row store.PortForwardSession) bool {
	if row.UserID == nil {
		return false
	}
	userID := actingUserID(id)
	return userID != nil && *userID == *row.UserID
}
