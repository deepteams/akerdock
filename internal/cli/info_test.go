package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// infoServer serves one application, one database and one compose stack in
// full, and records every path the CLI asked for — the negative assertions of
// this file are about paths that must NOT be requested.
func infoServer(t *testing.T, called *[]string) *httptest.Server {
	t.Helper()
	body := map[string]string{
		"/api/v1/applications": `{"data":[{"uuid":"app-1","name":"varuna"}]}`,
		"/api/v1/databases":    `{"data":[{"uuid":"db-1","name":"pg"}]}`,
		"/api/v1/services":     `{"data":[{"uuid":"svc-1","name":"monitoring"}]}`,
		"/api/v1/applications/app-1": `{"uuid":"app-1","name":"varuna","source_type":"git","build_pack":"compose",
			"desired_status":"running","observed_status":"unhealthy","observed_at":"2026-08-09T09:30:00Z",
			"domains":["https://varuna.ad.kedric.fr"],"scale_asleep":false,
			"health_check":{"enabled":true,"path":"/healthz","port":3000,"method":"GET","interval_seconds":30},
			"last_deployment_uuid":"dep-1","last_deployment_at":"2026-08-09T09:00:00Z","version":7}`,
		"/api/v1/applications/app-1/components": `{"data":[
			{"uuid":"c-1","name":"web","observed_status":"unhealthy","created_at":"2026-08-09T09:00:00Z"},
			{"uuid":"c-2","name":"migrate","observed_status":"exited","exclude_from_hc":true,"created_at":"2026-08-09T09:00:00Z"}
		]}`,
		"/api/v1/deployments/dep-1": `{"uuid":"dep-1","application_uuid":"app-1","status":"succeeded","trigger":"manual",
			"branch":"main","commit_sha":"3f9a1c2ddddddddd","created_at":"2026-08-09T09:00:00Z"}`,
		"/api/v1/databases/db-1": `{"uuid":"db-1","name":"pg","engine":"postgresql","desired_status":"running",
			"observed_status":"healthy","is_redacted":true,"is_public":true,"public_port":5433,
			"restart_required":true,"version":3,"created_at":"2026-08-09T09:00:00Z"}`,
		"/api/v1/services/svc-1": `{"uuid":"svc-1","name":"monitoring","desired_status":"running",
			"observed_status":"healthy","compose_content":"services: {}","version":2,"created_at":"2026-08-09T09:00:00Z"}`,
		"/api/v1/services/svc-1/components": `{"data":[
			{"uuid":"c-9","name":"grafana","observed_status":"healthy","created_at":"2026-08-09T09:00:00Z"}
		]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = append(*called, r.URL.Path)
		payload, ok := body[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"no such path ` + r.URL.Path + `"}`))
			return
		}
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The application view renders every field it fetched, and each of the three
// documents it gathers is fetched exactly once.
func TestInfoApplication(t *testing.T) {
	var called []string
	srv := infoServer(t, &called)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(infoCmd(kindApp), "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{
		"varuna", "app-1", "git · compose",
		"running", "unhealthy", // desired against observed: the gap is the point
		"GET :3000/healthz every 30s",
		"https://varuna.ad.kedric.fr",
		"succeeded", "manual", "main@3f9a1c2", "dep-1",
		"COMPONENTS", "web", "migrate", "one-shot job",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout misses %q:\n%s", want, out)
		}
	}
	for _, want := range []string{"/api/v1/applications/app-1", "/api/v1/applications/app-1/components", "/api/v1/deployments/dep-1"} {
		if !slices.Contains(called, want) {
			t.Fatalf("called = %v, want it to include %q", called, want)
		}
	}
}

// A database has no components endpoint (it is one container) and no
// deployment. Neither must be requested: a 404 invented for symmetry would fail
// the whole view.
func TestInfoDatabaseCallsNoComponents(t *testing.T) {
	var called []string
	srv := infoServer(t, &called)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(infoCmd(kindDB), "pg") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"pg", "postgresql", "healthy", "port 5433", "db restart pg"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout misses %q:\n%s", want, out)
		}
	}
	// The connection URL carries the password (INV-003) and never appears here.
	if strings.Contains(out, "postgres://") || strings.Contains(out, "HEALTH CHECK") {
		t.Fatalf("stdout = %q", out)
	}
	for _, forbidden := range []string{"/api/v1/databases/db-1/components", "/api/v1/deployments/"} {
		for _, path := range called {
			if strings.HasPrefix(path, forbidden) {
				t.Fatalf("called %q — a database has no such endpoint", path)
			}
		}
	}
}

// A compose stack has components and a deployment history but no domain field:
// its routing lives in the compose file, so no URL row is invented for it.
func TestInfoService(t *testing.T) {
	var called []string
	srv := infoServer(t, &called)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(infoCmd(kindSvc), "monitoring") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "grafana") || !strings.Contains(out, "COMPONENTS") {
		t.Fatalf("stdout = %q", out)
	}
	if strings.Contains(out, "URL") || strings.Contains(out, "LAST DEPLOY") {
		t.Fatalf("stdout = %q — neither field exists on this stack, and a placeholder is not an answer", out)
	}
	if !slices.Contains(called, "/api/v1/services/svc-1/components") {
		t.Fatalf("called = %v", called)
	}
}

