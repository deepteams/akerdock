package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// Dispatcher turns outbox events into deliveries (ADR-019). It runs on the
// elected scheduler leader, so its cursor needs no locking.
//
// The outbox is the single source of truth: notifications are one consumer
// among others (SSE reads the same table), which is why the cursor lives here
// and not in the outbox rows.
type Dispatcher struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Sender  *Sender
	Logger  *slog.Logger
}

// batchSize bounds one pass. A backlog is drained over several passes rather
// than in one long transaction.
const batchSize = 200

// Dispatch reads the events published since the last pass and delivers them.
// The cursor only advances over events that were fully considered: an event
// whose delivery failed keeps its row (status failed) and is not re-read, but
// an event we never got to is read again next time.
func (d *Dispatcher) Dispatch(ctx context.Context) {
	cursor, err := d.Store.GetNotificationCursor(ctx)
	if err != nil {
		d.Logger.Warn("notifications: cannot read the cursor", "error", err)
		return
	}
	events, err := d.Store.ListOutboxEventsAfter(ctx, store.ListOutboxEventsAfterParams{
		ID: cursor, Limit: batchSize,
	})
	if err != nil {
		d.Logger.Warn("notifications: cannot read the outbox", "error", err)
		return
	}

	for _, event := range events {
		if ctx.Err() != nil {
			return
		}
		if err := d.dispatchOne(ctx, event); err != nil {
			// A transient failure (the database went away) must not advance the
			// cursor past an event nobody was told about.
			d.Logger.Warn("notifications: dispatch failed", "event_id", event.ID, "error", err)
			return
		}
		if err := d.Store.SetNotificationCursor(ctx, event.ID); err != nil {
			d.Logger.Warn("notifications: cannot advance the cursor", "error", err)
			return
		}
	}
}

