// CLI TCP port-forward (ADR-032): a two-step mint/redeem like the terminal.
// POST .../port-forwards mints a single-use attach token bound to a fixed
// container:port; GET /tunnel/ws redeems it and multiplexes TCP streams to
// that target over one WebSocket, dialed server-side over SSH.
package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/adoption"
	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// portForwardTeamCap bounds concurrent tunnel sessions per team (§ADR-032).
const portForwardTeamCap = 10

// portForwardTokenTTL is how long an attach token stays redeemable.
const portForwardTokenTTL = 60 * time.Second

const tunnelWebsocketPath = "/tunnel/ws"

func newPortForwardToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "akdp_" + hex.EncodeToString(raw), nil
}

func hashPortForwardToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// portForwardSpec is what a mint resolved: the server to SSH into, the
// container to dial, and the target port.
type portForwardSpec struct {
	serverID   int64
	resourceID *int64
	previewID  *int64
	name       string
	component  *string
	port       int
}

// CreateApplicationPortForward implements POST /applications/{uuid}/port-forwards.
func (a *API) CreateApplicationPortForward(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.CreateApplicationPortForwardParams) {
	id, ok := a.require(w, r, auth.PermPortForwardsOpen)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	body, ok := decodePortForwardBody(w, r)
	if !ok {
		return
	}
	spec := portForwardSpec{serverID: row.ServerRowID, resourceID: &row.Resource.ID, name: row.Resource.Name, port: body.Port}
	if params.Component != nil && *params.Component != "" {
		if !a.componentExists(w, r, row.Resource.ID, *params.Component) {
			return
		}
		spec.component = params.Component
		spec.name = row.Resource.Name + " · " + *params.Component
	}
	a.createPortForward(w, r, id, spec)
}

// CreateDatabasePortForward implements POST /databases/{uuid}/port-forwards.
func (a *API) CreateDatabasePortForward(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid) {
	id, ok := a.require(w, r, auth.PermPortForwardsOpen)
	if !ok {
		return
	}
	row, ok := a.resolveDatabase(w, r, id, databaseUuid)
	if !ok {
		return
	}
	body, ok := decodePortForwardBody(w, r)
	if !ok {
		return
	}
	dest, err := a.Store.GetDestinationByID(r.Context(), row.Resource.DestinationID)
	if err != nil {
		a.internalError(w, r, "port-forward", err)
		return
	}
	a.createPortForward(w, r, id, portForwardSpec{
		serverID: dest.ServerID, resourceID: &row.Resource.ID, name: row.Resource.Name, port: body.Port,
	})
}

// CreatePreviewPortForward implements
// POST /applications/{uuid}/previews/{uuid}/port-forwards (ADR-032): a tunnel
// into a PR preview's container.
func (a *API) CreatePreviewPortForward(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, previewUuid string, params api.CreatePreviewPortForwardParams) {
	id, ok := a.require(w, r, auth.PermPortForwardsOpen)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	preview, ok := a.resolvePreview(w, r, id, row.Resource.ID, previewUuid)
	if !ok {
		return
	}
	if preview.Status == store.PreviewStatusDestroyed || preview.Status == store.PreviewStatusDestroying {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this preview is destroyed")
		return
	}
	body, ok := decodePortForwardBody(w, r)
	if !ok {
		return
	}
	spec := portForwardSpec{
		serverID: row.ServerRowID, resourceID: &row.Resource.ID, previewID: &preview.ID,
		name: fmt.Sprintf("%s · PR #%d", row.Resource.Name, preview.PrID), port: body.Port,
	}
	if params.Component != nil && *params.Component != "" {
		if !a.componentExists(w, r, row.Resource.ID, *params.Component) {
			return
		}
		spec.component = params.Component
	}
	a.createPortForward(w, r, id, spec)
}

func decodePortForwardBody(w http.ResponseWriter, r *http.Request) (api.PortForwardCreate, bool) {
	var body api.PortForwardCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Port < 1 || body.Port > 65535 {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "port must be an integer in 1–65535")
		return api.PortForwardCreate{}, false
	}
	return body, true
}

// componentExists validates a compose service name against the resource's
// components (same rule as terminal-sessions).
func (a *API) componentExists(w http.ResponseWriter, r *http.Request, resourceID int64, component string) bool {
	components, err := a.Store.ListServiceComponents(r.Context(), resourceID)
	if err != nil {
		a.internalError(w, r, "port-forward", err)
		return false
	}
	for _, c := range components {
		if c.Name == component {
			return true
		}
	}
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound,
		fmt.Sprintf("unknown component %q — see GET /applications/{uuid}/components", component))
	return false
}

