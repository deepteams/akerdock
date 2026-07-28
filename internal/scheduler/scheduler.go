// Package scheduler runs the instance maintenance crons (§18.2): retention
// purges and proxy drift reconciliation. Several scheduler processes may
// run concurrently — a PostgreSQL advisory lock elects a single active one
// (instance-config §2.1), so a task never runs twice at the same time.
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/terminal"
)

// lockID identifies the scheduler election lock among advisory locks.
const lockID int64 = 0x414B4552 // "AKER"

// Retention windows (§19.2, §22.2).
const jobRetentionDays = 30

// uptimeResultRetentionDays bounds the raw probe history (ADR-017).
const uptimeResultRetentionDays = 30

// Scheduler runs the maintenance tasks of the instance.
type Scheduler struct {
	// Tick is how often due work is looked for (cron backups, expiring
	// certificates, notifications). Zero falls back to the default.
	Tick       time.Duration
	Pool       *pgxpool.Pool
	Store      Store
	Keyring    *envelope.Keyring
	Audit      *audit.Recorder
	Dispatcher NotificationDispatcher
	Logger     *slog.Logger
	// thresholdProbes throttles the §3.7 disk probes (leader-local state:
	// only the elected leader schedules).
	thresholdProbes map[int64]time.Time
	// uptimeInflight prevents uptime passes from piling up when targets are
	// slow (ADR-017): the pass runs in the background, one at a time.
	uptimeInflight atomic.Bool
	// TerminalMaxDuration bounds the crash-net sweep of terminal sessions
	// (§24.4): a session row still open past this ceiling can only be a
	// control-plane restart — sessions live in-process. Zero falls back to
	// the default.
	TerminalMaxDuration time.Duration
	// AuditRetentionDays bounds how long audit rows are kept (§23.4). Zero (the
	// default) keeps everything; a positive value enables the daily purge of
	// aged-out rows through the sanctioned append-only bypass.
	AuditRetentionDays int
	// InstancePort is the control plane's port, used to build the localhost
	// server's agent push URL through the Docker host gateway (ADR-040).
	InstancePort int
	// WakerImage is this release's own image (AKERDOCK_IMAGE / baked default):
	// the scale-to-zero pass recreates any waker whose running image differs, so
	// an upgrade propagates to every server's waker without waiting for a deploy
	// (ADR-036). Empty disables the reconciliation.
	WakerImage string

	acquireLeader func(context.Context) (leaderConnection, error)
	dialSSH       func(context.Context, store.Server, string) (remoteClient, error)
}

// NotificationDispatcher drains the outbox and flushes pending digests on the
// scheduler's tick; the concrete implementation lives in the notify package.
type NotificationDispatcher interface {
	Dispatch(context.Context)
	FlushDigests(context.Context)
}

type remoteClient interface {
	Run(context.Context, string) (*sshexec.Result, error)
	RunInput(context.Context, string, string) (*sshexec.Result, error)
	Close() error
}

