package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tasksServer serves one application, its scheduled tasks, and the manual
// trigger — recording which task UUID the trigger reached. The task list is
// deliberately awkward: two tasks share the name "cleanup", which is the case
// `run` has to refuse rather than resolve by luck.
func tasksServer(t *testing.T, tasksJSON string, ranPath *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/applications", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
	})
	mux.HandleFunc("/api/v1/applications/app-1/scheduled-tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tasksJSON))
	})
	mux.HandleFunc("/api/v1/scheduled-tasks/", func(w http.ResponseWriter, r *http.Request) {
		*ranPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_uuid":"jb-9","status_url":"/jobs/jb-9"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const tasksPayload = `{"data":[
	{"uuid":"tk-1","name":"nightly-cleanup","command":"php artisan cleanup --force",
	 "cron_expression":"0 3 * * *","timezone":"Europe/Paris","enabled":true,
	 "overlap_policy":"skip","timeout_seconds":300,
	 "last_run_at":"2026-08-09T01:00:00Z","next_run_at":"2026-08-10T01:00:00Z"},
	{"uuid":"tk-2","name":"reindex","command":"bin/reindex",
	 "cron_expression":"@hourly","timezone":"UTC","enabled":false,
	 "last_run_at":null,"next_run_at":null}
]}`

// The listing answers what runs, when, and whether it will run at all — a
// disabled task with no next occurrence is not a broken schedule.
func TestTasksList(t *testing.T) {
	var ran string
	srv := tasksServer(t, tasksPayload, &ran)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(tasksCmd(), "list", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{
		"nightly-cleanup", "0 3 * * * (Europe/Paris)", "enabled", "php artisan cleanup",
		"reindex", "@hourly", "disabled", "tk-2",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want it to contain %q", out, want)
		}
	}
	// UTC is the default and stays implicit; a never-run task shows a dash.
	if strings.Contains(out, "(UTC)") {
		t.Fatalf("stdout = %q — the default timezone is not repeated on every row", out)
	}
	if !strings.Contains(out, "-") {
		t.Fatalf("stdout = %q — a null last run renders as a dash", out)
	}
}

// `ls` is the registered alias of every listing (ADR-070 §4).
func TestTasksListAlias(t *testing.T) {
	var ran string
	srv := tasksServer(t, tasksPayload, &ran)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(tasksCmd(), "ls", "varuna") })
	if err != nil || !strings.Contains(out, "nightly-cleanup") {
		t.Fatalf("err = %v, stdout = %q", err, out)
	}
}

// The table elides a long command; `-o json` is where the whole definition —
// command, policies, timeout — is read.
func TestTasksListJSON(t *testing.T) {
	var ran string
	srv := tasksServer(t, tasksPayload, &ran)
	setupContext(t, srv.URL)
	flags.output = "json"

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(tasksCmd(), "list", "varuna") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var tasks []scheduledTask
	if err := json.Unmarshal([]byte(out), &tasks); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	if len(tasks) != 2 || tasks[0].Command != "php artisan cleanup --force" || tasks[0].OverlapPolicy != "skip" {
		t.Fatalf("tasks = %+v", tasks)
	}
}

// TASK is a name or a UUID, resolved against this application's tasks — and the
// application itself stays the last, optional positional.
func TestTasksRunResolvesTheTask(t *testing.T) {
	for _, ref := range []string{"nightly-cleanup", "tk-1"} {
		var ran string
		srv := tasksServer(t, tasksPayload, &ran)
		setupContext(t, srv.URL)

		var err error
		out, _ := captureOutput(t, func() { err = runCmd(tasksCmd(), "run", ref, "varuna") })
		if err != nil {
			t.Fatalf("run %s: err = %v", ref, err)
		}
		if ran != "/api/v1/scheduled-tasks/tk-1/run" {
			t.Fatalf("run %s reached %q", ref, ran)
		}
		if !strings.Contains(out, "nightly-cleanup") || !strings.Contains(out, "jb-9") {
			t.Fatalf("run %s: stdout = %q — the caller is told what ran and under which job", ref, out)
		}
	}
}

// Two tasks may carry the same name; running "the first one" would be a silent
// guess about which command executes in production.
func TestTasksRunAmbiguousName(t *testing.T) {
	var ran string
	srv := tasksServer(t, `{"data":[
		{"uuid":"tk-a","name":"cleanup","command":"bin/a","cron_expression":"@daily","enabled":true},
		{"uuid":"tk-b","name":"cleanup","command":"bin/b","cron_expression":"@daily","enabled":true}
	]}`, &ran)
	setupContext(t, srv.URL)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(tasksCmd(), "run", "cleanup", "varuna") })
	if err == nil || !strings.Contains(err.Error(), "several scheduled tasks") {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"tk-a", "tk-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v — the message names the UUIDs to choose between", err)
		}
	}
	if ran != "" {
		t.Fatalf("ran %q — an ambiguous name executes nothing", ran)
	}
}

func TestTasksRunUnknownTask(t *testing.T) {
	var ran string
	srv := tasksServer(t, tasksPayload, &ran)
	setupContext(t, srv.URL)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(tasksCmd(), "run", "nope", "varuna") })
	if err == nil || !strings.Contains(err.Error(), `no scheduled task named "nope"`) {
		t.Fatalf("err = %v", err)
	}
	if ran != "" {
		t.Fatalf("ran %q", ran)
	}
}

// `run` with no TASK is a caller error (exit 2), and the message shows the
// shape — TASK first, the optional application last.
func TestTasksRunNeedsATask(t *testing.T) {
	setupHome(t)
	err := runCmd(tasksCmd(), "run")
	if err == nil || !IsUsageError(err) || !strings.Contains(err.Error(), "tasks run TASK [NAME]") {
		t.Fatalf("err = %v", err)
	}
	if err := runCmd(tasksCmd(), "run", "a", "b", "c"); err == nil || !IsUsageError(err) {
		t.Fatalf("three positionals: err = %v", err)
	}
}

// The group is exactly `list` and `run` (ADR-070 §1): a verb the API has no
// endpoint for would move the failure from --help to runtime.
func TestTasksGroupVerbs(t *testing.T) {
	verbs := map[string]bool{}
	for _, sub := range tasksCmd().Commands() {
		verbs[sub.Name()] = true
	}
	if len(verbs) != 2 || !verbs["list"] || !verbs["run"] {
		t.Fatalf("verbs = %v, want exactly list and run", verbs)
	}
}

// The elision is on runes: a command holding a UTF-8 path must not be cut
// mid-character.
func TestElide(t *testing.T) {
	if got := elide("bin/run", 48); got != "bin/run" {
		t.Fatalf("elide short = %q", got)
	}
	if got := elide("éééééé", 4); got != "ééé…" {
		t.Fatalf("elide = %q", got)
	}
}

// The ACTION column names what firing does: the command, or the workflow and
// its pinned ref for a dispatch task (ADR-071).
func TestTaskAction(t *testing.T) {
	if got := taskAction(scheduledTask{Command: "bin/run"}); got != "bin/run" {
		t.Fatalf("command task = %q", got)
	}
	if got := taskAction(scheduledTask{Kind: "github_workflow", WorkflowFile: "build.yml"}); got != "workflow: build.yml" {
		t.Fatalf("workflow task = %q", got)
	}
	if got := taskAction(scheduledTask{Kind: "github_workflow", WorkflowFile: "build.yml", WorkflowRef: "v2"}); got != "workflow: build.yml @ v2" {
		t.Fatalf("pinned workflow task = %q", got)
	}
}
