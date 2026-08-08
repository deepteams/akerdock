package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/uptime"
)

// fakeAuditStore records the audit rows the retention sweeps write for
// crash-orphaned sessions.
type fakeAuditStore struct {
	mu       sync.Mutex
	inserted []store.InsertAuditEventParams
}

func (f *fakeAuditStore) InsertAuditEvent(_ context.Context, arg store.InsertAuditEventParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserted = append(f.inserted, arg)
	return nil
}

func (f *fakeAuditStore) ResolveAuditTargetName(context.Context, store.ResolveAuditTargetNameParams) (string, error) {
	return "", errors.New("unknown target in the fake")
}

func TestRetentionAuditPurgeAndSessionSweeps(t *testing.T) {
	db := &fakeSchedulerStore{
		ints:         map[string]int64{"purgeAudit": 3, "purgeIngress": 2},
		sweptTunnels: []store.SweepPortForwardSessionsRow{{TeamID: 1, Uuid: testUUID()}},
		sweptIngress: []store.IngressTunnelSession{{TeamID: 2, Uuid: testUUID()}},
	}
	scheduler := newScheduler(t, db)
	auditStore := &fakeAuditStore{}
	scheduler.Audit = &audit.Recorder{Store: auditStore, Logger: schedulerLogger()}
	scheduler.AuditRetentionDays = 30
	scheduler.purgeRetention(context.Background())

	if len(auditStore.inserted) != 2 {
		t.Fatalf("audit rows = %d", len(auditStore.inserted))
	}
	actions := []string{auditStore.inserted[0].Action, auditStore.inserted[1].Action}
	if actions[0] != "port-forward.close" || actions[1] != "ingress-tunnel.close" {
		t.Fatalf("audit actions = %v", actions)
	}

	failing := newScheduler(t, &fakeSchedulerStore{errs: map[string]error{
		"purgeAudit":   errors.New("x"),
		"sweepIngress": errors.New("x"),
		"purgeIngress": errors.New("x"),
	}})
	failing.AuditRetentionDays = 1
	failing.purgeRetention(context.Background())
}

