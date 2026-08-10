package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// Scheduled tasks (§192, amendement de spec n°15). The cron grammar is the one
// the backup plans already use — same normalizeCron, same aliases, same
// authority (cronexpr): a task the scheduler cannot fire is refused here,
// never accepted and then silently never run.

func scheduledTaskToAPI(t store.ScheduledTask, appUUID pgtype.UUID) api.ScheduledTask {
	out := api.ScheduledTask{
		Uuid:            ptr(uuidString(t.Uuid)),
		ApplicationUuid: ptr(uuidString(appUUID)),
		Kind:            api.TaskKind(t.Kind),
		Name:            t.Name,
		Command:         t.Command,
		Container:       t.Container,
		WorkflowFile:    t.WorkflowFile,
		WorkflowRef:     t.WorkflowRef,
		CronExpression:  t.CronExpression,
		Timezone:        ptr(t.Timezone),
		Enabled:         ptr(t.Enabled),
		OverlapPolicy:   ptr(api.TaskOverlapPolicy(t.OverlapPolicy)),
		MissedRunPolicy: ptr(api.TaskMissedRunPolicy(t.MissedRunPolicy)),
		TimeoutSeconds:  ptr(int(t.TimeoutSeconds)),
		Version:         ptr(int(t.Version)),
		CreatedAt:       timePtr(t.CreatedAt),
		UpdatedAt:       timePtr(t.UpdatedAt),
	}
	if len(t.WorkflowInputs) > 0 {
		var inputs map[string]string
		if json.Unmarshal(t.WorkflowInputs, &inputs) == nil {
			out.WorkflowInputs = &inputs
		}
	}
	// next_run_at is owned by the scheduler: until it has seen the task, the
	// field is absent rather than guessed.
	if t.NextRunAt.Valid {
		out.NextRunAt = timePtr(t.NextRunAt)
	}
	if t.LastRunAt.Valid {
		out.LastRunAt = timePtr(t.LastRunAt)
	}
	return out
}

func taskExecutionToAPI(e store.TaskExecution) api.TaskExecution {
	out := api.TaskExecution{
		Uuid:            ptr(uuidString(e.Uuid)),
		Status:          api.TaskExecutionStatus(e.Status),
		SkipReason:      e.SkipReason,
		Output:          e.Output,
		OutputTruncated: ptr(e.OutputTruncated),
		StartedAt:       e.StartedAt.Time,
		FinishedAt:      timePtr(e.FinishedAt),
	}
	if e.ExitCode != nil {
		out.ExitCode = ptr(int(*e.ExitCode))
	}
	if e.DurationMs != nil {
		out.DurationMs = ptr(int(*e.DurationMs))
	}
	return out
}

func (a *API) resolveScheduledTask(w http.ResponseWriter, r *http.Request, id *auth.Identity, taskUUID string) (store.GetScheduledTaskByUUIDRow, bool) {
	u, ok := a.scanUUID(w, r, taskUUID, "scheduled task")
	if !ok {
		return store.GetScheduledTaskByUUIDRow{}, false
	}
	// Uniform 404 across teams (INV-002): a task of another team is not found,
	// not forbidden — the difference is what tells an attacker it exists.
	row, err := a.Store.GetScheduledTaskByUUID(r.Context(), store.GetScheduledTaskByUUIDParams{
		Uuid: u, TeamID: id.TeamID,
	})
	return resolveRow(a, w, r, "scheduled task", row, err)
}

