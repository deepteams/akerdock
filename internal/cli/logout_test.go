package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLogoutNoContext(t *testing.T) {
	setupHome(t)
	if err := runCmd(logoutCmd()); err == nil || !strings.Contains(err.Error(), "no context selected") {
		t.Fatalf("err = %v", err)
	}
}

func TestLogoutConfigError(t *testing.T) {
	setupHome(t)
	t.Setenv("HOME", "")
	if err := runCmd(logoutCmd()); err == nil {
		t.Fatal("expected a config error")
	}
}

func TestLogoutClearsToken(t *testing.T) {
	setupContext(t, "https://m.example.com")
	out, _ := captureOutput(t, func() {
		if err := runCmd(logoutCmd()); err != nil {
			t.Errorf("logout: %v", err)
		}
	})
	if !strings.Contains(out, `logged out of "test"`) {
		t.Fatalf("out = %q", out)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := creds.Tokens["test"]; ok {
		t.Fatal("token should be gone")
	}
}

// cliTokenName recomputes the identity `login` gives this machine's token —
// revokeOwnCliToken finds the token to delete by this exact name.
func cliTokenName(t *testing.T) string {
	t.Helper()
	host, _ := os.Hostname()
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	return fmt.Sprintf("cli — %s@%s", user, host)
}

func TestLogoutRevoke(t *testing.T) {
	var deleted atomic.Int32
	name := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/teams/team-1/tokens":
			_, _ = fmt.Fprintf(w, `{"data":[{"uuid":"tok-other","name":"ci"},{"uuid":"tok-cli","name":%q}]}`, name)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/teams/team-1/tokens/tok-cli":
			deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	setupContext(t, srv.URL)
	name = cliTokenName(t)

	_, errOut := captureOutput(t, func() {
		if err := runCmd(logoutCmd(), "--revoke"); err != nil {
			t.Errorf("logout --revoke: %v", err)
		}
	})
	if deleted.Load() != 1 {
		t.Fatal("the server-side token was not revoked")
	}
	if strings.Contains(errOut, "could not revoke") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// A failed revocation must not block the local logout: the note tells the
// operator to finish the job in the panel, and the token is still dropped.
func TestLogoutRevokeBestEffort(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"no matching token": func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":[]}`))
			}))
			t.Cleanup(srv.Close)
			setupContext(t, srv.URL)
		},
		"list fails": func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(srv.Close)
			setupContext(t, srv.URL)
		},
		"no team on the context": func(t *testing.T) {
			setupHome(t)
			cfg := &Config{CurrentContext: "test", Contexts: map[string]Context{"test": {URL: "http://127.0.0.1:1"}}}
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
			if err := setToken("test", "akd_x"); err != nil {
				t.Fatal(err)
			}
		},
		"no client at all": func(t *testing.T) {
			// A context with no token: the revocation cannot even build a client.
			setupHome(t)
			cfg := &Config{CurrentContext: "test", Contexts: map[string]Context{"test": {URL: "http://127.0.0.1:1"}}}
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
		},
	}
	for tcName, setup := range cases {
		t.Run(tcName, func(t *testing.T) {
			setup(t)
			out, errOut := captureOutput(t, func() {
				if err := runCmd(logoutCmd(), "--revoke"); err != nil {
					t.Errorf("logout --revoke: %v", err)
				}
			})
			if !strings.Contains(errOut, "could not revoke") {
				t.Errorf("stderr = %q, want the best-effort note", errOut)
			}
			if !strings.Contains(out, "logged out") {
				t.Errorf("out = %q, want the local logout to proceed", out)
			}
		})
	}
}
