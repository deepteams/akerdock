// The egress access path driven end to end over a real HTTP transport: the
// session request that holds the tunnel open, the data stream one accepted
// local connection gets, and the reasons a session stops.
//
// The rungs above the WebSocket are only exercised here, because they are the
// only ones where the CLI has to identify itself twice — once with the mint
// token on the session, and then on every stream with the session and the
// attach key that claimed it. The other files cover the bottom rung.
//
// Every top-level identifier is prefixed eflow (concurrent-agent rule).
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tun "github.com/deepteams/akerdock/internal/tunnel"
)

// eflowSessionUUID is what the plane binds this session's data streams to. A
// stream that names anything else is a stream from another attacher.
const eflowSessionUUID = "session-egress-1"

// eflowPlane is the control plane's half of one HTTP-attached port-forward. It
// checks what the real control plane refuses on — the mint token on the session
// request, the session UUID and attach key on every data stream — and answers
// each stream itself, standing in for the target it would have dialled through
// SSH.
type eflowPlane struct {
	t     *testing.T
	token string

	// cut ends the session with the reason the developer will read. Buffered,
	// so a test can arm the close before the handler reaches its select.
	cut chan sessionEnd
	// drop ends the session the way a restarted control plane or a front that
	// cut a long-lived request does: the wire stops, with no close frame on it.
	drop chan struct{}
	// pong receives once the CLI has answered the liveness check that proves
	// the laptop is still on the other end.
	pong chan struct{}

	mu  sync.Mutex
	key string // the attach key the session request presented

	streams atomic.Int32
	// sessions counts attach attempts, which is how a test tells "the ladder
	// stepped down" from "the command gave up on the server's verdict".
	sessions atomic.Int32

	// The refusals a test can arm, in the order the CLI meets them.
	refuseSession int    // status the session attach is answered with (0 accepts)
	refusal       string // the sentence sent beside it
	silent        bool   // answer 200 without echoing the wire's name
	refuseStreams int    // status every data stream is answered with (0 accepts)
	streamRefusal string
}

func newEflowPlane(t *testing.T) *eflowPlane {
	t.Helper()
	return &eflowPlane{
		t:     t,
		token: "mint-token",
		cut:   make(chan sessionEnd, 1),
		drop:  make(chan struct{}, 1),
		pong:  make(chan struct{}, 1),
	}
}

func (p *eflowPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", "OPTIONS, POST, GET")
		w.Header().Set(tun.EgressHTTP.CapabilitiesHeader, tun.EgressHTTP.Name+",h3,h2,websocket")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Header.Get("Content-Type") {
	case tun.EgressHTTP.ControlContentType:
		p.session(w, r)
	case tun.EgressHTTP.StreamContentType:
		p.stream(w, r)
	default:
		// ADR-027's rule enforced rather than assumed: another access path's
		// content type is not this endpoint's business.
		w.WriteHeader(http.StatusUnsupportedMediaType)
	}
}

