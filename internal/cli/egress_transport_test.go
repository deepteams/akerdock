package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	tun "github.com/deepteams/akerdock/internal/tunnel"
)

// The two deadlines must not race. The control plane dials the forward target
// BEFORE writing the response head, and bounds that dial at
// tun.EgressDialTimeout; a client that gives up first turns a target that never
// answered into a transport fault, which is the wrong thing to go and debug.
func TestEgressDataOpenBudgetOutlastsTheServersDial(t *testing.T) {
	if egressDataOpenTimeout <= tun.EgressDialTimeout {
		t.Fatalf("egress open budget %s must outlast the server's dial budget %s",
			egressDataOpenTimeout, tun.EgressDialTimeout)
	}
	// Ingress is the other half of the reason this is not one shared constant:
	// its dial is to the developer's own loopback and has no business waiting
	// out a WAN round trip.
	if ingressDataOpenTimeout >= egressDataOpenTimeout {
		t.Fatalf("ingress budget %s should stay tighter than egress's %s — its dial is local",
			ingressDataOpenTimeout, egressDataOpenTimeout)
	}
}

// The SESSION attach no longer pays for anything the client cannot predict:
// since ADR-066 the head is written from local state alone — a token claim and
// a few indexed reads — and every remote leg runs behind it. So the one shared
// budget is right again for every access path, and the per-path budget that
// covered someone else's SSH handshake is gone. What stays derived from a real
// server-side bound is the DATA open, because that dial is still in front of
// its own response head.
func TestSessionAttachBudgetIsSharedAgainAndDataBudgetIsNot(t *testing.T) {
	if transportAttachTimeout != 5*time.Second {
		t.Fatalf("the session attach budget moved to %s — check what else pays it", transportAttachTimeout)
	}
	// A session budget wider than the data budget would be the old bet coming
	// back: it can only be justified by work the server does before the head,
	// and ADR-066 moved all of that behind it.
	if transportAttachTimeout >= egressDataOpenTimeout {
		t.Fatalf("the session budget %s is as patient as the data budget %s — nothing before the head justifies that",
			transportAttachTimeout, egressDataOpenTimeout)
	}
}

// Once the attach request has gone out, the token is presented — and since
// ADR-065 that is no longer fatal: the same token re-presented with the same
// attach key re-takes the session. So a rung that fails to answer is a plain
// transport verdict again, and only what the server says ABOUT THE SESSION ends
// the command.
func TestOnlyATransportRefusalStepsDownTheLadder(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		refused bool
	}{
		{
			"the peer does not speak the protocol",
			&rejectedAttachError{kind: transportH3, code: http.StatusUpgradeRequired},
			true,
		},
		{
			"the rung cannot do full duplex",
			&rejectedAttachError{kind: transportH2, code: http.StatusHTTPVersionNotSupported},
			true,
		},
		{
			"the token is refused",
			&rejectedAttachError{
				kind: transportH3, code: http.StatusUnauthorized,
				message: "invalid, expired or already used tunnel token",
			},
			false,
		},
		{
			"the target is unreachable",
			&rejectedAttachError{
				kind: transportH2, code: http.StatusConflict,
				message: "the server is not reachable over SSH right now",
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rejection *rejectedAttachError
			if !errors.As(tc.err, &rejection) {
				t.Fatal("the fixture is not a rejection")
			}
			if got := rejection.transportRefused(); got != tc.refused {
				t.Fatalf("transportRefused = %v, want %v", got, tc.refused)
			}
		})
	}
	// And an open that simply never answered is not a rejection at all, so it
	// steps down instead of ending the command — the step-down ADR-064 always
	// intended and could not have while it burnt the token.
	var rejection *rejectedAttachError
	if errors.As(errors.New("HTTP/2 attach: the peer sent no response headers within 5s"), &rejection) {
		t.Fatal("a silent peer must not be read as the server's verdict on the session")
	}
}

// A tunnel that dies without a word reads as a platform bug (ADR-045 §5), and
// a container the platform itself put to sleep is the case where the developer
// is least likely to guess.
func TestForwardCloseMessageIsActionable(t *testing.T) {
	for reason, want := range map[string]string{
		"idle_timeout":   "run the command again",
		"max_duration":   "session limit",
		"grant_expired":  "request access again",
		"revoked":        "administrator",
		"target_stopped": "scale-to-zero",
	} {
		got := forwardCloseMessage(reason, "")
		if got == "" {
			t.Errorf("%q produced no message at all", reason)
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("forwardCloseMessage(%q) = %q, want it to mention %q", reason, got, want)
		}
	}
	if got := forwardCloseMessage("target_stopped", ""); !strings.Contains(got, "run the command again") {
		t.Errorf("a stopped target must say what to do next, got %q", got)
	}
	if got := forwardCloseMessage("user_close", ""); got != "" {
		t.Errorf("a deliberate close should stay silent, got %q", got)
	}
	if got := forwardCloseMessage("something_new", ""); got == "" {
		t.Error("an unrecognised reason must still be reported")
	}
}

