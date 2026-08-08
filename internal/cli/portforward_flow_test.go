package cli

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestHandshakeReason(t *testing.T) {
	if got := handshakeReason(nil); got != "" {
		t.Fatalf("nil response: %q", got)
	}
	if got := handshakeReason(&http.Response{}); got != "" {
		t.Fatalf("nil body: %q", got)
	}
	rec := httptest.NewRecorder()
	_, _ = rec.WriteString(`{"message":"server offline"}`)
	if got := handshakeReason(rec.Result()); got != "server offline" {
		t.Fatalf("got %q", got)
	}
	rec = httptest.NewRecorder()
	_, _ = rec.WriteString("not json")
	if got := handshakeReason(rec.Result()); got != "" {
		t.Fatalf("invalid body: %q", got)
	}
}

// tunnelServer fakes the whole port-forward choreography: the REST mints and
// a live akerdock-tunnel-v1 websocket that answers one stream then closes
// with a policy reason.
func tunnelServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/databases", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"db-1","name":"pg"},{"uuid":"db-9","name":"refused"}]}`))
	})
	mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
	})
	mux.HandleFunc("/api/v1/applications/app-1/previews", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"pv-1","pr_id":8,"status":"active"}]}`))
	})
	mux.HandleFunc("/api/v1/ingress-endpoints", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"ig-1","name":"dev"}]}`))
	})
	mux.HandleFunc("/api/v1/external-endpoints", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"ep-1","name":"replica"}]}`))
	})
	mux.HandleFunc("/api/v1/external-endpoints/ep-1/port-forwards", func(w http.ResponseWriter, r *http.Request) {
		// ADR-045: an endpoint mint takes no body at all.
		if body, _ := io.ReadAll(r.Body); len(body) != 0 {
			t.Errorf("endpoint mint carried a body: %q", body)
		}
		_, _ = w.Write([]byte(`{"websocket_path":"/refused","token":"tk"}`))
	})
	mux.HandleFunc("/api/v1/databases/db-1/port-forwards", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]int
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["port"] != 5432 {
			t.Errorf("mint body port = %d", body["port"])
		}
		until := time.Now().Add(time.Hour).Format(time.RFC3339)
		_, _ = fmt.Fprintf(w, `{"websocket_path":"/tunnel","token":"tk","authorized_until":%q}`, until)
	})
	mux.HandleFunc("/api/v1/databases/db-9/port-forwards", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"websocket_path":"/refused","token":"tk"}`))
	})
	mux.HandleFunc("/api/v1/applications/app-1/previews/pv-1/port-forwards", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"websocket_path":"/refused","token":"tk"}`))
	})
	mux.HandleFunc("/refused", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"the server is not reachable over SSH right now"}`))
	})
	mux.HandleFunc("/tunnel", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"akerdock-tunnel-v1"}})
		if err != nil {
			return
		}
		ctx := r.Context()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageText:
				var m tunnelCtrl
				if json.Unmarshal(data, &m) != nil || m.T != "open" {
					continue
				}
				// Answer the new stream with a greeting the test can read.
				frame := make([]byte, 4+5)
				binary.BigEndian.PutUint32(frame, m.ID)
				copy(frame[4:], "hello")
				if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
					return
				}
			case websocket.MessageBinary:
				if len(data) < 4 || string(data[4:]) != "ping" {
					t.Errorf("unexpected tunnel payload %q", data)
				}
				id := binary.BigEndian.Uint32(data[:4])
				// Junk the client must survive: a failed open for another
				// stream, a truncated binary frame, an unparsable control.
				openErr, _ := json.Marshal(tunnelCtrl{T: "open_err", ID: 99, Code: "dial_failed", Msg: "no route"})
				_ = conn.Write(ctx, websocket.MessageText, openErr)
				_ = conn.Write(ctx, websocket.MessageBinary, []byte{0x00})
				_ = conn.Write(ctx, websocket.MessageText, []byte("{not json"))
				eof, _ := json.Marshal(tunnelCtrl{T: "eof", ID: id})
				_ = conn.Write(ctx, websocket.MessageText, eof)
				// End of the session: a policy close the client must explain.
				_ = conn.Close(websocket.StatusNormalClosure, "revoked")
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func dialRetry(t *testing.T, port int) net.Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return conn
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("local forward port never came up")
	return nil
}

