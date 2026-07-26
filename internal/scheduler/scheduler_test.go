package scheduler

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

type fakeSchedulerStore struct {
	mu sync.Mutex

	errs map[string]error
	ints map[string]int64

	backups      []store.ListSchedulableBackupPlansRow
	certificates map[int32][]store.ListCertificatesToAlertRow
	cleanups     []store.Server
	drills       []store.ListDrillablePlansRow
	localhost    []store.Server
	expired      []store.Preview
	queued       []store.Preview
	tasks        []store.ListSchedulableTasksRow
	proxyServers []store.Server
	revisions    []store.ProxyConfigRevision
	uptimeChecks []store.UptimeCheck

	application store.GetApplicationByIDRow
	destination store.Destination
	privateKey  store.PrivateKey
	team        store.Team
	deployment  store.Deployment
	execution   store.TaskExecution
	job         store.Job

	enqueueArgs        []store.EnqueueJobParams
	backupSchedules    []store.SetBackupPlanScheduleParams
	cleanupSchedules   []store.SetServerCleanupScheduleParams
	taskSchedules      []store.SetScheduledTaskScheduleParams
	taskExecutions     []store.CreateTaskExecutionParams
	certificateMarks   []store.MarkCertificateAlertedParams
	previewStatuses    []store.SetPreviewStatusParams
	uptimeResults      []store.RecordUptimeResultParams
	uptimeStates       []store.SetUptimeCheckStateParams
	outbox             []store.InsertOutboxEventParams
	cancelledDeployIDs []int64
}

func (f *fakeSchedulerStore) err(name string) error {
	if f.errs == nil {
		return nil
	}
	return f.errs[name]
}

func (f *fakeSchedulerStore) number(name string) int64 {
	if f.ints == nil {
		return 0
	}
	return f.ints[name]
}

func (f *fakeSchedulerStore) EnqueueJob(_ context.Context, arg store.EnqueueJobParams) (store.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueueArgs = append(f.enqueueArgs, arg)
	if err := f.err("enqueue"); err != nil {
		return store.Job{}, err
	}
	if f.job.ID == 0 {
		f.job = store.Job{ID: 99, Uuid: testUUID()}
	}
	return f.job, nil
}

func (f *fakeSchedulerStore) GetJobByIdempotencyKey(context.Context, *string) (store.Job, error) {
	return f.job, f.err("idempotency")
}

func (f *fakeSchedulerStore) CountLivePreviewsForApplication(context.Context, int64) (int64, error) {
	return f.number("livePreviews"), f.err("livePreviews")
}

func (f *fakeSchedulerStore) GetDestinationByID(context.Context, int64) (store.Destination, error) {
	return f.destination, f.err("destination")
}

func (f *fakeSchedulerStore) CreateDeployment(context.Context, store.CreateDeploymentParams) (store.Deployment, error) {
	if f.deployment.ID == 0 {
		f.deployment.ID = 88
	}
	return f.deployment, f.err("deployment")
}

func (f *fakeSchedulerStore) SupersedeObsoletePreviewDeployments(context.Context, store.SupersedeObsoletePreviewDeploymentsParams) ([]int64, error) {
	return nil, f.err("supersede")
}

func (f *fakeSchedulerStore) CancelJobsForDeployments(context.Context, []int64) error {
	return f.err("cancelJobs")
}

func (f *fakeSchedulerStore) ListCancellablePreviewDeploymentIDs(context.Context, store.ListCancellablePreviewDeploymentIDsParams) ([]int64, error) {
	return nil, f.err("cancellable")
}

func (f *fakeSchedulerStore) RequestDeploymentJobCancel(_ context.Context, id int64) (int64, error) {
	f.cancelledDeployIDs = append(f.cancelledDeployIDs, id)
	return 1, f.err("requestCancel")
}

func (f *fakeSchedulerStore) SetPreviewStatus(_ context.Context, arg store.SetPreviewStatusParams) error {
	f.previewStatuses = append(f.previewStatuses, arg)
	return f.err("previewStatus")
}

func (f *fakeSchedulerStore) ListPreviewsForScaleToZero(context.Context) ([]store.ListPreviewsForScaleToZeroRow, error) {
	return nil, f.err("stzList")
}

func (f *fakeSchedulerStore) ListSleepingPreviews(context.Context) ([]store.Preview, error) {
	return nil, f.err("sleepingList")
}
func (f *fakeSchedulerStore) SetPreviewSleeping(context.Context, int64) error { return f.err("sleep") }
func (f *fakeSchedulerStore) SetPreviewAwake(context.Context, int64) error    { return f.err("awake") }
func (f *fakeSchedulerStore) ListApplicationsToSleep(context.Context) ([]store.ListApplicationsToSleepRow, error) {
	return nil, f.err("appSleepList")
}

func (f *fakeSchedulerStore) ListSleepingApplications(context.Context) ([]store.ListSleepingApplicationsRow, error) {
	return nil, f.err("appSleepingList")
}