func TestRunReturnsWhenCancelledAsFollower(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	scheduler := newScheduler(t, &fakeSchedulerStore{})
	scheduler.acquireLeader = func(context.Context) (leaderConnection, error) {
		// Another instance holds the lock; the caller shuts down meanwhile.
		cancel()
		return &fakeLeaderConnection{}, nil
	}
	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestTryLeadAcquiresFromThePool(t *testing.T) {
	// A real (lazy) pool exercises the production acquire path; the cancelled
	// context fails the acquire before any connection is made.
	pool, err := pgxpool.New(context.Background(), "postgres://akerdock@127.0.0.1:1/akerdock")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	scheduler := newScheduler(t, &fakeSchedulerStore{})
	scheduler.Pool = pool
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !scheduler.tryLead(ctx) {
		t.Fatal("cancelled pool acquire did not report cancellation")
	}
}

func TestLeaderServesCronTicksThenStepsDown(t *testing.T) {
	connection := &fakeLeaderConnection{acquired: true, closeAfter: 1}
	scheduler := newScheduler(t, &fakeSchedulerStore{})
	scheduler.Tick = time.Millisecond
	scheduler.acquireLeader = func(context.Context) (leaderConnection, error) {
		return connection, nil
	}
	if scheduler.tryLead(context.Background()) {
		t.Fatal("stepping down was reported as context cancellation")
	}
	if connection.execs != 1 || !connection.released {
		t.Fatalf("unlocks=%d released=%v", connection.execs, connection.released)
	}
	// The initial pass plus at least the one served cron tick.
	if dispatcher := scheduler.Dispatcher.(*fakeDispatcher); dispatcher.dispatches < 2 {
		t.Fatalf("dispatches = %d", dispatcher.dispatches)
	}
}

func TestUptimeEdgeBranches(t *testing.T) {
	t.Run("nil probe uses the real prober and unchanged state stays silent", func(t *testing.T) {
		db := &fakeSchedulerStore{}
		scheduler := newScheduler(t, db)
		check := store.UptimeCheck{
			ID: 1, Uuid: testUUID(), TeamID: 2, Name: "tcp",
			Kind: store.UptimeCheckKindTcp, Target: "127.0.0.1:1", // closed port: instant refusal
			TimeoutSeconds: 1, IntervalSeconds: 60,
			Status: store.UptimeStatusUp, FailureThreshold: 3, SuccessThreshold: 3,
		}
		scheduler.probeUptimeCheck(context.Background(), check)
		if len(db.uptimeResults) != 1 || len(db.uptimeStates) != 1 || len(db.outbox) != 0 {
			t.Fatalf("results=%d states=%d outbox=%d", len(db.uptimeResults), len(db.uptimeStates), len(db.outbox))
		}
	})
	t.Run("a fresh check establishing up alerts nobody", func(t *testing.T) {
		db := &fakeSchedulerStore{}
		scheduler := newScheduler(t, db)
		scheduler.probeUptime = func(context.Context, string, string, time.Duration) uptime.Result {
			return uptime.Result{OK: true, StatusCode: 200, LatencyMs: 5}
		}
		check := store.UptimeCheck{
			ID: 1, Uuid: testUUID(), Kind: store.UptimeCheckKindHttp, Target: "https://site.example",
			TimeoutSeconds: 1, IntervalSeconds: 60,
			Status: store.UptimeStatusUnknown, FailureThreshold: 1, SuccessThreshold: 1,
		}
		scheduler.probeUptimeCheck(context.Background(), check)
		if len(db.uptimeStates) != 1 || len(db.outbox) != 0 {
			t.Fatalf("states=%d outbox=%d", len(db.uptimeStates), len(db.outbox))
		}
	})
	t.Run("cancelled pass probes nothing", func(t *testing.T) {
		db := &fakeSchedulerStore{uptimeChecks: []store.UptimeCheck{{
			ID: 1, Kind: store.UptimeCheckKindHttp, Target: "https://site.example",
			TimeoutSeconds: 1, IntervalSeconds: 60,
		}}}
		scheduler := newScheduler(t, db)
		scheduler.probeUptime = func(context.Context, string, string, time.Duration) uptime.Result {
			t.Error("probed despite a cancelled context")
			return uptime.Result{}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		scheduler.runDueUptimeChecks(ctx)
		deadline := time.Now().Add(time.Second)
		for scheduler.uptimeInflight.Load() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if scheduler.uptimeInflight.Load() {
			t.Fatal("cancelled uptime pass did not finish")
		}
		if len(db.uptimeResults) != 0 {
			t.Fatalf("results = %#v", db.uptimeResults)
		}
	})
}

func TestCronPassCancellationAndItemFailures(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("backup plan failure logged, pass continues", func(t *testing.T) {
		db := &fakeSchedulerStore{backups: []store.ListSchedulableBackupPlansRow{{
			ID: 1, Uuid: testUUID(), CronExpression: "bad", Timezone: "UTC",
		}}}
		newScheduler(t, db).runDueBackups(context.Background())
		if len(db.enqueueArgs) != 0 {
			t.Fatal("invalid plan enqueued")
		}
	})
	t.Run("cleanup pass cancellation and item failure", func(t *testing.T) {
		bad := "bad"
		cron := "*/5 * * * *"
		db := &fakeSchedulerStore{cleanups: []store.Server{{ID: 1, Uuid: testUUID(), CleanupCron: &cron}}}
		newScheduler(t, db).runDueCleanups(cancelled)
		if len(db.cleanupSchedules) != 0 {
			t.Fatal("cancelled cleanup pass mutated state")
		}
		invalid := &fakeSchedulerStore{cleanups: []store.Server{{ID: 1, Uuid: testUUID(), CleanupCron: &bad}}}
		newScheduler(t, invalid).runDueCleanups(context.Background())
	})
	t.Run("cleanup store failures surface", func(t *testing.T) {
		cron := "*/5 * * * *"
		due := store.Server{
			ID: 1, Uuid: testUUID(), TeamID: 2, CleanupCron: &cron,
			CleanupNextRunAt: pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		}
		for _, key := range []string{"active", "enqueue"} {
			db := &fakeSchedulerStore{errs: map[string]error{key: errors.New(key)}}
			if err := newScheduler(t, db).scheduleServerCleanup(context.Background(), due, now); err == nil {
				t.Errorf("%s error hidden", key)
			}
		}
	})
	t.Run("drill pass cancellation", func(t *testing.T) {
		db := &fakeSchedulerStore{drills: []store.ListDrillablePlansRow{{
			DatabaseBackupPlan: store.DatabaseBackupPlan{ID: 1, Uuid: testUUID()},
		}}}
		newScheduler(t, db).runDueDrills(cancelled)
		if len(db.enqueueArgs) != 0 {
			t.Fatal("cancelled drill pass enqueued")
		}
	})
	t.Run("task pass cancellation and item failure", func(t *testing.T) {
		task := store.ScheduledTask{ID: 1, Uuid: testUUID(), CronExpression: "*/5 * * * *", Timezone: "UTC"}
		db := &fakeSchedulerStore{tasks: []store.ListSchedulableTasksRow{{ScheduledTask: task}}}
		newScheduler(t, db).runDueScheduledTasks(cancelled)
		if len(db.taskSchedules) != 0 {
			t.Fatal("cancelled task pass mutated state")
		}
		task.CronExpression = "bad"
		invalid := &fakeSchedulerStore{tasks: []store.ListSchedulableTasksRow{{ScheduledTask: task}}}
		newScheduler(t, invalid).runDueScheduledTasks(context.Background())
	})
	t.Run("skip record failure is swallowed", func(t *testing.T) {
		db := &fakeSchedulerStore{errs: map[string]error{"taskExecution": errors.New("x")}}
		task := store.ScheduledTask{
			ID: 1, Uuid: testUUID(), CronExpression: "*/5 * * * *", Timezone: "UTC",
			MissedRunPolicy: store.TaskMissedRunPolicySkip,
			OverlapPolicy:   store.TaskOverlapPolicyQueue,
			NextRunAt:       pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		}
		if err := newScheduler(t, db).scheduleTask(context.Background(), task, now); err != nil {
			t.Fatalf("skip-record failure surfaced: %v", err)
		}
		if len(db.taskSchedules) != 1 {
			t.Fatalf("schedule not advanced after a failed skip record: %#v", db.taskSchedules)
		}
	})
	t.Run("certificate alerts mark on success and stop on cancellation", func(t *testing.T) {
		cert := store.ListCertificatesToAlertRow{
			ID: 1, Uuid: testUUID(), ServerUuid: testUUID(), TeamUuid: testUUID(),
			MainDomain: "example.test",
			NotAfter:   pgtype.Timestamptz{Time: time.Now().Add(5 * 24 * time.Hour), Valid: true},
		}
		db := &fakeSchedulerStore{certificates: map[int32][]store.ListCertificatesToAlertRow{7: {cert}}}
		newScheduler(t, db).alertExpiringCertificates(context.Background())
		if len(db.outbox) != 1 || len(db.certificateMarks) != 1 {
			t.Fatalf("outbox=%d marks=%d", len(db.outbox), len(db.certificateMarks))
		}
		stopped := &fakeSchedulerStore{certificates: map[int32][]store.ListCertificatesToAlertRow{7: {cert}}}
		newScheduler(t, stopped).alertExpiringCertificates(cancelled)
		if len(stopped.outbox) != 0 {
			t.Fatal("cancelled certificate pass emitted events")
		}
	})
}
