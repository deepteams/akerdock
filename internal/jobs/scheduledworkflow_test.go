// Execute-level coverage for the github_workflow task kind (ADR-071), on the
// prevjobs scaffolding: the fake DB steers the resolution chain, an httptest
// server plays GitHub. All scenarios assert the ADR's core invariant — every
// dispatch-level failure is a RESULT handed back as a queue success, because a
// dispatch is not idempotent and the queue must never replay one.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// workflowTaskJob mirrors miscjobsTaskJob for the dispatch kind.
func workflowTaskJob() store.Job {
	return store.Job{ID: 15, JobType: TypeScheduledTaskRun, Payload: []byte(`{"task_id":1,"execution_id":1}`)}
}

// workflowDeps wires a ScheduledTaskRun whose task resolves as github_workflow
// and whose GitHub App points at srvURL. The steering knobs stay exposed
// through the returned DB.
func workflowDeps(t *testing.T, srvURL string) (*ScheduledTaskRun, *store.Queries, *prevjobsDB) {
	t.Helper()
	q, keyring, logger, db := prevjobsDeps(t)
	db.enums["TaskKind"] = string(store.TaskKindGithubWorkflow)
	db.fillPtr["GetScheduledTaskByID"] = true // workflow_file, workflow_ref set
	db.strs["GetScheduledTaskByID"] = "build.yml"
	db.blobs["GetScheduledTaskByID"] = []byte(`{"reason":"nightly"}`) // workflow_inputs
	db.fillPtr["GetApplicationByID"] = true                           // git_source_id, repository_id set
	db.fillPtr["GetGitSourceByID"] = true                             // github_app_id set
	db.fillPtr["GetGithubAppByID"] = true                             // app_id, installation_id set
	db.strs["GetGithubAppByID"] = srvURL
	db.blobs["GetGithubAppByID"] = prevjobsEncrypt(t, keyring,
		"github_apps", "app_private_key_enc", prevjobsRSAKeyPEM(t))
	db.strs["GetRepositoryByID"] = "acme/site"
	h := &ScheduledTaskRun{
		Store: q, Keyring: keyring,
		Audit:  &audit.Recorder{Store: q, Logger: logger},
		Logger: logger,
	}
	return h, q, db
}

// workflowGithubServer answers the token mint and captures the dispatch.
func workflowGithubServer(t *testing.T, dispatchStatus int) (*httptest.Server, *struct {
	Path string
	Body map[string]any
},
) {
	t.Helper()
	captured := &struct {
		Path string
		Body map[string]any
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"tok","expires_at":%q}`,
				time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/dispatches"):
			captured.Path = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&captured.Body)
			w.WriteHeader(dispatchStatus)
			if dispatchStatus >= 400 {
				_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
			}
		default:
			t.Errorf("unexpected GitHub call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

func TestScheduledWorkflowDispatch(t *testing.T) {
	ctx := context.Background()

	t.Run("accepted dispatch is the success", func(t *testing.T) {
		srv, captured := workflowGithubServer(t, http.StatusNoContent)
		h, q, _ := workflowDeps(t, srv.URL)
		j := workflowTaskJob()
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["status"] != "succeeded" {
			t.Fatalf("result = %#v, %v", result, err)
		}
		// The workflow is named by the task, the repo by its repository row,
		// and the ref pinned on the task wins the fallback chain.
		if captured.Path != "/repos/acme/site/actions/workflows/build.yml/dispatches" {
			t.Fatalf("dispatch path = %q", captured.Path)
		}
		if captured.Body["ref"] != "build.yml" { // strs fills workflow_ref too
			t.Fatalf("dispatch ref = %v", captured.Body["ref"])
		}
		inputs, _ := captured.Body["inputs"].(map[string]any)
		if inputs["reason"] != "nightly" {
			t.Fatalf("dispatch inputs = %v", captured.Body["inputs"])
		}
	})

	t.Run("github refusal is a result, not a retry", func(t *testing.T) {
		srv, _ := workflowGithubServer(t, http.StatusForbidden)
		h, q, _ := workflowDeps(t, srv.URL)
		j := workflowTaskJob()
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil {
			t.Fatalf("a 403 must not reach the queue: %v", err)
		}
		reason, _ := result.(map[string]any)["reason"].(string)
		if result.(map[string]any)["status"] != "failed" || !strings.Contains(reason, "403") {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("no GitHub App source fails with the fix", func(t *testing.T) {
		h, q, db := workflowDeps(t, "http://unused.invalid")
		db.fillPtr["GetApplicationByID"] = false // git_source_id NULL
		j := workflowTaskJob()
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		reason, _ := result.(map[string]any)["reason"].(string)
		if err != nil || !strings.Contains(reason, "no GitHub App source") {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})

	t.Run("uninstalled App fails with the fix", func(t *testing.T) {
		h, q, db := workflowDeps(t, "http://unused.invalid")
		db.fillPtr["GetGithubAppByID"] = false // app_id, installation_id NULL
		j := workflowTaskJob()
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		reason, _ := result.(map[string]any)["reason"].(string)
		if err != nil || !strings.Contains(reason, "not installed yet") {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})

	t.Run("unparseable inputs fail before any dispatch", func(t *testing.T) {
		srv, captured := workflowGithubServer(t, http.StatusNoContent)
		h, q, db := workflowDeps(t, srv.URL)
		db.blobs["GetScheduledTaskByID"] = []byte("not a string map")
		j := workflowTaskJob()
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		reason, _ := result.(map[string]any)["reason"].(string)
		if err != nil || !strings.Contains(reason, "workflow_inputs") {
			t.Fatalf("result = %#v, %v", result, err)
		}
		if captured.Path != "" {
			t.Fatal("a task with broken inputs must not dispatch anything")
		}
	})

	t.Run("task vanished is the one queue-level error", func(t *testing.T) {
		h, q, db := workflowDeps(t, "http://unused.invalid")
		db.errs["GetScheduledTaskByID"] = fmt.Errorf("no rows")
		j := workflowTaskJob()
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("a vanished task is a job failure, not a task result")
		}
	})
}

// The fallback chain of ADR-071 §2: task ref, then application branch, then
// the repository's default branch — and empty when none exists, never a
// guessed main.
func TestResolveDispatchRef(t *testing.T) {
	ref := func(s string) *string { return &s }
	app := func(branch *string) store.GetApplicationByIDRow {
		var row store.GetApplicationByIDRow
		row.Application.GitBranch = branch
		return row
	}
	cases := []struct {
		name          string
		task          store.ScheduledTask
		app           store.GetApplicationByIDRow
		defaultBranch string
		want          string
	}{
		{"task ref wins", store.ScheduledTask{WorkflowRef: ref("v2")}, app(ref("develop")), "main", "v2"},
		{"empty task ref falls through", store.ScheduledTask{WorkflowRef: ref("")}, app(ref("develop")), "main", "develop"},
		{"application branch next", store.ScheduledTask{}, app(ref("develop")), "main", "develop"},
		{"default branch last", store.ScheduledTask{}, app(nil), "main", "main"},
		{"nothing anywhere is empty", store.ScheduledTask{}, app(nil), "", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveDispatchRef(tt.task, tt.app, tt.defaultBranch); got != tt.want {
				t.Fatalf("ref = %q, want %q", got, tt.want)
			}
		})
	}
}
