// HTTP attach for the egress tunnel (ADR-064): the attach path that redeems a
// WebSocket also carries the HTTP v2 ladder. One path, three
// protocols, dispatched on the method and the content type — the shape the
// agent's ingress attach already proved (internal/agent/ingress.go).
//
// The direction is the mirror of ingress: here the CLI opens a stream whenever
// a local client connects, so the session request carries no opens. It carries
// the session itself — its bounds, its liveness, and why it ended.

package handlers

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// egressAttach is one live HTTP-attached port-forward. It lives in this
// process only, like the WebSocket bridge it replaces: the durable row records
// the session, this records the socket serving it.
type egressAttach struct {
	key     [sha256.Size]byte
	dial    func(ctx context.Context) (net.Conn, error)
	session *tunnel.HTTPSession
}

func (a *API) egressRegister(sessionUUID string, attach *egressAttach) {
	a.egressMu.Lock()
	defer a.egressMu.Unlock()
	if a.egressLive == nil {
		a.egressLive = map[string]*egressAttach{}
	}
	a.egressLive[sessionUUID] = attach
}

func (a *API) egressRelease(sessionUUID string, attach *egressAttach) {
	a.egressMu.Lock()
	defer a.egressMu.Unlock()
	if a.egressLive[sessionUUID] == attach {
		delete(a.egressLive, sessionUUID)
	}
}

// egressLookup resolves a data stream to its session, in constant time on the
// key: the session UUID is not a secret, the attach key is.
func (a *API) egressLookup(sessionUUID string, key [sha256.Size]byte) *egressAttach {
	a.egressMu.Lock()
	attach := a.egressLive[sessionUUID]
	a.egressMu.Unlock()
	if attach == nil || subtle.ConstantTimeCompare(attach.key[:], key[:]) != 1 {
		return nil
	}
	return attach
}

// TunnelAttachOptions implements OPTIONS on the attach path — the capability probe of
// ADR-061, answered without spending the single-use token.
func (a *API) TunnelAttachOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "OPTIONS, POST, GET")
	w.Header().Set(tunnel.EgressHTTP.CapabilitiesHeader, tunnel.EgressHTTP.Name+",h3,h2,websocket")
	w.WriteHeader(http.StatusNoContent)
}

// TunnelAttach implements POST on the attach path — the session request or one data
// stream, told apart by content type. A request carrying another access path's
// content type falls through to 415, which is ADR-027's rule enforced rather
// than assumed.
func (a *API) TunnelAttach(w http.ResponseWriter, r *http.Request) {
	switch baseContentType(r.Header.Get("Content-Type")) {
	case tunnel.EgressHTTP.ControlContentType:
		a.tunnelAttachSession(w, r)
	case tunnel.EgressHTTP.StreamContentType:
		a.tunnelAttachStream(w, r)
	default:
		httpapi.WriteError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"this endpoint carries "+tunnel.EgressHTTP.Name+" only")
	}
}

// tunnelAttachSession claims the one-time token and holds the session open for
// its whole life. The SSH client it resolves is dialed once here and used by
// every data stream, so it must outlive them all — which is exactly what this
// request's lifetime is for.
func (a *API) tunnelAttachSession(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(tunnel.EgressHTTP.ProtocolHeader) != tunnel.EgressHTTP.Name {
		httpapi.WriteError(w, r, http.StatusUpgradeRequired, "unsupported_protocol",
			"this endpoint speaks "+tunnel.EgressHTTP.Name)
		return
	}
	key, err := decodeAttachKey(r.Header.Get(tunnel.EgressHTTP.AttachKeyHeader))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid attach key")
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "missing tunnel token")
		return
	}
	row, err := a.Store.ClaimPortForwardSession(r.Context(), hashPortForwardToken(token))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized,
			"invalid, expired or already used tunnel token")
		return
	}

	client, addr, errMsg := a.tunnelTarget(r.Context(), row)
	if errMsg != "" {
		a.endPortForwardSession(row, tunnel.EndDisconnect)
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, errMsg)
		return
	}
	defer func() { _ = client.Close() }()

	if err := enableFullDuplex(w, r); err != nil {
		a.endPortForwardSession(row, tunnel.EndDisconnect)
		httpapi.WriteError(w, r, http.StatusHTTPVersionNotSupported, "full_duplex_unavailable",
			"full-duplex HTTP streaming is unavailable on this connection")
		return
	}
	sessionUUID := uuidString(row.Uuid)
	w.Header().Set("Content-Type", tunnel.EgressHTTP.ControlContentType)
	w.Header().Set(tunnel.EgressHTTP.ProtocolHeader, tunnel.EgressHTTP.Name)
	// The CLI binds its data streams to this: the mint token was spent here,
	// and a stream presents the session and the attach key instead.
	w.Header().Set(tunnel.EgressHTTP.SessionHeader, sessionUUID)
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		a.endPortForwardSession(row, tunnel.EndDisconnect)
		return
	}

	bounds := sessionBounds(row)
	bounds.Cancel = a.Tunnels.register(row.ID)
	bounds.OnHeartbeat = a.portForwardHeartbeat(row)
	defer a.Tunnels.unregister(row.ID)

	control := tunnel.NewLineControl(r.Body, responseWriter{w}, controller.Flush, r.Body.Close)
	session := tunnel.NewHTTPSession(control, bounds)
	attach := &egressAttach{
		key:     key,
		dial:    func(context.Context) (net.Conn, error) { return client.DialTCP(addr) },
		session: session,
	}
	a.egressRegister(sessionUUID, attach)

	reason := session.Run(r.Context(), bounds)
	// A session cut by its grant running out is neither an idle timeout nor a
	// revocation, and the CLI says exactly that to the developer (ADR-045 §5).
	if reason == tunnel.EndMaxDuration && row.GrantID != nil {
		reason = endReasonGrantExpired
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Second)
	_ = session.SendClose(closeCtx, reason)
	cancel()
	_ = session.Close()
	a.egressRelease(sessionUUID, attach)
	a.endPortForwardSession(row, reason)
}

