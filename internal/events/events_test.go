package events

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

type fakeOutboxStore struct {
	claimRows  []store.OutboxEvent
	claimErr   error
	replayRows []store.OutboxEvent
	replayErr  error
	replayArg  store.ListOutboxEventsForTeamAfterParams
}

func (f *fakeOutboxStore) ClaimUnpublishedOutboxEvents(context.Context, int32) ([]store.OutboxEvent, error) {
	return f.claimRows, f.claimErr
}

func (f *fakeOutboxStore) ListOutboxEventsForTeamAfter(_ context.Context, arg store.ListOutboxEventsForTeamAfterParams) ([]store.OutboxEvent, error) {
	f.replayArg = arg
	return f.replayRows, f.replayErr
}

const (
	teamA = "11111111-1111-4111-8111-111111111111"
	teamB = "22222222-2222-4222-8222-222222222222"
)

// mustSubscribe subscribes below the cap, where refusal is a test bug.
func mustSubscribe(t *testing.T, b *Broker, teamUUID string) (<-chan Event, func()) {
	t.Helper()
	ch, cancel, err := b.Subscribe(teamUUID)
	if err != nil {
		t.Fatalf("Subscribe(%s): %v", teamUUID, err)
	}
	return ch, cancel
}

// recv reads one event with a timeout so a broken fan-out fails fast
// instead of hanging the test binary.
func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed while an event was expected")
		}
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
	return Event{} // unreachable
}

func TestBrokerFanOut(t *testing.T) {
	b := NewBroker()
	ch1, cancel1 := mustSubscribe(t, b, teamA)
	defer cancel1()
	ch2, cancel2 := mustSubscribe(t, b, teamA)
	defer cancel2()

	want := Event{Sequence: 7, EventType: "application.deployed"}
	b.publish(teamA, want)

	for i, ch := range []<-chan Event{ch1, ch2} {
		got := recv(t, ch)
		if got.Sequence != want.Sequence || got.EventType != want.EventType {
			t.Errorf("subscriber %d: got %+v, want %+v", i, got, want)
		}
	}
}

func TestBrokerTeamIsolation(t *testing.T) {
	b := NewBroker()
	chA, cancelA := mustSubscribe(t, b, teamA)
	defer cancelA()
	chB, cancelB := mustSubscribe(t, b, teamB)
	defer cancelB()

	b.publish(teamA, Event{Sequence: 1, EventType: "server.created"})

	// Team A receives its event; team B must not see it.
	recv(t, chA)
	select {
	case ev := <-chB:
		t.Fatalf("team B received team A's event: %+v", ev)
	default:
	}
}

