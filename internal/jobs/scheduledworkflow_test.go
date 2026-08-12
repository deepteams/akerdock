// Execute-level coverage for the github_workflow task kind (ADR-071), on the
// prevjobs scaffolding: the fake DB steers the resolution chain, an httptest
// server plays GitHub. All scenarios assert the ADR's core invariant — every
// dispatch-level failure is a RESULT handed back as a queue success, because a
// dispatch is not idempotent and the queue must never replay one.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/githubapp"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/envelope"
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

	// The 403 GitHub sends for a missing installation permission names no
	// permission at all. The recorded reason must keep GitHub's answer and add
	// the action, or the operator reads it as a defect of the platform.
	t.Run("a permission refusal carries both the answer and the fix", func(t *testing.T) {
		srv, _ := workflowGithubServer(t, http.StatusForbidden)
		h, q, _ := workflowDeps(t, srv.URL)
		j := workflowTaskJob()
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil {
			t.Fatalf("a 403 must not reach the queue: %v", err)
		}
		reason, _ := result.(map[string]any)["reason"].(string)
		for _, want := range []string{"403", "Actions (write)", "approve the request"} {
			if !strings.Contains(reason, want) {
				t.Errorf("reason = %q, want it to mention %q", reason, want)
			}
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

// workflowTask builds a dispatchable task; the mutation shapes the row the
// CHECK constraint cannot be asked to produce (a NULL workflow_file, a zero
// timeout), which is exactly what these refusals defend against.
func workflowTask(mut func(*store.ScheduledTask)) store.ScheduledTask {
	task := store.ScheduledTask{
		ID: 1, TeamID: 1, ResourceID: 1, Name: "nightly",
		Kind: store.TaskKindGithubWorkflow, TimeoutSeconds: 30,
		WorkflowFile: ptr("build.yml"),
	}
	_ = task.Uuid.Scan(jobFixtureUUID)
	if mut != nil {
		mut(&task)
	}
	return task
}

// workflowApp is an application whose resolution chain reaches a GitHub App:
// both foreign keys are set, so the refusals below come from the rows the fake
// returns rather than from the application itself.
func workflowApp() store.GetApplicationByIDRow {
	return prevjobsApp(func(a *store.GetApplicationByIDRow) {
		a.Application.GitSourceID = ptr(int64(1))
		a.Application.RepositoryID = ptr(int64(1))
	})
}

// The refusals dispatchWorkflow reports itself, driven at the function rather
// than through Execute: the fake shapes a whole row at a time, and these cases
// need one column at a time. Every one of them asserts the ADR's invariant —
// nil error, failure in the result.
func TestScheduledWorkflowDispatchRefusals(t *testing.T) {
	ctx := context.Background()
	payload := ScheduledTaskPayload{TaskID: 1, ExecutionID: 1}
	dispatch := func(t *testing.T, h *ScheduledTaskRun, q *store.Queries, task store.ScheduledTask) map[string]any {
		t.Helper()
		j := workflowTaskJob()
		out, err := h.dispatchWorkflow(ctx, payload, task, workflowApp(), queue.NewStepRecorder(q, j))
		if err != nil {
			t.Fatalf("a dispatch-level failure must never reach the queue: %v", err)
		}
		result, _ := out.(map[string]any)
		if result == nil {
			t.Fatalf("result = %#v", out)
		}
		return result
	}
	refused := func(t *testing.T, result map[string]any, want string) {
		t.Helper()
		reason, _ := result["reason"].(string)
		if result["status"] != "failed" || !strings.Contains(reason, want) {
			t.Fatalf("result = %#v, want a failure mentioning %q", result, want)
		}
	}

	t.Run("a hand-edited row without workflow_file is refused, not dereferenced", func(t *testing.T) {
		h, q, _ := workflowDeps(t, "http://unused.invalid")
		result := dispatch(t, h, q, workflowTask(func(task *store.ScheduledTask) { task.WorkflowFile = nil }))
		refused(t, result, "no workflow_file")
	})

	t.Run("no ref anywhere is refused, never a guessed main", func(t *testing.T) {
		srv, captured := workflowGithubServer(t, http.StatusNoContent)
		h, q, _ := workflowDeps(t, srv.URL)
		// Nothing pinned on the task, no branch on the application, and the
		// repository's default_branch stays NULL: the chain runs dry.
		result := dispatch(t, h, q, workflowTask(nil))
		refused(t, result, "no ref to dispatch on")
		if captured.Path != "" {
			t.Fatal("a task with no ref must not dispatch anything")
		}
	})

	t.Run("the repository default branch closes the fallback chain", func(t *testing.T) {
		srv, captured := workflowGithubServer(t, http.StatusNoContent)
		h, q, db := workflowDeps(t, srv.URL)
		db.fillPtr["GetRepositoryByID"] = true // default_branch is no longer NULL
		result := dispatch(t, h, q, workflowTask(nil))
		if result["status"] != "succeeded" {
			t.Fatalf("result = %#v", result)
		}
		// The fake gives every string column of a row the same value, so the
		// default branch reads as the repository's full name here.
		if captured.Body["ref"] != "acme/site" {
			t.Fatalf("dispatch ref = %v, want the repository default branch", captured.Body["ref"])
		}
	})

	t.Run("a dispatch that outruns its timeout says so", func(t *testing.T) {
		srv, _ := workflowGithubServer(t, http.StatusNoContent)
		h, q, _ := workflowDeps(t, srv.URL)
		result := dispatch(t, h, q, workflowTask(func(task *store.ScheduledTask) {
			task.WorkflowRef = ptr("main")
			task.TimeoutSeconds = 0 // the dispatch context is over before it starts
		}))
		refused(t, result, "exceeded its timeout of 0s")
	})

	t.Run("a bookkeeping failure after an accepted dispatch stays a success", func(t *testing.T) {
		srv, captured := workflowGithubServer(t, http.StatusNoContent)
		h, q, db := workflowDeps(t, srv.URL)
		db.errs["FinishTaskExecution"] = fmt.Errorf("db gone")
		result := dispatch(t, h, q, workflowTask(func(task *store.ScheduledTask) {
			task.WorkflowRef = ptr("main")
		}))
		// GitHub already started a build: reporting a failure here would have
		// the queue dispatch a second one.
		if result["status"] != "succeeded" || captured.Path == "" {
			t.Fatalf("result = %#v, dispatch path = %q", result, captured.Path)
		}
	})
}

// Every refusal of the resolution chain application → git source → GitHub App
// → repository → installation token. The error strings are what the operator
// reads in the execution history, so each case asserts the fix it names.
func TestScheduledWorkflowGithubResolution(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, h *ScheduledTaskRun, db *prevjobsDB, keyring *envelope.Keyring)
		want  string
	}{
		{
			name: "a git source that is not a GitHub App",
			setup: func(_ *testing.T, _ *ScheduledTaskRun, db *prevjobsDB, _ *envelope.Keyring) {
				db.fillPtr["GetGitSourceByID"] = false
			},
			want: "not a GitHub App",
		},
		{
			name: "the App row vanished",
			setup: func(_ *testing.T, _ *ScheduledTaskRun, db *prevjobsDB, _ *envelope.Keyring) {
				db.errs["GetGithubAppByID"] = fmt.Errorf("no rows")
			},
			want: "no longer exists",
		},
		{
			name: "the repository is not known yet",
			setup: func(_ *testing.T, _ *ScheduledTaskRun, db *prevjobsDB, _ *envelope.Keyring) {
				db.errs["GetRepositoryByID"] = fmt.Errorf("no rows")
			},
			want: "redeploy once to resync",
		},
		{
			name:  "no keyring is configured",
			setup: func(_ *testing.T, h *ScheduledTaskRun, _ *prevjobsDB, _ *envelope.Keyring) { h.Keyring = nil },
			want:  "no keyring",
		},
		{
			name: "the stored private key does not decrypt",
			setup: func(_ *testing.T, _ *ScheduledTaskRun, db *prevjobsDB, _ *envelope.Keyring) {
				db.blobs["GetGithubAppByID"] = []byte("not ciphertext")
			},
			want: "envelope:",
		},
		{
			name: "the decrypted key is not a key",
			setup: func(t *testing.T, _ *ScheduledTaskRun, db *prevjobsDB, keyring *envelope.Keyring) {
				db.blobs["GetGithubAppByID"] = prevjobsEncrypt(t, keyring,
					"github_apps", "app_private_key_enc", []byte("not a PEM block"))
			},
			want: "not PEM",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, q, db := workflowDeps(t, "http://unused.invalid")
			tc.setup(t, h, db, h.Keyring)
			j := workflowTaskJob()
			result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
			reason, _ := result.(map[string]any)["reason"].(string)
			if err != nil || !strings.Contains(reason, tc.want) {
				t.Fatalf("result = %#v, err = %v, want a failure mentioning %q", result, err, tc.want)
			}
		})
	}

	t.Run("github refuses to mint the installation token", func(t *testing.T) {
		srv := prevjobsGithubServer(t, map[string]int{"access_tokens": http.StatusInternalServerError})
		h, q, _ := workflowDeps(t, srv.URL)
		j := workflowTaskJob()
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		reason, _ := result.(map[string]any)["reason"].(string)
		if err != nil || !strings.Contains(reason, "github installation token") {
			t.Fatalf("result = %#v, err = %v", result, err)
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

// GitHub answers "Resource not accessible by integration" for any missing
// installation permission and never names the permission. Left raw, the message
// reads like an AkerDock defect; the hint is what turns it into an action.
func TestDispatchPermissionHint(t *testing.T) {
	forbidden := &githubapp.APIError{
		Status: http.StatusForbidden,
		Path:   "/repos/acme/app/actions/workflows/tag.yml/dispatches",
		Body:   `{"message":"Resource not accessible by integration","status":"403"}`,
	}
	hint := dispatchPermissionHint(forbidden)
	for _, want := range []string{"Actions (write)", "approve the request"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint = %q, want it to mention %q", hint, want)
		}
	}

	// Every other failure keeps GitHub's own words: inventing a permission
	// problem where there is none would send the reader to the wrong screen.
	for name, err := range map[string]error{
		"another 403":       &githubapp.APIError{Status: http.StatusForbidden, Body: `{"message":"Repository was archived"}`},
		"a 404":             &githubapp.APIError{Status: http.StatusNotFound, Body: `{"message":"Not Found"}`},
		"a 422":             &githubapp.APIError{Status: http.StatusUnprocessableEntity, Body: `{"message":"Workflow does not have workflow_dispatch trigger"}`},
		"a transport error": errors.New("dial tcp: connection refused"),
	} {
		if got := dispatchPermissionHint(err); got != "" {
			t.Errorf("%s must not produce a permission hint, got %q", name, got)
		}
	}
}
