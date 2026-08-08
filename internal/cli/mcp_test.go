package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMcpTarget(t *testing.T) {
	t.Run("flags win", func(t *testing.T) {
		setupHome(t)
		base, token, err := mcpTarget("https://x.example.com/", "akd_flag")
		if err != nil || base != "https://x.example.com" || token != "akd_flag" {
			t.Fatalf("base=%q token=%q err=%v", base, token, err)
		}
	})

	t.Run("environment", func(t *testing.T) {
		setupHome(t)
		t.Setenv("AKERDOCK_URL", "https://env.example.com")
		t.Setenv("AKERDOCK_TOKEN", "akd_env")
		base, token, err := mcpTarget("", "")
		if err != nil || base != "https://env.example.com" || token != "akd_env" {
			t.Fatalf("base=%q token=%q err=%v", base, token, err)
		}
	})

	t.Run("context fills the gaps", func(t *testing.T) {
		setupContext(t, "https://ctx.example.com")
		base, token, err := mcpTarget("", "")
		if err != nil || base != "https://ctx.example.com" || token != "akd_secret" {
			t.Fatalf("base=%q token=%q err=%v", base, token, err)
		}
		// A flag overrides only its half; the context supplies the rest.
		base, token, err = mcpTarget("", "akd_flag")
		if err != nil || base != "https://ctx.example.com" || token != "akd_flag" {
			t.Fatalf("base=%q token=%q err=%v", base, token, err)
		}
	})

	t.Run("nothing configured points at both flags", func(t *testing.T) {
		setupHome(t)
		_, _, err := mcpTarget("", "")
		if err == nil || !strings.Contains(err.Error(), "--url and --token") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("half-configured keeps the original error", func(t *testing.T) {
		setupHome(t)
		_, _, err := mcpTarget("https://only-url.example.com", "")
		if err == nil || strings.Contains(err.Error(), "--url and --token") {
			t.Fatalf("err = %v", err)
		}
	})
}

// mcpServer answers /mcp the way the instance does: echoes requests, 202 for
// notifications (no id in the payload).
func mcpServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer akd_tok" {
			w.WriteHeader(401)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"id"`)) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_, _ = w.Write(append([]byte(`{"jsonrpc":"2.0","result":"ok","echo":`), append(body, '}')...))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMcpCmdBridgesStdio(t *testing.T) {
	srv := mcpServer(t)
	setupHome(t)
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n" +
			"\n" + // blank line: ignored
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out bytes.Buffer
	cmd := mcpCmd()
	cmd.SetIn(in)
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--url", srv.URL, "--token", "akd_tok"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// One response for the request, none for the notification (202).
	if len(lines) != 1 || !strings.Contains(lines[0], `"tools/list"`) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestMcpCmdTargetError(t *testing.T) {
	setupHome(t)
	cmd := mcpCmd()
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected a target resolution error")
	}
}

func TestMcpBridgeTransportErrorStaysInProtocol(t *testing.T) {
	// The instance is unreachable: the assistant must receive a JSON-RPC error
	// carrying its request id, not a dead pipe.
	in := strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	if err := mcpBridge(in, &out, "http://127.0.0.1:1", "tok"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"id":7`) || !strings.Contains(out.String(), `-32000`) {
		t.Fatalf("output = %q", out.String())
	}

	// A failing notification has nobody to tell: silence, not a bogus reply.
	out.Reset()
	in = strings.NewReader(`{"jsonrpc":"2.0","method":"notify"}` + "\n")
	if err := mcpBridge(in, &out, "http://127.0.0.1:1", "tok"); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q", out.String())
	}
}

func TestMcpBridgeReadError(t *testing.T) {
	if err := mcpBridge(errReader{}, io.Discard, "http://x", "tok"); err == nil || err.Error() != "read failed" {
		t.Fatalf("err = %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestMcpBridgeWriteError(t *testing.T) {
	srv := mcpServer(t)
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"m"}` + "\n")
	if err := mcpBridge(in, errWriter{}, srv.URL, "akd_tok"); err == nil {
		t.Fatal("expected the write failure to surface")
	}
}

func TestMcpForwardStatuses(t *testing.T) {
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("details"))
	}))
	defer srv.Close()
	client := srv.Client()
	msg := []byte(`{"id":1}`)

	for _, tc := range []struct {
		status int
		want   string
	}{
		{404, "does not expose MCP"},
		{401, "token was refused"},
		{500, "answered 500"},
	} {
		status = tc.status
		_, err := mcpForward(client, srv.URL, "tok", msg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: err = %v, want %q", tc.status, err, tc.want)
		}
	}

	status = 202
	resp, err := mcpForward(client, srv.URL, "tok", msg)
	if err != nil || resp != nil {
		t.Fatalf("202 should be silent, got resp=%q err=%v", resp, err)
	}

	status = 200
	resp, err = mcpForward(client, srv.URL, "tok", msg)
	if err != nil || string(resp) != "details" {
		t.Fatalf("200 body lost: resp=%q err=%v", resp, err)
	}
}

func TestMcpTransportErrorWithoutID(t *testing.T) {
	if got := mcpTransportError([]byte(`{"method":"notify"}`), errors.New("x")); got != nil {
		t.Fatalf("got %q", got)
	}
	if got := mcpTransportError([]byte(`not json`), errors.New("x")); got != nil {
		t.Fatalf("got %q", got)
	}
}
