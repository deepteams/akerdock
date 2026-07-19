// Package events publishes the transactional outbox (§18.2, §24.2) and
// fans the published events out to the SSE subscribers of each team
// (ADR-024). Events are facts: they are published only after their
// mutation committed, and referenced by public UUID — never a secret.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// Event is one published domain event, as seen by an SSE subscriber.
type Event struct {
	Sequence     int64           `json:"sequence"`
	EventType    string          `json:"event_type"`
	OccurredAt   time.Time       `json:"occurred_at"`
	ResourceUUID string          `json:"resource_uuid,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

// Broker fans out published events to the live subscribers of a team.
// Subscribers that fall behind are dropped rather than slowing the
// publisher: they resume from their Last-Event-ID on reconnect.
type Broker struct {
	mu   sync.RWMutex
	subs map[string]map[chan Event]struct{} // team uuid → subscribers
}

// NewBroker builds an empty broker.
func NewBroker() *Broker {
	return &Broker{subs: map[string]map[chan Event]struct{}{}}
}

// Subscribe registers a subscriber for a team; the returned cancel func
// must be called when the stream ends.
func (b *Broker) Subscribe(teamUUID string) (<-chan Event, func()) {
	ch := make(chan Event, 32)
	b.mu.Lock()
	if b.subs[teamUUID] == nil {
		b.subs[teamUUID] = map[chan Event]struct{}{}
	}
	b.subs[teamUUID][ch] = struct{}{}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		delete(b.subs[teamUUID], ch)
		if len(b.subs[teamUUID]) == 0 {
			delete(b.subs, teamUUID)
		}
		b.mu.Unlock()
		close(ch)
	}
}

func (b *Broker) publish(teamUUID string, ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs[teamUUID] {
		select {
		case ch <- ev:
		default: // slow subscriber: drop, it resumes via Last-Event-ID
		}
	}
}

// Publisher drains the outbox in commit order and feeds the broker. Several
// instances may run concurrently: the claim uses FOR UPDATE SKIP LOCKED, so
// each event is published exactly once.
type Publisher struct {
	Store  OutboxStore
	Broker *Broker
	Logger *slog.Logger
}

// OutboxStore is the small generated-query surface the event subsystem owns.
// Keeping the boundary explicit lets the fan-out and replay semantics be
// tested without booting PostgreSQL; the SQL locking itself stays covered by
// the store module tests.
type OutboxStore interface {
	ClaimUnpublishedOutboxEvents(context.Context, int32) ([]store.OutboxEvent, error)
	ListOutboxEventsForTeamAfter(context.Context, store.ListOutboxEventsForTeamAfterParams) ([]store.OutboxEvent, error)
}

const (
	publishBatch    = 100
	publishInterval = 500 * time.Millisecond
)

// Run drains the outbox until ctx is cancelled.
func (p *Publisher) Run(ctx context.Context) {
	ticker := time.NewTicker(publishInterval)
	defer ticker.Stop()
	p.Logger.Info("outbox publisher started")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.drain(ctx)
		}
	}
}

func (p *Publisher) drain(ctx context.Context) {
	rows, err := p.Store.ClaimUnpublishedOutboxEvents(ctx, publishBatch)
	if err != nil {
		if ctx.Err() == nil {
			p.Logger.Warn("outbox claim failed", "error", err)
		}
		return
	}
	for _, row := range rows {
		p.Broker.publish(pguuid.String(row.TeamUuid), toEvent(row))
	}
}

func toEvent(row store.OutboxEvent) Event {
	ev := Event{
		Sequence:     row.ID,
		EventType:    row.EventType,
		ResourceUUID: pguuid.String(row.ResourceUuid),
		Payload:      json.RawMessage(row.Payload),
	}
	if row.OccurredAt.Valid {
		ev.OccurredAt = row.OccurredAt.Time.UTC()
	}
	return ev
}

// Replay returns the events a reconnecting subscriber missed, in order
// (Last-Event-ID resume, ADR-024).
func Replay(ctx context.Context, q OutboxStore, teamUUID string, after int64, limit int32) ([]Event, error) {
	u := pguuid.MustParse(teamUUID)
	rows, err := q.ListOutboxEventsForTeamAfter(ctx, store.ListOutboxEventsForTeamAfterParams{
		TeamUuid: u, ID: after, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		out = append(out, toEvent(row))
	}
	return out, nil
}