// tunnelAttachStream carries one forwarded TCP connection. It is authenticated
// by the ephemeral attach key alone: the mint token was spent by the session
// request, and re-presenting it here would mean a second claim.
func (a *API) tunnelAttachStream(w http.ResponseWriter, r *http.Request) {
	key, err := decodeAttachKey(r.Header.Get(tunnel.EgressHTTP.AttachKeyHeader))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "invalid attach key")
		return
	}
	attach := a.egressLookup(r.Header.Get(tunnel.EgressHTTP.SessionHeader), key)
	if attach == nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "unknown tunnel session")
		return
	}

	target, err := attach.dial(r.Context())
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadGateway, "target_unreachable",
			"the target refused the connection")
		return
	}
	// Admitted before the response head is written, so a refusal is an answer
	// the local client gets at once rather than a stream that stalls.
	admitted, err := attach.session.Admit(target)
	if err != nil {
		_ = target.Close()
		if errors.Is(err, tunnel.ErrSessionStreamLimit) {
			w.Header().Set("Retry-After", "1")
			httpapi.WriteError(w, r, http.StatusServiceUnavailable, "too_many_streams",
				"this tunnel is at its concurrent-connection limit — retry shortly")
			return
		}
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "the tunnel session has ended")
		return
	}
	defer func() { _ = admitted.Close() }()

	if err := enableFullDuplex(w, r); err != nil {
		httpapi.WriteError(w, r, http.StatusHTTPVersionNotSupported, "full_duplex_unavailable",
			"full-duplex HTTP streaming is unavailable on this connection")
		return
	}
	w.Header().Set("Content-Type", tunnel.EgressHTTP.StreamContentType)
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		return
	}
	stream := tunnel.NewDuplexConn(r.Body, responseWriter{w}, controller.Flush, nil)
	spliceConns(r.Context(), attach.session.Done(), stream, admitted)
}

// spliceConns copies both ways until either side ends or the session stops.
// The session's Done is watched too: a forwarded connection must not outlive
// the authorization that opened it (§24.4).
func spliceConns(ctx context.Context, sessionDone <-chan struct{}, a, b net.Conn) {
	done := make(chan struct{}, 2)
	copyOne := func(dst, src net.Conn) {
		buf := egressCopyBuffers.Get().(*[]byte)
		_, _ = io.CopyBuffer(dst, src, *buf)
		egressCopyBuffers.Put(buf)
		done <- struct{}{}
	}
	go copyOne(a, b)
	go copyOne(b, a)
	select {
	case <-done:
	case <-sessionDone:
	case <-ctx.Done():
	}
	_ = a.Close()
	_ = b.Close()
}

var egressCopyBuffers = sync.Pool{New: func() any {
	buf := make([]byte, 64*1024)
	return &buf
}}

// portForwardHeartbeat persists liveness on the durable row. The socket
// remains the source of truth while this process is alive, so a transient
// database failure is logged and retried on the next beat; zero rows updated
// means another replica or the scheduler finalized the session, and the actual
// socket must not outlive its durable authorization.
func (a *API) portForwardHeartbeat(row store.PortForwardSession) func(context.Context) bool {
	return func(parent context.Context) bool {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		defer cancel()
		n, err := a.Store.HeartbeatPortForwardSession(ctx, row.ID)
		if err != nil {
			a.Logger.Warn("port-forward heartbeat failed", "session", uuidString(row.Uuid), "error", err)
			return true
		}
		return n > 0
	}
}

// baseContentType drops the parameters: `application/…+json; charset=utf-8`
// must dispatch like `application/…+json`.
func baseContentType(value string) string {
	if i := strings.IndexByte(value, ';'); i >= 0 {
		value = value[:i]
	}
	return strings.TrimSpace(value)
}

// decodeAttachKey hashes the key on sight; the plaintext lives only in the
// request headers, never in this process's state.
func decodeAttachKey(value string) ([sha256.Size]byte, error) {
	var hash [sha256.Size]byte
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != sha256.Size {
		return hash, errors.New("attach key must be 256-bit base64url")
	}
	return sha256.Sum256(raw), nil
}

// enableFullDuplex lets the handler read the request body after it started
// answering. Required on HTTP/1.1 — which is how Traefik reaches the control
// plane — and native above it.
func enableFullDuplex(w http.ResponseWriter, r *http.Request) error {
	err := http.NewResponseController(w).EnableFullDuplex()
	if err != nil && r.ProtoMajor < 2 {
		return err
	}
	return nil
}

// responseWriter adapts the response half to io.WriteCloser: closing it is the
// handler's business, not the stream's.
type responseWriter struct{ io.Writer }

func (responseWriter) Close() error { return nil }