func (p *eflowPlane) session(w http.ResponseWriter, r *http.Request) {
	p.sessions.Add(1)
	if got := r.URL.Query().Get("token"); got != p.token {
		p.t.Errorf("session attach presented token %q, want the minted %q", got, p.token)
	}
	if got := r.Header.Get(tun.EgressHTTP.ProtocolHeader); got != tun.EgressHTTP.Name {
		p.t.Errorf("session attach protocol header = %q, want %q", got, tun.EgressHTTP.Name)
	}
	if got := r.Header.Get(tun.EgressHTTP.TransportHeader); got == "" {
		p.t.Error("the session attach must name the rung it is climbing, so a refusal can name it back")
	}
	key := r.Header.Get(tun.EgressHTTP.AttachKeyHeader)
	if key == "" {
		p.t.Error("the session attach must identify its attacher (ADR-065 §3)")
	}
	p.mu.Lock()
	p.key = key
	p.mu.Unlock()

	if p.refuseSession != 0 {
		w.WriteHeader(p.refuseSession)
		_, _ = io.WriteString(w, p.refusal)
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.EnableFullDuplex()
	w.Header().Set("Content-Type", tun.EgressHTTP.ControlContentType)
	if !p.silent {
		w.Header().Set(tun.EgressHTTP.ProtocolHeader, tun.EgressHTTP.Name)
	}
	w.Header().Set(tun.EgressHTTP.SessionHeader, eflowSessionUUID)
	w.WriteHeader(http.StatusOK)
	if controller.Flush() != nil || p.silent {
		return
	}

	control := tun.NewLineControl(r.Body, httpWriter{w}, controller.Flush, nil)
	// Liveness first: while no local client is connected — which is most of a
	// port-forward's life — this exchange is the only thing that tells the
	// control plane the laptop is still there.
	if err := control.Send(r.Context(), tun.HTTPControlFrame{Type: "ping"}); err != nil {
		return
	}
	go func() {
		for {
			frame, err := control.Receive()
			if err != nil {
				return
			}
			if frame.Type == "pong" {
				select {
				case p.pong <- struct{}{}:
				default:
				}
			}
		}
	}()
	select {
	case end := <-p.cut:
		_ = control.Send(r.Context(), tun.HTTPControlFrame{
			Type: "session_close", Reason: end.reason, Msg: end.message,
		})
		// Let the frame land: returning closes both halves under the CLI, and
		// a close it never read is the silent death this whole path avoids.
		time.Sleep(50 * time.Millisecond)
	case <-p.drop:
	case <-r.Context().Done():
	}
}

func (p *eflowPlane) stream(w http.ResponseWriter, r *http.Request) {
	p.streams.Add(1)
	p.mu.Lock()
	key := p.key
	p.mu.Unlock()
	if got := r.Header.Get(tun.EgressHTTP.SessionHeader); got != eflowSessionUUID {
		p.t.Errorf("data stream named session %q, want the one the head assigned (%q)", got, eflowSessionUUID)
	}
	if got := r.Header.Get(tun.EgressHTTP.AttachKeyHeader); got != key {
		p.t.Errorf("data stream key = %q, want the session's %q", got, key)
	}
	// The mint token is single-use and was spent on the session request. A
	// stream that carried it again would be a secret replayed once per
	// forwarded connection, in every intermediary's access log.
	if got := r.URL.Query().Get("token"); got != "" {
		p.t.Errorf("a data stream re-presented the mint token: %q", got)
	}

	if p.refuseStreams != 0 {
		w.WriteHeader(p.refuseStreams)
		_, _ = io.WriteString(w, p.streamRefusal)
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.EnableFullDuplex()
	w.Header().Set("Content-Type", tun.EgressHTTP.StreamContentType)
	w.WriteHeader(http.StatusOK)
	if controller.Flush() != nil {
		return
	}
	// Standing in for the target the control plane would have reached over SSH:
	// every line is answered, so a byte read back by the local client proves the
	// whole path — listener, stream, and back — rather than one direction.
	stream := tun.NewDuplexConn(r.Body, httpWriter{w}, controller.Flush, nil)
	lines := bufio.NewScanner(stream)
	for lines.Scan() {
		if _, err := fmt.Fprintf(stream, "pong:%s\n", lines.Text()); err != nil {
			return
		}
	}
}

// eflowSession runs the plane over one rung and opens the session on it.
func eflowSession(t *testing.T, plane *eflowPlane, kind transportKind) (*egressSession, context.Context) {
	t.Helper()
	attach, pool := attachToFakeControlPlane(t, plane, kind)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	if err := probeEgress(ctx, pool, attach, kind); err != nil {
		t.Fatalf("%s probe: %v", kind, err)
	}
	key, err := tun.NewIngressAttachKey()
	if err != nil {
		t.Fatal(err)
	}
	session, err := openEgressSession(ctx, pool, attach, mintedTunnel{token: plane.token}, key, kind)
	if err != nil {
		t.Fatalf("%s session: %v", kind, err)
	}
	t.Cleanup(session.close)
	return session, ctx
}

// The journey a `port-forward` is: a session held open, one local connection
// relayed over its own stream, liveness answered while it runs, and a close the
// developer can act on.
func TestEflowPortForwardRelaysOverTheHTTPRungs(t *testing.T) {
	for _, kind := range []transportKind{transportH2, transportH3} {
		t.Run(string(kind), func(t *testing.T) {
			plane := newEflowPlane(t)
			session, ctx := eflowSession(t, plane, kind)
			if session.uuid != eflowSessionUUID {
				t.Fatalf("session UUID = %q — the streams bind to whatever the head assigned", session.uuid)
			}
			if session.kind != kind {
				t.Fatalf("session labelled %s on a %s rung", session.kind, kind)
			}
			localPort, err := freePort()
			if err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(time.Hour)

			var served error
			_, stderr := captureOutput(t, func() {
				done := make(chan error, 1)
				go func() { done <- serveForwardSession(ctx, session, &until, localPort, 5432) }()

				local := dialRetry(t, localPort)
				if _, err := fmt.Fprintln(local, "SELECT 1"); err != nil {
					t.Errorf("write to the forwarded port: %v", err)
				}
				answer, err := bufio.NewReader(local).ReadString('\n')
				if err != nil || strings.TrimSpace(answer) != "pong:SELECT 1" {
					t.Errorf("read back %q, %v — the bytes must cross both ways", answer, err)
				}
				_ = local.Close()

				select {
				case <-plane.pong:
				case <-time.After(10 * time.Second):
					t.Error("the session never answered the liveness check — the plane would reap this tunnel")
				}
				plane.cut <- sessionEnd{reason: "grant_expired"}
				served = <-done
			})
			if served != nil {
				t.Fatalf("a session the server closed is not a command failure: %v", served)
			}
			if got := plane.streams.Load(); got != 1 {
				t.Fatalf("the plane saw %d data streams for one local connection", got)
			}
			if !strings.Contains(stderr, "the access grant expired") {
				t.Fatalf("stderr = %q — the close reason is what the developer acts on", stderr)
			}
			// And the listener the announcement named is gone with it: a socket
			// still accepting on a tunnel that ended answers connections nothing
			// will ever serve.
			if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), time.Second); err == nil {
				_ = conn.Close()
				t.Fatal("the local listener outlived the session that justified it")
			}
		})
	}
}

