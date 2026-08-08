// Ingress endpoints — the mirror of ADR-045's bastion (ADR-060): a declared,
// stable public URL relayed to a developer's machine. Where an external
// endpoint is a host:port AkerDock dials OUT to, an ingress endpoint is a
// hostname AkerDock accepts visitors ON and relays to whoever holds the
// attach socket.
//
// Declaring one publishes a hostname onto arbitrary laptop software and is an
// admin act (ingress-endpoints:manage); attaching exposes one's own machine
// and is a member act, separately grantable (ingress-tunnels:open). The wall
// (§5) protects the visitor side, sso by default; the exclusive-occupancy
// rule (§6) keeps one URL pointing at one laptop.
package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// ingressTokenTTL is how long an attach token stays redeemable at the agent.
// Same 60 s as every mint: long enough for the CLI to dial, short enough that
// the armed expectation is a transient in-memory secret.
const ingressTokenTTL = 60 * time.Second

// ingressTeamCap bounds concurrent ingress tunnels per team. Lower than the
// port-forward cap: an ingress endpoint is a declared resource, and one live
// tunnel per endpoint is the model — this is a backstop, not the primary
// limit (that is the per-endpoint occupancy).
const ingressTeamCap = 20

func newIngressToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "akdi_" + hex.EncodeToString(raw), nil
}

func hashIngressToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// resolveIngressEndpoint loads a team-scoped endpoint or writes the 404.
func (a *API) resolveIngressEndpoint(w http.ResponseWriter, r *http.Request, id *auth.Identity, endpointUUID string) (store.IngressEndpoint, bool) {
	u, ok := a.scanUUID(w, r, endpointUUID, "ingress endpoint")
	if !ok {
		return store.IngressEndpoint{}, false
	}
	row, err := a.Store.GetIngressEndpointByUUID(r.Context(), store.GetIngressEndpointByUUIDParams{
		Uuid: u, TeamID: id.TeamID,
	})
	return resolveRow(a, w, r, "ingress endpoint", row, err)
}

func (a *API) ingressEndpointToAPI(r *http.Request, row store.IngressEndpoint) api.IngressEndpoint {
	out := api.IngressEndpoint{
		Uuid:        uuidString(row.Uuid),
		Name:        row.Name,
		Description: row.Description,
		Fqdn:        row.Fqdn,
		Url:         "https://" + row.Fqdn,
		Access:      api.IngressEndpointAccess(row.Access),
		CreatedAt:   row.CreatedAt.Time,
	}
	if row.UpdatedAt.Valid {
		out.UpdatedAt = &row.UpdatedAt.Time
	}
	if server, err := a.Store.GetServerByID(r.Context(), row.ServerID); err == nil {
		out.ServerUuid = uuidString(server.Uuid)
	}
	if occ, err := a.Store.GetOpenIngressSessionForEndpoint(r.Context(), &row.ID); err == nil {
		out.Occupied = true
		out.OccupantEmail = occ.UserEmail
	}
	return out
}

// ListIngressEndpoints implements GET /ingress-endpoints.
func (a *API) ListIngressEndpoints(w http.ResponseWriter, r *http.Request) {
	id, ok := a.require(w, r, auth.PermIngressEndpointsRead)
	if !ok {
		return
	}
	rows, err := a.Store.ListIngressEndpoints(r.Context(), id.TeamID)
	if err != nil {
		a.internalError(w, r, "list ingress endpoints", err)
		return
	}
	data := make([]api.IngressEndpoint, 0, len(rows))
	for _, row := range rows {
		data = append(data, a.ingressEndpointToAPI(r, store.IngressEndpoint{
			ID: row.ID, Uuid: row.Uuid, TeamID: row.TeamID, Name: row.Name,
			Description: row.Description, Fqdn: row.Fqdn, ServerID: row.ServerID,
			Access: row.Access, BasicAuthHash: row.BasicAuthHash,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Version: row.Version,
		}))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data []api.IngressEndpoint `json:"data"`
	}{data})
}

