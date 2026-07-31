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

	"github.com/docker/docker/api/types/container"

	"github.com/deepteams/akerdock/internal/dockerruntime"
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

// ProxyLifecycle is the worker handler. The lifecycle verbs go through the
// agent channel (ADR-052); the bootstrap that (re)creates the container and
// its configuration stays on SSH, like every provisioning path.
type ProxyLifecycle struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Docker  dockerruntime.Source
	Logger  *slog.Logger
	// ControlPlanePort is the published port of this instance (AKERDOCK_PORT),
	// used to route the instance FQDN on the server that hosts it (§14.2).
	ControlPlanePort int
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
	rt, err := h.Docker.Runtime(ctx, server.ID)
	if err != nil {
		rec.Fail(ctx, "the server's agent is not connected")
		return nil, err
	}

	grace := 10
	var desired store.ProxyDesiredState
	switch payload.Action {
	case "start":
		// The container may be gone entirely (a manual `docker rm`, a pruned
		// host): converging is what "start" means — not a bare start on a
		// name that no longer exists. The bootstrap is a provisioning path
		// and stays on SSH.
		client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
			time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
		if err != nil {
			rec.Fail(ctx, "SSH connection failed")
			return nil, err
		}
		defer func() { _ = client.Close() }()
		if err := bootstrapProxy(ctx, h.Store, h.Keyring, client, server, false, h.ControlPlanePort); err != nil {
			rec.Fail(ctx, err.Error())
			_ = h.Store.SetProxyObservedStatus(ctx, store.SetProxyObservedStatusParams{ID: server.ID, ProxyObservedStatus: store.ResourceObservedStatusUnhealthy})
			return nil, err
		}
		// A start error is not the verdict — the status inspect below is, the
		// same stance as the old `>/dev/null 2>&1; docker inspect` pipeline.
		_ = rt.ContainerStart(ctx, proxy.ContainerName, container.StartOptions{})
		desired = store.ProxyDesiredStateRunning
	case "stop", "restart":
		// Stop and restart operate on an EXISTING container: on a proxy that
		// was never started, "No such container" from the daemon explains
		// nothing — say what the operator should actually do.
		if _, err := rt.ContainerInspect(ctx, proxy.ContainerName); err != nil {
			if dockerruntime.IsNotFound(err) {
				msg := "the proxy container does not exist yet — the first start creates its configuration and the container: press Start"
				rec.Fail(ctx, msg)
				return nil, fmt.Errorf("%s", msg)
			}
			rec.Fail(ctx, err.Error())
			return nil, err
		}
		if payload.Action == "stop" {
			_ = rt.ContainerStop(ctx, proxy.ContainerName, container.StopOptions{Timeout: &grace})
			desired = store.ProxyDesiredStateStopped
		} else {
			if err := rt.ContainerRestart(ctx, proxy.ContainerName, container.StopOptions{Timeout: &grace}); err != nil {
				rec.Fail(ctx, firstLine(err.Error()))
				_ = h.Store.SetProxyObservedStatus(ctx, store.SetProxyObservedStatusParams{ID: server.ID, ProxyObservedStatus: store.ResourceObservedStatusUnhealthy})
				return nil, err
			}
			desired = store.ProxyDesiredStateRunning
		}
	default:
		rec.Fail(ctx, "unknown action")
		return nil, fmt.Errorf("unknown proxy action %q", payload.Action)
	}

	status := "unknown"
	if resp, err := rt.ContainerInspect(ctx, proxy.ContainerName); err == nil && resp.State != nil {
		status = resp.State.Status
	} else if err != nil {
		rec.Fail(ctx, firstLine(err.Error()))
		return nil, err
	}
	if payload.Action != "stop" && status != "running" {
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
