package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/adoption"
	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/serverdial"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/terminal"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// Web terminal (§5.7, §24.4, ADR-024). Two halves:
//
//   - the contract operations POST .../terminal-sessions mint a short-lived
//     single-use attach token (only its hash is stored, §23.2);
//   - the attach path — outside the contract, like /auth — redeems that
//     token and bridges to a PTY on the server. GET upgrades to WebSocket,
//     which is the ladder's bottom rung; POST attaches over HTTP/2 or HTTP/3
//     (ADR-064 §3, terminalattach.go).
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
	// terminalAttachPath is where the attach token is redeemed. It stopped
	// naming a protocol when ADR-064 put three of them on it: OPTIONS probes,
	// POST attaches over HTTP/2 or HTTP/3, GET upgrades to the WebSocket that
	// remains the ladder's bottom rung. The mint response hands this value to
	// its client, so the rename needs no flag day — and
	// terminalLegacyWebsocketPath keeps answering for tokens minted by an
	// older release and clients that pinned the old spelling.
	terminalAttachPath          = "/terminal/attach"
	terminalLegacyWebsocketPath = "/terminal/ws"
)

// CreateApplicationTerminalSession implements
// POST /applications/{application_uuid}/terminal-sessions (permission: write).
func (a *API) CreateApplicationTerminalSession(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.CreateApplicationTerminalSessionParams) {
	id, ok := a.require(w, r, auth.PermTerminalOpen)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	spec := terminalTargetSpec{
		kind:       store.TerminalTargetContainer,
		serverID:   row.ServerRowID,
		resourceID: &row.Resource.ID,
		name:       row.Resource.Name,
		wake:       applicationWakeSpec(row),
	}
	// A compose stack has no container of its own (compose-spec §2.2): the
	// shell opens in ONE service's container, validated here — never a
	// guessed name at connect time.
	if params.Component != nil && *params.Component != "" {
		components, err := a.Store.ListServiceComponents(r.Context(), row.Resource.ID)
		if err != nil {
			a.internalError(w, r, "terminal session", err)
			return
		}
		found := false
		for _, c := range components {
			if c.Name == *params.Component {
				found = true
				break
			}
		}
		if !found {
			httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound,
				fmt.Sprintf("unknown component %q — see GET /applications/{uuid}/components", *params.Component))
			return
		}
		spec.component = *params.Component
		spec.name = row.Resource.Name + " · " + *params.Component
	}
	a.createTerminalSession(w, r, id, spec)
}