// Store is the scheduler's database boundary. Scheduling decisions
// are ordinary unit-testable state machines; only advisory-lock and SQL
// concurrency guarantees need PostgreSQL module tests.
type Store interface {
	// Agent enrollment (ADR-040): the reconcile re-injects the per-server
	// token whenever it recreates the helper.
	jobs.AgentEnrollmentStore
	jobs.PreviewPromotionStore
	audit.OutboxStore

	ListSchedulableBackupPlans(context.Context) ([]store.ListSchedulableBackupPlansRow, error)
	CountActiveJobsByLockKey(context.Context, *string) (int64, error)
	SetBackupPlanSchedule(context.Context, store.SetBackupPlanScheduleParams) error
	ListCertificatesToAlert(context.Context, int32) ([]store.ListCertificatesToAlertRow, error)
	MarkCertificateAlerted(context.Context, store.MarkCertificateAlertedParams) error
	ListCleanupSchedulableServers(context.Context) ([]store.Server, error)
	SetServerCleanupSchedule(context.Context, store.SetServerCleanupScheduleParams) error
	ListDrillablePlans(context.Context) ([]store.ListDrillablePlansRow, error)
	ListUnvalidatedLocalhostServers(context.Context) ([]store.Server, error)
	ListExpiredPreviews(context.Context) ([]store.Preview, error)
	ListPreviewsToWarn(context.Context) ([]store.Preview, error)
	SetPreviewExpiryWarned(context.Context, int64) error
	ListQueuedPreviews(context.Context) ([]store.Preview, error)
	ListPreviewsForScaleToZero(context.Context) ([]store.ListPreviewsForScaleToZeroRow, error)
	ListSleepingPreviews(context.Context) ([]store.Preview, error)
	SetPreviewSleeping(context.Context, int64) error
	SetPreviewAwake(context.Context, int64) error
	ListApplicationsToSleep(context.Context) ([]store.ListApplicationsToSleepRow, error)
	ListSleepingApplications(context.Context) ([]store.ListSleepingApplicationsRow, error)
	SetApplicationSlept(context.Context, int64) error
	SetApplicationAwake(context.Context, int64) error
	GetDestinationByID(context.Context, int64) (store.Destination, error)
	GetServerByID(context.Context, int64) (store.Server, error)
	GetApplicationByID(context.Context, int64) (store.GetApplicationByIDRow, error)
	ListSchedulableTasks(context.Context) ([]store.ListSchedulableTasksRow, error)
	CountRunningTaskExecutions(context.Context, int64) (int64, error)
	CreateTaskExecution(context.Context, store.CreateTaskExecutionParams) (store.TaskExecution, error)
	SetScheduledTaskSchedule(context.Context, store.SetScheduledTaskScheduleParams) error
	PurgeTerminalJobs(context.Context, int32) (int64, error)
	PurgePublishedOutboxEvents(context.Context) (int64, error)
	PurgeIdempotencyKeys(context.Context) error
	PurgeWebhookDeliveries(context.Context) (int64, error)
	PurgeUptimeResults(context.Context, int32) (int64, error)
	PurgeAuditEvents(context.Context, int32) (int64, error)
	SweepTerminalSessions(context.Context, int32) (int64, error)
	PurgeTerminalSessions(context.Context, int32) (int64, error)
	ListServersWithProxy(context.Context) ([]store.Server, error)
	ListAppliedProxyRevisions(context.Context, int64) ([]store.ProxyConfigRevision, error)
	GetPrivateKeyByID(context.Context, int64) (store.PrivateKey, error)
	ListDueUptimeChecks(context.Context) ([]store.UptimeCheck, error)
	RecordUptimeResult(context.Context, store.RecordUptimeResultParams) error
	SetUptimeCheckState(context.Context, store.SetUptimeCheckStateParams) error
	GetTeamByID(context.Context, int64) (store.Team, error)
}

type leaderConnection interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	IsClosed() bool
	Release()
}

type poolLeaderConnection struct{ connection *pgxpool.Conn }

func (c poolLeaderConnection) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return c.connection.QueryRow(ctx, sql, args...)
}

func (c poolLeaderConnection) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return c.connection.Exec(ctx, sql, args...)
}
func (c poolLeaderConnection) IsClosed() bool { return c.connection.Conn().IsClosed() }
func (c poolLeaderConnection) Release()       { c.connection.Release() }

