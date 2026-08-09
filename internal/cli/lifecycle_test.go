package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// lifecycleServer accepts every lifecycle POST, records the method, path, body
// and query it was called with, and answers the 202 envelope of §24.1.
func lifecycleServer(t *testing.T, got *struct{ method, path, query, body string }) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, list := range []string{"/api/v1/applications", "/api/v1/databases", "/api/v1/services"} {
		mux.HandleFunc(list, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[
				{"uuid":"app-1","name":"varuna"},{"uuid":"db-1","name":"pg"},{"uuid":"svc-1","name":"monitoring"}
			]}`))
		})
	}
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.method, got.path, got.query, got.body = r.Method, r.URL.Path, r.URL.RawQuery, string(body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_uuid":"jb9x2mc","status_url":"/jobs/jb9x2mc"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// groupFor mounts the lifecycle verbs the way cli.go does, so the test drives
// the real `akerdock <group> <verb>` path rather than a bare command.
func groupFor(k resourceKind) *cobra.Command {
	cmd := &cobra.Command{Use: k.group}
	cmd.AddCommand(lifecycleCmds(k)...)
	return cmd
}

// The three verbs under each of the three groups reach the right path, with the
// method the spec declares (ADR-070 §Verification).
func TestLifecycleReachesTheRightEndpoint(t *testing.T) {
	cases := []struct {
		kind resourceKind
		name string
		verb string
		path string
	}{
		{kindApp, "varuna", "restart", "/api/v1/applications/app-1/restart"},
		{kindApp, "varuna", "start", "/api/v1/applications/app-1/start"},
		{kindApp, "varuna", "stop", "/api/v1/applications/app-1/stop"},
		{kindDB, "pg", "restart", "/api/v1/databases/db-1/restart"},
		{kindDB, "pg", "start", "/api/v1/databases/db-1/start"},
		{kindDB, "pg", "stop", "/api/v1/databases/db-1/stop"},
		{kindSvc, "monitoring", "restart", "/api/v1/services/svc-1/restart"},
		{kindSvc, "monitoring", "start", "/api/v1/services/svc-1/start"},
		{kindSvc, "monitoring", "stop", "/api/v1/services/svc-1/stop"},
	}
	for _, tc := range cases {
		t.Run(tc.kind.group+" "+tc.verb, func(t *testing.T) {
			var got struct{ method, path, query, body string }
			srv := lifecycleServer(t, &got)
			setupContext(t, srv.URL)

			var err error
			out, _ := captureOutput(t, func() {
				err = runCmd(groupFor(tc.kind), tc.verb, tc.name)
			})
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got.method != http.MethodPost || got.path != tc.path {
				t.Fatalf("%s %s, want POST %s", got.method, got.path, tc.path)
			}
			// None of the nine endpoints declares a body or a query parameter;
			// sending one would be the client inventing a contract.
			if got.body != "" || got.query != "" {
				t.Fatalf("body = %q, query = %q — the endpoint declares neither", got.body, got.query)
			}
			// The 202 is an acceptance, not a completion: the confirmation must
			// not claim the containers have already moved.
			if !strings.Contains(out, tc.name+": "+tc.verb+" accepted") || !strings.Contains(out, "jb9x2mc") {
				t.Fatalf("stdout = %q", out)
			}
		})
	}
}

// The flags the API does not accept are absent from the tree, not accepted and
// dropped: a --pr that restarted production would be a silent lie.
func TestLifecycleHasNoComponentOrPreviewFlag(t *testing.T) {
	for _, k := range []resourceKind{kindApp, kindDB, kindSvc} {
		for _, cmd := range lifecycleCmds(k) {
			for _, flag := range []string{"component", "pr"} {
				if f := cmd.Flags().Lookup(flag); f != nil {
					t.Errorf("%s %s declares --%s, which no lifecycle endpoint accepts", k.group, cmd.Name(), flag)
				}
			}
		}
	}
}

// The name may be omitted where `.akerdock` supplies one — applications only.
func TestLifecycleUsesTheDirectoryDefault(t *testing.T) {
	var got struct{ method, path, query, body string }
	srv := lifecycleServer(t, &got)
	setupContext(t, srv.URL)
	// setupContext already moved us to an empty temp dir; write the file there.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, ".akerdock"), []byte("application: varuna\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var runErr error
	_, _ = captureOutput(t, func() { runErr = runCmd(groupFor(kindApp), "restart") })
	if runErr != nil {
		t.Fatalf("err = %v", runErr)
	}
	if got.path != "/api/v1/applications/app-1/restart" {
		t.Fatalf("path = %q", got.path)
	}
}

// A database has no directory default: the repository declares the app it
// deploys, never the database it talks to — and the refusal says so.
func TestLifecycleWithoutANameOnADatabase(t *testing.T) {
	var got struct{ method, path, query, body string }
	srv := lifecycleServer(t, &got)
	setupContext(t, srv.URL)

	err := runCmd(groupFor(kindDB), "stop")
	if err == nil || !strings.Contains(err.Error(), "no database given") {
		t.Fatalf("err = %v", err)
	}
	if got.path != "" {
		t.Fatalf("path = %q — nothing must be posted before a target is resolved", got.path)
	}
}

// An unknown name fails with the resolver's message, and nothing is posted.
func TestLifecycleUnknownName(t *testing.T) {
	var got struct{ method, path, query, body string }
	srv := lifecycleServer(t, &got)
	setupContext(t, srv.URL)

	err := runCmd(groupFor(kindApp), "restart", "ghost")
	if err == nil || !strings.Contains(err.Error(), `no applications named "ghost"`) {
		t.Fatalf("err = %v", err)
	}
	if got.path != "" {
		t.Fatalf("path = %q — an unresolved target must not reach a lifecycle endpoint", got.path)
	}
}

// -o json hands back the JobAccepted object entire; --quiet hands back the uuid
// alone, which is what a script captures to poll the job.
func TestLifecycleOutputForms(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		var got struct{ method, path, query, body string }
		srv := lifecycleServer(t, &got)
		setupContext(t, srv.URL)
		flags.output = "json"

		var err error
		out, _ := captureOutput(t, func() { err = runCmd(groupFor(kindSvc), "start", "monitoring") })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(out, `"job_uuid": "jb9x2mc"`) || !strings.Contains(out, `"status_url": "/jobs/jb9x2mc"`) {
			t.Fatalf("stdout = %q", out)
		}
	})

	t.Run("quiet", func(t *testing.T) {
		var got struct{ method, path, query, body string }
		srv := lifecycleServer(t, &got)
		setupContext(t, srv.URL)
		flags.quiet = true

		var err error
		out, _ := captureOutput(t, func() { err = runCmd(groupFor(kindDB), "restart", "pg") })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if strings.TrimSpace(out) != "jb9x2mc" {
			t.Fatalf("stdout = %q — quiet prints the job uuid alone", out)
		}
	})
}

// The platform's refusal is what the operator reads, unwrapped.
func TestLifecycleRefused(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
	})
	mux.HandleFunc("/api/v1/applications/app-1/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"forbidden","message":"stopping needs applications:lifecycle"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	setupContext(t, srv.URL)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(groupFor(kindApp), "stop", "varuna") })
	if err == nil || !strings.Contains(err.Error(), "applications:lifecycle") {
		t.Fatalf("err = %v", err)
	}
}