// CreateDatabaseTerminalSession implements
// POST /databases/{database_uuid}/terminal-sessions (permission: write).
func (a *API) CreateDatabaseTerminalSession(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid) {
	id, ok := a.require(w, r, auth.PermTerminalOpen)
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
	id, ok := a.require(w, r, auth.PermTerminalRoot)
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
	// component names the compose service whose container the shell opens in
	// — empty for single-container resources (compose-spec §2.2).
	component string
	// previewID targets a PREVIEW instance: its containers derive from the
	// preview uuid, not the resource's (INV-011).
	previewID *int64
	// wake is what the mint resolved about the target's scale-to-zero state
	// (ADR-067 §8) — which clock to stamp, and whether this shell may and must
	// ask for a wake before it answers. A server shell and a database leave it
	// zero, which is "neither": a server shell has no container, no resource and
	// no clock, and ADR-037 §2 excludes databases by construction.
	wake sessionWakeSpec
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

	// ADR-067 §3: a shell into a sleeping target wakes it, and the decision
	// belongs to the mint — the only step carrying the caller's identity. Before
	// the row is created, because every refusal it can produce must leave none:
	// §7's 403 on an application, and an agent that cannot wake at all.
	wake, ok := a.wakeForSession(w, r, id, target.wake, terminalFamily)
	if !ok {
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
		TeamID:     id.TeamID,
		UserID:     userID,
		TargetKind: target.kind,
		ServerID:   &target.serverID,
		ResourceID: target.resourceID,
		TargetName: target.name,
		TargetComponent: func() *string {
			if target.component == "" {
				return nil
			}
			return &target.component
		}(),
		PreviewID:      target.previewID,
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
	// The wake becomes findable by the attach that follows (ADR-067 §5); the
	// session uuid only reaches a client through the response below, so nothing
	// can have raced this.
	a.rememberWake(uuidString(row.Uuid), wake)
	// And the target's clock is stamped here rather than at the first beat
	// twenty seconds from now (ADR-067 §1): a shell is typically opened
	// precisely because nothing has touched the resource in a while, so the
	// sleep decision it races is the one at 29:50 of a 30-minute window. On the
	// wake path this is not an optimisation but a necessity — the resource has
	// just been started and would otherwise be a candidate for the very next
	// pass. A server shell and a database stamp nothing, asserted as the
	// absence of a write rather than as no error.
	a.stampSessionActivity(r.Context(), target.wake, uuidString(row.Uuid))

	httpapi.WriteJSON(w, http.StatusCreated, api.TerminalSession{
		Uuid:               uuidString(row.Uuid),
		TargetKind:         api.TerminalSessionTargetKind(target.kind),
		TargetName:         target.name,
		WebsocketPath:      terminalAttachPath,
		Token:              token,
		TokenExpiresAt:     row.TokenExpiresAt.Time,
		IdleTimeoutSeconds: int(a.terminalIdleTimeout().Seconds()),
		MaxDurationSeconds: int(a.terminalMaxDuration().Seconds()),
		// ADR-067 §6: the client prints the cold-start notice from THIS, before
		// the terminal window appears — a blank window for up to 75 s reads as a
		// hung client, and the control frame only arrives once the session is
		// already open.
		State: wakeMintState(wake, api.Waking),
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
	// The bottom rung carries the attach key too (ADR-065 §7), in the same
	// header every other rung uses rather than the query string: a WebSocket
	// attach arriving after an HTTP rung burnt the token is exactly the failure
	// the idempotent claim exists to fix, so leaving this rung out would fix the
	// ladder everywhere except where it lands.
	claimKey, err := terminalClaimKey(r)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid attach key")
		return
	}
	row, err := a.Store.ClaimTerminalSession(r.Context(), store.ClaimTerminalSessionParams{
		TokenHash: hashTerminalToken(token), AttachKeyHash: claimKey[:],
	})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "invalid, expired or already used terminal token")
		return
	}
	sessionUUID := uuidString(row.Uuid)
	// The session's own context: a cut cancels it, and cancelling it is what
	// stops the resolution below mid-handshake.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// A re-claim that lands here won the session, so whatever attach still holds
	// it must be cut rather than left running beside a second PTY (ADR-065 §5).
	// REGISTERING, rather than merely cutting, is ADR-067 §2's "every rung
	// enforces it": until now this rung put nothing in the register, so a
	// WebSocket-attached shell was reachable by no cut at all — not the beat's,
	// when its container vanished, and not a re-claim arriving on an HTTP rung
	// either. It holds no data stream, but it holds a session.
	attach := newTerminalWebSocketAttach(claimKey, cancel)
	a.terminalRegister(sessionUUID, attach)
	defer a.terminalRelease(sessionUUID, attach)
	defer attach.finish()

	// Resolve the target BEFORE upgrading: an HTTP error is diagnosable, a
	// WebSocket that closes immediately is a mystery. This rung keeps that
	// choreography deliberately (ADR-066 §5) — the defect the answer-first
	// ordering removes is manufactured by a bounded open, and websocket.Dial
	// has none, so the server may take its whole handshake and still be read.
	cols, rows := terminalGeometry(r)
	// This rung pays the whole cold start in front of its handshake, which is
	// where it belongs here: websocket.Dial carries no open budget to lose
	// (ADR-066 §5), and a wake that fails is then a 409 whose body the client
	// prints verbatim — a better report than a frame on a socket never upgraded.
	wake := a.lookupWake(sessionUUID)
	target, errMsg := a.terminalResolveTarget(ctx, row, cols, rows)
	var pty terminal.PTY
	var cleanup func()
	if errMsg == "" {
		pty, cleanup, errMsg = a.terminalOpenPTYAfterWake(ctx, wake, target)
	}
	if errMsg != "" {
		a.endTerminalSession(row, terminalSessionEndReason(wake))
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, errMsg)
		return
	}
	defer cleanup()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		_ = pty.Close()
		a.endTerminalSession(row, terminal.EndRevoked)
		return
	}
	// The PTY is attached: the row now records that it carried one, which is
	// what keeps the sweep from closing it at token expiry (ADR-065 §6).
	a.markTerminalSessionStreamed(row)

	// From here a cut is delivered as a word rather than as a cancellation:
	// coder/websocket reads a cancelled Read as a hard close, and the end frame
	// naming the reason would never reach the developer.
	attach.bridging.Store(true)
	reason := terminal.Bridge(ctx, wsConn{conn}, pty, terminal.Options{
		IdleTimeout: a.terminalIdleTimeout(),
		MaxDuration: a.terminalMaxDuration(),
		// The same two duties the HTTP rung's bridge carries, from the same
		// closure: ADR-064 put both rungs on one set of bounds, and a shell that
		// kept its target awake on one rung but not the other would be the
		// asymmetry that decision exists to forbid.
		OnHeartbeat: a.terminalHeartbeat(row),
		Cancel:      attach.cut,
	})
	a.endTerminalSession(row, reason)
	_ = conn.Close(websocket.StatusNormalClosure, string(reason))
}

