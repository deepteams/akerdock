package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// envCall is one request the CLI made, kept whole: the assertions below are
// about WHICH endpoint was hit with WHAT body, which is the only part of this
// command a unit test can hold the platform to.
type envCall struct{ method, path, query, body string }

type envLog struct {
	mu    sync.Mutex
	calls []envCall
}

func (l *envLog) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, envCall{r.Method, r.URL.Path, r.URL.RawQuery, string(body)})
}

func (l *envLog) all() []envCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]envCall(nil), l.calls...)
}

// writes returns everything that was not a read: a GET is how this CLI looks
// things up, and no test here cares how many times it did.
func (l *envLog) writes() []envCall {
	var out []envCall
	for _, c := range l.all() {
		if c.method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

func (l *envLog) only(t *testing.T, method, path string) envCall {
	t.Helper()
	var hits []envCall
	for _, c := range l.all() {
		if c.method == method && c.path == path {
			hits = append(hits, c)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("%s %s hit %d times, want once — calls: %+v", method, path, len(hits), l.all())
	}
	return hits[0]
}

func (c envCall) decoded(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(c.body), &m); err != nil {
		t.Fatalf("body %q is not JSON: %v", c.body, err)
	}
	return m
}

// The production set of the fixture application: one plain value, one the
// server masked (INV-003), one locked, one multi-line.
const envFixture = `[
	{"uuid":"v-log","key":"LOG_LEVEL","value":"info","is_redacted":false,"is_build_time":false,"is_secret":false,"is_literal":false,"is_multiline":false,"is_locked":false,"created_at":"2026-08-09T10:00:00Z"},
	{"uuid":"v-dsn","key":"DATABASE_URL","value":null,"is_redacted":true,"is_build_time":false,"is_secret":false,"is_literal":false,"is_multiline":false,"is_locked":false,"created_at":"2026-08-09T10:00:00Z"},
	{"uuid":"v-key","key":"SIGNING_KEY","value":null,"is_redacted":true,"is_build_time":false,"is_secret":true,"is_literal":false,"is_multiline":false,"is_locked":true,"created_at":"2026-08-09T10:00:00Z"},
	{"uuid":"v-pem","key":"TLS_CERT","value":"-----BEGIN-----\nMIIB\n-----END-----","is_redacted":false,"is_build_time":true,"is_secret":false,"is_literal":false,"is_multiline":true,"is_locked":false,"created_at":"2026-08-09T10:00:00Z"}
]`

// The effective set of PR #7: one variable inherited from the shared preview
// set, one override belonging to this preview only (§20.4).
const envPreviewFixture = `[
	{"uuid":"v-shared","key":"LOG_LEVEL","value":"debug","is_redacted":false,"is_preview_override":false,"is_build_time":false,"is_literal":false,"is_multiline":false,"is_locked":false,"created_at":"2026-08-09T10:00:00Z"},
	{"uuid":"v-over","key":"FEATURE_X","value":"on","is_redacted":false,"is_preview_override":true,"is_build_time":false,"is_literal":false,"is_multiline":false,"is_locked":false,"created_at":"2026-08-09T10:00:00Z"}
]`

// envServer stands in for the platform: the application and stack listings the
// name resolves against, the two variable collections, the item paths, the PR
// preview and the two deployment triggers.
func envServer(t *testing.T, vars, previewVars string) (*httptest.Server, *envLog) {
	t.Helper()
	log := &envLog{}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		write(w, http.StatusOK, `{"data":[{"uuid":"app-1","name":"varuna"}]}`)
	})
	mux.HandleFunc("/api/v1/services", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		write(w, http.StatusOK, `{"data":[{"uuid":"svc-1","name":"tooling"}]}`)
	})
	collection := func(set string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			if r.Method == http.MethodPost {
				write(w, http.StatusCreated, `{"uuid":"v-new","key":"NEW","value":null,"is_redacted":true}`)
				return
			}
			write(w, http.StatusOK, `{"data":`+set+`}`)
		}
	}
	mux.HandleFunc("/api/v1/applications/app-1/envs", collection(vars))
	mux.HandleFunc("/api/v1/services/svc-1/envs", collection(vars))
	mux.HandleFunc("/api/v1/applications/app-1/previews/pv-1/envs", collection(previewVars))
	item := func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		write(w, http.StatusOK, `{"uuid":"v-log","key":"LOG_LEVEL","value":null,"is_redacted":true}`)
	}
	mux.HandleFunc("/api/v1/applications/app-1/envs/", item)
	mux.HandleFunc("/api/v1/services/svc-1/envs/", item)
	mux.HandleFunc("/api/v1/applications/app-1/previews", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		write(w, http.StatusOK, `{"data":[{"uuid":"pv-1","pr_id":7,"status":"running"}]}`)
	})
	deploy := func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		write(w, http.StatusAccepted, `{"deployment_uuid":"dp-9","status_url":"/deployments/dp-9"}`)
	}
	mux.HandleFunc("/api/v1/applications/app-1/deploy", deploy)
	mux.HandleFunc("/api/v1/services/svc-1/deploy", deploy)
	mux.HandleFunc("/api/v1/applications/app-1/previews/pv-1/redeploy", deploy)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, log
}

