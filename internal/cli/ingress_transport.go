package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"

	tun "github.com/deepteams/akerdock/internal/tunnel"
)

type ingressTransportKind string

const (
	ingressTransportH3 ingressTransportKind = "h3"
	ingressTransportH2 ingressTransportKind = "h2"
	ingressTransportWS ingressTransportKind = "websocket"

	ingressTransportProbeTimeout  = 3 * time.Second
	ingressTransportAttachTimeout = 5 * time.Second
	ingressHTTP2Lanes             = 4

	// ingressTransportFailureBudget is how many consecutive lost sessions a
	// transport gets before the CLI falls back to the next one. A transport
	// whose every attach succeeds and whose every session then dies is not a
	// transport this path can carry: an HTTP front that bounds how long a
	// request may last (Traefik's respondingTimeouts.readTimeout, 60 s by
	// default) cuts the control request on a perfectly regular schedule, and
	// every relayed connection with it. WebSocket transports are immune —
	// hijacked connections leave the server's read deadline behind — so the
	// fallback is the answer, not an eleventh re-dial.
	ingressTransportFailureBudget = 3

	// ingressTransportProbeCooldown is how long a failed capability probe is
	// remembered. A network that blocks UDP fails the HTTP/3 probe on every
	// single reconnect, and paying a QUIC handshake timeout each time is pure
	// downtime; a cooldown rather than a permanent verdict because a laptop
	// that changes network may well gain QUIC.
	ingressTransportProbeCooldown = 5 * time.Minute
)

func (k ingressTransportKind) label() string {
	switch k {
	case ingressTransportH3:
		return "HTTP/3 (QUIC)"
	case ingressTransportH2:
		return "HTTP/2"
	default:
		return "WebSocket"
	}
}

// ingressTransportState is the per-process memory of what this network can
// actually carry. It outlives one session on purpose: the reconnect loop would
// otherwise re-learn the same verdict every minute.
type ingressTransportState struct {
	now         func() time.Time
	disabled    map[ingressTransportKind]bool
	probeFailed map[ingressTransportKind]time.Time
	failures    map[ingressTransportKind]int
	announced   ingressTransportKind
}

func newIngressTransportState() *ingressTransportState {
	return &ingressTransportState{
		now:         time.Now,
		disabled:    make(map[ingressTransportKind]bool),
		probeFailed: make(map[ingressTransportKind]time.Time),
		failures:    make(map[ingressTransportKind]int),
	}
}

// usable reports whether kind is worth another attempt right now.
func (s *ingressTransportState) usable(kind ingressTransportKind) bool {
	if s.disabled[kind] {
		return false
	}
	failed, ok := s.probeFailed[kind]
	return !ok || s.now().Sub(failed) >= ingressTransportProbeCooldown
}

// noteProbeFailure remembers that kind could not even be negotiated.
func (s *ingressTransportState) noteProbeFailure(kind ingressTransportKind) {
	s.probeFailed[kind] = s.now()
}

// disable retires a transport for the rest of the process.
func (s *ingressTransportState) disable(kind ingressTransportKind) {
	s.disabled[kind] = true
}

