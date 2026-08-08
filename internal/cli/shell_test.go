package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
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
		err = runCmd(shellCmd(), "app/varuna", "-c", "web")
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

func TestShellUserCloseIsSilent(t *testing.T) {
	srv := terminalServer(t, `{"type":"end","reason":"user_close"}`)
	setupContext(t, srv.URL)
	var err error
	var errOut string
	installShellStdin(t)
	_, errOut = captureOutput(t, func() {
		err = runCmd(shellCmd(), "app/varuna", "-c", "web")
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
		if err := runCmd(shellCmd(), "app/varuna"); err == nil {
			t.Fatal("expected a client error")
		}
	})

	srv := terminalServer(t, `{"type":"end"}`)

	t.Run("non-app ref", func(t *testing.T) {
		setupContext(t, srv.URL)
		if err := runCmd(shellCmd(), "db/pg"); err == nil || !strings.Contains(err.Error(), "app/…") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("bad ref", func(t *testing.T) {
		setupContext(t, srv.URL)
		if err := runCmd(shellCmd(), "nope"); err == nil {
			t.Fatal("expected a ref error")
		}
	})

	t.Run("unknown app", func(t *testing.T) {
		setupContext(t, srv.URL)
		if err := runCmd(shellCmd(), "app/ghost"); err == nil {
			t.Fatal("expected a resolve error")
		}
	})

	t.Run("session mint refused", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
		})
		mux.HandleFunc("/api/v1/applications/app-1/terminal-sessions", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"code":"forbidden","message":"no shell for you"}`))
		})
		deniedSrv := httptest.NewServer(mux)
		defer deniedSrv.Close()
		setupContext(t, deniedSrv.URL)
		if err := runCmd(shellCmd(), "app/varuna"); err == nil || !strings.Contains(err.Error(), "no shell for you") {
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
			w.WriteHeader(410)
		})
		goneSrv := httptest.NewServer(mux)
		defer goneSrv.Close()
		setupContext(t, goneSrv.URL)
		var err error
		installShellStdin(t)
		err = runCmd(shellCmd(), "app/varuna")
		if err == nil || !strings.Contains(err.Error(), "cannot open terminal") {
			t.Fatalf("err = %v", err)
		}
	})
}