// The full journey: mint, websocket dial, local listener, one TCP stream
// relayed both ways, then a server-side close whose reason is explained.
func TestPortForwardRelaysAndExplainsClose(t *testing.T) {
	srv := tunnelServer(t)
	setupContext(t, srv.URL)
	localPort, err := freePort()
	if err != nil {
		t.Fatal(err)
	}

	var cmdErr error
	_, errOut := captureOutput(t, func() {
		done := make(chan error, 1)
		go func() {
			done <- runCmd(portForwardCmd(), "db/pg", fmt.Sprintf("%d:5432", localPort))
		}()
		conn := dialRetry(t, localPort)
		defer func() { _ = conn.Close() }()
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Error(err)
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello" {
			t.Errorf("read %q err=%v", buf, err)
		}
		cmdErr = <-done
	})
	if cmdErr != nil {
		t.Fatalf("port-forward: %v", cmdErr)
	}
	// The opening line announces the local port AND the authorization deadline;
	// the close explains itself.
	if !strings.Contains(errOut, fmt.Sprintf("forwarding 127.0.0.1:%d", localPort)) {
		t.Fatalf("stderr = %q", errOut)
	}
	if !strings.Contains(errOut, "authorized until") {
		t.Fatalf("stderr misses the deadline: %q", errOut)
	}
	if !strings.Contains(errOut, "administrator revoked") {
		t.Fatalf("stderr misses the close reason: %q", errOut)
	}
	// The failed open for the other stream was reported, not swallowed.
	if !strings.Contains(errOut, "stream 99: dial_failed") {
		t.Fatalf("stderr misses the open_err: %q", errOut)
	}
}

