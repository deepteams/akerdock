// External endpoints — the bastion of ADR-045: tunnels to destinations that
// are NOT containers AkerDock deploys (a managed RDS, a legacy database on a
// neighboring VM, an analytics cluster on the private network).
//
// The invariant that makes this safe is that the address belongs to a DECLARED
// resource, never to a request: the mint names an endpoint and carries no body
// at all, so the tunnel protocol stays addressless and a `write` holder cannot
// turn it into a scanner of the server's private network. Declaring an endpoint
// draws a network boundary and is therefore an admin act, kept separate from
// using one.
//
// On a `sensitive` endpoint (the default) minting also requires a live access
// grant: a bounded, reasoned window obtained in the dashboard behind a fresh
// second factor. The grant is the session deadline, not merely permission to
// start one — a tunnel never outlives the authorization that opened it.
package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

const (
	// grantStepUpWindow is how fresh the second factor must be when a grant is
	// requested. Same value as the root terminal (rbac-matrix §5): the ceremony
	// and the act it authorizes should be one continuous gesture.
	grantStepUpWindow = 5 * time.Minute

	// maxGrantMinutesCap mirrors the CHECK on external_endpoints: no single
	// request buys more than a working day. Renewal is unbounded in total —
	// the bound is the window itself, repeated, each time behind a fresh
	// factor (ADR-045 §5).
	maxGrantMinutesCap = 480

	// defaultMaxGrantMinutes is one ceremony in the morning, one in the
	// afternoon: the friction is calibrated on a working day.
	defaultMaxGrantMinutes = 240
)

// resolveExternalEndpoint loads a team-scoped endpoint or writes the 404. Like
// every other resolve helper, a foreign team's row is "not found", never
// "forbidden" — the boundary must not leak what exists on the other side.
func (a *API) resolveExternalEndpoint(w http.ResponseWriter, r *http.Request, id *auth.Identity, endpointUUID string) (store.ExternalEndpoint, bool) {
	var u pgtype.UUID
	if err := u.Scan(endpointUUID); err == nil {
		row, err := a.Store.GetExternalEndpointByUUID(r.Context(), store.GetExternalEndpointByUUIDParams{
			Uuid: u, TeamID: id.TeamID,
		})
		if err == nil {
			return row, true
		}
	}
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "external endpoint not found")
	return store.ExternalEndpoint{}, false
}

// externalEndpointToAPI renders an endpoint, resolving the internal ids it
// stores into the public uuids the contract speaks.
func (a *API) externalEndpointToAPI(r *http.Request, row store.ExternalEndpoint, activeGrant *store.ExternalEndpointGrant) api.ExternalEndpoint {
	out := api.ExternalEndpoint{
		Uuid:            uuidString(row.Uuid),
		Name:            row.Name,
		Description:     row.Description,
		Host:            row.Host,
		Port:            int(row.Port),
		Criticality:     api.ExternalEndpointCriticality(row.Criticality),
		MaxGrantMinutes: int(row.MaxGrantMinutes),
		CreatedAt:       row.CreatedAt.Time,
	}
	if row.UpdatedAt.Valid {
		out.UpdatedAt = &row.UpdatedAt.Time
	}
	if server, err := a.Store.GetServerByID(r.Context(), row.ServerID); err == nil {
		out.ServerUuid = uuidString(server.Uuid)
	}
	if row.ProjectID != nil {
		if p, err := a.Store.GetProjectByID(r.Context(), *row.ProjectID); err == nil {
			out.ProjectUuid = ptr(uuidString(p.Uuid))
		}
	}
	if row.EnvironmentID != nil {
		if e, err := a.Store.GetEnvironmentByID(r.Context(), *row.EnvironmentID); err == nil {
			out.EnvironmentUuid = ptr(uuidString(e.Uuid))
		}
	}
	if activeGrant != nil {
		g := grantToAPI(*activeGrant, "")
		out.ActiveGrant = &g
	}
	return out
}