// GetIngressEndpoint implements GET /ingress-endpoints/{uuid}.
func (a *API) GetIngressEndpoint(w http.ResponseWriter, r *http.Request, endpointUUID api.IngressEndpointUuid) {
	id, ok := a.require(w, r, auth.PermIngressEndpointsRead)
	if !ok {
		return
	}
	row, ok := a.resolveIngressEndpoint(w, r, id, endpointUUID)
	if !ok {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, a.ingressEndpointToAPI(r, row))
}

// validIngressFQDN enforces the domains.fqdn shape: an exact hostname, no
// scheme/port/path, no wildcard.
func validIngressFQDN(fqdn string) bool {
	fqdn = strings.TrimSpace(fqdn)
	if fqdn == "" || len(fqdn) > 253 || strings.ContainsAny(fqdn, " \t/:*") {
		return false
	}
	return strings.Contains(fqdn, ".")
}

// ingressAccessOrDefault resolves the access mode, defaulting to sso — a fresh
// endpoint admits the team's authenticated users and nobody else (ADR-060 §5).
func ingressAccessOrDefault(a *api.IngressEndpointCreateAccess) store.IngressAccess {
	if a == nil {
		return store.IngressAccessSso
	}
	switch *a {
	case api.IngressEndpointCreateAccessNone:
		return store.IngressAccessNone
	case api.IngressEndpointCreateAccessBasicAuth:
		return store.IngressAccessBasicAuth
	default:
		return store.IngressAccessSso
	}
}

// CreateIngressEndpoint implements POST /ingress-endpoints.
func (a *API) CreateIngressEndpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := a.require(w, r, auth.PermIngressEndpointsManage)
	if !ok {
		return
	}
	var body api.IngressEndpointCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 63 {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "name is required (1–63 characters)")
		return
	}
	if !validIngressFQDN(body.Fqdn) {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"fqdn must be an exact hostname — no scheme, port, path or wildcard")
		return
	}
	access := ingressAccessOrDefault(body.Access)
	hash, ok := a.ingressBasicAuthHash(w, r, access, body.BasicAuthPassword)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, body.ServerUuid)
	if !ok {
		return
	}

	row, err := a.Store.CreateIngressEndpoint(r.Context(), store.CreateIngressEndpointParams{
		TeamID:        id.TeamID,
		Name:          name,
		Description:   body.Description,
		Fqdn:          strings.TrimSpace(body.Fqdn),
		ServerID:      server.ID,
		Access:        access,
		BasicAuthHash: hash,
		CreatedBy:     actingUserID(id),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
				"an ingress endpoint with this name or hostname already exists")
			return
		}
		a.internalError(w, r, "create ingress endpoint", err)
		return
	}
	// Register the FQDN in the routing namespace; a collision with an app or a
	// preview surfaces here as a unique violation, which is the whole point of
	// the instance-wide (fqdn, path) constraint (INV-002).
	du, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "register ingress domain", err)
		return
	}
	if _, err := a.Store.CreateIngressDomain(r.Context(), store.CreateIngressDomainParams{
		Uuid: du, IngressEndpointID: &row.ID, Fqdn: row.Fqdn,
	}); err != nil {
		_, _ = a.Store.DeleteIngressEndpoint(r.Context(), store.DeleteIngressEndpointParams{Uuid: row.Uuid, TeamID: id.TeamID})
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
				"this hostname is already routed by another resource")
			return
		}
		a.internalError(w, r, "register ingress domain", err)
		return
	}
	a.enqueueIngressRouting(r, row, server.ID)
	a.recordAudit(r, id, "ingress-endpoint.create", "ingress_endpoint", row.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, a.ingressEndpointToAPI(r, row))
}