// terminalClaimKey is the attacher identity the claim binds the row to
// (ADR-065 §3): the hash of the ephemeral per-mint attach key the CLI presents
// on every rung.
//
// An attach that presents no key — an N-1 CLI mid-rollout, or the dashboard's
// browser terminal, which is not on the ladder and has no retry to rescue —
// gets server-generated random bytes instead of a NULL or a fixed sentinel. No
// presentable key hashes to them, so such a session stays strictly single-use
// exactly as before. The column must never hold a value that matches anything:
// that would turn a compatibility shim into the replay hole the rule exists to
// keep shut.
// It is an array rather than a slice because the register this rung now joins
// compares keys in constant time on a fixed width (ADR-067 §2): a length that
// is 32 bytes only by convention is one convention too many between a claim and
// a lookup.
func terminalClaimKey(r *http.Request) ([sha256.Size]byte, error) {
	presented := r.Header.Get(tunnel.TerminalHTTP.AttachKeyHeader)
	if presented == "" {
		var unmatchable [sha256.Size]byte
		if _, err := rand.Read(unmatchable[:]); err != nil {
			return unmatchable, err
		}
		return unmatchable, nil
	}
	return decodeAttachKey(presented)
}

// terminalTarget is what the LOCAL half of an attach resolved (ADR-066 §1):
// the identity of the shell, in facts this control plane holds itself. Every
// field here came from an indexed read on a pooled connection, which is why
// the refusals that produce it stay in front of the response head while
// everything they feed moves behind it.
type terminalTarget struct {
	server store.Server
	// container is empty for a server shell. For a container shell it is the
	// whole of what the exec needs — resource, preview and component already
	// folded into one name by terminalContainer.
	container  string
	cols, rows int
}

// terminalResolveTarget is the local half: which server, and for a container
// shell which container. Its refusals are the actionable ones — a deleted
// server, a deleted resource, a destroyed preview — and they keep their 409 and
// their prose on every rung.
func (a *API) terminalResolveTarget(ctx context.Context, row store.TerminalSession, cols, rows int) (terminalTarget, string) {
	if row.ServerID == nil {
		return terminalTarget{}, "the target server no longer exists"
	}
	server, err := a.Store.GetServerByID(ctx, *row.ServerID)
	if err != nil {
		return terminalTarget{}, "the target server no longer exists"
	}
	target := terminalTarget{server: server, cols: cols, rows: rows}
	if row.TargetKind == store.TerminalTargetContainer {
		ref, _, errMsg := a.resolveTerminalTargetRef(ctx, row)
		if errMsg != "" {
			return terminalTarget{}, errMsg
		}
		target.container = ref.container
	}
	return target, ""
}

