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
	"github.com/deepteams/akerdock/internal/terminal"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// terminalStreamOpenTimeout bounds how long a session waits for its one data
// stream. The CLI opens it the moment the session response head arrives, so
// this only has to cover a round trip; a session that never gets its stream is
// a client that died between the two requests, and its PTY must not sit there
// holding an SSH channel.
const terminalStreamOpenTimeout = 15 * time.Second

// terminalAttach is one live HTTP-attached terminal, in this process only —
// like the WebSocket bridge it joins: the durable row records the session,
// this records the two requests serving it.
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
}

func newTerminalAttach(key [sha256.Size]byte) *terminalAttach {
	return &terminalAttach{key: key, stream: make(chan net.Conn, 1), done: make(chan struct{})}
}

func (t *terminalAttach) finish() { t.doneOnce.Do(func() { close(t.done) }) }

func (a *API) terminalRegister(sessionUUID string, attach *terminalAttach) {
	a.terminalMu.Lock()
	defer a.terminalMu.Unlock()
	if a.terminalLive == nil {
		a.terminalLive = map[string]*terminalAttach{}
	}
	a.terminalLive[sessionUUID] = attach
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

// terminalAttachSession claims the one-time token and holds the control wire
// open for the session's whole life, then bridges the PTY to the data stream
// that joins it.
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
	row, err := a.Store.ClaimTerminalSession(r.Context(), hashTerminalToken(token))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized,
			"invalid, expired or already used terminal token")
		return
	}

	// Resolve the target BEFORE committing the response, exactly as the
	// WebSocket rung does: an HTTP error is diagnosable, a stream that dies is
	// a mystery.
	cols, rows := terminalGeometry(r)
	pty, cleanup, errMsg := a.terminalPTY(r.Context(), row, cols, rows)
	if errMsg != "" {
		a.endTerminalSession(row, terminal.EndRevoked)
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, errMsg)
		return
	}
	defer cleanup()

	if err := enableFullDuplex(w, r); err != nil {
		_ = pty.Close()
		a.endTerminalSession(row, terminal.EndRevoked)
		httpapi.WriteError(w, r, http.StatusHTTPVersionNotSupported, "full_duplex_unavailable",
			"full-duplex HTTP streaming is unavailable on this connection")
		return
	}
	sessionUUID := uuidString(row.Uuid)
	// Registered BEFORE the response head goes out: the CLI opens its data
	// stream the moment it reads that head, and a session not yet in the
	// register would answer it "unknown session".
	attach := newTerminalAttach(key)
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
		_ = pty.Close()
		a.endTerminalSession(row, terminal.EndDisconnect)
		return
	}

	control := tunnel.NewLineControl(r.Body, responseWriter{w}, controller.Flush, r.Body.Close)
	data, ok := awaitTerminalStream(r.Context(), attach)
	if !ok {
		_ = pty.Close()
		_ = control.Close()
		a.endTerminalSession(row, terminal.EndDisconnect)
		return
	}

	conn := terminal.NewHTTPConn(control, data)
	reason := terminal.Bridge(r.Context(), conn, pty, terminal.Options{
		IdleTimeout: a.terminalIdleTimeout(),
		MaxDuration: a.terminalMaxDuration(),
	})
	a.endTerminalSession(row, reason)
	_ = conn.Close()
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