// UpdateIngressEndpoint implements PUT /ingress-endpoints/{uuid}. The FQDN and
// server are immutable (baked into the certificate and the router); the label,
// description and access mode may change.
func (a *API) UpdateIngressEndpoint(w http.ResponseWriter, r *http.Request, endpointUUID api.IngressEndpointUuid) {
	id, ok := a.require(w, r, auth.PermIngressEndpointsManage)
	if !ok {
		return
	}
	current, ok := a.resolveIngressEndpoint(w, r, id, endpointUUID)
	if !ok {
		return
	}
	var body api.IngressEndpointUpdate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 63 {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "name is required (1–63 characters)")
		return
	}
	access := current.Access
	if body.Access != nil {
		access = ingressUpdateAccess(*body.Access)
	}
	hash := current.BasicAuthHash
	if access == store.IngressAccessBasicAuth && body.BasicAuthPassword != nil && *body.BasicAuthPassword != "" {
		h, ok := a.ingressBasicAuthHash(w, r, access, body.BasicAuthPassword)
		if !ok {
			return
		}
		hash = h
	}
	if access == store.IngressAccessBasicAuth && (hash == nil || *hash == "") {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"basic_auth requires a password")
		return
	}
	if access != store.IngressAccessBasicAuth {
		hash = nil
	}

	row, err := a.Store.UpdateIngressEndpoint(r.Context(), store.UpdateIngressEndpointParams{
		Uuid: current.Uuid, TeamID: id.TeamID, Name: name,
		Description: body.Description, Access: access, BasicAuthHash: hash,
		UpdatedBy: actingUserID(id),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
				"an ingress endpoint with this name already exists")
			return
		}
		a.internalError(w, r, "update ingress endpoint", err)
		return
	}
	// Only re-apply the router when the access regime actually changed — the
	// wall is the sole routing-relevant field, and switching to `none` is the
	// audited moment (ADR-060 §5).
	if access != current.Access {
		a.enqueueIngressRouting(r, row, row.ServerID)
	}
	a.recordAuditDiff(r, id, "ingress-endpoint.update", "ingress_endpoint", row.Uuid,
		map[string]any{"access": string(current.Access)},
		map[string]any{"access": string(row.Access)})
	httpapi.WriteJSON(w, http.StatusOK, a.ingressEndpointToAPI(r, row))
}

func ingressUpdateAccess(a api.IngressEndpointUpdateAccess) store.IngressAccess {
	switch a {
	case api.IngressEndpointUpdateAccessNone:
		return store.IngressAccessNone
	case api.IngressEndpointUpdateAccessBasicAuth:
		return store.IngressAccessBasicAuth
	default:
		return store.IngressAccessSso
	}
}

// DeleteIngressEndpoint implements DELETE /ingress-endpoints/{uuid}. The live
// attach, if any, is cut through the command channel before the row goes, so
// the laptop is told `revoked` rather than seeing its socket die silently.
func (a *API) DeleteIngressEndpoint(w http.ResponseWriter, r *http.Request, endpointUUID api.IngressEndpointUuid) {
	id, ok := a.require(w, r, auth.PermIngressEndpointsManage)
	if !ok {
		return
	}
	row, ok := a.resolveIngressEndpoint(w, r, id, endpointUUID)
	if !ok {
		return
	}
	if occ, err := a.Store.GetOpenIngressSessionForEndpoint(r.Context(), &row.ID); err == nil {
		a.cutIngressSession(r.Context(), row.ServerID, occ.Uuid, string(tunnelEndReasonRevoked))
		_, _ = a.Store.EndIngressSession(r.Context(), store.EndIngressSessionParams{
			ID: occ.ID, EndReason: ingressEndReason(tunnelEndReasonRevoked),
		})
	}
	serverID := row.ServerID
	if _, err := a.Store.DeleteIngressEndpoint(r.Context(), store.DeleteIngressEndpointParams{
		Uuid: row.Uuid, TeamID: id.TeamID,
	}); err != nil {
		a.internalError(w, r, "delete ingress endpoint", err)
		return
	}
	// The row is gone; the routing job removes the file and the agent host
	// entry by UUID (the payload carries what the missing row cannot).
	a.enqueueIngressRoutingRemoval(r, row.Uuid, serverID)
	a.recordAudit(r, id, "ingress-endpoint.delete", "ingress_endpoint", row.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// ingressBasicAuthHash validates and bcrypt-hashes the basic-auth password for
// the wall. The clear password never touches the database.
func (a *API) ingressBasicAuthHash(w http.ResponseWriter, r *http.Request, access store.IngressAccess, password *string) (*string, bool) {
	if access != store.IngressAccessBasicAuth {
		return nil, true
	}
	if password == nil || *password == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"basic_auth requires a password")
		return nil, false
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(*password), bcrypt.DefaultCost)
	if err != nil {
		a.internalError(w, r, "hash basic auth", err)
		return nil, false
	}
	// Traefik's basicAuth wants "user:hash"; the user is fixed, the password is
	// the secret.
	pair := "akerdock:" + string(hash)
	return &pair, true
}

