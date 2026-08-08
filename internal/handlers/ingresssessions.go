// The operator's view of the ingress tunnels of ADR-060: who is publishing
// their machine right now, on which URL — and the means to cut one. Liveness
// is agent-reported (the socket lives on the ingress server), so `active`
// reflects the last report, not a control-plane connection.
package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// ingressSeenStaleAfter mirrors the sweep window: an attached session whose
// agent has not reported within it is no longer counted live.
const ingressSeenStaleAfter = 90 * time.Second

// ListIngressTunnelSessions implements GET /ingress-tunnel-sessions.
func (a *API) ListIngressTunnelSessions(w http.ResponseWriter, r *http.Request, params api.ListIngressTunnelSessionsParams) {
	id, ok := a.require(w, r, auth.PermIngressEndpointsRead)
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
	if before == 0 {
		before = int64(^uint64(0) >> 1)
	}
	var endpointID *int64
	if params.IngressEndpointUuid != nil && *params.IngressEndpointUuid != "" {
		endpoint, found := a.resolveIngressEndpoint(w, r, id, *params.IngressEndpointUuid)
		if !found {
			return
		}
		endpointID = &endpoint.ID
	}
	rows, err := a.Store.ListIngressSessionsPage(r.Context(), store.ListIngressSessionsPageParams{
		TeamID:     id.TeamID,
		BeforeID:   before,
		EndpointID: endpointID,
		ActiveOnly: params.Active == nil || *params.Active,
		Limit:      limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list ingress sessions", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(s store.ListIngressSessionsPageRow) int64 { return s.ID })

	data := make([]api.IngressTunnelSessionInfo, 0, len(rows))
	for _, row := range rows {
		data = append(data, ingressSessionToAPI(row))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.IngressTunnelSessionInfo `json:"data"`
		NextCursor *string                        `json:"next_cursor"`
	}{data, cursor})
}

func ingressSessionToAPI(row store.ListIngressSessionsPageRow) api.IngressTunnelSessionInfo {
	out := api.IngressTunnelSessionInfo{
		Uuid:      uuidString(row.Uuid),
		UserEmail: row.UserEmail,
		Active:    ingressSessionActive(row),
		CreatedAt: row.CreatedAt.Time,
	}
	if row.EndpointUuid.Valid {
		out.EndpointUuid = ptr(uuidString(row.EndpointUuid))
	}
	if row.EndpointName != nil {
		out.EndpointName = row.EndpointName
	}
	if row.EndpointFqdn != nil {
		out.Fqdn = row.EndpointFqdn
	}
	if row.ClientIp != nil {
		out.ClientIp = ptr(row.ClientIp.String())
	}
	if row.StartedAt.Valid {
		out.StartedAt = &row.StartedAt.Time
	}
	if row.LastSeenAt.Valid {
		out.LastSeenAt = &row.LastSeenAt.Time
	}
	if row.EndedAt.Valid {
		out.EndedAt = &row.EndedAt.Time
	}
	if row.EndReason != nil {
		out.EndReason = ptr(api.IngressTunnelSessionInfoEndReason(*row.EndReason))
	}
	return out
}

// ingressSessionActive is the same "open" definition as the sweep: a
// redeemable token, or an attached session whose agent has reported recently.
func ingressSessionActive(row store.ListIngressSessionsPageRow) bool {
	if row.EndedAt.Valid {
		return false
	}
	now := time.Now()
	if !row.ClaimedAt.Valid {
		return row.TokenExpiresAt.Time.After(now)
	}
	return !row.LastSeenAt.Valid || row.LastSeenAt.Time.After(now.Add(-ingressSeenStaleAfter))
}

// CloseIngressTunnelSession implements DELETE /ingress-tunnel-sessions/{uuid}.
// Closing one's own attach needs the permission that opened it; closing
// someone else's requires ingress-endpoints:manage. Either way the laptop is
// told why, and a policy close is not re-dialed (ADR-060 §6).
func (a *API) CloseIngressTunnelSession(w http.ResponseWriter, r *http.Request, sessionUUID api.IngressTunnelSessionUuid) {
	id, ok := a.require(w, r, auth.PermIngressTunnelsOpen)
	if !ok {
		return
	}
	u, ok := a.scanUUID(w, r, sessionUUID, "session")
	if !ok {
		return
	}
	// The list query is the read path; for the close we need the row with its
	// server and owner, which the session row + endpoint lookup provides.
	rows, err := a.Store.ListIngressSessionsPage(r.Context(), store.ListIngressSessionsPageParams{
		TeamID: id.TeamID, BeforeID: int64(^uint64(0) >> 1), ActiveOnly: false, Limit: 1_000_000,
	})
	if err != nil {
		a.internalError(w, r, "close ingress session", err)
		return
	}
	var target *store.ListIngressSessionsPageRow
	for i := range rows {
		if rows[i].Uuid == u {
			target = &rows[i]
			break
		}
	}
	if target == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "ingress session not found")
		return
	}

	reason := tunnel.EndUserClose
	if !ingressSessionOwnedBy(id, target) {
		if _, ok := a.require(w, r, auth.PermIngressEndpointsManage); !ok {
			return
		}
		reason = tunnelEndReasonRevoked
	}
	if target.EndedAt.Valid {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Cut the live socket through the agent, then finalize the row. The server
	// is resolved from the endpoint; a session whose endpoint was deleted has
	// no socket left to cut.
	if target.EndpointUuid.Valid {
		if endpoint, err := a.Store.GetIngressEndpointByUUIDGlobal(r.Context(), target.EndpointUuid); err == nil {
			a.cutIngressSession(r.Context(), endpoint.ServerID, target.Uuid, string(reason))
		}
	}
	if _, err := a.Store.EndIngressSessionByUUID(r.Context(), store.EndIngressSessionByUUIDParams{
		Uuid: u, TeamID: id.TeamID, EndReason: ingressEndReason(reason),
	}); err != nil {
		a.internalError(w, r, "close ingress session", err)
		return
	}
	a.recordAudit(r, id, "ingress-tunnel.close", "ingress_tunnel_session", target.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

func ingressSessionOwnedBy(id *auth.Identity, row *store.ListIngressSessionsPageRow) bool {
	if row.UserID == nil {
		return false
	}
	userID := actingUserID(id)
	return userID != nil && *userID == *row.UserID
}

// applyIngressObservation records what a server's agent reports about its
// ingress tunnels (ADR-060 §3): the claim stamps the row started, a heartbeat
// keeps it live, a close finalizes it with the reason the agent enforced.
// Scoped to the sender's server by construction — the session's endpoint must
// belong to it — so a compromised agent cannot finalize another server's
// sessions.
func (a *API) applyIngressObservation(ctx context.Context, serverID int64, o ingressObservation) {
	var u pgtype.UUID
	if err := u.Scan(o.SessionUUID); err != nil {
		return
	}
	switch o.Type {
	case "ingress_claimed":
		_, _ = a.Store.MarkIngressSessionClaimed(ctx, u)
	case "ingress_alive":
		if n, err := a.Store.TouchIngressSession(ctx, u); err == nil && n == 0 {
			// The row was finalized elsewhere (operator close, sweep): cut the
			// socket the next report cannot keep alive.
			a.cutIngressSession(ctx, serverID, u, string(tunnelEndReasonRevoked))
		}
	case "ingress_closed":
		reason := tunnel.EndReason(o.State)
		if reason == "" {
			reason = tunnel.EndDisconnect
		}
		if s, err := a.Store.GetOpenIngressSessionByUUID(ctx, u); err == nil {
			_, _ = a.Store.EndIngressSession(ctx, store.EndIngressSessionParams{
				ID: s.ID, EndReason: ingressEndReason(reason),
			})
		}
	}
}

// ingressObservation is the ADR-060 lifecycle hint an agent pushes on the
// observation rail. It reuses the Observation frame's fields: ResourceUUID
// carries the session uuid, State the end reason.
type ingressObservation struct {
	Type        string
	SessionUUID string
	State       string
}