// The listing shows what the server gave and nothing else: a masked value is
// named as masked rather than blanked, and a multi-line one does not tear the
// table apart.
func TestEnvListRendersTheServersAnswer(t *testing.T) {
	srv, _ := envServer(t, envFixture, envPreviewFixture)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(envCmd(kindApp), "list", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"KEY", "VALUE", "FLAGS", "LOG_LEVEL", "info", "<redacted>", "locked,secret", "build"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(out, "BEGIN-----\nMIIB") {
		t.Fatalf("stdout = %q — a multi-line value must not break the row it is printed in", out)
	}
	if !strings.Contains(out, `-----BEGIN-----\nMIIB`) {
		t.Fatalf("stdout = %q — the multi-line value is shown with its breaks escaped", out)
	}
}

// `-o json` emits the API objects unaltered — fields this CLI never decodes
// included, so a script is coupled to the contract and not to our struct.
func TestEnvListJSONIsTheAPIObject(t *testing.T) {
	srv, _ := envServer(t, envFixture, envPreviewFixture)
	setupContext(t, srv.URL)
	flags.output = "json"

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(envCmd(kindApp), "ls", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	if len(got) != 4 || got[0]["key"] != "LOG_LEVEL" {
		t.Fatalf("got = %+v", got)
	}
	if got[0]["created_at"] != "2026-08-09T10:00:00Z" {
		t.Fatalf("got[0] = %+v — a field the CLI does not read must survive to the output", got[0])
	}
	if got[1]["value"] != nil {
		t.Fatalf("got[1] = %+v — the masked value stays null", got[1])
	}
}

// `get` prints the value alone so it can be captured; a value the server
// withheld ends the command instead of printing an empty line a script would
// take for a password.
func TestEnvGet(t *testing.T) {
	t.Run("prints the value alone", func(t *testing.T) {
		srv, _ := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		out, _ := captureOutput(t, func() { err = runCmd(envCmd(kindApp), "get", "LOG_LEVEL", "varuna") })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if out != "info\n" {
			t.Fatalf("stdout = %q, want exactly the value", out)
		}
	})

	t.Run("a missing key fails and names the key", func(t *testing.T) {
		srv, _ := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		out, _ := captureOutput(t, func() { err = runCmd(envCmd(kindApp), "get", "NOPE", "varuna") })
		if err == nil || !strings.Contains(err.Error(), `no variable named "NOPE"`) {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(err.Error(), "varuna") {
			t.Fatalf("err = %v — the message names the resource it looked in", err)
		}
		if out != "" {
			t.Fatalf("stdout = %q — nothing is printed when there is no value", out)
		}
	})

	t.Run("a masked value fails rather than print nothing", func(t *testing.T) {
		srv, _ := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		out, _ := captureOutput(t, func() { err = runCmd(envCmd(kindApp), "get", "DATABASE_URL", "varuna") })
		if err == nil || !strings.Contains(err.Error(), "read:sensitive") {
			t.Fatalf("err = %v", err)
		}
		if out != "" {
			t.Fatalf("stdout = %q", out)
		}
	})

	t.Run("a locked value says it is write-only, not that permission is missing", func(t *testing.T) {
		srv, _ := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() { err = runCmd(envCmd(kindApp), "get", "SIGNING_KEY", "varuna") })
		if err == nil || !strings.Contains(err.Error(), "locked") {
			t.Fatalf("err = %v", err)
		}
	})
}

// An existing key is patched on its own item path, a new one is posted to the
// collection — and neither echoes the value back to a terminal that keeps
// scrollback (ADR-070 §Verification).
func TestEnvSetCreatesAndUpdates(t *testing.T) {
	srv, log := envServer(t, envFixture, envPreviewFixture)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() {
		err = runCmd(envCmd(kindApp), "set", "LOG_LEVEL=debug", "SENTRY_DSN=https://x", "varuna")
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	patch := log.only(t, http.MethodPatch, "/api/v1/applications/app-1/envs/v-log")
	if body := patch.decoded(t); body["value"] != "debug" || body["key"] != nil {
		t.Fatalf("PATCH body = %v — the value is sent, the key is not modifiable", body)
	}
	post := log.only(t, http.MethodPost, "/api/v1/applications/app-1/envs")
	if body := post.decoded(t); body["key"] != "SENTRY_DSN" || body["value"] != "https://x" {
		t.Fatalf("POST body = %v", body)
	}
	if strings.Contains(out, "debug") || strings.Contains(out, "https://x") {
		t.Fatalf("stdout = %q — a value written is never echoed back", out)
	}
	if !strings.Contains(out, "LOG_LEVEL set") || !strings.Contains(out, "SENTRY_DSN set") {
		t.Fatalf("stdout = %q", out)
	}
}

// --secret decides HOW a build-time value reaches the build (§5.2). It is sent
// only when it was typed: a plain `set` on an existing build secret must not
// quietly demote it to a build ARG.
func TestEnvSetSecretFlag(t *testing.T) {
	srv, log := envServer(t, envFixture, envPreviewFixture)
	setupContext(t, srv.URL)
	var err error
	_, _ = captureOutput(t, func() {
		err = runCmd(envCmd(kindApp), "set", "NPM_TOKEN=abc", "varuna", "--secret")
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if body := log.only(t, http.MethodPost, "/api/v1/applications/app-1/envs").decoded(t); body["is_secret"] != true {
		t.Fatalf("POST body = %v", body)
	}

	srv2, log2 := envServer(t, envFixture, envPreviewFixture)
	setupContext(t, srv2.URL)
	_, _ = captureOutput(t, func() { err = runCmd(envCmd(kindApp), "set", "LOG_LEVEL=debug", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if body := log2.only(t, http.MethodPatch, "/api/v1/applications/app-1/envs/v-log").decoded(t); body["is_secret"] != nil {
		t.Fatalf("PATCH body = %v — is_secret is absent when --secret was not typed", body)
	}
}

// --apply is ADR-048's redeployment: the pipeline reruns over the artifact
// already running, so the values reach the process without a build. Without it,
// nothing is deployed at all — and the developer is told so.
func TestEnvSetApplyTriggersTheSkipBuildDeployment(t *testing.T) {
	srv, log := envServer(t, envFixture, envPreviewFixture)
	setupContext(t, srv.URL)

	var err error
	out, errOut := captureOutput(t, func() {
		err = runCmd(envCmd(kindApp), "set", "LOG_LEVEL=debug", "varuna", "--apply")
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	body := log.only(t, http.MethodPost, "/api/v1/applications/app-1/deploy").decoded(t)
	if body["skip_build"] != true {
		t.Fatalf("deploy body = %v — --apply must not rebuild (ADR-048)", body)
	}
	if body["force_rebuild"] != nil {
		t.Fatalf("deploy body = %v — the two are mutually exclusive, so only one is sent", body)
	}
	if !strings.Contains(out, "dp-9") {
		t.Fatalf("stdout = %q — the queued deployment is named so it can be followed", out)
	}
	if strings.Contains(errOut, "not applied") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestEnvSetWithoutApplyDeploysNothing(t *testing.T) {
	srv, log := envServer(t, envFixture, envPreviewFixture)
	setupContext(t, srv.URL)

	var err error
	_, errOut := captureOutput(t, func() {
		err = runCmd(envCmd(kindApp), "set", "LOG_LEVEL=debug", "varuna")
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, c := range log.all() {
		if strings.HasSuffix(c.path, "/deploy") || strings.HasSuffix(c.path, "/redeploy") {
			t.Fatalf("called %s %s — a plain `set` deploys nothing", c.method, c.path)
		}
	}
	if !strings.Contains(errOut, "--apply") {
		t.Fatalf("stderr = %q — the developer is told the change is not live yet", errOut)
	}
}

// A pair that is not a pair costs no request: half-written sets are how a
// deployment gets a variable and misses the next one.
func TestEnvSetRefusesAMalformedPairBeforeAnyRequest(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{args: []string{"set", "LOG_LEVEL="}, want: "no value"},
		{args: []string{"set", "=debug"}, want: "missing key"},
		{args: []string{"set", "LOG_LEVEL", "SENTRY_DSN=x"}, want: "expected KEY=VALUE"},
		{args: []string{"set", "LOG-LEVEL=debug"}, want: "invalid variable name"},
		{args: []string{"set", "varuna"}, want: "no KEY=VALUE given"},
	}
	for _, tc := range cases {
		srv, log := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() { err = runCmd(envCmd(kindApp), tc.args...) })
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: err = %v, want it to mention %q", tc.args, err, tc.want)
		}
		if !IsUsageError(err) {
			t.Fatalf("%v: err is not a usage error — the spec promises exit 2 for what the caller typed", tc.args)
		}
		if calls := log.all(); len(calls) != 0 {
			t.Fatalf("%v: the CLI made %d request(s) before refusing: %+v", tc.args, len(calls), calls)
		}
	}
}

// `unset` resolves each key to its uuid and deletes on the item path — and if
// one key is unknown, nothing is deleted at all: a typo in a list of five must
// not leave four of them gone.
func TestEnvUnset(t *testing.T) {
	t.Run("resolves the keys then deletes them", func(t *testing.T) {
		srv, log := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		out, _ := captureOutput(t, func() {
			err = runCmd(envCmd(kindApp), "unset", "LOG_LEVEL", "TLS_CERT", "varuna")
		})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		log.only(t, http.MethodDelete, "/api/v1/applications/app-1/envs/v-log")
		log.only(t, http.MethodDelete, "/api/v1/applications/app-1/envs/v-pem")
		if !strings.Contains(out, "LOG_LEVEL unset") {
			t.Fatalf("stdout = %q", out)
		}
	})

	t.Run("one unknown key deletes nothing", func(t *testing.T) {
		srv, log := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() {
			err = runCmd(envCmd(kindApp), "unset", "LOG_LEVEL", "TYPO", "varuna")
		})
		if err == nil || !strings.Contains(err.Error(), `"TYPO"`) || !strings.Contains(err.Error(), "nothing was deleted") {
			t.Fatalf("err = %v", err)
		}
		if writes := log.writes(); len(writes) != 0 {
			t.Fatalf("writes = %+v — the whole call is refused before the first delete", writes)
		}
	})

	// KEY and NAME are both bare words: the split is settled against the team's
	// real resources, not against the shape of the argument.
	t.Run("a trailing argument that names no resource is a key", func(t *testing.T) {
		srv, log := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		t.Setenv(envApplication, "varuna") // the default the positional would override
		var err error
		_, _ = captureOutput(t, func() {
			err = runCmd(envCmd(kindApp), "unset", "LOG_LEVEL", "TLS_CERT")
		})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		log.only(t, http.MethodDelete, "/api/v1/applications/app-1/envs/v-log")
		log.only(t, http.MethodDelete, "/api/v1/applications/app-1/envs/v-pem")
	})

	t.Run("the REF spelling is refused by name", func(t *testing.T) {
		srv, _ := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() {
			err = runCmd(envCmd(kindApp), "unset", "LOG_LEVEL", "app/varuna")
		})
		if err == nil || !strings.Contains(err.Error(), "akerdock app <verb> varuna") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("--apply redeploys after deleting", func(t *testing.T) {
		srv, log := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() {
			err = runCmd(envCmd(kindApp), "unset", "LOG_LEVEL", "varuna", "--apply")
		})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if body := log.only(t, http.MethodPost, "/api/v1/applications/app-1/deploy").decoded(t); body["skip_build"] != true {
			t.Fatalf("deploy body = %v", body)
		}
	})
}

// --pr reads the preview's EFFECTIVE set (INV-010): the shared preview
// variables merged with this PR's own overrides, and never a production value.
func TestEnvPreviewScope(t *testing.T) {
	t.Run("list reads the preview collection", func(t *testing.T) {
		srv, log := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		out, _ := captureOutput(t, func() {
			err = runCmd(envCmd(kindApp), "list", "varuna", "--pr", "7")
		})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(out, "FEATURE_X") || !strings.Contains(out, "override") {
			t.Fatalf("stdout = %q — a PR's own overrides are marked as such", out)
		}
		var seen bool
		for _, c := range log.all() {
			if c.path == "/api/v1/applications/app-1/previews/pv-1/envs" {
				seen = true
			}
			if c.path == "/api/v1/applications/app-1/envs" {
				t.Fatalf("the production set was read under --pr — INV-010 says it never appears here")
			}
		}
		if !seen {
			t.Fatalf("calls = %+v", log.all())
		}
	})

	t.Run("set copies an inherited key into an override, patches an existing one", func(t *testing.T) {
		srv, log := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() {
			err = runCmd(envCmd(kindApp), "set", "LOG_LEVEL=trace", "FEATURE_X=off", "varuna", "--pr", "7")
		})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		// LOG_LEVEL is inherited from the shared set: posting an override is
		// the only write that does not travel to the other previews.
		post := log.only(t, http.MethodPost, "/api/v1/applications/app-1/previews/pv-1/envs")
		if body := post.decoded(t); body["key"] != "LOG_LEVEL" || body["value"] != "trace" {
			t.Fatalf("POST body = %v", body)
		}
		// FEATURE_X already belongs to this preview: it is patched in place.
		log.only(t, http.MethodPatch, "/api/v1/applications/app-1/envs/v-over")
	})

	t.Run("unset refuses a key the preview only inherits", func(t *testing.T) {
		srv, log := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() {
			err = runCmd(envCmd(kindApp), "unset", "LOG_LEVEL", "varuna", "--pr", "7")
		})
		if err == nil || !strings.Contains(err.Error(), "every preview") {
			t.Fatalf("err = %v", err)
		}
		if writes := log.writes(); len(writes) != 0 {
			t.Fatalf("writes = %+v", writes)
		}
	})

	t.Run("--apply redeploys the preview at its pinned SHA", func(t *testing.T) {
		srv, log := envServer(t, envFixture, envPreviewFixture)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() {
			err = runCmd(envCmd(kindApp), "set", "FEATURE_X=off", "varuna", "--pr", "7", "--apply")
		})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		body := log.only(t, http.MethodPost, "/api/v1/applications/app-1/previews/pv-1/redeploy").decoded(t)
		if body["skip_build"] != true {
			t.Fatalf("redeploy body = %v", body)
		}
	})
}

// The same group on a compose stack, minus what a stack does not have: no
// previews, so `--pr` is a flag that does not exist rather than one that fails
// at runtime (ADR-070 §1).
func TestEnvOnAComposeStack(t *testing.T) {
	srv, log := envServer(t, envFixture, envPreviewFixture)
	setupContext(t, srv.URL)

	var err error
	_, _ = captureOutput(t, func() {
		err = runCmd(envCmd(kindSvc), "set", "LOG_LEVEL=debug", "tooling", "--apply")
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	log.only(t, http.MethodPatch, "/api/v1/services/svc-1/envs/v-log")
	// A stack's deploy endpoint takes no body at all: sending skip_build would
	// be inventing a field the contract does not have.
	if deploy := log.only(t, http.MethodPost, "/api/v1/services/svc-1/deploy"); deploy.body != "" {
		t.Fatalf("deploy body = %q, want empty", deploy.body)
	}

	srv2, _ := envServer(t, envFixture, envPreviewFixture)
	setupContext(t, srv2.URL)
	_, _ = captureOutput(t, func() { err = runCmd(envCmd(kindSvc), "list", "tooling", "--pr", "7") })
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("err = %v — a compose stack has no previews, so --pr is not one of its flags", err)
	}
}