func grantToAPI(row store.ExternalEndpointGrant, email string) api.ExternalEndpointGrant {
	out := api.ExternalEndpointGrant{
		Uuid:        uuidString(row.Uuid),
		Reason:      row.Reason,
		Factor:      api.ExternalEndpointGrantFactor(row.Factor),
		RequestedAt: row.RequestedAt.Time,
		ExpiresAt:   row.ExpiresAt.Time,
		Renewed:     ptr(row.RenewedFrom != nil),
	}
	if email != "" {
		out.UserEmail = &email
	}
	if row.RevokedAt.Valid {
		out.RevokedAt = &row.RevokedAt.Time
	}
	return out
}

// validateEndpointBody enforces the exact-pair rule (ADR-045 §1). A network is
// deliberately not addressable as a unit: one endpoint is one destination, and
// that is what keeps this feature from becoming a port scanner with an audit
// log attached.
func validateEndpointBody(w http.ResponseWriter, r *http.Request, body api.ExternalEndpointCreate) bool {
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 63 {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"name is required (1–63 characters)")
		return false
	}
	host := strings.TrimSpace(body.Host)
	if host == "" || strings.ContainsAny(host, " \t/,:") {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"host must be a single hostname or IP — no scheme, path, port or list")
		return false
	}
	if body.Port < 1 || body.Port > 65535 {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"port must be an integer in 1–65535")
		return false
	}
	if body.MaxGrantMinutes != nil && (*body.MaxGrantMinutes < 1 || *body.MaxGrantMinutes > maxGrantMinutesCap) {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"max_grant_minutes must be between 1 and 480")
		return false
	}
	return true
}

// endpointScope resolves the optional project/environment an endpoint is
// restricted to. The scope is what lets a production replica be reachable only
// by the people who already hold rights there (ADR-038).
func (a *API) endpointScope(w http.ResponseWriter, r *http.Request, id *auth.Identity, body api.ExternalEndpointCreate) (projectID, environmentID *int64, ok bool) {
	if body.ProjectUuid != nil && *body.ProjectUuid != "" {
		project, found := a.resolveProject(w, r, id, *body.ProjectUuid)
		if !found {
			return nil, nil, false
		}
		projectID = ptr(project.ID)
		if body.EnvironmentUuid != nil && *body.EnvironmentUuid != "" {
			env, found := a.resolveEnvironment(w, r, project, *body.EnvironmentUuid)
			if !found {
				return nil, nil, false
			}
			environmentID = ptr(env.ID)
		}
		return projectID, environmentID, true
	}
	if body.EnvironmentUuid != nil && *body.EnvironmentUuid != "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"environment_uuid requires project_uuid — an environment is scoped by its project")
		return nil, nil, false
	}
	return nil, nil, true
}

// ListExternalEndpoints implements GET /external-endpoints.
func (a *API) ListExternalEndpoints(w http.ResponseWriter, r *http.Request, params api.ListExternalEndpointsParams) {
	id, ok := a.require(w, r, auth.PermExternalEndpointsRead)
	if !ok {
		return
	}
	limit, ok := pageLimit(w, r, params.Limit)
	if !ok {
		return
	}
	after, ok := afterID(w, r, params.Cursor)
	if !ok {
		return
	}
	rows, err := a.Store.ListExternalEndpointsPage(r.Context(), store.ListExternalEndpointsPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list external endpoints", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(e store.ExternalEndpoint) int64 { return e.ID })

	data := make([]api.ExternalEndpoint, 0, len(rows))
	for _, row := range rows {
		data = append(data, a.externalEndpointToAPI(r, row, a.liveGrantFor(r, row, id)))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.ExternalEndpoint `json:"data"`
		NextCursor *string                `json:"next_cursor"`
	}{data, cursor})
}

// liveGrantFor returns the caller's own live grant on an endpoint, if any, so
// the UI can show "you have access until 14:30" without a second round-trip.
// Best effort: a lookup failure simply means no badge.
func (a *API) liveGrantFor(r *http.Request, row store.ExternalEndpoint, id *auth.Identity) *store.ExternalEndpointGrant {
	userID := actingUserID(id)
	if userID == nil {
		return nil
	}
	grant, err := a.Store.GetLiveExternalEndpointGrant(r.Context(), store.GetLiveExternalEndpointGrantParams{
		EndpointID: row.ID, UserID: *userID,
	})
	if err != nil {
		return nil
	}
	return &grant
}

