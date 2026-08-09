package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/tunnel"
)

func freshAttachKey(t *testing.T) (string, [sha256.Size]byte) {
	t.Helper()
	raw := make([]byte, sha256.Size)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	return encoded, sha256.Sum256(raw)
}

// ADR-027's rule, enforced rather than assumed: a request carrying another
// access path's content type is refused here. An attach token minted for a
// laptop's public URL must not be usable to open a TCP tunnel into a database,
// and the endpoints tell the two apart on the content type alone — before any
// token is even looked at.
func TestTunnelAttachRefusesAnotherAccessPathsWire(t *testing.T) {
	for name, contentType := range map[string]string{
		"ingress control": tunnel.IngressHTTP.ControlContentType,
		"ingress stream":  tunnel.IngressHTTP.StreamContentType,
		"terminal":        tunnel.TerminalHTTP.ControlContentType,
		"plain json":      "application/json",
		"absent":          "",
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, tunnelAttachPath+"?token=whatever", strings.NewReader(""))
			if contentType != "" {
				request.Header.Set("Content-Type", contentType)
			}
			(&API{}).TunnelAttach(recorder, request)
			if recorder.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnsupportedMediaType)
			}
			if !strings.Contains(recorder.Body.String(), tunnel.EgressHTTP.Name) {
				t.Fatalf("the refusal must name the wire this endpoint speaks: %s", recorder.Body.String())
			}
		})
	}
}

// The probe answers what this server can carry without spending the one-time
// token — the whole point of a capability probe (ADR-061).
func TestTunnelAttachOptionsAdvertisesTheLadder(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodOptions, tunnelAttachPath, nil)
	(&API{}).TunnelAttachOptions(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	capabilities := recorder.Header().Get(tunnel.EgressHTTP.CapabilitiesHeader)
	for _, want := range []string{tunnel.EgressHTTP.Name, "h3", "h2", "websocket"} {
		if !strings.Contains(capabilities, want) {
			t.Fatalf("capabilities %q omit %q", capabilities, want)
		}
	}
	if allow := recorder.Header().Get("Allow"); !strings.Contains(allow, "POST") || !strings.Contains(allow, "GET") {
		t.Fatalf("Allow = %q — the WebSocket rung must stay advertised", allow)
	}
}

// A data stream is authenticated by the ephemeral key alone: the mint token was
// spent by the session request. A wrong key, a wrong session or a session that
// ended must all be refused, and the comparison must not leak by timing.
func TestEgressLookupRequiresTheRightKey(t *testing.T) {
	api := &API{}
	_, key := freshAttachKey(t)
	attach := &egressAttach{key: key}
	api.egressRegister("session-1", attach)

	if got := api.egressLookup("session-1", key); got != attach {
		t.Fatal("the right session and key must resolve")
	}
	_, other := freshAttachKey(t)
	if got := api.egressLookup("session-1", other); got != nil {
		t.Fatal("a wrong key must not resolve")
	}
	if got := api.egressLookup("session-2", key); got != nil {
		t.Fatal("a key is bound to its own session")
	}

	api.egressRelease("session-1", attach)
	if got := api.egressLookup("session-1", key); got != nil {
		t.Fatal("a released session must not resolve")
	}
	// Releasing an attach that was replaced must not evict the replacement.
	replacement := &egressAttach{key: key}
	api.egressRegister("session-1", replacement)
	api.egressRelease("session-1", attach)
	if got := api.egressLookup("session-1", key); got != replacement {
		t.Fatal("releasing a stale attach evicted the live one")
	}
}

func TestDecodeAttachKeyRejectsAnythingButA256BitKey(t *testing.T) {
	valid, want := freshAttachKey(t)
	got, err := decodeAttachKey(valid)
	if err != nil || got != want {
		t.Fatalf("decode = %x, %v", got, err)
	}
	for name, value := range map[string]string{
		"empty":      "",
		"not base64": "!!!!",
		"too short":  base64.RawURLEncoding.EncodeToString([]byte("short")),
		"padded":     base64.URLEncoding.EncodeToString(make([]byte, sha256.Size)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeAttachKey(value); err == nil {
				t.Fatal("accepted an invalid attach key")
			}
		})
	}
}

func TestBaseContentTypeIgnoresParameters(t *testing.T) {
	for value, want := range map[string]string{
		tunnel.EgressHTTP.ControlContentType:                     tunnel.EgressHTTP.ControlContentType,
		tunnel.EgressHTTP.ControlContentType + "; charset=utf-8": tunnel.EgressHTTP.ControlContentType,
		"  " + tunnel.EgressHTTP.StreamContentType + " ;v=1":     tunnel.EgressHTTP.StreamContentType,
		"": "",
	} {
		if got := baseContentType(value); got != want {
			t.Fatalf("baseContentType(%q) = %q, want %q", value, got, want)
		}
	}
}

