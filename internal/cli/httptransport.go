// HTTP attach transport, shared by every CLI-to-server tunnel (ADR-061's
// ladder, generalised). Nothing here knows what the streams carry: it picks a
// transport the network can actually carry, keeps enough connections open to
// hold what the peer will admit, opens full-duplex requests on them, and
// remembers what failed so the next attempt does not re-learn it.
//
// The wire identifiers are NOT shared — they arrive as a tun.HTTPAttachProtocol
// per access path, which is ADR-027's rule and the reason a token minted for
// one path cannot be redeemed on another.

package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"

	tun "github.com/deepteams/akerdock/internal/tunnel"
)

type transportKind string

const (
	transportH3 transportKind = "h3"
	transportH2 transportKind = "h2"
	transportWS transportKind = "websocket"

	transportProbeTimeout = 3 * time.Second

	// transportAttachTimeout is the session-attach budget, and since ADR-066 it
	// is one constant for every access path again. A session attach now answers
	// on what the control plane holds locally — the token claim and a handful of
	// indexed reads — because everything that crosses the network to another
	// machine (the agent RPC, the SSH handshake, the PTY, and under ADR-067 the
	// wake) runs BEHIND the response head. So this is no longer a bet on how
	// long someone else's SSH handshake takes: a peer that has not answered in
	// 5 s is not busy resolving, it is stalled, which is the one thing this
	// timer exists to detect.
	transportAttachTimeout = 5 * time.Second

	// transportHTTP2Lanes and transportHTTP3Lanes exist for the same reason: a
	// transport must be able to carry what the peer's admission accepts. A QUIC
	// peer advertises how many streams it will accept at once, and quic-go —
	// which the managed Traefik runs — defaults to 100 (its
	// protocol.DefaultMaxIncomingStreams); an HTTP/2 peer typically allows 250.
	// Past that ceiling an open does not fail, it BLOCKS on stream credit, so
	// the far side waits out its own open timeout and answers an error having
	// relayed nothing. Four connections keep both comfortably above the largest
	// admission bound in the product (tunnel.IngressMaxStreams).
	transportHTTP2Lanes = 4
	transportHTTP3Lanes = 4

	// The data-stream open budget bounds the wait for a stream's response
	// headers — not the stream's life, which belongs to what it carries.
	// Refusing an unanswered open turns a silent stall into an answer the
	// caller gets at once.
	//
	// It is per access path and not one shared constant, because what happens
	// before the response head differs per path and that is the whole of what
	// this waits for. On ingress the peer only has to admit the stream; on
	// egress the control plane must first dial the target THROUGH SSH, and it
	// bounds that dial at tun.EgressDialTimeout. A client budget shorter than
	// the server's dial makes the two deadlines race, and when the client wins
	// it blames the transport for a target that never answered — so the egress
	// budget is that dial plus the room a WAN round trip and the SSH channel
	// setup need.
	ingressDataOpenTimeout  = 5 * time.Second
	terminalDataOpenTimeout = 5 * time.Second
	egressDataOpenTimeout   = tun.EgressDialTimeout + 5*time.Second

	// egressWakeOpenTimeout replaces the budget above for as long as the server
	// says the target is cold-starting (ADR-067 §6's `waking` frame). The
	// platform's promise there is that the local accept() succeeds at once and
	// the operation behind it waits — up to §5's ceiling of 75 s — so a client
	// budget of ten seconds would refuse the very first connection while the
	// server was doing exactly what it announced. Same reasoning as the line
	// above, one order of magnitude out: the client's budget must outlast the
	// server's, or the two deadlines race and the loser blames the transport.
	//
	// The 75 s is stated on both sides (sessionWakeCeiling,
	// internal/handlers/sessionwake.go) because the two processes are versioned
	// independently; the frame is what synchronises them in practice, not the
	// constant.
	egressWakeOpenTimeout = 75*time.Second + egressDataOpenTimeout

	// transportFailureBudget is how many consecutive lost sessions a transport
	// gets before the CLI falls back to the next one. A transport whose every
	// attach succeeds and whose every session then dies is not a transport this
	// path can carry: an HTTP front that bounds how long a request may last
	// (Traefik's respondingTimeouts.readTimeout, 60 s by default) cuts the
	// request on a perfectly regular schedule. WebSocket transports are immune —
	// a hijacked connection leaves the server's read deadline behind — so the
	// fallback is the answer, not an eleventh re-dial.
	transportFailureBudget = 3

	// transportProbeCooldown is how long a failed capability probe is
	// remembered. A network that blocks UDP fails the HTTP/3 probe on every
	// single reconnect, and paying a QUIC handshake timeout each time is pure
	// downtime; a cooldown rather than a permanent verdict because a laptop
	// that changes network may well gain QUIC.
	transportProbeCooldown = 5 * time.Minute
)

