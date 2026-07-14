package events

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

const (
	teamA = "11111111-1111-4111-8111-111111111111"
	teamB = "22222222-2222-4222-8222-222222222222"
)

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
	ch1, cancel1 := b.Subscribe(teamA)
	defer cancel1()
	ch2, cancel2 := b.Subscribe(teamA)
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
	chA, cancelA := b.Subscribe(teamA)
	defer cancelA()
	chB, cancelB := b.Subscribe(teamB)
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
	ch, cancel := b.Subscribe(teamA)
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
	ch, cancel := b.Subscribe(teamA)
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
