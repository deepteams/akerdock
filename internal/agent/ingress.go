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
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
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

// ingressMaxDuration is the session ceiling (ADR-060 §6): ADR-032's 4 h was
// calibrated on interactive egress sessions; an ingress tunnel's typical day
// is a webhook target exercised irregularly until evening. 12 h still
// guarantees nothing survives an unattended night.
const ingressMaxDuration = 12 * time.Hour

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
	origin       *tunnel.Origin
	cancel       chan tunnel.EndReason
	relay        http.Handler
}

// Ingress relays visitor traffic for declared ingress endpoints to the laptop
// currently attached. Its lifetime is the process — a routing-table reload
// must never drop a live tunnel — so Serve constructs one and updates its
// host table on each reload.
type Ingress struct {
	Logger *slog.Logger
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
	for _, s := range ig.live {
		if s.sessionUUID == sessionUUID {
			target = s
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
		case target.cancel <- r:
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
	if ok {
		session = ig.live[endpointUUID]
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
	if session == nil {
		ig.serveOfflinePage(w, r, host)
		return
	}
	session.relay.ServeHTTP(w, r)
}

// attach redeems the single-use token and becomes the endpoint's live tunnel.
func (ig *Ingress) attach(w http.ResponseWriter, r *http.Request, endpointUUID string) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing attach token", http.StatusUnauthorized)
		return
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
		http.Error(w, "invalid attach token", http.StatusUnauthorized)
		return
	case expect.expires.Before(time.Now()):
		ig.mu.Unlock()
		http.Error(w, "attach token expired — mint a new session", http.StatusUnauthorized)
		return
	case ig.live[endpointUUID] != nil:
		ig.mu.Unlock()
		http.Error(w, "endpoint occupied — one laptop per endpoint", http.StatusConflict)
		return
	}
	// Reserve the slot before releasing the lock so two racing attaches
	// cannot both pass the occupancy check.
	placeholder := &ingressSession{sessionUUID: expect.sessionUUID, endpointUUID: endpointUUID}
	ig.live[endpointUUID] = placeholder
	ig.mu.Unlock()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{IngressSubprotocol}})
	if err != nil {
		ig.release(endpointUUID, placeholder)
		return
	}
	// Same unlimited read as every tunnel end: frames follow the relayed
	// stream's size, and the 32 KiB default would kill the session on the
	// first large one.
	conn.SetReadLimit(-1)

	origin := tunnel.NewOrigin(tunnelWSConn{conn})
	placeholder.origin = origin
	placeholder.cancel = make(chan tunnel.EndReason, 1)
	placeholder.relay = ig.newRelay(origin)

	ig.notify(Observation{Type: "ingress_claimed", At: time.Now(), ResourceUUID: expect.sessionUUID})
	ig.Logger.Info("ingress: laptop attached", "endpoint", endpointUUID, "session", expect.sessionUUID)

	reason := origin.Run(r.Context(), tunnel.Options{
		IdleTimeout: tunnel.DefaultIdleTimeout,
		MaxDuration: ingressMaxDuration,
		Cancel:      placeholder.cancel,
		OnHeartbeat: func(context.Context) bool {
			ig.notify(Observation{Type: "ingress_alive", At: time.Now(), ResourceUUID: expect.sessionUUID})
			return true
		},
	})

	ig.release(endpointUUID, placeholder)
	ig.notify(Observation{Type: "ingress_closed", At: time.Now(), ResourceUUID: expect.sessionUUID, State: string(reason)})
	ig.Logger.Info("ingress: tunnel closed", "endpoint", endpointUUID, "session", expect.sessionUUID, "reason", string(reason))
	// The reason travels in the close frame's Reason field (ADR-045's
	// discipline): the CLI decides re-dial vs exit on it.
	_ = conn.Close(websocket.StatusNormalClosure, string(reason))
}

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

// newRelay builds the reverse proxy that carries one visitor request per mux
// stream. Keep-alives are OFF by design: Traefik pools its connections to
// this front across routers, so a spliced or reused backend connection could
// carry an unrelated resource's request to the laptop. One stream per request
// keeps every byte attributable; upgrades (WebSocket, SSE) ride the same
// stream for their whole life — httputil.ReverseProxy handles the switch and
// splices both directions itself.
func (ig *Ingress) newRelay(origin *tunnel.Origin) http.Handler {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return origin.OpenStream(ctx)
		},
		DisableKeepAlives: true,
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
		Transport:     transport,
		FlushInterval: 100 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			ig.Logger.Warn("ingress: relay failed", "host", hostname(r.Host), "error", err)
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