// ADR-066 moved the target's dial behind the response head, so the listener is
// already open when a connection turns out to be unservable. That is only
// acceptable because the failure reaches the developer on the connection they
// made: the tunnel says why, and the local socket closes rather than hanging.
func TestEflowUnservableConnectionIsRefusedInWords(t *testing.T) {
	plane := newEflowPlane(t)
	plane.refuseStreams = http.StatusBadGateway
	plane.streamRefusal = "the target did not accept the connection: dial 10.1.2.3:5432: connect: connection refused"
	session, ctx := eflowSession(t, plane, transportH2)
	localPort, err := freePort()
	if err != nil {
		t.Fatal(err)
	}

	_, stderr := captureOutput(t, func() {
		done := make(chan error, 1)
		go func() { done <- serveForwardSession(ctx, session, nil, localPort, 5432) }()
		local := dialRetry(t, localPort)
		// The read is the assertion: a refused stream must end the local
		// connection, not leave psql waiting on a socket nobody serves.
		if _, err := io.ReadAll(local); err != nil {
			t.Errorf("the local connection did not end cleanly: %v", err)
		}
		_ = local.Close()
		plane.cut <- sessionEnd{reason: "user_close"}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	if !strings.Contains(stderr, "connection refused by the tunnel") {
		t.Fatalf("stderr = %q — a connection that went nowhere must say so", stderr)
	}
	if !strings.Contains(stderr, "connect: connection refused") {
		t.Fatalf("stderr = %q — the plane's own diagnosis is the actionable half", stderr)
	}
}

// Ctrl-C is not a failure and needs no explanation: the read that fails because
// we tore the session down must not be reported as the session dying.
func TestEflowUserCloseEndsQuietly(t *testing.T) {
	plane := newEflowPlane(t)
	session, ctx := eflowSession(t, plane, transportH2)
	stopped, stop := context.WithCancel(ctx)
	localPort, err := freePort()
	if err != nil {
		t.Fatal(err)
	}

	var served error
	_, stderr := captureOutput(t, func() {
		done := make(chan error, 1)
		go func() { done <- serveForwardSession(stopped, session, nil, localPort, 5432) }()
		_ = dialRetry(t, localPort).Close()
		stop()
		served = <-done
	})
	if served != nil {
		t.Fatalf("a session the developer stopped is not an error: %v", served)
	}
	if strings.Contains(stderr, "tunnel closed") {
		t.Fatalf("stderr = %q — the developer knows why they pressed Ctrl-C", stderr)
	}
}

// The end of the session is what ends the command, whichever way it arrives.
// This is the case that has no close frame to read — the wire just stops, the
// way it does when the control plane restarts or a front cuts a long-lived
// request — and it is the shape the accept loop is easiest to get wrong on: a
// forward that keeps its local port open past its session accepts connections
// nothing can serve, and looks alive while doing it.
func TestEflowSessionEndReleasesTheLocalPort(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(plane *eflowPlane)
	}{
		{"the server closes the session", func(plane *eflowPlane) { plane.cut <- sessionEnd{reason: "idle_timeout"} }},
		{"the wire dies with nothing on it", func(plane *eflowPlane) { plane.drop <- struct{}{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plane := newEflowPlane(t)
			session, ctx := eflowSession(t, plane, transportH2)
			localPort, err := freePort()
			if err != nil {
				t.Fatal(err)
			}

			var served error
			_, _ = captureOutput(t, func() {
				done := make(chan error, 1)
				go func() { done <- serveForwardSession(ctx, session, nil, localPort, 5432) }()
				_ = dialRetry(t, localPort).Close()
				tc.end(plane)
				select {
				case served = <-done:
				case <-time.After(10 * time.Second):
					t.Error("the session ended and the forward kept running")
				}
			})
			if served != nil {
				t.Fatalf("err = %v", served)
			}
			if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), time.Second); err == nil {
				_ = conn.Close()
				t.Fatal("the local listener outlived the session that authorized it")
			}
		})
	}
}