// An endpoint forward with no ports argument: the OS picks the local port and
// the announcement names the endpoint's declared target instead of a number.
func TestPortForwardEndpointPicksLocalPort(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/external-endpoints", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"ep-1","name":"replica"}]}`))
	})
	mux.HandleFunc("/api/v1/external-endpoints/ep-1/port-forwards", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"websocket_path":"/quick","token":"tk"}`))
	})
	mux.HandleFunc("/quick", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"akerdock-tunnel-v1"}})
		if err != nil {
			return
		}
		// A deliberate user_close right away: the client exits silently.
		_ = conn.Close(websocket.StatusNormalClosure, "user_close")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	setupContext(t, srv.URL)

	var err error
	_, errOut := captureOutput(t, func() {
		err = runCmd(portForwardCmd(), "endpoint/replica")
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut, "the endpoint's declared target") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestPortForwardHandshakeRefused(t *testing.T) {
	srv := tunnelServer(t)
	setupContext(t, srv.URL)
	var err error
	_, _ = captureOutput(t, func() {
		err = runCmd(portForwardCmd(), "db/refused", "15432:5432")
	})
	if err == nil || !strings.Contains(err.Error(), "not reachable over SSH") {
		t.Fatalf("err = %v", err)
	}
}

func TestPortForwardEndpointNeedsNoPorts(t *testing.T) {
	srv := tunnelServer(t)
	setupContext(t, srv.URL)
	var err error
	_, _ = captureOutput(t, func() {
		err = runCmd(portForwardCmd(), "endpoint/replica")
	})
	// The mint (asserted body-less server-side) succeeds; the refused upgrade
	// ends the run with the server's sentence.
	if err == nil || !strings.Contains(err.Error(), "not reachable over SSH") {
		t.Fatalf("err = %v", err)
	}
}

func TestPortForwardPreview(t *testing.T) {
	srv := tunnelServer(t)
	setupContext(t, srv.URL)
	var err error
	_, _ = captureOutput(t, func() {
		err = runCmd(portForwardCmd(), "app/varuna", "15432:5432", "--pr", "8")
	})
	if err == nil || !strings.Contains(err.Error(), "not reachable over SSH") {
		t.Fatalf("err = %v", err)
	}
}

func TestPortForwardArgumentErrors(t *testing.T) {
	srv := tunnelServer(t)

	t.Run("without a client", func(t *testing.T) {
		setupHome(t)
		if err := runCmd(portForwardCmd(), "db/pg", "1:2"); err == nil {
			t.Fatal("expected a client error")
		}
	})

	run := func(t *testing.T, args ...string) error {
		t.Helper()
		setupContext(t, srv.URL)
		return runCmd(portForwardCmd(), args...)
	}

	t.Run("no port for a container target", func(t *testing.T) {
		if err := run(t, "db/pg"); err == nil || !strings.Contains(err.Error(), "no port given") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad ports", func(t *testing.T) {
		if err := run(t, "db/pg", "x:y"); err == nil {
			t.Fatal("expected a ports error")
		}
	})
	t.Run("bad ref", func(t *testing.T) {
		if err := run(t, "nope/x", "1:2"); err == nil {
			t.Fatal("expected a ref error")
		}
	})
	t.Run("pr on a database", func(t *testing.T) {
		if err := run(t, "db/pg", "1:2", "--pr", "8"); err == nil || !strings.Contains(err.Error(), "--pr only applies") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown preview", func(t *testing.T) {
		if err := run(t, "app/varuna", "1:2", "--pr", "99"); err == nil || !strings.Contains(err.Error(), "no preview") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unsupported kind", func(t *testing.T) {
		if err := run(t, "ingress/dev", "1:2"); err == nil || !strings.Contains(err.Error(), "does not support") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestPortForwardListenFailure(t *testing.T) {
	srv := tunnelServer(t)
	setupContext(t, srv.URL)
	// Occupy the local port before the command asks for it.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	port := busy.Addr().(*net.TCPAddr).Port

	var cmdErr error
	_, _ = captureOutput(t, func() {
		cmdErr = runCmd(portForwardCmd(), "db/pg", fmt.Sprintf("%d:5432", port))
	})
	if cmdErr == nil || !strings.Contains(cmdErr.Error(), "cannot listen") {
		t.Fatalf("err = %v", cmdErr)
	}
}

func TestPortForwardMintAccessRequiredWithoutURL(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/databases", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"db-1","name":"pg"}]}`))
	})
	mux.HandleFunc("/api/v1/databases/db-1/port-forwards", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"access_request_required","message":"needs an access grant"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	setupContext(t, srv.URL)
	var err error
	_, _ = captureOutput(t, func() {
		err = runCmd(portForwardCmd(), "db/pg", "1:5432")
	})
	// No request_url configured: fail with instructions instead of spinning.
	if err == nil || !strings.Contains(err.Error(), "request access from the dashboard") {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitForAccessGrant(t *testing.T) {
	t.Setenv("PATH", "") // neutralize openBrowser: no launcher to run

	t.Run("cancelled while waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var err error
		_, _ = captureOutput(t, func() {
			err = waitForAccessGrant(ctx, &apiError{RequestURL: "https://x/grant"}, func() error { return nil })
		})
		if err != context.Canceled {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("grant arrives", func(t *testing.T) {
		calls := 0
		mint := func() error {
			calls++
			if calls == 1 {
				return &apiError{Code: "access_request_required", Message: "still waiting"}
			}
			return nil
		}
		var err error
		_, _ = captureOutput(t, func() {
			err = waitForAccessGrant(context.Background(), &apiError{RequestURL: "https://x/grant"}, mint)
		})
		if err != nil || calls != 2 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})

	t.Run("any other error is final", func(t *testing.T) {
		boom := errors.New("revoked token")
		var err error
		_, _ = captureOutput(t, func() {
			err = waitForAccessGrant(context.Background(), &apiError{RequestURL: "https://x/grant"}, func() error { return boom })
		})
		if err != boom {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestWarnBeforeExpiry(t *testing.T) {
	t.Run("deadline near prints the heads-up", func(t *testing.T) {
		until := time.Now().Add(2*time.Minute + 200*time.Millisecond)
		_, errOut := captureOutput(t, func() {
			warnBeforeExpiry(context.Background(), until)
		})
		if !strings.Contains(errOut, "authorization ends in 2m") {
			t.Fatalf("stderr = %q", errOut)
		}
	})

	t.Run("deadline already past says nothing", func(t *testing.T) {
		_, errOut := captureOutput(t, func() {
			warnBeforeExpiry(context.Background(), time.Now().Add(-time.Minute))
		})
		if errOut != "" {
			t.Fatalf("stderr = %q", errOut)
		}
	})

	t.Run("cancellation stops the wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, errOut := captureOutput(t, func() {
			warnBeforeExpiry(ctx, time.Now().Add(16*time.Minute))
		})
		if errOut != "" {
			t.Fatalf("stderr = %q", errOut)
		}
	})
}
