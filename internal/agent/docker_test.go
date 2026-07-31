package agent

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"

	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
)

func TestRuntimeDockerStopUsesTheSleepTimeout(t *testing.T) {
	rt := &fake.Runtime{}
	d := NewRuntimeDocker(rt)
	if err := d.Stop(context.Background(), "akerdock-app"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	calls := rt.Calls()
	if len(calls) != 1 || calls[0].Method != "ContainerStop" {
		t.Fatalf("calls = %+v", calls)
	}
	opts := calls[0].Args[1].(container.StopOptions)
	if opts.Timeout == nil || *opts.Timeout != 10 {
		t.Fatalf("stop timeout = %v, want 10 (the control plane's sleep)", opts.Timeout)
	}
}

func TestRuntimeDockerInspectMapsRunningAndHealth(t *testing.T) {
	rt := &fake.Runtime{}
	d := NewRuntimeDocker(rt)

	cases := []struct {
		name  string
		state *container.State
		want  ContainerState
	}{
		{"running with healthcheck", &container.State{Running: true, Health: &container.Health{Status: "starting"}}, ContainerState{Running: true, Health: "starting"}},
		{"running without healthcheck", &container.State{Running: true}, ContainerState{Running: true, Health: "none"}},
		{"stopped", &container.State{Running: false}, ContainerState{Running: false, Health: "none"}},
		{"no state in the answer", nil, ContainerState{Running: false, Health: "none"}},
	}
	for _, tc := range cases {
		rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
			return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: tc.state}}, nil
		}
		got, err := d.Inspect(context.Background(), "c")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: state = %+v, want %+v", tc.name, got, tc.want)
		}
	}

	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return container.InspectResponse{}, errors.New("no such container")
	}
	if _, err := d.Inspect(context.Background(), "gone"); err == nil {
		t.Fatal("Inspect must surface the daemon error")
	}
}

func TestRuntimeDockerListManagedFiltersAndTrimsNames(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerListFn = func(_ context.Context, opts container.ListOptions) ([]container.Summary, error) {
		if !opts.All {
			t.Fatal("resync must list stopped containers too")
		}
		if got := opts.Filters.Get("label"); len(got) != 1 || got[0] != "akerdock.managed=true" {
			t.Fatalf("label filter = %v", got)
		}
		return []container.Summary{
			{Names: []string{"/akerdock-app", "/alias"}},
			{Names: []string{"/akerdock-db"}},
			{Names: nil},
		}, nil
	}
	d := NewRuntimeDocker(rt)
	names, err := d.ListManaged(context.Background())
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(names) != 2 || names[0] != "akerdock-app" || names[1] != "akerdock-db" {
		t.Fatalf("names = %v", names)
	}
}

func TestRuntimeDockerStreamEventsMapsAndFilters(t *testing.T) {
	msgs := make(chan events.Message, 1)
	errs := make(chan error)
	rt := &fake.Runtime{}
	rt.EventsFn = func(_ context.Context, opts events.ListOptions) (<-chan events.Message, <-chan error) {
		if got := opts.Filters.Get("type"); len(got) != 1 || got[0] != "container" {
			t.Fatalf("type filter = %v", got)
		}
		if got := opts.Filters.Get("label"); len(got) != 1 || got[0] != "akerdock.managed=true" {
			t.Fatalf("label filter = %v", got)
		}
		return msgs, errs
	}
	d := NewRuntimeDocker(rt)

	at := time.Now()
	msgs <- events.Message{
		Action:   events.Action("health_status: healthy"),
		Actor:    events.Actor{Attributes: map[string]string{"name": "akerdock-app"}},
		TimeNano: at.UnixNano(),
	}

	got := make(chan ContainerEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.StreamEvents(ctx, func(ev ContainerEvent) { got <- ev }) }()

	ev := <-got
	if ev.Container != "akerdock-app" || ev.Action != "health_status: healthy" || !ev.At.Equal(time.Unix(0, at.UnixNano())) {
		t.Fatalf("event = %+v", ev)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("after cancel: %v", err)
	}
}

func TestRuntimeDockerStreamEventsSurfacesABrokenStream(t *testing.T) {
	msgs := make(chan events.Message)
	errs := make(chan error, 1)
	rt := &fake.Runtime{}
	rt.EventsFn = func(context.Context, events.ListOptions) (<-chan events.Message, <-chan error) {
		return msgs, errs
	}
	d := NewRuntimeDocker(rt)

	errs <- io.ErrUnexpectedEOF
	err := d.StreamEvents(context.Background(), func(ContainerEvent) {})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("StreamEvents = %v, want the stream error (the agent reconnects on it)", err)
	}
}
