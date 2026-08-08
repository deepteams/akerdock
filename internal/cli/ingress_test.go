package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	tun "github.com/deepteams/akerdock/internal/tunnel"
)

func TestSleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Fatal("an undisturbed sleep reports true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("a cancelled sleep reports false")
	}
}

func TestParseLocalPort(t *testing.T) {
	for in, want := range map[string]int{"3000": 3000, "1": 1, "65535": 65535} {
		got, err := parseLocalPort(in)
		if err != nil || got != want {
			t.Errorf("parseLocalPort(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"abc", "0", "65536", "-1", ""} {
		if _, err := parseLocalPort(in); err == nil {
			t.Errorf("parseLocalPort(%q) should fail", in)
		}
	}
}

func TestNextBackoff(t *testing.T) {
	if got := nextBackoff(time.Second); got != 2*time.Second {
		t.Fatalf("got %v", got)
	}
	if got := nextBackoff(20 * time.Second); got != 30*time.Second {
		t.Fatalf("cap not applied: %v", got)
	}
}

func TestIsPolicyClose(t *testing.T) {
	for _, reason := range []string{"idle_timeout", "max_duration", "revoked", "user_close"} {
		if !isPolicyClose(reason) {
			t.Errorf("%q is a policy close", reason)
		}
	}
	for _, reason := range []string{"", "disconnect", "anything"} {
		if isPolicyClose(reason) {
			t.Errorf("%q is not a policy close", reason)
		}
	}
}

func TestIngressCloseMessage(t *testing.T) {
	for reason, want := range map[string]string{
		"idle_timeout": "run the command again",
		"max_duration": "12-hour",
		"revoked":      "administrator",
		"weird":        "tunnel closed (weird)",
	} {
		if got := ingressCloseMessage(reason); !strings.Contains(got, want) {
			t.Errorf("ingressCloseMessage(%q) = %q, want %q", reason, got, want)
		}
	}
	if got := ingressCloseMessage("user_close"); got != "" {
		t.Errorf("deliberate close should stay silent, got %q", got)
	}
	if got := ingressCloseMessage(""); got != "" {
		t.Errorf("bare transport drop has its own line, got %q", got)
	}
}

func TestIngressCmdArgumentErrors(t *testing.T) {
	t.Run("without a client", func(t *testing.T) {
		setupHome(t)
		if err := runCmd(ingressCmd(), "dev", "3000"); err == nil {
			t.Fatal("expected a client error")
		}
	})
	t.Run("bad port", func(t *testing.T) {
		setupContext(t, "http://127.0.0.1:1")
		if err := runCmd(ingressCmd(), "dev", "abc"); err == nil || !strings.Contains(err.Error(), "invalid local port") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("wrong arity", func(t *testing.T) {
		setupContext(t, "http://127.0.0.1:1")
		if err := runCmd(ingressCmd(), "dev"); err == nil || !strings.Contains(err.Error(), "usage: akerdock ingress") {
			t.Fatalf("err = %v", err)
		}
	})
}

// ingressServer fakes the resolve + mint + attach choreography. mint decides
// per call what to answer; attach handles the websocket side.
func ingressServer(t *testing.T, mint func(call int, w http.ResponseWriter), attach http.HandlerFunc, closeSession ...http.HandlerFunc) *httptest.Server {
	t.Helper()
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ingress-endpoints", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"ig-1","name":"dev-kedric"}]}`))
	})
	mux.HandleFunc("/api/v1/ingress-endpoints/ig-1/tunnels", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		mint(calls, w)
	})
	if attach != nil {
		mux.HandleFunc("/attach", attach)
	}
	if len(closeSession) > 0 {
		mux.HandleFunc("/api/v1/ingress-tunnel-sessions/", closeSession[0])
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func mintSession(t *testing.T, srvURL string) func(int, http.ResponseWriter) {
	t.Helper()
	return func(_ int, w http.ResponseWriter) {
		attachURL := "ws" + strings.TrimPrefix(srvURL, "http") + "/attach"
		_, _ = fmt.Fprintf(w, `{"uuid":"ig-session-1","url":"https://dev.example.com","attach_url":%q,"token":"tk"}`, attachURL)
	}
}

// A policy close ends the relay for good: the CLI explains it and exits
// instead of re-dialing through the control (ADR-060 §6).
func TestIngressPolicyCloseEndsTheRelay(t *testing.T) {
	var srvURL string
	attach := func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("token"); got != "tk" {
			t.Errorf("attach token = %q, want tk", got)
		}
		if !strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), ingressSubprotocol) {
			t.Errorf("attach without the ingress subprotocol: %q", r.Header.Get("Sec-WebSocket-Protocol"))
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ingressSubprotocol}})
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "revoked")
	}
	srv := ingressServer(t, func(call int, w http.ResponseWriter) { mintSession(t, srvURL)(call, w) }, attach)
	srvURL = srv.URL
	setupContext(t, srv.URL)

	var err error
	_, errOut := captureOutput(t, func() {
		err = runCmd(ingressCmd(), "dev-kedric", "3000")
	})
	if err != nil {
		t.Fatalf("ingress: %v", err)
	}
	if !strings.Contains(errOut, "relaying https://dev.example.com -> 127.0.0.1:3000") {
		t.Fatalf("stderr = %q", errOut)
	}
	if !strings.Contains(errOut, "administrator closed it") {
		t.Fatalf("stderr misses the close reason: %q", errOut)
	}
}

