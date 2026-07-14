package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/terminal"
)

// Web terminal (§5.7, §24.4, ADR-024). Two halves:
//
//   - the contract operations POST .../terminal-sessions mint a short-lived
//     single-use attach token (only its hash is stored, §23.2);
//   - GET /terminal/ws — outside the contract, like /auth — redeems that
//     token, upgrades to WebSocket and bridges to an SSH PTY on the server.
//
// The session is bounded (idle timeout, max duration), audited at open and
// close, and killed with the socket (§24.4). Keystrokes are never recorded.

const (
	// terminalTokenTTL is how long an attach token stays redeemable. It only
	// has to cover the client turning around to open the WebSocket.
	terminalTokenTTL = 60 * time.Second
	// terminalStepUpWindow is how fresh the passkey re-authentication must be
	// to open a server terminal (double control, rbac-matrix §5).
	terminalStepUpWindow = 5 * time.Minute
	// terminalTeamCap bounds concurrent sessions per team — the missing cap
	// called out by the threat model (§3.3 D), sized from the 50-session
	// instance target of §22.2.
	terminalTeamCap = 20
	// terminalWebsocketPath is where the attach token is redeemed.
	terminalWebsocketPath = "/terminal/ws"
)

// CreateApplicationTerminalSession implements
// POST /applications/{application_uuid}/terminal-sessions (permission: write).
func (a *API) CreateApplicationTerminalSession(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	a.createTerminalSession(w, r, id, terminalTargetSpec{
		kind:       store.TerminalTargetContainer,
		serverID:   row.ServerRowID,
		resourceID: &row.Resource.ID,
		name:       row.Resource.Name,
	})
}

// CreateDatabaseTerminalSession implements
// POST /databases/{database_uuid}/terminal-sessions (permission: write).
func (a *API) CreateDatabaseTerminalSession(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveDatabase(w, r, id, databaseUuid)
	if !ok {
		return
	}
	dest, err := a.Store.GetDestinationByID(r.Context(), row.Resource.DestinationID)
	if err != nil {
		a.internalError(w, r, "terminal session", err)
		return
	}
	a.createTerminalSession(w, r, id, terminalTargetSpec{
		kind:       store.TerminalTargetContainer,
		serverID:   dest.ServerID,
		resourceID: &row.Resource.ID,
		name:       row.Resource.Name,
	})
}

// CreateServerTerminalSession implements
// POST /servers/{server_uuid}/terminal-sessions (permission: write). A server
// shell runs as ssh_user — a root terminal in the sense of rbac-matrix §5 —
// so it takes the double control on top of the permission: a browser session
// must carry a recent passkey step-up, an API token must be root (a token
// cannot re-authenticate, and root is already the credential that can do
// everything else).
func (a *API) CreateServerTerminalSession(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	if id.Session {
		sess, err := a.Sessions.SessionFromRequest(r.Context(), r)
		if err != nil {
			httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "no active session")
			return
		}
		if !sess.MfaVerifiedAt.Valid || time.Since(sess.MfaVerifiedAt.Time) > terminalStepUpWindow {
			a.Audit.Record(r, id, audit.Event{
				Action: "terminal.open", TargetKind: "server", TargetUUID: server.Uuid,
				Result: store.AuditResultDenied,
			})
			httpapi.WriteError(w, r, http.StatusForbidden, "stepup_required",
				"a server terminal requires a recent passkey re-authentication (rbac-matrix §5)")
			return
		}
	} else if !id.IsRoot() {
		a.Audit.Record(r, id, audit.Event{
			Action: "terminal.open", TargetKind: "server", TargetUUID: server.Uuid,
			Result: store.AuditResultDenied,
		})
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden,
			"a server terminal requires the root permission for API tokens")
		return
	}
	a.createTerminalSession(w, r, id, terminalTargetSpec{
		kind:     store.TerminalTargetServer,
		serverID: server.ID,
		name:     server.Name,
	})
}

// terminalTargetSpec is what a create operation resolved: which server to
// SSH into and, for containers, which resource names the container.
type terminalTargetSpec struct {
	kind       store.TerminalTarget
	serverID   int64
	resourceID *int64
	name       string
}

