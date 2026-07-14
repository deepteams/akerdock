package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/uptime"
)

// uptimeProbeConcurrency bounds the parallel probes of one pass: enough to
// keep a fleet of checks on schedule, small enough that a wall of timeouts
// cannot exhaust the leader.
const uptimeProbeConcurrency = 16

// runDueUptimeChecks probes the checks whose window has passed (ADR-017).
// The pass runs in the background guarded by an in-flight flag: a wall of
// slow targets must delay the NEXT uptime pass, never the scheduler loop.
func (s *Scheduler) runDueUptimeChecks(ctx context.Context) {
	if !s.uptimeInflight.CompareAndSwap(false, true) {
		return
	}
	checks, err := s.Store.ListDueUptimeChecks(ctx)
	if err != nil {
		s.Logger.Warn("uptime: cannot list due checks", "error", err)
		s.uptimeInflight.Store(false)
		return
	}
	if len(checks) == 0 {
		s.uptimeInflight.Store(false)
		return
	}
	go func() {
		defer s.uptimeInflight.Store(false)
		var wg sync.WaitGroup
		sem := make(chan struct{}, uptimeProbeConcurrency)
		for _, check := range checks {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(check store.UptimeCheck) {
				defer wg.Done()
				defer func() { <-sem }()
				s.probeUptimeCheck(ctx, check)
			}(check)
		}
		wg.Wait()
	}()
}

// probeUptimeCheck runs one probe and applies the state machine. A verdict
// flip — and only a flip — publishes an event: the thresholds are the
// anti-flapping, the notifier never sees the individual probes.
func (s *Scheduler) probeUptimeCheck(ctx context.Context, check store.UptimeCheck) {
	res := uptime.Probe(ctx, string(check.Kind), check.Target,
		time.Duration(check.TimeoutSeconds)*time.Second)

	var latency, code *int32
	if res.LatencyMs >= 0 {
		latency = &res.LatencyMs
	}
	if res.StatusCode != 0 {
		code = &res.StatusCode
	}
	var probeErr *string
	if res.Error != "" {
		msg := res.Error
		probeErr = &msg
	}
	if err := s.Store.RecordUptimeResult(ctx, store.RecordUptimeResultParams{
		CheckID: check.ID, Ok: res.OK, LatencyMs: latency, StatusCode: code, Error: probeErr,
	}); err != nil {
		s.Logger.Warn("uptime: cannot record a result", "check_id", check.ID, "error", err)
	}

	state, changed := uptime.Transition(uptime.State{
		Status:               uptime.Status(check.Status),
		ConsecutiveFailures:  check.ConsecutiveFailures,
		ConsecutiveSuccesses: check.ConsecutiveSuccesses,
	}, res.OK, check.FailureThreshold, check.SuccessThreshold)

	params := store.SetUptimeCheckStateParams{
		ID:                   check.ID,
		Status:               store.UptimeStatus(state.Status),
		ConsecutiveFailures:  state.ConsecutiveFailures,
		ConsecutiveSuccesses: state.ConsecutiveSuccesses,
		LastLatencyMs:        latency,
		LastError:            probeErr,
		NextRunAt:            pgtype.Timestamptz{Time: time.Now().Add(time.Duration(check.IntervalSeconds) * time.Second), Valid: true},
	}
	if changed {
		params.StatusSince = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}
	if err := s.Store.SetUptimeCheckState(ctx, params); err != nil {
		s.Logger.Warn("uptime: cannot persist the check state", "check_id", check.ID, "error", err)
		return
	}
	if !changed {
		return
	}

	// The transition is the alert (ADR-017 → §11 channels): down is critical
	// by suffix, a recovery is routine. A fresh check establishing `up` from
	// `unknown` is neither — nothing was ever wrong, nobody gets a message.
	var event string
	switch {
	case state.Status == uptime.StatusDown:
		event = "uptime.check.failed.v1"
	case check.Status == store.UptimeStatusDown:
		event = "uptime.check.recovered.v1"
	default:
		return
	}
	payload := map[string]any{
		"check_uuid": pguuid.String(check.Uuid),
		"name":       check.Name,
		"kind":       string(check.Kind),
		"target":     check.Target,
		"status":     string(state.Status),
	}
	if probeErr != nil {
		payload["reason"] = *probeErr
	}
	var teamUUID pgtype.UUID
	if team, err := s.Store.GetTeamByID(ctx, check.TeamID); err == nil {
		teamUUID = team.Uuid
	}
	s.Audit.Outbox(ctx, s.Store, event, teamUUID, check.Uuid,
		"uptime_check:"+pguuid.String(check.Uuid), payload)
	s.Logger.Info("uptime check transitioned", "check", check.Name, "status", string(state.Status))
}
