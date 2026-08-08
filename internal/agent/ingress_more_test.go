package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// TestIngressAttachRequiresAToken pins the naked attach: no token, no
// upgrade, 401 — nothing consumed.
func TestIngressAttachRequiresAToken(t *testing.T) {
	ig := NewIngress(nil)
	ig.SetRoutes([]IngressRoute{{Host: "dev.example.com", EndpointUUID: "ep1"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://dev.example.com/.akerdock/ingress", nil)
	ig.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("attach without token: got %d, want 401", rec.Code)
	}
}

func TestIngressCapabilityProbeDoesNotConsumeToken(t *testing.T) {
	ig := NewIngress(nil)
	ig.SetRoutes([]IngressRoute{{Host: "dev.example.com", EndpointUUID: "ep1"}})
	armed(ig, "sess1", "ep1", "tok", time.Minute)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "http://dev.example.com/.akerdock/ingress", nil)
	ig.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("probe status = %d", rec.Code)
	}
	if got := rec.Header().Get(tunnel.IngressCapabilitiesHeader); got != tunnel.IngressHTTPProtocol+",h3,h2,websocket-v2,websocket" {
		t.Fatalf("capabilities = %q", got)
	}
	ig.mu.Lock()
	remaining := len(ig.expects)
	ig.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("probe consumed a mint: %d expectations remain", remaining)
	}
}

