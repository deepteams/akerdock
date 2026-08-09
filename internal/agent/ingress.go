// Ingress module (ADR-060): the agent side of a dev ingress tunnel. Traefik
// terminates the endpoint's stable FQDN and hands every request to this HTTP
// front; when a laptop holds the attach socket, the request is relayed over
// the mux as one stream per connection; otherwise the URL serves an offline
// page. The attach WebSocket arrives THROUGH Traefik on the endpoint's own
// host (reserved path), authenticated by a single-use token the control plane
// armed over the command channel — visitor bytes never touch the control
// plane (INV-007).
package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// IngressRoute maps one public host to its declared ingress endpoint. Part of
// the deposited routing table (routes.json), so the agent recognizes its
// ingress hosts — and serves their offline page — across restarts, before any
// session control arrives.
type IngressRoute struct {
	Host         string `json:"host"`
	EndpointUUID string `json:"endpoint_uuid"`
}

// IngressSubprotocol is the attach WebSocket subprotocol (ADR-060 §3). A
// distinct name per access path is ADR-027's standing rule, and it is
// load-bearing: an ingress attach token must never be redeemable against the
// egress tunnel or vice versa.
const IngressSubprotocol = "akerdock-ingress-v1"

const (
	// ingressMaxDuration is the session ceiling (ADR-060 §6): ADR-032's 4 h
	// was calibrated on interactive egress sessions; an ingress tunnel's
	// typical day is a webhook target exercised irregularly until evening.
	ingressMaxDuration = 12 * time.Hour

	// HTTP/2 can fan one page load into hundreds of requests at once. Keep the
	// laptop's actual TCP fan-out bounded while absorbing that burst ahead of
	// the mux instead of turning its 129th request into an overload response.
	ingressMaxActiveStreams = tunnel.IngressMaxStreams
	ingressMaxQueuedStreams = 512
	ingressStreamQueueWait  = 30 * time.Second
)

// endReasonRevoked mirrors the terminal_end_reason value used for an operator
// cut or an endpoint deletion — a policy close the CLI does not re-dial
// through.
const endReasonRevoked tunnel.EndReason = "revoked"

// ingressExpect is one armed attach expectation — in-memory only, as befits a
// 60-second secret: an agent restart refuses the attach and the CLI re-mints.
type ingressExpect struct {
	sessionUUID  string
	endpointUUID string
	expires      time.Time
}

// ingressSession is one live attach.
type ingressSession struct {
	sessionUUID  string
	endpointUUID string
	origin       ingressStreamOpener
	httpOrigin   *tunnel.HTTPOrigin
	wsLanes      *tunnel.MultiLaneConn
	attachKey    [sha256.Size]byte
	transport    string
	cancel       chan tunnel.EndReason
	relay        http.Handler
}

// Ingress relays visitor traffic for declared ingress endpoints to the laptop
// currently attached. Its lifetime is the process — a routing-table reload
// must never drop a live tunnel — so Serve constructs one and updates its
// host table on each reload.
type Ingress struct {
	Logger  *slog.Logger
	metrics *ingressMetrics
	// Notify pushes lifecycle observations (claimed / alive / closed) to the
	// control plane; nil drops them (un-enrolled helper — sessions cannot be
	// armed either, so nothing is lost).
	Notify func(Observation)

	mu      sync.Mutex
	hosts   map[string]string // host → endpoint uuid
	expects map[string]ingressExpect
	live    map[string]*ingressSession // endpoint uuid → session
}

// NewIngress builds an empty module; SetRoutes arms its hosts.
func NewIngress(logger *slog.Logger) *Ingress {
	if logger == nil {
		logger = slog.Default()
	}
	return &Ingress{
		Logger:  logger,
		metrics: newIngressMetrics(),
		hosts:   map[string]string{},
		expects: map[string]ingressExpect{},
		live:    map[string]*ingressSession{},
	}
}

