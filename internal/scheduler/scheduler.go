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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/notify"
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
	Store      *store.Queries
	Keyring    *envelope.Keyring
	Audit      *audit.Recorder
	Dispatcher *notify.Dispatcher
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
}

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
	conn, err := s.Pool.Acquire(ctx)
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
			if conn.Conn().IsClosed() {
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
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, hostKeyOf(server))
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	for _, rev := range revisions {
		path := "/data/akerdock/proxy/dynamic/" + rev.Scope + ".yaml"
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
