package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/githubapp"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// dispatchWorkflow fires one occurrence of a `github_workflow` task (ADR-071):
// it asks GitHub to dispatch the task's workflow on the application's
// repository, signed by its GitHub App. Every refusal — a missing
// installation, an unresolvable ref, a 4xx/5xx from GitHub — is a RESULT
// written to the history and published, never an error returned to the queue:
// a dispatch is not idempotent (each one starts a build), so the queue must
// never replay it behind the operator's back.
func (h *ScheduledTaskRun) dispatchWorkflow(ctx context.Context, payload ScheduledTaskPayload, task store.ScheduledTask, app store.GetApplicationByIDRow, rec *queue.StepRecorder) (any, error) {
	rec.Start(ctx, "dispatch")
	if task.WorkflowFile == nil || *task.WorkflowFile == "" {
		// Unreachable while the CHECK constraint holds; refused here anyway so
		// a hand-edited row fails with a reason instead of a nil dereference.
		return h.failWorkflow(ctx, payload, task, app, rec, "the task has no workflow_file"), nil
	}

	client, token, fullName, defaultBranch, err := h.githubForTask(ctx, app)
	if err != nil {
		return h.failWorkflow(ctx, payload, task, app, rec, err.Error()), nil
	}

	ref := resolveDispatchRef(task, app, defaultBranch)
	if ref == "" {
		return h.failWorkflow(ctx, payload, task, app, rec,
			"no ref to dispatch on: set workflow_ref, or a branch on the application"), nil
	}

	var inputs map[string]string
	if len(task.WorkflowInputs) > 0 {
		if err := json.Unmarshal(task.WorkflowInputs, &inputs); err != nil {
			return h.failWorkflow(ctx, payload, task, app, rec, "workflow_inputs is not a string map: "+firstLine(err.Error())), nil
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(task.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := client.DispatchWorkflow(runCtx, token, fullName, *task.WorkflowFile, ref, inputs); err != nil {
		reason := firstLine(err.Error())
		if runCtx.Err() != nil {
			reason = fmt.Sprintf("the dispatch exceeded its timeout of %ds", task.TimeoutSeconds)
		}
		return h.failWorkflow(ctx, payload, task, app, rec, reason), nil
	}

	// GitHub accepted the dispatch: this is the success this kind records (the
	// build's own outcome stays on GitHub, §3 of the ADR). From here on,
	// bookkeeping failures are logged and swallowed — returning an error would
	// make the queue dispatch a second build for the same occurrence.
	message := fmt.Sprintf("dispatched %s on %s (%s)", *task.WorkflowFile, ref, fullName)
	if err := h.Store.FinishTaskExecution(ctx, store.FinishTaskExecutionParams{
		ID: payload.ExecutionID, Status: store.TaskExecutionStatusSucceeded,
		Output: &message,
	}); err != nil {
		h.Logger.Warn("cannot close the task execution", "execution_id", payload.ExecutionID, "error", err)
	}
	h.publish(ctx, task, app.Resource.Uuid, "scheduled_task.succeeded.v1", map[string]any{
		"task": task.Name, "workflow": *task.WorkflowFile, "ref": ref,
	})
	rec.Succeed(ctx, message)
	return map[string]any{"status": "succeeded"}, nil
}

// resolveDispatchRef picks the ref to dispatch on, first match wins: the
// task's pinned ref, the application's branch, the repository's default
// branch (ADR-071 §2). Resolution happens at fire time, never at creation: a
// repository whose default branch is renamed keeps working. Empty means "no
// ref anywhere" and is the caller's failure to report — never a guessed main.
func resolveDispatchRef(task store.ScheduledTask, app store.GetApplicationByIDRow, defaultBranch string) string {
	switch {
	case task.WorkflowRef != nil && *task.WorkflowRef != "":
		return *task.WorkflowRef
	case app.Application.GitBranch != nil && *app.Application.GitBranch != "":
		return *app.Application.GitBranch
	default:
		return defaultBranch
	}
}

// failWorkflow closes the execution with the reason, publishes the failure —
// a cron that stops dispatching is the canonical thing nobody notices (§290) —
// and builds the result the queue is handed as a success: the failure is the
// result.
func (h *ScheduledTaskRun) failWorkflow(ctx context.Context, payload ScheduledTaskPayload, task store.ScheduledTask, app store.GetApplicationByIDRow, rec *queue.StepRecorder, reason string) map[string]any {
	h.fail(ctx, payload.ExecutionID, reason)
	h.publish(ctx, task, app.Resource.Uuid, "scheduled_task.failed.v1", map[string]any{
		"task": task.Name, "reason": reason,
	})
	rec.Fail(ctx, reason)
	return map[string]any{"status": "failed", "reason": reason}
}

// githubForTask resolves application → git source → GitHub App → repository
// and mints an installation token restricted to that one repository — the same
// chain and the same restriction as the deploy job's installGithubToken. The
// error strings name the fix: they end up verbatim in the execution history.
func (h *ScheduledTaskRun) githubForTask(ctx context.Context, app store.GetApplicationByIDRow) (client *githubapp.Client, token, fullName, defaultBranch string, err error) {
	if app.Application.GitSourceID == nil || app.Application.RepositoryID == nil {
		return nil, "", "", "", fmt.Errorf("this application has no GitHub App source — a workflow dispatch is signed by the App (§2.2)")
	}
	source, err := h.Store.GetGitSourceByID(ctx, *app.Application.GitSourceID)
	if err != nil || source.GithubAppID == nil {
		return nil, "", "", "", fmt.Errorf("this application's git source is not a GitHub App")
	}
	gh, err := h.Store.GetGithubAppByID(ctx, *source.GithubAppID)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("the GitHub App of this source no longer exists")
	}
	if gh.AppID == nil || gh.InstallationID == nil || gh.AppPrivateKeyEnc == nil {
		return nil, "", "", "", fmt.Errorf("the GitHub App is not installed yet — finish the installation first")
	}
	repo, err := h.Store.GetRepositoryByID(ctx, *app.Application.RepositoryID)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("the application's repository is not known — redeploy once to resync")
	}
	if h.Keyring == nil {
		return nil, "", "", "", fmt.Errorf("no keyring is configured")
	}
	pem, err := h.Keyring.Decrypt("github_apps", "app_private_key_enc", pguuid.String(gh.Uuid), gh.AppPrivateKeyEnc)
	if err != nil {
		return nil, "", "", "", err
	}
	client = &githubapp.Client{APIURL: gh.ApiUrl}
	jwt, err := githubapp.AppJWT(*gh.AppID, pem, time.Now())
	if err != nil {
		return nil, "", "", "", err
	}
	var repos []string
	if _, name, ok := strings.Cut(repo.FullName, "/"); ok {
		repos = []string{name}
	}
	minted, err := client.InstallationToken(ctx, jwt, *gh.InstallationID, repos)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("github installation token: %w", err)
	}
	if repo.DefaultBranch != nil {
		defaultBranch = *repo.DefaultBranch
	}
	return client, minted.Token, repo.FullName, defaultBranch, nil
}
