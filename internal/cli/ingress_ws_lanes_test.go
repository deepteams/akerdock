// The ingress WebSocket rung as it actually runs: one attach that spends the
// mint, three more sockets joined to it, and a visitor's request relayed to the
// developer's own port.
//
// The rung matters more than its position suggests — it is what a network that
// eats HTTP/2 and QUIC leaves — and the extra lanes are the part nothing else
// looks at: they authenticate on the attach key alone, and what they must NOT
// carry is the whole of why that key exists.
//
// Every top-level identifier is prefixed iflow (concurrent-agent rule).
package cli

import (
	"context"
	"crypto/sha256"
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

	"github.com/coder/websocket"

	"github.com/deepteams/akerdock/internal/agent"
	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/proxy"
	tun "github.com/deepteams/akerdock/internal/tunnel"
)

const iflowSessionUUID = "session-ws-lanes"

// iflowAttachWatch stands between the CLI and the agent to read what each
// socket presents. A lane join is invisible from both ends otherwise: the agent
// only reports whether it accepted one, and the CLI drops the ones it could not
// open.
type iflowAttachWatch struct {
	t    *testing.T
	next http.Handler

	mu     sync.Mutex
	key    string       // the attach key the primary socket presented
	joined map[int]bool // the lanes the agent was allowed to see
	// refuseOnce answers the first lane join 401, the way the agent itself does
	// when a lane beats the primary's registration to the mutex. refuseAll
	// keeps every join out, which is what a front that will not carry a second
	// socket per session looks like from here.
	refuseOnce bool
	refuseAll  bool
	refusals   int
}

func newIflowAttachWatch(t *testing.T, next http.Handler) *iflowAttachWatch {
	t.Helper()
	return &iflowAttachWatch{t: t, next: next, joined: map[int]bool{}}
}

func (w *iflowAttachWatch) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != proxy.IngressAttachPath {
		w.next.ServeHTTP(rw, r)
		return
	}
	laneHeader := r.Header.Get(tun.IngressLaneHeader)
	if laneHeader == "" {
		if r.URL.Query().Get("token") == "" {
			w.t.Error("the primary attach is the one socket that spends the mint token")
		}
		w.mu.Lock()
		w.key = r.Header.Get(tun.IngressAttachKeyHeader)
		w.mu.Unlock()
		w.next.ServeHTTP(rw, r)
		return
	}

	lane, err := strconv.Atoi(laneHeader)
	if err != nil || lane < 1 || lane > 3 {
		w.t.Errorf("lane header = %q, want one of the three secondary lanes", laneHeader)
	}
	// The mint token was spent by the primary and is single-use. A lane that
	// carried it again would put a live secret in three more access logs and
	// prove nothing the attach key does not already prove.
	if got := r.URL.Query().Get("token"); got != "" {
		w.t.Errorf("lane %d re-presented the mint token: %q", lane, got)
	}
	w.mu.Lock()
	key := w.key
	refuse := w.refuseAll || w.refuseOnce
	if refuse {
		w.refuseOnce = false
		w.refusals++
	}
	w.mu.Unlock()
	if got := r.Header.Get(tun.IngressSessionHeader); got != iflowSessionUUID {
		w.t.Errorf("lane %d named session %q, want %q", lane, got, iflowSessionUUID)
	}
	if got := r.Header.Get(tun.IngressAttachKeyHeader); got == "" || got != key {
		w.t.Errorf("lane %d presented key %q, want the primary's %q", lane, got, key)
	}
	if refuse {
		// Exactly what the agent answers a lane that arrived before the primary
		// registered itself — the one race the join is allowed to lose.
		http.Error(rw, "unknown ingress WebSocket session", http.StatusUnauthorized)
		return
	}
	w.mu.Lock()
	w.joined[lane] = true
	w.mu.Unlock()
	w.next.ServeHTTP(rw, r)
}

func (w *iflowAttachWatch) lanes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.joined)
}

// iflowRelay stands the whole rung up: a dev server on the loopback, an agent
// serving the endpoint's public URL, and the CLI attached between them.
type iflowRelay struct {
	watch   *iflowAttachWatch
	ingress *agent.Ingress
	web     *httptest.Server
	mint    ingressMint
	local   int
}