// CreateIngressTunnel implements POST /ingress-endpoints/{uuid}/tunnels — the
// mint. Empty body: the hostname, server and access were frozen at
// declaration, the local port is the laptop's business. The token is redeemed
// agent-side (§3), so the mint arms the agent over the command channel and
// returns the attach URL on the endpoint's own FQDN.
func (a *API) CreateIngressTunnel(w http.ResponseWriter, r *http.Request, endpointUUID api.IngressEndpointUuid) {
	id, ok := a.require(w, r, auth.PermIngressTunnelsOpen)
	if !ok {
		return
	}
	endpoint, ok := a.resolveIngressEndpoint(w, r, id, endpointUUID)
	if !ok {
		return
	}
	// Exclusive occupancy (§6): one laptop per endpoint. Checked before the
	// team cap so the more specific message wins.
	if occ, err := a.Store.GetOpenIngressSessionForEndpoint(r.Context(), &endpoint.ID); err == nil {
		msg := "this endpoint is already in use"
		if occ.UserEmail != nil {
			msg = "this endpoint is already in use by " + *occ.UserEmail
		}
		httpapi.WriteError(w, r, http.StatusConflict, "occupied", msg)
		return
	}
	// Team-wide backstop above the per-endpoint occupancy (§6): a runaway
	// script cannot mint an unbounded number of pending sessions.
	if open, err := a.Store.CountOpenIngressSessions(r.Context(), id.TeamID); err == nil && open >= ingressTeamCap {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
			"this team already has the maximum number of open ingress tunnels")
		return
	}
	// The agent must be reachable, or no attach can ever be expected — fail the
	// mint now rather than hand the CLI a token nothing will honour.
	if a.AgentRPC == nil {
		httpapi.WriteError(w, r, http.StatusConflict, "server_agent_unavailable",
			"the ingress server's agent is not connected")
		return
	}
	if _, live := a.AgentRPC.Sender(endpoint.ServerID); !live {
		httpapi.WriteError(w, r, http.StatusConflict, "server_agent_unavailable",
			"the ingress server's agent is not connected")
		return
	}

	token, err := newIngressToken()
	if err != nil {
		a.internalError(w, r, "ingress tunnel", err)
		return
	}
	expiresAt := time.Now().Add(ingressTokenTTL)
	row, err := a.Store.CreateIngressSession(r.Context(), store.CreateIngressSessionParams{
		TeamID:         id.TeamID,
		EndpointID:     &endpoint.ID,
		UserID:         actingUserID(id),
		ClientIp:       clientAddr(r),
		TokenHash:      hashIngressToken(token),
		TokenExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		// The partial unique index turns a lost occupancy race into a clean 409.
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, "occupied", "this endpoint is already in use")
			return
		}
		a.internalError(w, r, "ingress tunnel", err)
		return
	}

	// Arm the agent's single-use expectation over the command channel. If it
	// fails, finalize the row so the endpoint is not left occupied by a mint
	// nobody can redeem.
	if err := a.armIngressExpect(r.Context(), endpoint, row, token, expiresAt); err != nil {
		_, _ = a.Store.EndIngressSession(r.Context(), store.EndIngressSessionParams{
			ID: row.ID, EndReason: ingressEndReason(tunnel.EndDisconnect),
		})
		httpapi.WriteError(w, r, http.StatusConflict, "server_agent_unavailable",
			"the ingress server's agent did not accept the session")
		return
	}
	a.recordAudit(r, id, "ingress-tunnel.open", "ingress_tunnel_session", row.Uuid)

	fqdn := endpoint.Fqdn
	httpapi.WriteJSON(w, http.StatusCreated, api.IngressTunnelSession{
		Uuid:           uuidString(row.Uuid),
		Fqdn:           fqdn,
		Url:            "https://" + fqdn,
		AttachUrl:      "wss://" + fqdn + proxy.IngressAttachPath,
		Token:          token,
		TokenExpiresAt: expiresAt,
	})
}

