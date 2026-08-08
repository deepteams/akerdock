package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// front starts the agent's real HTTP front on a loopback port and returns its
// authority. The server is the one Serve builds — the h2c wiring under test is
// the shipped wiring, not a test-local imitation.
func front(t *testing.T, handler http.Handler) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newFront(listener.Addr().String(), handler)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return listener.Addr().String()
}

// h2cClient speaks HTTP/2 over cleartext with prior knowledge — the shape
// Traefik uses for an `h2c://` backend.
func h2cClient() *http.Client {
	transport := &http.Transport{}
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: transport}
}

// TestFrontServesTheAttachPathOverH2C is ADR-063's end-to-end claim on the
// agent side: the laptop's control request reaches the ingress module over h2c
// and stays full-duplex — the agent writes an `open` frame on the response body
// while the request body is still open, and reads a frame back on it afterwards.
func TestFrontServesTheAttachPathOverH2C(t *testing.T) {
	ig := NewIngress(nil)
	authority := front(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ig.ServeHTTP(w, r)
	}))
	ig.SetRoutes([]IngressRoute{{Host: hostname(authority), EndpointUUID: "ep1"}})
	armed(ig, "sess-h2c", "ep1", "tok", time.Minute)
	key, err := tunnel.NewIngressAttachKey()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := h2cClient()

	controlReader, controlWriter := io.Pipe()
	controlReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+authority+proxy.IngressAttachPath+"?token=tok", controlReader)
	if err != nil {
		t.Fatal(err)
	}
	controlReq.Header.Set("Content-Type", tunnel.IngressControlContentType)
	controlReq.Header.Set(tunnel.IngressProtocolHeader, tunnel.IngressHTTPProtocol)
	controlReq.Header.Set(tunnel.IngressAttachKeyHeader, key)
	controlReq.Header.Set(tunnel.IngressTransportHeader, "h2")
	controlResp, err := client.Do(controlReq)
	if err != nil {
		t.Fatalf("control attach over h2c: %v", err)
	}
	defer func() { _ = controlResp.Body.Close() }()

	if controlResp.ProtoMajor != 2 {
		t.Fatalf("the attach was answered over %s — the front is not serving h2c", controlResp.Proto)
	}
	if controlResp.StatusCode != http.StatusOK {
		t.Fatalf("control attach status = %d", controlResp.StatusCode)
	}

	// A visitor on the same front (HTTP/1.1, as Traefik serves them) makes the
	// agent ask the laptop for a stream.
	go func() {
		resp, visitErr := http.Get("http://" + authority + "/asset.js") //nolint:noctx // bounded by the server's lifetime
		if visitErr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	control := tunnel.NewLineControl(controlResp.Body, controlWriter, nil, func() error {
		_ = controlWriter.Close()
		return controlResp.Body.Close()
	})
	open, err := control.Receive()
	if err != nil || open.Type != "open" {
		t.Fatalf("control frame over h2c = %+v, %v", open, err)
	}

	// The request body direction is live too: a close travels laptop → agent
	// on the very stream whose response body just carried the open.
	if err := control.Send(ctx, tunnel.HTTPControlFrame{Type: "session_close", Reason: "user_close"}); err != nil {
		t.Fatalf("session close over h2c: %v", err)
	}
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, controlResp.Body)
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not end the h2c control stream after session_close")
	}
}

// TestFrontAdvertisesStreamsAboveTheTunnelBound reads the front's SETTINGS
// frame the way Traefik's HTTP/2 transport does. Go's default of 250 would
// rebuild, inside HTTP/2, the queue this hop exists to remove: the tunnel alone
// admits ingressMaxActiveStreams + ingressMaxQueuedStreams requests, and it is
// the one that must do the rejecting, explicitly.
func TestFrontAdvertisesStreamsAboveTheTunnelBound(t *testing.T) {
	authority := front(t, http.NotFoundHandler())

	conn, err := net.DialTimeout("tcp", authority, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	framer := http2.NewFramer(conn, conn)
	if err := framer.WriteSettings(); err != nil {
		t.Fatal(err)
	}

	var advertised uint32
	for advertised == 0 {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("no SETTINGS frame from the front: %v", err)
		}
		settings, ok := frame.(*http2.SettingsFrame)
		if !ok || settings.IsAck() {
			continue
		}
		if value, ok := settings.Value(http2.SettingMaxConcurrentStreams); ok {
			advertised = value
		}
	}

	admission := uint32(ingressMaxActiveStreams + ingressMaxQueuedStreams)
	if advertised <= admission {
		t.Fatalf("the front advertises %d concurrent streams, at or below the tunnel's %d-request admission bound",
			advertised, admission)
	}
}

// TestFrontNeverArmsReadTimeout guards the regression that cost a day at the
// Traefik level (ADR-061, proxy-contract §5.2): Go's HTTP/2 server arms
// ReadTimeout as a per-stream deadline on the REQUEST BODY, so a non-zero value
// here would cut every long-lived ingress control and data request. The header
// timeout is the one that may be set.
func TestFrontNeverArmsReadTimeout(t *testing.T) {
	srv := newFront("127.0.0.1:0", http.NotFoundHandler())
	if srv.ReadTimeout != 0 {
		t.Fatalf("ReadTimeout = %s: an HTTP/2 request body is not a request head", srv.ReadTimeout)
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout must still bound a stalled request head")
	}
	if !srv.Protocols.UnencryptedHTTP2() || !srv.Protocols.HTTP1() {
		t.Fatalf("the front must serve both HTTP/1.1 and h2c: %+v", srv.Protocols)
	}
}