// terminalOpenPTY is the remote half: everything that crosses the network to a
// machine this control plane does not own. A container terminal is an exec
// attach on the agent channel (ADR-052) and opens no SSH connection at all —
// its cleanup is empty because there is no transport to close, the execPTY
// owning the hijacked stream. A server shell keeps its SSH PTY — the one
// terminal SSH keeps forever — dialled here rather than pooled anywhere
// (ADR-066 §7: serverdial.Open is a full handshake, every time), so its cleanup
// closes the client the PTY rides on.
//
// The returned message is an operator sentence, not an error: it is what the
// developer reads, either as a 409 on the WebSocket rung or as the end frame of
// a session that had already opened.
func (a *API) terminalOpenPTY(ctx context.Context, target terminalTarget) (terminal.PTY, func(), string) {
	if target.container != "" {
		pty, errMsg := a.execTerminal(ctx, target.server.ID, target.container, target.cols, target.rows)
		if errMsg != "" {
			return nil, nil, errMsg
		}
		return pty, func() {}, ""
	}

	client, err := serverdial.Open(ctx, a.Store, a.Keyring, target.server)
	if err != nil {
		return nil, nil, "the server is not reachable over SSH right now"
	}
	pty, err := client.StartPTY("", target.cols, target.rows)
	if err != nil {
		_ = client.Close()
		return nil, nil, "could not start the remote terminal"
	}
	return pty, func() { _ = client.Close() }, ""
}

// terminalOpenPTYAfterWake is the remote half with ADR-067's two gates in front
// of it — the terminal's twin of resolveTunnelTargetAfterWake, and the path
// every rung resolves through.
//
// Gate 1 is the wake's own answer: the exec would be refused outright while the
// container is down, so it is not attempted at all until the wake returns ready.
// Gate 2 is the exec attach itself, retried — the exact operation the session
// exists to perform, not a probe of it. It is lighter here than for a tunnel and
// deliberately so: a container that runs has a shell, so the retry only covers
// the moment just after start when the daemon may still refuse an exec.
func (a *API) terminalOpenPTYAfterWake(ctx context.Context, wake *sessionWake, target terminalTarget) (terminal.PTY, func(), string) {
	if msg, ok := wake.await(ctx); !ok {
		return nil, nil, msg
	}
	if wake == nil {
		return a.terminalOpenPTY(ctx, target)
	}
	var pty terminal.PTY
	var cleanup func()
	if msg := retryUntilReady(ctx, func(ctx context.Context) string {
		opened, release, errMsg := a.terminalOpenPTY(ctx, target)
		if errMsg != "" {
			return errMsg
		}
		pty, cleanup = opened, release
		return ""
	}); msg != "" {
		wake.gateFailed.Store(true)
		return nil, nil, msg
	}
	return pty, cleanup, ""
}

// terminalTargetRef is what a container session points at: the resource row
// that owns the shell's container, and the container's name. The attach path
// and the per-beat liveness probe both go through it so the two can never
// disagree about WHICH container a session is about (INV-011 naming) — a
// divergence there would either exec into one container and watch another, or
// declare a healthy shell dead.
//
// It is the terminal's own type rather than the tunnel's tunnelTargetRef, for
// ADR-064 §1's reason applied one level in: the two families resolve the same
// fact from different session rows, and pooling the vocabulary would put a
// port-forward's column names in a terminal's call site for the sake of two
// fields.
type terminalTargetRef struct {
	resource  store.Resource
	container string
}

// resolveTerminalTargetRef names the session's container: the resource's UUID
// (INV-011) — or, for an adopted resource awaiting normalization (§20.7),
// the original Docker name our own adopt job recorded.
//
// The `gone` return is the whole point of the shape, and it is what ADR-067 §2
// rests on: a failure to resolve is not the same fact as an absence. A
// destroyed preview or a deleted row means the container is DEFINITELY not
// there; a database that timed out means nothing at all. The attach path
// answers a 409 either way and only reads the message; the beat cuts a live
// shell on it, and must only ever do so on a definite absence.
func (a *API) resolveTerminalTargetRef(ctx context.Context, row store.TerminalSession) (terminalTargetRef, bool, string) {
	if row.ResourceID == nil {
		return terminalTargetRef{}, true, "the target resource no longer exists"
	}
	res, err := a.Store.GetResourceByID(ctx, *row.ResourceID)
	if err != nil {
		return terminalTargetRef{}, errors.Is(err, pgx.ErrNoRows), "the target resource no longer exists"
	}
	containerName := adoption.ContainerName(res.Adoption, uuidString(res.Uuid))
	base := uuidString(res.Uuid)
	if row.PreviewID != nil {
		// A preview instance: every container derives from the PREVIEW uuid
		// (INV-011) — and a destroyed preview has no container left to exec
		// into, say so instead of a raw daemon error.
		preview, err := a.Store.GetPreviewByID(ctx, *row.PreviewID)
		if err != nil {
			return terminalTargetRef{}, errors.Is(err, pgx.ErrNoRows),
				"the preview no longer exists — it may have been destroyed"
		}
		if preview.Status == store.PreviewStatusDestroyed {
			return terminalTargetRef{}, true, "the preview no longer exists — it may have been destroyed"
		}
		base = uuidString(preview.Uuid)
		containerName = base
	}
	if row.TargetComponent != nil && *row.TargetComponent != "" {
		// A compose service's container (compose-spec §2.2). The component
		// was validated against service_components at session creation.
		containerName = base + "-" + *row.TargetComponent
	}
	return terminalTargetRef{resource: res, container: containerName}, false, ""
}