// The field failure this exists for: psql printed "server closed the connection
// unexpectedly" and the CLI blamed its own transport, because the dial to the
// target ran unbounded BEFORE the response head and its error was thrown away.
// A target that refuses must produce a 502 carrying the dial's own words —
// early enough that the CLI still has a request to read it on.
func TestTunnelAttachStreamAnswersBadGatewayWhenTheTargetIsUnreachable(t *testing.T) {
	encodedKey, key := freshAttachKey(t)
	api := &API{}
	var dialDeadline time.Time
	var dialed bool
	api.egressRegister("session-1", &egressAttach{
		key: key,
		dial: func(ctx context.Context) (net.Conn, error) {
			dialed = true
			dialDeadline, _ = ctx.Deadline()
			return nil, errors.New("dial 10.1.2.3:5432: connect: connection refused")
		},
		// Left nil on purpose: a dial that fails must be answered before the
		// session is ever asked to admit anything.
		session: nil,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, tunnelAttachPath, strings.NewReader(""))
	request.Header.Set("Content-Type", tunnel.EgressHTTP.StreamContentType)
	request.Header.Set(tunnel.EgressHTTP.AttachKeyHeader, encodedKey)
	request.Header.Set(tunnel.EgressHTTP.SessionHeader, "session-1")
	api.tunnelAttachStream(recorder, request)

	if !dialed {
		t.Fatal("the stream did not even dial the target")
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d — a target that refuses is not a transport fault", recorder.Code, http.StatusBadGateway)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "connection refused") || !strings.Contains(body, "10.1.2.3:5432") {
		t.Fatalf("the 502 must carry the dial's own words, got: %s", body)
	}
	if !strings.Contains(body, "target_unreachable") {
		t.Fatalf("the 502 must be typed so the CLI can tell it apart, got: %s", body)
	}
	// Bounded, and bounded by the value the CLI derives its own patience from:
	// an unbounded dial is what spent the client's budget in the first place.
	if dialDeadline.IsZero() {
		t.Fatal("the dial ran unbounded — it must not outlast the client's open budget")
	}
	if remaining := time.Until(dialDeadline); remaining > tunnel.EgressDialTimeout {
		t.Fatalf("dial budget = %s, want at most %s", remaining, tunnel.EgressDialTimeout)
	}
}

// A target that never answers must not hold the stream open past the request:
// the dial follows the request's context, so a client that walks away frees it.
func TestTunnelAttachStreamAnswersWhenTheTargetNeverReplies(t *testing.T) {
	encodedKey, key := freshAttachKey(t)
	api := &API{}
	api.egressRegister("session-1", &egressAttach{
		key: key,
		dial: func(ctx context.Context) (net.Conn, error) {
			<-ctx.Done()
			return nil, fmt.Errorf("dial 10.1.2.3:5432: %w", ctx.Err())
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, tunnelAttachPath, strings.NewReader("")).WithContext(ctx)
	request.Header.Set("Content-Type", tunnel.EgressHTTP.StreamContentType)
	request.Header.Set(tunnel.EgressHTTP.AttachKeyHeader, encodedKey)
	request.Header.Set(tunnel.EgressHTTP.SessionHeader, "session-1")

	answered := make(chan struct{})
	go func() {
		api.tunnelAttachStream(recorder, request)
		close(answered)
	}()
	select {
	case <-answered:
	case <-time.After(2 * time.Second):
		t.Fatal("a target that never answers stalled the stream instead of failing it")
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "10.1.2.3:5432") {
		t.Fatalf("the 502 must name what could not be reached, got: %s", body)
	}
}

// The whole point of wrapping ssh.Client.Dial: the caller stops waiting, and
// the connection that turns up afterwards is closed rather than leaked — one
// abandoned SSH channel per attempt is how a slow target exhausts a session.
func TestDialTCPContextAbandonsAndClosesALateConnection(t *testing.T) {
	release := make(chan struct{})
	late, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	dialer := dialerFunc(func(addr string) (net.Conn, error) {
		<-release
		return late, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	conn, err := dialTCPContext(ctx, dialer, "10.1.2.3:5432")
	if conn != nil || err == nil {
		t.Fatalf("dial = %v, %v — an expired context must abandon the dial", conn, err)
	}
	if !strings.Contains(err.Error(), "10.1.2.3:5432") {
		t.Fatalf("the abandonment must name the target, got: %v", err)
	}

	close(release)
	// The connection the goroutine finally receives belongs to nobody: it must
	// be closed, which the peer sees as its own read failing.
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, readErr := peer.Read(make([]byte, 1)); readErr == nil {
		t.Fatal("the late connection was left open")
	}
}

type dialerFunc func(addr string) (net.Conn, error)

func (f dialerFunc) DialTCP(addr string) (net.Conn, error) { return f(addr) }

// A forwarded connection must not outlive the session that authorized it
// (§24.4): the splice watches the session's end, not only its two conns.
func TestSpliceConnsEndsWithTheSession(t *testing.T) {
	left, leftPeer := net.Pipe()
	right, rightPeer := net.Pipe()
	defer func() { _ = leftPeer.Close(); _ = rightPeer.Close() }()

	sessionDone := make(chan struct{})
	spliced := make(chan struct{})
	go func() {
		spliceConns(context.Background(), sessionDone, left, right)
		close(spliced)
	}()

	// Bytes cross while the session is alive.
	go func() { _, _ = leftPeer.Write([]byte("ping")) }()
	got := make([]byte, 4)
	if _, err := io.ReadFull(rightPeer, got); err != nil || string(got) != "ping" {
		t.Fatalf("relayed %q, %v", got, err)
	}

	close(sessionDone)
	select {
	case <-spliced:
	case <-time.After(2 * time.Second):
		t.Fatal("the splice outlived its session")
	}
	if _, err := leftPeer.Write([]byte("x")); err == nil {
		t.Fatal("the forwarded connection is still open after the session ended")
	}
}