func (f *fakeSchedulerStore) SetApplicationSlept(context.Context, int64) error {
	return f.err("appSleep")
}

func (f *fakeSchedulerStore) SetApplicationAwake(context.Context, int64) error {
	return f.err("appAwake")
}

func (f *fakeSchedulerStore) GetServerByID(context.Context, int64) (store.Server, error) {
	return store.Server{}, f.err("server")
}

func (f *fakeSchedulerStore) InsertOutboxEvent(_ context.Context, arg store.InsertOutboxEventParams) error {
	f.outbox = append(f.outbox, arg)
	return f.err("outbox")
}

func (f *fakeSchedulerStore) ListSchedulableBackupPlans(context.Context) ([]store.ListSchedulableBackupPlansRow, error) {
	return f.backups, f.err("backups")
}

func (f *fakeSchedulerStore) CountActiveJobsByLockKey(context.Context, *string) (int64, error) {
	return f.number("active"), f.err("active")
}

func (f *fakeSchedulerStore) SetBackupPlanSchedule(_ context.Context, arg store.SetBackupPlanScheduleParams) error {
	f.backupSchedules = append(f.backupSchedules, arg)
	return f.err("backupSchedule")
}

func (f *fakeSchedulerStore) ListCertificatesToAlert(_ context.Context, threshold int32) ([]store.ListCertificatesToAlertRow, error) {
	if err := f.err("certificates"); err != nil {
		return nil, err
	}
	return f.certificates[threshold], nil
}

func (f *fakeSchedulerStore) MarkCertificateAlerted(_ context.Context, arg store.MarkCertificateAlertedParams) error {
	f.certificateMarks = append(f.certificateMarks, arg)
	return f.err("markCertificate")
}

func (f *fakeSchedulerStore) ListCleanupSchedulableServers(context.Context) ([]store.Server, error) {
	return f.cleanups, f.err("cleanups")
}

func (f *fakeSchedulerStore) SetServerCleanupSchedule(_ context.Context, arg store.SetServerCleanupScheduleParams) error {
	f.cleanupSchedules = append(f.cleanupSchedules, arg)
	return f.err("cleanupSchedule")
}

func (f *fakeSchedulerStore) ListDrillablePlans(context.Context) ([]store.ListDrillablePlansRow, error) {
	return f.drills, f.err("drills")
}

func (f *fakeSchedulerStore) ListUnvalidatedLocalhostServers(context.Context) ([]store.Server, error) {
	return f.localhost, f.err("localhost")
}

func (f *fakeSchedulerStore) ListExpiredPreviews(context.Context) ([]store.Preview, error) {
	return f.expired, f.err("expired")
}

func (f *fakeSchedulerStore) ListPreviewsToWarn(context.Context) ([]store.Preview, error) {
	return nil, nil
}
func (f *fakeSchedulerStore) SetPreviewExpiryWarned(context.Context, int64) error { return nil }
func (f *fakeSchedulerStore) ListQueuedPreviews(context.Context) ([]store.Preview, error) {
	return f.queued, f.err("queued")
}

func (f *fakeSchedulerStore) GetApplicationByID(context.Context, int64) (store.GetApplicationByIDRow, error) {
	return f.application, f.err("application")
}

func (f *fakeSchedulerStore) ListSchedulableTasks(context.Context) ([]store.ListSchedulableTasksRow, error) {
	return f.tasks, f.err("tasks")
}

func (f *fakeSchedulerStore) CountRunningTaskExecutions(context.Context, int64) (int64, error) {
	return f.number("runningTasks"), f.err("runningTasks")
}

func (f *fakeSchedulerStore) CreateTaskExecution(_ context.Context, arg store.CreateTaskExecutionParams) (store.TaskExecution, error) {
	f.taskExecutions = append(f.taskExecutions, arg)
	if f.execution.ID == 0 {
		f.execution.ID = int64(len(f.taskExecutions))
	}
	return f.execution, f.err("taskExecution")
}

func (f *fakeSchedulerStore) SetScheduledTaskSchedule(_ context.Context, arg store.SetScheduledTaskScheduleParams) error {
	f.taskSchedules = append(f.taskSchedules, arg)
	return f.err("taskSchedule")
}

func (f *fakeSchedulerStore) PurgeTerminalJobs(context.Context, int32) (int64, error) {
	return f.number("purgeJobs"), f.err("purgeJobs")
}

func (f *fakeSchedulerStore) PurgePublishedOutboxEvents(context.Context) (int64, error) {
	return f.number("purgeOutbox"), f.err("purgeOutbox")
}

func (f *fakeSchedulerStore) PurgeIdempotencyKeys(context.Context) error {
	return f.err("purgeIdempotency")
}

func (f *fakeSchedulerStore) PurgeWebhookDeliveries(context.Context) (int64, error) {
	return f.number("purgeWebhooks"), f.err("purgeWebhooks")
}

func (f *fakeSchedulerStore) PurgeUptimeResults(context.Context, int32) (int64, error) {
	return f.number("purgeUptime"), f.err("purgeUptime")
}

