// The laptop's half of an HTTP-attached ingress session: the control wire the
// agent asks for connections on, and what the CLI answers when it cannot give
// it one.
//
// Every open the agent sends is a visitor already waiting on a TCP connection.
// An answer it never gets is a visitor held until the agent's own open budget
// runs out, so the failure paths here are the ones that turn a developer's
// stopped dev server into a prompt 502 rather than a fifteen-second hang.
//
// Every top-level identifier is prefixed ibridge (concurrent-agent rule).
package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	tun "github.com/deepteams/akerdock/internal/tunnel"
)

// ibridgeWire is the agent's end of the control wire, wired to the CLI's end
// through a pair of pipes. The control request is an ordinary full-duplex HTTP
// stream in production; nothing in the bridge depends on that, so the pipes are
// the whole of what it needs to be driven.
type ibridgeWire struct {
	agent *tun.LineControl
	cli   *tun.LineControl
}

func newIbridgeWire() *ibridgeWire {
	toCLI, fromAgent := io.Pipe()
	toAgent, fromCLI := io.Pipe()
	return &ibridgeWire{
		agent: tun.NewLineControl(toAgent, fromAgent, nil, fromAgent.Close),
		cli:   tun.NewLineControl(toCLI, fromCLI, nil, fromCLI.Close),
	}
}

// ibridgePlane serves the HTTP data streams the bridge opens per visitor
// connection, refusing them all with one status when refuse is set.
type ibridgePlane struct {
	t      *testing.T
	refuse int
	body   string
}