// SetRoutes replaces the host table on a routing reload. A live session whose
// endpoint left the table is cut as revoked — the control plane also sends an
// explicit cut on deletion; this is the belt to that suspender.
func (ig *Ingress) SetRoutes(routes []IngressRoute) {
	ig.mu.Lock()
	hosts := make(map[string]string, len(routes))
	known := make(map[string]bool, len(routes))
	for _, r := range routes {
		hosts[r.Host] = r.EndpointUUID
		known[r.EndpointUUID] = true
	}
	ig.hosts = hosts
	var cut []*ingressSession
	for uuid, s := range ig.live {
		if !known[uuid] {
			cut = append(cut, s)
		}
	}
	ig.mu.Unlock()
	for _, s := range cut {
		select {
		case s.cancel <- endReasonRevoked:
		default:
		}
	}
}

// Handles reports whether host belongs to an ingress endpoint.
func (ig *Ingress) Handles(host string) bool {
	ig.mu.Lock()
	defer ig.mu.Unlock()
	_, ok := ig.hosts[host]
	return ok
}

// Expect arms a single-use attach expectation (command channel, ADR-060 §3).
func (ig *Ingress) Expect(p agentwire.IngressExpectParams) {
	ig.mu.Lock()
	defer ig.mu.Unlock()
	now := time.Now()
	for hash, e := range ig.expects {
		if e.expires.Before(now) {
			delete(ig.expects, hash)
		}
	}
	ig.expects[strings.ToLower(p.TokenSHA256)] = ingressExpect{
		sessionUUID:  p.SessionUUID,
		endpointUUID: p.EndpointUUID,
		expires:      time.Unix(p.ExpiresAtUnix, 0),
	}
}

// Cut ends a live session (or disarms its pending expectation) with the given
// reason. Reports whether anything matched.
func (ig *Ingress) Cut(sessionUUID, reason string) bool {
	ig.mu.Lock()
	var target *ingressSession
	var cancel chan tunnel.EndReason
	for _, s := range ig.live {
		if s.sessionUUID == sessionUUID {
			target = s
			cancel = s.cancel
			break
		}
	}
	found := target != nil
	for hash, e := range ig.expects {
		if e.sessionUUID == sessionUUID {
			delete(ig.expects, hash)
			found = true
		}
	}
	ig.mu.Unlock()
	if target != nil {
		r := tunnel.EndReason(reason)
		if r == "" {
			r = endReasonRevoked
		}
		select {
		case cancel <- r:
		default:
		}
	}
	return found
}

// ServeHTTP handles every request whose Host is an ingress endpoint's.
func (ig *Ingress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostname(r.Host)
	ig.mu.Lock()
	endpointUUID, ok := ig.hosts[host]
	var session *ingressSession
	var relay http.Handler
	if ok {
		session = ig.live[endpointUUID]
		if session != nil {
			relay = session.relay
		}
	}
	ig.mu.Unlock()
	if !ok {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	if r.URL.Path == proxy.IngressAttachPath {
		ig.attach(w, r, endpointUUID)
		return
	}
	if session == nil || relay == nil {
		ig.serveOfflinePage(w, r, host)
		return
	}
	relay.ServeHTTP(w, r)
}

// attach routes the reserved path without consuming a mint during capability
// discovery. HTTP v2 and the compatibility WebSocket share the same one-use
// claim and endpoint occupancy boundary.
func (ig *Ingress) attach(w http.ResponseWriter, r *http.Request, endpointUUID string) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", "OPTIONS, POST, GET")
		w.Header().Set(tunnel.IngressCapabilitiesHeader, tunnel.IngressHTTPProtocol+",h3,h2,websocket-v2,websocket")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	contentType := r.Header.Get("Content-Type")
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = strings.TrimSpace(contentType[:i])
	}
	if r.Method == http.MethodPost && contentType == tunnel.IngressControlContentType {
		ig.attachHTTPControl(w, r, endpointUUID)
		return
	}
	if r.Method == http.MethodPost && contentType == tunnel.IngressStreamContentType {
		ig.attachHTTPStream(w, r, endpointUUID)
		return
	}
	if r.Header.Get(tunnel.IngressLaneHeader) != "" {
		ig.attachWebSocketLane(w, r, endpointUUID)
		return
	}
	ig.attachWebSocket(w, r, endpointUUID)
}