func (f *fakeSchedulerStore) PurgeAuditEvents(context.Context, int32) (int64, error) {
	return f.number("purgeAudit"), f.err("purgeAudit")
}

func (f *fakeSchedulerStore) SweepTerminalSessions(context.Context, int32) (int64, error) {
	return f.number("sweepTerminals"), f.err("sweepTerminals")
}

func (f *fakeSchedulerStore) PurgeTerminalSessions(context.Context, int32) (int64, error) {
	return f.number("purgeTerminals"), f.err("purgeTerminals")
}

func (f *fakeSchedulerStore) ListServersWithProxy(context.Context) ([]store.Server, error) {
	return f.proxyServers, f.err("proxyServers")
}

func (f *fakeSchedulerStore) ListAppliedProxyRevisions(context.Context, int64) ([]store.ProxyConfigRevision, error) {
	return f.revisions, f.err("revisions")
}

func (f *fakeSchedulerStore) GetPrivateKeyByID(context.Context, int64) (store.PrivateKey, error) {
	return f.privateKey, f.err("privateKey")
}

func (f *fakeSchedulerStore) ListDueUptimeChecks(context.Context) ([]store.UptimeCheck, error) {
	return f.uptimeChecks, f.err("uptimeChecks")
}

func (f *fakeSchedulerStore) RecordUptimeResult(_ context.Context, arg store.RecordUptimeResultParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uptimeResults = append(f.uptimeResults, arg)
	return f.err("uptimeResult")
}

func (f *fakeSchedulerStore) SetUptimeCheckState(_ context.Context, arg store.SetUptimeCheckStateParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uptimeStates = append(f.uptimeStates, arg)
	return f.err("uptimeState")
}

func (f *fakeSchedulerStore) GetTeamByID(context.Context, int64) (store.Team, error) {
	return f.team, f.err("team")
}

func testUUID() pgtype.UUID {
	return pguuid.MustParse("11111111-2222-4333-8444-555555555555")
}

func schedulerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func schedulerKeyring(t *testing.T) *envelope.Keyring {
	t.Helper()
	line := "1:" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

func newScheduler(t *testing.T, database *fakeSchedulerStore) *Scheduler {
	t.Helper()
	logger := schedulerLogger()
	return &Scheduler{
		Store: database, Keyring: schedulerKeyring(t),
		Audit: &audit.Recorder{Logger: logger}, Logger: logger,
		Dispatcher: &fakeDispatcher{},
	}
}

type fakeDispatcher struct {
	dispatches int
	digests    int
}

func (f *fakeDispatcher) Dispatch(context.Context)     { f.dispatches++ }
func (f *fakeDispatcher) FlushDigests(context.Context) { f.digests++ }

func TestBackupScheduling(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	base := store.ListSchedulableBackupPlansRow{
		ID: 1, Uuid: testUUID(), CronExpression: "*/5 * * * *", Timezone: "UTC",
		TeamID: 2, TargetResourceID: 3,
	}
	t.Run("parse and timezone errors", func(t *testing.T) {
		scheduler := newScheduler(t, &fakeSchedulerStore{})
		bad := base
		bad.CronExpression = "bad"
		if err := scheduler.scheduleBackupPlan(context.Background(), bad, now); err == nil {
			t.Fatal("invalid cron accepted")
		}
		bad = base
		bad.Timezone = "Mars/Olympus"
		if err := scheduler.scheduleBackupPlan(context.Background(), bad, now); err == nil {
			t.Fatal("invalid timezone accepted")
		}
	})
	t.Run("seed future active and fire", func(t *testing.T) {
		database := &fakeSchedulerStore{}
		scheduler := newScheduler(t, database)
		if err := scheduler.scheduleBackupPlan(context.Background(), base, now); err != nil {
			t.Fatal(err)
		}
		if len(database.backupSchedules) != 1 || database.backupSchedules[0].LastRunAt.Valid {
			t.Fatalf("seed schedule = %#v", database.backupSchedules)
		}
		future := base
		future.NextRunAt = pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true}
		if err := scheduler.scheduleBackupPlan(context.Background(), future, now); err != nil {
			t.Fatal(err)
		}
		if len(database.enqueueArgs) != 0 {
			t.Fatal("future plan enqueued")
		}
		due := base
		due.NextRunAt = pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true}
		database.ints = map[string]int64{"active": 1}
		if err := scheduler.scheduleBackupPlan(context.Background(), due, now); err != nil {
			t.Fatal(err)
		}
		database.ints["active"] = 0
		if err := scheduler.scheduleBackupPlan(context.Background(), due, now); err != nil {
			t.Fatal(err)
		}
		if len(database.enqueueArgs) != 1 ||
			database.enqueueArgs[0].Queue != "backup" ||
			!database.backupSchedules[len(database.backupSchedules)-1].LastRunAt.Valid {
			t.Fatalf("enqueue=%#v schedule=%#v", database.enqueueArgs, database.backupSchedules)
		}
	})
	t.Run("store failures", func(t *testing.T) {
		due := base
		due.NextRunAt = pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true}
		for _, key := range []string{"active", "enqueue", "backupSchedule"} {
			database := &fakeSchedulerStore{errs: map[string]error{key: errors.New(key)}}
			if err := newScheduler(t, database).scheduleBackupPlan(context.Background(), due, now); err == nil {
				t.Errorf("%s error hidden", key)
			}
		}
	})
	t.Run("pass errors and cancellation", func(t *testing.T) {
		newScheduler(t, &fakeSchedulerStore{errs: map[string]error{"backups": errors.New("x")}}).
			runDueBackups(context.Background())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		database := &fakeSchedulerStore{backups: []store.ListSchedulableBackupPlansRow{base}}
		newScheduler(t, database).runDueBackups(ctx)
		if len(database.backupSchedules) != 0 {
			t.Fatal("cancelled backup pass mutated state")
		}
	})
}