func (k transportKind) label() string {
	switch k {
	case transportH3:
		return "HTTP/3 (QUIC)"
	case transportH2:
		return "HTTP/2"
	default:
		return "WebSocket"
	}
}

// transportPreference is the ladder, strongest first.
func transportPreference() [3]transportKind {
	return [3]transportKind{transportH3, transportH2, transportWS}
}

// transportState is the per-process memory of what this network can actually
// carry. It outlives one session on purpose: a reconnect loop would otherwise
// re-learn the same verdict every minute.
type transportState struct {
	now         func() time.Time
	disabled    map[transportKind]bool
	probeFailed map[transportKind]time.Time
	failures    map[transportKind]int
	announced   transportKind
}

func newTransportState() *transportState {
	return &transportState{
		now:         time.Now,
		disabled:    make(map[transportKind]bool),
		probeFailed: make(map[transportKind]time.Time),
		failures:    make(map[transportKind]int),
	}
}

// usable reports whether kind is worth another attempt right now.
func (s *transportState) usable(kind transportKind) bool {
	if s.disabled[kind] {
		return false
	}
	failed, ok := s.probeFailed[kind]
	return !ok || s.now().Sub(failed) >= transportProbeCooldown
}

// noteProbeFailure remembers that kind could not even be negotiated.
func (s *transportState) noteProbeFailure(kind transportKind) {
	s.probeFailed[kind] = s.now()
}

// disable retires a transport for the rest of the process.
func (s *transportState) disable(kind transportKind) { s.disabled[kind] = true }

// noteFailure charges one lost attempt to kind's budget and returns the
// operator-facing diagnosis when the budget runs out. lifetime is how long the
// attempt held, which is the single most diagnostic number here: a tunnel that
// dies after the same suspiciously round duration, over and over, is a tunnel
// someone is cutting on a timer.
func (s *transportState) noteFailure(kind transportKind, lifetime time.Duration) string {
	s.failures[kind]++
	if s.failures[kind] < transportFailureBudget {
		return ""
	}
	s.disable(kind)
	return fmt.Sprintf(
		"%s keeps dropping (%d attempts in a row, the last after %s) — falling back to the next transport.\n"+
			"Something on the path cuts long-lived HTTP requests. On the AkerDock proxy that setting is\n"+
			"entryPoints.websecure.transport.respondingTimeouts.readTimeout (Traefik defaults to 60s; it must be 0s);\n"+
			"any HTTP proxy in front of it needs the same treatment.",
		kind.label(), s.failures[kind], lifetime.Round(time.Second))
}

// noteSuccess clears the budget: whatever happened before, this transport did
// carry the tunnel, so the next drop starts from zero rather than inheriting a
// count from an outage half a day ago.
func (s *transportState) noteSuccess(kind transportKind) { delete(s.failures, kind) }

// attachRejection is an attach the server refused with an HTTP status. It is a
// policy or authentication verdict — an expired mint, a session already
// occupied — and says nothing about the transport, except for the two statuses
// that do.
type attachRejection struct {
	kind    transportKind
	status  string
	code    int
	message string
}

func (e *attachRejection) Error() string {
	return fmt.Sprintf("%s attach returned %s: %s", e.kind.label(), e.status, e.message)
}

// transportRefused reports whether the peer answered "not over this protocol",
// which is the only rejection worth retiring the transport for.
func (e *attachRejection) transportRefused() bool {
	return e.code == http.StatusUpgradeRequired || e.code == http.StatusHTTPVersionNotSupported
}

// sessionEnd is the mirror of attachRejection: not why a session never opened,
// but why one that did has stopped. It is shared by the egress and terminal
// paths because their end reasons are one enum on the server, and since ADR-066
// they also share the reason a session can now end for something that would
// once have been refused at open — a target that was never reached.
//
// The reason is the persisted value; the message is the operator-facing sentence
// the server sends beside it when it has one. It carries what the reason cannot:
// a reason names a category, and the sentence names which machine, on a failure
// whose cause is by definition not on this one.
type sessionEnd struct {
	reason  string
	message string
}

