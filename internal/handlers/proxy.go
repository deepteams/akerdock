// Proxy lifecycle and logs of a server (PRD §3). Stopping a proxy cuts every
// inbound route of its server: the API exposes the action, the dashboard makes
// the consequence explicit before it is taken.

package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// ProxyLifecycle implements POST /servers/{server_uuid}/proxy/{action}
// (permission: write) — 202 + job.
func (a *API) ProxyLifecycle(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid, action string) {
	id, ok := a.require(w, r, auth.PermServersProxy)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	if server.ProxyType != store.ProxyTypeTraefik {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this server has no managed proxy (proxy_type: none)")
		return
	}

	var jobType string
	switch action {
	case "start":
		jobType = jobs.TypeProxyStart
	case "stop":
		jobType = jobs.TypeProxyStop
	case "restart":
		jobType = jobs.TypeProxyRestart
	default:
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("action"), Code: ptr("invalid"), Message: "action must be start, stop or restart",
		}})
		return
	}

	// One proxy action at a time per server: two concurrent converges would
	// race on the same container.
	lockKey := "proxy:" + uuidString(server.Uuid)
	if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err != nil {
		a.internalError(w, r, "proxy lifecycle", err)
		return
	} else if active > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "operation_in_progress", "a proxy action is already running on this server")
		return
	}

	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:   "maintenance",
		Type:    jobType,
		Payload: jobs.ProxyLifecyclePayload{ServerID: server.ID, Action: action},
		LockKey: &lockKey,
		TeamID:  ptr(id.TeamID),
	})
	if err != nil {
		a.internalError(w, r, "proxy lifecycle", err)
		return
	}
	a.recordAudit(r, id, "proxy."+action, "server", server.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

// GetProxyLogs implements GET /servers/{server_uuid}/proxy/logs (permission:
// read): read straight off the server, never stored.
func (a *API) GetProxyLogs(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid, params api.GetProxyLogsParams) {
	id, ok := a.require(w, r, auth.PermServersRead)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	if server.ProxyType != store.ProxyTypeTraefik {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this server has no managed proxy (proxy_type: none)")
		return
	}
	lines := 200
	if params.Lines != nil && *params.Lines > 0 && *params.Lines <= 2000 {
		lines = *params.Lines
	}

	key, err := a.Store.GetPrivateKeyByID(r.Context(), server.PrivateKeyID)
	if err != nil {
		a.internalError(w, r, "proxy logs", err)
		return
	}
	pem, err := a.Keyring.Decrypt("private_keys", "private_key_enc", uuidString(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		a.internalError(w, r, "proxy logs", err)
		return
	}
	client, err := sshexec.Dial(r.Context(), server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, jobs.PinnedHostKey(server))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "the server is not reachable over SSH right now")
		return
	}
	defer func() { _ = client.Close() }()

	res, err := client.Run(r.Context(), fmt.Sprintf("docker logs --tail %d %s 2>&1", lines, proxy.ContainerName))
	if err != nil {
		a.internalError(w, r, "proxy logs", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": containerLogLines(res.Stdout)})
}

// containerLogLines renders the container output as the contract's LogLine shape.
// The proxy writes everything to its own stream: there is no per-line channel
// to recover, so the whole tail is stdout.
func containerLogLines(out string) []api.LogLine {
	raw := strings.Split(strings.TrimRight(out, "\n"), "\n")
	lines := make([]api.LogLine, 0, len(raw))
	now := time.Now().UTC()
	for i, line := range raw {
		if line == "" && len(raw) == 1 {
			break
		}
		lines = append(lines, api.LogLine{
			Sequence:  i + 1,
			Timestamp: now,
			Channel:   api.LogLineChannelStdout,
			Message:   ansiEscapes.ReplaceAllString(line, ""),
		})
	}
	return lines
}
