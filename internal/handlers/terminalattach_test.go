package handlers

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/tunnel"
)

// ADR-027's rule, enforced rather than assumed: the terminal endpoint refuses
// another access path's content type before it looks at any token. A mint for
// a shell must not be redeemable as a TCP tunnel, and the two are told apart
// on the content type alone.
func TestTerminalAttachRefusesAnotherAccessPathsWire(t *testing.T) {
	for name, contentType := range map[string]string{
		"egress control":  tunnel.EgressHTTP.ControlContentType,
		"egress stream":   tunnel.EgressHTTP.StreamContentType,
		"ingress control": tunnel.IngressHTTP.ControlContentType,
		"ingress stream":  tunnel.IngressHTTP.StreamContentType,
		"plain json":      "application/json",
		"absent":          "",
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, terminalAttachPath+"?token=whatever", strings.NewReader(""))
			if contentType != "" {
				request.Header.Set("Content-Type", contentType)
			}
			(&API{}).TerminalAttach(recorder, request)
			if recorder.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnsupportedMediaType)
			}
			if !strings.Contains(recorder.Body.String(), tunnel.TerminalHTTP.Name) {
				t.Fatalf("the refusal must name the wire this endpoint speaks: %s", recorder.Body.String())
			}
		})
	}
}

// The probe answers what this server can carry without spending the one-time
// token — the whole point of a capability probe (ADR-061), and the reason a
// CLI can step down without burning a mint.
func TestTerminalAttachOptionsAdvertisesTheLadder(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodOptions, terminalAttachPath, nil)
	(&API{}).TerminalAttachOptions(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	capabilities := recorder.Header().Get(tunnel.TerminalHTTP.CapabilitiesHeader)
	for _, want := range []string{tunnel.TerminalHTTP.Name, "h3", "h2", "websocket"} {
		if !strings.Contains(capabilities, want) {
			t.Fatalf("capabilities %q omit %q", capabilities, want)
		}
	}
	if capabilities == recorder.Header().Get(tunnel.EgressHTTP.CapabilitiesHeader) {
		t.Fatal("the terminal must advertise on its own header, never the egress one")
	}
	if allow := recorder.Header().Get("Allow"); !strings.Contains(allow, "POST") || !strings.Contains(allow, "GET") {
		t.Fatalf("Allow = %q — the WebSocket rung must stay advertised", allow)
	}
}

// Everything the session request can refuse before it claims the token: the
// order matters, because each of these answers costs nothing and a claim is
// single-use.
func TestTerminalAttachSessionRefusalsBeforeTheClaim(t *testing.T) {
	key, _ := freshAttachKey(t)
	for name, tc := range map[string]struct {
		protocol string
		key      string
		token    string
		want     int
	}{
		"another path's protocol": {protocol: tunnel.EgressHTTP.Name, key: key, token: "tk", want: http.StatusUpgradeRequired},
		"no protocol":             {key: key, token: "tk", want: http.StatusUpgradeRequired},
		"malformed key":           {protocol: tunnel.TerminalHTTP.Name, key: "not-a-key", token: "tk", want: http.StatusBadRequest},
		"no token":                {protocol: tunnel.TerminalHTTP.Name, key: key, want: http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			target := terminalAttachPath
			if tc.token != "" {
				target += "?token=" + tc.token
			}
			request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(""))
			request.Header.Set("Content-Type", tunnel.TerminalHTTP.ControlContentType)
			if tc.protocol != "" {
				request.Header.Set(tunnel.TerminalHTTP.ProtocolHeader, tc.protocol)
			}
			request.Header.Set(tunnel.TerminalHTTP.AttachKeyHeader, tc.key)
			recorder := httptest.NewRecorder()
			// A nil Store proves the point: none of these reach the claim.
			(&API{}).TerminalAttach(recorder, request)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, tc.want, recorder.Body.String())
			}
		})
	}
}

