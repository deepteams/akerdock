package queue

import (
	"context"
	"encoding/json"
	"time"

	"github.com/deepteams/akerdock/internal/store"
)

// Step mirrors the OpenAPI JobStep schema: every job step is visible, with
// a remediation message on failure (§20.1, §22.5).
type Step struct {
	Name       string     `json:"name"`
	Status     string     `json:"status"` // pending|running|succeeded|failed|skipped
	Message    *string    `json:"message,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// StepRecorder persists job steps as the handler progresses.
type StepRecorder struct {
	q     StepStore
	jobID int64
	steps []Step
}

type StepStore interface {
	UpdateJobSteps(context.Context, store.UpdateJobStepsParams) error
}

// NewStepRecorder starts an empty step timeline for a job attempt.
func NewStepRecorder(q StepStore, job store.Job) *StepRecorder {
	return &StepRecorder{q: q, jobID: job.ID}
}

// Start opens a new running step.
func (r *StepRecorder) Start(ctx context.Context, name string) {
	now := time.Now().UTC()
	r.steps = append(r.steps, Step{Name: name, Status: "running", StartedAt: &now})
	r.Flush(ctx)
}

// Succeed closes the current step as succeeded.
func (r *StepRecorder) Succeed(ctx context.Context, message string) {
	r.finish(ctx, "succeeded", message)
}

// Fail closes the current step as failed, with a remediation message.
func (r *StepRecorder) Fail(ctx context.Context, message string) { r.finish(ctx, "failed", message) }

// Skip records a step that did not need to run.
func (r *StepRecorder) Skip(ctx context.Context, name, message string) {
	now := time.Now().UTC()
	msg := message
	r.steps = append(r.steps, Step{Name: name, Status: "skipped", Message: &msg, StartedAt: &now, FinishedAt: &now})
	r.Flush(ctx)
}

func (r *StepRecorder) finish(ctx context.Context, status, message string) {
	if len(r.steps) == 0 {
		return
	}
	now := time.Now().UTC()
	current := &r.steps[len(r.steps)-1]
	current.Status = status
	current.FinishedAt = &now
	if message != "" {
		current.Message = &message
	}
	r.Flush(ctx)
}

// Flush persists the timeline; persistence failures are non-fatal for the
// job itself.
func (r *StepRecorder) Flush(ctx context.Context) {
	data, err := json.Marshal(r.steps)
	if err != nil {
		return
	}
	_ = r.q.UpdateJobSteps(ctx, store.UpdateJobStepsParams{ID: r.jobID, Steps: data})
}