// HTTP v2 carries each relayed connection in its own full-duplex request. This
// test acts as the CLI at the byte level and proves the agent reverse proxy can
// exchange an HTTP request and response without the WebSocket mux.
func TestIngressHTTPV2EndToEndRelay(t *testing.T) {
	ig := NewIngress(nil)
	srv := httptest.NewServer(ig)
	defer srv.Close()
	host := hostname(strings.TrimPrefix(srv.URL, "http://"))
	ig.SetRoutes([]IngressRoute{{Host: host, EndpointUUID: "ep1"}})
	armed(ig, "sess-http", "ep1", "tok", time.Minute)
	key, err := tunnel.NewIngressAttachKey()
	if err != nil {
		t.Fatal(err)
	}

	controlReader, controlWriter := io.Pipe()
	controlReq, _ := http.NewRequest(http.MethodPost, srv.URL+proxy.IngressAttachPath+"?token=tok", controlReader)
	controlReq.Header.Set("Content-Type", tunnel.IngressControlContentType)
	controlReq.Header.Set(tunnel.IngressProtocolHeader, tunnel.IngressHTTPProtocol)
	controlReq.Header.Set(tunnel.IngressAttachKeyHeader, key)
	controlReq.Header.Set(tunnel.IngressTransportHeader, "h2")
	controlResp, err := http.DefaultClient.Do(controlReq)
	if err != nil {
		t.Fatalf("control attach: %v", err)
	}
	defer func() { _ = controlResp.Body.Close() }()
	clientControl := tunnel.NewLineControl(controlResp.Body, controlWriter, nil, func() error {
		_ = controlWriter.Close()
		return controlResp.Body.Close()
	})

	visitorDone := make(chan struct {
		status int
		body   string
		err    error
	}, 1)
	go func() {
		resp, requestErr := http.Get(srv.URL + "/asset.js") //nolint:noctx // bounded by test server lifetime
		if requestErr != nil {
			visitorDone <- struct {
				status int
				body   string
				err    error
			}{err: requestErr}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, requestErr := io.ReadAll(resp.Body)
		visitorDone <- struct {
			status int
			body   string
			err    error
		}{status: resp.StatusCode, body: string(body), err: requestErr}
	}()

	open, err := clientControl.Receive()
	if err != nil || open.Type != "open" {
		t.Fatalf("control open = %+v, %v", open, err)
	}
	dataReader, dataWriter := io.Pipe()
	streamURL, _ := url.Parse(srv.URL + proxy.IngressAttachPath)
	dataReq, _ := http.NewRequest(http.MethodPost, streamURL.String(), dataReader)
	dataReq.Header.Set("Content-Type", tunnel.IngressStreamContentType)
	dataReq.Header.Set(tunnel.IngressSessionHeader, "sess-http")
	dataReq.Header.Set(tunnel.IngressStreamHeader, fmt.Sprint(open.ID))
	dataReq.Header.Set(tunnel.IngressAttachKeyHeader, key)
	dataResp, err := http.DefaultClient.Do(dataReq)
	if err != nil {
		t.Fatalf("data attach: %v", err)
	}
	defer func() { _ = dataResp.Body.Close() }()

	reader := bufio.NewReader(dataResp.Body)
	requestHead, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(requestHead, "GET /asset.js HTTP/1.1") {
		t.Fatalf("relayed request line = %q, %v", requestHead, err)
	}
	// Drain the remaining request headers before replying as the local app.
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("relayed request headers: %v", readErr)
		}
		if line == "\r\n" {
			break
		}
	}
	_, _ = io.WriteString(dataWriter, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	_ = dataWriter.Close()

	select {
	case result := <-visitorDone:
		if result.err != nil || result.status != http.StatusOK || result.body != "ok" {
			t.Fatalf("visitor = status %d body %q err %v", result.status, result.body, result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("visitor request did not cross HTTP v2")
	}
	_ = clientControl.Send(context.Background(), tunnel.HTTPControlFrame{Type: "session_close", Reason: "user_close"})
}

// TestIngressAttachReleasesSlotOnFailedUpgrade pins the reservation cleanup:
// a valid token on a request that cannot upgrade to WebSocket must free the
// endpoint (and the token stays consumed — single use, valid or not).
func TestIngressAttachReleasesSlotOnFailedUpgrade(t *testing.T) {
	ig := NewIngress(nil)
	ig.SetRoutes([]IngressRoute{{Host: "dev.example.com", EndpointUUID: "ep1"}})
	armed(ig, "sess1", "ep1", "tok", time.Minute)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://dev.example.com/.akerdock/ingress?token=tok", nil)
	ig.ServeHTTP(rec, req) // a plain GET: websocket.Accept refuses it

	if rec.Code == http.StatusSwitchingProtocols {
		t.Fatal("a plain GET must not upgrade")
	}
	ig.mu.Lock()
	_, occupied := ig.live["ep1"]
	expects := len(ig.expects)
	ig.mu.Unlock()
	if occupied {
		t.Fatal("the failed upgrade left the endpoint occupied")
	}
	if expects != 0 {
		t.Fatal("the token must be consumed even when the upgrade fails")
	}
}

// TestIngressCutEmptyReasonDefaultsToRevoked pins the reason fallback of an
// operator cut.
func TestIngressCutEmptyReasonDefaultsToRevoked(t *testing.T) {
	ig := NewIngress(nil)
	cancel := liveSession(ig, "ep1", "sess-live")
	if !ig.Cut("sess-live", "") {
		t.Fatal("Cut should reach the live session")
	}
	select {
	case r := <-cancel:
		if r != endReasonRevoked {
			t.Fatalf("cancel reason = %q, want the revoked default", r)
		}
	default:
		t.Fatal("Cut did not deliver a reason")
	}
}

// TestIngressExpectPurgesExpiredEntries pins the housekeeping: arming a new
// expectation sweeps the expired ones, so the map never grows unbounded.
func TestIngressExpectPurgesExpiredEntries(t *testing.T) {
	ig := NewIngress(nil)
	armed(ig, "sess-old", "ep1", "tok-old", -time.Second) // already expired
	armed(ig, "sess-new", "ep1", "tok-new", time.Minute)

	ig.mu.Lock()
	defer ig.mu.Unlock()
	if len(ig.expects) != 1 {
		t.Fatalf("expects = %d entries, want the expired one purged", len(ig.expects))
	}
	if _, ok := ig.expects[tokenHash("tok-new")]; !ok {
		t.Fatal("the fresh expectation must survive the purge")
	}
}

// TestIngressRelayRewriteAndErrorHandler pins the relay proxy's two callbacks
// without a live tunnel: the rewrite changes only the URL (the
// public Host survives for the developer's app), and a failed dial answers
// 502 with a human cause.
func TestIngressRelayRewriteAndErrorHandler(t *testing.T) {
	ig := NewIngress(nil)
	rp, ok := ig.newRelay(nil).(*httputil.ReverseProxy)
	if !ok {
		t.Fatal("newRelay must build a ReverseProxy")
	}

	req := httptest.NewRequest(http.MethodGet, "http://dev.example.com/hook?x=1", nil)
	req.Host = "dev.example.com"
	out := req.Clone(req.Context())
	rp.Rewrite(&httputil.ProxyRequest{In: req, Out: out})
	if out.URL.Scheme != "http" || out.URL.Host != "ingress" {
		t.Fatalf("rewritten URL = %s, want the placeholder target", out.URL)
	}
	if out.Host != "dev.example.com" {
		t.Fatalf("out.Host = %q, want the public host preserved", out.Host)
	}

	rec := httptest.NewRecorder()
	rp.ErrorHandler(rec, req, errors.New("mux stream refused"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("relay failure = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "did not answer") {
		t.Fatalf("relay failure body = %q", rec.Body.String())
	}

	for _, queueErr := range []error{tunnel.ErrOriginQueueFull, tunnel.ErrOriginQueueTimeout} {
		rec = httptest.NewRecorder()
		rp.ErrorHandler(rec, req, queueErr)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("queue failure = %d, want 503", rec.Code)
		}
		if rec.Header().Get("Retry-After") != "1" {
			t.Fatalf("queue failure Retry-After = %q", rec.Header().Get("Retry-After"))
		}
		if !strings.Contains(rec.Body.String(), "busy") {
			t.Fatalf("queue failure body = %q", rec.Body.String())
		}
	}
}

func TestIngressStreamAdmissionLimits(t *testing.T) {
	if ingressMaxActiveStreams != 32 || ingressMaxQueuedStreams != 512 || ingressStreamQueueWait != 30*time.Second {
		t.Fatalf("ingress stream admission = active %d, queued %d, wait %s",
			ingressMaxActiveStreams, ingressMaxQueuedStreams, ingressStreamQueueWait)
	}
}

func TestIngressMetricOutcomeHasBoundedValues(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{tunnel.ErrOriginQueueFull, "queue_full"},
		{tunnel.ErrOriginQueueTimeout, "queue_timeout"},
		{tunnel.ErrOriginClosed, "closed"},
		{context.Canceled, "canceled"},
		{errors.New("network"), "failed"},
	}
	for _, tt := range tests {
		if got := ingressMetricOutcome(tt.err); got != tt.want {
			t.Fatalf("outcome(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

type countingIngressOpener struct {
	address string
	opens   atomic.Int64
}

func (o *countingIngressOpener) OpenStream(ctx context.Context) (net.Conn, error) {
	o.opens.Add(1)
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", o.address)
}

func TestIngressRelayReusesConnectionsWithinOneSessionOnly(t *testing.T) {
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer dev.Close()
	devURL, err := url.Parse(dev.URL)
	if err != nil {
		t.Fatal(err)
	}
	ig := NewIngress(nil)
	request := func(relay http.Handler) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://dev.example.test/asset.js", nil)
		relay.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("relay response = %d %q", rec.Code, rec.Body.String())
		}
	}

	first := &countingIngressOpener{address: devURL.Host}
	firstRelay := ig.newRelay(first)
	request(firstRelay)
	request(firstRelay)
	if got := first.opens.Load(); got != 1 {
		t.Fatalf("two sequential requests opened %d local connections, want one keep-alive connection", got)
	}

	second := &countingIngressOpener{address: devURL.Host}
	request(ig.newRelay(second))
	if first.opens.Load() != 1 || second.opens.Load() != 1 {
		t.Fatalf("session pools crossed: first=%d second=%d", first.opens.Load(), second.opens.Load())
	}
}

func TestIngressWebSocketV2AttachesFourAuthenticatedLanes(t *testing.T) {
	ig := NewIngress(nil)
	srv := httptest.NewServer(ig)
	defer srv.Close()
	host := hostname(strings.TrimPrefix(srv.URL, "http://"))
	ig.SetRoutes([]IngressRoute{{Host: host, EndpointUUID: "ep1"}})
	armed(ig, "sess-ws-v2", "ep1", "tok", time.Minute)
	key, err := tunnel.NewIngressAttachKey()
	if err != nil {
		t.Fatal(err)
	}
	attachURL := "ws" + strings.TrimPrefix(srv.URL, "http") + proxy.IngressAttachPath
	primaryHeader := make(http.Header)
	primaryHeader.Set(tunnel.IngressAttachKeyHeader, key)
	primary, _, err := websocket.Dial(context.Background(), attachURL+"?token=tok", &websocket.DialOptions{
		Subprotocols: []string{tunnel.IngressWebSocketV2, IngressSubprotocol},
		HTTPHeader:   primaryHeader,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = primary.CloseNow() }()
	if primary.Subprotocol() != tunnel.IngressWebSocketV2 {
		t.Fatalf("primary negotiated %q", primary.Subprotocol())
	}

	deadline := time.Now().Add(time.Second)
	var lanes *tunnel.MultiLaneConn
	for lanes == nil && time.Now().Before(deadline) {
		ig.mu.Lock()
		if session := ig.live["ep1"]; session != nil {
			lanes = session.wsLanes
		}
		ig.mu.Unlock()
		if lanes == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if lanes == nil {
		t.Fatal("primary v2 lane group was not published")
	}

	secondary := make([]*websocket.Conn, 0, 3)
	for lane := 1; lane < 4; lane++ {
		header := make(http.Header)
		header.Set(tunnel.IngressSessionHeader, "sess-ws-v2")
		header.Set(tunnel.IngressAttachKeyHeader, key)
		header.Set(tunnel.IngressLaneHeader, fmt.Sprint(lane))
		conn, _, dialErr := websocket.Dial(context.Background(), attachURL, &websocket.DialOptions{
			Subprotocols: []string{tunnel.IngressWebSocketV2}, HTTPHeader: header,
		})
		if dialErr != nil {
			t.Fatalf("lane %d: %v", lane, dialErr)
		}
		secondary = append(secondary, conn)
	}
	defer func() {
		for _, conn := range secondary {
			_ = conn.CloseNow()
		}
	}()
	if got := lanes.LaneCount(); got != 4 {
		t.Fatalf("attached lanes = %d, want 4", got)
	}

	// Let every server-side graceful close complete instead of waiting on a
	// client that never consumes its close control frame.
	for _, conn := range secondary {
		go func() { _, _, _ = conn.Read(context.Background()) }()
	}
	if !ig.Cut("sess-ws-v2", "revoked") {
		t.Fatal("v2 session was not live")
	}
	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, readErr := primary.Read(readCtx); readErr == nil {
		t.Fatal("primary lane stayed open after session cut")
	}
}

// TestIngressOfflinePageHeadHasNoBody pins the HEAD arm of the offline page.
func TestIngressOfflinePageHeadHasNoBody(t *testing.T) {
	ig := NewIngress(nil)
	ig.SetRoutes([]IngressRoute{{Host: "dev.example.com", EndpointUUID: "ep1"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "http://dev.example.com/", nil)
	ig.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q, want empty", rec.Body.String())
	}
}

// TestTunnelWSConnAdapter drives the coder/websocket adapter over a real
// loopback pair: text and binary frames map to the tunnel's types in both
// directions, pings answer while the peer reads, and a normal close comes
// back as ErrClientClosed — the signal the tunnel treats as a clean hangup.
func TestTunnelWSConnAdapter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		if conn.Write(ctx, websocket.MessageText, []byte("hello text")) != nil {
			return
		}
		if conn.Write(ctx, websocket.MessageBinary, []byte{0x1}) != nil {
			return
		}
		// Read the client's two frames (and answer its ping meanwhile).
		for i := 0; i < 2; i++ {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	tw := tunnelWSConn{conn}

	// A persistent read loop, like the tunnel's own: frames flow to the
	// channel, control frames (the pong) are handled inside Read.
	type frame struct {
		typ  tunnel.MessageType
		data []byte
	}
	frames := make(chan frame, 4)
	readErr := make(chan error, 1)
	go func() {
		for {
			typ, data, err := tw.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			frames <- frame{typ, data}
		}
	}()
	next := func() frame {
		select {
		case f := <-frames:
			return f
		case err := <-readErr:
			t.Fatalf("read loop died: %v", err)
			return frame{}
		}
	}

	if f := next(); f.typ != tunnel.MessageText || string(f.data) != "hello text" {
		t.Fatalf("first frame = %v %q, want the text frame", f.typ, f.data)
	}
	if f := next(); f.typ != tunnel.MessageBinary || len(f.data) != 1 {
		t.Fatalf("second frame = %v %q, want the binary frame", f.typ, f.data)
	}
	// Both peers are reading: the ping gets its pong.
	if err := tw.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := tw.Write(ctx, tunnel.MessageText, []byte("up text")); err != nil {
		t.Fatalf("text write: %v", err)
	}
	if err := tw.Write(ctx, tunnel.MessageBinary, []byte{0x2}); err != nil {
		t.Fatalf("binary write: %v", err)
	}
	// The server closes normally: the adapter reports the clean hangup.
	if err := <-readErr; !errors.Is(err, tunnel.ErrClientClosed) {
		t.Fatalf("read after close = %v, want ErrClientClosed", err)
	}
}
