// Proxy lifecycle (PRD §3): start, stop and restart the managed proxy of one
// server. Stopping it cuts EVERY inbound route of that server — which is why
// the intent is persisted: a proxy an operator deliberately stopped is not
// drift, and the reconciliation must not "repair" it behind their back.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// Job types of the proxy lifecycle.
const (
	TypeProxyStart   = "proxy.start"
	TypeProxyStop    = "proxy.stop"
	TypeProxyRestart = "proxy.restart"
)

// ProxyLifecyclePayload names the server and the action.
type ProxyLifecyclePayload struct {
	ServerID int64  `json:"server_id"`
	Action   string `json:"action"` // start | stop | restart
}

// ProxyLifecycle is the worker handler.
type ProxyLifecycle struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
}

// Execute drives the proxy container of one server.
func (h *ProxyLifecycle) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload ProxyLifecyclePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	server, err := h.Store.GetServerByID(ctx, payload.ServerID)
	if err != nil {
		return nil, fmt.Errorf("server vanished: %w", err)
	}
	if server.ProxyType != store.ProxyTypeTraefik {
		return nil, fmt.Errorf("this server has no managed proxy")
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
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
	if err != nil {
		rec.Fail(ctx, "SSH connection failed")
		return nil, err
	}
	defer func() { _ = client.Close() }()

	var cmd string
	var desired store.ProxyDesiredState
	switch payload.Action {
	case "start":
		// The container may be gone entirely (a manual `docker rm`, a pruned
		// host): converging is what "start" means — not `docker start` on a
		// name that no longer exists.
		if err := bootstrapProxy(ctx, h.Store, h.Keyring, client, server, false); err != nil {
			rec.Fail(ctx, err.Error())
			_ = h.Store.SetProxyObservedStatus(ctx, store.SetProxyObservedStatusParams{ID: server.ID, ProxyObservedStatus: store.ResourceObservedStatusUnhealthy})
			return nil, err
		}
		cmd = "docker start " + proxy.ContainerName + " >/dev/null 2>&1; docker inspect --format '{{.State.Status}}' " + proxy.ContainerName
		desired = store.ProxyDesiredStateRunning
	case "stop":
		cmd = "docker stop -t 10 " + proxy.ContainerName + " >/dev/null 2>&1; docker inspect --format '{{.State.Status}}' " + proxy.ContainerName
		desired = store.ProxyDesiredStateStopped
	case "restart":
		cmd = "docker restart -t 10 " + proxy.ContainerName + " >/dev/null && docker inspect --format '{{.State.Status}}' " + proxy.ContainerName
		desired = store.ProxyDesiredStateRunning
	default:
		rec.Fail(ctx, "unknown action")
		return nil, fmt.Errorf("unknown proxy action %q", payload.Action)
	}

	res, err := client.Run(ctx, cmd)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	status := firstLine(res.Stdout)
	if res.ExitCode != 0 || (payload.Action != "stop" && status != "running") {
		msg := fmt.Sprintf("the proxy is %q after %s", status, payload.Action)
		rec.Fail(ctx, msg)
		_ = h.Store.SetProxyObservedStatus(ctx, store.SetProxyObservedStatusParams{ID: server.ID, ProxyObservedStatus: store.ResourceObservedStatusUnhealthy})
		return nil, fmt.Errorf("%s", msg)
	}

	// The intent is recorded only once the action really happened: an
	// operator's "stopped" that failed to stop must not read as stopped.
	if err := h.Store.SetProxyDesiredState(ctx, store.SetProxyDesiredStateParams{ID: server.ID, ProxyDesiredState: desired}); err != nil {
		return nil, err
	}
	observed := store.ResourceObservedStatusHealthy
	if payload.Action == "stop" {
		observed = store.ResourceObservedStatusExited
	}
	_ = h.Store.SetProxyObservedStatus(ctx, store.SetProxyObservedStatusParams{ID: server.ID, ProxyObservedStatus: observed})

	rec.Succeed(ctx, "proxy "+status)
	h.Logger.Info("proxy lifecycle", "server_id", server.ID, "action", payload.Action, "status", status)
	if payload.Action == "stop" {
		h.Logger.Warn("the proxy of this server is stopped — every inbound route of the server is down", "server_id", server.ID)
	}
	return map[string]any{"action": payload.Action, "proxy_status": status}, nil
}