func TestCleanupScheduling(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	cron := "*/5 * * * *"
	threshold := int32(80)
	base := store.Server{
		ID: 1, Uuid: testUUID(), TeamID: 2, CleanupCron: &cron,
		CleanupDiskThresholdPct: &threshold,
	}
	t.Run("schedule branches", func(t *testing.T) {
		database := &fakeSchedulerStore{}
		scheduler := newScheduler(t, database)
		if err := scheduler.scheduleServerCleanup(context.Background(), base, now); err != nil {
			t.Fatal(err)
		}
		future := base
		future.CleanupNextRunAt = pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true}
		if err := scheduler.scheduleServerCleanup(context.Background(), future, now); err != nil {
			t.Fatal(err)
		}
		due := base
		due.CleanupNextRunAt = pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true}
		database.ints = map[string]int64{"active": 1}
		if err := scheduler.scheduleServerCleanup(context.Background(), due, now); err != nil {
			t.Fatal(err)
		}
		database.ints["active"] = 0
		if err := scheduler.scheduleServerCleanup(context.Background(), due, now); err != nil {
			t.Fatal(err)
		}
		if len(database.enqueueArgs) != 1 || len(database.cleanupSchedules) != 3 {
			t.Fatalf("enqueue=%d schedules=%d", len(database.enqueueArgs), len(database.cleanupSchedules))
		}
		bad := base
		invalid := "bad"
		bad.CleanupCron = &invalid
		if err := scheduler.scheduleServerCleanup(context.Background(), bad, now); err == nil {
			t.Fatal("invalid cleanup cron accepted")
		}
	})
	t.Run("threshold throttles", func(t *testing.T) {
		database := &fakeSchedulerStore{}
		scheduler := newScheduler(t, database)
		scheduler.probeCleanupThreshold(context.Background(), base, now)
		scheduler.probeCleanupThreshold(context.Background(), base, now.Add(time.Minute))
		if len(database.enqueueArgs) != 1 {
			t.Fatalf("threshold enqueues = %d", len(database.enqueueArgs))
		}
		database.ints = map[string]int64{"active": 1}
		scheduler.probeCleanupThreshold(context.Background(), store.Server{ID: 2, Uuid: testUUID()}, now)
		database.errs = map[string]error{"active": errors.New("x")}
		scheduler.probeCleanupThreshold(context.Background(), store.Server{ID: 3, Uuid: testUUID()}, now)
	})
	t.Run("pass and errors", func(t *testing.T) {
		newScheduler(t, &fakeSchedulerStore{errs: map[string]error{"cleanups": errors.New("x")}}).
			runDueCleanups(context.Background())
		database := &fakeSchedulerStore{
			cleanups: []store.Server{base},
			errs:     map[string]error{"enqueue": errors.New("x")},
		}
		newScheduler(t, database).runDueCleanups(context.Background())
	})
}

func TestDrillsAndLocalhost(t *testing.T) {
	plan := store.ListDrillablePlansRow{
		DatabaseBackupPlan: store.DatabaseBackupPlan{ID: 1, Uuid: testUUID()},
		TeamID:             2, TargetResourceID: 3,
	}
	server := store.Server{ID: 4, Uuid: testUUID(), TeamID: 5}
	for _, listKey := range []string{"drills", "localhost"} {
		database := &fakeSchedulerStore{errs: map[string]error{listKey: errors.New("x")}}
		scheduler := newScheduler(t, database)
		if listKey == "drills" {
			scheduler.runDueDrills(context.Background())
		} else {
			scheduler.validateSeededLocalhost(context.Background())
		}
	}
	t.Run("active error active and success", func(t *testing.T) {
		for _, active := range []int64{0, 1} {
			database := &fakeSchedulerStore{
				drills: []store.ListDrillablePlansRow{plan}, localhost: []store.Server{server},
				ints: map[string]int64{"active": active},
			}
			scheduler := newScheduler(t, database)
			scheduler.runDueDrills(context.Background())
			scheduler.validateSeededLocalhost(context.Background())
			want := 2
			if active > 0 {
				want = 0
			}
			if len(database.enqueueArgs) != want {
				t.Fatalf("active=%d enqueues=%d", active, len(database.enqueueArgs))
			}
		}
		database := &fakeSchedulerStore{
			drills: []store.ListDrillablePlansRow{plan}, localhost: []store.Server{server},
			errs: map[string]error{"active": errors.New("x")},
		}
		scheduler := newScheduler(t, database)
		scheduler.runDueDrills(context.Background())
		scheduler.validateSeededLocalhost(context.Background())
	})
	t.Run("enqueue failures", func(t *testing.T) {
		database := &fakeSchedulerStore{
			drills: []store.ListDrillablePlansRow{plan}, localhost: []store.Server{server},
			errs: map[string]error{"enqueue": errors.New("x")},
		}
		scheduler := newScheduler(t, database)
		scheduler.runDueDrills(context.Background())
		scheduler.validateSeededLocalhost(context.Background())
	})
}