type ingressClaimError struct {
	status  int
	message string
}

// claimAttach consumes a mint on sight and reserves exclusive occupancy.
func (ig *Ingress) claimAttach(token, endpointUUID string) (ingressExpect, *ingressSession, *ingressClaimError) {
	if token == "" {
		return ingressExpect{}, nil, &ingressClaimError{http.StatusUnauthorized, "missing attach token"}
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])

	ig.mu.Lock()
	expect, ok := ig.expects[hash]
	// Single use: consumed on sight, valid or not — a replay learns nothing.
	delete(ig.expects, hash)
	switch {
	case !ok || subtle.ConstantTimeCompare([]byte(expect.endpointUUID), []byte(endpointUUID)) != 1:
		ig.mu.Unlock()
		return ingressExpect{}, nil, &ingressClaimError{http.StatusUnauthorized, "invalid attach token"}
	case expect.expires.Before(time.Now()):
		ig.mu.Unlock()
		return ingressExpect{}, nil, &ingressClaimError{http.StatusUnauthorized, "attach token expired — mint a new session"}
	case ig.live[endpointUUID] != nil:
		ig.mu.Unlock()
		return ingressExpect{}, nil, &ingressClaimError{http.StatusConflict, "endpoint occupied — one laptop per endpoint"}
	}
	placeholder := &ingressSession{
		sessionUUID: expect.sessionUUID, endpointUUID: endpointUUID,
		cancel: make(chan tunnel.EndReason, 1),
	}
	ig.live[endpointUUID] = placeholder
	ig.mu.Unlock()
	return expect, placeholder, nil
}

