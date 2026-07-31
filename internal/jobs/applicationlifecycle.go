package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/deepteams/akerdock/internal/adoption"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// Lifecycle job types: start/stop/restart existing containers without a
// rebuild (§5.3).
const (
	TypeApplicationStart   = "application.start"
	TypeApplicationStop    = "application.stop"
	TypeApplicationRestart = "application.restart"
)

// ApplicationLifecyclePayload references the target resource.
type ApplicationLifecyclePayload struct {
	ResourceID int64  `json:"resource_id"`
	Action     string `json:"action"` // start|stop|restart
}

// ApplicationLifecycle drives container start/stop/restart on the app through
// the server's agent channel (ADR-052), converging desired and observed
// statuses (§21.2).
type ApplicationLifecycle struct {
	Store  *store.Queries
	Docker dockerruntime.Source
	Logger *slog.Logger
}

// Execute performs one lifecycle attempt (idempotent: start/stop of an
// already converged container answer 304, which the runtime treats as
// success).
func (h *ApplicationLifecycle) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload ApplicationLifecyclePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	stack := false
	app, err := h.Store.GetApplicationByID(ctx, payload.ResourceID)
	if err != nil {
		// Compose stacks share the lifecycle job: their containers are found
		// by the management labels, not by a single name (§2.3).
		resource, rerr := h.Store.GetResourceByID(ctx, payload.ResourceID)
		if rerr != nil || resource.ResourceType != store.ResourceTypeService {
			return nil, fmt.Errorf("application vanished: %w", err)
		}
		app = store.GetApplicationByIDRow{Resource: resource}
		stack = true
	}
	appUUID := pguuid.String(app.Resource.Uuid)

	dest, err := h.Store.GetDestinationByID(ctx, app.Resource.DestinationID)
	if err != nil {
		return nil, err
	}
	server, err := h.Store.GetServerByID(ctx, dest.ServerID)
	if err != nil {
		return nil, err
	}

	var desired store.ResourceDesiredStatus
	switch payload.Action {
	case "start", "restart":
		desired = store.ResourceDesiredStatusRunning
	case "stop":
		desired = store.ResourceDesiredStatusStopped
	default:
		rec.Start(ctx, payload.Action)
		rec.Fail(ctx, "unknown action")
		return nil, fmt.Errorf("unknown lifecycle action %q", payload.Action)
	}

	rec.Start(ctx, payload.Action)
	rt, err := h.Docker.Runtime(ctx, server.ID)
	if err != nil {
		rec.Fail(ctx, "the server's agent is not connected")
		return nil, err
	}

	grace := int(app.RuntimeConfig.StopGracePeriodSeconds)
	if stack && grace == 0 {
		grace = 30
	}
	if stack {
		err = stackLifecycle(ctx, rt, payload.Action, stackFilter(app.Resource, appUUID), grace)
	} else {
		// An adopted resource awaiting normalization (§20.7) still lives under
		// the names its original platform gave it — lifecycle targets those.
		err = containerLifecycle(ctx, rt, payload.Action, adoption.ContainerName(app.Resource.Adoption, appUUID), grace)
	}
	if err != nil {
		msg := firstLine(err.Error())
		if dockerruntime.IsNotFound(err) {
			msg = "no container exists for this application — deploy it first"
		}
		rec.Fail(ctx, msg)
		return nil, fmt.Errorf("%s failed: %s", payload.Action, msg)
	}

	_ = h.Store.SetResourceDesiredStatus(ctx, store.SetResourceDesiredStatusParams{ID: app.Resource.ID, DesiredStatus: desired})
	observed := store.ResourceObservedStatusHealthy
	if payload.Action == "stop" {
		observed = store.ResourceObservedStatusExited
	}
	_ = h.Store.SetResourceObservedStatus(ctx, store.SetResourceObservedStatusParams{ID: app.Resource.ID, ObservedStatus: observed})
	rec.Succeed(ctx, payload.Action+" completed")
	return map[string]any{"action": payload.Action, "app_uuid": appUUID}, nil
}

// containerLifecycle drives one container by name.
func containerLifecycle(ctx context.Context, rt dockerruntime.Runtime, action, target string, grace int) error {
	switch action {
	case "start":
		return rt.ContainerStart(ctx, target, container.StartOptions{})
	case "stop":
		return rt.ContainerStop(ctx, target, container.StopOptions{Timeout: &grace})
	case "restart":
		return rt.ContainerRestart(ctx, target, container.StopOptions{Timeout: &grace})
	}
	return fmt.Errorf("unknown lifecycle action %q", action)
}

// stackFilter selects every container of a compose stack: the management
// labels normally, the original compose-project label for an adopted stack
// awaiting normalization (§20.7).
func stackFilter(resource store.Resource, appUUID string) filters.Args {
	if p := adoption.ParsePointer(resource.Adoption); p != nil && p.ComposeProject != "" {
		return filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+p.ComposeProject))
	}
	return filters.NewArgs(
		filters.Arg("label", "akerdock.managed=true"),
		filters.Arg("label", "akerdock.resource_uuid="+appUUID),
	)
}

// stackLifecycle drives every container of a compose stack (§2.3). One-shot
// jobs (akerdock.oneshot) are never started or restarted by a lifecycle
// action: re-running a migration behind the operator's back is not a
// "start". Stop only lists running containers, so exited one-shots stay
// exited — and an empty stack is a no-op, not an error. A failing container
// does not stop the sweep; the first failure reports.
func stackLifecycle(ctx context.Context, rt dockerruntime.Runtime, action string, byLabel filters.Args, grace int) error {
	list, err := rt.ContainerList(ctx, container.ListOptions{All: action != "stop", Filters: byLabel})
	if err != nil {
		return err
	}
	var firstErr error
	for _, c := range list {
		if len(c.Names) == 0 {
			continue
		}
		name := strings.TrimPrefix(c.Names[0], "/")
		if action != "stop" && c.Labels["akerdock.oneshot"] == "true" {
			continue
		}
		if err := containerLifecycle(ctx, rt, action, name, grace); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