// ADR-066: the listener opens before the target has been reached, so the
// failures that used to be a 409 at open now arrive on the session's close —
// and the server sends the sentence beside the reason precisely because the
// reason alone names a category, not a machine.
func TestForwardCloseMessageCarriesTheServersSentence(t *testing.T) {
	const sentence = "the server's agent is not connected right now"
	got := forwardCloseMessage("target_unreachable", sentence)
	if !strings.Contains(got, sentence) {
		t.Fatalf("the server's own words must reach the developer, got %q", got)
	}
	// Without one — an older control plane, or a reason sent bare — the CLI
	// still has to say something better than the enum value.
	got = forwardCloseMessage("target_unreachable", "")
	if got == "" || strings.Contains(got, "target_unreachable") {
		t.Fatalf("a bare reason needs a phrased fallback, got %q", got)
	}
	if !strings.Contains(got, "could not be reached") {
		t.Fatalf("the fallback must say what happened, got %q", got)
	}
	// ADR-067's wake: the developer stopped nothing, so scale-to-zero has to be
	// named or the failure looks like the platform breaking by itself.
	got = forwardCloseMessage("wake_failed", "")
	if !strings.Contains(got, "scale-to-zero") || !strings.Contains(got, "run the command again") {
		t.Fatalf("a failed wake must name the mechanism and the next move, got %q", got)
	}
	if got := forwardCloseMessage("wake_failed", "the resource did not become ready in time"); !strings.Contains(got, "did not become ready") {
		t.Fatalf("the server's sentence wins when it has one, got %q", got)
	}
}

// The open timer fires for any slow answer, not only for a transport out of
// stream credit. It must therefore report what was seen — no headers, this
// long — and leave the diagnosis to the status the peer eventually sends.
func TestAttachStreamOpenTimeoutStatesWhatWasObserved(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	pool := newH2PoolWithTLS(clientTLSFor(t, server))
	defer func() { _ = pool.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := openAttachStream(ctx, pool, -1, server.URL, http.Header{}, transportH3, 100*time.Millisecond)
	if err == nil {
		t.Fatal("a peer that never answers must not be waited on forever")
	}
	if !strings.Contains(err.Error(), "no response headers within") {
		t.Fatalf("the timeout must state what was observed, got: %v", err)
	}
	if strings.Contains(err.Error(), "capacity") {
		t.Fatalf("the timeout must not name one cause among several, got: %v", err)
	}
	// And it must name the transport actually in use, since that is the first
	// thing anyone reading it will go and look at.
	if !strings.Contains(err.Error(), transportH3.label()) {
		t.Fatalf("the timeout must name the transport it was waiting on, got: %v", err)
	}
}

// The bug as the developer met it: psql died, and the CLI said "HTTP/2 attach:
// no transport capacity" on a session that was HTTP/3 and had plenty. A target
// the control plane cannot reach must surface as the server's own 502, labelled
// with the transport the session really runs on.
func TestEgressStreamSurfacesTheServersDiagnosis(t *testing.T) {
	for _, kind := range []transportKind{transportH2, transportH3} {
		t.Run(string(kind), func(t *testing.T) {
			const refusal = "the target did not accept the connection: dial 10.1.2.3:5432: connect: connection refused"
			plane := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"code":"target_unreachable","message":"` + refusal + `"}`))
			})
			attach, pool := attachToFakeControlPlane(t, plane, kind)
			session := &egressSession{attach: attach, pool: pool, kind: kind, uuid: "session-1", key: "key-1"}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			conn, err := session.openStream(ctx)
			if err == nil {
				_ = conn.Close()
				t.Fatal("an unreachable target must not yield a usable stream")
			}
			var rejection *rejectedAttachError
			if !errors.As(err, &rejection) {
				t.Fatalf("want the server's verdict, got a bare transport error: %v", err)
			}
			if rejection.code != http.StatusBadGateway {
				t.Fatalf("status = %d, want %d", rejection.code, http.StatusBadGateway)
			}
			if !strings.Contains(rejection.message, "connection refused") {
				t.Fatalf("the refusal must reach the developer intact, got: %s", rejection.message)
			}
			if rejection.kind != kind {
				t.Fatalf("the error blames %s on a %s session", rejection.kind, kind)
			}
			if !strings.Contains(err.Error(), kind.label()) {
				t.Fatalf("the message must name the transport in use, got: %v", err)
			}
		})
	}
}

// clientTLSFor trusts the test server's own certificate and nothing else.
func clientTLSFor(t *testing.T, server *httptest.Server) *tls.Config {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
}

// attachToFakeControlPlane serves handler over the given rung and returns the
// attach URL and a matching pool. HTTP/3 needs its own listener: the point of
// the h3 case is that the session really runs on QUIC, so a downgraded client
// would prove nothing.
func attachToFakeControlPlane(t *testing.T, handler http.Handler, kind transportKind) (*url.URL, *httpLanePool) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	clientTLS := clientTLSFor(t, server)

	authority := strings.TrimPrefix(server.URL, "https://")
	var pool *httpLanePool
	if kind == transportH3 {
		packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		h3 := &http3.Server{Handler: handler, TLSConfig: server.TLS.Clone()}
		served := make(chan error, 1)
		go func() { served <- h3.Serve(packetConn) }()
		t.Cleanup(func() {
			_ = h3.Close()
			_ = packetConn.Close()
			select {
			case <-served:
			case <-time.After(time.Second):
			}
		})
		authority = packetConn.LocalAddr().String()
		pool = newH3PoolWithTLS(clientTLS)
	} else {
		pool = newH2PoolWithTLS(clientTLS)
	}
	t.Cleanup(func() { _ = pool.Close() })

	attach, err := attachURL("https://" + authority + "/tunnel/attach")
	if err != nil {
		t.Fatal(err)
	}
	return attach, pool
}