// execTerminal opens the container terminal through the agent channel: a TTY
// exec — bash when the image has it, sh otherwise, the same fixed fallback
// chain as always — attached bidirectionally, resized by typed command.
func (a *API) execTerminal(ctx context.Context, serverID int64, containerName string, cols, rows int) (terminal.PTY, string) {
	rt, err := a.AgentRPC.Runtime(ctx, serverID)
	if err != nil {
		return nil, "the server's agent is not connected — it reconnects on its own; check the server page if this persists"
	}
	created, err := rt.ContainerExecCreate(ctx, containerName, containertypes.ExecOptions{
		Tty: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
		Env: []string{"TERM=xterm-256color"},
		Cmd: []string{"sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"},
	})
	if err != nil {
		if dockerruntime.IsNotFound(err) {
			return nil, "the container does not exist on the server — deploy it first"
		}
		if dockerruntime.IsConflict(err) {
			return nil, "the container is not running — start it first"
		}
		return nil, "could not start the remote terminal"
	}
	hijack, err := rt.ContainerExecAttach(ctx, created.ID, containertypes.ExecAttachOptions{
		Tty: true, ConsoleSize: &[2]uint{uint(rows), uint(cols)},
	})
	if err != nil {
		return nil, "could not start the remote terminal"
	}
	return &execPTY{hijack: hijack, rt: rt, execID: created.ID}, ""
}

// execPTY adapts a channel exec attach to the terminal bridge: the hijacked
// stream carries the TTY bytes, resize travels as the typed command.
type execPTY struct {
	hijack types.HijackedResponse
	rt     dockerruntime.Runtime
	execID string
}

func (p *execPTY) Read(b []byte) (int, error)  { return p.hijack.Reader.Read(b) }
func (p *execPTY) Write(b []byte) (int, error) { return p.hijack.Conn.Write(b) }

func (p *execPTY) Close() error {
	p.hijack.Close()
	return nil
}

func (p *execPTY) Resize(cols, rows int) error {
	// The bridge calls this from the client's resize messages; the session
	// ctx may already be gone at teardown, so the command gets its own bound.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.rt.ContainerExecResize(ctx, p.execID, containertypes.ResizeOptions{
		Width: uint(cols), Height: uint(rows),
	})
}

// endReasonTargetStopped is what a shell whose container vanished under it
// reports (ADR-067 §2). It is not a disconnect and it is emphatically not a
// revocation: nothing about the developer's connection failed and nobody
// revoked anything — the container they were typing in stopped existing, in a
// redeploy, an operator's stop, a crash, or a scale-to-zero sleep decided just
// before the first beat.
//
// The gain here is narrower than the tunnel's and should not be oversold: the
// daemon closes a hijacked exec when its container dies, so the stream does end
// on its own. What it does not do is say WHY, and a shell that ends as
// `disconnect` reads as a network glitch the developer will go and inspect
// their own laptop for.
//
// The value is a member of terminal_end_reason since 00094 — one enum shared by
// both session tables — so this costs a constant and no migration.
const terminalEndReasonTargetStopped terminal.EndReason = "target_stopped"

// terminalEndReasonSuperseded is the reason a displaced attach is cut with when
// a re-claim takes its session (ADR-065 §5). Like the tunnel's twin it is
// WIRE-ONLY and never persisted: it is a fact about a socket, not about the
// session, and the session it names is still open — for the attach that won.
const terminalEndReasonSuperseded terminal.EndReason = "superseded"