func (d *Dispatcher) dispatchOne(ctx context.Context, event store.OutboxEvent) error {
	if !event.TeamUuid.Valid {
		return nil // instance-level event: no team owns it, no rule matches it
	}
	// Rules can be scoped to a project or an environment: resolve where the
	// resource lives. An event about no resource stays team-wide.
	var projectID, environmentID *int64
	if event.ResourceUuid.Valid {
		if scope, err := d.Store.ResolveProjectEnvironmentOfResource(ctx, event.ResourceUuid); err == nil {
			projectID, environmentID = &scope.ProjectID, &scope.EnvironmentID
		}
	}

	rules, err := d.Store.MatchNotificationRules(ctx, store.MatchNotificationRulesParams{
		EventType:     event.EventType,
		TeamUuid:      event.TeamUuid,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if err != nil {
		return err
	}

	severity := SeverityOf(event.EventType)
	for _, rule := range rules {
		if severity < ParseSeverity(string(rule.MinSeverity)) {
			continue // below this rule's threshold: not its business
		}
		delivery, err := d.Store.CreateNotificationDelivery(ctx, store.CreateNotificationDeliveryParams{
			RuleID: rule.ID, ChannelID: rule.ChannelID, OutboxEventID: event.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT DO NOTHING returned no row: this rule already saw this
			// event. Never notify twice.
			continue
		}
		if err != nil {
			return err
		}
		d.deliver(ctx, rule, delivery, event, severity)
	}
	return nil
}

// deliver applies the noise rules, then sends. A suppressed delivery is
// recorded, never dropped: the operator must be able to see that an event was
// matched and deliberately not sent.
func (d *Dispatcher) deliver(ctx context.Context, rule store.MatchNotificationRulesRow,
	delivery store.NotificationDelivery, event store.OutboxEvent, severity Severity,
) {
	finish := func(status store.NotificationDeliveryStatus, errMsg, reason *string) {
		_ = d.Store.FinishNotificationDelivery(ctx, store.FinishNotificationDeliveryParams{
			ID: delivery.ID, Status: status, LastError: errMsg, SuppressedReason: reason,
		})
	}

	if reason := d.suppressionReason(ctx, rule, severity); reason != nil {
		finish(store.NotificationDeliveryStatusSuppressed, nil, reason)
		return
	}

	// Deferred digest (ADR-019 §4): a non-critical event on a digest rule is
	// left pending and picked up by the flush below. Nothing is lost — the
	// delivery row is the queue. A critical event never waits.
	if rule.DigestEnabled && severity < SeverityCritical {
		return
	}

	channel, err := d.Store.GetNotificationChannelByID(ctx, rule.ChannelID)
	if err != nil {
		msg := err.Error()
		finish(store.NotificationDeliveryStatusFailed, &msg, nil)
		return
	}
	cfg, err := d.config(channel)
	if err != nil {
		msg := "could not decrypt the channel configuration"
		finish(store.NotificationDeliveryStatusFailed, &msg, nil)
		return
	}

	// The debounced events this one now stands for.
	last, _ := d.Store.LastSentDelivery(ctx, rule.ID)
	suppressed, _ := d.Store.CountSuppressedSince(ctx, store.CountSuppressedSinceParams{
		RuleID: rule.ID, Since: last,
	})

	if err := d.Sender.Send(ctx, string(channel.Kind), cfg, buildEvent(event, severity, int(suppressed))); err != nil {
		msg := err.Error()
		d.Logger.Warn("notification delivery failed", "channel", channel.Name, "error", err)
		finish(store.NotificationDeliveryStatusFailed, &msg, nil)
		return
	}
	finish(store.NotificationDeliveryStatusSent, nil, nil)
}

// suppressionReason applies the two noise rules of ADR-019, in order. Both are
// deliberately bypassed by a critical event: a debounce window or a quiet hour
// must never swallow a production outage.
func (d *Dispatcher) suppressionReason(ctx context.Context, rule store.MatchNotificationRulesRow, severity Severity) *string {
	if severity == SeverityCritical {
		return nil
	}
	if rule.DebounceSeconds > 0 {
		if last, err := d.Store.LastSentDelivery(ctx, rule.ID); err == nil && last.Valid {
			if time.Since(last.Time) < time.Duration(rule.DebounceSeconds)*time.Second {
				return ptr(fmt.Sprintf("debounced: this rule notified less than %ds ago", rule.DebounceSeconds))
			}
		}
	}
	if inQuietHours(rule, time.Now()) {
		return ptr("quiet hours")
	}
	return nil
}

// inQuietHours reports whether now falls inside the rule's quiet window. A
// window that wraps around midnight (22:00 → 07:00) is the normal case, not
// the exception.
func inQuietHours(rule store.MatchNotificationRulesRow, now time.Time) bool {
	if !rule.QuietHoursStart.Valid || !rule.QuietHoursEnd.Valid {
		return false
	}
	minutes := int64(now.Hour()*60+now.Minute()) * 60 * 1_000_000
	start, end := rule.QuietHoursStart.Microseconds, rule.QuietHoursEnd.Microseconds
	if start <= end {
		return minutes >= start && minutes < end
	}
	return minutes >= start || minutes < end
}

// config decrypts the channel's configuration blob.
func (d *Dispatcher) config(channel store.NotificationChannel) (Config, error) {
	raw, err := d.Keyring.Decrypt("notification_channels", "config_enc", pguuid.String(channel.Uuid), channel.ConfigEnc)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// buildEvent renders the outbox row into what a channel receives.
func buildEvent(event store.OutboxEvent, severity Severity, suppressed int) Event {
	e := Event{
		Type:       event.EventType,
		Severity:   severity.String(),
		OccurredAt: event.OccurredAt.Time.UTC(),
		Suppressed: suppressed,
	}
	if event.TeamUuid.Valid {
		e.TeamUUID = pguuid.String(event.TeamUuid)
	}
	if event.ResourceUuid.Valid {
		e.Resource = pguuid.String(event.ResourceUuid)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err == nil {
		e.Payload = payload
	}
	return e
}

func ptr[T any](v T) *T { return &v }

// FlushDigests sends one grouped message per digest rule whose window has
// elapsed (ADR-019 §4): "what did not need to wake me up, told once".
//
// The pending deliveries ARE the queue — there is no second table to keep in
// sync, and a crash mid-flush simply leaves them pending for the next pass.
func (d *Dispatcher) FlushDigests(ctx context.Context) {
	rules, err := d.Store.ListDigestRulesDue(ctx)
	if err != nil {
		d.Logger.Warn("notifications: cannot list digest rules", "error", err)
		return
	}
	for _, rule := range rules {
		if ctx.Err() != nil {
			return
		}
		if err := d.flushDigest(ctx, rule); err != nil {
			d.Logger.Warn("notifications: digest flush failed", "rule_id", rule.ID, "error", err)
		}
	}
}

func (d *Dispatcher) flushDigest(ctx context.Context, rule store.ListDigestRulesDueRow) error {
	pending, err := d.Store.ListPendingDigestDeliveries(ctx, rule.ID)
	if err != nil || len(pending) == 0 {
		return err
	}
	channel, err := d.Store.GetNotificationChannelByID(ctx, rule.ChannelID)
	if err != nil {
		return err
	}
	cfg, err := d.config(channel)
	if err != nil {
		return err
	}

	ids := make([]int64, 0, len(pending))
	counts := map[string]int{}
	var oldest time.Time
	for _, p := range pending {
		ids = append(ids, p.ID)
		counts[p.EventType]++
		if oldest.IsZero() || p.OccurredAt.Time.Before(oldest) {
			oldest = p.OccurredAt.Time
		}
	}

	event := Event{
		Type:       "notification.digest.v1",
		Severity:   SeverityInfo.String(),
		OccurredAt: time.Now().UTC(),
		Suppressed: len(ids) - 1, // the digest stands for all of them
		Payload: map[string]any{
			"events":     counts,
			"total":      len(ids),
			"since":      oldest.UTC(),
			"rule_event": rule.EventType,
		},
	}
	if err := d.Sender.Send(ctx, string(channel.Kind), cfg, event); err != nil {
		msg := err.Error()
		_ = d.Store.MarkDigestDeliveriesFailed(ctx, store.MarkDigestDeliveriesFailedParams{
			DeliveryIds: ids, LastError: &msg,
		})
		return err
	}
	if err := d.Store.MarkDigestDeliveriesSent(ctx, ids); err != nil {
		return err
	}
	_ = d.Store.SetRuleDigestFlushed(ctx, rule.ID)
	d.Logger.Info("digest sent", "channel", channel.Name, "events", len(ids))
	return nil
}
