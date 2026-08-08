package cli

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/deepteams/akerdock/internal/agent"
	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/proxy"
	tun "github.com/deepteams/akerdock/internal/tunnel"
)

func TestIngressHTTPLanePoolChoosesLeastLoaded(t *testing.T) {
	pool := &ingressHTTPLanePool{lanes: []*ingressHTTPLane{{}, {}, {}, {}}}
	pool.lanes[0].active.Store(3)
	pool.lanes[1].active.Store(1)
	pool.lanes[2].active.Store(2)
	pool.lanes[3].active.Store(1)
	if got := pool.leastLoaded(); got != 1 {
		t.Fatalf("least loaded lane = %d, want first lane at load 1", got)
	}
}

func TestIngressTransportPreference(t *testing.T) {
	want := [3]ingressTransportKind{ingressTransportH3, ingressTransportH2, ingressTransportWS}
	if got := ingressTransportPreference(); got != want {
		t.Fatalf("transport preference = %v, want %v", got, want)
	}
}

func TestIngressHTTPURLConversionDoesNotLeakMintTokenIntoProbe(t *testing.T) {
	sess := ingressMint{AttachUrl: "wss://dev.example.com/.akerdock/ingress?token=stale&x=1", Token: "fresh"}
	probe, err := ingressHTTPURL(sess)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Scheme != "https" || probe.Query().Get("token") != "" || probe.Query().Get("x") != "1" {
		t.Fatalf("probe URL = %s", probe)
	}
	control, err := ingressHTTPControlURL(sess)
	if err != nil {
		t.Fatal(err)
	}
	if control.Query().Get("token") != "fresh" {
		t.Fatal("control URL must carry the separately returned authoritative token")
	}
}

func TestIngressHTTP2AndHTTP3Relay(t *testing.T) {
	for _, kind := range []ingressTransportKind{ingressTransportH2, ingressTransportH3} {
		t.Run(string(kind), func(t *testing.T) {
			testIngressHTTPRelay(t, kind)
		})
	}
}

func testIngressHTTPRelay(t *testing.T, kind ingressTransportKind) {
	t.Helper()
	const requestCount = 40
	started := make(chan struct{}, requestCount)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = fmt.Fprintf(w, "local:%s", r.URL.Path)
	}))
	defer dev.Close()
	devURL, _ := url.Parse(dev.URL)
	_, portText, _ := net.SplitHostPort(devURL.Host)
	localPort, _ := strconv.Atoi(portText)

	ingress := agent.NewIngress(nil)
	web := httptest.NewUnstartedServer(ingress)
	web.EnableHTTP2 = true
	web.StartTLS()
	defer web.Close()
	ingress.SetRoutes([]agent.IngressRoute{{Host: "127.0.0.1", EndpointUUID: "ep1"}})
	token := "http-transport-token"
	tokenSum := sha256.Sum256([]byte(token))
	ingress.Expect(agentwire.IngressExpectParams{
		SessionUUID:   "session-http",
		EndpointUUID:  "ep1",
		TokenSHA256:   hex.EncodeToString(tokenSum[:]),
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	})

	roots := x509.NewCertPool()
	roots.AddCert(web.Certificate())
	clientTLS := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	attachAuthority := strings.TrimPrefix(web.URL, "https://")
	var transportPool *ingressHTTPLanePool
	var closeH3 func()
	if kind == ingressTransportH3 {
		packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		h3Server := &http3.Server{Handler: ingress, TLSConfig: web.TLS.Clone()}
		serveDone := make(chan error, 1)
		go func() { serveDone <- h3Server.Serve(packetConn) }()
		closeH3 = func() {
			_ = h3Server.Close()
			_ = packetConn.Close()
			select {
			case <-serveDone:
			case <-time.After(time.Second):
			}
		}
		attachAuthority = packetConn.LocalAddr().String()
		transportPool = newIngressH3PoolWithTLS(clientTLS)
	} else {
		transportPool = newIngressH2PoolWithTLS(clientTLS)
	}
	if closeH3 != nil {
		defer closeH3()
	}
	defer func() { _ = transportPool.Close() }()

	sess := ingressMint{
		Uuid:      "session-http",
		AttachUrl: "wss://" + attachAuthority + proxy.IngressAttachPath,
		Token:     token,
	}
	attach, err := ingressHTTPURL(sess)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := probeIngressHTTP(ctx, transportPool, attach, kind); err != nil {
		t.Fatalf("%s probe: %v", kind, err)
	}
	key, err := tun.NewIngressAttachKey()
	if err != nil {
		t.Fatal(err)
	}
	control, err := openIngressHTTPControl(ctx, transportPool, sess, key, kind)
	if err != nil {
		t.Fatalf("%s control: %v", kind, err)
	}
	defer func() { _ = control.control.Close() }()

	bridgeDone := make(chan struct {
		reason string
		err    error
	}, 1)
	go func() {
		reason, bridgeErr := runIngressHTTPBridge(ctx, control.control, transportPool, attach, sess.Uuid, key, localPort)
		bridgeDone <- struct {
			reason string
			err    error
		}{reason: reason, err: bridgeErr}
	}()

	type visitorResult struct {
		index  int
		status int
		body   string
		err    error
	}
	visitorResults := make(chan visitorResult, requestCount)
	for i := range requestCount {
		go func() {
			path := fmt.Sprintf("/asset-%d.js", i)
			req, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, web.URL+path, nil)
			if requestErr != nil {
				visitorResults <- visitorResult{index: i, err: requestErr}
				return
			}
			visitor, requestErr := web.Client().Do(req)
			if requestErr != nil {
				visitorResults <- visitorResult{index: i, err: requestErr}
				return
			}
			body, readErr := io.ReadAll(visitor.Body)
			_ = visitor.Body.Close()
			visitorResults <- visitorResult{index: i, status: visitor.StatusCode, body: string(body), err: readErr}
		}()
	}
	for i := 0; i < 32; i++ {
		select {
		case <-started:
		case result := <-visitorResults:
			t.Fatalf("%s visitor %d finished before admission filled: status %d err %v", kind, result.index, result.status, result.err)
		case <-ctx.Done():
			t.Fatalf("%s admitted only %d concurrent streams", kind, i)
		}
	}
	// Keep the first 32 handlers blocked long enough for the remaining eight
	// requests to reach Origin's bounded pending queue. None may become the
	// 33rd active local connection.
	time.Sleep(50 * time.Millisecond)
	if extra := len(started); extra != 0 {
		t.Fatalf("%s admitted %d streams beyond the active limit", kind, extra)
	}
	releaseOnce.Do(func() { close(release) })
	for range requestCount {
		select {
		case result := <-visitorResults:
			want := fmt.Sprintf("local:/asset-%d.js", result.index)
			if result.err != nil || result.status != http.StatusOK || result.body != want {
				t.Fatalf("%s visitor %d = status %d body %q err %v", kind, result.index, result.status, result.body, result.err)
			}
		case <-ctx.Done():
			t.Fatalf("%s visitor burst did not drain", kind)
		}
	}
	if !ingress.Cut("session-http", "revoked") {
		t.Fatal("live HTTP session was not registered")
	}
	select {
	case result := <-bridgeDone:
		if result.err != nil || result.reason != "revoked" {
			t.Fatalf("%s bridge = reason %q err %v", kind, result.reason, result.err)
		}
	case <-ctx.Done():
		t.Fatalf("%s bridge did not close", kind)
	}
}
