// The guards on the shared attach transport: what the capability probe refuses,
// and what a stream open refuses before it has a stream.
//
// The probe is the only thing standing between the ladder and a rung that looks
// negotiated and is not. Every check it drops turns into a session that attaches
// and then behaves inexplicably, which is the failure mode ADR-064's ladder
// exists to avoid rather than produce.
//
// Every top-level identifier is prefixed hguard (concurrent-agent rule).
package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	tun "github.com/deepteams/akerdock/internal/tunnel"
)

// hguardProbe runs the probe against a handler served over one rung.
func hguardProbe(t *testing.T, handler http.HandlerFunc, served, probed transportKind) error {
	t.Helper()
	attach, pool := attachToFakeControlPlane(t, handler, served)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return probeAttach(ctx, pool, attach, probed, tun.EgressHTTP)
}

// hguardCapabilities answers the probe the way a working control plane does.
func hguardCapabilities(capabilities string, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if capabilities != "" {
			w.Header().Set(tun.EgressHTTP.CapabilitiesHeader, capabilities)
		}
		w.WriteHeader(status)
	}
}

// Everything the probe must refuse. Each of these answered something — the
// point is that answering is not the same as carrying this wire on this rung.
func TestHguardProbeRefusesAnythingButAWorkingRung(t *testing.T) {
	t.Run("a peer that answered, but not the probe's answer", func(t *testing.T) {
		err := hguardProbe(t, hguardCapabilities(tun.EgressHTTP.Name+",h2", http.StatusOK), transportH2, transportH2)
		if err == nil || !strings.Contains(err.Error(), "capability probe returned") {
			t.Fatalf("err = %v — only 204 is the probe's answer", err)
		}
	})

	t.Run("a peer that carries the rung but not this access path", func(t *testing.T) {
		err := hguardProbe(t, hguardCapabilities("akerdock-something-else,h2", http.StatusNoContent), transportH2, transportH2)
		if err == nil || !strings.Contains(err.Error(), tun.EgressHTTP.Name) {
			t.Fatalf("err = %v — a token minted for one path is not redeemable on another (ADR-027)", err)
		}
	})

	t.Run("a peer that carries this access path but not this rung", func(t *testing.T) {
		err := hguardProbe(t, hguardCapabilities(tun.EgressHTTP.Name+",websocket", http.StatusNoContent), transportH2, transportH2)
		if err == nil || !strings.Contains(err.Error(), "does not advertise") {
			t.Fatalf("err = %v — the ladder must step down rather than attach anyway", err)
		}
	})

	t.Run("a front that silently downgraded the rung", func(t *testing.T) {
		// The peer advertises HTTP/3 and answers over HTTP/2. Taking that for a
		// working HTTP/3 path is how a session ends up labelled with a transport
		// it never ran on — and every error it produces then names the wrong
		// protocol to go and look at.
		err := hguardProbe(t, hguardCapabilities(tun.EgressHTTP.Name+",h3,h2", http.StatusNoContent), transportH2, transportH3)
		if err == nil || !strings.Contains(err.Error(), "negotiated") {
			t.Fatalf("err = %v — what was negotiated is not what was asked for", err)
		}
	})
}

// A URL no request can be built from must fail as itself, at the caller, rather
// than as a transport that would not answer.
func TestHguardUnbuildableRequestFailsAtOnce(t *testing.T) {
	pool := newH2Pool()
	defer func() { _ = pool.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	broken := &url.URL{Scheme: "https", Host: "not a host"}

	if err := probeAttach(ctx, pool, broken, transportH2, tun.EgressHTTP); err == nil {
		t.Fatal("the probe must refuse a URL it cannot request")
	}
	_, err := openAttachStream(ctx, pool, -1, broken.String(), http.Header{}, transportH2, time.Second)
	if err == nil {
		t.Fatal("the stream open must refuse a URL it cannot request")
	}
	var rejection *rejectedAttachError
	if errors.As(err, &rejection) {
		t.Fatalf("err = %v — nothing was ever asked, so nothing refused it", err)
	}
}

// A peer that is not there fails on the dial, and that failure must reach the
// caller as itself: the open timeout above it exists for a peer that answers
// slowly, and reporting one as the other sends the reader looking at latency.
func TestHguardUnreachablePeerFailsOnTheDialNotTheTimer(t *testing.T) {
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	pool := newH2Pool()
	defer func() { _ = pool.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	started := time.Now()
	_, err = openAttachStream(ctx, pool, 0,
		"https://127.0.0.1:"+strconv.Itoa(port)+"/tunnel/attach", http.Header{}, transportH2, 8*time.Second)
	if err == nil {
		t.Fatal("a peer that is not listening must not yield a stream")
	}
	if strings.Contains(err.Error(), "no response headers within") {
		t.Fatalf("err = %v — a refused dial is not a slow peer", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the refusal took %s — it must not wait out the open budget", elapsed)
	}
}

// A caller that hands the pool a TLS config without a floor must not silently
// get one: these connections carry a tunnel into someone's production database.
func TestHguardPoolNeverDropsBelowTLS12(t *testing.T) {
	caller := &tls.Config{ServerName: "panel.example.com"} //nolint:gosec // the missing floor is the fixture
	pool := newH2PoolWithTLS(caller)
	defer func() { _ = pool.Close() }()
	if got := len(pool.lanes); got != transportHTTP2Lanes {
		t.Fatalf("the pool has %d lanes, want %d", got, transportHTTP2Lanes)
	}
	if got := hguardMinVersion(t, pool); got != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x, want TLS 1.2 — an unset floor is not permission to drop", got)
	}
	// The caller's own config is cloned, not amended: it may well be the one
	// the rest of the process dials with.
	if caller.MinVersion != 0 {
		t.Fatal("the pool wrote its floor into the caller's config")
	}
	// A caller that set a stricter floor keeps it.
	strict := newH2PoolWithTLS(&tls.Config{MinVersion: tls.VersionTLS13})
	defer func() { _ = strict.Close() }()
	if got := hguardMinVersion(t, strict); got != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %#x, want the caller's TLS 1.3", got)
	}
	// And a pool built with no config at all still has one.
	bare := newH2Pool()
	defer func() { _ = bare.Close() }()
	if got := hguardMinVersion(t, bare); got != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x on a default pool", got)
	}
}

// Closing the pool reports the first lane that could not be closed rather than
// the last: a pool half torn down is not a pool that closed.
func TestHguardPoolCloseSurfacesALaneThatWouldNotClose(t *testing.T) {
	first := errors.New("lane 1 would not close")
	pool := &httpLanePool{lanes: []*httpLane{
		{close: func() error { return nil }},
		{close: func() error { return first }},
		{close: func() error { return errors.New("lane 2 would not close either") }},
	}}
	if err := pool.Close(); !errors.Is(err, first) {
		t.Fatalf("Close() = %v, want the first failure", err)
	}
}

func hguardMinVersion(t *testing.T, pool *httpLanePool) uint16 {
	t.Helper()
	transport, ok := pool.lanes[0].roundTripper.(*http2.Transport)
	if !ok {
		t.Fatalf("lane 0 round-tripper is %T, not an HTTP/2 transport", pool.lanes[0].roundTripper)
	}
	return transport.TLSClientConfig.MinVersion
}