// attachWebSocket is ADR-060's compatible final fallback.
func (ig *Ingress) attachWebSocket(w http.ResponseWriter, r *http.Request, endpointUUID string) {
	token := r.URL.Query().Get("token")
	expect, placeholder, claimErr := ig.claimAttach(token, endpointUUID)
	if claimErr != nil {
		http.Error(w, claimErr.message, claimErr.status)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{tunnel.IngressWebSocketV2, IngressSubprotocol}})
	if err != nil {
		ig.release(endpointUUID, placeholder)
		return
	}
	if conn.Subprotocol() != tunnel.IngressWebSocketV2 && conn.Subprotocol() != IngressSubprotocol {
		ig.release(endpointUUID, placeholder)
		_ = conn.Close(websocket.StatusPolicyViolation, "unsupported ingress protocol")
		return
	}
	// Same unlimited read as every tunnel end: frames follow the relayed
	// stream's size, and the 32 KiB default would kill the session on the
	// first large one.
	conn.SetReadLimit(-1)

	var tunnelConn tunnel.Conn = tunnelWSConn{conn}
	var lanes *tunnel.MultiLaneConn
	if conn.Subprotocol() == tunnel.IngressWebSocketV2 {
		keyHash, keyErr := decodeAttachKey(r.Header.Get(tunnel.IngressAttachKeyHeader))
		if keyErr != nil {
			ig.release(endpointUUID, placeholder)
			_ = conn.Close(websocket.StatusPolicyViolation, "invalid ingress attach key")
			return
		}
		lanes = tunnel.NewMultiLaneConn(tunnelConn, nil, 4)
		tunnelConn = lanes
		ig.mu.Lock()
		placeholder.attachKey = keyHash
		placeholder.wsLanes = lanes
		ig.mu.Unlock()
	}

	transport := "websocket"
	streamOpts := tunnel.Options{
		IdleTimeout:        tunnel.DefaultIdleTimeout,
		MaxDuration:        ingressMaxDuration,
		MaxStreams:         ingressMaxActiveStreams,
		MaxPendingStreams:  ingressMaxQueuedStreams,
		StreamQueueTimeout: ingressStreamQueueWait,
		OnStreamWait: func(wait time.Duration, err error) {
			ig.metrics.recordQueueWait(r.Context(), transport, wait, err)
		},
		Cancel: placeholder.cancel,
		// The agent holds no session row, so it has nothing to discover and
		// nothing to name: the empty reason is "still attached", and this beat
		// only ever reports that the endpoint is alive.
		OnHeartbeat: func(context.Context) tunnel.EndReason {
			ig.notify(Observation{Type: "ingress_alive", At: time.Now(), ResourceUUID: expect.sessionUUID})
			return ""
		},
	}
	origin := tunnel.NewOriginWithOptions(tunnelConn, streamOpts)
	relay := ig.newRelay(origin, transport)
	ig.mu.Lock()
	placeholder.origin = origin
	placeholder.transport = transport
	placeholder.relay = relay
	ig.mu.Unlock()

	ig.metrics.recordSessionStart(r.Context(), transport)
	ig.notify(Observation{Type: "ingress_claimed", At: time.Now(), ResourceUUID: expect.sessionUUID})
	ig.Logger.Info("ingress: laptop attached", "endpoint", endpointUUID, "session", expect.sessionUUID, "transport", placeholder.transport)

	reason := origin.Run(r.Context(), streamOpts)
	ig.metrics.recordSessionEnd(context.WithoutCancel(r.Context()), transport, reason)

	if lanes != nil {
		_ = lanes.Close()
	}
	ig.release(endpointUUID, placeholder)
	ig.notify(Observation{Type: "ingress_closed", At: time.Now(), ResourceUUID: expect.sessionUUID, State: string(reason)})
	ig.Logger.Info("ingress: tunnel closed", "endpoint", endpointUUID, "session", expect.sessionUUID, "reason", string(reason))
	// The reason travels in the close frame's Reason field (ADR-045's
	// discipline): the CLI decides re-dial vs exit on it.
	_ = conn.Close(websocket.StatusNormalClosure, string(reason))
}

// attachWebSocketLane joins one authenticated secondary socket to a v2
// fallback session. It does not consume a mint token: the primary already did
// that, and the ephemeral attach key binds this lane to that exact session.
func (ig *Ingress) attachWebSocketLane(w http.ResponseWriter, r *http.Request, endpointUUID string) {
	lane64, err := strconv.ParseUint(r.Header.Get(tunnel.IngressLaneHeader), 10, 8)
	if err != nil || lane64 < 1 || lane64 > 3 {
		http.Error(w, "invalid ingress WebSocket lane", http.StatusBadRequest)
		return
	}
	keyHash, err := decodeAttachKey(r.Header.Get(tunnel.IngressAttachKeyHeader))
	if err != nil {
		http.Error(w, "invalid ingress attach key", http.StatusUnauthorized)
		return
	}
	sessionUUID := r.Header.Get(tunnel.IngressSessionHeader)
	ig.mu.Lock()
	session := ig.live[endpointUUID]
	valid := session != nil && session.wsLanes != nil && session.sessionUUID == sessionUUID &&
		subtle.ConstantTimeCompare(session.attachKey[:], keyHash[:]) == 1
	var lanes *tunnel.MultiLaneConn
	if valid {
		lanes = session.wsLanes
	}
	ig.mu.Unlock()
	if !valid {
		http.Error(w, "unknown ingress WebSocket session", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{tunnel.IngressWebSocketV2}})
	if err != nil {
		return
	}
	closeLane := func() error { return conn.Close(websocket.StatusNormalClosure, "") }
	if conn.Subprotocol() != tunnel.IngressWebSocketV2 {
		_ = conn.Close(websocket.StatusPolicyViolation, "unsupported ingress protocol")
		return
	}
	if err := lanes.AddLane(int(lane64), tunnelWSConn{conn}, closeLane); err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	select {
	case <-lanes.Done():
	case <-r.Context().Done():
	}
}

