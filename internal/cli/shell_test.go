package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/deepteams/akerdock/internal/terminal"
	tun "github.com/deepteams/akerdock/internal/tunnel"
)

func TestToWS(t *testing.T) {
	if got := toWS("https://m.example.com"); got != "wss://m.example.com" {
		t.Fatalf("got %q", got)
	}
	if got := toWS("http://127.0.0.1:8080"); got != "ws://127.0.0.1:8080" {
		t.Fatalf("got %q", got)
	}
}

// terminalServer fakes the shell choreography: session mint plus a terminal
// websocket that emits output, ignores an unknown text frame, then ends the
// session with a reason.
func terminalServer(t *testing.T, endPayload string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
	})
	mux.HandleFunc("/api/v1/applications/app-1/terminal-sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("component") != "web" {
			t.Errorf("component query = %q", r.URL.Query().Get("component"))
		}
		_, _ = w.Write([]byte(`{"websocket_path":"/term","token":"tk"}`))
	})
	mux.HandleFunc("/term", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "tk" || r.URL.Query().Get("cols") == "" {
			t.Errorf("terminal query = %q", r.URL.RawQuery)
		}
		// ADR-065 §7: the bottom rung identifies its attacher in the terminal
		// path's own header, so a step-down onto it is a retry and not a replay.
		// The token stays in the query string, where ADR-024 put it; the key
		// does not join it there.
		if r.Header.Get(tun.TerminalHTTP.AttachKeyHeader) == "" {
			t.Error("the WebSocket rung must present the per-mint attach key")
		}
		if r.URL.Query().Get("key") != "" {
			t.Errorf("the attach key must not travel in the query string: %q", r.URL.RawQuery)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		// Poke the resize handler: on a pipe stdin GetSize fails, so nothing
		// is sent — the handler must simply survive the signal.
		time.Sleep(30 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGWINCH)
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("motd\r\n"))
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"noise"}`))
		_ = conn.Write(ctx, websocket.MessageText, []byte(endPayload))
		// Leave the socket to the client's normal close.
		time.Sleep(50 * time.Millisecond)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Shell tests park attachTerminal's keystroke pump on a pipe that never
// delivers and is never closed. The pump goroutine reads the os.Stdin global
// and outlives the call, so writing that global again afterwards would be a
// data race — instead the pipe is installed once and left in place for the
// rest of the test binary. The parked goroutines are a deliberate, bounded
// leak. Tests that feed os.Stdin lines (login) are declared in files that run
// earlier, before any pump exists.
var (
	shellStdinR *os.File
	shellStdinW *os.File // held so the write end is neither closed nor GC'd
)

func installShellStdin(t *testing.T) {
	t.Helper()
	if shellStdinR != nil {
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	shellStdinR, shellStdinW = r, w
	_ = shellStdinW
	os.Stdin = r
}

func TestShellSessionEndsWithReason(t *testing.T) {
	srv := terminalServer(t, `{"type":"end","reason":"container stopped"}`)
	setupContext(t, srv.URL)
	var err error
	var out, errOut string
	installShellStdin(t)
	// One pending keystroke: the pump reads it and relays it as a binary
	// frame before parking on the still-open pipe.
	if _, werr := shellStdinW.WriteString("k"); werr != nil {
		t.Fatal(werr)
	}
	out, errOut = captureOutput(t, func() {
		err = runCmd(shellCmd(kindApp), "varuna", "-c", "web")
	})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	if !strings.Contains(out, "motd") {
		t.Fatalf("stdout = %q", out)
	}
	if !strings.Contains(errOut, "session ended: container stopped") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// ADR-066: the shell that never appeared is explained on the end message and
// nowhere else, so the operator sentence the server sends beside the reason is
// what the developer must read — not `session ended: target_unreachable`.
func TestShellReportsAnUnreachableTargetInWords(t *testing.T) {
	srv := terminalServer(t,
		`{"type":"end","reason":"target_unreachable","msg":"the container is not running — start it first"}`)
	setupContext(t, srv.URL)
	installShellStdin(t)
	var err error
	_, errOut := captureOutput(t, func() {
		err = runCmd(shellCmd(kindApp), "varuna", "-c", "web")
	})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	if !strings.Contains(errOut, "the container is not running") {
		t.Fatalf("stderr = %q — the server's sentence is the whole diagnosis", errOut)
	}
	if strings.Contains(errOut, "target_unreachable") {
		t.Fatalf("stderr = %q — the enum value is not a sentence", errOut)
	}
}

func TestShellUserCloseIsSilent(t *testing.T) {
	srv := terminalServer(t, `{"type":"end","reason":"user_close"}`)
	setupContext(t, srv.URL)
	var err error
	var errOut string
	installShellStdin(t)
	_, errOut = captureOutput(t, func() {
		err = runCmd(shellCmd(kindApp), "varuna", "-c", "web")
	})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	if strings.Contains(errOut, "session ended") {
		t.Fatalf("a deliberate close should stay silent: %q", errOut)
	}
}

func TestShellErrors(t *testing.T) {
	t.Run("without a client", func(t *testing.T) {
		setupHome(t)
		if err := runCmd(shellCmd(kindApp), "varuna"); err == nil {
			t.Fatal("expected a client error")
		}
	})

	srv := terminalServer(t, `{"type":"end"}`)

	// The type/name form is gone (ADR-070 §5) and must be refused by naming the
	// spelling that replaced it — never resolved as a literal name.
	t.Run("the old REF form is refused by name", func(t *testing.T) {
		setupContext(t, srv.URL)
		err := runCmd(shellCmd(kindApp), "db/pg")
		if err == nil || !strings.Contains(err.Error(), "akerdock db <verb> pg") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("bad ref", func(t *testing.T) {
		setupContext(t, srv.URL)
		if err := runCmd(shellCmd(kindApp), "nope"); err == nil {
			t.Fatal("expected a ref error")
		}
	})

	t.Run("unknown app", func(t *testing.T) {
		setupContext(t, srv.URL)
		if err := runCmd(shellCmd(kindApp), "ghost"); err == nil {
			t.Fatal("expected a resolve error")
		}
	})

	t.Run("session mint refused", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
		})
		mux.HandleFunc("/api/v1/applications/app-1/terminal-sessions", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"forbidden","message":"no shell for you"}`))
		})
		deniedSrv := httptest.NewServer(mux)
		defer deniedSrv.Close()
		setupContext(t, deniedSrv.URL)
		if err := runCmd(shellCmd(kindApp), "varuna"); err == nil || !strings.Contains(err.Error(), "no shell for you") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("terminal dial refused", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
		})
		mux.HandleFunc("/api/v1/applications/app-1/terminal-sessions", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"websocket_path":"/term","token":"tk"}`))
		})
		mux.HandleFunc("/term", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusGone)
		})
		goneSrv := httptest.NewServer(mux)
		defer goneSrv.Close()
		setupContext(t, goneSrv.URL)
		var err error
		installShellStdin(t)
		err = runCmd(shellCmd(kindApp), "varuna")
		if err == nil || !strings.Contains(err.Error(), "cannot open terminal") {
			t.Fatalf("err = %v", err)
		}
	})
}

// fakeTerminalConn scripts what the transport hands the pump.
type fakeTerminalConn struct{ in chan fakeTerminalMessage }

type fakeTerminalMessage struct {
	typ  terminal.MessageType
	data []byte
	err  error
}

func (c *fakeTerminalConn) Read(ctx context.Context) (terminal.MessageType, []byte, error) {
	select {
	case msg := <-c.in:
		return msg.typ, msg.data, msg.err
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (c *fakeTerminalConn) Write(context.Context, terminal.MessageType, []byte) error { return nil }
func (c *fakeTerminalConn) Ping(context.Context) error                                { return nil }

// The server writes its end message and then lets the transport close. Over
// two wires those two events race, so a stream that died is not the end of the
// story: the reason still has to reach the developer.
func TestDrainTerminalEndPicksUpAReasonStillInFlight(t *testing.T) {
	conn := &fakeTerminalConn{in: make(chan fakeTerminalMessage, 4)}
	conn.in <- fakeTerminalMessage{typ: terminal.MessageBinary, data: []byte("trailing output")}
	conn.in <- fakeTerminalMessage{typ: terminal.MessageText, data: []byte(`{"type":"noise"}`)}
	conn.in <- fakeTerminalMessage{typ: terminal.MessageText, data: []byte(`{"type":"end","reason":"max_duration"}`)}
	if got := drainTerminalEnd(conn); got.reason != "max_duration" {
		t.Fatalf("reason = %q", got.reason)
	}

	// The operator sentence travels beside the reason (ADR-066 §3) and must
	// survive the drain too — it is the half that names the machine.
	spoken := &fakeTerminalConn{in: make(chan fakeTerminalMessage, 1)}
	spoken.in <- fakeTerminalMessage{
		typ:  terminal.MessageText,
		data: []byte(`{"type":"end","reason":"target_unreachable","msg":"the container is not running"}`),
	}
	got := drainTerminalEnd(spoken)
	if got.reason != "target_unreachable" || got.message != "the container is not running" {
		t.Fatalf("end = %+v", got)
	}

	// Nothing in flight: the drain is bounded and says nothing rather than
	// waiting on a transport that is already gone.
	silent := &fakeTerminalConn{in: make(chan fakeTerminalMessage, 1)}
	silent.in <- fakeTerminalMessage{err: terminal.ErrClientClosed}
	start := time.Now()
	if got := drainTerminalEnd(silent); got.reason != "" || got.message != "" {
		t.Fatalf("end = %+v", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the drain took %s — it must be bounded", elapsed)
	}
}

// The terminal's one data stream carries the PTY, so it has nothing to answer a
// 502 on: this line is the ONLY channel a reachability failure reaches the
// developer through (ADR-066 §3), and a bare enum value there is a dead end.
func TestTerminalEndMessagePrefersTheServersSentence(t *testing.T) {
	if got := terminalEndMessage("user_close", ""); got != "" {
		t.Fatalf("a deliberate close should stay silent, got %q", got)
	}
	if got := terminalEndMessage("", ""); got != "" {
		t.Fatalf("no reason at all should stay silent, got %q", got)
	}
	got := terminalEndMessage("target_unreachable", "the server's agent is not connected right now")
	if !strings.Contains(got, "agent is not connected") {
		t.Fatalf("the server's own words must reach the developer, got %q", got)
	}
	if strings.Contains(got, "target_unreachable") {
		t.Fatalf("the enum value is not a sentence, got %q", got)
	}
	if got := terminalEndMessage("target_unreachable", ""); !strings.Contains(got, "could not be reached") {
		t.Fatalf("a bare reason needs a phrased fallback, got %q", got)
	}
	// ADR-067's wake: the developer stopped nothing, so scale-to-zero is named.
	if got := terminalEndMessage("wake_failed", ""); !strings.Contains(got, "scale-to-zero") {
		t.Fatalf("a failed wake must name the mechanism, got %q", got)
	}
	// Anything else still surfaces, and a sentence beside it still wins.
	if got := terminalEndMessage("idle_timeout", ""); !strings.Contains(got, "idle_timeout") {
		t.Fatalf("an unphrased reason must still be reported, got %q", got)
	}
	if got := terminalEndMessage("idle_timeout", "no keystroke for 30 minutes"); !strings.Contains(got, "30 minutes") {
		t.Fatalf("a sentence beside the reason wins, got %q", got)
	}
}

// The WebSocket rung's adapter: the pump reads terminal messages, whatever
// carried them. A clean close is a wanted close, not a vanished peer.
func TestWebSocketTerminalConnAdaptsBothDirections(t *testing.T) {
	received := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"end","reason":"revoked"}`))
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("motd"))
		// Read to the end: coder/websocket answers a ping from its read loop,
		// so a server that stopped reading would time the ping out.
		for {
			_, data, readErr := conn.Read(ctx)
			if readErr != nil {
				return
			}
			if string(data) == "bye" {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			received <- string(data)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, _, err := websocket.Dial(ctx, toWS(srv.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn := wsTerminalConn{raw}

	typ, data, err := conn.Read(ctx)
	if err != nil || typ != terminal.MessageText || !strings.Contains(string(data), "revoked") {
		t.Fatalf("read = %d, %q, %v", typ, data, err)
	}
	typ, data, err = conn.Read(ctx)
	if err != nil || typ != terminal.MessageBinary || string(data) != "motd" {
		t.Fatalf("read = %d, %q, %v", typ, data, err)
	}

	if err := conn.Write(ctx, terminal.MessageText, []byte(`{"type":"resize","cols":90,"rows":30}`)); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, terminal.MessageBinary, []byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	if got := <-received; !strings.Contains(got, "resize") {
		t.Fatalf("the server received %q", got)
	}
	if got := <-received; got != "ls\n" {
		t.Fatalf("the server received %q", got)
	}
	// The pong is processed by whoever is reading — which in production is the
	// pump, running concurrently with the bridge's heartbeat.
	closed := make(chan error, 1)
	go func() {
		_, _, readErr := conn.Read(ctx)
		closed <- readErr
	}()
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := conn.Write(ctx, terminal.MessageBinary, []byte("bye")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, terminal.ErrClientClosed) {
			t.Fatalf("a normal closure must read as a wanted close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the close never reached the pump")
	}
}

// The geometry the session opens with: the local window when there is one,
// and what a terminal that will not say is assumed to be otherwise. The test
// binary's stdin is a pipe, which is the second case.
func TestTerminalSizeFallsBackToEightyByTwentyFour(t *testing.T) {
	installShellStdin(t)
	if cols, rows := terminalSize(); cols != 80 || rows != 24 {
		t.Fatalf("size = %dx%d", cols, rows)
	}
}
