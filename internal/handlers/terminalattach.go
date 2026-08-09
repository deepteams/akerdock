// HTTP attach for the terminal (ADR-064 §3): the path that redeems a
// WebSocket also carries the HTTP ladder. One path, three protocols,
// dispatched on the method and the content type — the shape the egress attach
// already proved (portforwardattach.go).
//
// The terminal is not a special case of the ladder; it is the ladder with
// exactly ONE data stream. The session request carries the control wire —
// resize, liveness, the end reason — and one data stream carries the PTY's
// bytes. Nothing is re-framed: terminal.HTTPConn merges the pair back into the
// typed messages terminal.Bridge already reads, so the bridge that owns every
// bound and the guaranteed teardown is the same one the WebSocket rung uses.

package handlers

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/terminal"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// terminalStreamOpenTimeout bounds how long a session waits for its one data
// stream. The CLI opens it the moment the session response head arrives, so
// this only has to cover a round trip; a session that never gets its stream is
// a client that died between the two requests, and its PTY must not sit there
// holding an SSH channel.
const terminalStreamOpenTimeout = 15 * time.Second

// terminalAttach is one live terminal session, in this process only: the
// durable row records the session, this records whatever is serving it right
// now. Every rung has one — the HTTP pair of requests and the WebSocket alike
// (ADR-067 §2) — because a session that could not be cut for having landed on
// one rung rather than another is exactly the asymmetry ADR-064 §2 forbids.
type terminalAttach struct {
	key [sha256.Size]byte
	// stream carries the data request to the session, once.
	stream chan net.Conn
	// claimed makes the single data stream single: a second one is refused
	// before it commits a response head.
	claimed atomic.Bool
	// done releases the data request's handler when the bridge ends. That
	// handler must outlive the bytes it carries — returning would close the
	// response body under the PTY.
	done     chan struct{}
	doneOnce sync.Once
	// cancel ends the session request that owns this attach. It is what makes a
	// cut a cut rather than a hope (ADR-065 §5): the session may be waiting for
	// its stream, waiting on its PTY resolution, or already bridging, and one
	// cancel reaches all three.
	cancel context.CancelFunc
	// cut carries the WORD for that teardown to a bridge that is already
	// running. The cancel above ends a session at any stage but says only
	// "revoked"; this says target_stopped, or superseded, and it is what the
	// developer reads (ADR-067 §2). Buffered so a cut never blocks on a bridge
	// between selects, and dropped on the floor when a session is past reading
	// it — the cancel still lands.
	cut chan terminal.EndReason
	// bridging says the PTY and the socket are joined and a bridge is now
	// watching that channel. The distinction decides how a cut lands, and it is
	// not a nicety: BEFORE this, only the cancel reaches a session — it may be
	// waiting for its data stream or resolving a shell on another machine, and
	// nothing else interrupts either. AFTER it, the cancel is destructive,
	// because coder/websocket treats cancelling an in-flight Read as a hard
	// connection close and the end frame naming the reason would never leave the
	// process. So a cut past this point hands the bridge the word and lets it
	// unwind, which it does on its next select.
	bridging atomic.Bool
}

func newTerminalAttach(key [sha256.Size]byte, cancel context.CancelFunc) *terminalAttach {
	return &terminalAttach{
		key: key, stream: make(chan net.Conn, 1), done: make(chan struct{}),
		cancel: cancel, cut: make(chan terminal.EndReason, 1),
	}
}

// newTerminalWebSocketAttach is the bottom rung's registration. The WebSocket
// carries its own bytes, so it hands out no data stream — its slot is claimed at
// birth, and a data request that presents this session's attach key gets the
// ordinary 409 instead of parking a connection nobody will ever read.
func newTerminalWebSocketAttach(key [sha256.Size]byte, cancel context.CancelFunc) *terminalAttach {
	attach := newTerminalAttach(key, cancel)
	attach.claimed.Store(true)
	return attach
}

func (t *terminalAttach) finish() {
	t.doneOnce.Do(func() {
		close(t.done)
		if t.cancel != nil {
			t.cancel()
		}
	})
}