type httpLane struct {
	roundTripper http.RoundTripper
	close        func() error
	active       atomic.Int64
}

// httpLanePool spreads streams over several physical connections and tracks
// how loaded each one is.
type httpLanePool struct {
	lanes []*httpLane
}

func newH3Pool() *httpLanePool { return newH3PoolWithTLS(nil) }

func newH3PoolWithTLS(tlsConfig *tls.Config) *httpLanePool {
	pool := &httpLanePool{lanes: make([]*httpLane, 0, transportHTTP3Lanes)}
	for range transportHTTP3Lanes {
		transport := &http3.Transport{
			TLSClientConfig: tlsConfig,
			QUICConfig: &quic.Config{
				HandshakeIdleTimeout: transportProbeTimeout,
				MaxIdleTimeout:       90 * time.Second,
				KeepAlivePeriod:      20 * time.Second,
			},
		}
		pool.lanes = append(pool.lanes, &httpLane{roundTripper: transport, close: transport.Close})
	}
	return pool
}

func newH2Pool() *httpLanePool { return newH2PoolWithTLS(nil) }

func newH2PoolWithTLS(tlsConfig *tls.Config) *httpLanePool {
	pool := &httpLanePool{lanes: make([]*httpLane, 0, transportHTTP2Lanes)}
	for range transportHTTP2Lanes {
		laneTLS := tlsConfig
		if laneTLS == nil {
			laneTLS = &tls.Config{MinVersion: tls.VersionTLS12}
		} else {
			laneTLS = laneTLS.Clone()
			if laneTLS.MinVersion == 0 {
				laneTLS.MinVersion = tls.VersionTLS12
			}
		}
		transport := &http2.Transport{
			TLSClientConfig:            laneTLS,
			StrictMaxConcurrentStreams: true,
			IdleConnTimeout:            90 * time.Second,
			ReadIdleTimeout:            30 * time.Second,
			PingTimeout:                10 * time.Second,
			WriteByteTimeout:           30 * time.Second,
		}
		pool.lanes = append(pool.lanes, &httpLane{
			roundTripper: transport,
			close: func() error {
				transport.CloseIdleConnections()
				return nil
			},
		})
	}
	return pool
}

// newPool builds the pool of the given HTTP transport; WebSocket has none.
func newPool(kind transportKind) *httpLanePool {
	if kind == transportH3 {
		return newH3Pool()
	}
	return newH2Pool()
}

func (p *httpLanePool) RoundTrip(req *http.Request) (*http.Response, error) {
	return p.roundTripOn(p.leastLoaded(), req)
}

func (p *httpLanePool) roundTripOn(index int, req *http.Request) (*http.Response, error) {
	lane := p.lanes[index]
	lane.active.Add(1)
	resp, err := lane.roundTripper.RoundTrip(req)
	if err != nil {
		lane.active.Add(-1)
		return nil, err
	}
	resp.Body = &laneResponseBody{ReadCloser: resp.Body, release: func() { lane.active.Add(-1) }}
	return resp, nil
}

func (p *httpLanePool) leastLoaded() int {
	best := 0
	load := p.lanes[0].active.Load()
	for i := 1; i < len(p.lanes); i++ {
		if candidate := p.lanes[i].active.Load(); candidate < load {
			best, load = i, candidate
		}
	}
	return best
}

func (p *httpLanePool) Close() error {
	var result error
	for _, lane := range p.lanes {
		if err := lane.close(); result == nil && err != nil {
			result = err
		}
	}
	return result
}

type laneResponseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *laneResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

// attachURL normalizes a minted attach URL to its HTTP form and strips the
// token: a capability probe must not spend a single-use secret.
func attachURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid attach URL: %w", err)
	}
	switch parsed.Scheme {
	case "wss":
		parsed.Scheme = "https"
	case "ws":
		parsed.Scheme = "http"
	case "https", "http":
	default:
		return nil, fmt.Errorf("unsupported attach URL scheme %q", parsed.Scheme)
	}
	query := parsed.Query()
	query.Del("token")
	parsed.RawQuery = query.Encode()
	return parsed, nil
}