func decodeAttachKey(value string) ([sha256.Size]byte, error) {
	var hash [sha256.Size]byte
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != sha256.Size {
		return hash, errors.New("attach key must be 256-bit base64url")
	}
	// Hash the key immediately; plaintext lives only in the request headers.
	hash = sha256.Sum256(raw)
	return hash, nil
}

func (ig *Ingress) attachHTTPControl(w http.ResponseWriter, r *http.Request, endpointUUID string) {
	if r.Header.Get(tunnel.IngressProtocolHeader) != tunnel.IngressHTTPProtocol {
		http.Error(w, "unsupported ingress protocol", http.StatusUpgradeRequired)
		return
	}
	keyHash, err := decodeAttachKey(r.Header.Get(tunnel.IngressAttachKeyHeader))
	if err != nil {
		http.Error(w, "invalid ingress attach key", http.StatusBadRequest)
		return
	}
	expect, session, claimErr := ig.claimAttach(r.URL.Query().Get("token"), endpointUUID)
	if claimErr != nil {
		http.Error(w, claimErr.message, claimErr.status)
		return
	}
	if err := enableHTTPFullDuplex(w, r); err != nil {
		ig.release(endpointUUID, session)
		http.Error(w, "full-duplex HTTP streaming is unavailable", http.StatusHTTPVersionNotSupported)
		return
	}

	w.Header().Set("Content-Type", tunnel.IngressControlContentType)
	w.Header().Set(tunnel.IngressProtocolHeader, tunnel.IngressHTTPProtocol)
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		ig.release(endpointUUID, session)
		return
	}
	control := tunnel.NewLineControl(r.Body, responseWriteCloser{Writer: w}, controller.Flush, r.Body.Close)
	cancelCh := session.cancel
	transport := normalizedIngressTransport(r.Header.Get(tunnel.IngressTransportHeader))
	streamOpts := tunnel.Options{
		IdleTimeout:        tunnel.DefaultIdleTimeout,
		MaxDuration:        ingressMaxDuration,
		MaxStreams:         ingressMaxActiveStreams,
		MaxPendingStreams:  ingressMaxQueuedStreams,
		StreamQueueTimeout: ingressStreamQueueWait,
		OnStreamWait: func(wait time.Duration, err error) {
			ig.metrics.recordQueueWait(r.Context(), transport, wait, err)
		},
		Cancel: cancelCh,
		// The agent holds no session row, so it has nothing to discover and
		// nothing to name: the empty reason is "still attached", and this beat
		// only ever reports that the endpoint is alive.
		OnHeartbeat: func(context.Context) tunnel.EndReason {
			ig.notify(Observation{Type: "ingress_alive", At: time.Now(), ResourceUUID: expect.sessionUUID})
			return ""
		},
	}
	origin := tunnel.NewHTTPOrigin(control, streamOpts)
	relay := ig.newRelay(origin, transport)
	ig.mu.Lock()
	session.origin = origin
	session.httpOrigin = origin
	session.attachKey = keyHash
	session.transport = transport
	session.cancel = cancelCh
	session.relay = relay
	ig.mu.Unlock()

	ig.metrics.recordSessionStart(r.Context(), transport)
	ig.notify(Observation{Type: "ingress_claimed", At: time.Now(), ResourceUUID: expect.sessionUUID})
	ig.Logger.Info("ingress: laptop attached", "endpoint", endpointUUID, "session", expect.sessionUUID, "transport", session.transport)
	reason := origin.Run(r.Context(), streamOpts)
	ig.metrics.recordSessionEnd(context.WithoutCancel(r.Context()), transport, reason)
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Second)
	_ = origin.SendClose(closeCtx, reason)
	cancel()
	_ = origin.Close()
	ig.release(endpointUUID, session)
	ig.notify(Observation{Type: "ingress_closed", At: time.Now(), ResourceUUID: expect.sessionUUID, State: string(reason)})
	ig.Logger.Info("ingress: tunnel closed", "endpoint", endpointUUID, "session", expect.sessionUUID, "transport", session.transport, "reason", string(reason))
}

