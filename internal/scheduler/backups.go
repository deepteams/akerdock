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

// runDueBackups fires the backup plans whose cron occurrence has passed
// (§7.1, ADR-014). Only the elected leader runs this, so the read-then-write
// on next_run_at needs no extra locking.
//
// The cron is a *trigger*, not a queue: a plan that was due while the
// instance was down does not replay every missed occurrence — it fires once
// and the window is advanced from now.
func (s *Scheduler) runDueBackups(ctx context.Context) {
	plans, err := s.Store.ListSchedulableBackupPlans(ctx)
	if err != nil {
		s.Logger.Warn("backup scheduling: cannot list plans", "error", err)
		return
	}
	now := time.Now()
	for _, plan := range plans {
		if ctx.Err() != nil {
			return
		}
		if err := s.scheduleBackupPlan(ctx, plan, now); err != nil {
			s.Logger.Warn("backup scheduling failed", "plan_id", plan.ID, "error", err)
		}
	}
}

func (s *Scheduler) scheduleBackupPlan(ctx context.Context, plan store.ListSchedulableBackupPlansRow, now time.Time) error {
	schedule, err := cronexpr.Parse(plan.CronExpression)
	if err != nil {
		// The expression was validated at the API edge; an unparseable one
		// means a hand-written row. Disabling the plan silently would hide a
		// missing backup, so it is left as-is and reported on every pass.
		return err
	}
	loc, err := time.LoadLocation(plan.Timezone)
	if err != nil {
		return err
	}

	// A plan with no window yet (freshly created, re-enabled, or rescheduled)
	// is seeded without firing: its first backup is its first occurrence.
	if !plan.NextRunAt.Valid {
		return s.advance(ctx, plan, schedule, loc, now, false)
	}
	if plan.NextRunAt.Time.After(now) {
		return nil
	}

	// One backup at a time per plan: a still-running execution (a large dump,
	// or a plan firing faster than it completes) skips this occurrence
	// instead of piling up.
	lockKey := "backup:plan:" + pguuid.String(plan.Uuid)
	active, err := s.Store.CountActiveJobsByLockKey(ctx, &lockKey)
	if err != nil {
		return err
	}
	if active > 0 {
		s.Logger.Warn("backup plan still running, skipping this occurrence", "plan_id", plan.ID)
		return s.advance(ctx, plan, schedule, loc, now, false)
	}

	job, err := queue.Enqueue(ctx, s.Store, queue.EnqueueOptions{
		Queue:      "backup",
		Type:       jobs.TypeBackupExecute,
		Payload:    jobs.BackupPayload{PlanID: plan.ID},
		LockKey:    &lockKey,
		TeamID:     &plan.TeamID,
		ResourceID: plan.DatabaseID,
	})
	if err != nil {
		return err
	}
	s.Logger.Info("scheduled backup enqueued", "plan_id", plan.ID, "job", pguuid.String(job.Uuid))
	return s.advance(ctx, plan, schedule, loc, now, true)
}

// advance moves the plan's window to the next occurrence after now. `fired`
// records the run in last_run_at.
func (s *Scheduler) advance(ctx context.Context, plan store.ListSchedulableBackupPlansRow, schedule *cronexpr.Schedule, loc *time.Location, now time.Time, fired bool) error {
	next := schedule.Next(now, loc)
	params := store.SetBackupPlanScheduleParams{ID: plan.ID}
	if !next.IsZero() {
		params.NextRunAt = pgtype.Timestamptz{Time: next, Valid: true}
	}
	if fired {
		params.LastRunAt = pgtype.Timestamptz{Time: now, Valid: true}
	}
	return s.Store.SetBackupPlanSchedule(ctx, params)
}