// terminalBeatBudget bounds each step of a beat. A beat is bookkeeping: it must
// never be the thing that stalls a shell, so the two steps that can touch the
// network get one bounded budget each — worst case well inside the 20-second
// cadence they run at.
const terminalBeatBudget = 3 * time.Second

// terminalHeartbeat is the beat BOTH rungs run — the WebSocket bridge and the
// HTTP session share this closure so a session cannot behave differently for
// having landed on one rather than the other (ADR-064 §2). Three duties, the
// same three the tunnel's beat carries: persist liveness, tell scale-to-zero
// somebody is connected, and notice a target that vanished.
//
// The socket remains the source of truth while this process is alive.
// Persistence is only the crash/restart net, so a transient database failure is
// logged and retried on the next beat; a non-empty reason is reserved for a
// session that is durably over.
func (a *API) terminalHeartbeat(row store.TerminalSession) func(context.Context) terminal.EndReason {
	return func(parent context.Context) terminal.EndReason {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), terminalBeatBudget)
		defer cancel()
		n, err := a.Store.HeartbeatTerminalSession(ctx, store.HeartbeatTerminalSessionParams{
			ID: row.ID, AttachSeq: row.AttachSeq,
		})
		if err != nil {
			a.Logger.Warn("terminal heartbeat failed", "session", uuidString(row.Uuid), "error", err)
			return ""
		}
		// Zero means another replica or the sweep has finalized the row — or
		// that another attach superseded this one (ADR-065 §5), which is the
		// same sentence and the same conclusion. Do not leave the actual PTY
		// alive after its durable authorization is gone.
		if n == 0 {
			return a.finalizedTerminalEndReason(parent, row)
		}
		a.watchTerminalTarget(parent, row)
		return ""
	}
}

// finalizedTerminalEndReason answers the beat that just discovered its session
// is over: WHY it is over, read off the row somebody else finalized.
//
// This is the cross-replica half of every cut. The replica that decides — the
// sweep, a revocation, an administrator, the §2 liveness cut — writes the reason
// and never touches the socket, which some other replica holds; that replica
// learns of the decision only by its beat matching zero rows. Reporting
// `disconnect` there told a developer whose container had stopped that their own
// network dropped, which is the one sentence ADR-067 §2 exists to stop them
// reading. On a single-replica instance the cut reaches the socket directly and
// this path never runs, which is exactly why the defect survived.
//
// It costs one indexed read, and it runs at most ONCE per session — on the beat
// that finds the row gone, after which the bridge leaves. The 20-second beat
// itself is untouched and stays a single statement.
//
// The fallback is a decision, not a leftover: a row that says nothing (still
// open, so this attach was superseded rather than the session ended) and a row
// that cannot be read at all (purged, or a database that just went away) both
// end the socket as `disconnect`. It is the honest word when the control plane
// genuinely does not know, and it is what this branch reported for every case
// before — so the fallback is a narrowing of the lie, never a widening.
func (a *API) finalizedTerminalEndReason(parent context.Context, row store.TerminalSession) terminal.EndReason {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), terminalBeatBudget)
	defer cancel()
	persisted, err := a.Store.GetTerminalSessionEndReason(ctx, row.ID)
	if err != nil {
		a.Logger.Warn("terminal session ended elsewhere, reason unreadable",
			"session", uuidString(row.Uuid), "error", err)
		return terminal.EndDisconnect
	}
	if persisted == nil || *persisted == "" {
		return terminal.EndDisconnect
	}
	return terminal.EndReason(*persisted)
}

