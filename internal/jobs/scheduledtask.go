package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeScheduledTaskRun runs one occurrence of a scheduled task (§192).
const TypeScheduledTaskRun = "scheduled_task.run"

// maxTaskOutput bounds what a task's output can cost us. A command that prints
// a gigabyte must not be able to fill the control plane's database; what is
// kept is the TAIL, because the end of an output is where the failure is.
const maxTaskOutput = 64 << 10

// ScheduledTaskPayload references the task and the execution row the API (or
// the scheduler) already created, so the history exists even if this job dies
// before it can write anything.
type ScheduledTaskPayload struct {
	TaskID      int64 `json:"task_id"`
	ExecutionID int64 `json:"execution_id"`
}

// ScheduledTaskRun executes a task's command inside the resource's container.
type ScheduledTaskRun struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Audit   *audit.Recorder
	Logger  *slog.Logger
}

// Execute runs one occurrence. A command that FAILS is a result, not a job to
// retry: the queue is told the job succeeded (the execution row carries the
// failure), because retrying would run the operator's command again behind
// their back — which is precisely what a cron must never do.
func (h *ScheduledTaskRun) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload ScheduledTaskPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	row, err := h.Store.GetScheduledTaskByID(ctx, payload.TaskID)
	if err != nil {
		return nil, fmt.Errorf("scheduled task vanished: %w", err)
	}
	task := row.ScheduledTask

	app, err := h.Store.GetApplicationByID(ctx, task.ResourceID)
	if err != nil {
		return nil, fmt.Errorf("resource vanished: %w", err)
	}
	// A task targets a container by the resource's UUID (INV-011). A compose
	// stack (P2) will name a service in `container` instead.
	container := pguuid.String(app.Resource.Uuid)
	if task.Container != nil && *task.Container != "" {
		container = *task.Container
	}

	dest, err := h.Store.GetDestinationByID(ctx, app.Resource.DestinationID)
	if err != nil {
		return nil, err
	}
	server, err := h.Store.GetServerByID(ctx, dest.ServerID)
	if err != nil {
		return nil, err
	}
	key, err := h.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return nil, err
	}
	pem, err := h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return nil, err
	}

	rec.Start(ctx, "exec")
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
	if err != nil {
		h.fail(ctx, payload.ExecutionID, nil, "SSH connection failed: "+err.Error())
		rec.Fail(ctx, "SSH connection failed")
		return nil, err
	}
	defer func() { _ = client.Close() }()

	// The command is the operator's shell, deliberately: it is quoted whole and
	// handed to the container's shell, never inspected or sanitised (INV-012 is
	// about the boundary, not about second-guessing the command).
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(task.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := fmt.Sprintf("docker exec %s sh -c %s", container, shellQuote(task.Command))

	res, err := client.Run(runCtx, cmd)
	if err != nil {
		// A timeout is a failure of the TASK, not of the job: retrying a command
		// that hangs would just hang again, and the history must say what
		// happened rather than leave a `running` row forever.
		reason := err.Error()
		if runCtx.Err() != nil {
			reason = fmt.Sprintf("the command exceeded its timeout of %ds", task.TimeoutSeconds)
		}
		h.fail(ctx, payload.ExecutionID, nil, reason)
		rec.Fail(ctx, reason)
		return map[string]any{"status": "failed", "reason": reason}, nil
	}

	output, truncated := clampOutput(res.Stdout + res.Stderr)
	status := store.TaskExecutionStatusSucceeded
	if res.ExitCode != 0 {
		status = store.TaskExecutionStatusFailed
	}
	exit := int32(res.ExitCode)
	if err := h.Store.FinishTaskExecution(ctx, store.FinishTaskExecutionParams{
		ID: payload.ExecutionID, Status: status, ExitCode: &exit,
		Output: &output, OutputTruncated: truncated,
	}); err != nil {
		return nil, err
	}

	if res.ExitCode != 0 {
		// A failing scheduled task is worth a notification (§290) — it is the
		// canonical thing that silently stops working.
		h.publish(ctx, task, app.Resource.Uuid, "scheduled_task.failed.v1", map[string]any{
			"task": task.Name, "exit_code": res.ExitCode,
		})
		rec.Fail(ctx, fmt.Sprintf("the command exited with code %d", res.ExitCode))
		return map[string]any{"status": "failed", "exit_code": res.ExitCode}, nil
	}
	h.publish(ctx, task, app.Resource.Uuid, "scheduled_task.succeeded.v1", map[string]any{"task": task.Name})
	rec.Succeed(ctx, "the command exited with code 0")
	return map[string]any{"status": "succeeded", "exit_code": 0}, nil
}

// fail closes the execution row. It never returns an error to the queue for a
// task-level failure: the command failing is a RESULT, not a job that must be
// retried — retrying it would run the command again, which is exactly what a
// cron must not do behind the operator's back.
func (h *ScheduledTaskRun) fail(ctx context.Context, executionID int64, exit *int32, reason string) {
	output, truncated := clampOutput(reason)
	if err := h.Store.FinishTaskExecution(ctx, store.FinishTaskExecutionParams{
		ID: executionID, Status: store.TaskExecutionStatusFailed, ExitCode: exit,
		Output: &output, OutputTruncated: truncated,
	}); err != nil {
		h.Logger.Warn("cannot close the task execution", "execution_id", executionID, "error", err)
	}
}

// publish routes the outcome to the notification pipeline. `scheduled_task.failed`
// is classified critical by its suffix (notify.SeverityOf): a cron that stops
// working is the canonical thing nobody notices.
func (h *ScheduledTaskRun) publish(ctx context.Context, task store.ScheduledTask, resourceUUID pgtype.UUID, event string, payload map[string]any) {
	if h.Audit == nil {
		return
	}
	var teamUUID pgtype.UUID
	if team, err := h.Store.GetTeamByID(ctx, task.TeamID); err == nil {
		teamUUID = team.Uuid
	}
	payload["task_uuid"] = pguuid.String(task.Uuid)
	h.Audit.Outbox(ctx, h.Store, event, teamUUID, resourceUUID,
		"scheduled_task:"+pguuid.String(task.Uuid), payload)
}

// clampOutput keeps the TAIL of an output and says so. The head of a build log
// is boilerplate; the end is where the error is.
func clampOutput(s string) (string, bool) {
	if len(s) <= maxTaskOutput {
		return s, false
	}
	return s[len(s)-maxTaskOutput:], true
}