// withToken returns the attach URL carrying the authoritative token.
func withToken(attach *url.URL, token string) (*url.URL, error) {
	carried := *attach
	query := carried.Query()
	if token != "" {
		query.Set("token", token)
	}
	if query.Get("token") == "" {
		return nil, errors.New("the mint response has no attach token")
	}
	carried.RawQuery = query.Encode()
	return &carried, nil
}

// probeAttach asks the peer, without spending a token, whether it speaks this
// protocol AND whether the negotiated transport is the one we intended: a
// front that silently downgraded HTTP/3 to HTTP/2 must not be mistaken for a
// working HTTP/3 path.
func probeAttach(ctx context.Context, pool *httpLanePool, attach *url.URL, kind transportKind, proto tun.HTTPAttachProtocol) error {
	probeCtx, cancel := context.WithTimeout(ctx, transportProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodOptions, attach.String(), nil)
	if err != nil {
		return err
	}
	resp, err := pool.roundTripOn(0, req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("capability probe returned %s", resp.Status)
	}
	capabilities := resp.Header.Get(proto.CapabilitiesHeader)
	if !strings.Contains(capabilities, proto.Name) || !strings.Contains(capabilities, string(kind)) {
		return fmt.Errorf("the peer does not advertise %s over %s", proto.Name, kind)
	}
	if kind == transportH3 && resp.ProtoMajor != 3 {
		return fmt.Errorf("HTTP/3 probe negotiated %s", resp.Proto)
	}
	if kind == transportH2 && resp.ProtoMajor != 2 {
		return fmt.Errorf("HTTP/2 probe negotiated %s", resp.Proto)
	}
	return nil
}

// attachStream is one full-duplex HTTP request: the request body is the write
// half and the response body the read half, both open for as long as whatever
// rides on them.
type attachStream struct {
	resp   *http.Response
	writer *io.PipeWriter
	cancel context.CancelFunc
}

// openAttachStream opens one such request. lane < 0 spreads over the pool;
// a fixed lane is for the request a session is pinned to. openTimeout bounds
// the WAIT FOR RESPONSE HEADERS, never the stream: a transport out of stream
// credit blocks instead of failing, and a silent block is the worst outcome —
// the peer waits out its own timeout while the caller hangs.
func openAttachStream(
	ctx context.Context,
	pool *httpLanePool,
	lane int,
	target string,
	headers http.Header,
	kind transportKind,
	openTimeout time.Duration,
) (*attachStream, error) {
	requestCtx, cancel := context.WithCancel(ctx)
	bodyReader, bodyWriter := io.Pipe()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, target, bodyReader)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header = headers.Clone()

	type result struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		var resp *http.Response
		var roundTripErr error
		if lane < 0 {
			resp, roundTripErr = pool.RoundTrip(req)
		} else {
			resp, roundTripErr = pool.roundTripOn(lane, req)
		}
		resultCh <- result{resp: resp, err: roundTripErr}
	}()
	timer := time.NewTimer(openTimeout)
	defer timer.Stop()

	var resp *http.Response
	select {
	case got := <-resultCh:
		if got.err != nil {
			cancel()
			_ = bodyWriter.CloseWithError(got.err)
			return nil, got.err
		}
		resp = got.resp
	case <-timer.C:
		cancel()
		_ = bodyWriter.CloseWithError(context.DeadlineExceeded)
		// Only what was observed. The causes are several — the peer still busy
		// reaching whatever it must reach before answering, a front holding the
		// request, a transport out of stream credit (which blocks rather than
		// failing) — and naming one of them here turns a guess into a verdict
		// the developer then chases. The status the peer eventually sends is
		// the diagnosis; this timer exists so the wait for it is not endless.
		return nil, fmt.Errorf("%s attach: the peer sent no response headers within %s", kind.label(), openTimeout)
	case <-ctx.Done():
		cancel()
		_ = bodyWriter.CloseWithError(ctx.Err())
		return nil, ctx.Err()
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		cancel()
		_ = bodyWriter.Close()
		return nil, &attachRejection{
			kind:    kind,
			status:  resp.Status,
			code:    resp.StatusCode,
			message: strings.TrimSpace(string(message)),
		}
	}
	return &attachStream{resp: resp, writer: bodyWriter, cancel: cancel}, nil
}