// A failed WebSocket handshake must release the just-minted durable session
// before the reconnect mints another one. Otherwise the CLI rejects itself as
// the endpoint's occupant.
func TestIngressFailedAttachClosesMintBeforeReconnect(t *testing.T) {
	var srvURL string
	var attachCalls atomic.Int32
	var closedFirst atomic.Bool
	srv := ingressServer(t, func(call int, w http.ResponseWriter) {
		if call == 2 && !closedFirst.Load() {
			t.Error("second mint arrived before the failed first mint was closed")
		}
		attachURL := "ws" + strings.TrimPrefix(srvURL, "http") + "/attach"
		_, _ = fmt.Fprintf(w,
			`{"uuid":"ig-session-%d","url":"https://dev.example.com","attach_url":%q,"token":"tk-%d"}`,
			call, attachURL, call)
	}, func(w http.ResponseWriter, r *http.Request) {
		switch attachCalls.Add(1) {
		case 1:
			http.Error(w, "temporary attach failure", http.StatusUnauthorized)
		case 2:
			if got := r.URL.Query().Get("token"); got != "tk-2" {
				t.Errorf("reconnect token = %q, want tk-2", got)
			}
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ingressSubprotocol}})
			if err != nil {
				return
			}
			_ = conn.Close(websocket.StatusNormalClosure, "revoked")
		default:
			t.Error("unexpected extra attach")
		}
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("cleanup method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/api/v1/ingress-tunnel-sessions/ig-session-1" {
			t.Errorf("cleanup path = %q", r.URL.Path)
		}
		closedFirst.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})
	srvURL = srv.URL
	setupContext(t, srv.URL)

	var err error
	_, errOut := captureOutput(t, func() {
		err = runCmd(ingressCmd(), "dev-kedric", "3000")
	})
	if err != nil {
		t.Fatalf("ingress: %v", err)
	}
	if !closedFirst.Load() {
		t.Fatal("failed mint was not closed")
	}
	if !strings.Contains(errOut, "tunnel dropped") {
		t.Fatalf("stderr misses the reconnect notice: %q", errOut)
	}
}

func TestIngressOccupiedIsFatal(t *testing.T) {
	srv := ingressServer(t, func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"occupied","message":"someone is already attached"}`))
	}, nil)
	setupContext(t, srv.URL)
	var err error
	_, _ = captureOutput(t, func() {
		err = runCmd(ingressCmd(), "dev-kedric", "3000")
	})
	if err == nil || !strings.Contains(err.Error(), "someone is already attached") {
		t.Fatalf("err = %v", err)
	}
}

// A failed mint is retried with backoff; a transport-level attach failure
// reconnects. Both paths funnel into the occupied answer to end the test.
func TestIngressRetriesThenReconnects(t *testing.T) {
	var srvURL string
	srv := ingressServer(t, func(call int, w http.ResponseWriter) {
		switch call {
		case 1: // transient mint failure: retry
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"boom","message":"transient"}`))
		case 2: // session whose attach URL nobody answers: transport drop, reconnect
			_, _ = fmt.Fprint(w, `{"url":"https://dev.example.com","attach_url":"ws://127.0.0.1:1/attach","token":"tk"}`)
		default: // stop the loop deterministically
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"occupied","message":"stop here"}`))
		}
	}, nil)
	srvURL = srv.URL
	_ = srvURL
	setupContext(t, srv.URL)

	var err error
	_, errOut := captureOutput(t, func() {
		err = runCmd(ingressCmd(), "dev-kedric", "3000")
	})
	if err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut, "retrying in 1s") {
		t.Fatalf("stderr misses the mint retry: %q", errOut)
	}
	if !strings.Contains(errOut, "tunnel dropped") {
		t.Fatalf("stderr misses the reconnect notice: %q", errOut)
	}
}

// An attach refused with a JSON reason (e.g. sessions disabled) surfaces that
// sentence through the reconnect notice.
func TestIngressAttachRefusedWithReason(t *testing.T) {
	var srvURL string
	srv := ingressServer(t, func(call int, w http.ResponseWriter) {
		if call == 1 {
			mintSession(t, srvURL)(call, w)
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"occupied","message":"stop here"}`))
	}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"tunnel sessions are disabled"}`))
	})
	srvURL = srv.URL
	setupContext(t, srv.URL)

	var err error
	_, errOut := captureOutput(t, func() {
		err = runCmd(ingressCmd(), "dev-kedric", "3000")
	})
	if err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut, "tunnel sessions are disabled") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// Ctrl-C during the mint (or during the retry backoff) is a clean exit, not
// an error to report.
func TestIngressCancelDuringMint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := ingressServer(t, func(_ int, w http.ResponseWriter) {
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
	}, nil)
	setupContext(t, srv.URL)
	var err error
	_, _ = captureOutput(t, func() {
		err = runCmdCtx(ctx, ingressCmd(), "dev-kedric", "3000")
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestIngressCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := ingressServer(t, func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"boom","message":"transient"}`))
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
	}, nil)
	setupContext(t, srv.URL)
	var err error
	_, errOut := captureOutput(t, func() {
		err = runCmdCtx(ctx, ingressCmd(), "dev-kedric", "3000")
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut, "retrying") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestIngressResolveError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	setupContext(t, srv.URL)
	if err := runCmd(ingressCmd(), "dev-kedric", "3000"); err == nil {
		t.Fatal("expected the resolve error to surface")
	}
}