// The data stream is authenticated by the ephemeral key alone — the mint token
// was spent by the session request — and there is exactly one of it.
func TestTerminalAttachStreamAuthenticationAndSingleness(t *testing.T) {
	api := &API{}
	key, keyHash := freshAttachKey(t)
	attach := newTerminalAttach(keyHash, nil)
	api.terminalRegister("session-1", attach)

	stream := func(sessionUUID, attachKey string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, terminalAttachPath, strings.NewReader(""))
		request.Header.Set("Content-Type", tunnel.TerminalHTTP.StreamContentType)
		request.Header.Set(tunnel.TerminalHTTP.SessionHeader, sessionUUID)
		request.Header.Set(tunnel.TerminalHTTP.AttachKeyHeader, attachKey)
		recorder := httptest.NewRecorder()
		api.TerminalAttach(recorder, request)
		return recorder
	}

	if got := stream("session-1", "not-a-key"); got.Code != http.StatusUnauthorized {
		t.Fatalf("malformed key: status = %d", got.Code)
	}
	otherKey, _ := freshAttachKey(t)
	if got := stream("session-1", otherKey); got.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status = %d", got.Code)
	}
	if got := stream("session-2", key); got.Code != http.StatusUnauthorized {
		t.Fatalf("wrong session: status = %d", got.Code)
	}

	// A session that already carries its stream refuses the second one before
	// committing a response head — an answer the client can act on, rather
	// than a stream nothing will ever read.
	attach.claimed.Store(true)
	got := stream("session-1", key)
	if got.Code != http.StatusConflict {
		t.Fatalf("second stream: status = %d, want %d", got.Code, http.StatusConflict)
	}
	if !strings.Contains(got.Body.String(), "already carries") {
		t.Fatalf("the refusal must say why: %s", got.Body.String())
	}
}

func TestTerminalLookupRequiresTheRightKey(t *testing.T) {
	api := &API{}
	_, keyHash := freshAttachKey(t)
	attach := newTerminalAttach(keyHash, nil)
	api.terminalRegister("session-1", attach)

	if got := api.terminalLookup("session-1", keyHash); got != attach {
		t.Fatal("the right session and key must resolve")
	}
	_, other := freshAttachKey(t)
	if got := api.terminalLookup("session-1", other); got != nil {
		t.Fatal("a wrong key must not resolve")
	}
	if got := api.terminalLookup("session-2", keyHash); got != nil {
		t.Fatal("a key is bound to its own session")
	}

	api.terminalRelease("session-1", attach)
	if got := api.terminalLookup("session-1", keyHash); got != nil {
		t.Fatal("a released session must not resolve")
	}
	// Releasing an attach that was replaced must not evict the replacement.
	replacement := newTerminalAttach(keyHash, nil)
	api.terminalRegister("session-1", replacement)
	api.terminalRelease("session-1", attach)
	if got := api.terminalLookup("session-1", keyHash); got != replacement {
		t.Fatal("releasing a stale attach evicted the live one")
	}
}

// A session that never gets its data stream must not sit there holding a PTY:
// the wait is bounded, and a client that vanished between the two requests
// ends the session rather than leaking it.
func TestAwaitTerminalStreamGivesUpWithItsRequest(t *testing.T) {
	attach := newTerminalAttach([32]byte{}, nil)
	local, remote := net.Pipe()
	defer func() { _ = local.Close(); _ = remote.Close() }()
	attach.stream <- local
	if got, ok := awaitTerminalStream(context.Background(), attach); !ok || got != local {
		t.Fatalf("the session must take the stream that joined it (ok=%v)", ok)
	}

	empty := newTerminalAttach([32]byte{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, ok := awaitTerminalStream(ctx, empty); ok {
		t.Fatal("a dead request must not keep waiting for a stream")
	}
	if elapsed := time.Since(start); elapsed > terminalStreamOpenTimeout {
		t.Fatalf("waited %s — the request's own end must win", elapsed)
	}
}

// finish is what releases the data request's handler; it is called from a
// defer and possibly twice.
func TestTerminalAttachFinishIsIdempotent(t *testing.T) {
	attach := newTerminalAttach([32]byte{}, nil)
	attach.finish()
	attach.finish()
	select {
	case <-attach.done:
	default:
		t.Fatal("finish must release the data request")
	}
}