// actingUserID is the human behind the request, whichever door they came
// through: the session's user, or the creator an API token is capped by
// (rbac-matrix §4.2). It is already resolved on the identity — authentication
// did it — so this costs nothing and, unlike asking the session store, has an
// answer for a token.
//
// "Who did this" is a person, not a credential: recording the token's creator
// on a tunnel they opened is the honest answer, and it is the same person the
// access grant was issued to.
func actingUserID(id *auth.Identity) *int64 { return id.UserID }

// GetExternalEndpoint implements GET /external-endpoints/{uuid}.
func (a *API) GetExternalEndpoint(w http.ResponseWriter, r *http.Request, endpointUUID api.ExternalEndpointUuid) {
	id, ok := a.require(w, r, auth.PermExternalEndpointsRead)
	if !ok {
		return
	}
	row, ok := a.resolveExternalEndpoint(w, r, id, endpointUUID)
	if !ok {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, a.externalEndpointToAPI(r, row, a.liveGrantFor(r, row, id)))
}

// CreateExternalEndpoint implements POST /external-endpoints.
func (a *API) CreateExternalEndpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := a.require(w, r, auth.PermExternalEndpointsManage)
	if !ok {
		return
	}
	var body api.ExternalEndpointCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	if !validateEndpointBody(w, r, body) {
		return
	}
	server, ok := a.resolveServer(w, r, id, body.ServerUuid)
	if !ok {
		return
	}
	projectID, environmentID, ok := a.endpointScope(w, r, id, body)
	if !ok {
		return
	}

	row, err := a.Store.CreateExternalEndpoint(r.Context(), store.CreateExternalEndpointParams{
		TeamID:          id.TeamID,
		Name:            strings.TrimSpace(body.Name),
		Description:     body.Description,
		Host:            strings.TrimSpace(body.Host),
		Port:            int32(body.Port),
		ServerID:        server.ID,
		ProjectID:       projectID,
		EnvironmentID:   environmentID,
		Criticality:     criticalityOrDefault(body.Criticality),
		MaxGrantMinutes: int32(intOrDefault(body.MaxGrantMinutes, defaultMaxGrantMinutes)),
		CreatedBy:       actingUserID(id),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
				"an external endpoint with this name already exists")
			return
		}
		a.internalError(w, r, "create external endpoint", err)
		return
	}
	a.recordAudit(r, id, "external-endpoint.create", "external_endpoint", row.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, a.externalEndpointToAPI(r, row, nil))
}

// UpdateExternalEndpoint implements PUT /external-endpoints/{uuid}.
func (a *API) UpdateExternalEndpoint(w http.ResponseWriter, r *http.Request, endpointUUID api.ExternalEndpointUuid) {
	id, ok := a.require(w, r, auth.PermExternalEndpointsManage)
	if !ok {
		return
	}
	current, ok := a.resolveExternalEndpoint(w, r, id, endpointUUID)
	if !ok {
		return
	}
	var body api.ExternalEndpointCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	if !validateEndpointBody(w, r, body) {
		return
	}
	server, ok := a.resolveServer(w, r, id, body.ServerUuid)
	if !ok {
		return
	}
	projectID, environmentID, ok := a.endpointScope(w, r, id, body)
	if !ok {
		return
	}

	row, err := a.Store.UpdateExternalEndpoint(r.Context(), store.UpdateExternalEndpointParams{
		ID:              current.ID,
		Name:            strings.TrimSpace(body.Name),
		Description:     body.Description,
		Host:            strings.TrimSpace(body.Host),
		Port:            int32(body.Port),
		ServerID:        server.ID,
		ProjectID:       projectID,
		EnvironmentID:   environmentID,
		Criticality:     criticalityOrDefault(body.Criticality),
		MaxGrantMinutes: int32(intOrDefault(body.MaxGrantMinutes, int(current.MaxGrantMinutes))),
		UpdatedBy:       actingUserID(id),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
				"an external endpoint with this name already exists")
			return
		}
		a.internalError(w, r, "update external endpoint", err)
		return
	}
	// The address and the regime are the security-relevant fields, so they are
	// what the audit diff carries.
	a.recordAuditDiff(r, id, "external-endpoint.update", "external_endpoint", row.Uuid,
		map[string]any{"host": current.Host, "port": current.Port, "criticality": string(current.Criticality)},
		map[string]any{"host": row.Host, "port": row.Port, "criticality": string(row.Criticality)})
	httpapi.WriteJSON(w, http.StatusOK, a.externalEndpointToAPI(r, row, nil))
}

