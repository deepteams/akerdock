package scheduler

import (
	"context"

	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
)

// runDueDrills fires the restore drills whose interval has elapsed (ADR-014).
//
// A plan that has never been drilled is due immediately: the first drill is
// the one that tells the operator whether the backups were ever any good, and
// deferring it by a week would mean a week of green backups nobody has checked.
func (s *Scheduler) runDueDrills(ctx context.Context) {
	plans, err := s.Store.ListDrillablePlans(ctx)
	if err != nil {
		s.Logger.Warn("drill scheduling: cannot list plans", "error", err)
		return
	}
	for _, p := range plans {
		if ctx.Err() != nil {
			return
		}
		plan := p.DatabaseBackupPlan

		// A drill restores a whole database: one at a time per plan, and never
		// while a backup of the same plan is running — they would fight over
		// the dump file the retention may be purging.
		lockKey := "backup:plan:" + pguuid.String(plan.Uuid)
		active, err := s.Store.CountActiveJobsByLockKey(ctx, &lockKey)
		if err != nil {
			s.Logger.Warn("drill scheduling failed", "plan_id", plan.ID, "error", err)
			continue
		}
		if active > 0 {
			continue // next pass: the window has not moved, so nothing is lost
		}

		if _, err := queue.Enqueue(ctx, s.Store, queue.EnqueueOptions{
			Queue:      "backup",
			Type:       jobs.TypeBackupDrill,
			Payload:    jobs.BackupPayload{PlanID: plan.ID},
			LockKey:    &lockKey,
			TeamID:     &p.TeamID,
			ResourceID: plan.DatabaseID,
		}); err != nil {
			s.Logger.Warn("cannot enqueue the restore drill", "plan_id", plan.ID, "error", err)
			continue
		}
		s.Logger.Info("restore drill enqueued", "plan_id", plan.ID)
	}
}