func normalizedIngressTransport(value string) string {
	switch value {
	case "h3", "h2":
		return value
	default:
		return "http"
	}
}

func enableHTTPFullDuplex(w http.ResponseWriter, r *http.Request) error {
	err := http.NewResponseController(w).EnableFullDuplex()
	if err != nil && r.ProtoMajor < 2 {
		return err
	}
	return nil
}

func (ig *Ingress) attachHTTPStream(w http.ResponseWriter, r *http.Request, endpointUUID string) {
	sessionUUID := r.Header.Get(tunnel.IngressSessionHeader)
	streamID64, err := strconv.ParseUint(r.Header.Get(tunnel.IngressStreamHeader), 10, 32)
	if err != nil || streamID64 == 0 {
		http.Error(w, "invalid ingress stream id", http.StatusBadRequest)
		return
	}
	keyHash, err := decodeAttachKey(r.Header.Get(tunnel.IngressAttachKeyHeader))
	if err != nil {
		http.Error(w, "invalid ingress attach key", http.StatusUnauthorized)
		return
	}
	ig.mu.Lock()
	session := ig.live[endpointUUID]
	valid := session != nil && session.httpOrigin != nil && session.sessionUUID == sessionUUID &&
		subtle.ConstantTimeCompare(session.attachKey[:], keyHash[:]) == 1
	ig.mu.Unlock()
	if !valid {
		http.Error(w, "unknown ingress HTTP session", http.StatusUnauthorized)
		return
	}
	id := uint32(streamID64)
	if !session.httpOrigin.WantsStream(id) {
		http.Error(w, "ingress stream is not pending", http.StatusConflict)
		return
	}
	if err := enableHTTPFullDuplex(w, r); err != nil {
		http.Error(w, "full-duplex HTTP streaming is unavailable", http.StatusHTTPVersionNotSupported)
		return
	}
	w.Header().Set("Content-Type", tunnel.IngressStreamContentType)
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		return
	}
	done := make(chan struct{})
	var doneOnce sync.Once
	conn := tunnel.NewDuplexConn(r.Body, responseWriteCloser{Writer: w}, controller.Flush, func() {
		doneOnce.Do(func() { close(done) })
	})
	if err := session.httpOrigin.AttachStream(id, conn); err != nil {
		_ = conn.Close()
		return
	}
	select {
	case <-done:
	case <-r.Context().Done():
		_ = conn.Close()
	}
}

type responseWriteCloser struct{ io.Writer }

func (responseWriteCloser) Close() error { return nil }

func (ig *Ingress) release(endpointUUID string, s *ingressSession) {
	ig.mu.Lock()
	if ig.live[endpointUUID] == s {
		delete(ig.live, endpointUUID)
	}
	ig.mu.Unlock()
}

func (ig *Ingress) notify(o Observation) {
	if ig.Notify != nil {
		ig.Notify(o)
	}
}

type ingressStreamOpener interface {
	OpenStream(context.Context) (net.Conn, error)
}