// cutWith ends this attach from outside and names the reason the developer
// reads. The order is the contract: the reason is queued BEFORE anything else,
// because a cancellation reaching the bridge on its own reports `revoked`, and
// the bridge prefers the queued reason precisely because it is already there.
//
// A live bridge is then left to unwind itself, for the reason `bridging` gives.
// A session that has not got that far is cancelled, which is the only thing
// that reaches it. The window between the two is one atomic store wide, and a
// cut that loses it still persists the right reason — it may only lose the
// frame that would have carried it, on a socket that is closing either way.
func (t *terminalAttach) cutWith(reason terminal.EndReason) {
	select {
	case t.cut <- reason:
	default:
	}
	if t.bridging.Load() {
		return
	}
	t.finish()
}

// terminalRegister publishes the attach serving a session. A session that
// already had one is being re-claimed by a client that climbed a rung
// (ADR-065): the loser is cut here rather than silently overwritten, because
// two live attaches on one session means two PTYs on someone's container.
//
// The displaced attach never finalizes the row — its close carries a stale
// attach generation and updates nothing (ADR-065 §5) — so the log line below is
// the only trace a supersession leaves, and that is where it belongs rather
// than in the audit trail: it is one developer's retry, not a second open.
func (a *API) terminalRegister(sessionUUID string, attach *terminalAttach) {
	a.terminalMu.Lock()
	if a.terminalLive == nil {
		a.terminalLive = map[string]*terminalAttach{}
	}
	displaced := a.terminalLive[sessionUUID]
	a.terminalLive[sessionUUID] = attach
	a.terminalMu.Unlock()
	if cutTerminalAttach(displaced, terminalEndReasonSuperseded) && a.Logger != nil {
		a.Logger.Info("terminal attach superseded by a re-claim", "session", sessionUUID)
	}
}

// terminalCut ends whatever attach currently holds a session, from outside and
// with a reason — the presence cut ADR-067 §2 obliges, and the terminal's twin
// of TunnelPresence.Cut. Reports whether a live attach was reached here; like
// the tunnel's it is per-process, so across replicas a cut converges through
// the beat's zero-rows answer instead, within one beat.
//
// The register is deliberately left alone: an attach removes its own entry as
// it unwinds (terminalRelease, by pointer), so deleting here would take a
// winner's entry with it whenever a cut raced a re-claim.
func (a *API) terminalCut(sessionUUID string, reason terminal.EndReason) bool {
	a.terminalMu.Lock()
	attach := a.terminalLive[sessionUUID]
	a.terminalMu.Unlock()
	return cutTerminalAttach(attach, reason)
}

// cutTerminalAttach tears one attach down: the reason reaches a bridge that is
// already running, done releases the data request parked on it, and the cancel
// unwinds a session request that has not got that far — closing its PTY on the
// way out.
func cutTerminalAttach(attach *terminalAttach, reason terminal.EndReason) bool {
	if attach == nil {
		return false
	}
	attach.cutWith(reason)
	return true
}

func (a *API) terminalRelease(sessionUUID string, attach *terminalAttach) {
	a.terminalMu.Lock()
	defer a.terminalMu.Unlock()
	if a.terminalLive[sessionUUID] == attach {
		delete(a.terminalLive, sessionUUID)
	}
}

// terminalLookup resolves a data stream to its session, in constant time on
// the key: the session UUID is not a secret, the attach key is.
func (a *API) terminalLookup(sessionUUID string, key [sha256.Size]byte) *terminalAttach {
	a.terminalMu.Lock()
	attach := a.terminalLive[sessionUUID]
	a.terminalMu.Unlock()
	if attach == nil || subtle.ConstantTimeCompare(attach.key[:], key[:]) != 1 {
		return nil
	}
	return attach
}

// TerminalAttachOptions implements OPTIONS on the terminal attach path — the
// capability probe of ADR-061, answered without spending the single-use token.
func (a *API) TerminalAttachOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "OPTIONS, POST, GET")
	w.Header().Set(tunnel.TerminalHTTP.CapabilitiesHeader, tunnel.TerminalHTTP.Name+",h3,h2,websocket")
	w.WriteHeader(http.StatusNoContent)
}

