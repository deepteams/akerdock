package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

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

// TestIngressRelayDirectorAndErrorHandler pins the relay proxy's two
// callbacks without a live tunnel: the director rewrites only the URL (the
// public Host survives for the developer's app), and a failed dial answers
// 502 with a human cause.
func TestIngressRelayDirectorAndErrorHandler(t *testing.T) {
	ig := NewIngress(nil)
	rp, ok := ig.newRelay(nil).(*httputil.ReverseProxy)
	if !ok {
		t.Fatal("newRelay must build a ReverseProxy")
	}

	req := httptest.NewRequest(http.MethodGet, "http://dev.example.com/hook?x=1", nil)
	req.Host = "dev.example.com"
	rp.Director(req)
	if req.URL.Scheme != "http" || req.URL.Host != "ingress" {
		t.Fatalf("directed URL = %s, want the placeholder target", req.URL)
	}
	if req.Host != "dev.example.com" {
		t.Fatalf("req.Host = %q, want the public host preserved", req.Host)
	}

	rec := httptest.NewRecorder()
	rp.ErrorHandler(rec, req, errors.New("mux stream refused"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("relay failure = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "did not answer") {
		t.Fatalf("relay failure body = %q", rec.Body.String())
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
