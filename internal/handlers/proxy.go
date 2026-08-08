// Proxy lifecycle and logs of a server (PRD §3). Stopping a proxy cuts every
// inbound route of its server: the API exposes the action, the dashboard makes
// the consequence explicit before it is taken.

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
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

	// Stopping the proxy that serves the dashboard is the one lifecycle action
	// with no way back from inside the product (ADR-062).
	if action == "stop" {
		acknowledged, ok := proxyLockoutAcknowledged(w, r)
		if !ok {
			return
		}
		if !acknowledged {
			servesDashboard, err := a.proxyServesTheDashboard(r.Context(), server.ID)
			if err != nil {
				a.internalError(w, r, "proxy lifecycle", err)
				return
			}
			if servesDashboard {
				httpapi.WriteError(w, r, http.StatusConflict, "dashboard_lockout",
					"this proxy routes this instance's own dashboard: stopping it takes down the page you would "+
						"use to start it again, and passkey and OIDC sign-in are bound to that address, so a port-forward "+
						"to the control plane cannot authenticate you. Recovery is `docker start "+proxy.ContainerName+
						"` on the server, or `akerdock proxy repair`. Resend with {\"acknowledge_lockout\": true} to proceed.")
				return
			}
		}
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

// proxyLockoutAcknowledged reads the optional body of a lifecycle action. The
// body is optional on purpose: every proxy but one keeps its one-click stop,
// and a client that sends nothing is the normal case, not an error.
func proxyLockoutAcknowledged(w http.ResponseWriter, r *http.Request) (bool, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "unreadable body")
		return false, false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return false, true
	}
	var body api.ProxyLifecycleRequest
	if err := json.Unmarshal(raw, &body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return false, false
	}
	return body.AcknowledgeLockout != nil && *body.AcknowledgeLockout, true
}

// proxyServesTheDashboard reports whether this server's proxy routes the
// instance FQDN (PRD §14.2). The reserved scope is the authority: it is
// written by the control-plane route generator and by nothing else, so its
// presence among the applied revisions IS the fact, with no second source to
// drift from.
func (a *API) proxyServesTheDashboard(ctx context.Context, serverID int64) (bool, error) {
	revisions, err := a.Store.ListAppliedProxyRevisions(ctx, serverID)
	if err != nil {
		return false, err
	}
	return revisionsRouteTheDashboard(revisions), nil
}

func revisionsRouteTheDashboard(revisions []store.ProxyConfigRevision) bool {
	for _, revision := range revisions {
		if revision.Scope == proxy.ControlPlaneScope {
			return true
		}
	}
	return false
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

	rt, ok := a.agentRuntime(w, r, server.ID)
	if !ok {
		return
	}
	out, err := containerLogsSnapshot(r.Context(), rt, proxy.ContainerName, lines)
	if err != nil {
		if dockerruntime.IsNotFound(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
				"the proxy container does not exist on the server")
			return
		}
		a.internalError(w, r, "proxy logs", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": containerLogLines(out)})
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