func TestBrokerSlowSubscriberDoesNotBlockPublish(t *testing.T) {
	b := NewBroker()
	ch, cancel := mustSubscribe(t, b, teamA)
	defer cancel()

	// Nobody reads: fill the buffer (cap 32) and keep publishing. Extra
	// events must be dropped via the default branch instead of blocking.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 100 {
			b.publish(teamA, Event{Sequence: int64(i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a slow subscriber")
	}

	// The buffered events are the first ones, in order; the rest were dropped.
	if got := recv(t, ch); got.Sequence != 0 {
		t.Errorf("first buffered event: sequence = %d, want 0", got.Sequence)
	}
	if n := len(ch); n != 31 {
		t.Errorf("buffered events left = %d, want 31 (channel capacity minus one read)", n)
	}
}

func TestBrokerCancelClosesChannelAndCleansMap(t *testing.T) {
	b := NewBroker()
	ch, cancel := mustSubscribe(t, b, teamA)
	cancel()

	if _, ok := <-ch; ok {
		t.Error("channel still open after cancel")
	}

	b.mu.RLock()
	_, teamStillThere := b.subs[teamA]
	b.mu.RUnlock()
	if teamStillThere {
		t.Error("team entry not removed from subs map after last unsubscribe")
	}

	// Publishing after cancel must not panic (send on closed channel).
	b.publish(teamA, Event{Sequence: 1})
}

func TestBrokerTeamStreamCap(t *testing.T) {
	b := NewBroker()
	cancels := make([]func(), 0, subscriberTeamCap)
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	for range subscriberTeamCap {
		_, cancel := mustSubscribe(t, b, teamA)
		cancels = append(cancels, cancel)
	}

	// Stream cap+1 is refused; another team is unaffected; freeing one slot
	// re-admits — the cap counts live streams, not a lifetime total.
	if _, _, err := b.Subscribe(teamA); !errors.Is(err, ErrTeamStreamLimit) {
		t.Fatalf("Subscribe over cap: err = %v, want ErrTeamStreamLimit", err)
	}
	_, cancelB := mustSubscribe(t, b, teamB)
	cancelB()
	cancels[0]()
	cancels = cancels[1:]
	_, cancel := mustSubscribe(t, b, teamA)
	cancels = append(cancels, cancel)
}

func TestToEvent(t *testing.T) {
	occurred := time.Date(2026, 7, 12, 10, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	resource := pguuid.MustParse("33333333-3333-4333-8333-333333333333")

	tests := []struct {
		name string
		row  store.OutboxEvent
		want Event
	}{
		{
			name: "all fields, OccurredAt converted to UTC",
			row: store.OutboxEvent{
				ID:           42,
				EventType:    "application.deployed",
				OccurredAt:   pgtype.Timestamptz{Time: occurred, Valid: true},
				ResourceUuid: resource,
				Payload:      []byte(`{"status":"ok"}`),
			},
			want: Event{
				Sequence:     42,
				EventType:    "application.deployed",
				OccurredAt:   occurred.UTC(),
				ResourceUUID: "33333333-3333-4333-8333-333333333333",
				Payload:      json.RawMessage(`{"status":"ok"}`),
			},
		},
		{
			name: "invalid OccurredAt left as zero time",
			row: store.OutboxEvent{
				ID:         7,
				EventType:  "server.created",
				OccurredAt: pgtype.Timestamptz{Time: occurred, Valid: false},
			},
			want: Event{Sequence: 7, EventType: "server.created"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toEvent(tt.row)
			if got.Sequence != tt.want.Sequence {
				t.Errorf("Sequence = %d, want %d", got.Sequence, tt.want.Sequence)
			}
			if got.EventType != tt.want.EventType {
				t.Errorf("EventType = %q, want %q", got.EventType, tt.want.EventType)
			}
			if !got.OccurredAt.Equal(tt.want.OccurredAt) {
				t.Errorf("OccurredAt = %v, want %v", got.OccurredAt, tt.want.OccurredAt)
			}
			if tt.row.OccurredAt.Valid && got.OccurredAt.Location() != time.UTC {
				t.Errorf("OccurredAt location = %v, want UTC", got.OccurredAt.Location())
			}
			if got.ResourceUUID != tt.want.ResourceUUID {
				t.Errorf("ResourceUUID = %q, want %q", got.ResourceUUID, tt.want.ResourceUUID)
			}
			if string(got.Payload) != string(tt.want.Payload) {
				t.Errorf("Payload = %q, want %q", got.Payload, tt.want.Payload)
			}
		})
	}
}

func TestPublisherDrain(t *testing.T) {
	broker := NewBroker()
	ch, cancel := mustSubscribe(t, broker, teamA)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rows := []store.OutboxEvent{{
		ID:        9,
		TeamUuid:  pguuid.MustParse(teamA),
		EventType: "server.updated",
	}}
	publisher := &Publisher{
		Store: &fakeOutboxStore{claimRows: rows}, Broker: broker, Logger: logger,
	}
	publisher.drain(context.Background())
	if got := recv(t, ch); got.Sequence != 9 || got.EventType != "server.updated" {
		t.Fatalf("published event = %+v", got)
	}

	publisher.Store = &fakeOutboxStore{claimErr: errors.New("database unavailable")}
	publisher.drain(context.Background())
	cancelled, cancelContext := context.WithCancel(context.Background())
	cancelContext()
	publisher.drain(cancelled) // a shutdown error is deliberately not logged
}

func TestPublisherRunStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	publisher := &Publisher{
		Store: &fakeOutboxStore{}, Broker: NewBroker(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	done := make(chan struct{})
	go func() {
		publisher.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publisher did not stop after cancellation")
	}
}

func TestReplay(t *testing.T) {
	row := store.OutboxEvent{
		ID:        11,
		TeamUuid:  pguuid.MustParse(teamA),
		EventType: "application.deployed",
	}
	fake := &fakeOutboxStore{replayRows: []store.OutboxEvent{row}}
	got, err := Replay(context.Background(), fake, teamA, 7, 25)
	if err != nil || len(got) != 1 || got[0].Sequence != 11 {
		t.Fatalf("Replay = %+v, %v", got, err)
	}
	if fake.replayArg.ID != 7 || fake.replayArg.Limit != 25 ||
		pguuid.String(fake.replayArg.TeamUuid) != teamA {
		t.Fatalf("replay query arguments = %+v", fake.replayArg)
	}

	fake.replayErr = errors.New("query failed")
	if _, err := Replay(context.Background(), fake, teamA, 7, 25); err == nil {
		t.Fatal("Replay should return store failures")
	}
}
