package waker

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"

	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// managedLabel filters every daemon query to AkerDock's own containers: the
// helper never sees, let alone touches, anything else on the host.
const managedLabel = "akerdock.managed=true"

// stopTimeout mirrors the control plane's sleep (`docker stop -t 10`), since
// Stop exists solely to roll back a failed wake.
const stopTimeout = 10

// ContainerEvent is the slice of a Docker event the agent pushes (ADR-040):
// a state transition of an akerdock.managed container.
type ContainerEvent struct {
	Container string // container name
	Action    string // start, die, stop, oom, health_status: healthy, …
	At        time.Time
}

// RuntimeDocker serves the waker's code-limited Docker interface (§8.1) and
// the agent's observation sources from the shared runtime adapter (ADR-051).
// The restriction to start/inspect/stop plus read-only listing and events is
// enforced here, by what this type exposes — the runtime underneath can do
// more, this helper cannot.
type RuntimeDocker struct {
	rt dockerruntime.Runtime
}

// NewRuntimeDocker wraps rt — for the waker, the local-socket runtime.
func NewRuntimeDocker(rt dockerruntime.Runtime) *RuntimeDocker {
	return &RuntimeDocker{rt: rt}
}

var (
	_ Docker          = (*RuntimeDocker)(nil)
	_ eventStreamer   = (*RuntimeDocker)(nil)
	_ stateReader     = (*RuntimeDocker)(nil)
	_ containerLister = (*RuntimeDocker)(nil)
)

// Start starts a container by name or id; already running is a success.
func (d *RuntimeDocker) Start(ctx context.Context, name string) error {
	return d.rt.ContainerStart(ctx, name, container.StartOptions{})
}

// Stop stops a container — the rollback of a failed wake, the same operation
// the control plane's sleep performs, so a half-woken stack does not
// crash-loop while the control plane believes it asleep.
func (d *RuntimeDocker) Stop(ctx context.Context, name string) error {
	t := stopTimeout
	return d.rt.ContainerStop(ctx, name, container.StopOptions{Timeout: &t})
}

// Inspect reports the container's running/health state.
func (d *RuntimeDocker) Inspect(ctx context.Context, name string) (ContainerState, error) {
	resp, err := d.rt.ContainerInspect(ctx, name)
	if err != nil {
		return ContainerState{}, err
	}
	st := ContainerState{Health: "none"}
	if resp.State != nil {
		st.Running = resp.State.Running
		if resp.State.Health != nil && resp.State.Health.Status != "" {
			st.Health = resp.State.Health.Status
		}
	}
	return st, nil
}

// ListManaged returns the names of the akerdock.managed containers on this
// host, running or not — the agent's periodic resync (ADR-040): a missed or
// misread event must never leave the control plane with a stale observed
// state forever.
func (d *RuntimeDocker) ListManaged(ctx context.Context) ([]string, error) {
	list, err := d.rt.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", managedLabel)),
	})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for _, c := range list {
		if len(c.Names) > 0 { // the first name is the container's own
			names = append(names, strings.TrimPrefix(c.Names[0], "/"))
		}
	}
	return names, nil
}

// StreamEvents follows the daemon's event stream, filtered to container
// events of akerdock.managed containers, and calls handler for each until ctx
// ends or the stream breaks (the caller reconnects with backoff).
func (d *RuntimeDocker) StreamEvents(ctx context.Context, handler func(ContainerEvent)) error {
	msgs, errs := d.rt.Events(ctx, events.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("type", "container"),
			filters.Arg("label", managedLabel),
		),
	})
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errs:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !ok || err == nil {
				return io.EOF
			}
			return err
		case m := <-msgs:
			handler(ContainerEvent{
				Container: m.Actor.Attributes["name"],
				Action:    string(m.Action),
				At:        time.Unix(0, m.TimeNano),
			})
		}
	}
}