// watchTerminalTarget is what a beat owes the target it is holding open: record
// that somebody is connected to it, and end the session when it is definitely
// gone.
//
// The consequence a reviewer will ask about, stated plainly: an attached but
// SILENT shell now keeps a scale-to-zero resource awake. That is the intended
// reading of "someone is connected" — a developer reading a log in a shell is
// still working — and it is bounded by the session's own limits rather than the
// resource's window: 15 minutes idle and 4 hours absolute (§24.4), the idle
// timer counting keystrokes only, so a forgotten shell watching a spinner dies
// on schedule and the resource sleeps one window later.
func (a *API) watchTerminalTarget(parent context.Context, row store.TerminalSession) {
	// A server shell (terminal:root) has no container, no resource and no
	// scale-to-zero clock: it is outside this decision in both directions, and
	// its liveness is the SSH connection's business (ADR-067 §2).
	if row.TargetKind != store.TerminalTargetContainer || row.ServerID == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), terminalBeatBudget)
	defer cancel()

	ref, gone, msg := a.resolveTerminalTargetRef(ctx, row)
	if msg != "" {
		if gone {
			a.cutTerminalOnStoppedTarget(row, msg)
		}
		return
	}
	a.recordTerminalActivity(ctx, row, ref)
	if a.targetContainerStopped(ctx, *row.ServerID, ref.container) {
		a.cutTerminalOnStoppedTarget(row, "the target container is no longer running")
	}
}

// recordTerminalActivity is the beat's call into the shared writer, with the
// target kind read off the resource the beat just resolved anyway. A terminal
// opened on one COMPONENT of a Compose-deployed application records against the
// application, which is where the flag and the clock live (ADR-067 §1) — the
// component's resource row IS the application's.
func (a *API) recordTerminalActivity(ctx context.Context, row store.TerminalSession, ref terminalTargetRef) {
	var applicationID *int64
	if ref.resource.ResourceType == store.ResourceTypeApplication {
		applicationID = &ref.resource.ID
	}
	a.recordSessionActivity(ctx, row.PreviewID, applicationID, uuidString(row.Uuid))
}

// cutTerminalOnStoppedTarget ends the session through the presence register, so
// the bridge returns target_stopped and the durable row, the audit entry and the
// last line the developer reads all come from that one value. The row itself is
// finalized by the rung as it leaves — cutting here and ending there is exactly
// what the tunnel's twin does.
func (a *API) cutTerminalOnStoppedTarget(row store.TerminalSession, why string) {
	if a.terminalCut(uuidString(row.Uuid), terminalEndReasonTargetStopped) {
		a.Logger.Info("terminal target vanished, session cut",
			"session", uuidString(row.Uuid), "target", row.TargetName, "reason", why)
	}
}

// endTerminalSession closes the row and audits the close. It runs on a fresh
// context: the request context is usually already dead when a session ends.
//
// The close is guarded by the attach generation the caller claimed with
// (ADR-065 §5): an attach that a re-claim displaced updates zero rows, so the
// row stays open for the winner and no close is audited for the loser. Only an
// attach calls this — revocation and the sweep finalize unconditionally,
// because their verdict is about the session rather than about whichever socket
// happens to hold it.
func (a *API) endTerminalSession(row store.TerminalSession, reason terminal.EndReason) {
	// `superseded` is never written, for the reason its constant gives: the
	// session did not end, this socket did. The attach generation already makes
	// the statement a no-op for a displaced attach, and saying it here keeps a
	// future caller from depending on that coincidence.
	if reason == terminalEndReasonSuperseded {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endReason := store.TerminalEndReason(reason)
	attachSeq := row.AttachSeq
	n, err := a.Store.EndTerminalSession(ctx, store.EndTerminalSessionParams{
		ID: row.ID, EndReason: &endReason, AttachSeq: &attachSeq,
	})
	if err != nil {
		a.Logger.Warn("terminal session close failed", "session", uuidString(row.Uuid), "error", err)
	}
	if n > 0 {
		a.Audit.System(ctx, &row.TeamID, "terminal.close", "terminal_session", row.Uuid, store.AuditResultSuccess)
	}
}

// markTerminalSessionStreamed records that this session's PTY actually
// attached. It is written once per session — a terminal has exactly one data
// stream — and read by the sweep alone, which needs it to tell an abandoned
// claim from a live shell now that an abandoned attach leaves its row
// re-claimable rather than ended (ADR-065 §6).
func (a *API) markTerminalSessionStreamed(row store.TerminalSession) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.Store.MarkTerminalSessionStreamed(ctx, row.ID); err != nil {
		// Not fatal to the session: the cost of losing this write is a slot
		// held until the max-duration ceiling, not a shell that fails to open.
		a.Logger.Warn("terminal session stream stamp failed", "session", uuidString(row.Uuid), "error", err)
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
