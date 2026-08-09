package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// apiCall is one request the CLI made, kept whole: the body is as much of the
// contract as the path is, and every assertion in this file is about one or the
// other.
type apiCall struct {
	method string
	path   string
	query  string
	body   string
}

// deployServer answers a fixed route table and records every call. A path with
// no route answers 404 with its own name in the message, so a test that reaches
// for an endpoint it did not intend to call fails saying which.
func deployServer(t *testing.T, calls *[]apiCall, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*calls = append(*calls, apiCall{r.Method, r.URL.Path, r.URL.RawQuery, string(body)})
		resp, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no route for ` + r.URL.Path + `"}`))
			return
		}
		if sse, isSSE := strings.CutPrefix(resp, "sse:"); isSSE {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sse))
			return
		}
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// appRoutes is the resolution listing plus a trigger that accepts.
func appRoutes() map[string]string {
	return map[string]string{
		"/api/v1/applications":                `{"data":[{"uuid":"app-1","name":"varuna"}]}`,
		"/api/v1/applications/app-1/deploy":   `{"deployment_uuid":"dp-1","status_url":"/deployments/dp-1"}`,
		"/api/v1/applications/app-1/rollback": `{"deployment_uuid":"dp-2","status_url":"/deployments/dp-2"}`,
	}
}

func findCall(calls []apiCall, method, path string) (apiCall, bool) {
	for _, c := range calls {
		if c.method == method && c.path == path {
			return c, true
		}
	}
	return apiCall{}, false
}