// The name resolves from the .akerdock default, for applications only.
func TestInfoUsesTheDirectoryDefault(t *testing.T) {
	var called []string
	srv := infoServer(t, &called)
	setupContext(t, srv.URL)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, ".akerdock"), []byte("application: varuna\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var runErr error
	out, _ := captureOutput(t, func() { runErr = runCmd(infoCmd(kindApp)) })
	if runErr != nil {
		t.Fatalf("err = %v", runErr)
	}
	if !strings.Contains(out, "app-1") {
		t.Fatalf("stdout = %q", out)
	}
}

// -o json passes the API objects through: the envelope names them, nothing
// rewrites them.
func TestInfoJSONPassesTheObjectsThrough(t *testing.T) {
	var called []string
	srv := infoServer(t, &called)
	setupContext(t, srv.URL)
	flags.output = "json"

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(infoCmd(kindApp), "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var doc struct {
		Resource       map[string]any   `json:"resource"`
		Components     []map[string]any `json:"components"`
		LastDeployment map[string]any   `json:"last_deployment"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	// `version` has no row in the human view; a script must still find it.
	if doc.Resource["version"] != float64(7) || doc.Resource["uuid"] != "app-1" {
		t.Fatalf("resource = %v", doc.Resource)
	}
	if len(doc.Components) != 2 || doc.Components[0]["uuid"] != "c-1" {
		t.Fatalf("components = %v", doc.Components)
	}
	if doc.LastDeployment["status"] != "succeeded" {
		t.Fatalf("last_deployment = %v", doc.LastDeployment)
	}
}

// The deployment detail sits behind `deployments:read`, which the caller of
// `info` need not have (ADR-059's reviewer does not). Losing it must not lose
// the statuses the command was run for — the resource's own timestamp stands in.
func TestInfoSurvivesAForbiddenDeployment(t *testing.T) {
	// This server refuses everything but the application and its components,
	// which is what a token holding applications:read and nothing else meets.
	srvWithGap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications":
			_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
		case "/api/v1/applications/app-1":
			_, _ = w.Write([]byte(`{"uuid":"app-1","name":"varuna","desired_status":"running","observed_status":"healthy",
				"last_deployment_uuid":"dep-9","last_deployment_at":"2026-08-09T09:00:00Z"}`))
		case "/api/v1/applications/app-1/components":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"forbidden","message":"needs deployments:read"}`))
		}
	}))
	defer srvWithGap.Close()
	setupContext(t, srvWithGap.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(infoCmd(kindApp), "varuna") })
	if err != nil {
		t.Fatalf("err = %v — the enrichment is optional, the view is not", err)
	}
	if !strings.Contains(out, "healthy") || !strings.Contains(out, "dep-9") {
		t.Fatalf("stdout = %q", out)
	}
	if strings.Contains(out, "COMPONENTS") {
		t.Fatalf("stdout = %q — an application off the compose build pack answers an empty list, which is not a section", out)
	}
}

// A scale-to-zero application observes as `exited` exactly like a broken one.
// The word that separates them is the whole value of the row.
func TestInfoNamesAnAsleepApplication(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications":
			_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
		case "/api/v1/applications/app-1":
			_, _ = w.Write([]byte(`{"uuid":"app-1","name":"varuna","desired_status":"running",
				"observed_status":"exited","scale_asleep":true,"health_check":{"enabled":false}}`))
		default:
			_, _ = w.Write([]byte(`{"data":[]}`))
		}
	}))
	defer srv.Close()
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(infoCmd(kindApp), "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "asleep") || !strings.Contains(out, "disabled") {
		t.Fatalf("stdout = %q", out)
	}
}

// An unknown name fails with the resolver's message, before any detail is
// fetched.
func TestInfoUnknownName(t *testing.T) {
	var called []string
	srv := infoServer(t, &called)
	setupContext(t, srv.URL)

	err := runCmd(infoCmd(kindDB), "ghost")
	if err == nil || !strings.Contains(err.Error(), `no databases named "ghost"`) {
		t.Fatalf("err = %v", err)
	}
	if slices.Contains(called, "/api/v1/databases/db-1") {
		t.Fatalf("called = %v — nothing is fetched before the target resolves", called)
	}
}

// The REF spelling the tree replaced is refused by name, not looked up.
func TestInfoRefusesTheOldRef(t *testing.T) {
	var called []string
	srv := infoServer(t, &called)
	setupContext(t, srv.URL)

	err := runCmd(infoCmd(kindApp), "app/varuna")
	if err == nil || !strings.Contains(err.Error(), "akerdock app <verb> varuna") {
		t.Fatalf("err = %v", err)
	}
}
