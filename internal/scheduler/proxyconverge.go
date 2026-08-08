// Proxy container convergence (ADR-062). ADR-009's drift reconciliation keeps
// the routing files honest; this keeps the process that reads them alive. A
// proxy removed by a stray `docker system prune`, killed by the OOM reaper or
// left stopped after a partial reboot is otherwise observed by nobody — and on
// the server that hosts the instance it takes the dashboard down with it,
// including the page that would bring it back (PRD §14.2).

package scheduler

import (
	"context"
	"time"

	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

const (
	// proxyConvergeUnhealthyAfter is how many convergence attempts may fail
	// before the proxy is reported unhealthy. A proxy that cannot come back —
	// its port taken by something else, typically — must be visible in the
	// dashboard, not silently retried forever behind a green status.
	proxyConvergeUnhealthyAfter = 3
	// The delay doubles from the base to the cap. Capped rather than stopped:
	// the usual cause of a proxy that will not start is transient (a port held
	// by a process someone is about to kill), and a converger that gave up for
	// good would need a human to notice, which is the failure this exists to
	// remove.
	proxyConvergeBaseDelay = time.Minute
	proxyConvergeMaxDelay  = 30 * time.Minute
)

// proxyConvergeState is leader-local, like the disk-probe throttle: only the
// elected leader reconciles, so a plain map is enough and a leader change
// simply restarts the accounting.
type proxyConvergeState struct {
	attempts int
	nextTry  time.Time
}

// convergeProxyContainer restores a proxy whose intent is `running` but whose
// container is gone or stopped. It reports whether an action was taken.
func (s *Scheduler) convergeProxyContainer(ctx context.Context, server store.Server, now time.Time) error {
	// An explicit `stopped` is the operator's decision and is never repaired
	// (§20.1, and the API contract says so in as many words). Converging is
	// following the intent, not overriding it.
	if server.ProxyType != store.ProxyTypeTraefik || server.ProxyDesiredState != store.ProxyDesiredStateRunning {
		delete(s.proxyConverge, server.ID)
		return nil
	}

	if s.Docker == nil {
		return nil
	}
	rt, err := s.Docker.Runtime(ctx, server.ID)
	if err != nil {
		// No channel to the server: its agent is another pass's business, and
		// nothing here could act on the container anyway.
		return nil //nolint:nilerr // an unreachable agent is not a convergence failure
	}
	running, err := proxyContainerRunning(ctx, rt)
	if err != nil {
		return err
	}
	if running {
		delete(s.proxyConverge, server.ID)
		return nil
	}

	state := s.proxyConverge[server.ID]
	if now.Before(state.nextTry) {
		return nil
	}

	// The same lock key the API uses: a converge and an operator's action must
	// never run on the same container at once.
	lockKey := "proxy:" + pguuid.String(server.Uuid)
	active, err := s.Store.CountActiveJobsByLockKey(ctx, &lockKey)
	if err != nil {
		return err
	}
	if active > 0 {
		return nil
	}

	if _, err := queue.Enqueue(ctx, s.Store, queue.EnqueueOptions{
		Queue: "maintenance",
		Type:  jobs.TypeProxyStart,
		// `start` is the converging action: it re-renders the static config,
		// recreates the container when that config drifted, and starts what it
		// finds. It also re-affirms the desired state we just read.
		Payload: jobs.ProxyLifecyclePayload{ServerID: server.ID, Action: "start"},
		LockKey: &lockKey,
		TeamID:  &server.TeamID,
	}); err != nil {
		return err
	}

	if s.proxyConverge == nil {
		s.proxyConverge = map[int64]proxyConvergeState{}
	}
	state.attempts++
	state.nextTry = now.Add(proxyConvergeDelay(state.attempts))
	s.proxyConverge[server.ID] = state
	s.Logger.Warn("proxy is down while its intent is running — converging",
		"server_id", server.ID, "attempt", state.attempts)

	if state.attempts >= proxyConvergeUnhealthyAfter {
		if err := s.Store.SetProxyObservedStatus(ctx, store.SetProxyObservedStatusParams{
			ID: server.ID, ProxyObservedStatus: store.ResourceObservedStatusUnhealthy,
		}); err != nil {
			return err
		}
	}
	return nil
}

// proxyConvergeDelay doubles from the base to the cap.
func proxyConvergeDelay(attempts int) time.Duration {
	delay := proxyConvergeBaseDelay
	for range attempts - 1 {
		delay *= 2
		if delay >= proxyConvergeMaxDelay {
			return proxyConvergeMaxDelay
		}
	}
	return delay
}

// proxyContainerRunning reports whether the managed proxy is up. A container
// that does not exist is not an error here — it is the very case to converge.
func proxyContainerRunning(ctx context.Context, rt dockerruntime.Runtime) (bool, error) {
	res, err := rt.ContainerInspect(ctx, proxy.ContainerName)
	switch {
	case err == nil:
		return res.State != nil && res.State.Running, nil
	case dockerruntime.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}