func (p *ibridgePlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set(tun.IngressHTTP.CapabilitiesHeader, tun.IngressHTTP.Name+",h3,h2,websocket")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if got := r.Header.Get(tun.IngressHTTP.StreamHeader); got == "" {
		p.t.Error("a data stream must name the open it answers, or the agent cannot pair it")
	}
	if p.refuse != 0 {
		w.WriteHeader(p.refuse)
		_, _ = io.WriteString(w, p.body)
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.EnableFullDuplex()
	w.Header().Set("Content-Type", tun.IngressHTTP.StreamContentType)
	w.WriteHeader(http.StatusOK)
	_ = controller.Flush()
	stream := tun.NewDuplexConn(r.Body, httpWriter{w}, controller.Flush, nil)
	_, _ = io.Copy(io.Discard, stream)
}

// ibridgeRun starts the bridge and returns its verdict once it stops.
func ibridgeRun(
	ctx context.Context, t *testing.T, wire *ibridgeWire, plane *ibridgePlane, localPort int,
) <-chan struct {
	reason string
	err    error
} {
	t.Helper()
	attach, pool := attachToFakeControlPlane(t, plane, transportH2)
	done := make(chan struct {
		reason string
		err    error
	}, 1)
	go func() {
		reason, err := runIngressHTTPBridge(ctx, wire.cli, pool, attach, "session-1", "key-1", localPort, transportH2)
		done <- struct {
			reason string
			err    error
		}{reason, err}
	}()
	return done
}

// ibridgeLocalPort starts a dev server on the loopback and returns its port.
func ibridgeLocalPort(t *testing.T) int {
	t.Helper()
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(dev.Close)
	parsed, err := url.Parse(dev.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// ibridgeAwait reads one frame from the agent's end, bounded. LineControl.Receive
// blocks with no deadline of its own, and an answer that never comes is exactly
// what these tests are about: it has to be reported as a missing answer, not as
// a test binary that stopped.
func ibridgeAwait(t *testing.T, wire *ibridgeWire) tun.HTTPControlFrame {
	t.Helper()
	type received struct {
		frame tun.HTTPControlFrame
		err   error
	}
	got := make(chan received, 1)
	go func() {
		frame, err := wire.agent.Receive()
		got <- received{frame, err}
	}()
	select {
	case result := <-got:
		if result.err != nil {
			t.Fatalf("reading the laptop's answer: %v", result.err)
		}
		return result.frame
	case <-time.After(10 * time.Second):
		t.Fatal("the laptop never answered — the visitor waits out the agent's open budget for nothing")
		return tun.HTTPControlFrame{}
	}
}

// A dev server the developer has not started yet is the single most common
// state of an ingress tunnel, and the agent has to hear about it: the visitor
// is already connected, and an open nobody answers is a page that hangs until
// the agent gives up on it.
func TestIbridgeUnservableOpenIsAnsweredNotDropped(t *testing.T) {
	closedPort, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	wire := newIbridgeWire()
	plane := &ibridgePlane{t: t}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := ibridgeRun(ctx, t, wire, plane, closedPort)

	if err := wire.agent.Send(ctx, tun.HTTPControlFrame{Type: "open", ID: 7}); err != nil {
		t.Fatal(err)
	}
	frame := ibridgeAwait(t, wire)
	if frame.Type != "open_err" || frame.ID != 7 {
		t.Fatalf("answer = %+v, want an open_err for stream 7", frame)
	}
	if frame.Code != "dial_failed" {
		t.Fatalf("code = %q — the agent decides its 502 on this", frame.Code)
	}
	if frame.Msg == "" {
		t.Fatal("the dial's own error is what tells the developer their app is not up")
	}

	_ = wire.agent.Send(ctx, tun.HTTPControlFrame{Type: "session_close", Reason: "revoked"})
	select {
	case result := <-done:
		if result.err != nil || result.reason != "revoked" {
			t.Fatalf("bridge = %q, %v", result.reason, result.err)
		}
	case <-ctx.Done():
		t.Fatal("the bridge outlived its session")
	}
}

// The other half of the same promise: the dev server answered, but the stream
// that was to carry it could not be opened. The agent still has a visitor
// waiting, and the local connection must not be left open behind a stream that
// will never exist.
func TestIbridgeRefusedStreamIsReportedAndTheLocalDialReleased(t *testing.T) {
	wire := newIbridgeWire()
	plane := &ibridgePlane{t: t, refuse: http.StatusConflict, body: "the session has ended"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := ibridgeRun(ctx, t, wire, plane, ibridgeLocalPort(t))

	if err := wire.agent.Send(ctx, tun.HTTPControlFrame{Type: "open", ID: 4}); err != nil {
		t.Fatal(err)
	}
	frame := ibridgeAwait(t, wire)
	if frame.Type != "open_err" || frame.ID != 4 || frame.Code != "stream_failed" {
		t.Fatalf("answer = %+v, want stream_failed for stream 4", frame)
	}
	// The peer's own words travel with it: "the session has ended" and "no
	// capacity" are different problems with different next moves.
	if !strings.Contains(frame.Msg, "the session has ended") {
		t.Fatalf("msg = %q — the peer's diagnosis is the actionable half", frame.Msg)
	}

	_ = wire.agent.Send(ctx, tun.HTTPControlFrame{Type: "session_close", Reason: "idle_timeout"})
	select {
	case result := <-done:
		if result.err != nil || result.reason != "idle_timeout" {
			t.Fatalf("bridge = %q, %v", result.reason, result.err)
		}
	case <-ctx.Done():
		t.Fatal("the bridge outlived its session")
	}
}

// Liveness: the agent's ping is what tells it the laptop is still there while
// no visitor is connected, which is most of a tunnel's life. A bridge that
// stopped answering would be reaped on the agent's own timer.
func TestIbridgeAnswersLiveness(t *testing.T) {
	wire := newIbridgeWire()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := ibridgeRun(ctx, t, wire, &ibridgePlane{t: t}, ibridgeLocalPort(t))

	if err := wire.agent.Send(ctx, tun.HTTPControlFrame{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	if frame := ibridgeAwait(t, wire); frame.Type != "pong" {
		t.Fatalf("liveness answer = %+v", frame)
	}
	// And a frame this build has never heard of is dropped rather than fatal:
	// an agent that gains a control type must not break every laptop older
	// than it.
	_ = wire.agent.Send(ctx, tun.HTTPControlFrame{Type: "something_from_the_future"})
	_ = wire.agent.Send(ctx, tun.HTTPControlFrame{Type: "ping"})
	if frame := ibridgeAwait(t, wire); frame.Type != "pong" {
		t.Fatalf("an unknown frame broke the wire: %+v", frame)
	}

	_ = wire.agent.Send(ctx, tun.HTTPControlFrame{Type: "session_close", Reason: "user_close"})
	<-done
}

// Ctrl-C and a wire that died are the same read failure and must not be the
// same verdict: one is the developer stopping the tunnel, the other is a drop
// the relay is expected to reconnect through.
func TestIbridgeTellsAStoppedTunnelFromADroppedOne(t *testing.T) {
	t.Run("the developer stopped it", func(t *testing.T) {
		wire := newIbridgeWire()
		ctx, cancel := context.WithCancel(context.Background())
		done := ibridgeRun(ctx, t, wire, &ibridgePlane{t: t}, ibridgeLocalPort(t))
		cancel()
		_ = wire.agent.Close()
		result := <-done
		if result.err != nil || result.reason != "user_close" {
			t.Fatalf("bridge = %q, %v — a cancelled read is the cancel, not a fault", result.reason, result.err)
		}
	})

	t.Run("the wire died under it", func(t *testing.T) {
		wire := newIbridgeWire()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		done := ibridgeRun(ctx, t, wire, &ibridgePlane{t: t}, ibridgeLocalPort(t))
		_ = wire.agent.Close()
		result := <-done
		if result.err == nil {
			t.Fatal("a wire that died is an error to reconnect on, not a clean end")
		}
		if result.reason != "" {
			t.Fatalf("reason = %q — a drop has no reason, which is how the relay knows to re-dial", result.reason)
		}
	})
}

// The control request is where a session is claimed, and the two ways it can
// fail before there is a session are a URL that cannot be built and a peer that
// answers something else entirely.
func TestIbridgeControlAttachRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("a mint whose attach URL cannot be built", func(t *testing.T) {
		if _, err := ingressHTTPControlURL(ingressMint{AttachUrl: "://nope", Token: "tk"}); err == nil {
			t.Fatal("an unparsable attach URL must be refused")
		}
		attach, pool := attachToFakeControlPlane(t, &ibridgePlane{t: t}, transportH2)
		_ = attach
		if _, err := openIngressHTTPControl(ctx, pool, ingressMint{AttachUrl: "://nope"}, "key", transportH2); err == nil {
			t.Fatal("the control attach must fail on the URL, not dial an empty one")
		}
	})

	t.Run("a peer that answers 200 without echoing the wire", func(t *testing.T) {
		const decoy = "<html>captive portal</html>"
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, decoy)
		}))
		srv.EnableHTTP2 = true
		srv.StartTLS()
		defer srv.Close()
		pool := newH2PoolWithTLS(clientTLSFor(t, srv))
		defer func() { _ = pool.Close() }()

		sess := ingressMint{AttachUrl: strings.Replace(srv.URL, "https", "wss", 1) + "/attach", Token: "tk"}
		_, err := openIngressHTTPControl(ctx, pool, sess, "key", transportH2)
		var rejection *rejectedAttachError
		if !errors.As(err, &rejection) {
			t.Fatalf("err = %v, want an attach rejection", err)
		}
		// Whatever answered is the whole diagnosis: a 200 from a captive portal
		// and a 200 from a stale AkerDock are told apart by nothing else.
		if !strings.Contains(rejection.message, decoy) {
			t.Fatalf("message = %q — what the peer sent is what identifies it", rejection.message)
		}
	})
}
