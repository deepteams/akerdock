package handlers

// Per-kind validation of scheduled tasks (ADR-071), on the opscov scaffolding:
// what one kind requires, the other refuses — a field that would be silently
// ignored teaches the operator a knob that does not exist.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/store"
)

// fillInt64Ptrs hands every nullable bigint a value: it is how the fake
// application row gets a git_source_id and a repository_id, and the fake git
// source its github_app_id.
func fillInt64Ptrs(_ int, dest any) bool {
	if p, ok := dest.(**int64); ok {
		v := int64(1)
		*p = &v
		return true
	}
	return false
}

// nilInt64Ptrs leaves every nullable bigint NULL — an application without a
// git source or a repository.
func nilInt64Ptrs(_ int, dest any) bool {
	if p, ok := dest.(**int64); ok {
		*p = nil
		return true
	}
	return false
}

// fillWorkflowKind makes the stored task a github_workflow one.
func fillWorkflowKind(_ int, dest any) bool {
	if p, ok := dest.(*store.TaskKind); ok {
		*p = store.TaskKindGithubWorkflow
		return true
	}
	return false
}

func TestCreateScheduledTaskKinds(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CreateScheduledTask(rec, opscovRequest(http.MethodPost, "/x", body), fixtureUUID, api.CreateScheduledTaskParams{})
		return rec
	}
	githubReady := func(db *opscovDB) {
		db.on("GetApplicationByUUID").fill = fillInt64Ptrs
		db.on("GetGitSourceByID").fill = fillInt64Ptrs
	}

	if rec := run(t, `{"kind":"whenever","name":"x","cron_expression":"daily"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown kind = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"kind":"github_workflow","name":"x","cron_expression":"daily"}`, githubReady); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "workflow_file") {
		t.Errorf("workflow without file = %d %s, want 422 naming workflow_file", rec.Code, rec.Body)
	}
	if rec := run(t, `{"kind":"github_workflow","name":"x","cron_expression":"daily","workflow_file":"build.yml","command":"make"}`, githubReady); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("workflow with command = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"kind":"github_workflow","name":"x","cron_expression":"daily","workflow_file":"build.yml","container":"web"}`, githubReady); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("workflow with container = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"x","cron_expression":"daily","command":"make","workflow_file":"build.yml"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("command kind with workflow_file = %d, want 422", rec.Code)
	}
	// An application without a GitHub App source can never dispatch: refused at
	// creation with the fix, not accepted and then failed nightly.
	if rec := run(t, `{"kind":"github_workflow","name":"x","cron_expression":"daily","workflow_file":"build.yml"}`, func(db *opscovDB) {
		db.on("GetApplicationByUUID").fill = nilInt64Ptrs
	}); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "GitHub App") {
		t.Errorf("no GitHub App source = %d %s, want 422 naming the fix", rec.Code, rec.Body)
	}
	good := `{"kind":"github_workflow","name":"nightly-build","cron_expression":"daily",` +
		`"workflow_file":"build.yml","workflow_ref":"main","workflow_inputs":{"reason":"nightly"}}`
	if rec := run(t, good, githubReady); rec.Code != http.StatusCreated {
		t.Errorf("workflow happy path = %d, want 201: %s", rec.Code, rec.Body)
	}
}

func TestUpdateScheduledTaskKinds(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateScheduledTask(rec, opscovRequest(http.MethodPatch, "/x", body), fixtureUUID,
			api.UpdateScheduledTaskParams{IfMatch: `"1"`})
		return rec
	}
	onWorkflowTask := func(db *opscovDB) {
		db.on("GetScheduledTaskByUUID").fill = fillWorkflowKind
	}

	if rec := run(t, `{"command":"make"}`, onWorkflowTask); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("command on a workflow task = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"container":null}`, onWorkflowTask); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("container on a workflow task = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"workflow_file":"deploy.yml"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("workflow_file on a command task = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"workflow_file":" "}`, onWorkflowTask); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("blank workflow_file = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"workflow_inputs":{"a":"1","b":"2","c":"3","d":"4","e":"5","f":"6","g":"7","h":"8","i":"9","j":"10","k":"11"}}`, onWorkflowTask); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("11 inputs = %d, want 422 (GitHub caps at 10)", rec.Code)
	}
	// Explicit nulls reset the ref and the inputs; the file moves; nothing of
	// the other kind is touched.
	full := `{"workflow_file":"deploy.yml","workflow_ref":null,"workflow_inputs":null}`
	if rec := run(t, full, onWorkflowTask); rec.Code != http.StatusOK {
		t.Errorf("workflow patch happy path = %d, want 200: %s", rec.Code, rec.Body)
	}
}