// Run elects a leader and runs the maintenance loop until ctx is cancelled.
// Followers keep trying to acquire the lock, so a leader crash is picked up
// by another instance.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if s.tryLead(ctx) {
			return // ctx cancelled while leading
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tryLead acquires the advisory lock on a dedicated connection (the lock is
// held for as long as that connection lives) and runs the tasks. It returns
// true when ctx was cancelled.
func (s *Scheduler) tryLead(ctx context.Context) bool {
	var conn leaderConnection
	var err error
	if s.acquireLeader != nil {
		conn, err = s.acquireLeader(ctx)
	} else {
		var pooled *pgxpool.Conn
		pooled, err = s.Pool.Acquire(ctx)
		if err == nil {
			conn = poolLeaderConnection{connection: pooled}
		}
	}
	if err != nil {
		return ctx.Err() != nil
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockID).Scan(&acquired); err != nil || !acquired {
		return false // another instance leads
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockID)
	}()

	s.Logger.Info("scheduler elected leader")
	// Two cadences: maintenance is coarse, but backup plans are cron
	// expressions that can fire every minute — they need a fine tick.
	maintenance := time.NewTicker(maintenanceInterval)
	defer maintenance.Stop()
	cron := time.NewTicker(s.tick())
	defer cron.Stop()

	s.runTasks(ctx)
	s.runCronTasks(ctx)
	for {
		select {
		case <-ctx.Done():
			return true
		case <-cron.C:
			// Losing the connection loses the lock: another instance takes over.
			if conn.IsClosed() {
				s.Logger.Warn("scheduler lost its lock connection, stepping down")
				return false
			}
			s.runCronTasks(ctx)
		case <-maintenance.C:
			s.runTasks(ctx)
		}
	}
}

// Tick cadences of the leader loop.
const (
	maintenanceInterval = 5 * time.Minute
	cronInterval        = 30 * time.Second
)

func (s *Scheduler) tick() time.Duration {
	if s.Tick > 0 {
		return s.Tick
	}
	return cronInterval
}

// runCronTasks is the fine tick: what must not wait five minutes. The expiry
// scan rides it because it is an indexed read that alerts at most once per
// threshold — and an expiring certificate announced late is an outage
// announced late.
func (s *Scheduler) runCronTasks(ctx context.Context) {
	s.runDueBackups(ctx)
	s.runDueScheduledTasks(ctx)
	s.runDueDrills(ctx)
	s.runDueCleanups(ctx)
	s.runDueUptimeChecks(ctx)
	s.alertExpiringCertificates(ctx)
	s.Dispatcher.Dispatch(ctx)
	s.Dispatcher.FlushDigests(ctx)
}

func (s *Scheduler) runTasks(ctx context.Context) {
	s.purgeRetention(ctx)
	s.reconcileProxyDrift(ctx)
	s.validateSeededLocalhost(ctx)
	s.reapPreviews(ctx)
	s.scaleZeroPreviews(ctx)
	s.scaleZeroApplications(ctx)
}

