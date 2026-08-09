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

// The SESSION attach pays for work no transport knows about: the control plane
// claims the single-use mint token, then inspects the container over the agent
// channel and dials a FRESH SSH connection — nothing pools them — all before it
// writes the response head. A budget shorter than that makes the two deadlines
// race, and the loser is the developer: the CLI walks away, the token stays
// burnt, and every later attempt is refused.
func TestEgressAttachBudgetOutlastsTheServersResolution(t *testing.T) {
	if egressAttachTimeout <= egressAttachSSHBudget+egressAttachAgentRound {
		t.Fatalf("egress attach budget %s leaves no WAN margin over the server's %s of work",
			egressAttachTimeout, egressAttachSSHBudget+egressAttachAgentRound)
	}
	// The whole point of a per-access-path budget: the shared one is what made
	// the two deadlines race, and it must stay visibly too small to be reused
	// here by accident.
	if transportAttachTimeout >= egressAttachSSHBudget {
		t.Fatalf("the shared attach budget %s would still race the server's %s SSH dial",
			transportAttachTimeout, egressAttachSSHBudget)
	}
	// …and it stays the default for the paths whose peer answers out of its own
	// memory, where waiting out a WAN SSH handshake would only delay the truth.
	if transportAttachTimeout != 5*time.Second {
		t.Fatalf("the default attach budget moved to %s — check what else pays it", transportAttachTimeout)
	}
}

// Once the attach request has gone out, the token is presented and the server
// claims it before resolving anything. What the failure means for the ladder is
// therefore never "the transport failed, step down" by default: descending on a
// burnt token wastes the two lower rungs and ends on a 401 whose sentence blames
// the developer for the ladder's own impatience.
func TestAttachFailureClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want attachVerdict
	}{
		{
			"the open timed out",
			errors.New("HTTP/2 attach: the peer sent no response headers within 45s"),
			attachSpent,
		},
		{
			"the server says the token is already used",
			&attachRejection{kind: transportH3, code: http.StatusUnauthorized,
				message: "invalid, expired or already used tunnel token"},
			attachSpent,
		},
		{
			"the peer does not speak the protocol",
			&attachRejection{kind: transportH3, code: http.StatusUpgradeRequired},
			attachDescend,
		},
		{
			"the rung cannot do full duplex",
			&attachRejection{kind: transportH2, code: http.StatusHTTPVersionNotSupported},
			attachRetireRung,
		},
		{
			"the target is unreachable",
			&attachRejection{kind: transportH2, code: http.StatusConflict,
				message: "the server is not reachable over SSH right now"},
			attachFinal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAttachFailure(tc.err); got != tc.want {
				t.Fatalf("classifyAttachFailure = %d, want %d", got, tc.want)
			}
		})
	}
}

// A spent token must be said in terms of what happened, not in the server's own
// words: "invalid, expired or already used" sends the developer looking for an
// expiry that never occurred.
func TestSpentTokenExplainsWhoBurntIt(t *testing.T) {
	refused := &attachRejection{kind: transportH2, status: "401 Unauthorized",
		code: http.StatusUnauthorized, message: "invalid, expired or already used tunnel token"}
	got := spentToken(refused).Error()
	if !strings.Contains(got, "already spent") || !strings.Contains(got, "timed out") {
		t.Fatalf("the message must say the token was spent by an earlier attempt, got: %s", got)
	}
	if strings.Contains(got, "invalid, expired") {
		t.Fatalf("the server's sentence blames the developer and must not be the whole message, got: %s", got)
	}
	if !errors.Is(spentToken(refused), refused) {
		t.Fatal("the underlying rejection must stay reachable")
	}
	// Anything else keeps what was actually observed — that IS the diagnosis.
	timeout := errors.New("HTTP/3 (QUIC) attach: the peer sent no response headers within 45s")
	if !strings.Contains(spentToken(timeout).Error(), "no response headers") {
		t.Fatalf("a timed-out open must surface as itself, got: %s", spentToken(timeout))
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
		got := forwardCloseMessage(reason)
		if got == "" {
			t.Errorf("%q produced no message at all", reason)
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("forwardCloseMessage(%q) = %q, want it to mention %q", reason, got, want)
		}
	}
	if got := forwardCloseMessage("target_stopped"); !strings.Contains(got, "run the command again") {
		t.Errorf("a stopped target must say what to do next, got %q", got)
	}
	if got := forwardCloseMessage("user_close"); got != "" {
		t.Errorf("a deliberate close should stay silent, got %q", got)
	}
	if got := forwardCloseMessage("something_new"); got == "" {
		t.Error("an unrecognised reason must still be reported")
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
			var rejection *attachRejection
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
