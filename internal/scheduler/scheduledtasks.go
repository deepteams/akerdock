package scheduler

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/cronexpr"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// runDueScheduledTasks fires the tasks whose cron occurrence has passed
// (§192). Only the elected leader runs this, so the read-then-write on
// next_run_at needs no extra locking.
func (s *Scheduler) runDueScheduledTasks(ctx context.Context) {
	tasks, err := s.Store.ListSchedulableTasks(ctx)
	if err != nil {
		s.Logger.Warn("task scheduling: cannot list tasks", "error", err)
		return
	}
	now := time.Now()
	for _, task := range tasks {
		if ctx.Err() != nil {
			return
		}
		if err := s.scheduleTask(ctx, task.ScheduledTask, now); err != nil {
			s.Logger.Warn("task scheduling failed", "task_id", task.ScheduledTask.ID, "error", err)
		}
	}
}

func (s *Scheduler) scheduleTask(ctx context.Context, task store.ScheduledTask, now time.Time) error {
	schedule, err := cronexpr.Parse(task.CronExpression)
	if err != nil {
		// Validated at the API edge; an unparseable expression means a
		// hand-written row. Disabling the task silently would hide a cron that
		// never runs, so it is left alone and reported on every pass.
		return err
	}
	loc, err := time.LoadLocation(task.Timezone)
	if err != nil {
		return err
	}

	// No window yet (freshly created, re-enabled, or rescheduled): seed it
	// without firing. A task's first run is its first occurrence, not the
	// moment it was created.
	if !task.NextRunAt.Valid {
		return s.advanceTask(ctx, task, schedule, loc, now, false)
	}
	if task.NextRunAt.Time.After(now) {
		return nil
	}

	// Was this occurrence MISSED, or is it merely due? An occurrence is missed
	// when a whole period went by without the scheduler seeing it — the
	// instance was down, or this process was not the leader. The threshold is
	// the task's own period, so it scales with the cron: a minutely task
	// tolerates a minute of lateness, a daily one tolerates a day. No magic
	// constant, and no need to know when the instance was last up.
	period := schedule.Next(task.NextRunAt.Time, loc).Sub(task.NextRunAt.Time)
	missed := period > 0 && now.Sub(task.NextRunAt.Time) > period
	if missed && task.MissedRunPolicy == store.TaskMissedRunPolicySkip {
		s.recordSkip(ctx, task, "the occurrence was missed (the instance was down) and the policy is to skip it")
		return s.advanceTask(ctx, task, schedule, loc, now, false)
	}

	// One run at a time, unless the task asked for a queue. A cron firing
	// faster than it completes must not pile up executions on the server.
	if task.OverlapPolicy == store.TaskOverlapPolicySkip {
		running, err := s.Store.CountRunningTaskExecutions(ctx, task.ID)
		if err != nil {
			return err
		}
		if running > 0 {
			s.recordSkip(ctx, task, "the previous execution was still running")
			return s.advanceTask(ctx, task, schedule, loc, now, false)
		}
	}

	if _, err := s.enqueueTask(ctx, task); err != nil {
		return err
	}
	return s.advanceTask(ctx, task, schedule, loc, now, true)
}

// enqueueTask opens the execution row FIRST, then queues the job that will
// close it. The row is the history: created before the work, it exists even if
// the worker dies before writing anything, and the operator sees a `running`
// execution rather than nothing at all.
func (s *Scheduler) enqueueTask(ctx context.Context, task store.ScheduledTask) (store.TaskExecution, error) {
	exec, err := s.Store.CreateTaskExecution(ctx, store.CreateTaskExecutionParams{
		ScheduledTaskID: task.ID, Status: store.TaskExecutionStatusRunning,
	})
	if err != nil {
		return store.TaskExecution{}, err
	}
	lockKey := "scheduled_task:" + pguuid.String(task.Uuid)
	job, err := queue.Enqueue(ctx, s.Store, queue.EnqueueOptions{
		Queue:      "task",
		Type:       jobs.TypeScheduledTaskRun,
		Payload:    jobs.ScheduledTaskPayload{TaskID: task.ID, ExecutionID: exec.ID},
		LockKey:    &lockKey,
		TeamID:     &task.TeamID,
		ResourceID: &task.ResourceID,
	})
	if err != nil {
		return store.TaskExecution{}, err
	}
	s.Logger.Info("scheduled task enqueued", "task_id", task.ID, "job", pguuid.String(job.Uuid))
	return exec, nil
}

// recordSkip writes the occurrence that did NOT run, with its reason. An empty
// history reads exactly like "nothing was ever scheduled" — which is how a
// nightly job goes missing for a month.
func (s *Scheduler) recordSkip(ctx context.Context, task store.ScheduledTask, reason string) {
	if _, err := s.Store.CreateTaskExecution(ctx, store.CreateTaskExecutionParams{
		ScheduledTaskID: task.ID,
		Status:          store.TaskExecutionStatusSkipped,
		SkipReason:      &reason,
	}); err != nil {
		s.Logger.Warn("cannot record the skipped occurrence", "task_id", task.ID, "error", err)
	}
	s.Logger.Warn("scheduled task occurrence skipped", "task_id", task.ID, "reason", reason)
}

func (s *Scheduler) advanceTask(ctx context.Context, task store.ScheduledTask, schedule *cronexpr.Schedule, loc *time.Location, now time.Time, fired bool) error {
	next := schedule.Next(now, loc)
	params := store.SetScheduledTaskScheduleParams{ID: task.ID}
	if !next.IsZero() {
		params.NextRunAt = pgtype.Timestamptz{Time: next, Valid: true}
	}
	if fired {
		params.LastRunAt = pgtype.Timestamptz{Time: now, Valid: true}
	}
	return s.Store.SetScheduledTaskSchedule(ctx, params)
}
