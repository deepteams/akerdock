package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// forbiddenServer fails the test on any request. `whoami` answers "where am I
// pointed, as whom" — a question worth asking before a destructive command and
// worthless if it can only be answered while the instance is up — so the
// no-network promise is asserted with a server that cannot be touched silently.
func forbiddenServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("whoami made an HTTP request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWhoamiMakesNoHTTPRequest(t *testing.T) {
	srv := forbiddenServer(t)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(whoamiCmd()) })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"context", "test", srv.URL, "team-1", "stored"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want it to contain %q", out, want)
		}
	}
}

// The token is what makes a `whoami` unpasteable into an issue. It is never
// printed — not the value, not a prefix, not a truncation.
func TestWhoamiNeverPrintsTheToken(t *testing.T) {
	srv := forbiddenServer(t)
	setupContext(t, srv.URL) // stores the token "akd_secret"

	for _, format := range []string{"table", "json"} {
		flags.output = format
		var err error
		out, errOut := captureOutput(t, func() { err = runCmd(whoamiCmd()) })
		if err != nil {
			t.Fatalf("-o %s: err = %v", format, err)
		}
		for _, leak := range []string{"akd_secret", "akd_", "secret"} {
			if strings.Contains(out+errOut, leak) {
				t.Fatalf("-o %s printed %q: %q", format, leak, out+errOut)
			}
		}
	}
}

// The offline answer is still an answer: an instance that is down is exactly
// when someone checks where they are pointed.
func TestWhoamiWorksAgainstADeadInstance(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing is listening any more

	setupContext(t, url)
	var err error
	out, _ := captureOutput(t, func() { err = runCmd(whoamiCmd()) })
	if err != nil {
		t.Fatalf("err = %v — whoami is local and must not depend on the instance", err)
	}
	if !strings.Contains(out, url) {
		t.Fatalf("stdout = %q", out)
	}
}

func TestWhoamiJSON(t *testing.T) {
	srv := forbiddenServer(t)
	setupContext(t, srv.URL)
	flags.output = "json"

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(whoamiCmd()) })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	if got["context"] != "test" || got["url"] != srv.URL || got["team_uuid"] != "team-1" || got["authenticated"] != true {
		t.Fatalf("got = %+v", got)
	}
	// No token field at all: the absence is the guarantee, not a formatting rule.
	for _, key := range []string{"token", "credential", "scopes"} {
		if _, present := got[key]; present {
			t.Fatalf("whoami -o json exposes %q: %+v", key, got)
		}
	}
}

// A context with no stored token is a real state, and the answer is the command
// that fixes it rather than a blank cell.
func TestWhoamiWithoutACredential(t *testing.T) {
	setupHome(t)
	cfg := &Config{
		CurrentContext: "prod",
		Contexts:       map[string]Context{"prod": {URL: "https://manager.example.com", Fqdn: "manager.example.com"}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(whoamiCmd()) })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "akerdock login --context prod") {
		t.Fatalf("stdout = %q", out)
	}
	// No team_uuid stored: the token acts in the server's default, and saying so
	// beats printing nothing.
	if !strings.Contains(out, "default team") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestWhoamiWithoutAContext(t *testing.T) {
	setupHome(t)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(whoamiCmd()) })
	if err == nil || !strings.Contains(err.Error(), "akerdock login") {
		t.Fatalf("err = %v", err)
	}
}

// whoami is transversal (ADR-070 §1): it targets no type, so it takes no name.
func TestWhoamiTakesNoArgument(t *testing.T) {
	setupHome(t)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(whoamiCmd(), "varuna") })
	if err == nil {
		t.Fatal("whoami accepted a positional argument")
	}
}