func newIflowRelay(t *testing.T) *iflowRelay {
	t.Helper()
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "local:%s", r.URL.Path)
	}))
	devURL, err := url.Parse(dev.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(devURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	localPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	ingress := agent.NewIngress(nil)
	watch := newIflowAttachWatch(t, ingress)
	web := httptest.NewServer(watch)
	// Cleanups run LIFO and httptest.Close waits for outstanding requests, of
	// which the attach is one that only a cut ends: unblock the tunnel before
	// closing anything that serves it.
	t.Cleanup(web.Close)
	t.Cleanup(func() { ingress.Cut(iflowSessionUUID, "revoked") })
	t.Cleanup(dev.Close)

	ingress.SetRoutes([]agent.IngressRoute{{Host: "127.0.0.1", EndpointUUID: "ep-lanes"}})
	const token = "ws-lane-token"
	sum := sha256.Sum256([]byte(token))
	ingress.Expect(agentwire.IngressExpectParams{
		SessionUUID:   iflowSessionUUID,
		EndpointUUID:  "ep-lanes",
		TokenSHA256:   hex.EncodeToString(sum[:]),
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	})
	return &iflowRelay{
		watch:   watch,
		ingress: ingress,
		web:     web,
		local:   localPort,
		mint: ingressMint{
			Uuid:      iflowSessionUUID,
			Url:       "https://dev.example.com",
			AttachUrl: "ws" + strings.TrimPrefix(web.URL, "http") + proxy.IngressAttachPath,
			Token:     token,
		},
	}
}

// visit is one visitor request against the endpoint's public URL.
func (r *iflowRelay) visit(ctx context.Context, t *testing.T, path string) string {
	t.Helper()
	status, body := r.tryVisit(ctx, t, path)
	if status != http.StatusOK {
		t.Fatalf("visitor %s: status %d (%s)", path, status, body)
	}
	return body
}

func (r *iflowRelay) tryVisit(ctx context.Context, t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.web.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.web.Client().Do(req)
	if err != nil {
		t.Fatalf("visitor %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// waitAttached blocks until the endpoint has a laptop behind it. The agent
// answers 503 until then, and a visitor that arrives first would be measuring
// the test's own start-up.
func (r *iflowRelay) waitAttached(ctx context.Context, t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if status, _ := r.tryVisit(ctx, t, "/ready"); status == http.StatusOK {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the CLI never attached to the endpoint")
}

// The rung end to end: the primary spends the mint, three lanes join it on the
// attach key alone, visitors reach the developer's port, and the agent's cut
// reaches the CLI as the reason it stops.
func TestIflowWebSocketRelayJoinsItsLanesAndCarriesVisitors(t *testing.T) {
	relay := newIflowRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	attached := make(chan struct {
		reason string
		err    error
	}, 1)
	go func() {
		reason, err := (&Client{}).attachIngressWebSocket(ctx, relay.mint, relay.local)
		attached <- struct {
			reason string
			err    error
		}{reason, err}
	}()

	// The lanes are joined before the bridge starts relaying, so a visitor that
	// answers proves the session is up — but the count is what proves the joins
	// happened at all, and it is the thing a broken header would silently lose.
	deadline := time.Now().Add(15 * time.Second)
	for relay.watch.lanes() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := relay.watch.lanes(); got != 3 {
		t.Fatalf("%d extra lanes joined, want 3 — a v2 session runs on four sockets", got)
	}

	// More requests than sockets: the relay must spread them, not serialise on
	// the one the primary opened.
	for i := range 8 {
		path := fmt.Sprintf("/asset-%d.js", i)
		if got := relay.visit(ctx, t, path); got != "local:"+path {
			t.Fatalf("visitor %s got %q", path, got)
		}
	}

	if !relay.ingress.Cut(iflowSessionUUID, "revoked") {
		t.Fatal("the agent has no live session — the CLI never attached")
	}
	select {
	case result := <-attached:
		if result.err != nil {
			t.Fatalf("attach: %v", result.err)
		}
		// The end reason is deliberately NOT asserted against the real agent:
		// it does not currently reach an attached CLI over this rung at all
		// (reported separately — the agent's close frame never makes it onto
		// the wire, on either subprotocol). Asserting the value it happens to
		// produce today would turn that defect into a specification. What the
		// CLI itself does with a close frame that DOES arrive is pinned below.
	case <-ctx.Done():
		t.Fatal("the cut never reached the CLI")
	}
}

// ADR-060 §6 turns entirely on this string: a policy close ends the command and
// a transport drop re-dials, and the only thing that tells them apart is the
// reason read off the close frame. The v2 rung wraps its socket in a lane group
// before anything reads it, so the frame crosses one more layer than the reason
// this pins — and a layer that swallowed it would leave the CLI re-dialling
// through a revocation.
func TestIflowV2CloseReasonSurvivesTheLaneGroup(t *testing.T) {
	for _, subprotocol := range []string{tun.IngressWebSocketV2, ingressSubprotocol} {
		t.Run(subprotocol, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(tun.IngressLaneHeader) != "" {
					// A peer that carries no extra lane: the tunnel is degraded,
					// and the close still has to be readable.
					http.Error(w, "no lanes here", http.StatusForbidden)
					return
				}
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
					Subprotocols: []string{subprotocol},
				})
				if err != nil {
					return
				}
				_ = conn.Close(websocket.StatusNormalClosure, "max_duration")
			}))
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			reason, err := (&Client{}).attachIngressWebSocket(ctx, ingressMint{
				Uuid:      iflowSessionUUID,
				AttachUrl: "ws" + strings.TrimPrefix(srv.URL, "http") + "/attach",
				Token:     "tk",
			}, 3000)
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			if reason != "max_duration" {
				t.Fatalf("close reason = %q, want the server's own", reason)
			}
			if !isPolicyClose(reason) {
				t.Fatalf("%q did not read as a policy close — the relay would re-dial through it", reason)
			}
		})
	}
}