// createTerminalSession is the shared tail: cap check, token mint, audit,
// contract response.
func (a *API) createTerminalSession(w http.ResponseWriter, r *http.Request, id *auth.Identity, target terminalTargetSpec) {
	open, err := a.Store.CountOpenTerminalSessions(r.Context(), id.TeamID)
	if err != nil {
		a.internalError(w, r, "terminal session", err)
		return
	}
	if open >= terminalTeamCap {
		httpapi.WriteError(w, r, http.StatusConflict, "terminal_session_limit",
			fmt.Sprintf("this team already has %d open terminal sessions", open))
		return
	}

	token, err := newTerminalToken()
	if err != nil {
		a.internalError(w, r, "terminal session", err)
		return
	}

	var userID *int64
	if id.Session && a.Sessions != nil {
		if sess, err := a.Sessions.SessionFromRequest(r.Context(), r); err == nil {
			userID = &sess.UserID
		}
	}

	row, err := a.Store.CreateTerminalSession(r.Context(), store.CreateTerminalSessionParams{
		TeamID:         id.TeamID,
		UserID:         userID,
		TargetKind:     target.kind,
		ServerID:       &target.serverID,
		ResourceID:     target.resourceID,
		TargetName:     target.name,
		ClientIp:       clientAddr(r),
		TokenHash:      hashTerminalToken(token),
		TokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(terminalTokenTTL), Valid: true},
	})
	if err != nil {
		a.internalError(w, r, "terminal session", err)
		return
	}

	// The open is audited here, where the actor is known; the close carries
	// the end reason on the session row (§23.4, §24.4).
	a.recordAudit(r, id, "terminal.open", "terminal_session", row.Uuid)

	httpapi.WriteJSON(w, http.StatusCreated, api.TerminalSession{
		Uuid:               uuidString(row.Uuid),
		TargetKind:         api.TerminalSessionTargetKind(target.kind),
		TargetName:         target.name,
		WebsocketPath:      terminalWebsocketPath,
		Token:              token,
		TokenExpiresAt:     row.TokenExpiresAt.Time,
		IdleTimeoutSeconds: int(a.terminalIdleTimeout().Seconds()),
		MaxDurationSeconds: int(a.terminalMaxDuration().Seconds()),
	})
}

func (a *API) terminalIdleTimeout() time.Duration {
	if a.TerminalIdleTimeout > 0 {
		return a.TerminalIdleTimeout
	}
	return terminal.DefaultIdleTimeout
}

func (a *API) terminalMaxDuration() time.Duration {
	if a.TerminalMaxDuration > 0 {
		return a.TerminalMaxDuration
	}
	return terminal.DefaultMaxDuration
}

// TerminalWebSocket implements GET /terminal/ws?token=… — outside the
// contract (§27.24: the WebSocket is not describable in OpenAPI 3.0), mounted
// next to /auth. The one-time token is the sole credential: it was minted by
// an authenticated, team-scoped operation seconds ago, and the SQL claim
// consumes it atomically, so a replay authenticates nothing (§24.4).
func (a *API) TerminalWebSocket(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "missing terminal token")
		return
	}
	row, err := a.Store.ClaimTerminalSession(r.Context(), hashTerminalToken(token))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "invalid, expired or already used terminal token")
		return
	}

	// Resolve the target BEFORE upgrading: an HTTP error is diagnosable, a
	// WebSocket that closes immediately is a mystery.
	client, command, errMsg := a.terminalConnect(r.Context(), row)
	if errMsg != "" {
		a.endTerminalSession(row, terminal.EndRevoked)
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, errMsg)
		return
	}
	defer func() { _ = client.Close() }()

	cols, rows := terminalGeometry(r)
	pty, err := client.StartPTY(command, cols, rows)
	if err != nil {
		a.endTerminalSession(row, terminal.EndRevoked)
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "could not start the remote terminal")
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		_ = pty.Close()
		a.endTerminalSession(row, terminal.EndRevoked)
		return
	}

	reason := terminal.Bridge(r.Context(), wsConn{conn}, pty, terminal.Options{
		IdleTimeout: a.terminalIdleTimeout(),
		MaxDuration: a.terminalMaxDuration(),
	})
	a.endTerminalSession(row, reason)
	_ = conn.Close(websocket.StatusNormalClosure, string(reason))
}

