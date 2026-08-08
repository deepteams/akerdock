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

type ingressTransportState struct {
	disabled  map[ingressTransportKind]bool
	announced ingressTransportKind
}

func newIngressTransportState() *ingressTransportState {
	return &ingressTransportState{disabled: make(map[ingressTransportKind]bool)}
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
		return nil, fmt.Errorf("%s attach returned %s: %s", kind.label(), resp.Status, strings.TrimSpace(string(message)))
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
				var dialer net.Dialer
				local, dialErr := dialer.DialContext(workCtx, "tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
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