// A local port someone else already holds is the one failure that happens
// before anything is minted, and it has to name the port.
func TestEflowListenerFailureNamesThePort(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	port := busy.Addr().(*net.TCPAddr).Port

	plane := newEflowPlane(t)
	session, ctx := eflowSession(t, plane, transportH2)
	var served error
	_, _ = captureOutput(t, func() {
		served = serveForwardSession(ctx, session, nil, port, 5432)
	})
	if served == nil || !strings.Contains(served.Error(), fmt.Sprintf("cannot listen on 127.0.0.1:%d", port)) {
		t.Fatalf("err = %v", served)
	}
}

// What openEgressSession refuses before there is a session to serve. Each of
// these is a different party's fault, and the CLI has to say which.
func TestEflowSessionAttachRefusals(t *testing.T) {
	t.Run("a mint with no token spends no request", func(t *testing.T) {
		plane := newEflowPlane(t)
		attach, pool := attachToFakeControlPlane(t, plane, transportH2)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := openEgressSession(ctx, pool, attach, mintedTunnel{}, "key", transportH2); err == nil {
			t.Fatal("a mint with no token must be refused before any request")
		}
		if got := plane.sessions.Load(); got != 0 {
			t.Fatalf("the plane saw %d attach attempts for a token we never had", got)
		}
	})

	t.Run("a peer that answers 200 without echoing the wire", func(t *testing.T) {
		plane := newEflowPlane(t)
		plane.silent = true
		attach, pool := attachToFakeControlPlane(t, plane, transportH2)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := openEgressSession(ctx, pool, attach, mintedTunnel{token: plane.token}, "key", transportH2)
		var rejection *rejectedAttachError
		if err == nil || !errors.As(err, &rejection) {
			t.Fatalf("err = %v, want an attach rejection", err)
		}
		if !strings.Contains(rejection.message, "did not echo "+tun.EgressHTTP.Name) {
			t.Fatalf("message = %q — a 200 from something that is not this wire is not a session", rejection.message)
		}
		// It is not a transport verdict: some other endpoint answered, and the
		// rung below would reach the same one.
		if rejection.transportRefused() {
			t.Fatal("a peer that answered 200 did not refuse the protocol")
		}
	})

	t.Run("a refusal carries the server's sentence", func(t *testing.T) {
		plane := newEflowPlane(t)
		plane.refuseSession = http.StatusUnauthorized
		plane.refusal = "invalid, expired or already used tunnel token"
		attach, pool := attachToFakeControlPlane(t, plane, transportH2)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := openEgressSession(ctx, pool, attach, mintedTunnel{token: plane.token}, "key", transportH2)
		var rejection *rejectedAttachError
		if err == nil || !errors.As(err, &rejection) {
			t.Fatalf("err = %v, want an attach rejection", err)
		}
		if rejection.code != http.StatusUnauthorized || !strings.Contains(rejection.message, "already used") {
			t.Fatalf("rejection = %+v", rejection)
		}
	})
}