// armIngressExpect sends the single-use attach expectation to the ingress
// server's agent (ADR-060 §3). The clear token never rides the channel; only
// its hash does.
func (a *API) armIngressExpect(ctx context.Context, endpoint store.IngressEndpoint, row store.IngressTunnelSession, token string, expiresAt time.Time) error {
	sender, ok := a.AgentRPC.Sender(endpoint.ServerID)
	if !ok {
		return errNoStore
	}
	_, err := sender.Command(ctx, agentwire.MethodIngressExpect, agentwire.IngressExpectParams{
		SessionUUID:   uuidString(row.Uuid),
		EndpointUUID:  uuidString(endpoint.Uuid),
		TokenSHA256:   hashIngressToken(token),
		ExpiresAtUnix: expiresAt.Unix(),
	})
	return err
}

// cutIngressSession tells the ingress server's agent to end (or disarm) a
// session with a reason. Best-effort: the row is finalized either way, and the
// heartbeat sweep converges any socket a lost command left behind.
func (a *API) cutIngressSession(ctx context.Context, serverID int64, sessionUUID pgtype.UUID, reason string) {
	if a.AgentRPC == nil {
		return
	}
	sender, ok := a.AgentRPC.Sender(serverID)
	if !ok {
		return
	}
	if _, err := sender.Command(ctx, agentwire.MethodIngressCut, agentwire.IngressCutParams{
		SessionUUID: uuidString(sessionUUID), Reason: reason,
	}); err != nil {
		a.Logger.Warn("ingress cut command failed", "session", uuidString(sessionUUID), "error", err)
	}
}

func ingressEndReason(reason tunnel.EndReason) *store.TerminalEndReason {
	v := store.TerminalEndReason(reason)
	return &v
}

// enqueueIngressRouting converges the endpoint's router on a ready proxy server.
func (a *API) enqueueIngressRouting(r *http.Request, row store.IngressEndpoint, serverID int64) {
	a.enqueueIngressRoutingPayload(r, jobs.IngressRoutingPayload{
		EndpointID: row.ID, EndpointUUID: uuidString(row.Uuid), ServerID: serverID,
	}, uuidString(row.Uuid))
}

func (a *API) enqueueIngressRoutingRemoval(r *http.Request, endpointUUID pgtype.UUID, serverID int64) {
	a.enqueueIngressRoutingPayload(r, jobs.IngressRoutingPayload{
		EndpointID: 0, EndpointUUID: uuidString(endpointUUID), ServerID: serverID,
	}, uuidString(endpointUUID))
}

func (a *API) enqueueIngressRoutingPayload(r *http.Request, payload jobs.IngressRoutingPayload, scope string) {
	server, err := a.Store.GetServerByID(r.Context(), payload.ServerID)
	if err != nil || server.ProxyType != store.ProxyTypeTraefik || server.Status != store.ServerStatusReady {
		return
	}
	lockKey := "deploy:ingress:" + scope
	if _, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue: "deploy", Type: jobs.TypeIngressRouting, Payload: payload, LockKey: &lockKey,
	}); err != nil {
		a.Logger.Warn("failed to enqueue ingress routing", "error", err)
	}
}