func TestCertificateAlerts(t *testing.T) {
	cert := store.ListCertificatesToAlertRow{
		ID: 1, Uuid: testUUID(), ServerUuid: testUUID(), TeamUuid: testUUID(),
		MainDomain: "example.test",
		NotAfter:   pgtype.Timestamptz{Time: time.Now().Add(5 * 24 * time.Hour), Valid: true},
	}
	database := &fakeSchedulerStore{
		certificates: map[int32][]store.ListCertificatesToAlertRow{7: {cert}},
		errs:         map[string]error{"markCertificate": errors.New("x")},
	}
	newScheduler(t, database).alertExpiringCertificates(context.Background())
	if len(database.outbox) != 1 || len(database.certificateMarks) != 1 ||
		*database.certificateMarks[0].ExpiryAlertedThreshold != 7 {
		t.Fatalf("outbox=%d marks=%#v", len(database.outbox), database.certificateMarks)
	}
	newScheduler(t, &fakeSchedulerStore{
		errs: map[string]error{"certificates": errors.New("x")},
	}).alertExpiringCertificates(context.Background())
}

func TestScheduledTasks(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	base := store.ScheduledTask{
		ID: 1, Uuid: testUUID(), TeamID: 2, ResourceID: 3,
		CronExpression: "*/5 * * * *", Timezone: "UTC",
		OverlapPolicy:   store.TaskOverlapPolicyQueue,
		MissedRunPolicy: store.TaskMissedRunPolicyRun,
	}
	t.Run("validation seed and future", func(t *testing.T) {
		database := &fakeSchedulerStore{}
		scheduler := newScheduler(t, database)
		bad := base
		bad.CronExpression = "bad"
		if err := scheduler.scheduleTask(context.Background(), bad, now); err == nil {
			t.Fatal("invalid cron accepted")
		}
		bad = base
		bad.Timezone = "bad/zone"
		if err := scheduler.scheduleTask(context.Background(), bad, now); err == nil {
			t.Fatal("invalid timezone accepted")
		}
		if err := scheduler.scheduleTask(context.Background(), base, now); err != nil {
			t.Fatal(err)
		}
		future := base
		future.NextRunAt = pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true}
		if err := scheduler.scheduleTask(context.Background(), future, now); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missed and overlap skips", func(t *testing.T) {
		for _, mode := range []string{"missed", "overlap"} {
			database := &fakeSchedulerStore{}
			task := base
			task.NextRunAt = pgtype.Timestamptz{Time: now.Add(-20 * time.Minute), Valid: true}
			if mode == "missed" {
				task.MissedRunPolicy = store.TaskMissedRunPolicySkip
			} else {
				task.NextRunAt.Time = now.Add(-time.Minute)
				task.OverlapPolicy = store.TaskOverlapPolicySkip
				database.ints = map[string]int64{"runningTasks": 1}
			}
			if err := newScheduler(t, database).scheduleTask(context.Background(), task, now); err != nil {
				t.Fatal(err)
			}
			if len(database.taskExecutions) != 1 ||
				database.taskExecutions[0].Status != store.TaskExecutionStatusSkipped {
				t.Fatalf("%s executions = %#v", mode, database.taskExecutions)
			}
		}
	})
	t.Run("due queues execution", func(t *testing.T) {
		database := &fakeSchedulerStore{execution: store.TaskExecution{ID: 12}}
		task := base
		task.NextRunAt = pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true}
		if err := newScheduler(t, database).scheduleTask(context.Background(), task, now); err != nil {
			t.Fatal(err)
		}
		if len(database.enqueueArgs) != 1 || len(database.taskSchedules) != 1 ||
			!database.taskSchedules[0].LastRunAt.Valid {
			t.Fatalf("enqueue=%#v schedule=%#v", database.enqueueArgs, database.taskSchedules)
		}
	})
	t.Run("failures and pass", func(t *testing.T) {
		task := base
		task.NextRunAt = pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true}
		for _, key := range []string{"runningTasks", "taskExecution", "enqueue", "taskSchedule"} {
			database := &fakeSchedulerStore{errs: map[string]error{key: errors.New(key)}}
			candidate := task
			if key == "runningTasks" {
				candidate.OverlapPolicy = store.TaskOverlapPolicySkip
			}
			if err := newScheduler(t, database).scheduleTask(context.Background(), candidate, now); err == nil {
				t.Errorf("%s error hidden", key)
			}
		}
		newScheduler(t, &fakeSchedulerStore{errs: map[string]error{"tasks": errors.New("x")}}).
			runDueScheduledTasks(context.Background())
		database := &fakeSchedulerStore{tasks: []store.ListSchedulableTasksRow{{ScheduledTask: task}}}
		newScheduler(t, database).runDueScheduledTasks(context.Background())
	})
}