// DeleteExternalEndpoint implements DELETE /external-endpoints/{uuid}. Live
// sessions are not hunted down here: they die at their next dial, exactly like
// a destroyed preview's tunnel (the target simply no longer resolves).
func (a *API) DeleteExternalEndpoint(w http.ResponseWriter, r *http.Request, endpointUUID api.ExternalEndpointUuid) {
	id, ok := a.require(w, r, auth.PermExternalEndpointsManage)
	if !ok {
		return
	}
	row, ok := a.resolveExternalEndpoint(w, r, id, endpointUUID)
	if !ok {
		return
	}
	if _, err := a.Store.DeleteExternalEndpoint(r.Context(), store.DeleteExternalEndpointParams{
		ID: row.ID, TeamID: id.TeamID,
	}); err != nil {
		a.internalError(w, r, "delete external endpoint", err)
		return
	}
	a.recordAudit(r, id, "external-endpoint.delete", "external_endpoint", row.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

func criticalityOrDefault(c *api.ExternalEndpointCreateCriticality) store.ExternalEndpointCriticality {
	if c != nil && *c == api.ExternalEndpointCreateCriticalityStandard {
		return store.ExternalEndpointCriticalityStandard
	}
	// Declaring an external endpoint usually means reaching a real database:
	// sensitive is the default and downgrading is a conscious act (ADR-045 §6).
	return store.ExternalEndpointCriticalitySensitive
}

func intOrDefault(v *int, fallback int) int {
	if v == nil || *v == 0 {
		return fallback
	}
	return *v
}

// tunnelEndReasonRevoked is what a session torn down by a revoked grant is
// recorded and reported as.
const tunnelEndReasonRevoked tunnel.EndReason = "revoked"

var errNoStore = errors.New("no store")

// endSessionsOfGrant closes every live session a grant opened. Without this,
// revoking a grant would mean nothing to someone already connected — which is
// the only reason revocation exists.
func (a *API) endSessionsOfGrant(r *http.Request, grantID int64) {
	rows, err := a.Store.ListLivePortForwardSessionsByGrant(r.Context(), &grantID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, errNoStore) {
			a.Logger.Warn("listing sessions of a revoked grant failed", "error", err)
		}
		return
	}
	for _, row := range rows {
		// Cut first, record second: the socket is what the developer is holding,
		// and they are told why it went away rather than left with a tunnel that
		// died in silence.
		a.Tunnels.Cut(row.ID, tunnelEndReasonRevoked)
		a.endPortForwardSession(row, tunnelEndReasonRevoked)
	}
}

// requiredFactor names the second factor this user must present, chosen by the
// SERVER and never offered as a menu — a choice would let an attacker pick the
// weakest. A passkey outranks a TOTP whenever one is enrolled.
//
// Returns "" when the user holds no confirmed factor at all: on an instance
// where MFA is not enforced that is a real gate, and it is the intended one —
// an endpoint that reaches production is not reachable behind a password alone.
func (a *API) requiredFactor(r *http.Request, userID int64) string {
	if n, err := a.Store.CountPasskeysForUser(r.Context(), userID); err == nil && n > 0 {
		return "passkey"
	}
	if factor, err := a.Store.GetMfaFactorForUser(r.Context(), userID); err == nil && factor.ConfirmedAt.Valid {
		return "totp"
	}
	return ""
}

// freshFactor reports whether the session carries a recent ceremony of the
// required kind. The two markers are separate columns on purpose: a TOTP must
// never satisfy the passkey-only ritual the root terminal requires.
func freshFactor(sess *store.GetSessionByTokenHashRow, factor string) bool {
	var at pgtype.Timestamptz
	switch factor {
	case "passkey":
		at = sess.MfaVerifiedAt
	case "totp":
		at = sess.TotpVerifiedAt
	default:
		return false
	}
	return at.Valid && time.Since(at.Time) <= grantStepUpWindow
}

// RequestExternalEndpointGrant implements
// POST /external-endpoints/{uuid}/grants — the access request of ADR-045 §5.
//
// Called while a grant is still live it RENEWS it: the window is pushed back
// and the sessions it opened keep running, so a transfer in flight survives.
// A renewal costs exactly what the first request cost — a fresh reason and a
// fresh factor — because one that skipped the ceremony would be an unbounded
// grant delivered in slices.
func (a *API) RequestExternalEndpointGrant(w http.ResponseWriter, r *http.Request, endpointUUID api.ExternalEndpointUuid) {
	id, ok := a.require(w, r, auth.PermPortForwardsOpen)
	if !ok {
		return
	}
	endpoint, ok := a.resolveExternalEndpoint(w, r, id, endpointUUID)
	if !ok {
		return
	}
	if !a.endpointInScope(w, r, id, endpoint) {
		return
	}

	// Browser sessions only: rbac-matrix §5 is explicit that a token cannot
	// re-authenticate, and a grant without a fresh factor is exactly what this
	// endpoint exists to prevent.
	if !id.Session || a.Sessions == nil {
		httpapi.WriteError(w, r, http.StatusForbidden, "stepup_required",
			"an access grant is requested from the dashboard: an API token cannot re-authenticate")
		return
	}
	sess, err := a.Sessions.SessionFromRequest(r.Context(), r)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "no active session")
		return
	}

	var body api.ExternalEndpointGrantCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" || len(reason) > 500 {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"a reason is required (1–500 characters)")
		return
	}
	minutes := body.DurationMinutes
	if minutes < 1 {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"duration_minutes must be at least 1")
		return
	}
	// Clamped rather than refused: asking for more than the endpoint allows is
	// a reasonable thing to do, and the endpoint's ceiling is the answer.
	if minutes > int(endpoint.MaxGrantMinutes) {
		minutes = int(endpoint.MaxGrantMinutes)
	}

	factor := a.requiredFactor(r, sess.UserID)
	if factor == "" {
		a.Audit.Record(r, id, audit.Event{
			Action: "external-endpoint.grant", TargetKind: "external_endpoint",
			TargetUUID: endpoint.Uuid, Result: store.AuditResultDenied,
		})
		httpapi.WriteError(w, r, http.StatusForbidden, "stepup_required",
			"enrol a passkey or a TOTP factor first — an external endpoint is not reachable behind a password alone")
		return
	}
	if !freshFactor(sess, factor) {
		a.Audit.Record(r, id, audit.Event{
			Action: "external-endpoint.grant", TargetKind: "external_endpoint",
			TargetUUID: endpoint.Uuid, Result: store.AuditResultDenied,
		})
		httpapi.WriteError(w, r, http.StatusForbidden, "stepup_required",
			"this request needs a fresh "+factor+" re-authentication (rbac-matrix §5)")
		return
	}

	expires := pgtype.Timestamptz{Time: time.Now().Add(time.Duration(minutes) * time.Minute), Valid: true}

	// A live grant is extended in place, so the sessions it opened survive.
	if live, err := a.Store.GetLiveExternalEndpointGrant(r.Context(), store.GetLiveExternalEndpointGrantParams{
		EndpointID: endpoint.ID, UserID: sess.UserID,
	}); err == nil {
		renewed, err := a.Store.ExtendExternalEndpointGrant(r.Context(), store.ExtendExternalEndpointGrantParams{
			ID: live.ID, ExpiresAt: expires, Reason: reason, Factor: factor,
		})
		if err != nil {
			a.internalError(w, r, "renew endpoint grant", err)
			return
		}
		a.extendSessionsOfGrant(r, renewed)
		a.recordAudit(r, id, "external-endpoint.grant.renew", "external_endpoint_grant", renewed.Uuid)
		a.notifyGrant(r, id, endpoint, renewed, true)
		httpapi.WriteJSON(w, http.StatusCreated, grantToAPI(renewed, ""))
		return
	}

	grant, err := a.Store.CreateExternalEndpointGrant(r.Context(), store.CreateExternalEndpointGrantParams{
		EndpointID: endpoint.ID,
		UserID:     sess.UserID,
		Reason:     reason,
		Factor:     factor,
		// Self-service in v1: the requester is their own grantor. Stored apart
		// from user_id so third-party approval is a later feature, not a
		// migration (ADR-045 §5).
		GrantedBy: &sess.UserID,
		ExpiresAt: expires,
	})
	if err != nil {
		a.internalError(w, r, "create endpoint grant", err)
		return
	}
	a.recordAudit(r, id, "external-endpoint.grant", "external_endpoint_grant", grant.Uuid)
	a.notifyGrant(r, id, endpoint, grant, false)
	httpapi.WriteJSON(w, http.StatusCreated, grantToAPI(grant, ""))
}