// A cold-started session keeps the wider open budget the mint announced, and
// gives it back the moment the server says the target is up — on the SAME
// session object the streams read it from.
func TestEflowWakingMintWidensTheStreamBudget(t *testing.T) {
	plane := newEflowPlane(t)
	attach, pool := attachToFakeControlPlane(t, plane, transportH2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := openEgressSession(ctx, pool, attach,
		mintedTunnel{token: plane.token, waking: true}, "key", transportH2)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	if got := session.openTimeout(); got != egressWakeOpenTimeout {
		t.Fatalf("budget = %s — a session minted over a wake must carry it from the first stream", got)
	}
	// And the mint already told the developer, so the frame that repeats it
	// must not print a second time.
	_, stderr := captureOutput(t, func() { session.noteWaking(wakeFrameColdStart, wakeColdStartNotice) })
	if stderr != "" {
		t.Fatalf("the cold start was announced twice: %q", stderr)
	}
}

// The mint is the one thing the ladder cannot work around, so a mint it cannot
// use must fail once — not once per rung, and not as "no HTTP transport", which
// would send the same unusable mint down to the WebSocket.
func TestEflowUnusableMintFailsBeforeTheClimb(t *testing.T) {
	client := &Client{base: "https://panel.example.com"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.forwardOverHTTP(ctx, mintedTunnel{attachPath: "", token: "tk"}, "key", 0, 5432)
	if !errors.Is(err, errNoHTTPTransport) {
		t.Fatalf("err = %v — a mint with no attach path has nothing to probe, which is the WebSocket's case", err)
	}
	err = client.forwardOverHTTP(ctx, mintedTunnel{attachPath: "/tunnel/attach", token: ""}, "key", 0, 5432)
	if err == nil || errors.Is(err, errNoHTTPTransport) {
		t.Fatalf("err = %v — a mint with no token is unusable on every rung, so it fails here", err)
	}
	if !strings.Contains(err.Error(), "no attach token") {
		t.Fatalf("err = %v, want it to name what is missing", err)
	}
}

// egressAttachURL is where a mint response stops being trusted prose and
// becomes a URL: both refusals are a server that answered something this
// version cannot attach to.
func TestEflowAttachURLRefusesWhatItCannotUse(t *testing.T) {
	if _, err := egressAttachURL("https://panel.example.com", ""); err == nil {
		t.Fatal("a mint response with no attach path must be refused")
	}
	if _, err := egressAttachURL("gopher://panel.example.com", "/tunnel/attach"); err == nil {
		t.Fatal("an attach URL this client cannot speak must be refused")
	}
	attach, err := egressAttachURL("https://panel.example.com", "/tunnel/attach?token=stale&x=1")
	if err != nil {
		t.Fatal(err)
	}
	if attach.Query().Get("token") != "" || attach.Query().Get("x") != "1" {
		t.Fatalf("attach URL = %s — the probe must not spend a single-use secret", attach)
	}
}