// purgeRetention drops expired history: terminal jobs, published outbox
// events and idempotency keys (§19.2). dead_letter jobs are never purged —
// they wait for an operator (§21.3).
func (s *Scheduler) purgeRetention(ctx context.Context) {
	if n, err := s.Store.PurgeTerminalJobs(ctx, jobRetentionDays); err != nil {
		s.Logger.Warn("job retention purge failed", "error", err)
	} else if n > 0 {
		s.Logger.Info("purged terminal jobs", "count", n)
	}
	if n, err := s.Store.PurgePublishedOutboxEvents(ctx); err != nil {
		s.Logger.Warn("outbox purge failed", "error", err)
	} else if n > 0 {
		s.Logger.Info("purged published outbox events", "count", n)
	}
	if err := s.Store.PurgeIdempotencyKeys(ctx); err != nil {
		s.Logger.Warn("idempotency key purge failed", "error", err)
	}
	// Webhook deliveries bound the dedup window (INV-009): purging them earlier
	// than 30 days would reopen the replay window.
	if n, err := s.Store.PurgeWebhookDeliveries(ctx); err != nil {
		s.Logger.Warn("webhook delivery purge failed", "error", err)
	} else if n > 0 {
		s.Logger.Info("purged webhook deliveries", "count", n)
	}
	// Raw uptime probe results are bounded history (ADR-017); the check row
	// keeps the current verdict forever.
	if n, err := s.Store.PurgeUptimeResults(ctx, uptimeResultRetentionDays); err != nil {
		s.Logger.Warn("uptime result purge failed", "error", err)
	} else if n > 0 {
		s.Logger.Info("purged uptime results", "count", n)
	}
	// Audit trail: opt-in retention (§23.4). Zero keeps everything; a positive
	// value purges aged-out rows through the sanctioned append-only bypass.
	if s.AuditRetentionDays > 0 {
		if n, err := s.Store.PurgeAuditEvents(ctx, int32(s.AuditRetentionDays)); err != nil {
			s.Logger.Warn("audit retention purge failed", "error", err)
		} else if n > 0 {
			s.Logger.Info("purged audit events", "count", n, "retention_days", s.AuditRetentionDays)
		}
	}
	// Terminal sessions: sweep rows orphaned by a control-plane crash (the
	// sessions themselves live in-process), then purge the ended history.
	maxDuration := s.TerminalMaxDuration
	if maxDuration <= 0 {
		maxDuration = terminal.DefaultMaxDuration
	}
	if n, err := s.Store.SweepTerminalSessions(ctx, int32(maxDuration.Seconds())); err != nil {
		s.Logger.Warn("terminal session sweep failed", "error", err)
	} else if n > 0 {
		s.Logger.Info("swept orphaned terminal sessions", "count", n)
	}
	if n, err := s.Store.PurgeTerminalSessions(ctx, jobRetentionDays); err != nil {
		s.Logger.Warn("terminal session purge failed", "error", err)
	} else if n > 0 {
		s.Logger.Info("purged terminal sessions", "count", n)
	}
}

// reconcileProxyDrift compares the checksum of every remote routing file
// with its last applied revision and re-applies the expected content on
// drift — a manual edit is detected and corrected (§6.2.4, §18.3).
func (s *Scheduler) reconcileProxyDrift(ctx context.Context) {
	servers, err := s.Store.ListServersWithProxy(ctx)
	if err != nil {
		s.Logger.Warn("drift reconciliation: cannot list servers", "error", err)
		return
	}
	for _, server := range servers {
		if err := s.reconcileServer(ctx, server); err != nil && ctx.Err() == nil {
			s.Logger.Warn("drift reconciliation failed", "server_id", server.ID, "error", err)
		}
	}
}

func (s *Scheduler) reconcileServer(ctx context.Context, server store.Server) error {
	revisions, err := s.Store.ListAppliedProxyRevisions(ctx, server.ID)
	if err != nil || len(revisions) == 0 {
		return err
	}
	key, err := s.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return err
	}
	pem, err := s.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return err
	}
	var client remoteClient
	if s.dialSSH != nil {
		client, err = s.dialSSH(ctx, server, string(pem))
	} else {
		client, err = sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
			time.Duration(server.SshTimeoutSeconds)*time.Second, hostKeyOf(server))
	}
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	for _, rev := range revisions {
		path := "/var/lib/akerdock/proxy/dynamic/" + rev.Scope + ".yaml"
		res, err := client.Run(ctx, "cat "+path+" 2>/dev/null || true")
		if err != nil {
			return err
		}
		// The stored checksum is the reference (§6.2.4); trailing newlines
		// are normalized because the shell round-trip may drop the last one.
		sum := sha256.Sum256([]byte(strings.TrimRight(res.Stdout, "\n") + "\n"))
		expected := sha256.Sum256([]byte(strings.TrimRight(rev.Content, "\n") + "\n"))
		if hex.EncodeToString(sum[:]) == hex.EncodeToString(expected[:]) {
			continue
		}
		s.Logger.Warn("proxy config drift detected — re-applying the expected revision",
			"server_id", server.ID, "scope", rev.Scope, "revision", rev.Revision)
		if _, err := client.RunInput(ctx,
			"umask 077 && cat > "+path+".tmp && mv -f "+path+".tmp "+path, rev.Content); err != nil {
			return err
		}
	}
	return nil
}