// ListScheduledTasks implements GET /applications/{uuid}/scheduled-tasks.
func (a *API) ListScheduledTasks(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.ListScheduledTasksParams) {
	id, ok := a.require(w, r, auth.PermApplicationsRead)
	if !ok {
		return
	}
	app, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	limit, ok := pageLimit(w, r, params.Limit)
	if !ok {
		return
	}
	after, ok := afterID(w, r, params.Cursor)
	if !ok {
		return
	}
	rows, err := a.Store.ListScheduledTasksPage(r.Context(), store.ListScheduledTasksPageParams{
		ResourceID: app.Resource.ID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list scheduled tasks", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(row store.ListScheduledTasksPageRow) int64 { return row.ScheduledTask.ID })

	data := make([]api.ScheduledTask, 0, len(rows))
	for _, row := range rows {
		data = append(data, scheduledTaskToAPI(row.ScheduledTask, row.ApplicationUuid))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.ScheduledTask `json:"data"`
		NextCursor *string             `json:"next_cursor"`
	}{data, cursor})
}

// CreateScheduledTask implements POST /applications/{uuid}/scheduled-tasks.
func (a *API) CreateScheduledTask(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.CreateScheduledTaskParams) {
	id, ok := a.require(w, r, auth.PermApplicationsExec)
	if !ok {
		return
	}
	app, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	var body api.ScheduledTaskCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	kind := store.TaskKindContainerCommand
	if body.Kind != nil {
		kind = store.TaskKind(*body.Kind)
	}
	var details []api.ErrorDetail
	if body.Kind != nil && !body.Kind.Valid() {
		details = append(details, api.ErrorDetail{Field: ptr("kind"), Code: ptr("invalid"), Message: "kind must be container_command or github_workflow"})
	}
	if strings.TrimSpace(body.Name) == "" {
		details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must not be empty"})
	}
	// The two kinds have disjoint required fields (ADR-071): what one requires,
	// the other refuses — a field that would be silently ignored teaches the
	// operator a knob that does not exist.
	switch kind {
	case store.TaskKindGithubWorkflow:
		if body.WorkflowFile == nil || strings.TrimSpace(*body.WorkflowFile) == "" {
			details = append(details, api.ErrorDetail{Field: ptr("workflow_file"), Code: ptr("required"), Message: "workflow_file must not be empty"})
		}
		if body.Command != nil {
			details = append(details, api.ErrorDetail{Field: ptr("command"), Code: ptr("invalid"), Message: "a github_workflow task has no command"})
		}
		if body.Container != nil {
			details = append(details, api.ErrorDetail{Field: ptr("container"), Code: ptr("invalid"), Message: "a github_workflow task has no container"})
		}
		if body.WorkflowInputs != nil && len(*body.WorkflowInputs) > 10 {
			details = append(details, api.ErrorDetail{Field: ptr("workflow_inputs"), Code: ptr("invalid"), Message: "GitHub caps workflow_dispatch inputs at 10"})
		}
		if detail, ok := a.checkGithubWorkflowSource(r, app); !ok {
			details = append(details, detail)
		}
	default:
		if body.Command == nil || strings.TrimSpace(*body.Command) == "" {
			details = append(details, api.ErrorDetail{Field: ptr("command"), Code: ptr("required"), Message: "command must not be empty"})
		}
		if body.WorkflowFile != nil || body.WorkflowRef != nil || body.WorkflowInputs != nil {
			details = append(details, api.ErrorDetail{Field: ptr("workflow_file"), Code: ptr("invalid"), Message: "workflow fields only apply to a github_workflow task"})
		}
	}
	cron, valid := normalizeCron(body.CronExpression)
	if !valid {
		details = append(details, api.ErrorDetail{
			Field: ptr("cron_expression"), Code: ptr("invalid"),
			Message: "cron_expression must be a 5-field cron expression or one of: every_minute, hourly, daily, weekly, monthly, yearly",
		})
	}
	timezone := "UTC"
	if body.Timezone != nil && *body.Timezone != "" {
		timezone = *body.Timezone
	}
	if !validTimezone(timezone) {
		details = append(details, api.ErrorDetail{
			Field: ptr("timezone"), Code: ptr("invalid"),
			Message: "timezone must be an IANA name, e.g. Europe/Paris",
		})
	}
	timeout := int32(300)
	if body.TimeoutSeconds != nil {
		if *body.TimeoutSeconds < 1 {
			details = append(details, api.ErrorDetail{Field: ptr("timeout_seconds"), Code: ptr("invalid"), Message: "timeout_seconds must be at least 1"})
		} else {
			timeout = int32(*body.TimeoutSeconds)
		}
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	overlap := store.TaskOverlapPolicySkip
	if body.OverlapPolicy != nil {
		overlap = store.TaskOverlapPolicy(*body.OverlapPolicy)
	}
	missed := store.TaskMissedRunPolicyRun
	if body.MissedRunPolicy != nil {
		missed = store.TaskMissedRunPolicy(*body.MissedRunPolicy)
	}

	create := store.CreateScheduledTaskParams{
		TeamID: id.TeamID, ResourceID: app.Resource.ID, Kind: kind,
		Name:           body.Name,
		CronExpression: cron, Timezone: timezone, Enabled: enabled,
		OverlapPolicy: overlap, MissedRunPolicy: missed, TimeoutSeconds: timeout,
	}
	if kind == store.TaskKindGithubWorkflow {
		create.WorkflowFile = ptr(strings.TrimSpace(*body.WorkflowFile))
		create.WorkflowRef = body.WorkflowRef
		if body.WorkflowInputs != nil && len(*body.WorkflowInputs) > 0 {
			raw, err := json.Marshal(*body.WorkflowInputs)
			if err != nil {
				a.internalError(w, r, "create scheduled task", err)
				return
			}
			create.WorkflowInputs = raw
		}
	} else {
		create.Command = body.Command
		create.Container = body.Container
	}
	task, err := a.Store.CreateScheduledTask(r.Context(), create)
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a scheduled task with this name already exists on this application")
			return
		}
		a.internalError(w, r, "create scheduled task", err)
		return
	}
	a.recordAudit(r, id, "scheduled_task.create", "application", app.Resource.Uuid)
	w.Header().Set("ETag", etagFor(task.Version))
	httpapi.WriteJSON(w, http.StatusCreated, scheduledTaskToAPI(task, app.Resource.Uuid))
}

