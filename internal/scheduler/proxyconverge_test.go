package scheduler

import (
	"context"
	"fmt"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/store"
)

// stubRuntimeSource hands every caller the same runtime, or the mandatory-agent
// failure when none is set.
type stubRuntimeSource struct{ rt dockerruntime.Runtime }

func (s stubRuntimeSource) Runtime(context.Context, int64) (dockerruntime.Runtime, error) {
	if s.rt == nil {
		return nil, agentwire.Unavailable("not connected")
	}
	return s.rt, nil
}

func proxyRuntime(state *container.State, err error) *fake.Runtime {
	return &fake.Runtime{ContainerInspectFn: func(context.Context, string) (container.InspectResponse, error) {
		if err != nil {
			return container.InspectResponse{}, err
		}
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: state}}, nil
	}}
}

func proxyServer(desired store.ProxyDesiredState) store.Server {
	return store.Server{ID: 7, TeamID: 3, ProxyType: store.ProxyTypeTraefik, ProxyDesiredState: desired}
}

func notFound() error { return fmt.Errorf("inspect akerdock-proxy: %w", cerrdefs.ErrNotFound) }

// The decision table: what the intent says, crossed with what the server shows.
func TestProxyConvergenceDecisionTable(t *testing.T) {
	running := &container.State{Running: true, Status: "running"}
	stopped := &container.State{Running: false, Status: "exited"}

	for _, tc := range []struct {
		name     string
		server   store.Server
		runtime  dockerruntime.Runtime
		wantJobs int
	}{
		{"running intent, proxy up — nothing to do", proxyServer(store.ProxyDesiredStateRunning), proxyRuntime(running, nil), 0},
		{"running intent, proxy stopped — converge", proxyServer(store.ProxyDesiredStateRunning), proxyRuntime(stopped, nil), 1},
		{"running intent, container gone — converge", proxyServer(store.ProxyDesiredStateRunning), proxyRuntime(nil, notFound()), 1},
		// The operator's explicit stop is never repaired — the whole difference
		// between converging toward an intent and overriding one.
		{"stopped intent, proxy down — left alone", proxyServer(store.ProxyDesiredStateStopped), proxyRuntime(stopped, nil), 0},
		{"no managed proxy — left alone", store.Server{ID: 7, ProxyType: store.ProxyTypeNone}, proxyRuntime(stopped, nil), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := &fakeSchedulerStore{}
			s := newScheduler(t, database)
			s.Docker = stubRuntimeSource{rt: tc.runtime}
			if err := s.convergeProxyContainer(context.Background(), tc.server, time.Now()); err != nil {
				t.Fatalf("converge: %v", err)
			}
			if got := len(database.enqueueArgs); got != tc.wantJobs {
				t.Fatalf("enqueued %d jobs, want %d", got, tc.wantJobs)
			}
			if tc.wantJobs > 0 && database.enqueueArgs[0].JobType != jobs.TypeProxyStart {
				t.Fatalf("enqueued %q, want %q", database.enqueueArgs[0].JobType, jobs.TypeProxyStart)
			}
		})
	}
}

// An unreachable agent is not a convergence failure: there is nothing to act
// on, and the agent's own health is another pass's business.
func TestProxyConvergenceIgnoresAnUnreachableAgent(t *testing.T) {
	database := &fakeSchedulerStore{}
	s := newScheduler(t, database)
	s.Docker = stubRuntimeSource{}
	if err := s.convergeProxyContainer(context.Background(), proxyServer(store.ProxyDesiredStateRunning), time.Now()); err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(database.enqueueArgs) != 0 {
		t.Fatal("no job can be enqueued for a server we cannot even observe")
	}
}

// A proxy action already in flight owns the container: the converger must not
// enqueue a second one behind the same lock.
func TestProxyConvergenceYieldsToAnActionInFlight(t *testing.T) {
	database := &fakeSchedulerStore{ints: map[string]int64{"active": 1}}
	s := newScheduler(t, database)
	s.Docker = stubRuntimeSource{rt: proxyRuntime(&container.State{}, nil)}
	if err := s.convergeProxyContainer(context.Background(), proxyServer(store.ProxyDesiredStateRunning), time.Now()); err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(database.enqueueArgs) != 0 {
		t.Fatal("converged behind an operator's own proxy action")
	}
}

// Backoff, then a visible verdict: a proxy that cannot come back must appear
// unhealthy in the dashboard rather than be retried forever behind a green
// status — and the retries must space out instead of hammering every tick.
func TestProxyConvergenceBacksOffThenReportsUnhealthy(t *testing.T) {
	database := &fakeSchedulerStore{}
	s := newScheduler(t, database)
	s.Docker = stubRuntimeSource{rt: proxyRuntime(&container.State{}, nil)}
	server := proxyServer(store.ProxyDesiredStateRunning)

	now := time.Now()
	for attempt := 1; attempt <= proxyConvergeUnhealthyAfter; attempt++ {
		if err := s.convergeProxyContainer(context.Background(), server, now); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if got := len(database.enqueueArgs); got != attempt {
			t.Fatalf("attempt %d enqueued %d jobs in total", attempt, got)
		}
		// Same instant: the backoff must swallow a second pass.
		if err := s.convergeProxyContainer(context.Background(), server, now); err != nil {
			t.Fatal(err)
		}
		if got := len(database.enqueueArgs); got != attempt {
			t.Fatalf("attempt %d converged twice within its backoff (%d jobs)", attempt, got)
		}
		now = now.Add(proxyConvergeMaxDelay)
	}
	if len(database.proxyStatuses) == 0 {
		t.Fatalf("a proxy that failed to come back %d times must be reported unhealthy", proxyConvergeUnhealthyAfter)
	}
	last := database.proxyStatuses[len(database.proxyStatuses)-1]
	if last.ID != server.ID || last.ProxyObservedStatus != store.ResourceObservedStatusUnhealthy {
		t.Fatalf("observed status = %+v", last)
	}

	// A proxy that comes back clears the count: the next outage starts from
	// zero instead of inheriting an old one and going straight to unhealthy.
	s.Docker = stubRuntimeSource{rt: proxyRuntime(&container.State{Running: true}, nil)}
	if err := s.convergeProxyContainer(context.Background(), server, now); err != nil {
		t.Fatal(err)
	}
	if _, tracked := s.proxyConverge[server.ID]; tracked {
		t.Fatal("a recovered proxy must not keep its failure count")
	}
}

func TestProxyConvergeDelayDoublesToItsCap(t *testing.T) {
	if got := proxyConvergeDelay(1); got != proxyConvergeBaseDelay {
		t.Fatalf("first delay = %s, want %s", got, proxyConvergeBaseDelay)
	}
	if got := proxyConvergeDelay(2); got != 2*proxyConvergeBaseDelay {
		t.Fatalf("second delay = %s", got)
	}
	if got := proxyConvergeDelay(50); got != proxyConvergeMaxDelay {
		t.Fatalf("delay is unbounded: %s", got)
	}
}