// ingressClientConn adapts coder/websocket to the tunnel.Conn contract; the
// close-frame reason must be captured on the way out.
func TestIngressClientConn(t *testing.T) {
	serverConnCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		serverConnCh <- conn
		// Keep the handler alive until the test is done with the socket.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx := context.Background()
	clientWS, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientWS.CloseNow() }()
	server := <-serverConnCh

	var reason string
	cc := ingressClientConn{clientWS, &reason}

	// Both frame kinds cross the adapter in both directions.
	if err := server.Write(ctx, websocket.MessageText, []byte("ctrl")); err != nil {
		t.Fatal(err)
	}
	typ, data, err := cc.Read(ctx)
	if err != nil || typ != tun.MessageText || string(data) != "ctrl" {
		t.Fatalf("typ=%v data=%q err=%v", typ, data, err)
	}
	if err := server.Write(ctx, websocket.MessageBinary, []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	typ, _, err = cc.Read(ctx)
	if err != nil || typ != tun.MessageBinary {
		t.Fatalf("typ=%v err=%v", typ, err)
	}
	if err := cc.Write(ctx, tun.MessageText, []byte("up")); err != nil {
		t.Fatal(err)
	}
	if typ, data, err := server.Read(ctx); err != nil || typ != websocket.MessageText || string(data) != "up" {
		t.Fatalf("server got typ=%v data=%q err=%v", typ, data, err)
	}
	if err := cc.Write(ctx, tun.MessageBinary, []byte{3}); err != nil {
		t.Fatal(err)
	}
	if typ, _, err := server.Read(ctx); err != nil || typ != websocket.MessageBinary {
		t.Fatalf("server got typ=%v err=%v", typ, err)
	}

	// A ping needs both peers reading: control frames are only processed
	// inside a pending Read. Park one on each side, then ping.
	type readResult struct {
		err error
	}
	clientRead := make(chan readResult, 1)
	go func() {
		_, _, err := cc.Read(ctx)
		clientRead <- readResult{err}
	}()
	go func() { _, _, _ = server.Read(ctx) }()
	if err := cc.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// A clean close surfaces as ErrClientClosed and captures the reason.
	if err := server.Close(websocket.StatusGoingAway, "maintenance"); err != nil {
		t.Fatal(err)
	}
	if res := <-clientRead; res.err != tun.ErrClientClosed {
		t.Fatalf("err = %v, want ErrClientClosed", res.err)
	}
	if reason != "maintenance" {
		t.Fatalf("reason = %q", reason)
	}
}

func TestIngressClientConnNonCloseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		<-r.Context().Done()
	}))
	defer srv.Close()
	clientWS, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientWS.CloseNow() }()
	var reason string
	cc := ingressClientConn{clientWS, &reason}
	// A cancelled read is a transport error, not a close: no reason captured.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := cc.Read(ctx); err == nil || err == tun.ErrClientClosed {
		t.Fatalf("err = %v", err)
	}
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
}