// checkGithubWorkflowSource verifies at creation what the dispatch job will
// need at fire time: an application whose git source is a GitHub App. Fire
// time re-checks (sources change under a task); creation refuses what can
// never work, with a message naming the fix.
func (a *API) checkGithubWorkflowSource(r *http.Request, app store.GetApplicationByUUIDRow) (api.ErrorDetail, bool) {
	detail := api.ErrorDetail{
		Field: ptr("kind"), Code: ptr("invalid"),
		Message: "a github_workflow task needs an application whose git source is a GitHub App",
	}
	if app.Application.GitSourceID == nil || app.Application.RepositoryID == nil {
		return detail, false
	}
	source, err := a.Store.GetGitSourceByID(r.Context(), *app.Application.GitSourceID)
	if err != nil || source.GithubAppID == nil {
		return detail, false
	}
	return api.ErrorDetail{}, true
}

// GetScheduledTask implements GET /scheduled-tasks/{task_uuid}.
func (a *API) GetScheduledTask(w http.ResponseWriter, r *http.Request, taskUuid api.TaskUuid) {
	id, ok := a.require(w, r, auth.PermApplicationsRead)
	if !ok {
		return
	}
	row, ok := a.resolveScheduledTask(w, r, id, taskUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(row.ScheduledTask.Version))
	httpapi.WriteJSON(w, http.StatusOK, scheduledTaskToAPI(row.ScheduledTask, row.ApplicationUuid))
}

