package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// previewServer serves one application and its previews, and records the
// method, path and body of whatever act the CLI posts. Two previews, chosen so
// the table has something to say: a live one with a URL, and a fork that is
// queued because nobody approved it.
func previewServer(t *testing.T, method, path, body *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
	})
	mux.HandleFunc("/api/v1/applications/app-1/previews", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"uuid":"pv-7","pr_id":7,"status":"queued","source_branch":"patch-1","head_sha":"bbb",
			 "is_fork":true,"fork_approved":false,"fqdn":null,"last_deployed_at":null,
			 "created_at":"2026-08-09T09:00:00Z"},
			{"uuid":"pv-42","pr_id":42,"status":"active","source_branch":"feat/tunnels","head_sha":"aaa",
			 "is_fork":false,"fork_approved":false,"fqdn":"pr-42.example.com",
			 "last_deployed_at":"2026-08-09T10:00:00Z","created_at":"2026-08-09T08:00:00Z"}
		]}`))
	})
	mux.HandleFunc("/api/v1/applications/app-1/previews/", func(w http.ResponseWriter, r *http.Request) {
		*method, *path = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		*body = string(b)
		if strings.HasSuffix(r.URL.Path, "/keep") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"deployment_uuid":"dp-1","status_url":"/deployments/dp-1"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The listing is what a reviewer reads instead of opening the dashboard
// (ADR-059): the PR, its state, where to click, and why a fork is not running.
func TestPreviewList(t *testing.T) {
	var method, path, body string
	srv := previewServer(t, &method, &path, &body)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(previewCmd(), "list", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{
		"#42", "active", "https://pr-42.example.com", "feat/tunnels", "pv-42",
		"#7", "queued", "patch-1 (fork, unapproved)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want it to contain %q", out, want)
		}
	}
	// Newest pull request first: the one being worked on is the one at the top.
	if strings.Index(out, "#42") > strings.Index(out, "#7") {
		t.Fatalf("stdout = %q — previews are ordered by PR, newest first", out)
	}
	// A preview that never deployed says so; an empty cell would read as a bug.
	if !strings.Contains(out, "-") {
		t.Fatalf("stdout = %q — a null last deployment renders as a dash", out)
	}
}

// `ls` is a registered alias, and the default application comes from the
// directory when no name is typed (ADR-070 §1/§4).
func TestPreviewListAliasAndDirDefault(t *testing.T) {
	var method, path, body string
	srv := previewServer(t, &method, &path, &body)
	setupContext(t, srv.URL)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dirConfigName), []byte("application: varuna\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(previewCmd(), "ls") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "pv-42") {
		t.Fatalf("stdout = %q", out)
	}
}

// `-o json` hands back the API objects, every field of them — the fork flags
// and the head SHA have no column and must survive anyway.
func TestPreviewListJSON(t *testing.T) {
	var method, path, body string
	srv := previewServer(t, &method, &path, &body)
	setupContext(t, srv.URL)
	flags.output = "json"

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(previewCmd(), "list", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var previews []previewRecord
	if err := json.Unmarshal([]byte(out), &previews); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	if len(previews) != 2 {
		t.Fatalf("previews = %+v", previews)
	}
	// Unaltered means unsorted too: the payload's own order is preserved.
	if previews[0].Uuid != "pv-7" {
		t.Fatalf("previews[0] = %+v — the JSON output keeps the API's order", previews[0])
	}
	if previews[0].HeadSha != "bbb" || !previews[0].IsFork || previews[1].Fqdn != "pr-42.example.com" {
		t.Fatalf("previews = %+v — every schema field is carried through", previews)
	}
}

// `--pr N` is resolved against the application's previews before the act: the
// endpoints take a preview UUID, and nobody knows theirs by heart.
func TestPreviewRedeployResolvesThePR(t *testing.T) {
	var method, path, body string
	srv := previewServer(t, &method, &path, &body)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(previewCmd(), "redeploy", "--pr", "42", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if method != http.MethodPost || path != "/api/v1/applications/app-1/previews/pv-42/redeploy" {
		t.Fatalf("%s %s", method, path)
	}
	if body != "{}" {
		t.Fatalf("body = %q — a plain redeploy asks for neither a rebuild nor a skipped build", body)
	}
	if !strings.Contains(out, "dp-1") {
		t.Fatalf("stdout = %q — the queued deployment is what the caller follows next", out)
	}
}

// `--skip-build` is ADR-048's apply-without-rebuild, and it must reach the wire
// as the flag the API reads.
func TestPreviewRedeploySkipBuild(t *testing.T) {
	var method, path, body string
	srv := previewServer(t, &method, &path, &body)
	setupContext(t, srv.URL)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(previewCmd(), "redeploy", "--pr", "42", "--skip-build", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(body, `"skip_build":true`) {
		t.Fatalf("body = %q", body)
	}
}

// The two build flags contradict each other; the API answers 422 and the caller
// deserves exit 2 instead.
func TestPreviewRedeployRefusesBothBuildFlags(t *testing.T) {
	var method, path, body string
	srv := previewServer(t, &method, &path, &body)
	setupContext(t, srv.URL)

	var err error
	_, _ = captureOutput(t, func() {
		err = runCmd(previewCmd(), "redeploy", "--pr", "42", "--skip-build", "--force-rebuild", "varuna")
	})
	if err == nil || !IsUsageError(err) || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v", err)
	}
	if path != "" {
		t.Fatalf("path = %q — the contradiction is caught before any request", path)
	}
}

// `keep` re-arms the TTL with no body at all: the endpoint takes neither a
// duration nor a toggle (§20.4.3).
func TestPreviewKeep(t *testing.T) {
	var method, path, body string
	srv := previewServer(t, &method, &path, &body)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(previewCmd(), "keep", "--pr", "42", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if method != http.MethodPost || path != "/api/v1/applications/app-1/previews/pv-42/keep" {
		t.Fatalf("%s %s", method, path)
	}
	if body != "" {
		t.Fatalf("body = %q — /keep takes no request body", body)
	}
	if !strings.Contains(out, "#42") {
		t.Fatalf("stdout = %q", out)
	}
}

// Without --pr there is nothing to act on and no directory default to invent:
// that is a caller error (exit 2), not a 404 from the platform.
func TestPreviewVerbsRequirePR(t *testing.T) {
	for _, verb := range []string{"redeploy", "keep"} {
		var method, path, body string
		srv := previewServer(t, &method, &path, &body)
		setupContext(t, srv.URL)

		var err error
		_, _ = captureOutput(t, func() { err = runCmd(previewCmd(), verb, "varuna") })
		if err == nil || !IsUsageError(err) {
			t.Fatalf("%s without --pr: err = %v, want a usage error", verb, err)
		}
		if !strings.Contains(err.Error(), "--pr N") {
			t.Fatalf("%s: err = %v — the message names the flag that was missing", verb, err)
		}
		if path != "" {
			t.Fatalf("%s reached %q — nothing is posted before the target is known", verb, path)
		}
	}
}

// An unknown PR is refused by the shared resolver, not by a 404 on a made-up
// preview UUID.
func TestPreviewRedeployUnknownPR(t *testing.T) {
	var method, path, body string
	srv := previewServer(t, &method, &path, &body)
	setupContext(t, srv.URL)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(previewCmd(), "redeploy", "--pr", "99", "varuna") })
	if err == nil || !strings.Contains(err.Error(), "no preview for PR #99") {
		t.Fatalf("err = %v", err)
	}
	if path != "" {
		t.Fatalf("path = %q — nothing is posted for a preview that does not exist", path)
	}
}

// ADR-070 §2 removes `approve` on purpose: authorizing a fork's preview to run
// is project governance and stays in the dashboard. Its absence is a decision,
// so it is asserted — including that no flag of the surviving verbs reaches
// that endpoint by another name.
func TestPreviewHasNoApproveVerb(t *testing.T) {
	verbs := map[string]bool{}
	for _, sub := range previewCmd().Commands() {
		verbs[sub.Name()] = true
		for _, alias := range sub.Aliases {
			verbs[alias] = true
		}
		if strings.Contains(sub.Flags().FlagUsages(), "approve") {
			t.Fatalf("`preview %s` carries an approve flag — approving a fork is not this CLI's act", sub.Name())
		}
	}
	if verbs["approve"] {
		t.Fatal("`app preview approve` exists — ADR-070 §2 removed it on purpose")
	}
	for _, want := range []string{"list", "ls", "redeploy", "keep"} {
		if !verbs[want] {
			t.Fatalf("`app preview %s` is missing from the group", want)
		}
	}

	// And typing it gets the refusal Cobra gives any unknown verb, exit 2.
	setupHome(t)
	err := runCmd(previewCmd(), "approve", "--pr", "42")
	if err == nil || !IsUsageError(err) {
		t.Fatalf("err = %v, want a usage error for an unknown verb", err)
	}
}
