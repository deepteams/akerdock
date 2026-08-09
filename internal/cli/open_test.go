package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// stubBrowser replaces the launcher for the duration of a test and records what
// it was asked to open. No test ever launches a real browser: the value of this
// command is the resolution above the launcher, and login.go's spawn is not
// under test here.
func stubBrowser(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	old := browserOpener
	browserOpener = func(u string) error {
		opened = append(opened, u)
		return nil
	}
	t.Cleanup(func() { browserOpener = old })
	return &opened
}

// openServer serves one application, its detail (for server_uuid) and its
// server's domain table.
func openServer(t *testing.T, domains string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna","source_type":"git"}]}`))
	})
	mux.HandleFunc("/api/v1/applications/app-1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"uuid":"app-1","name":"varuna","server_uuid":"srv-1"}`))
	})
	mux.HandleFunc("/api/v1/servers/srv-1/domains", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(domains))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// An application carries no URL: the public address is resolved through its
// server's proxy, and only the entry whose resource_uuid matches counts.
func TestOpenResolvesTheDomainThroughTheServer(t *testing.T) {
	srv := openServer(t, `{"data":[
		{"resource_uuid":"app-other","resource_type":"application","domains":["other.example.com"]},
		{"resource_uuid":"app-1","resource_type":"application","domains":["varuna.example.com","alt.example.com"]}
	]}`)
	setupContext(t, srv.URL)
	opened := stubBrowser(t)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(appGroup(), "open", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(*opened) != 1 || (*opened)[0] != "https://varuna.example.com" {
		t.Fatalf("opened = %v", *opened)
	}
	// Printed as well as opened: on a machine with no browser the URL is the
	// deliverable.
	if !strings.Contains(out, "https://varuna.example.com") {
		t.Fatalf("stdout = %q", out)
	}
	if strings.Contains(out, "other.example.com") {
		t.Fatalf("stdout = %q — another resource's domain was picked up", out)
	}
}

// The schema allows an entry to already carry its scheme, port or path (§4.2);
// only a bare hostname needs one added, and https is what AkerDock serves.
func TestOpenKeepsAnExplicitScheme(t *testing.T) {
	srv := openServer(t, `{"data":[{"resource_uuid":"app-1","resource_type":"application","domains":["http://varuna.example.com:8080/app"]}]}`)
	setupContext(t, srv.URL)
	opened := stubBrowser(t)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(appGroup(), "open", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(*opened) != 1 || (*opened)[0] != "http://varuna.example.com:8080/app" {
		t.Fatalf("opened = %v", *opened)
	}
}

// No domain is a refusal, not a guess: a composed hostname that resolves to
// someone else's application is worse than being told there is nothing to open.
func TestOpenRefusesWhenNoDomainMatches(t *testing.T) {
	cases := map[string]string{
		"the server routes nothing":     `{"data":[]}`,
		"another resource is routed":    `{"data":[{"resource_uuid":"app-other","resource_type":"application","domains":["other.example.com"]}]}`,
		"the entry carries no hostname": `{"data":[{"resource_uuid":"app-1","resource_type":"application","domains":[]}]}`,
	}
	for name, domains := range cases {
		t.Run(name, func(t *testing.T) {
			srv := openServer(t, domains)
			setupContext(t, srv.URL)
			opened := stubBrowser(t)

			var err error
			_, _ = captureOutput(t, func() { err = runCmd(appGroup(), "open", "varuna") })
			if err == nil || !strings.Contains(err.Error(), "no domain") {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(err.Error(), "--dashboard") {
				t.Fatalf("err = %q — the refusal names the way in that does exist", err)
			}
			if len(*opened) != 0 {
				t.Fatalf("opened = %v — nothing was resolved, so nothing is launched", *opened)
			}
		})
	}
}

// --dashboard is the bridge back to the UI for the moments the UI is better,
// and it goes to the instance of the active context — never to the public URL.
func TestOpenDashboardUsesTheContextInstance(t *testing.T) {
	srv := openServer(t, `{"data":[{"resource_uuid":"app-1","resource_type":"application","domains":["varuna.example.com"]}]}`)
	setupContext(t, srv.URL)
	opened := stubBrowser(t)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(appGroup(), "open", "varuna", "--dashboard") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(*opened) != 1 || (*opened)[0] != srv.URL+"/applications/app-1" {
		t.Fatalf("opened = %v", *opened)
	}
}

// A resource with no domain still has a dashboard page: --dashboard must not
// pay for the resolution it does not need.
func TestOpenDashboardNeedsNoDomain(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
	})
	mux.HandleFunc("/api/v1/servers/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("--dashboard resolved domains it does not need: %s", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	setupContext(t, srv.URL)
	opened := stubBrowser(t)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(appGroup(), "open", "varuna", "--dashboard") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(*opened) != 1 || (*opened)[0] != srv.URL+"/applications/app-1" {
		t.Fatalf("opened = %v", *opened)
	}
}

// The permission asymmetry is real: domains live under `servers:read`, which a
// reader of applications does not necessarily hold. The API's refusal names the
// permission, so it is passed through rather than reworded.
func TestOpenPassesThroughAPermissionRefusal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
	})
	mux.HandleFunc("/api/v1/applications/app-1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"uuid":"app-1","server_uuid":"srv-1"}`))
	})
	mux.HandleFunc("/api/v1/servers/srv-1/domains", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"forbidden","message":"listing a server's domains needs servers:read"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	setupContext(t, srv.URL)
	stubBrowser(t)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(appGroup(), "open", "varuna") })
	if err == nil || !strings.Contains(err.Error(), "servers:read") {
		t.Fatalf("err = %v", err)
	}
}