// UpdateScheduledTask implements PATCH /scheduled-tasks/{task_uuid}.
func (a *API) UpdateScheduledTask(w http.ResponseWriter, r *http.Request, taskUuid api.TaskUuid, params api.UpdateScheduledTaskParams) {
	id, ok := a.require(w, r, auth.PermApplicationsExec)
	if !ok {
		return
	}
	row, ok := a.resolveScheduledTask(w, r, id, taskUuid)
	if !ok {
		return
	}
	expected, err := strconv.Atoi(strings.Trim(strings.TrimSpace(params.IfMatch), `"`))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid If-Match header")
		return
	}
	var body api.ScheduledTaskUpdate
	fields, ok := decodePatch(w, r, &body)
	if !ok {
		return
	}

	// A field of the other kind is refused, not ignored (ADR-071): silently
	// accepting `command` on a workflow task would teach a knob that does not
	// exist. `kind` itself is not in the schema — it is immutable.
	if row.ScheduledTask.Kind == store.TaskKindGithubWorkflow {
		if fields.Has("command") || fields.Has("container") {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("command"), Code: ptr("invalid"),
				Message: "a github_workflow task has no command or container",
			}})
			return
		}
	} else if fields.Has("workflow_file") || fields.Has("workflow_ref") || fields.Has("workflow_inputs") {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("workflow_file"), Code: ptr("invalid"),
			Message: "workflow fields only apply to a github_workflow task",
		}})
		return
	}

	update := store.UpdateScheduledTaskParams{
		ID: row.ScheduledTask.ID, Version: int32(expected),
		Name: body.Name, Command: body.Command,
		Timezone: body.Timezone, Enabled: body.Enabled,
	}
	// `container: null` is meaningful — it means "the resource's own
	// container" — so the intent to set it must be distinguished from its
	// absence. A COALESCE would silently ignore an explicit null.
	if fields.Has("container") {
		update.SetContainer = true
		update.Container = body.Container
	}
	if body.WorkflowFile != nil {
		if strings.TrimSpace(*body.WorkflowFile) == "" {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("workflow_file"), Code: ptr("required"), Message: "workflow_file must not be empty",
			}})
			return
		}
		update.WorkflowFile = ptr(strings.TrimSpace(*body.WorkflowFile))
	}
	// Like `container`, an explicit `workflow_ref: null` (fall back to the
	// branch chain) must be distinguished from absence.
	if fields.Has("workflow_ref") {
		update.SetWorkflowRef = true
		update.WorkflowRef = body.WorkflowRef
	}
	if fields.Has("workflow_inputs") {
		update.SetWorkflowInputs = true
		if body.WorkflowInputs != nil && len(*body.WorkflowInputs) > 0 {
			if len(*body.WorkflowInputs) > 10 {
				httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
					Field: ptr("workflow_inputs"), Code: ptr("invalid"), Message: "GitHub caps workflow_dispatch inputs at 10",
				}})
				return
			}
			raw, err := json.Marshal(*body.WorkflowInputs)
			if err != nil {
				a.internalError(w, r, "update scheduled task", err)
				return
			}
			update.WorkflowInputs = raw
		}
	}
	if body.CronExpression != nil {
		cron, valid := normalizeCron(*body.CronExpression)
		if !valid {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("cron_expression"), Code: ptr("invalid"),
				Message: "cron_expression must be a 5-field cron expression or one of: every_minute, hourly, daily, weekly, monthly, yearly",
			}})
			return
		}
		update.CronExpression = &cron
	}
	if body.Timezone != nil && !validTimezone(*body.Timezone) {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("timezone"), Code: ptr("invalid"),
			Message: "timezone must be an IANA name, e.g. Europe/Paris",
		}})
		return
	}
	if body.TimeoutSeconds != nil {
		if *body.TimeoutSeconds < 1 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("timeout_seconds"), Code: ptr("invalid"), Message: "timeout_seconds must be at least 1",
			}})
			return
		}
		update.TimeoutSeconds = ptr(int32(*body.TimeoutSeconds))
	}
	if body.OverlapPolicy != nil {
		update.OverlapPolicy = ptr(store.TaskOverlapPolicy(*body.OverlapPolicy))
	}
	if body.MissedRunPolicy != nil {
		update.MissedRunPolicy = ptr(store.TaskMissedRunPolicy(*body.MissedRunPolicy))
	}

	rows, err := a.Store.UpdateScheduledTask(r.Context(), update)
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a scheduled task with this name already exists on this application")
			return
		}
		a.internalError(w, r, "update scheduled task", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, row.ScheduledTask.Version)
		return
	}
	updated, ok := a.resolveScheduledTask(w, r, id, taskUuid)
	if !ok {
		return
	}
	a.recordAudit(r, id, "scheduled_task.update", "scheduled_task", row.ScheduledTask.Uuid)
	w.Header().Set("ETag", etagFor(updated.ScheduledTask.Version))
	httpapi.WriteJSON(w, http.StatusOK, scheduledTaskToAPI(updated.ScheduledTask, updated.ApplicationUuid))
}