// TerminalAttach implements POST on the terminal attach path — the session
// request or its one data stream, told apart by content type. A request
// carrying another access path's content type falls through to 415, which is
// ADR-027's rule enforced rather than assumed.
func (a *API) TerminalAttach(w http.ResponseWriter, r *http.Request) {
	switch baseContentType(r.Header.Get("Content-Type")) {
	case tunnel.TerminalHTTP.ControlContentType:
		a.terminalAttachSession(w, r)
	case tunnel.TerminalHTTP.StreamContentType:
		a.terminalAttachStream(w, r)
	default:
		httpapi.WriteError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"this endpoint carries "+tunnel.TerminalHTTP.Name+" only")
	}
}

// terminalAttachSession claims the attach token and holds the control wire open
// for the session's whole life, then bridges the PTY to the data stream that
// joins it. The claim spends the token on the session it opens rather than on
// the request that tries (ADR-065), and everything the shell needs from another
// machine happens behind the response head (ADR-066).
func (a *API) terminalAttachSession(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(tunnel.TerminalHTTP.ProtocolHeader) != tunnel.TerminalHTTP.Name {
		httpapi.WriteError(w, r, http.StatusUpgradeRequired, "unsupported_protocol",
			"this endpoint speaks "+tunnel.TerminalHTTP.Name)
		return
	}
	key, err := decodeAttachKey(r.Header.Get(tunnel.TerminalHTTP.AttachKeyHeader))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid attach key")
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "missing terminal token")
		return
	}
	// The same key binds the data stream to this request AND identifies the
	// attacher to the claim (ADR-065 §3), which is what makes a rung the CLI
	// gave up on retryable: a re-claim presenting the same key inside the
	// token's TTL is the same attempt, not a replay.
	row, err := a.Store.ClaimTerminalSession(r.Context(), store.ClaimTerminalSessionParams{
		TokenHash: hashTerminalToken(token), AttachKeyHash: key[:],
	})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized,
			"invalid, expired or already used terminal token")
		return
	}

	// The local half only (ADR-066 §1): which server, and for a container shell
	// which container. These are indexed reads on a pooled connection and their
	// refusals are the actionable ones, so they keep their 409 in front of the
	// response head. Everything that crosses the network to another machine —
	// the SSH handshake, the exec-create on the agent channel — happens once the
	// head is out, because holding it is a bet on someone else's latency that
	// the CLI's open budget loses, with the token already spent.
	cols, rows := terminalGeometry(r)
	target, errMsg := a.terminalResolveTarget(r.Context(), row, cols, rows)
	if errMsg != "" {
		a.endTerminalSession(row, terminal.EndTargetUnreachable)
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, errMsg)
		return
	}

	if err := enableFullDuplex(w, r); err != nil {
		a.endTerminalSession(row, terminal.EndRevoked)
		httpapi.WriteError(w, r, http.StatusHTTPVersionNotSupported, "full_duplex_unavailable",
			"full-duplex HTTP streaming is unavailable on this connection")
		return
	}
	sessionUUID := uuidString(row.Uuid)
	// The session's own context: a supersession cancels it, and cancelling it
	// is what stops the resolution below mid-handshake.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Registered BEFORE the response head goes out: the CLI opens its data
	// stream the moment it reads that head, and a session not yet in the
	// register would answer it "unknown session".
	attach := newTerminalAttach(key, cancel)
	a.terminalRegister(sessionUUID, attach)
	defer a.terminalRelease(sessionUUID, attach)
	defer attach.finish()

	w.Header().Set("Content-Type", tunnel.TerminalHTTP.ControlContentType)
	w.Header().Set(tunnel.TerminalHTTP.ProtocolHeader, tunnel.TerminalHTTP.Name)
	// The CLI binds its data stream to this: the mint token was spent here,
	// and the stream presents the session and the attach key instead.
	w.Header().Set(tunnel.TerminalHTTP.SessionHeader, sessionUUID)
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		// The head never reached the client, so nothing was served and nobody is
		// listening: the row stays claimed and re-claimable for the rest of its
		// TTL (ADR-065 §6) instead of being finalized against a client that may
		// still be climbing the ladder.
		return
	}

	// The remote half starts here and not one line earlier (ADR-066 §2), and
	// eagerly rather than on demand: the CLI opens its data stream the instant it
	// reads the head, so the handshake and the stream open overlap instead of
	// queueing. The session request owns the promise — releasing it is what
	// closes an SSH client or a shell that a resolution produced after everyone
	// stopped waiting for it.
	wake := a.lookupWake(sessionUUID)
	promise := startTerminalPTY(ctx, func(ctx context.Context) (terminal.PTY, func(), string) {
		return a.terminalOpenPTYAfterWake(ctx, wake, target)
	})
	defer promise.release()

	control := tunnel.NewLineControl(r.Body, responseWriter{w}, controller.Flush, r.Body.Close)
	// A shell that appears to hang for a minute reads as the bug this whole
	// investigation started from, so the session says on its own control wire
	// that a cold start is under way (ADR-067 §6). The frame goes out before the
	// data stream is even awaited: the client is already reading this wire.
	announceWake(ctx, control, wake)
	data, ok := awaitTerminalStream(ctx, attach)
	if !ok {
		// The client died between the two requests, or a re-claim took the
		// session away. Either way nothing was served and the row is left for
		// whoever comes back for it.
		_ = control.Close()
		return
	}
	// The PTY has a byte path: the row records it, which is what keeps the sweep
	// from closing it at token expiry (ADR-065 §6).
	a.markTerminalSessionStreamed(row)

	conn := terminal.NewHTTPConn(control, data)
	pty, errMsg, ok := promise.await(ctx)
	switch {
	case !ok:
		// The session ended before the target answered. Nobody to tell.
		_ = conn.Close()
		return
	case errMsg != "":
		// The end frame is the only report channel this family has: its data
		// stream carries no dial, so it has nothing to answer 502 about
		// (ADR-066 §3). The reason is target_unreachable and not revoked —
		// nobody revoked anything — and the sentence travels beside it. A target
		// that never came up carries wake_failed instead, so the audit row and
		// the last line the developer reads come from one value (ADR-067 §6).
		reason := terminalSessionEndReason(wake)
		terminal.SendEnd(conn, reason, errMsg)
		a.endTerminalSession(row, reason)
		_ = conn.Close()
		return
	}

	// From here a cut is delivered as a word rather than as a cancellation.
	attach.bridging.Store(true)
	reason := terminal.Bridge(ctx, conn, pty, terminal.Options{
		IdleTimeout: a.terminalIdleTimeout(),
		MaxDuration: a.terminalMaxDuration(),
		// Both duties of the beat, on the rung that has an attach register
		// (ADR-067 §1 and §2) — and identical to the WebSocket rung's, which is
		// what keeps a session from behaving differently for having landed here.
		OnHeartbeat: a.terminalHeartbeat(row),
		Cancel:      attach.cut,
	})
	a.endTerminalSession(row, reason)
	_ = conn.Close()
}