// createPortForward is the shared tail: cap check, token mint, audit, response.
func (a *API) createPortForward(w http.ResponseWriter, r *http.Request, id *auth.Identity, spec portForwardSpec) {
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
	var userID *int64
	if id.Session && a.Sessions != nil {
		if sess, err := a.Sessions.SessionFromRequest(r.Context(), r); err == nil {
			userID = &sess.UserID
		}
	}
	row, err := a.Store.CreatePortForwardSession(r.Context(), store.CreatePortForwardSessionParams{
		TeamID:          id.TeamID,
		UserID:          userID,
		ServerID:        &spec.serverID,
		ResourceID:      spec.resourceID,
		PreviewID:       spec.previewID,
		TargetName:      spec.name,
		TargetComponent: spec.component,
		TargetPort:      int32(spec.port),
		ClientIp:        clientAddr(r),
		TokenHash:       hashPortForwardToken(token),
		TokenExpiresAt:  pgtype.Timestamptz{Time: time.Now().Add(portForwardTokenTTL), Valid: true},
	})
	if err != nil {
		a.internalError(w, r, "port-forward", err)
		return
	}
	a.recordAudit(r, id, "port-forward.open", "port_forward_session", row.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, api.PortForwardSession{
		Uuid:           uuidString(row.Uuid),
		Port:           spec.port,
		WebsocketPath:  tunnelWebsocketPath,
		Token:          token,
		TokenExpiresAt: row.TokenExpiresAt.Time,
	})
}

// TunnelWebSocket implements GET /tunnel/ws?token=… — outside the contract
// (ADR-032, like /terminal/ws), mounted next to /auth. The one-time token is
// the sole credential; the SQL claim consumes it atomically.
func (a *API) TunnelWebSocket(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "missing tunnel token")
		return
	}
	row, err := a.Store.ClaimPortForwardSession(r.Context(), hashPortForwardToken(token))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "invalid, expired or already used tunnel token")
		return
	}

	client, addr, errMsg := a.tunnelTarget(r.Context(), row)
	if errMsg != "" {
		a.endPortForwardSession(row, tunnel.EndDisconnect)
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, errMsg)
		return
	}
	defer func() { _ = client.Close() }()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"akerdock-tunnel-v1"}})
	if err != nil {
		a.endPortForwardSession(row, tunnel.EndDisconnect)
		return
	}
	// The client frames raw TCP into binary messages whose size follows the
	// forwarded stream (a bulk COPY upload sends large frames). Without this,
	// coder/websocket's 32 KiB default read limit aborts the whole tunnel the
	// moment a big frame arrives — the client sets the same unlimited read.
	conn.SetReadLimit(-1)
	dial := func(ctx context.Context) (net.Conn, error) { return client.DialTCP(addr) }
	// Registered for the whole life of the bridge so a revoked grant or an
	// operator's close reaches this socket, rather than only the row that
	// records it (ADR-045 §5).
	bounds := sessionBounds(row)
	bounds.Cancel = a.Tunnels.register(row.ID)
	bounds.OnHeartbeat = func(parent context.Context) bool {
		// The socket remains the source of truth while this process is alive.
		// Persistence is only the crash/restart net, so a transient database
		// failure is logged and retried on the next 20-second heartbeat.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		defer cancel()
		n, err := a.Store.HeartbeatPortForwardSession(ctx, row.ID)
		if err != nil {
			a.Logger.Warn("port-forward heartbeat failed",
				"session", uuidString(row.Uuid), "error", err)
			return true
		}
		// Zero means another replica or the scheduler has finalized the row.
		// Do not leave the actual socket alive after its durable authorization
		// is gone.
		return n > 0
	}
	defer a.Tunnels.unregister(row.ID)
	reason := tunnel.Bridge(r.Context(), tunnelConn{conn}, dial, bounds)
	// A session cut by its grant running out is neither an idle timeout nor a
	// revocation, and the CLI says exactly that to the developer (ADR-045 §5).
	if reason == tunnel.EndMaxDuration && row.GrantID != nil {
		reason = endReasonGrantExpired
	}
	a.endPortForwardSession(row, reason)
	_ = conn.Close(websocket.StatusNormalClosure, string(reason))
}

// tunnelTarget dials the session's server and resolves the address to connect
// to: a declared external endpoint's own host:port (ADR-045), or the container's
// IP on its Docker network — both reachable from the host over SSH.
func (a *API) tunnelTarget(ctx context.Context, row store.PortForwardSession) (*sshexec.Client, string, string) {
	if row.ServerID == nil {
		return nil, "", "the target no longer exists"
	}
	server, err := a.Store.GetServerByID(ctx, *row.ServerID)
	if err != nil {
		return nil, "", "the target server no longer exists"
	}

	// External endpoint (ADR-045): the address was frozen at declaration, so
	// there is no container to inspect — the egress server dials it directly.
	if row.ExternalEndpointID != nil {
		endpoint, err := a.Store.GetExternalEndpointByID(ctx, *row.ExternalEndpointID)
		if err != nil {
			return nil, "", "the target endpoint no longer exists"
		}
		client, msg := a.dialSessionServer(ctx, server)
		if msg != "" {
			return nil, "", msg
		}
		return client, net.JoinHostPort(endpoint.Host, strconv.Itoa(int(endpoint.Port))), ""
	}

	if row.ResourceID == nil {
		return nil, "", "the target no longer exists"
	}
	res, err := a.Store.GetResourceByID(ctx, *row.ResourceID)
	if err != nil {
		return nil, "", "the target resource no longer exists"
	}
	// A preview instance names its containers after the PREVIEW uuid, not the
	// resource's (INV-011); a destroyed preview has nothing to dial.
	base := uuidString(res.Uuid)
	container := adoption.ContainerName(res.Adoption, base)
	if row.PreviewID != nil {
		preview, err := a.Store.GetPreviewByID(ctx, *row.PreviewID)
		if err != nil || preview.Status == store.PreviewStatusDestroyed {
			return nil, "", "the preview no longer exists — it may have been destroyed"
		}
		base = uuidString(preview.Uuid)
		container = base
	}
	if row.TargetComponent != nil && *row.TargetComponent != "" {
		container = base + "-" + *row.TargetComponent
	}

	client, msg := a.dialSessionServer(ctx, server)
	if msg != "" {
		return nil, "", msg
	}
	// The container's IP on its Docker network — reachable host→container even
	// without a published port. First network wins (INV-011 naming).
	ip, err := containerIP(ctx, client, container)
	if err != nil {
		_ = client.Close()
		return nil, "", "the target container is not running"
	}
	return client, fmt.Sprintf("%s:%d", ip, row.TargetPort), ""
}