// Each group posts to the collection of its own kind — the one thing a typed
// tree must not get wrong, since the name resolves against that collection too.
func TestDeployRunTriggersItsOwnKind(t *testing.T) {
	cases := []struct {
		kind         resourceKind
		name         string
		listPath     string
		deployPath   string
		listResponse string
	}{
		{kindApp, "varuna", "/api/v1/applications", "/api/v1/applications/app-1/deploy", `{"data":[{"uuid":"app-1","name":"varuna"}]}`},
		{kindSvc, "stack", "/api/v1/services", "/api/v1/services/svc-1/deploy", `{"data":[{"uuid":"svc-1","name":"stack"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.kind.group, func(t *testing.T) {
			var calls []apiCall
			srv := deployServer(t, &calls, map[string]string{
				tc.listPath:   tc.listResponse,
				tc.deployPath: `{"deployment_uuid":"dp-1","status_url":"/deployments/dp-1"}`,
			})
			setupContext(t, srv.URL)

			var err error
			out, _ := captureOutput(t, func() { err = runCmd(deployCmd(tc.kind), "run", tc.name) })
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			call, ok := findCall(calls, http.MethodPost, tc.deployPath)
			if !ok {
				t.Fatalf("no POST %s, calls = %+v", tc.deployPath, calls)
			}
			// No flag asked for anything, so no body: an empty JSON object and
			// no body mean the same deployment, and the spec makes it optional.
			if call.body != "" || call.query != "" {
				t.Fatalf("body = %q, query = %q, want neither", call.body, call.query)
			}
			if !strings.Contains(out, "dp-1") {
				t.Fatalf("stdout = %q — the uuid that tracks the deployment must be printed", out)
			}
		})
	}
}

// ADR-048's flag is the reason `env set --apply` has anything to call: it must
// travel, and it must not travel when nobody asked for it.
func TestDeployRunBuildFlags(t *testing.T) {
	t.Run("--skip-build sends skip_build", func(t *testing.T) {
		var calls []apiCall
		srv := deployServer(t, &calls, appRoutes())
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() { err = runCmd(deployCmd(kindApp), "run", "varuna", "--skip-build") })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		call, _ := findCall(calls, http.MethodPost, "/api/v1/applications/app-1/deploy")
		var body map[string]any
		if err := json.Unmarshal([]byte(call.body), &body); err != nil {
			t.Fatalf("body %q is not JSON: %v", call.body, err)
		}
		if body["skip_build"] != true {
			t.Fatalf("body = %v, want skip_build true", body)
		}
		if _, present := body["force_rebuild"]; present {
			t.Fatalf("body = %v — the flag nobody set must not be sent", body)
		}
	})

	t.Run("--force-rebuild sends force_rebuild", func(t *testing.T) {
		var calls []apiCall
		srv := deployServer(t, &calls, appRoutes())
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() { err = runCmd(deployCmd(kindApp), "run", "varuna", "--force-rebuild") })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		call, _ := findCall(calls, http.MethodPost, "/api/v1/applications/app-1/deploy")
		if !strings.Contains(call.body, `"force_rebuild":true`) || strings.Contains(call.body, "skip_build") {
			t.Fatalf("body = %q", call.body)
		}
	})

	// The spec states the two are mutually exclusive; the refusal is a usage
	// failure (exit 2) and nothing must be queued before it.
	t.Run("the two together are refused before anything is queued", func(t *testing.T) {
		var calls []apiCall
		srv := deployServer(t, &calls, appRoutes())
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() {
			err = runCmd(deployCmd(kindApp), "run", "varuna", "--skip-build", "--force-rebuild")
		})
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("err = %v", err)
		}
		if !IsUsageError(err) {
			t.Fatalf("err = %v — a contradiction in what was typed exits 2", err)
		}
		if _, ok := findCall(calls, http.MethodPost, "/api/v1/applications/app-1/deploy"); ok {
			t.Fatalf("a deployment was queued despite the refusal: %+v", calls)
		}
	})

	// POST /services/{uuid}/deploy declares no request body, so the flag does
	// not exist on that group rather than being sent and dropped (ADR-070 §1).
	t.Run("a compose stack has no --skip-build", func(t *testing.T) {
		var calls []apiCall
		srv := deployServer(t, &calls, map[string]string{
			"/api/v1/services": `{"data":[{"uuid":"svc-1","name":"stack"}]}`,
		})
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() { err = runCmd(deployCmd(kindSvc), "run", "stack", "--skip-build") })
		if err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("err = %v", err)
		}
	})
}

// -f follows the deployment the trigger JUST returned. "The latest" would be
// someone else's under a concurrent push, so the history endpoint must not even
// be consulted.
func TestDeployRunFollowsTheDeploymentItMinted(t *testing.T) {
	var calls []apiCall
	routes := appRoutes()
	routes["/api/v1/applications/app-1/deploy"] = `{"deployment_uuid":"dp-9","status_url":"/deployments/dp-9"}`
	routes["/api/v1/deployments/dp-9/logs"] = "sse:" +
		"data: {\"sequence\":1,\"channel\":\"stdout\",\"message\":\"step 1/3 cloning\"}\n\n" +
		"data: {\"sequence\":2,\"channel\":\"stdout\",\"message\":\"step 3/3 healthy\"}\n\n"
	srv := deployServer(t, &calls, routes)
	setupContext(t, srv.URL)

	var err error
	out, errOut := captureOutput(t, func() { err = runCmd(deployCmd(kindApp), "run", "varuna", "-f") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if _, ok := findCall(calls, http.MethodGet, "/api/v1/deployments/dp-9/logs"); !ok {
		t.Fatalf("the build log of dp-9 was not read, calls = %+v", calls)
	}
	if _, ok := findCall(calls, http.MethodGet, "/api/v1/applications/app-1/deployments"); ok {
		t.Fatalf("the history was consulted — -f follows the minted deployment, not the latest one")
	}
	if !strings.Contains(out, "step 1/3 cloning") || !strings.Contains(out, "step 3/3 healthy") {
		t.Fatalf("stdout = %q", out)
	}
	// The announcement leaves stdout to the log, so `-f > build.log` is clean.
	if !strings.Contains(errOut, "dp-9") || strings.Contains(out, "queued") {
		t.Fatalf("stdout = %q, stderr = %q", out, errOut)
	}
}

// historyServer paginates: two pages, the second reachable only by cursor.
func historyServer(t *testing.T, path string, queries *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
	})
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		*queries = append(*queries, r.URL.RawQuery)
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"data":[
				{"uuid":"dp-3","status":"succeeded","trigger":"manual","branch":"main","commit_sha":"abcdef1234567","started_at":"2026-08-09T10:00:00Z","finished_at":"2026-08-09T10:04:00Z","created_at":"2026-08-09T10:00:00Z","error_message":null,"attempt":1},
				{"uuid":"dp-2","status":"failed","trigger":"webhook","branch":"main","commit_sha":"0011223344556","created_at":"2026-08-09T09:00:00Z","error_message":"health check never passed","attempt":2}
			],"next_cursor":"c2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[
			{"uuid":"dp-1","status":"succeeded","trigger":"api","is_rollback":true,"image_digest":"sha256:deadbeefcafebabe","created_at":"2026-08-09T08:00:00Z"}
		],"next_cursor":null}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDeployListRendersAndPaginates(t *testing.T) {
	var queries []string
	srv := historyServer(t, "/api/v1/applications/app-1/deployments", &queries)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(deployCmd(kindApp), "ls", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("queries = %v — the cursor must be followed until the page runs out", queries)
	}
	if !strings.Contains(queries[0], "limit=20") || !strings.Contains(queries[1], "cursor=c2") {
		t.Fatalf("queries = %v", queries)
	}
	// What an operator scans for: the outcome, who asked, and from what.
	for _, want := range []string{"succeeded", "failed", "webhook", "main@abcdef1", "api (rollback)", "deadbeefcafe", "dp-1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want it to contain %q", out, want)
		}
	}
}

// The bound exists because the history is unbounded; the second page must ask
// only for what is still missing, and one page never exceeds the API's 100.
func TestDeployListLimit(t *testing.T) {
	t.Run("-n stops the walk and shortens the last page", func(t *testing.T) {
		var queries []string
		srv := historyServer(t, "/api/v1/applications/app-1/deployments", &queries)
		setupContext(t, srv.URL)
		var err error
		out, _ := captureOutput(t, func() { err = runCmd(deployCmd(kindApp), "list", "varuna", "-n", "2") })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(queries) != 1 {
			t.Fatalf("queries = %v — the first page already covered -n 2", queries)
		}
		if strings.Contains(out, "dp-1") {
			t.Fatalf("stdout = %q — only 2 rows were asked for", out)
		}
	})

	t.Run("a page is never asked for more than the API serves", func(t *testing.T) {
		var queries []string
		srv := historyServer(t, "/api/v1/applications/app-1/deployments", &queries)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() { err = runCmd(deployCmd(kindApp), "list", "varuna", "-n", "250") })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(queries[0], "limit=100") {
			t.Fatalf("queries = %v", queries)
		}
	})

	t.Run("-n 0 is a usage error", func(t *testing.T) {
		var queries []string
		srv := historyServer(t, "/api/v1/applications/app-1/deployments", &queries)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() { err = runCmd(deployCmd(kindApp), "list", "varuna", "-n", "0") })
		if !IsUsageError(err) {
			t.Fatalf("err = %v", err)
		}
	})
}

// -o json is what a script reads, so it carries the fields the table has no
// column for rather than the table's projection re-encoded.
func TestDeployListJSONIsUnaltered(t *testing.T) {
	var queries []string
	srv := historyServer(t, "/api/v1/applications/app-1/deployments", &queries)
	setupContext(t, srv.URL)
	flags.output = "json"

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(deployCmd(kindApp), "list", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want the three deployments of both pages", len(items))
	}
	if items[1]["error_message"] != "health check never passed" {
		t.Fatalf("items[1] = %v — a field with no column must survive -o json", items[1])
	}
	if items[1]["attempt"] != float64(2) {
		t.Fatalf("items[1] = %v", items[1])
	}
}

// The endpoint is transversal: a deployment uuid identifies its application on
// its own, so cancel takes no resource name.
func TestDeployCancel(t *testing.T) {
	var calls []apiCall
	srv := deployServer(t, &calls, map[string]string{
		"/api/v1/deployments/dp-7/cancel": `{"deployment_uuid":"dp-7","status_url":"/deployments/dp-7"}`,
	})
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(deployCmd(kindApp), "cancel", "dp-7") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if _, ok := findCall(calls, http.MethodPost, "/api/v1/deployments/dp-7/cancel"); !ok {
		t.Fatalf("calls = %+v", calls)
	}
	if !strings.Contains(out, "dp-7") {
		t.Fatalf("stdout = %q", out)
	}
	// No listing was walked: the uuid was enough.
	if len(calls) != 1 {
		t.Fatalf("calls = %+v — cancel resolves no resource", calls)
	}
}

func TestDeployCancelNeedsAUuid(t *testing.T) {
	setupHome(t)
	var err error
	_, _ = captureOutput(t, func() { err = runCmd(deployCmd(kindApp), "cancel") })
	if !IsUsageError(err) {
		t.Fatalf("err = %v — a missing argument is a usage failure (exit 2)", err)
	}
}

func TestDeployRollback(t *testing.T) {
	t.Run("no --to means the previous artifact, so no body", func(t *testing.T) {
		var calls []apiCall
		srv := deployServer(t, &calls, appRoutes())
		setupContext(t, srv.URL)
		var err error
		out, _ := captureOutput(t, func() { err = runCmd(deployCmd(kindApp), "rollback", "varuna") })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		call, ok := findCall(calls, http.MethodPost, "/api/v1/applications/app-1/rollback")
		if !ok {
			t.Fatalf("calls = %+v", calls)
		}
		if call.body != "" {
			t.Fatalf("body = %q, want none", call.body)
		}
		if !strings.Contains(out, "dp-2") {
			t.Fatalf("stdout = %q", out)
		}
	})

	t.Run("--to names the deployment whose image comes back", func(t *testing.T) {
		var calls []apiCall
		srv := deployServer(t, &calls, appRoutes())
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() {
			err = runCmd(deployCmd(kindApp), "rollback", "varuna", "--to", "dp-old")
		})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		call, _ := findCall(calls, http.MethodPost, "/api/v1/applications/app-1/rollback")
		if !strings.Contains(call.body, `"deployment_uuid":"dp-old"`) {
			t.Fatalf("body = %q", call.body)
		}
	})

	// A refused rollback is the platform's sentence, not a rebuild: the artifact
	// no longer exists and nothing can be invented in its place.
	t.Run("a pruned artifact is refused with the platform's words", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
		})
		mux.HandleFunc("/api/v1/applications/app-1/rollback", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"conflict","message":"the image of that deployment is no longer retained"}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() { err = runCmd(deployCmd(kindApp), "rollback", "varuna") })
		if err == nil || !strings.Contains(err.Error(), "no longer retained") {
			t.Fatalf("err = %v", err)
		}
	})
}

// The asymmetry is the API's and ADR-070 §1 made it a decision: only an
// application has a rollback endpoint, so only the app group shows the verb.
// Asserted rather than assumed, because a verb silently appearing on another
// type is exactly the failure the typed tree exists to prevent.
func TestDeployRollbackExistsOnApplicationsOnly(t *testing.T) {
	if !hasDeployVerb(deployCmd(kindApp), "rollback") {
		t.Fatal("the app group lost `deploy rollback`")
	}
	if hasDeployVerb(deployCmd(kindSvc), "rollback") {
		t.Fatal("`deploy rollback` appeared on the svc group — no endpoint serves it")
	}
	// The three verbs both groups do share.
	for _, k := range []resourceKind{kindApp, kindSvc} {
		for _, verb := range []string{"run", "list", "cancel"} {
			if !hasDeployVerb(deployCmd(k), verb) {
				t.Fatalf("%s deploy has no %q", k.group, verb)
			}
		}
	}
	// And the alias ADR-070 §4 keeps registered without ever showing it.
	if !hasDeployVerb(deployCmd(kindApp), "ls") {
		t.Fatal("`deploy ls` must stay reachable as an alias")
	}
}

// hasDeployVerb resolves a verb the way Cobra does at dispatch, so an alias
// counts exactly as the dispatcher counts it.
func hasDeployVerb(group *cobra.Command, verb string) bool {
	for _, sub := range group.Commands() {
		if sub.Name() == verb {
			return true
		}
		for _, a := range sub.Aliases {
			if a == verb {
				return true
			}
		}
	}
	return false
}

// The three columns that carry meaning rather than an identifier. is_rollback
// and skip_build are invisible in `trigger` by schema decision, and both change
// what the deployment did — so both must show.
func TestDeployRowRendering(t *testing.T) {
	pr := 42
	cases := []struct {
		d       deployment
		trigger string
		source  string
	}{
		{deployment{Trigger: "manual", Branch: "main", CommitSha: "abcdef1234567890"}, "manual", "main@abcdef1"},
		{deployment{Trigger: "preview", PrID: &pr, Branch: "feat/x", CommitSha: "0123456"}, "preview #42", "feat/x@0123456"},
		{deployment{Trigger: "api", IsRollback: true, ImageDigest: "sha256:deadbeefcafebabe0000"}, "api (rollback)", "deadbeefcafe"},
		{deployment{Trigger: "config_apply", SkipBuild: true}, "config_apply (no build)", "-"},
		{deployment{Trigger: "manual", ForceRebuild: true, CommitSha: "abc"}, "manual (no cache)", "abc"},
		{deployment{Trigger: "webhook", Branch: "release"}, "webhook", "release"},
	}
	for _, tc := range cases {
		if got := deployTrigger(tc.d); got != tc.trigger {
			t.Errorf("deployTrigger = %q, want %q", got, tc.trigger)
		}
		if got := deploySource(tc.d); got != tc.source {
			t.Errorf("deploySource = %q, want %q", got, tc.source)
		}
	}
}

// A queued deployment has not started and a running one has not finished: both
// are ordinary states of a history, and neither may render as a zero date.
func TestDeployTimeIsNullable(t *testing.T) {
	if got := deployTime(nil); got != "-" {
		t.Fatalf("deployTime(nil) = %q", got)
	}
	if got := deployTime(&time.Time{}); got != "-" {
		t.Fatalf("deployTime(zero) = %q", got)
	}
	at := time.Date(2026, 8, 9, 10, 4, 0, 0, time.UTC)
	if got, want := deployTime(&at), at.Local().Format("2006-01-02 15:04"); got != want {
		t.Fatalf("deployTime = %q, want %q", got, want)
	}
}
