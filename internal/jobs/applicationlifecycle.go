package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
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

// ApplicationLifecycle drives docker start/stop/restart on the app
// container, converging desired and observed statuses (§21.2).
type ApplicationLifecycle struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
}

// Execute performs one lifecycle attempt (idempotent: docker start/stop on
// an already converged container succeed as no-ops).
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
	key, err := h.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return nil, err
	}
	pem, err := h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return nil, err
	}

	rec.Start(ctx, payload.Action)
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem), time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
	if err != nil {
		rec.Fail(ctx, "SSH connection failed")
		return nil, err
	}
	defer func() { _ = client.Close() }()

	grace := app.RuntimeConfig.StopGracePeriodSeconds
	if stack && grace == 0 {
		grace = 30
	}
	var cmd string
	var desired store.ResourceDesiredStatus
	switch payload.Action {
	case "start":
		cmd = "docker start " + appUUID
		desired = store.ResourceDesiredStatusRunning
	case "stop":
		cmd = fmt.Sprintf("docker stop -t %d %s", grace, appUUID)
		desired = store.ResourceDesiredStatusStopped
	case "restart":
		cmd = fmt.Sprintf("docker restart -t %d %s", grace, appUUID)
		desired = store.ResourceDesiredStatusRunning
	default:
		rec.Fail(ctx, "unknown action")
		return nil, fmt.Errorf("unknown lifecycle action %q", payload.Action)
	}
	if stack {
		cmd = stackLifecycleCommand(payload.Action, appUUID, grace)
	}

	res, err := client.Run(ctx, cmd)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	if res.ExitCode != 0 {
		msg := firstLine(res.Stderr)
		if strings.Contains(msg, "No such container") {
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

// stackLifecycleCommand drives every container of a compose stack by its
// management labels (§2.3). One-shot jobs (akerdock.oneshot) are never
// started or restarted by a lifecycle action: re-running a migration behind
// the operator's back is not a "start".
func stackLifecycleCommand(action, stackUUID string, grace int32) string {
	byLabel := "--filter label=akerdock.managed=true --filter label=akerdock.resource_uuid=" + stackUUID
	switch action {
	case "stop":
		// Only running containers: exited one-shots stay exited.
		return fmt.Sprintf(`ids=$(docker ps -q %s); [ -n "$ids" ] && docker stop -t %d $ids || echo "nothing to stop"`, byLabel, grace)
	case "start", "restart":
		verb := "docker start"
		if action == "restart" {
			verb = fmt.Sprintf("docker restart -t %d", grace)
		}
		return fmt.Sprintf(`ones=$(docker ps -aq %s --filter label=akerdock.oneshot=true); `+
			`for c in $(docker ps -aq %s); do echo "$ones" | grep -q "^$c$" || %s "$c"; done`,
			byLabel, byLabel, verb)
	}
	return ""
}