// terminalPTYPromise is the remote half of an attach, in flight (ADR-066 §2):
// started once the response head is out, awaited by whoever needs the shell,
// and released exactly once by the session request that owns it.
//
// The release discipline is the whole point. A resolution that lands after its
// session tore down owns what it produced and closes it itself — the same
// late-arrival rule dialTCPContext applies to a hung ssh.Client.Dial, one level
// up. Without it, a session abandoned during a 30 s handshake leaks an SSH
// client, or a live shell and an exec instance on someone's container.
type terminalPTYPromise struct {
	done chan struct{}

	mu       sync.Mutex
	pty      terminal.PTY
	cleanup  func()
	errMsg   string
	released bool
}

// terminalResolver is the remote half as one function — the SSH handshake plus
// StartPTY, or the exec-create plus exec-attach — bound to the target its
// session named. Taking it as a value rather than reaching for the API keeps the
// promise's lifetime testable without a server on the other end, which is where
// the leak this type exists to prevent would otherwise hide.
type terminalResolver func(ctx context.Context) (terminal.PTY, func(), string)

// startTerminalPTY runs the remote half under the session's context, so a
// session that ends mid-dial cancels the dial. It takes no budget of its own:
// each leg carries the bound it already had — the agent RPC's timeout, or the
// server's ssh_timeout_seconds — and a second budget superimposed on those
// would give two places to look when a shell fails to open.
func startTerminalPTY(ctx context.Context, resolve terminalResolver) *terminalPTYPromise {
	promise := &terminalPTYPromise{done: make(chan struct{})}
	go func() {
		defer close(promise.done)
		pty, cleanup, errMsg := resolve(ctx)
		promise.mu.Lock()
		if promise.released {
			promise.mu.Unlock()
			releaseTerminalPTY(pty, cleanup)
			return
		}
		promise.pty, promise.cleanup, promise.errMsg = pty, cleanup, errMsg
		promise.mu.Unlock()
	}()
	return promise
}