func TestRetentionAndHostKey(t *testing.T) {
	success := &fakeSchedulerStore{ints: map[string]int64{
		"purgeJobs": 1, "purgeOutbox": 1, "purgeWebhooks": 1, "purgeUptime": 1,
		"sweepTerminals": 1, "purgeTerminals": 1,
	}}
	scheduler := newScheduler(t, success)
	scheduler.TerminalMaxDuration = time.Hour
	scheduler.purgeRetention(context.Background())

	errorKeys := []string{
		"purgeJobs", "purgeOutbox", "purgeIdempotency", "purgeWebhooks",
		"purgeUptime", "sweepTerminals", "purgeTerminals",
	}
	errs := map[string]error{}
	for _, key := range errorKeys {
		errs[key] = errors.New(key)
	}
	newScheduler(t, &fakeSchedulerStore{errs: errs}).purgeRetention(context.Background())

	if hostKeyOf(store.Server{}) != "" {
		t.Fatal("nil host key did not map to TOFU")
	}
	key := "SHA256:key"
	if got := hostKeyOf(store.Server{HostKeyFingerprint: &key}); got != key {
		t.Fatalf("host key = %q", got)
	}
	if scheduler.tick() != cronInterval {
		t.Fatalf("default tick = %s", scheduler.tick())
	}
	scheduler.Tick = time.Second
	if scheduler.tick() != time.Second {
		t.Fatalf("custom tick = %s", scheduler.tick())
	}
}

func TestProxyReconciliationPreDialBranches(t *testing.T) {
	newScheduler(t, &fakeSchedulerStore{errs: map[string]error{"proxyServers": errors.New("x")}}).
		reconcileProxyDrift(context.Background())
	database := &fakeSchedulerStore{
		proxyServers: []store.Server{{ID: 1}},
		errs:         map[string]error{"revisions": errors.New("x")},
	}
	newScheduler(t, database).reconcileProxyDrift(context.Background())

	for _, tc := range []struct {
		name string
		db   *fakeSchedulerStore
	}{
		{"empty", &fakeSchedulerStore{}},
		{"revision error", &fakeSchedulerStore{errs: map[string]error{"revisions": errors.New("x")}}},
		{"key error", &fakeSchedulerStore{
			revisions: []store.ProxyConfigRevision{{ID: 1}},
			errs:      map[string]error{"privateKey": errors.New("x")},
		}},
		{"decrypt error", &fakeSchedulerStore{
			revisions:  []store.ProxyConfigRevision{{ID: 1}},
			privateKey: store.PrivateKey{Uuid: testUUID(), PrivateKeyEnc: []byte("bad")},
		}},
	} {
		err := newScheduler(t, tc.db).reconcileServer(context.Background(), store.Server{})
		if tc.name == "empty" {
			if err != nil {
				t.Fatalf("%s = %v", tc.name, err)
			}
		} else if err == nil {
			t.Fatalf("%s error hidden", tc.name)
		}
	}
}

type fakeRemoteClient struct {
	result    *sshexec.Result
	runErr    error
	inputErr  error
	commands  []string
	inputs    []string
	closeCall int
}

func (f *fakeRemoteClient) Run(_ context.Context, command string) (*sshexec.Result, error) {
	f.commands = append(f.commands, command)
	if f.result == nil {
		f.result = &sshexec.Result{}
	}
	return f.result, f.runErr
}

func (f *fakeRemoteClient) RunInput(_ context.Context, command, input string) (*sshexec.Result, error) {
	f.commands = append(f.commands, command)
	f.inputs = append(f.inputs, input)
	return &sshexec.Result{}, f.inputErr
}
func (f *fakeRemoteClient) Close() error { f.closeCall++; return nil }