// noteFailure charges one lost attempt to kind's budget and returns the
// operator-facing diagnosis when the budget runs out. lifetime is how long
// the attempt held, which is the single most diagnostic number here: a tunnel
// that dies after the same suspiciously round duration, over and over, is a
// tunnel someone is cutting on a timer.
func (s *ingressTransportState) noteFailure(kind ingressTransportKind, lifetime time.Duration) string {
	s.failures[kind]++
	if s.failures[kind] < ingressTransportFailureBudget {
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
// carry the tunnel, so the next drop starts from zero rather than inheriting
// a count from an outage half a day ago.
func (s *ingressTransportState) noteSuccess(kind ingressTransportKind) {
	delete(s.failures, kind)
}

// ingressAttachRejection is an attach the agent refused with an HTTP status.
// It is a policy or authentication verdict — an expired mint, an endpoint
// still occupied by the very session being replaced — and says nothing about
// the transport, except for the two statuses that do.
type ingressAttachRejection struct {
	kind    ingressTransportKind
	status  string
	code    int
	message string
}

func (e *ingressAttachRejection) Error() string {
	return fmt.Sprintf("%s attach returned %s: %s", e.kind.label(), e.status, e.message)
}

// transportRefused reports whether the peer answered "not over this protocol",
// which is the only rejection worth retiring the transport for.
func (e *ingressAttachRejection) transportRefused() bool {
	return e.code == http.StatusUpgradeRequired || e.code == http.StatusHTTPVersionNotSupported
}

func ingressTransportPreference() [3]ingressTransportKind {
	return [3]ingressTransportKind{ingressTransportH3, ingressTransportH2, ingressTransportWS}
}

type ingressHTTPLane struct {
	roundTripper http.RoundTripper
	close        func() error
	active       atomic.Int64
}

type ingressHTTPLanePool struct {
	lanes []*ingressHTTPLane
}

func newIngressH3Pool() *ingressHTTPLanePool {
	return newIngressH3PoolWithTLS(nil)
}

func newIngressH3PoolWithTLS(tlsConfig *tls.Config) *ingressHTTPLanePool {
	transport := &http3.Transport{
		TLSClientConfig: tlsConfig,
		QUICConfig: &quic.Config{
			HandshakeIdleTimeout: ingressTransportProbeTimeout,
			MaxIdleTimeout:       90 * time.Second,
			KeepAlivePeriod:      20 * time.Second,
		},
	}
	return &ingressHTTPLanePool{lanes: []*ingressHTTPLane{{
		roundTripper: transport,
		close:        transport.Close,
	}}}
}

func newIngressH2Pool() *ingressHTTPLanePool {
	return newIngressH2PoolWithTLS(nil)
}

func newIngressH2PoolWithTLS(tlsConfig *tls.Config) *ingressHTTPLanePool {
	pool := &ingressHTTPLanePool{lanes: make([]*ingressHTTPLane, 0, ingressHTTP2Lanes)}
	for range ingressHTTP2Lanes {
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
		pool.lanes = append(pool.lanes, &ingressHTTPLane{
			roundTripper: transport,
			close: func() error {
				transport.CloseIdleConnections()
				return nil
			},
		})
	}
	return pool
}

func (p *ingressHTTPLanePool) RoundTrip(req *http.Request) (*http.Response, error) {
	return p.roundTripOn(p.leastLoaded(), req)
}

func (p *ingressHTTPLanePool) roundTripOn(index int, req *http.Request) (*http.Response, error) {
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

func (p *ingressHTTPLanePool) leastLoaded() int {
	best := 0
	load := p.lanes[0].active.Load()
	for i := 1; i < len(p.lanes); i++ {
		if candidate := p.lanes[i].active.Load(); candidate < load {
			best, load = i, candidate
		}
	}
	return best
}

func (p *ingressHTTPLanePool) Close() error {
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

func ingressHTTPURL(sess ingressMint) (*url.URL, error) {
	attach, err := url.Parse(sess.AttachUrl)
	if err != nil {
		return nil, fmt.Errorf("invalid ingress attach URL: %w", err)
	}
	switch attach.Scheme {
	case "wss":
		attach.Scheme = "https"
	case "ws":
		attach.Scheme = "http"
	case "https", "http":
	default:
		return nil, fmt.Errorf("unsupported ingress attach URL scheme %q", attach.Scheme)
	}
	query := attach.Query()
	query.Del("token")
	attach.RawQuery = query.Encode()
	return attach, nil
}

func ingressHTTPControlURL(sess ingressMint) (*url.URL, error) {
	attach, err := ingressHTTPURL(sess)
	if err != nil {
		return nil, err
	}
	query := attach.Query()
	if sess.Token != "" {
		query.Set("token", sess.Token)
	}
	if query.Get("token") == "" {
		return nil, errors.New("ingress mint response has no attach token")
	}
	attach.RawQuery = query.Encode()
	return attach, nil
}

func probeIngressHTTP(ctx context.Context, pool *ingressHTTPLanePool, attach *url.URL, kind ingressTransportKind) error {
	probeCtx, cancel := context.WithTimeout(ctx, ingressTransportProbeTimeout)
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
	capabilities := resp.Header.Get(tun.IngressCapabilitiesHeader)
	if !strings.Contains(capabilities, tun.IngressHTTPProtocol) || !strings.Contains(capabilities, string(kind)) {
		return errors.New("agent does not advertise HTTP ingress v2")
	}
	if kind == ingressTransportH3 && resp.ProtoMajor != 3 {
		return fmt.Errorf("HTTP/3 probe negotiated %s", resp.Proto)
	}
	if kind == ingressTransportH2 && resp.ProtoMajor != 2 {
		return fmt.Errorf("HTTP/2 probe negotiated %s", resp.Proto)
	}
	return nil
}

type ingressHTTPControlSession struct {
	control *tun.LineControl
	cancel  context.CancelFunc
}

func openIngressHTTPControl(
	ctx context.Context,
	pool *ingressHTTPLanePool,
	sess ingressMint,
	key string,
	kind ingressTransportKind,
) (*ingressHTTPControlSession, error) {
	controlURL, err := ingressHTTPControlURL(sess)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithCancel(ctx)
	bodyReader, bodyWriter := io.Pipe()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, controlURL.String(), bodyReader)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", tun.IngressControlContentType)
	req.Header.Set(tun.IngressProtocolHeader, tun.IngressHTTPProtocol)
	req.Header.Set(tun.IngressAttachKeyHeader, key)
	req.Header.Set(tun.IngressTransportHeader, string(kind))

	type result struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, roundTripErr := pool.roundTripOn(0, req)
		resultCh <- result{resp: resp, err: roundTripErr}
	}()
	timer := time.NewTimer(ingressTransportAttachTimeout)
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
		return nil, fmt.Errorf("%s attach timed out", kind.label())
	case <-ctx.Done():
		cancel()
		_ = bodyWriter.CloseWithError(ctx.Err())
		return nil, ctx.Err()
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get(tun.IngressProtocolHeader) != tun.IngressHTTPProtocol {
		defer func() { _ = resp.Body.Close() }()
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		cancel()
		_ = bodyWriter.Close()
		return nil, &ingressAttachRejection{
			kind:    kind,
			status:  resp.Status,
			code:    resp.StatusCode,
			message: strings.TrimSpace(string(message)),
		}
	}
	control := tun.NewLineControl(resp.Body, bodyWriter, nil, func() error {
		cancel()
		_ = bodyWriter.Close()
		return resp.Body.Close()
	})
	return &ingressHTTPControlSession{control: control, cancel: cancel}, nil
}

func openIngressHTTPData(
	ctx context.Context,
	pool *ingressHTTPLanePool,
	attach *url.URL,
	sessionUUID, key string,
	id uint32,
) (net.Conn, error) {
	requestCtx, cancel := context.WithCancel(ctx)
	bodyReader, bodyWriter := io.Pipe()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, attach.String(), bodyReader)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", tun.IngressStreamContentType)
	req.Header.Set(tun.IngressSessionHeader, sessionUUID)
	req.Header.Set(tun.IngressStreamHeader, strconv.FormatUint(uint64(id), 10))
	req.Header.Set(tun.IngressAttachKeyHeader, key)
	resp, err := pool.RoundTrip(req)
	if err != nil {
		cancel()
		_ = bodyWriter.CloseWithError(err)
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		_ = resp.Body.Close()
		cancel()
		_ = bodyWriter.Close()
		return nil, fmt.Errorf("stream %d returned %s: %s", id, resp.Status, strings.TrimSpace(string(message)))
	}
	return tun.NewDuplexConn(resp.Body, bodyWriter, nil, cancel), nil
}

func runIngressHTTPBridge(
	ctx context.Context,
	control *tun.LineControl,
	pool *ingressHTTPLanePool,
	attach *url.URL,
	sessionUUID, key string,
	localPort int,
) (string, error) {
	workCtx, cancel := context.WithCancel(ctx)
	var streams sync.WaitGroup
	defer func() {
		cancel()
		streams.Wait()
	}()
	for {
		frame, err := control.Receive()
		if err != nil {
			if ctx.Err() != nil {
				return "user_close", nil
			}
			return "", err
		}
		switch frame.Type {
		case "open":
			streams.Add(1)
			go func(id uint32) {
				defer streams.Done()
				local, dialErr := dialIngressLocal(workCtx, localPort)
				if dialErr != nil {
					_ = control.Send(workCtx, tun.HTTPControlFrame{Type: "open_err", ID: id, Code: "dial_failed", Msg: dialErr.Error()})
					return
				}
				remote, openErr := openIngressHTTPData(workCtx, pool, attach, sessionUUID, key, id)
				if openErr != nil {
					_ = local.Close()
					_ = control.Send(workCtx, tun.HTTPControlFrame{Type: "open_err", ID: id, Code: "stream_failed", Msg: openErr.Error()})
					return
				}
				bridgeIngressConns(local, remote)
			}(frame.ID)
		case "ping":
			if err := control.Send(workCtx, tun.HTTPControlFrame{Type: "pong"}); err != nil {
				return "", err
			}
		case "session_close":
			return frame.Reason, nil
		}
	}
}

// dialIngressLocal reaches the developer's app on the loopback. It resolves
// "localhost" rather than dialing 127.0.0.1 outright: a dev server bound to
// ::1 only (the default of more than one framework) is on the loopback too,
// and a tunnel that answers "connection refused" on it is a tunnel the
// developer cannot debug. Go's dual-stack dial tries both families.
func dialIngressLocal(ctx context.Context, port int) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", net.JoinHostPort("localhost", strconv.Itoa(port)))
}

func bridgeIngressConns(a, b net.Conn) {
	done := make(chan struct{}, 2)
	copyOne := func(dst, src net.Conn) {
		buf := ingressCopyBufferPool.Get().(*[]byte)
		_, _ = io.CopyBuffer(dst, src, *buf)
		ingressCopyBufferPool.Put(buf)
		done <- struct{}{}
	}
	go copyOne(a, b)
	go copyOne(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

var ingressCopyBufferPool = sync.Pool{New: func() any {
	buf := make([]byte, 64*1024)
	return &buf
}}
