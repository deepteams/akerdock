// CLI TCP port-forward (ADR-032): a two-step mint/redeem like the terminal.
// POST .../port-forwards mints a single-use attach token bound to a fixed
// container:port; the attach path redeems it and multiplexes TCP streams to
// that target over one WebSocket, dialed server-side over SSH.
package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/adoption"
	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/serverdial"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// portForwardTeamCap bounds concurrent tunnel sessions per team (§ADR-032).
const portForwardTeamCap = 10

// portForwardTokenTTL is how long an attach token stays redeemable.
const portForwardTokenTTL = 60 * time.Second

// tunnelAttachPath is where an attach token is redeemed. It stopped naming a
// protocol when ADR-064 put three of them on it: OPTIONS probes, POST attaches
// over HTTP/2 or HTTP/3, GET upgrades to the WebSocket that remains the
// ladder's bottom rung. The mint response hands this value to the CLI, so the
// rename needs no flag day — and tunnelLegacyWebsocketPath keeps answering for
// tokens minted by an older release and clients that pinned the old spelling.
const (
	tunnelAttachPath          = "/tunnel/attach"
	tunnelLegacyWebsocketPath = "/tunnel/ws"
)

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
	// application marks a resourceID that names an APPLICATION row, the only
	// resource kind with a scale-to-zero clock to stamp (ADR-037). The
	// database mint leaves it false rather than have the mint re-read the
	// resource it just resolved to learn its own kind.
	application bool
}

// applicationID is the resource whose application row carries the scale-to-zero
// clock, or nil when the target has none — a database, a Compose stack.
func (s portForwardSpec) applicationID() *int64 {
	if !s.application {
		return nil
	}
	return s.resourceID
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
	spec := portForwardSpec{
		serverID: row.ServerRowID, resourceID: &row.Resource.ID, name: row.Resource.Name,
		port: body.Port, application: true,
	}
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
		application: true,
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
	// Stamp the target's activity clock HERE, not at the first heartbeat
	// twenty seconds from now (ADR-067 §1). A tunnel is typically opened
	// precisely because nothing has touched the resource in a while, so the
	// sleep decision it races is the one at 29:50 of a 30-minute window: lose
	// that race and the scheduler stops the containers between the mint and the
	// attach, which then fails with "the target container is not running" — or
	// the session dies one beat later with target_stopped. It self-corrects on
	// a retry, and it still makes the tunnel the one door that appears to
	// break, which is the complaint this whole change answers.
	a.recordTunnelActivity(r.Context(), spec.previewID, spec.applicationID(), uuidString(row.Uuid))
	httpapi.WriteJSON(w, http.StatusCreated, api.PortForwardSession{
		Uuid:           uuidString(row.Uuid),
		Port:           spec.port,
		WebsocketPath:  tunnelAttachPath,
		Token:          token,
		TokenExpiresAt: row.TokenExpiresAt.Time,
	})
}

// TunnelWebSocket implements GET on the attach path (?token=…) — outside the contract
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
	// Registered BEFORE the heartbeat is installed: the beat cuts the session
	// through this same channel when the target is gone, and a beat that fired
	// against an unregistered bridge would have nowhere to report it.
	bounds.Cancel = a.Tunnels.register(row.ID)
	bounds.OnHeartbeat = a.portForwardHeartbeat(row)
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

	ref, _, msg := a.resolveTunnelTargetRef(ctx, row)
	if msg != "" {
		return nil, "", msg
	}

	// The container's IP on its Docker network — reachable host→container even
	// without a published port. First network wins (INV-011 naming); read
	// through the agent channel (ADR-052) before paying the SSH dial.
	ip, err := a.containerIP(ctx, server.ID, ref.container)
	if err != nil {
		if dockerruntime.IsUnavailable(err) {
			return nil, "", "the server's agent is not connected right now"
		}
		return nil, "", "the target container is not running"
	}
	client, msg := a.dialSessionServer(ctx, server)
	if msg != "" {
		return nil, "", msg
	}
	return client, fmt.Sprintf("%s:%d", ip, row.TargetPort), ""
}

// tunnelTargetRef is what a resource-backed session points at: the resource
// row and the container name that carries it. The attach path and the
// per-heartbeat liveness probe both go through it so the two can never
// disagree about WHICH container a session is about (INV-011 naming) — a
// divergence there would either dial one container and watch another, or
// declare a healthy tunnel dead.
type tunnelTargetRef struct {
	resource  store.Resource
	container string
}