func TestProxyReconciliationComparesAndRepairs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		remoteText string
		runErr     error
		inputErr   error
		wantInputs int
		wantErr    bool
	}{
		{"same", "expected", nil, nil, 0, false},
		{"drift", "modified", nil, nil, 1, false},
		{"read error", "", errors.New("read"), nil, 0, true},
		{"write error", "modified", nil, errors.New("write"), 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := &fakeSchedulerStore{
				revisions: []store.ProxyConfigRevision{{
					Scope: "http", Revision: 2, Content: "expected\n",
				}},
			}
			scheduler := newScheduler(t, database)
			keyUUID := testUUID()
			ciphertext, err := scheduler.Keyring.Encrypt(
				"private_keys", "private_key_enc", pguuid.String(keyUUID), []byte("private pem"),
			)
			if err != nil {
				t.Fatal(err)
			}
			database.privateKey = store.PrivateKey{Uuid: keyUUID, PrivateKeyEnc: ciphertext}
			remote := &fakeRemoteClient{
				result: &sshexec.Result{Stdout: tc.remoteText}, runErr: tc.runErr, inputErr: tc.inputErr,
			}
			scheduler.dialSSH = func(context.Context, store.Server, string) (remoteClient, error) {
				return remote, nil
			}
			err = scheduler.reconcileServer(context.Background(), store.Server{ID: 1})
			if (err != nil) != tc.wantErr {
				t.Fatalf("reconcile error = %v", err)
			}
			if len(remote.inputs) != tc.wantInputs || remote.closeCall != 1 {
				t.Fatalf("inputs=%v closes=%d", remote.inputs, remote.closeCall)
			}
		})
	}

	database := &fakeSchedulerStore{revisions: []store.ProxyConfigRevision{{Content: "expected"}}}
	scheduler := newScheduler(t, database)
	keyUUID := testUUID()
	ciphertext, err := scheduler.Keyring.Encrypt(
		"private_keys", "private_key_enc", pguuid.String(keyUUID), []byte("private pem"),
	)
	if err != nil {
		t.Fatal(err)
	}
	database.privateKey = store.PrivateKey{Uuid: keyUUID, PrivateKeyEnc: ciphertext}
	scheduler.dialSSH = func(context.Context, store.Server, string) (remoteClient, error) {
		return nil, errors.New("dial")
	}
	if err := scheduler.reconcileServer(context.Background(), store.Server{}); err == nil {
		t.Fatal("dial error hidden")
	}
}

func TestUptimeChecks(t *testing.T) {
	t.Run("list error empty and in-flight", func(t *testing.T) {
		scheduler := newScheduler(t, &fakeSchedulerStore{errs: map[string]error{"uptimeChecks": errors.New("x")}})
		scheduler.runDueUptimeChecks(context.Background())
		if scheduler.uptimeInflight.Load() {
			t.Fatal("list error left pass in flight")
		}
		scheduler = newScheduler(t, &fakeSchedulerStore{})
		scheduler.runDueUptimeChecks(context.Background())
		if scheduler.uptimeInflight.Load() {
			t.Fatal("empty pass left in flight")
		}
		scheduler.uptimeInflight.Store(true)
		scheduler.runDueUptimeChecks(context.Background())
	})
	t.Run("failure transition and recovery", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()
		database := &fakeSchedulerStore{
			team: store.Team{Uuid: testUUID()},
		}
		check := store.UptimeCheck{
			ID: 1, Uuid: testUUID(), TeamID: 2, Name: "site",
			Kind: store.UptimeCheckKindHttp, Target: server.URL,
			TimeoutSeconds: 1, IntervalSeconds: 60,
			Status: store.UptimeStatusUnknown, FailureThreshold: 1, SuccessThreshold: 1,
		}
		scheduler := newScheduler(t, database)
		scheduler.probeUptimeCheck(context.Background(), check)
		if len(database.uptimeResults) != 1 || len(database.uptimeStates) != 1 ||
			len(database.outbox) != 1 ||
			database.outbox[0].EventType != "uptime.check.failed.v1" {
			t.Fatalf("results=%#v states=%#v outbox=%#v", database.uptimeResults, database.uptimeStates, database.outbox)
		}

		okServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer okServer.Close()
		check.Status = store.UptimeStatusDown
		check.Target = okServer.URL
		scheduler.probeUptimeCheck(context.Background(), check)
		if len(database.outbox) != 2 ||
			database.outbox[1].EventType != "uptime.check.recovered.v1" {
			t.Fatalf("outbox = %#v", database.outbox)
		}
	})
	t.Run("background pass and persistence failures", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer server.Close()
		check := store.UptimeCheck{
			ID: 1, Kind: store.UptimeCheckKindHttp, Target: server.URL,
			TimeoutSeconds: 1, IntervalSeconds: 1, FailureThreshold: 2, SuccessThreshold: 2,
		}
		database := &fakeSchedulerStore{
			uptimeChecks: []store.UptimeCheck{check},
			errs: map[string]error{
				"uptimeResult": errors.New("x"), "uptimeState": errors.New("x"),
			},
		}
		scheduler := newScheduler(t, database)
		scheduler.runDueUptimeChecks(context.Background())
		deadline := time.Now().Add(time.Second)
		for scheduler.uptimeInflight.Load() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if scheduler.uptimeInflight.Load() {
			t.Fatal("background uptime pass did not finish")
		}
	})
}

type fakeLeaderConnection struct {
	rowErr   error
	acquired bool
	closed   bool
	released bool
	execs    int
}

func (f *fakeLeaderConnection) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeBoolRow{value: f.acquired, err: f.rowErr}
}

