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

// A network that cannot carry QUIC fails the probe on every reconnect. The
// verdict is remembered, so the handshake timeout is paid once — but not
// forever: a laptop that changes network may gain HTTP/3.
func TestIngressTransportRemembersProbeFailureUntilCooldown(t *testing.T) {
	state := newIngressTransportState()
	now := time.Now()
	state.now = func() time.Time { return now }

	state.noteProbeFailure(ingressTransportH3)
	if state.usable(ingressTransportH3) {
		t.Fatal("a failed probe must not be retried on the next reconnect")
	}
	if !state.usable(ingressTransportH2) {
		t.Fatal("one transport's probe failure must not condemn another")
	}
	now = now.Add(ingressTransportProbeCooldown)
	if !state.usable(ingressTransportH3) {
		t.Fatal("the probe must be retried once the cooldown expired")
	}
}

// The field failure this exists for: every attach succeeds, every session then
// dies on a regular schedule because something on the path bounds how long a
// request may last. Re-dialing forever is the wrong answer — fall back, and
// say why.
func TestIngressTransportFallsBackAfterRepeatedSessionLoss(t *testing.T) {
	state := newIngressTransportState()

	for i := 1; i < ingressTransportFailureBudget; i++ {
		if msg := state.noteFailure(ingressTransportH2, time.Minute); msg != "" {
			t.Fatalf("gave up after %d losses: %s", i, msg)
		}
		if !state.usable(ingressTransportH2) {
			t.Fatalf("transport retired after %d losses, budget is %d", i, ingressTransportFailureBudget)
		}
	}
	msg := state.noteFailure(ingressTransportH2, time.Minute)
	if msg == "" {
		t.Fatal("exhausting the budget must be explained, not silent")
	}
	if !strings.Contains(msg, "readTimeout") {
		t.Fatalf("the diagnosis must name the setting to change, got: %s", msg)
	}
	if state.usable(ingressTransportH2) {
		t.Fatal("the transport must be retired once its budget is spent")
	}
	if state.usable(ingressTransportH3) {
		return // h3 shares nothing with h2's budget only if it was never charged
	}
	t.Fatal("one transport's budget must not retire another")
}

func TestIngressTransportSessionThatHeldClearsTheBudget(t *testing.T) {
	state := newIngressTransportState()
	state.noteFailure(ingressTransportH2, time.Minute)
	state.noteSuccess(ingressTransportH2)
	for i := 1; i < ingressTransportFailureBudget; i++ {
		if msg := state.noteFailure(ingressTransportH2, time.Minute); msg != "" {
			t.Fatalf("a session that held must reset the count: %s", msg)
		}
	}
}

// A refused attach is a verdict about the session, not about the protocol —
// except for the two statuses that say "not over this protocol".
func TestIngressAttachRejectionOnlyRetiresProtocolRefusals(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{
		{http.StatusUnauthorized, false},
		{http.StatusConflict, false},
		{http.StatusUpgradeRequired, true},
		{http.StatusHTTPVersionNotSupported, true},
	} {
		rejection := &ingressAttachRejection{kind: ingressTransportH2, code: tc.code, status: "x"}
		if got := rejection.transportRefused(); got != tc.want {
			t.Fatalf("status %d: transportRefused = %v, want %v", tc.code, got, tc.want)
		}
	}
	// The refusal is what the developer reads on stderr: it must carry the
	// agent's own words, not just a status line.
	rejection := &ingressAttachRejection{
		kind: ingressTransportH2, code: 409, status: "409 Conflict",
		message: "endpoint occupied — one laptop per endpoint",
	}
	got := rejection.Error()
	if !strings.Contains(got, "HTTP/2") || !strings.Contains(got, "409") || !strings.Contains(got, "one laptop") {
		t.Fatalf("rejection message = %q", got)
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
	// The tunnel's whole admission bound at once, not a sample of it: the
	// transport must be able to carry what Origin admits. A QUIC peer
	// advertises a stream ceiling (quic-go defaults to 100) and a single
	// connection would silently block past it — the agent would then wait out
	// its open timeout and answer the visitor a 502 fifteen seconds later.
	requestCount := tun.IngressMaxStreams
	started := make(chan struct{}, requestCount)
	release := make(chan struct{})
	var releaseOnce sync.Once
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = fmt.Fprintf(w, "local:%s", r.URL.Path)
	}))
	devURL, _ := url.Parse(dev.URL)
	_, portText, _ := net.SplitHostPort(devURL.Host)
	localPort, _ := strconv.Atoi(portText)

	ingress := agent.NewIngress(nil)
	web := httptest.NewUnstartedServer(ingress)
	web.EnableHTTP2 = true
	web.StartTLS()
	// Teardown order matters and cannot be expressed with defer, which runs
	// before cleanups: httptest.Close waits for outstanding requests, and both
	// servers hold requests that only something else can end — the blocked dev
	// handlers, and the control request that lives until the session is cut.
	// Cleanups run LIFO, so this registers the reverse of the order needed:
	// unblock the handlers, close the dev server, cut the session, close the
	// ingress server. Get it wrong and any t.Fatal wedges the whole package
	// until the test binary times out, which is how a stale assertion in here
	// used to take four minutes to report itself.
	t.Cleanup(web.Close)
	t.Cleanup(func() { ingress.Cut("session-http", "revoked") })
	t.Cleanup(dev.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
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
	// Every request must be in flight AT ONCE: a page load fans out into far
	// more parallel requests than the old 32-stream ceiling allowed, and the
	// ceiling is what this asserts (tunnel.IngressMaxStreams, 128 active).
	// None of them may answer before the burst is admitted in full.
	for i := range requestCount {
		select {
		case <-started:
		case result := <-visitorResults:
			t.Fatalf("%s visitor %d finished before admission filled: status %d err %v", kind, result.index, result.status, result.err)
		case <-ctx.Done():
			t.Fatalf("%s admitted only %d concurrent streams, want %d", kind, i, requestCount)
		}
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