// terminalConnect dials the session's server and builds the remote command:
// none for a server shell, a docker exec for a container. The container is
// named by the resource's UUID (INV-011) — the same convention every job
// relies on.
func (a *API) terminalConnect(ctx context.Context, row store.TerminalSession) (*sshexec.Client, string, string) {
	if row.ServerID == nil {
		return nil, "", "the target server no longer exists"
	}
	server, err := a.Store.GetServerByID(ctx, *row.ServerID)
	if err != nil {
		return nil, "", "the target server no longer exists"
	}

	command := ""
	if row.TargetKind == store.TerminalTargetContainer {
		if row.ResourceID == nil {
			return nil, "", "the target resource no longer exists"
		}
		res, err := a.Store.GetResourceByID(ctx, *row.ResourceID)
		if err != nil {
			return nil, "", "the target resource no longer exists"
		}
		// bash when the image has it, sh otherwise; the fallback chain is a
		// fixed literal, and the container name is a UUID — nothing here is
		// user-controlled (INV-012).
		command = fmt.Sprintf(
			"docker exec -it -e TERM=xterm-256color %s sh -c 'command -v bash >/dev/null 2>&1 && exec bash || exec sh'",
			uuidString(res.Uuid))
	}

	key, err := a.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return nil, "", "the server's SSH key is not available"
	}
	pem, err := a.Keyring.Decrypt("private_keys", "private_key_enc", uuidString(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return nil, "", "the server's SSH key is not available"
	}
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, jobs.PinnedHostKey(server))
	if err != nil {
		return nil, "", "the server is not reachable over SSH right now"
	}
	return client, command, ""
}

// endTerminalSession closes the row and audits the close. It runs on a fresh
// context: the request context is usually already dead when a session ends.
func (a *API) endTerminalSession(row store.TerminalSession, reason terminal.EndReason) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endReason := store.TerminalEndReason(reason)
	n, err := a.Store.EndTerminalSession(ctx, store.EndTerminalSessionParams{
		ID: row.ID, EndReason: &endReason,
	})
	if err != nil {
		a.Logger.Warn("terminal session close failed", "session", uuidString(row.Uuid), "error", err)
	}
	if n > 0 {
		a.Audit.System(ctx, &row.TeamID, "terminal.close", "terminal_session", row.Uuid, store.AuditResultSuccess)
	}
}

// terminalGeometry reads the initial window size from the query string,
// bounded to something a real terminal could be.
func terminalGeometry(r *http.Request) (cols, rows int) {
	cols, rows = 80, 24
	if v, err := strconv.Atoi(r.URL.Query().Get("cols")); err == nil && v > 0 && v <= 1000 {
		cols = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("rows")); err == nil && v > 0 && v <= 1000 {
		rows = v
	}
	return cols, rows
}

// wsConn adapts coder/websocket to the bridge's Conn.
type wsConn struct{ c *websocket.Conn }

func (w wsConn) Read(ctx context.Context) (terminal.MessageType, []byte, error) {
	typ, data, err := w.c.Read(ctx)
	if err != nil {
		switch websocket.CloseStatus(err) {
		case websocket.StatusNormalClosure, websocket.StatusGoingAway:
			return 0, nil, terminal.ErrClientClosed
		}
		return 0, nil, err
	}
	if typ == websocket.MessageText {
		return terminal.MessageText, data, nil
	}
	return terminal.MessageBinary, data, nil
}

func (w wsConn) Write(ctx context.Context, typ terminal.MessageType, data []byte) error {
	kind := websocket.MessageBinary
	if typ == terminal.MessageText {
		kind = websocket.MessageText
	}
	return w.c.Write(ctx, kind, data)
}

func (w wsConn) Ping(ctx context.Context) error { return w.c.Ping(ctx) }

// newTerminalToken mints the single-use attach credential: 32 random bytes,
// hex — 256 bits, unguessable within any TTL.
func newTerminalToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "akdt_" + hex.EncodeToString(buf), nil
}

// hashTerminalToken is what gets stored — never the token itself (§23.2).
func hashTerminalToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// clientAddr extracts the peer address for the session record. RemoteAddr,
// never X-Forwarded-For — same reasoning as the auth rate limiter.
func clientAddr(r *http.Request) *netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	return &addr
}