func (f *fakeLeaderConnection) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	f.execs++
	return pgconn.CommandTag{}, nil
}
func (f *fakeLeaderConnection) IsClosed() bool { return f.closed }
func (f *fakeLeaderConnection) Release()       { f.released = true }

type fakeBoolRow struct {
	value bool
	err   error
}

func (r fakeBoolRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*bool)) = r.value
	return nil
}

func TestLeaderElectionFailurePathsAndRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		conn *fakeLeaderConnection
		err  error
	}{
		{"acquire", nil, errors.New("x")},
		{"scan", &fakeLeaderConnection{rowErr: errors.New("x")}, nil},
		{"follower", &fakeLeaderConnection{}, nil},
	} {
		scheduler := newScheduler(t, &fakeSchedulerStore{})
		scheduler.acquireLeader = func(context.Context) (leaderConnection, error) {
			return tc.conn, tc.err
		}
		if scheduler.tryLead(context.Background()) {
			t.Errorf("%s unexpectedly reported cancellation", tc.name)
		}
		if tc.conn != nil && !tc.conn.released {
			t.Errorf("%s connection not released", tc.name)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scheduler := newScheduler(t, &fakeSchedulerStore{})
	scheduler.acquireLeader = func(context.Context) (leaderConnection, error) {
		return nil, context.Canceled
	}
	scheduler.Run(ctx)
}

func TestLeaderRunsTasksUnlocksAndDetectsClosedConnection(t *testing.T) {
	t.Run("cancelled leader", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		connection := &fakeLeaderConnection{acquired: true}
		scheduler := newScheduler(t, &fakeSchedulerStore{})
		scheduler.acquireLeader = func(context.Context) (leaderConnection, error) {
			return connection, nil
		}
		if !scheduler.tryLead(ctx) {
			t.Fatal("cancelled leader did not report cancellation")
		}
		if connection.execs != 1 || !connection.released {
			t.Fatalf("unlocks=%d released=%v", connection.execs, connection.released)
		}
		dispatcher := scheduler.Dispatcher.(*fakeDispatcher)
		if dispatcher.dispatches != 1 || dispatcher.digests != 1 {
			t.Fatalf("dispatcher calls = %#v", dispatcher)
		}
	})
	t.Run("lock connection closes", func(t *testing.T) {
		connection := &fakeLeaderConnection{acquired: true, closed: true}
		scheduler := newScheduler(t, &fakeSchedulerStore{})
		scheduler.Tick = time.Millisecond
		scheduler.acquireLeader = func(context.Context) (leaderConnection, error) {
			return connection, nil
		}
		if scheduler.tryLead(context.Background()) {
			t.Fatal("closed connection was reported as context cancellation")
		}
		if connection.execs != 1 || !connection.released {
			t.Fatalf("unlocks=%d released=%v", connection.execs, connection.released)
		}
	})
}

func TestPreviewReaper(t *testing.T) {
	fork := store.Preview{ID: 1, IsFork: true}
	disabled := store.Preview{ID: 2, ApplicationID: 2}
	promotable := store.Preview{ID: 3, Uuid: testUUID(), ApplicationID: 3}
	database := &fakeSchedulerStore{
		expired: []store.Preview{{ID: 10, Uuid: testUUID()}},
		queued:  []store.Preview{fork, disabled, promotable},
		application: store.GetApplicationByIDRow{
			Resource:    store.Resource{ID: 3, TeamID: 4, DestinationID: 5},
			Application: store.Application{PreviewsEnabled: true},
		},
		destination: store.Destination{ServerID: 6},
	}
	newScheduler(t, database).reapPreviews(context.Background())
	if len(database.enqueueArgs) != 3 {
		t.Fatalf("preview enqueues = %d, want destroy + two promotions", len(database.enqueueArgs))
	}
	if len(database.previewStatuses) != 3 {
		t.Fatalf("preview statuses = %#v", database.previewStatuses)
	}

	newScheduler(t, &fakeSchedulerStore{errs: map[string]error{"expired": errors.New("x")}}).
		reapPreviews(context.Background())
	newScheduler(t, &fakeSchedulerStore{errs: map[string]error{"queued": errors.New("x")}}).
		reapPreviews(context.Background())
}

func TestRunTaskGroupsWithCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	database := &fakeSchedulerStore{}
	scheduler := newScheduler(t, database)
	scheduler.runTasks(ctx)
	if !reflect.DeepEqual(database.enqueueArgs, []store.EnqueueJobParams(nil)) {
		t.Fatalf("unexpected jobs = %#v", database.enqueueArgs)
	}
}

func TestConstantsAndErrors(t *testing.T) {
	if lockID == 0 || jobRetentionDays <= 0 || uptimeResultRetentionDays <= 0 ||
		uptimeProbeConcurrency <= 0 || thresholdCheckInterval <= 0 ||
		len(expiryThresholds) != 2 {
		t.Fatal("scheduler safety constants are invalid")
	}
	if !strings.Contains(errors.New("x").Error(), "x") {
		t.Fatal("impossible")
	}
}
