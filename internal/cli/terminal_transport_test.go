package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tun "github.com/deepteams/akerdock/internal/tunnel"
)

func TestTerminalAttachURLDoesNotLeakTheMintTokenIntoTheProbe(t *testing.T) {
	if _, err := terminalAttachURL("https://panel.example.com", ""); err == nil {
		t.Fatal("a mint with no attach path must be refused")
	}
	if _, err := terminalAttachURL("://nope", "/terminal/attach"); err == nil {
		t.Fatal("an unusable base must be refused")
	}
	probe, err := terminalAttachURL("https://panel.example.com", "/terminal/attach?token=stale&x=1")
	if err != nil {
		t.Fatal(err)
	}
	if probe.Path != "/terminal/attach" || probe.Query().Get("token") != "" || probe.Query().Get("x") != "1" {
		t.Fatalf("probe URL = %s", probe)
	}
}

// A server that carries no HTTP rung — one that predates the ladder, or a
// network that eats them — is not a failure: the WebSocket is the bottom rung,
// and errNoHTTPTransport is how the caller is told to take it.
func TestShellOverHTTPStepsDownToTheWebSocket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	client := &Client{base: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := client.shellOverHTTP(ctx, "/terminal/attach", "tk", "key", false); !errors.Is(err, errNoHTTPTransport) {
		t.Fatalf("err = %v, want errNoHTTPTransport", err)
	}
	// A mint with no attach path has nothing to probe, and steps down the same
	// way rather than failing the shell.
	if err := client.shellOverHTTP(ctx, "", "tk", "key", false); !errors.Is(err, errNoHTTPTransport) {
		t.Fatalf("err = %v, want errNoHTTPTransport", err)
	}
}

// terminalWire is the control plane's half of one HTTP-attached terminal, as
// the CLI must find it: a capability probe, a session request carrying the
// control wire, and one data stream carrying the PTY's bytes.
type terminalWire struct {
	t       *testing.T
	stream  chan net.Conn
	streams int
}

// httpWriter adapts the response half to io.WriteCloser: the handler owns the
// close, not the wire.
type httpWriter struct{ io.Writer }

func (httpWriter) Close() error { return nil }

func (s *terminalWire) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", "OPTIONS, POST, GET")
		w.Header().Set(tun.TerminalHTTP.CapabilitiesHeader, tun.TerminalHTTP.Name+",h3,h2,websocket")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Header.Get("Content-Type") {
	case tun.TerminalHTTP.ControlContentType:
		s.session(w, r)
	case tun.TerminalHTTP.StreamContentType:
		s.data(w, r)
	default:
		w.WriteHeader(http.StatusUnsupportedMediaType)
	}
}

func (s *terminalWire) session(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(tun.TerminalHTTP.ProtocolHeader) != tun.TerminalHTTP.Name {
		s.t.Errorf("protocol header = %q", r.Header.Get(tun.TerminalHTTP.ProtocolHeader))
	}
	if r.Header.Get(tun.TerminalHTTP.AttachKeyHeader) == "" {
		s.t.Error("the session request must carry the ephemeral attach key")
	}
	query := r.URL.Query()
	if query.Get("token") != "tk" || query.Get("cols") != "120" || query.Get("rows") != "40" {
		s.t.Errorf("session query = %q — the mint token and the geometry travel here", r.URL.RawQuery)
	}
	_ = http.NewResponseController(w).EnableFullDuplex()
	w.Header().Set("Content-Type", tun.TerminalHTTP.ControlContentType)
	w.Header().Set(tun.TerminalHTTP.ProtocolHeader, tun.TerminalHTTP.Name)
	w.Header().Set(tun.TerminalHTTP.SessionHeader, "session-1")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	_ = controller.Flush()

	control := tun.NewLineControl(r.Body, httpWriter{w}, controller.Flush, r.Body.Close)
	var stream net.Conn
	select {
	case stream = <-s.stream:
	case <-time.After(5 * time.Second):
		s.t.Error("the CLI never opened its data stream")
		return
	}
	if _, err := stream.Write([]byte("motd\r\n")); err != nil {
		s.t.Errorf("write to the data stream: %v", err)
		return
	}
	// Liveness rides the control wire, and the CLI's adapter answers it
	// without the session ever seeing it.
	if err := control.Send(r.Context(), tun.HTTPControlFrame{Type: "ping"}); err != nil {
		s.t.Errorf("ping: %v", err)
		return
	}
	frame, err := control.Receive()
	if err != nil || frame.Type != "pong" {
		s.t.Errorf("liveness answer = %+v, %v", frame, err)
		return
	}
	_ = control.Send(r.Context(), tun.HTTPControlFrame{Type: "end", Reason: "idle_timeout"})
	// Let the end frame land before the handler's return closes both halves.
	time.Sleep(50 * time.Millisecond)
}