// newRelay owns one reverse proxy and one Transport per claimed endpoint
// session. Keeping upstream connections alive is therefore safe: an idle
// connection can be reused only by requests already routed to this exact
// endpoint and laptop, never by another ingressSession. Upgrades (WebSocket,
// SSE) retain their dedicated connection for their whole life.
func (ig *Ingress) newRelay(origin ingressStreamOpener, transports ...string) http.Handler {
	transportName := "unknown"
	if len(transports) > 0 && transports[0] != "" {
		transportName = transports[0]
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			started := time.Now()
			conn, err := origin.OpenStream(ctx)
			ig.metrics.recordStreamOpen(ctx, transportName, time.Since(started), err)
			if err != nil {
				return nil, err
			}
			return ig.metrics.wrapStream(ctx, transportName, conn), nil
		},
		// Admission belongs to Origin: it owns the 128 active + 512 pending
		// bound and can return the explicit 30-second overload error. Capping
		// here would create an opaque, unbounded net/http wait in front of it.
		MaxIdleConns:        ingressMaxActiveStreams,
		MaxIdleConnsPerHost: ingressMaxActiveStreams,
		// An idle pooled connection is a tunnel stream still open, and on a
		// QUIC transport that stream still holds one of the peer's finite
		// stream credits (ADR-061). Ninety seconds of hoarding after a page
		// load could starve the next one; half a minute keeps the reuse that
		// makes a page load cheap without pinning capacity nobody is using.
		IdleConnTimeout: 30 * time.Second,
		// SSE and long-polls must not buffer server-side.
		ResponseHeaderTimeout: 0,
	}
	return &httputil.ReverseProxy{
		Rewrite: func(req *httputil.ProxyRequest) {
			// The dial ignores the address; Out.Host is left as the public
			// host, so the developer's app sees the real URL it is served on.
			req.Out.URL.Scheme = "http"
			req.Out.URL.Host = "ingress"
			req.Out.Host = req.In.Host
		},
		Transport: transport,
		// Zero lets net/http coalesce bulk responses; ReverseProxy still
		// switches to immediate flushing automatically for streaming bodies.
		FlushInterval: 0,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			ig.Logger.Warn("ingress: relay failed", "host", hostname(r.Host), "error", err)
			if errors.Is(err, tunnel.ErrOriginQueueFull) || errors.Is(err, tunnel.ErrOriginQueueTimeout) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "the ingress tunnel is busy — retry shortly", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "the developer's machine did not answer", http.StatusBadGateway)
		},
	}
}

// serveOfflinePage answers a visitor when no laptop is attached: the stable
// URL exists (certificate included) but its target is not there right now.
// Same self-contained style as the waking page; noindex is already stamped by
// the proxy middleware.
func (ig *Ingress) serveOfflinePage(w http.ResponseWriter, r *http.Request, host string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method == http.MethodHead {
		return
	}
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">")
	fmt.Fprintf(&b, "<title>Tunnel offline — %s</title>", htmlEscape(host))
	b.WriteString("<style>body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;" +
		"background:#101014;color:#e6e6ea;font:15px/1.5 system-ui,sans-serif}" +
		"main{max-width:26rem;padding:2rem}" +
		"h1{font-size:1.15rem;font-weight:600;margin:0 0 .35rem}" +
		"p{margin:.25rem 0;color:#9a9aa5}</style></head><body><main>")
	fmt.Fprintf(&b, "<h1>%s is offline</h1>", htmlEscape(host))
	b.WriteString("<p>This URL relays to a developer's machine, and no machine is connected right now.</p>")
	b.WriteString("</main></body></html>")
	_, _ = w.Write([]byte(b.String()))
}

// tunnelWSConn adapts coder/websocket to tunnel.Conn (the handlers' adapter,
// agent-side).
type tunnelWSConn struct{ c *websocket.Conn }

func (t tunnelWSConn) Read(ctx context.Context) (tunnel.MessageType, []byte, error) {
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

func (t tunnelWSConn) Write(ctx context.Context, typ tunnel.MessageType, data []byte) error {
	kind := websocket.MessageBinary
	if typ == tunnel.MessageText {
		kind = websocket.MessageText
	}
	return t.c.Write(ctx, kind, data)
}

func (t tunnelWSConn) Ping(ctx context.Context) error { return t.c.Ping(ctx) }