// endpointInScope checks that an endpoint's declared project belongs to the
// caller's team.
//
// ADR-045 §1 says more than that: it says `port-forwards:open` is "evaluated
// against that endpoint's scope". With ADR-047 withdrawing scoped assignments
// there is no per-project permission left to evaluate — a member holds their
// role over the whole team — so the project/environment fields of an endpoint
// are **documentation of intent, not a boundary**. They are kept because they
// say what a destination is for, and the dashboard labels them as such rather
// than implying a protection nothing enforces.
func (a *API) endpointInScope(w http.ResponseWriter, r *http.Request, id *auth.Identity, endpoint store.ExternalEndpoint) bool {
	if endpoint.ProjectID == nil {
		return true
	}
	project, err := a.Store.GetProjectByID(r.Context(), *endpoint.ProjectID)
	if err != nil || project.TeamID != id.TeamID {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "external endpoint not found")
		return false
	}
	return true
}

// extendSessionsOfGrant pushes back the deadline of the sessions a renewed
// grant opened. The bridge holds its own timer, so the row is the record and
// the running tunnel keeps its budget until it re-reads it at the next attach;
// what matters here is that the stored deadline never contradicts the grant.
func (a *API) extendSessionsOfGrant(r *http.Request, grant store.ExternalEndpointGrant) {
	rows, err := a.Store.ListLivePortForwardSessionsByGrant(r.Context(), &grant.ID)
	if err != nil {
		return
	}
	for _, row := range rows {
		if err := a.Store.SetPortForwardAuthorizedUntil(r.Context(), store.SetPortForwardAuthorizedUntilParams{
			ID: row.ID, AuthorizedUntil: grant.ExpiresAt,
		}); err != nil {
			a.Logger.Warn("extending a tunnel deadline failed", "session", uuidString(row.Uuid), "error", err)
		}
	}
}