// resolveTunnelTargetRef resolves a session's target container.
//
// The `gone` return is the whole point of the shape: a failure to resolve is
// not the same fact as an absence. A destroyed preview or a deleted row means
// the container is DEFINITELY not there; a database that timed out means
// nothing at all. The attach path answers a 409 either way and only reads the
// message; the heartbeat cuts a live tunnel on it, and must only ever do so on
// a definite absence.
func (a *API) resolveTunnelTargetRef(ctx context.Context, row store.PortForwardSession) (tunnelTargetRef, bool, string) {
	if row.ResourceID == nil {
		return tunnelTargetRef{}, true, "the target no longer exists"
	}
	res, err := a.Store.GetResourceByID(ctx, *row.ResourceID)
	if err != nil {
		return tunnelTargetRef{}, errors.Is(err, pgx.ErrNoRows), "the target resource no longer exists"
	}
	// A preview instance names its containers after the PREVIEW uuid, not the
	// resource's (INV-011); a destroyed preview has nothing to dial.
	base := uuidString(res.Uuid)
	name := adoption.ContainerName(res.Adoption, base)
	if row.PreviewID != nil {
		preview, err := a.Store.GetPreviewByID(ctx, *row.PreviewID)
		if err != nil {
			return tunnelTargetRef{}, errors.Is(err, pgx.ErrNoRows),
				"the preview no longer exists — it may have been destroyed"
		}
		if preview.Status == store.PreviewStatusDestroyed {
			return tunnelTargetRef{}, true, "the preview no longer exists — it may have been destroyed"
		}
		base = uuidString(preview.Uuid)
		name = base
	}
	if row.TargetComponent != nil && *row.TargetComponent != "" {
		name = base + "-" + *row.TargetComponent
	}
	return tunnelTargetRef{resource: res, container: name}, false, ""
}

// endReasonTargetStopped is what a tunnel whose target vanished under it
// reports. It is not a disconnect: nothing about the client's connection
// failed, the container it pointed at stopped existing (a redeploy, a manual
// stop, a crash, a scale-to-zero sleep decided just before the first beat).
//
// The distinction is what makes the failure visible at all. A forwarded TCP
// connection whose container's netns has been destroyed gets no RST and no FIN
// — psql sits on "sending keepalive" until the tunnel's own 30-minute idle
// timer fires, and only if nothing keeps the tunnel busy. Ending the session
// with a reason the CLI can print turns that silence into an error within one
// beat.
const endReasonTargetStopped tunnel.EndReason = "target_stopped"

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

// dialSessionServer opens the SSH connection a tunnel dials through. NOT a
// pooled one, whatever ADR-032's text and this comment used to claim:
// serverdial.Open performs a full handshake per attach, which is why the attach
// spends seconds before it can answer anything at all (ADR-066 records the
// discrepancy; pooling is its own decision, not made here).
//
// Returns a user-facing message (not an error) on failure: it is written
// straight into the 409 the redeem answers with.
func (a *API) dialSessionServer(ctx context.Context, server store.Server) (*sshexec.Client, string) {
	client, err := serverdial.Open(ctx, a.Store, a.Keyring, server)
	if err != nil {
		return nil, "the server is not reachable over SSH right now"
	}
	return client, ""
}

// containerIP resolves a container's first-network IP through the server's
// agent channel.
func (a *API) containerIP(ctx context.Context, serverID int64, container string) (string, error) {
	rt, err := a.AgentRPC.Runtime(ctx, serverID)
	if err != nil {
		return "", err
	}
	resp, err := rt.ContainerInspect(ctx, container)
	if err != nil {
		return "", err
	}
	if resp.NetworkSettings != nil {
		for _, n := range resp.NetworkSettings.Networks {
			if n != nil && n.IPAddress != "" {
				return n.IPAddress, nil
			}
		}
	}
	return "", fmt.Errorf("no IP for %s", container)
}

// portForwardBeatBudget bounds each step of a heartbeat. A beat is
// bookkeeping: it must never be the thing that stalls a tunnel, so the two
// steps that can touch the network get one bounded budget each — worst case
// well inside the 20-second cadence they run at.
const portForwardBeatBudget = 3 * time.Second

// portForwardHeartbeat is the beat BOTH transports run (ADR-064 put the HTTP
// session and the WebSocket bridge on the same bounds, and they share this
// closure so a session cannot behave differently for having landed on one
// rather than the other). Three duties: persist liveness, tell scale-to-zero
// somebody is connected, and notice a target that vanished.
//
// The socket remains the source of truth while this process is alive.
// Persistence is only the crash/restart net, so a transient database failure is
// logged and retried on the next beat; returning false is reserved for a
// session that is durably over.
func (a *API) portForwardHeartbeat(row store.PortForwardSession) func(context.Context) bool {
	return func(parent context.Context) bool {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), portForwardBeatBudget)
		defer cancel()
		n, err := a.Store.HeartbeatPortForwardSession(ctx, row.ID)
		if err != nil {
			a.Logger.Warn("port-forward heartbeat failed", "session", uuidString(row.Uuid), "error", err)
			return true
		}
		// Zero means another replica or the scheduler has finalized the row.
		// Do not leave the actual socket alive after its durable authorization
		// is gone.
		if n == 0 {
			return false
		}
		a.watchPortForwardTarget(parent, row)
		return true
	}
}