func (s *terminalWire) data(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(tun.TerminalHTTP.SessionHeader) != "session-1" {
		s.t.Errorf("the data stream must present its session: %q", r.Header.Get(tun.TerminalHTTP.SessionHeader))
	}
	if r.Header.Get(tun.TerminalHTTP.AttachKeyHeader) == "" {
		s.t.Error("the data stream authenticates with the attach key alone")
	}
	s.streams++
	_ = http.NewResponseController(w).EnableFullDuplex()
	w.Header().Set("Content-Type", tun.TerminalHTTP.StreamContentType)
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	stream := tun.NewDuplexConn(r.Body, httpWriter{w}, controller.Flush, nil)
	// Keystrokes are drained, not asserted on: the pump's stdin is a pipe
	// another test may have parked bytes in.
	go func() { _, _ = io.Copy(io.Discard, stream) }()
	s.stream <- stream
	<-r.Context().Done()
}

// The whole client-side ladder over a real HTTP/2 connection: probe, session,
// its one data stream, and the pump that cannot tell which rung carried it.
func TestTerminalOverHTTP2(t *testing.T) {
	installShellStdin(t)
	wire := &terminalWire{t: t, stream: make(chan net.Conn, 1)}
	srv := httptest.NewUnstartedServer(wire)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	pool := newH2PoolWithTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	defer func() { _ = pool.Close() }()

	attach, err := terminalAttachURL(srv.URL, "/terminal/attach")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := probeTerminal(ctx, pool, attach, transportH2); err != nil {
		t.Fatalf("probe: %v", err)
	}

	key, err := tun.NewIngressAttachKey()
	if err != nil {
		t.Fatal(err)
	}
	session, err := openTerminalSession(ctx, pool, attach, "tk", key, transportH2, 120, 40)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer session.close()

	var pumpErr error
	out, errOut := captureOutput(t, func() { pumpErr = runTerminalPumps(ctx, session.conn, false) })
	if pumpErr != nil {
		t.Fatalf("pump: %v", pumpErr)
	}
	if !strings.Contains(out, "motd") {
		t.Fatalf("stdout = %q — the PTY's bytes come off the data stream", out)
	}
	if !strings.Contains(errOut, "session ended: idle_timeout") {
		t.Fatalf("stderr = %q — the end reason comes off the control wire", errOut)
	}
	if wire.streams != 1 {
		t.Fatalf("the CLI opened %d data streams — a terminal has exactly one", wire.streams)
	}
}

// Everything openTerminalSession refuses before it has a session to pump: a
// mint with no token, a server that answers 200 without echoing the wire it
// was asked for, and a session whose one data stream cannot be opened.
func TestOpenTerminalSessionRefusalsBeforeTheFirstByte(t *testing.T) {
	var echoProtocol bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") == tun.TerminalHTTP.StreamContentType {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("no capacity"))
			return
		}
		_ = http.NewResponseController(w).EnableFullDuplex()
		if echoProtocol {
			w.Header().Set(tun.TerminalHTTP.ProtocolHeader, tun.TerminalHTTP.Name)
			w.Header().Set(tun.TerminalHTTP.SessionHeader, "session-1")
		}
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	pool := newH2PoolWithTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	defer func() { _ = pool.Close() }()
	attach, err := terminalAttachURL(srv.URL, "/terminal/attach")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := openTerminalSession(ctx, pool, attach, "", "key", transportH2, 80, 24); err == nil {
		t.Fatal("a mint with no token must be refused before any request")
	}

	_, err = openTerminalSession(ctx, pool, attach, "tk", "key", transportH2, 80, 24)
	var rejection *attachRejection
	if !errors.As(err, &rejection) || !strings.Contains(rejection.message, "did not echo") {
		t.Fatalf("err = %v — a server that does not echo the wire is not speaking it", err)
	}

	echoProtocol = true
	if _, err := openTerminalSession(ctx, pool, attach, "tk", "key", transportH2, 80, 24); err == nil {
		t.Fatal("a session whose data stream is refused is no session at all")
	}
}

// A server that refuses the attach on policy grounds — an expired mint, a
// container that stopped — is the server's verdict on this session, not a
// transport that cannot carry it: the CLI reports it instead of spending a
// second token on the WebSocket.
func TestTerminalHTTPPolicyRefusalDoesNotStepDown(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set(tun.TerminalHTTP.CapabilitiesHeader, tun.TerminalHTTP.Name+",h3,h2,websocket")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("the container is not running — start it first"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	pool := newH2PoolWithTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	defer func() { _ = pool.Close() }()

	attach, err := terminalAttachURL(srv.URL, "/terminal/attach")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = openTerminalSession(ctx, pool, attach, "tk", "not-a-key", transportH2, 80, 24)
	var rejection *attachRejection
	if !errors.As(err, &rejection) {
		t.Fatalf("err = %v, want an attach rejection", err)
	}
	if rejection.transportRefused() {
		t.Fatal("a 409 is a policy verdict, not a transport refusing the tunnel")
	}
	if !strings.Contains(rejection.message, "not running") {
		t.Fatalf("the refusal must carry the server's words: %q", rejection.message)
	}
}