// DeleteScheduledTask implements DELETE /scheduled-tasks/{task_uuid}.
func (a *API) DeleteScheduledTask(w http.ResponseWriter, r *http.Request, taskUuid api.TaskUuid) {
	id, ok := a.require(w, r, auth.PermApplicationsExec)
	if !ok {
		return
	}
	row, ok := a.resolveScheduledTask(w, r, id, taskUuid)
	if !ok {
		return
	}
	if _, err := a.Store.SoftDeleteScheduledTask(r.Context(), row.ScheduledTask.ID); err != nil {
		a.internalError(w, r, "delete scheduled task", err)
		return
	}
	a.recordAudit(r, id, "scheduled_task.delete", "scheduled_task", row.ScheduledTask.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// RunScheduledTask implements POST /scheduled-tasks/{task_uuid}/run: the same
// execution path as the cron trigger, overlap policy included. A manual run
// that ignored the policy would be a second way to do the thing the policy
// exists to prevent.
func (a *API) RunScheduledTask(w http.ResponseWriter, r *http.Request, taskUuid api.TaskUuid, params api.RunScheduledTaskParams) {
	id, ok := a.require(w, r, auth.PermApplicationsExec)
	if !ok {
		return
	}
	row, ok := a.resolveScheduledTask(w, r, id, taskUuid)
	if !ok {
		return
	}
	task := row.ScheduledTask

	if task.OverlapPolicy == store.TaskOverlapPolicySkip {
		running, err := a.Store.CountRunningTaskExecutions(r.Context(), task.ID)
		if err != nil {
			a.internalError(w, r, "run scheduled task", err)
			return
		}
		if running > 0 {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
				"an execution of this task is still running, and its overlap policy is to skip")
			return
		}
	}

	exec, err := a.Store.CreateTaskExecution(r.Context(), store.CreateTaskExecutionParams{
		ScheduledTaskID: task.ID, Status: store.TaskExecutionStatusRunning,
	})
	if err != nil {
		a.internalError(w, r, "run scheduled task", err)
		return
	}
	lockKey := "scheduled_task:" + uuidString(task.Uuid)
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:      "task",
		Type:       jobs.TypeScheduledTaskRun,
		Payload:    jobs.ScheduledTaskPayload{TaskID: task.ID, ExecutionID: exec.ID},
		LockKey:    &lockKey,
		TeamID:     &id.TeamID,
		ResourceID: &task.ResourceID,
	})
	if err != nil {
		a.internalError(w, r, "run scheduled task", err)
		return
	}
	a.recordAudit(r, id, "scheduled_task.run", "scheduled_task", task.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{JobUuid: uuidString(job.Uuid)})
}

// ListTaskExecutions implements GET /scheduled-tasks/{task_uuid}/executions.
func (a *API) ListTaskExecutions(w http.ResponseWriter, r *http.Request, taskUuid api.TaskUuid, params api.ListTaskExecutionsParams) {
	id, ok := a.require(w, r, auth.PermApplicationsRead)
	if !ok {
		return
	}
	row, ok := a.resolveScheduledTask(w, r, id, taskUuid)
	if !ok {
		return
	}
	limit, ok := pageLimit(w, r, params.Limit)
	if !ok {
		return
	}
	after, ok := afterID(w, r, params.Cursor)
	if !ok {
		return
	}
	execs, err := a.Store.ListTaskExecutionsPage(r.Context(), store.ListTaskExecutionsPageParams{
		TaskID: row.ScheduledTask.ID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list task executions", err)
		return
	}
	execs, cursor := nextCursor(execs, limit, func(e store.TaskExecution) int64 { return e.ID })

	data := make([]api.TaskExecution, 0, len(execs))
	for _, e := range execs {
		data = append(data, taskExecutionToAPI(e))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.TaskExecution `json:"data"`
		NextCursor *string             `json:"next_cursor"`
	}{data, cursor})
}