// notifyGrant announces on the team's channels that someone took access to a
// sensitive endpoint. A detective control that costs nothing: nobody reads an
// audit log at 3am, but somebody notices a message. Published through the
// transactional outbox like every other domain event (§24.2).
func (a *API) notifyGrant(r *http.Request, id *auth.Identity, endpoint store.ExternalEndpoint, grant store.ExternalEndpointGrant, renewed bool) {
	if a.Audit == nil || endpoint.Criticality != store.ExternalEndpointCriticalitySensitive {
		return
	}
	eventType := "external_endpoint.grant.v1"
	if renewed {
		eventType = "external_endpoint.grant.renewed.v1"
	}
	a.Audit.Outbox(r.Context(), a.Store, eventType, teamUUID(id), endpoint.Uuid,
		"external-endpoint:"+uuidString(endpoint.Uuid), map[string]any{
			"endpoint":   endpoint.Name,
			"host":       endpoint.Host,
			"port":       endpoint.Port,
			"reason":     grant.Reason,
			"factor":     grant.Factor,
			"expires_at": grant.ExpiresAt.Time.Format(time.RFC3339),
		})
}

// RevokeExternalEndpointGrant implements DELETE /external-endpoint-grants/{uuid}.
// Revoking tears down the sessions the grant opened — otherwise it would mean
// nothing to someone already connected, which is the only case that matters.
func (a *API) RevokeExternalEndpointGrant(w http.ResponseWriter, r *http.Request, grantUUID api.GrantUuid) {
	id, ok := a.require(w, r, auth.PermExternalEndpointsManage)
	if !ok {
		return
	}
	u, ok := a.scanUUID(w, r, grantUUID, "grant")
	if !ok {
		return
	}
	existing, err := a.Store.GetExternalEndpointGrantByUUID(r.Context(), u)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "grant not found")
		return
	}
	// Team boundary: the grant is reached through its endpoint.
	endpoint, err := a.Store.GetExternalEndpointByID(r.Context(), existing.EndpointID)
	if err != nil || endpoint.TeamID != id.TeamID {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "grant not found")
		return
	}
	revoked, err := a.Store.RevokeExternalEndpointGrant(r.Context(), store.RevokeExternalEndpointGrantParams{
		Uuid: u, RevokedBy: actingUserID(id),
	})
	if err != nil {
		// Already revoked: the window is closed either way, so this is not an
		// error worth surfacing.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	a.endSessionsOfGrant(r, revoked.ID)
	a.recordAudit(r, id, "external-endpoint.grant.revoke", "external_endpoint_grant", revoked.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// ListExternalEndpointGrants implements GET /external-endpoints/{uuid}/grants.
func (a *API) ListExternalEndpointGrants(w http.ResponseWriter, r *http.Request, endpointUUID api.ExternalEndpointUuid, params api.ListExternalEndpointGrantsParams) {
	id, ok := a.require(w, r, auth.PermExternalEndpointsRead)
	if !ok {
		return
	}
	endpoint, ok := a.resolveExternalEndpoint(w, r, id, endpointUUID)
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
	// The grant list walks ids downwards (newest first), so an absent cursor
	// starts above every row rather than below.
	if before == 0 {
		before = int64(^uint64(0) >> 1)
	}
	rows, err := a.Store.ListExternalEndpointGrantsPage(r.Context(), store.ListExternalEndpointGrantsPageParams{
		EndpointID: endpoint.ID, BeforeID: before, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list endpoint grants", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(g store.ListExternalEndpointGrantsPageRow) int64 { return g.ID })

	data := make([]api.ExternalEndpointGrant, 0, len(rows))
	for _, row := range rows {
		data = append(data, grantToAPI(store.ExternalEndpointGrant{
			Uuid: row.Uuid, Reason: row.Reason, Factor: row.Factor,
			RenewedFrom: row.RenewedFrom, RequestedAt: row.RequestedAt,
			ExpiresAt: row.ExpiresAt, RevokedAt: row.RevokedAt,
		}, row.UserEmail))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.ExternalEndpointGrant `json:"data"`
		NextCursor *string                     `json:"next_cursor"`
	}{data, cursor})
}

// teamUUID renders the acting team's public uuid in the pgtype the outbox
// speaks. The identity carries it as a string, the event table as a UUID.
func teamUUID(id *auth.Identity) pgtype.UUID {
	var u pgtype.UUID
	_ = u.Scan(id.TeamUUID)
	return u
}

// CreateExternalEndpointPortForward implements
// POST /external-endpoints/{uuid}/port-forwards.
//
// The mint takes NO body: neither host nor port is accepted from the client,
// both were frozen at declaration. It is therefore stricter than the ADR-032
// mints, which still take a port — and the tunnel protocol stays addressless,
// which is the whole reason this feature is not a port scanner.
func (a *API) CreateExternalEndpointPortForward(w http.ResponseWriter, r *http.Request, endpointUUID api.ExternalEndpointUuid) {
	id, ok := a.require(w, r, auth.PermPortForwardsOpen)
	if !ok {
		return
	}
	endpoint, ok := a.resolveExternalEndpoint(w, r, id, endpointUUID)
	if !ok {
		return
	}
	if !a.endpointInScope(w, r, id, endpoint) {
		return
	}

	// `standard` behaves exactly like an ADR-032 tunnel — no grant, no
	// ceremony, no new friction on the everyday case. That is deliberate: if
	// `standard` cost anything, every endpoint would be declared `standard`
	// and the `sensitive` regime would protect nothing.
	var grantID *int64
	var authorizedUntil pgtype.Timestamptz
	if endpoint.Criticality == store.ExternalEndpointCriticalitySensitive {
		// The acting HUMAN, whichever door they came through: the session's
		// user, or the creator a token is capped by (§4.2). Not the session
		// alone — the grant is requested from the dashboard but spent from the
		// CLI, which authenticates with a token. Requiring a session here made
		// a `sensitive` endpoint unreachable from the CLI no matter how many
		// grants were issued, while the mint kept handing back the URL of the
		// page that issues them (ADR-045 §5: "the CLI opens it and polls until
		// the grant exists, then replays the mint").
		//
		// The grant remains the control: it is obtained with a browser session
		// and a fresh second factor, which the holder of a stolen CLI token
		// does not have.
		userID := id.UserID
		if userID == nil {
			a.denyMint(r, id, endpoint)
			a.writeAccessRequestRequired(w, r, endpoint,
				"this endpoint requires an access grant, which is requested from the dashboard")
			return
		}
		grant, err := a.Store.GetLiveExternalEndpointGrant(r.Context(), store.GetLiveExternalEndpointGrantParams{
			EndpointID: endpoint.ID, UserID: *userID,
		})
		if err != nil {
			a.denyMint(r, id, endpoint)
			a.writeAccessRequestRequired(w, r, endpoint,
				"no live access grant for this endpoint")
			return
		}
		grantID = &grant.ID
		// The grant IS the deadline: ADR-032's 4 h ceiling does not stack on
		// top of it, because two bounds racing each other are two numbers to
		// explain where one suffices.
		authorizedUntil = grant.ExpiresAt
	}

	open, err := a.Store.CountOpenPortForwardSessions(r.Context(), id.TeamID)
	if err != nil {
		a.internalError(w, r, "port-forward", err)
		return
	}
	if open >= portForwardTeamCap {
		httpapi.WriteError(w, r, http.StatusConflict, "port_forward_limit",
			fmt.Sprintf("this team already has %d open port-forward sessions", open))
		return
	}
	token, err := newPortForwardToken()
	if err != nil {
		a.internalError(w, r, "port-forward", err)
		return
	}
	row, err := a.Store.CreateEndpointPortForwardSession(r.Context(), store.CreateEndpointPortForwardSessionParams{
		TeamID:             id.TeamID,
		UserID:             actingUserID(id),
		ServerID:           &endpoint.ServerID,
		ExternalEndpointID: &endpoint.ID,
		GrantID:            grantID,
		TargetName:         endpoint.Name,
		TargetPort:         endpoint.Port,
		ClientIp:           clientAddr(r),
		TokenHash:          hashPortForwardToken(token),
		TokenExpiresAt:     pgtype.Timestamptz{Time: time.Now().Add(portForwardTokenTTL), Valid: true},
		AuthorizedUntil:    authorizedUntil,
	})
	if err != nil {
		a.internalError(w, r, "port-forward", err)
		return
	}
	a.recordAudit(r, id, "port-forward.open", "port_forward_session", row.Uuid)

	out := api.PortForwardSession{
		Uuid:           uuidString(row.Uuid),
		Port:           int(endpoint.Port),
		WebsocketPath:  tunnelWebsocketPath,
		Token:          token,
		TokenExpiresAt: row.TokenExpiresAt.Time,
	}
	// Announced at open, not only when it ends: a deadline that arrives
	// unannounced reads as a bug, and the developer plans a long transfer
	// around this value.
	if authorizedUntil.Valid {
		out.AuthorizedUntil = &authorizedUntil.Time
	} else {
		out.AuthorizedUntil = ptr(time.Now().Add(tunnel.DefaultMaxDuration))
	}
	httpapi.WriteJSON(w, http.StatusCreated, out)
}

func (a *API) denyMint(r *http.Request, id *auth.Identity, endpoint store.ExternalEndpoint) {
	a.Audit.Record(r, id, audit.Event{
		Action: "port-forward.open", TargetKind: "external_endpoint",
		TargetUUID: endpoint.Uuid, Result: store.AuditResultDenied,
	})
}

// writeAccessRequestRequired answers the mint with the code and the URL the
// CLI needs: it opens that page, polls until the grant exists, then replays
// the call — the same choreography as `akerdock login` (ADR-031). Without the
// URL the developer is told "no" and left to find the page themselves, which
// is how a control becomes something people route around.
func (a *API) writeAccessRequestRequired(w http.ResponseWriter, r *http.Request, endpoint store.ExternalEndpoint, message string) {
	requestURL := ""
	if st, err := a.Settings.Get(r.Context()); err == nil && st.Fqdn != nil && *st.Fqdn != "" {
		requestURL = "https://" + *st.Fqdn + "/external-endpoints/" + uuidString(endpoint.Uuid) + "/request-access"
	}
	// The house error shape is flat (code/message/request_id) and every client
	// decodes exactly that; request_url rides alongside it rather than nesting
	// the error inside a wrapper only this one endpoint would use.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":        "access_request_required",
		"message":     message,
		"request_url": requestURL,
	})
}
