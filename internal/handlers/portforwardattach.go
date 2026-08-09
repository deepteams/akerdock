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
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/sshexec"
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
	// openBudget is what one data stream may spend getting served. It is the
	// ordinary dial budget, widened by ADR-067's ceiling when this session was
	// minted over a wake: the first connection is supposed to WAIT for the cold
	// start (§3), and refusing it after five seconds would report a transport
	// fault for a platform doing exactly what it promised.
	openBudget time.Duration
}

// budget reads a zero openBudget as "unset" and falls back to the ordinary one
// — the same reading sessionBounds gives a zero MaxDuration. An attach built
// without the field must serve its streams, not hand them an already-expired
// deadline.
func (e *egressAttach) budget() time.Duration {
	if e.openBudget <= 0 {
		return wakeStreamBudget(nil)
	}
	return e.openBudget
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

// egressResolution is the remote half of an attach (ADR-066 §2): the agent
// ContainerInspect and the SSH handshake, started once the response head is out
// and awaited by whoever needs the result. It is resolved exactly once and owned
// by the session request, which releases it on the way out — ownership did not
// move when the moment of acquisition did.
//
// The late-arrival discipline is the part that earns its complexity, and it is
// the same one dialTCPContext applies one level down: a resolution that
// completes AFTER the session tore down closes what it produced itself.
// Without it, a session abandoned during a 30-second handshake leaks one SSH
// client — a real cost, since ssh_timeout_seconds is the bound and 30 s is its
// default.
type egressResolution struct {
	// done is closed once the result is recorded, so a data stream that arrives
	// mid-resolution waits for it rather than being refused.
	done chan struct{}

	mu       sync.Mutex
	client   *sshexec.Client
	addr     string
	msg      string
	released bool
}

// start runs resolve in the background under ctx, so a session that ends
// mid-dial cancels the dial. The bounds are the ones each leg already carries —
// the agent RPC's own timeout and the server's ssh_timeout_seconds — and there
// is deliberately no new tunable on top of them.
func (p *egressResolution) start(ctx context.Context, resolve func(context.Context) (*sshexec.Client, string, string)) {
	// `go p.settle(resolve(ctx))` would evaluate resolve in THIS goroutine and
	// hand its result to a goroutine that has nothing left to do — the whole
	// handshake back in front of the session, one line after it was moved behind
	// the head.
	go func() { p.settle(resolve(ctx)) }()
}

func (p *egressResolution) settle(client *sshexec.Client, addr, msg string) {
	p.mu.Lock()
	if p.released {
		p.mu.Unlock()
		if client != nil {
			_ = client.Close()
		}
		return
	}
	p.client, p.addr, p.msg = client, addr, msg
	p.mu.Unlock()
	close(p.done)
}

// release closes whatever the resolution established, exactly once. A
// resolution still in flight is marked so that its own goroutine closes what it
// finally produces.
func (p *egressResolution) release() {
	p.mu.Lock()
	p.released = true
	client := p.client
	p.client = nil
	p.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

// failure is the resolver's sentence, or empty while it is still running or if
// it succeeded. It is what the session's close frame carries beside
// target_unreachable.
func (p *egressResolution) failure() string {
	select {
	case <-p.done:
	default:
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.msg
}

// dial is what a data stream does instead of dialing directly: await the
// resolution, then open the TCP channel on it. Both legs run inside the ONE
// EgressDialTimeout context the stream already builds (ADR-066 §3), so a stream
// that arrives mid-resolution waits and is then served, and it cannot hang
// longer than a stream that dials — which is what lets the client keep the
// margin it derives from that same constant.
func (p *egressResolution) dial(ctx context.Context) (net.Conn, error) {
	select {
	case <-p.done:
	case <-ctx.Done():
		return nil, &egressResolutionError{msg: "the target was still being resolved: " + ctx.Err().Error()}
	}
	p.mu.Lock()
	client, addr, msg := p.client, p.addr, p.msg
	p.mu.Unlock()
	if msg != "" {
		return nil, &egressResolutionError{msg: msg}
	}
	if client == nil {
		return nil, &egressResolutionError{msg: "the tunnel session has ended"}
	}
	return dialTCPContext(ctx, client, addr)
}

// egressResolutionError distinguishes "the target could not be resolved" from
// "the target refused the connection". Both are 502 target_unreachable — §3 adds
// no vocabulary — but only one of them has a target address to name, and a
// stream that waited out its budget must read as a dial that never completed
// rather than as a distinct failure.
type egressResolutionError struct{ msg string }

func (e *egressResolutionError) Error() string { return e.msg }

// egressUnreachableMessage phrases the 502 in the words of whatever actually
// failed: the resolver's sentence, or the dial's own first line.
func egressUnreachableMessage(err error) string {
	var unresolved *egressResolutionError
	if errors.As(err, &unresolved) {
		return "the target could not be reached: " + unresolved.msg
	}
	return "the target did not accept the connection: " + strings.SplitN(err.Error(), "\n", 2)[0]
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

// tunnelAttachSession claims the attach token and holds the session open for its
// whole life. The SSH client is used by every data stream, so it must outlive
// them all — which is exactly what this request's lifetime is for.
//
// What it does NOT do any more is dial before it answers (ADR-066). The head is
// produced from local state alone: the claim, the attach key, full duplex, and
// the store lookups that say WHAT the session names. Everything that crosses the
// network to a machine the control plane does not own starts in the background
// once the head is out, and whoever needs it awaits it. The old ordering was a
// stated principle — resolve before committing the response, because an HTTP
// error is diagnosable and a dead stream is a mystery — and it named two
// outcomes where there are three: a token spent on a response head the client
// stopped waiting for is neither, and the platform then reported a bad token for
// a handshake that was still in progress.
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
	row, err := a.claimPortForwardSession(r.Context(), token, key)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized,
			"invalid, expired or already used tunnel token")
		return
	}
	a.supersedePortForwardAttach(row)

	// The local half (ADR-066 §1): indexed reads on a pooled connection, whose
	// refusals are the actionable ones — the target server no longer exists, the
	// preview may have been destroyed. They keep their 409 and their prose, and
	// they finalize the row, because a re-claim would only reproduce the same
	// verdict (ADR-065 §6).
	target, errMsg := a.tunnelTargetSpec(r.Context(), row)
	if errMsg != "" {
		a.endPortForwardAttach(row, endReasonTargetUnreachable)
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, errMsg)
		return
	}
	if err := enableFullDuplex(w, r); err != nil {
		a.endPortForwardAttach(row, tunnel.EndDisconnect)
		httpapi.WriteError(w, r, http.StatusHTTPVersionNotSupported, "full_duplex_unavailable",
			"full-duplex HTTP streaming is unavailable on this connection")
		return
	}

	sessionUUID := uuidString(row.Uuid)
	// The wake this session was minted over, if any (ADR-067 §3). Nil is both
	// "nothing was asleep" and "this attach landed on another replica", and
	// every use below reads it as "there is nothing to wait for".
	wake := a.lookupWake(sessionUUID)
	bounds := sessionBounds(row)
	cancelBridge := a.Tunnels.register(row.ID)
	bounds.Cancel = cancelBridge
	bounds.OnHeartbeat = a.portForwardHeartbeat(row)
	defer a.Tunnels.unregister(row.ID, cancelBridge)

	controller := http.NewResponseController(w)
	control := tunnel.NewLineControl(r.Body, responseWriter{w}, controller.Flush, r.Body.Close)
	session := tunnel.NewHTTPSession(control, bounds)
	resolution := &egressResolution{done: make(chan struct{})}
	// The session request keeps ownership of whatever the resolution produced;
	// only the moment of acquisition moved. That is what keeps the SSH client
	// alive across every data stream.
	defer resolution.release()
	attach := &egressAttach{
		key:        key,
		dial:       resolution.dial,
		session:    session,
		openBudget: wakeStreamBudget(wake),
	}
	// Registered BEFORE the head is flushed: the CLI opens its data stream the
	// moment it reads that head, and a session not yet in the register would
	// answer it `unknown tunnel session`. The window is theoretical today — a WAN
	// round trip against a map insert — and answering earlier is exactly what
	// would widen it.
	a.egressRegister(sessionUUID, attach)
	defer a.egressRelease(sessionUUID, attach)

	w.Header().Set("Content-Type", tunnel.EgressHTTP.ControlContentType)
	w.Header().Set(tunnel.EgressHTTP.ProtocolHeader, tunnel.EgressHTTP.Name)
	// The CLI binds its data streams to this: the mint token was spent here,
	// and a stream presents the session and the attach key instead.
	w.Header().Set(tunnel.EgressHTTP.SessionHeader, sessionUUID)
	w.WriteHeader(http.StatusOK)
	if err := controller.Flush(); err != nil {
		// The head never reached the client: nothing was served, so this is an
		// abandonment and the row stays re-claimable (ADR-065 §6).
		a.endAbandonedPortForwardAttach(row)
		return
	}

	// Eagerly, not on first demand (ADR-066 §2): between the CLI's `forwarding
	// …` line and the first local connection sits a human typing `psql`, and
	// that dead time is what the handshake should be spending rather than the
	// connection. It runs under this request's context, so a session that ends
	// mid-dial cancels the dial.
	//
	// A session minted over a wake says so on the wire it already has, before
	// the resolution starts: a minute of apparent silence reads as the bug this
	// whole investigation started from (ADR-067 §6).
	announceWake(r.Context(), control, wake)
	resolution.start(r.Context(), func(ctx context.Context) (*sshexec.Client, string, string) {
		return a.resolveTunnelTargetAfterWake(ctx, wake, target)
	})
	go a.cutOnFailedResolution(row, session, resolution, wake)

	reason := session.Run(r.Context(), bounds)
	// A session cut by its grant running out is neither an idle timeout nor a
	// revocation, and the CLI says exactly that to the developer (ADR-045 §5).
	if reason == tunnel.EndMaxDuration && row.GrantID != nil {
		reason = endReasonGrantExpired
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Second)
	// A failure belonging to no particular stream — the agent channel is down,
	// SSH refused, the container is not running — is reported here, on the
	// session, before it closes. It is the only way the developer learns why a
	// tunnel they were not using disappeared (ADR-045 §5), and the sentence
	// travels beside the reason so it names what was unreachable. Only that
	// reason carries it: a session the developer closed themselves does not need
	// to be told about a handshake that failed on the way out.
	var msg string
	if reason == endReasonTargetUnreachable || reason == endReasonWakeFailed {
		msg = resolution.failure()
	}
	_ = session.SendClose(closeCtx, reason, msg)
	cancel()
	_ = session.Close()
	if reason == tunnel.EndDisconnect && r.Context().Err() != nil {
		a.endAbandonedPortForwardAttach(row)
		return
	}
	a.endPortForwardAttach(row, reason)
}

