package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stdinFrom replaces os.Stdin with a pipe carrying the given content, closed
// at once so a scanner sees EOF after it. loginWithToken reads the process
// stdin directly — there is no injection point.
func stdinFrom(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
}

func teamsServer(t *testing.T, teams []map[string]string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/teams" {
			w.WriteHeader(500)
			return
		}
		if status >= 400 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"unauthorized","message":"bad token"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": teams})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoginCmdValidation(t *testing.T) {
	setupHome(t)
	if err := runCmd(loginCmd()); err == nil || !strings.Contains(err.Error(), "--url is required") {
		t.Fatalf("err = %v", err)
	}
	if err := runCmd(loginCmd(), "--url", "http://"); err == nil || !strings.Contains(err.Error(), "invalid --url") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginWithTokenSavesTheContext(t *testing.T) {
	srv := teamsServer(t, []map[string]string{{"uuid": "team-1", "name": "core"}}, 0)
	setupHome(t)
	stdinFrom(t, "akd_pasted\n")
	_, _ = captureOutput(t, func() {
		if err := runCmd(loginCmd(), "--url", srv.URL+"/", "--with-token"); err != nil {
			t.Errorf("login: %v", err)
		}
	})

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// The context is named after the host and becomes current; the token lands
	// in the credentials file only.
	host := strings.TrimPrefix(srv.URL, "http://")
	if cfg.CurrentContext != host {
		t.Fatalf("current context = %q, want %q", cfg.CurrentContext, host)
	}
	ctx := cfg.Contexts[host]
	if ctx.TeamUUID != "team-1" || ctx.Fqdn != host {
		t.Fatalf("context = %+v", ctx)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if creds.Tokens[host] != "akd_pasted" {
		t.Fatalf("token = %q", creds.Tokens[host])
	}
}

func TestLoginWithTokenVariants(t *testing.T) {
	t.Run("no token on stdin", func(t *testing.T) {
		setupHome(t)
		stdinFrom(t, "\n")
		_, _, err := loginWithToken(context.Background(), "http://127.0.0.1:1")
		if err == nil || !strings.Contains(err.Error(), "no token provided") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("rejected token", func(t *testing.T) {
		srv := teamsServer(t, nil, 401)
		setupHome(t)
		stdinFrom(t, "akd_bad\n")
		_, _, err := loginWithToken(context.Background(), srv.URL)
		if err == nil || !strings.Contains(err.Error(), "token rejected") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("no team", func(t *testing.T) {
		srv := teamsServer(t, nil, 0)
		setupHome(t)
		stdinFrom(t, "akd_tok\n")
		token, team, err := loginWithToken(context.Background(), srv.URL)
		if err != nil || token != "akd_tok" || team != "" {
			t.Fatalf("token=%q team=%q err=%v", token, team, err)
		}
	})

	t.Run("several teams picks the first and says so", func(t *testing.T) {
		srv := teamsServer(t, []map[string]string{{"uuid": "team-a"}, {"uuid": "team-b"}}, 0)
		setupHome(t)
		stdinFrom(t, "akd_tok\n")
		var token, team string
		var err error
		_, errOut := captureOutput(t, func() {
			token, team, err = loginWithToken(context.Background(), srv.URL)
		})
		if err != nil || token != "akd_tok" || team != "team-a" {
			t.Fatalf("token=%q team=%q err=%v", token, team, err)
		}
		if !strings.Contains(errOut, "several teams") {
			t.Fatalf("stderr = %q", errOut)
		}
	})
}

// cliAuthServer fakes /auth/cli/start and /auth/cli/token. The first token
// poll answers "pending", the second delivers the token — the shortest path
// that still exercises the polling loop.
func cliAuthServer(t *testing.T, startStatus int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/cli/start":
			if startStatus >= 400 {
				w.WriteHeader(startStatus)
				_, _ = w.Write([]byte(`{"code":"boom","message":"start refused"}`))
				return
			}
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["challenge"] == "" {
				t.Error("start carried no PKCE challenge")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request_id": "req-1", "user_code": "WXYZ", "interval": 1, "expires_in": 30,
			})
		case "/auth/cli/token":
			if polls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "approved", "token": "akd_browser", "team_uuid": "team-b",
			})
		default:
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

func TestLoginBrowserFlow(t *testing.T) {
	srv, polls := cliAuthServer(t, 0)
	var token, team string
	var err error
	_, errOut := captureOutput(t, func() {
		token, team, err = loginBrowser(context.Background(), srv.URL, "read,write", true)
	})
	if err != nil || token != "akd_browser" || team != "team-b" {
		t.Fatalf("token=%q team=%q err=%v", token, team, err)
	}
	if polls.Load() != 2 {
		t.Fatalf("polls = %d, want the pending answer to be retried", polls.Load())
	}
	// The confirmation code and the fallback verify URL (server sent none) are
	// both printed for the human to check.
	if !strings.Contains(errOut, "WXYZ") || !strings.Contains(errOut, "/cli/authorize?request_id=req-1") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestLoginBrowserStartRefused(t *testing.T) {
	srv, _ := cliAuthServer(t, 500)
	_, _, err := loginBrowser(context.Background(), srv.URL, "read", true)
	if err == nil || !strings.Contains(err.Error(), "login start failed") {
		t.Fatalf("err = %v", err)
	}
}

// The browser path through the command itself; the failed start comes back as
// the command's error.
func TestLoginCmdBrowserPath(t *testing.T) {
	srv, _ := cliAuthServer(t, 500)
	setupHome(t)
	var err error
	_, _ = captureOutput(t, func() {
		err = runCmd(loginCmd(), "--url", srv.URL, "--no-browser")
	})
	if err == nil || !strings.Contains(err.Error(), "login start failed") {
		t.Fatalf("err = %v", err)
	}
}

// Without --no-browser the flow tries the launcher; with an empty PATH that is
// inert (best-effort by contract) and the flow proceeds to the poll.
func TestLoginBrowserOpensTheLauncher(t *testing.T) {
	t.Setenv("PATH", "")
	srv, _ := cliAuthServer(t, 0)
	var token string
	var err error
	_, _ = captureOutput(t, func() {
		token, _, err = loginBrowser(context.Background(), srv.URL, "read", false)
	})
	if err != nil || token != "akd_browser" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestLoginBrowserCancelled(t *testing.T) {
	// Ctrl-C while waiting for the browser approval: the start succeeds, the
	// cancellation lands during the poll wait.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/cli/start" {
			t.Errorf("unexpected call to %s after cancellation", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"request_id": "req-1", "user_code": "WXYZ", "interval": 1, "expires_in": 30,
		})
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
	}))
	defer srv.Close()
	_, _ = captureOutput(t, func() {
		if _, _, err := loginBrowser(ctx, srv.URL, "read", true); err != context.Canceled {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}

func TestLoginBrowserPollError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/cli/start":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request_id": "req-1", "user_code": "WXYZ",
				"verify_url": "https://x/verify", "interval": 1, "expires_in": 30,
			})
		default:
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"code":"denied","message":"request rejected"}`))
		}
	}))
	defer srv.Close()
	_, errOut := captureOutput(t, func() {
		if _, _, err := loginBrowser(context.Background(), srv.URL, "read", true); err == nil || !strings.Contains(err.Error(), "request rejected") {
			t.Errorf("err = %v", err)
		}
	})
	// The server-provided verify URL is used verbatim when present.
	if !strings.Contains(errOut, "https://x/verify") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestPostJSONErrors(t *testing.T) {
	ctx := context.Background()
	if err := postJSON(ctx, "http://127.0.0.1:1/x", map[string]string{}, nil); err == nil {
		t.Fatal("expected a connection error")
	}
	if err := postJSON(ctx, "http://x/y", make(chan int), nil); err == nil {
		t.Fatal("expected a marshal error")
	}
	if err := postJSON(ctx, "http://[::1]:namedport/x", map[string]string{}, nil); err == nil {
		t.Fatal("expected a request build error")
	}
	// A 2xx with no out decodes nothing and succeeds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	}))
	defer srv.Close()
	if err := postJSON(ctx, srv.URL, map[string]string{}, nil); err != nil {
		t.Fatal(err)
	}
}

// openBrowser is best-effort by contract; with an empty PATH the launcher
// binary cannot be found and the error must come back instead of a panic.
func TestOpenBrowserWithoutLauncher(t *testing.T) {
	t.Setenv("PATH", "")
	if err := openBrowser("https://example.com"); err == nil {
		t.Fatal("expected an exec error with an empty PATH")
	}
}

func TestLoginCmdSaveFailure(t *testing.T) {
	srv := teamsServer(t, nil, 0)
	setupHome(t)
	// A read-only $HOME: LoadConfig succeeds (no file yet), Save cannot create
	// ~/.akerdock and the failure must surface.
	home := t.TempDir()
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	t.Setenv("HOME", home)
	stdinFrom(t, "akd_tok\n")
	_, _ = captureOutput(t, func() {
		if err := runCmd(loginCmd(), "--url", srv.URL, "--with-token"); err == nil {
			t.Error("expected the save failure to surface")
		}
	})
}