// await blocks until the remote half settles or the session ends first. Taking
// the PTY takes responsibility for closing it — that is the bridge's job at
// teardown; the promise keeps the transport cleanup, so the SSH client the
// shell rides on is released whichever way the session ends.
//
// ok is false when the session ended first: an abandonment, which reports
// nothing and finalizes nothing, not a refusal on the merits.
func (p *terminalPTYPromise) await(ctx context.Context) (terminal.PTY, string, bool) {
	select {
	case <-p.done:
	case <-ctx.Done():
		return nil, "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pty := p.pty
	p.pty = nil
	return pty, p.errMsg, true
}

// release is idempotent and settles ownership between the two arrivals that can
// race here: whichever of the resolution and the session's teardown comes last
// closes what the other left.
func (p *terminalPTYPromise) release() {
	p.mu.Lock()
	p.released = true
	pty, cleanup := p.pty, p.cleanup
	p.pty, p.cleanup = nil, nil
	p.mu.Unlock()
	releaseTerminalPTY(pty, cleanup)
}

func releaseTerminalPTY(pty terminal.PTY, cleanup func()) {
	if pty != nil {
		_ = pty.Close()
	}
	if cleanup != nil {
		cleanup()
	}
}

// awaitTerminalStream waits for the session's one data stream, bounded: a
// client that opened a session and then vanished must not hold a PTY open on
// the target server.
func awaitTerminalStream(ctx context.Context, attach *terminalAttach) (net.Conn, bool) {
	timer := time.NewTimer(terminalStreamOpenTimeout)
	defer timer.Stop()
	select {
	case stream := <-attach.stream:
		return stream, true
	case <-timer.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// terminalAttachStream carries the PTY's bytes. It is authenticated by the
// ephemeral attach key alone: the mint token was spent by the session request,
// and re-presenting it here would mean a second claim.
func (a *API) terminalAttachStream(w http.ResponseWriter, r *http.Request) {
	key, err := decodeAttachKey(r.Header.Get(tunnel.TerminalHTTP.AttachKeyHeader))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "invalid attach key")
		return
	}
	attach := a.terminalLookup(r.Header.Get(tunnel.TerminalHTTP.SessionHeader), key)
	if attach == nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "unknown terminal session")
		return
	}
	// A terminal has exactly one data stream. The second one is refused before
	// the response head is written, so the client gets an answer rather than a
	// stream nothing will ever read.
	if !attach.claimed.CompareAndSwap(false, true) {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
			"this terminal session already carries its data stream")
		return
	}

	if err := enableFullDuplex(w, r); err != nil {
		httpapi.WriteError(w, r, http.StatusHTTPVersionNotSupported, "full_duplex_unavailable",
			"full-duplex HTTP streaming is unavailable on this connection")
		return
	}
	w.Header().Set("Content-Type", tunnel.TerminalHTTP.StreamContentType)
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		return
	}
	stream := tunnel.NewDuplexConn(r.Body, responseWriter{w}, controller.Flush, nil)
	select {
	case attach.stream <- stream:
	case <-attach.done:
		return
	}
	// Hold the request open for as long as the session: this handler IS the
	// PTY's byte path, and returning closes it.
	select {
	case <-attach.done:
	case <-r.Context().Done():
	}
}