// cutOnFailedResolution ends the session when its target turns out to be
// unreachable. Before ADR-066 this was a 409 the CLI printed as
// `cannot open tunnel: <sentence>`; now the session is already open, so the
// verdict travels the channel a revocation uses — which is what puts the reason
// and the sentence on the session's close frame rather than leaving a listener
// forwarding to nothing.
func (a *API) cutOnFailedResolution(row store.PortForwardSession, session *tunnel.HTTPSession, resolution *egressResolution, wake *sessionWake) {
	select {
	case <-resolution.done:
	case <-session.Done():
		return
	}
	if msg := resolution.failure(); msg != "" {
		// A target that never came up is a wake failure and not an unreachable
		// one, and the developer reads the difference: one says "check the
		// server, its agent and the container", the other says "it was asleep
		// and could not be started" (ADR-067 §6).
		reason := sessionEndReason(wake)
		a.Tunnels.Cut(row.ID, reason)
		a.Logger.Info("port-forward target never came up, session cut",
			"session", uuidString(row.Uuid), "target", row.TargetName,
			"reason", string(reason), "detail", msg)
	}
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

	// Bounded, and bounded SHORTER than the CLI's own open budget — which is
	// why the bound is tunnel.EgressDialTimeout, the value both sides derive
	// from. This dial happens BEFORE the response head, so an unbounded one —
	// ssh.Client.Dial takes no context — spends the client's patience and
	// surfaces there as a transport timeout, blaming the tunnel for a target
	// that never answered. A 502 carrying the dial's own words is what makes it
	// diagnosable.
	//
	// Since ADR-066 this budget also covers the wait for the session's
	// resolution, when a stream arrives while it is still in flight. One context
	// for both legs is what keeps that wait bounded by exactly the deadline the
	// stream would otherwise have spent dialing — widened, and only then, by
	// ADR-067's ceiling when the session is waiting on a cold start.
	dialCtx, cancelDial := context.WithTimeout(r.Context(), attach.budget())
	target, err := attach.dial(dialCtx)
	cancelDial()
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadGateway, "target_unreachable", egressUnreachableMessage(err))
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

// tcpDialer is the one method this file needs of an SSH client. It is an
// interface so the abandonment path below can be exercised without a server:
// what it does with a connection that arrives after the caller gave up is the
// difference between a bounded dial and a channel leak per attempt.
type tcpDialer interface {
	DialTCP(addr string) (net.Conn, error)
}

var _ tcpDialer = (*sshexec.Client)(nil)

// dialTCPContext gives ssh.Client.Dial the context it does not take. A hung
// dial is abandoned by the caller, and the goroutine closes whatever arrives
// late so a slow target cannot leak one channel per attempt.
func dialTCPContext(ctx context.Context, client tcpDialer, addr string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := client.DialTCP(addr)
		select {
		case done <- result{conn: conn, err: err}:
		default:
			if conn != nil {
				_ = conn.Close()
			}
		}
	}()
	select {
	case got := <-done:
		return got.conn, got.err
	case <-ctx.Done():
		return nil, fmt.Errorf("dial %s: %w", addr, ctx.Err())
	}
}