// -o json makes the resolution scriptable without a browser in the loop.
func TestOpenJSONPrintsTheResolvedURL(t *testing.T) {
	srv := openServer(t, `{"data":[{"resource_uuid":"app-1","resource_type":"application","domains":["varuna.example.com"]}]}`)
	setupContext(t, srv.URL)
	stubBrowser(t)
	flags.output = "json"

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(appGroup(), "open", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	if got.URL != "https://varuna.example.com" {
		t.Fatalf("got = %+v", got)
	}
}

// A machine with no launcher is not a failed command: the URL is already out.
func TestOpenSurvivesAMissingBrowser(t *testing.T) {
	srv := openServer(t, `{"data":[{"resource_uuid":"app-1","resource_type":"application","domains":["varuna.example.com"]}]}`)
	setupContext(t, srv.URL)
	old := browserOpener
	browserOpener = func(string) error { return errors.New("exec: \"xdg-open\": executable file not found in $PATH") }
	t.Cleanup(func() { browserOpener = old })

	var err error
	out, errOut := captureOutput(t, func() { err = runCmd(appGroup(), "open", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "https://varuna.example.com") {
		t.Fatalf("stdout = %q", out)
	}
	if !strings.Contains(errOut, "could not launch a browser") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// `open` belongs to the application group only: a database has no public URL,
// and the tree must say so in --help rather than at runtime (ADR-070 §1).
func TestOpenIsOnlyUnderTheApplicationGroup(t *testing.T) {
	for _, group := range []*cobra.Command{dbGroup(), svcGroup()} {
		for _, c := range group.Commands() {
			if c.Name() == "open" {
				t.Fatalf("`akerdock %s open` exists — only an application has a public URL", group.Name())
			}
		}
	}
}

// The removed REF spelling is refused by name, not left to fail as a lookup.
func TestOpenRefusesTheRefSpelling(t *testing.T) {
	srv := openServer(t, `{"data":[]}`)
	setupContext(t, srv.URL)
	stubBrowser(t)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(appGroup(), "open", "app/varuna") })
	if err == nil || !strings.Contains(err.Error(), "akerdock app <verb> varuna") {
		t.Fatalf("err = %v", err)
	}
}