// watchPortForwardTarget is what a beat owes the target it is holding open:
// record that somebody is connected to it, and end the session when it is
// definitely gone.
//
// The consequence a reviewer will ask about, stated plainly: an attached but
// SILENT tunnel now keeps a scale-to-zero resource awake. That is the intended
// reading of "someone is connected" — a developer with a psql session open and
// idle is still working — and it is bounded by the tunnel's own limits rather
// than the resource's window: the 30-minute idle timeout and the 4-hour
// ceiling (§24.4) both end the session, and the resource sleeps one window
// later.
func (a *API) watchPortForwardTarget(parent context.Context, row store.PortForwardSession) {
	// An external endpoint (ADR-045) has no container: its address was frozen
	// at declaration and the far side is not ours to inspect — nor does it have
	// a scale-to-zero clock to reset.
	if row.ExternalEndpointID != nil || row.ServerID == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), portForwardBeatBudget)
	defer cancel()

	ref, gone, msg := a.resolveTunnelTargetRef(ctx, row)
	if msg != "" {
		if gone {
			a.cutOnStoppedTarget(row, msg)
		}
		return
	}
	a.recordPortForwardActivity(ctx, row, ref)
	if a.targetContainerStopped(ctx, *row.ServerID, ref.container) {
		a.cutOnStoppedTarget(row, "the target container is no longer running")
	}
}

// cutOnStoppedTarget ends the session through the same channel a revocation
// uses, so the bridge returns endReasonTargetStopped and the durable row, the
// audit entry and the message the developer reads all come from that one
// value. The row itself is finalized by the transport as it leaves — cutting
// here and ending there is exactly what ClosePortForwardSession does.
func (a *API) cutOnStoppedTarget(row store.PortForwardSession, why string) {
	if a.Tunnels.Cut(row.ID, endReasonTargetStopped) {
		a.Logger.Info("port-forward target vanished, session cut",
			"session", uuidString(row.Uuid), "target", row.TargetName, "reason", why)
	}
}

// recordTunnelActivity feeds scale-to-zero the one signal it structurally
// cannot see. Its only source is the waker's per-resource activity file, which
// moves when the waker serves a PROXIED HTTP request (ADR-036/037); a tunnel
// goes control plane → SSH → container IP and never crosses the proxy, so a
// tunnelled session reads as perfect idleness — and the scheduler stops the
// very container the developer is connected to.
//
// A preview instance carries its own row and wins over the application it
// belongs to; both nil is a target with no clock at all — a database, a
// Compose stack, a declared external endpoint — and writes nothing rather than
// inventing a signal for it.
//
// A failure is logged and dropped, at the mint as at every beat: this is a
// timestamp, and no tunnel should ever fail to open, or be dropped, because one
// did not land.
func (a *API) recordTunnelActivity(ctx context.Context, previewID, applicationID *int64, session string) {
	var err error
	switch {
	case previewID != nil:
		err = a.Store.RecordPreviewActivity(ctx, *previewID)
	case applicationID != nil:
		err = a.Store.RecordApplicationActivity(ctx, *applicationID)
	default:
		return
	}
	if err != nil {
		a.Logger.Warn("port-forward activity not recorded", "session", session, "error", err)
	}
}

// recordPortForwardActivity is the beat's call into the above, with the target
// kind read off the resource the beat just resolved anyway.
func (a *API) recordPortForwardActivity(ctx context.Context, row store.PortForwardSession, ref tunnelTargetRef) {
	var applicationID *int64
	if ref.resource.ResourceType == store.ResourceTypeApplication {
		applicationID = &ref.resource.ID
	}
	a.recordTunnelActivity(ctx, row.PreviewID, applicationID, uuidString(row.Uuid))
}

// targetContainerStopped reports a container the agent DEFINITELY says is not
// running. Everything else answers false: an agent channel that is momentarily
// unavailable (a helper restart, a relay reconnect) says nothing about the
// container, and reading that silence as absence would tear down healthy
// tunnels every time an agent blinks — trading a rare hang for a routine one.
func (a *API) targetContainerStopped(ctx context.Context, serverID int64, container string) bool {
	rt, err := a.AgentRPC.Runtime(ctx, serverID)
	if err != nil {
		return false
	}
	resp, err := rt.ContainerInspect(ctx, container)
	if err != nil {
		// A removed container is a definite answer; unavailable — and anything
		// else the channel reports — is not.
		return dockerruntime.IsNotFound(err)
	}
	// A nil State is an answer we cannot read: only an explicit "not running"
	// counts.
	return resp.State != nil && !resp.State.Running
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