// endReasonGrantExpired is the ADR-045 close reason: the tunnel outlived
// nothing — its authorization ran out. It mirrors the enum value added to
// terminal_end_reason, so the audit row and the message the developer reads
// come from the same value.
const endReasonGrantExpired tunnel.EndReason = "grant_expired"

// sessionBounds turns the session's authorized_until into the bridge's maximum
// duration. On a `sensitive` external endpoint that instant is the grant's
// expiry (ADR-045 §5: a session never outlives its authorization, and ADR-032's
// 4 h ceiling does not stack on top of it); elsewhere the column is unset and
// the package default applies.
func sessionBounds(row store.PortForwardSession) tunnel.Options {
	if !row.AuthorizedUntil.Valid {
		return tunnel.Options{}
	}
	remaining := time.Until(row.AuthorizedUntil.Time)
	if remaining <= 0 {
		// The grant lapsed between mint and attach; hand the bridge the
		// smallest positive budget rather than zero, which would read as
		// "unset" and restore the default ceiling.
		remaining = time.Millisecond
	}
	return tunnel.Options{MaxDuration: remaining}
}

// dialSessionServer opens the pooled SSH connection a tunnel dials through.
// Returns a user-facing message (not an error) on failure: it is written
// straight into the 409 the redeem answers with.
func (a *API) dialSessionServer(ctx context.Context, server store.Server) (*sshexec.Client, string) {
	key, err := a.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return nil, "the server's SSH key is not available"
	}
	pem, err := a.Keyring.Decrypt("private_keys", "private_key_enc", uuidString(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return nil, "the server's SSH key is not available"
	}
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, jobs.PinnedHostKey(server))
	if err != nil {
		return nil, "the server is not reachable over SSH right now"
	}
	return client, ""
}

// containerIP resolves a container's first-network IP via docker inspect.
func containerIP(ctx context.Context, client *sshexec.Client, container string) (string, error) {
	res, err := client.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' %s", container))
	if err != nil || res.ExitCode != 0 {
		return "", fmt.Errorf("docker inspect failed")
	}
	ip := strings.TrimSpace(strings.Fields(res.Stdout + " ")[0])
	if ip == "" {
		return "", fmt.Errorf("no IP for %s", container)
	}
	return ip, nil
}

func (a *API) endPortForwardSession(row store.PortForwardSession, reason tunnel.EndReason) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endReason := store.TerminalEndReason(reason)
	n, err := a.Store.EndPortForwardSession(ctx, store.EndPortForwardSessionParams{ID: row.ID, EndReason: &endReason})
	if err != nil {
		a.Logger.Warn("port-forward close failed", "session", uuidString(row.Uuid), "error", err)
	}
	if n > 0 {
		a.Audit.System(ctx, &row.TeamID, "port-forward.close", "port_forward_session", row.Uuid, store.AuditResultSuccess)
	}
}

// tunnelConn adapts coder/websocket to tunnel.Conn.
type tunnelConn struct{ c *websocket.Conn }

func (t tunnelConn) Read(ctx context.Context) (tunnel.MessageType, []byte, error) {
	typ, data, err := t.c.Read(ctx)
	if err != nil {
		switch websocket.CloseStatus(err) {
		case websocket.StatusNormalClosure, websocket.StatusGoingAway:
			return 0, nil, tunnel.ErrClientClosed
		}
		return 0, nil, err
	}
	if typ == websocket.MessageText {
		return tunnel.MessageText, data, nil
	}
	return tunnel.MessageBinary, data, nil
}

func (t tunnelConn) Write(ctx context.Context, typ tunnel.MessageType, data []byte) error {
	kind := websocket.MessageBinary
	if typ == tunnel.MessageText {
		kind = websocket.MessageText
	}
	return t.c.Write(ctx, kind, data)
}

func (t tunnelConn) Ping(ctx context.Context) error { return t.c.Ping(ctx) }