// The lanes are an optimisation, and a laptop that cannot open them must still
// have a working tunnel: a front that refuses the second socket per session
// would otherwise take the whole ingress down rather than slow it.
func TestIflowRelaySurvivesLanesItCannotJoin(t *testing.T) {
	relay := newIflowRelay(t)
	relay.watch.refuseAll = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	attached := make(chan string, 1)
	go func() {
		reason, err := (&Client{}).attachIngressWebSocket(ctx, relay.mint, relay.local)
		if err != nil {
			t.Errorf("a tunnel with no extra lane is degraded, not broken: %v", err)
		}
		attached <- reason
	}()

	relay.waitAttached(ctx, t)
	if got := relay.visit(ctx, t, "/only-lane.js"); got != "local:/only-lane.js" {
		t.Fatalf("visitor got %q — the primary lane alone must carry the relay", got)
	}
	if got := relay.watch.lanes(); got != 0 {
		t.Fatalf("%d lanes joined — the fixture stopped testing the degraded path", got)
	}

	relay.ingress.Cut(iflowSessionUUID, "revoked")
	select {
	case <-attached:
	case <-ctx.Done():
		t.Fatal("the cut never reached the CLI")
	}
}

// A lane join races the primary's registration on the agent's own mutex, and
// the loser is answered 401. Giving up there would silently run the tunnel on
// fewer sockets for its whole life, so that one status — and only that one — is
// retried.
func TestIflowLaneJoinRetriesTheRegistrationRace(t *testing.T) {
	relay := newIflowRelay(t)
	relay.watch.refuseOnce = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	attached := make(chan string, 1)
	go func() {
		reason, _ := (&Client{}).attachIngressWebSocket(ctx, relay.mint, relay.local)
		attached <- reason
	}()

	deadline := time.Now().Add(15 * time.Second)
	for relay.watch.lanes() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := relay.watch.lanes(); got != 3 {
		t.Fatalf("%d lanes joined after one refusal, want 3 — the race is retried, not conceded", got)
	}
	relay.watch.mu.Lock()
	refusals := relay.watch.refusals
	relay.watch.mu.Unlock()
	if refusals != 1 {
		t.Fatalf("the watch refused %d joins — the fixture stopped testing the retry", refusals)
	}

	relay.ingress.Cut(iflowSessionUUID, "idle_timeout")
	select {
	case <-attached:
	case <-ctx.Done():
		t.Fatal("the cut never reached the CLI")
	}
}

// The attach URL is where a mint response stops being trusted prose. Both
// refusals are a server that answered something no socket can be opened on, and
// the CLI must say so rather than dial a URL it has just failed to build.
func TestIflowAttachURLRefusesAnUnusableMint(t *testing.T) {
	if _, err := ingressAttachURL(ingressMint{AttachUrl: "://nope", Token: "tk"}); err == nil {
		t.Fatal("an unparsable attach URL must be refused")
	}
	if _, err := ingressAttachURL(ingressMint{AttachUrl: "wss://dev.example.com/attach"}); err == nil {
		t.Fatal("a mint with no attach token must be refused")
	}
	// The separately returned token is the authority and replaces a stale one
	// left in the URL — the mint response is written twice and only one half is
	// guaranteed fresh.
	got, err := ingressAttachURL(ingressMint{AttachUrl: "wss://dev.example.com/attach?token=stale", Token: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("token") != "fresh" {
		t.Fatalf("attach URL = %s", got)
	}
	// A mint that carries its token only in the URL still attaches: it is what
	// a control plane older than the separate field sends.
	if _, err := ingressAttachURL(ingressMint{AttachUrl: "wss://dev.example.com/attach?token=carried"}); err != nil {
		t.Fatalf("a token carried in the URL is still a token: %v", err)
	}
}

// A session whose attach URL cannot be built must fail the attach, not dial an
// empty string and report a transport error for a mint the CLI already knew was
// unusable.
func TestIflowWebSocketAttachRefusesAnUnusableMint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := (&Client{}).attachIngressWebSocket(ctx, ingressMint{AttachUrl: "://nope"}, 3000)
	if err == nil {
		t.Fatal("an unusable attach URL must fail before any socket is opened")
	}
}
