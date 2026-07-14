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

// thresholdCheckInterval throttles the disk-threshold probe (§3.7): the
// check is one SSH `df` per server, run by the cleanup job itself — the
// scheduler only decides how often that probe is worth a job.
const thresholdCheckInterval = 15 * time.Minute

// runDueCleanups fires the §3.7 cleanups: the cron window exactly like the
// backup plans (seeded without firing, advanced from now, never replaying
// missed occurrences), plus a throttled threshold probe for servers that
// configured one. The safety rules (INV-015, never during a deployment)
// live in the job — the scheduler only decides WHEN.
func (s *Scheduler) runDueCleanups(ctx context.Context) {
	servers, err := s.Store.ListCleanupSchedulableServers(ctx)
	if err != nil {
		s.Logger.Warn("cleanup scheduling: cannot list servers", "error", err)
		return
	}
	now := time.Now()
	for _, server := range servers {
		if ctx.Err() != nil {
			return
		}
		if server.CleanupCron != nil && *server.CleanupCron != "" {
			if err := s.scheduleServerCleanup(ctx, server, now); err != nil {
				s.Logger.Warn("cleanup scheduling failed", "server_id", server.ID, "error", err)
			}
		}
		if server.CleanupDiskThresholdPct != nil {
			s.probeCleanupThreshold(ctx, server, now)
		}
	}
}

func (s *Scheduler) scheduleServerCleanup(ctx context.Context, server store.Server, now time.Time) error {
	schedule, err := cronexpr.Parse(*server.CleanupCron)
	if err != nil {
		// Validated at the API edge; an unparseable row is reported on every
		// pass rather than silently disabled.
		return err
	}

	// No window yet (freshly enabled or rescheduled): seed without firing.
	if !server.CleanupNextRunAt.Valid {
		return s.advanceCleanup(ctx, server.ID, schedule, now)
	}
	if server.CleanupNextRunAt.Time.After(now) {
		return nil
	}

	lockKey := "server:cleanup:" + pguuid.String(server.Uuid)
	active, err := s.Store.CountActiveJobsByLockKey(ctx, &lockKey)
	if err != nil {
		return err
	}
	if active > 0 {
		return s.advanceCleanup(ctx, server.ID, schedule, now)
	}
	if _, err := queue.Enqueue(ctx, s.Store, queue.EnqueueOptions{
		Queue:   "cleanup",
		Type:    jobs.TypeServerCleanup,
		Payload: jobs.ServerCleanupPayload{ServerID: server.ID, Reason: "cron"},
		LockKey: &lockKey,
		TeamID:  &server.TeamID,
	}); err != nil {
		return err
	}
	s.Logger.Info("scheduled cleanup enqueued", "server_id", server.ID)
	return s.advanceCleanup(ctx, server.ID, schedule, now)
}

func (s *Scheduler) advanceCleanup(ctx context.Context, serverID int64, schedule *cronexpr.Schedule, now time.Time) error {
	next := schedule.Next(now, time.UTC)
	params := store.SetServerCleanupScheduleParams{ID: serverID}
	if !next.IsZero() {
		params.NextRunAt = pgtype.Timestamptz{Time: next, Valid: true}
	}
	return s.Store.SetServerCleanupSchedule(ctx, params)
}

// probeCleanupThreshold enqueues a threshold-reason cleanup job — the job
// measures the disk itself and exits without pruning below the limit. The
// in-memory throttle is enough: only the elected leader runs this.
func (s *Scheduler) probeCleanupThreshold(ctx context.Context, server store.Server, now time.Time) {
	if s.thresholdProbes == nil {
		s.thresholdProbes = map[int64]time.Time{}
	}
	if last, ok := s.thresholdProbes[server.ID]; ok && now.Sub(last) < thresholdCheckInterval {
		return
	}
	lockKey := "server:cleanup:" + pguuid.String(server.Uuid)
	if active, err := s.Store.CountActiveJobsByLockKey(ctx, &lockKey); err != nil || active > 0 {
		return
	}
	if _, err := queue.Enqueue(ctx, s.Store, queue.EnqueueOptions{
		Queue:   "cleanup",
		Type:    jobs.TypeServerCleanup,
		Payload: jobs.ServerCleanupPayload{ServerID: server.ID, Reason: "threshold"},
		LockKey: &lockKey,
		TeamID:  &server.TeamID,
	}); err != nil {
		s.Logger.Warn("cannot enqueue the threshold cleanup probe", "server_id", server.ID, "error", err)
		return
	}
	s.thresholdProbes[server.ID] = now
}
