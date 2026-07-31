package handlers

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

// browsableRepo normalises a git remote (scp-like `git@host:owner/repo.git`,
// `ssh://…`, or an http(s) URL) into the browsable https base, so the dashboard
// can link the branch, commit and PR back to the forge. Empty stays empty.
func browsableRepo(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.TrimSuffix(s, ".git")
	switch {
	case strings.HasPrefix(s, "git@"):
		if host, path, ok := strings.Cut(strings.TrimPrefix(s, "git@"), ":"); ok {
			s = "https://" + host + "/" + strings.TrimPrefix(path, "/")
		}
	case strings.HasPrefix(s, "ssh://"):
		if u, err := url.Parse(s); err == nil {
			host := u.Host
			if at := strings.LastIndexByte(host, '@'); at >= 0 {
				host = host[at+1:]
			}
			s = "https://" + host + u.Path
		}
	case strings.HasPrefix(s, "http://"):
		s = "https://" + strings.TrimPrefix(s, "http://")
	}
	return strings.TrimRight(s, "/")
}

func deploymentToAPI(d store.Deployment, applicationUUID string, prID *int32, repoURL, provider string) api.Deployment {
	u := uuidString(d.Uuid)
	dep := api.Deployment{
		Uuid:            ptr(u),
		ApplicationUuid: ptr(applicationUUID),
		Status:          api.DeploymentStatus(d.Status),
		Trigger:         ptr(api.DeploymentTrigger(d.Trigger)),
		IsRollback:      ptr(d.IsRollback),
		ForceRebuild:    ptr(d.ForceRebuild),
		CommitSha:       d.CommitSha,
		CommitAuthor:    d.CommitAuthor,
		CommitMessage:   d.CommitMessage,
		ImageDigest:     d.ImageDigest,
		Attempt:         ptr(int(d.Attempt)),
		ErrorMessage:    d.ErrorMessage,
		LogsUrl:         ptr("/deployments/" + u + "/logs"),
		QueuedAt:        timePtr(d.QueuedAt),
		StartedAt:       timePtr(d.StartedAt),
		FinishedAt:      timePtr(d.FinishedAt),
		CreatedAt:       timePtr(d.CreatedAt),
	}
	if prID != nil {
		dep.PrId = ptr(int(*prID))
	}
	if d.GitBranch != nil && *d.GitBranch != "" {
		dep.Branch = d.GitBranch
	}
	if repoURL != "" {
		dep.RepositoryUrl = ptr(repoURL)
	}
	if provider != "" {
		dep.Provider = ptr(api.DeploymentProvider(provider))
	}
	return dep
}

// GetDeployment implements GET /deployments/{deployment_uuid} (permission:
// read).
func (a *API) GetDeployment(w http.ResponseWriter, r *http.Request, deploymentUuid api.DeploymentUuid) {
	id, ok := a.require(w, r, auth.PermDeploymentsRead)
	if !ok {
		return
	}
	u, ok := a.scanUUID(w, r, deploymentUuid, "deployment")
	if !ok {
		return
	}
	dep, err := a.Store.GetDeploymentByUUIDForTeam(r.Context(), store.GetDeploymentByUUIDForTeamParams{Uuid: u, TeamID: id.TeamID})
	row, ok := resolveRow(a, w, r, "deployment", dep, err)
	if !ok {
		return
	}
	repoURL := ""
	if row.GitRepositoryUrl != nil {
		repoURL = browsableRepo(*row.GitRepositoryUrl)
	}
	provider := ""
	if row.GitProvider != nil {
		provider = string(*row.GitProvider)
	}
	httpapi.WriteJSON(w, http.StatusOK, deploymentToAPI(row.Deployment, uuidString(row.ResourceUuid), row.PrID, repoURL, provider))
}

// CancelDeployment implements POST /deployments/{deployment_uuid}/cancel
// (permission: deploy): cooperative cancellation before the traffic switch
// (§21.1, §2.6). Only the candidate is ever removed (INV-006).
func (a *API) CancelDeployment(w http.ResponseWriter, r *http.Request, deploymentUuid api.DeploymentUuid) {
	id, ok := a.require(w, r, auth.PermDeploymentsCancel)
	if !ok {
		return
	}
	u, ok := a.scanUUID(w, r, deploymentUuid, "deployment")
	if !ok {
		return
	}
	dep, err := a.Store.GetDeploymentByUUIDForTeam(r.Context(), store.GetDeploymentByUUIDForTeamParams{Uuid: u, TeamID: id.TeamID})
	row, ok := resolveRow(a, w, r, "deployment", dep, err)
	if !ok {
		return
	}
	d := row.Deployment

	switch d.Status {
	case store.DeploymentStatusSucceeded, store.DeploymentStatusFailed, store.DeploymentStatusCancelled, store.DeploymentStatusSuperseded:
		httpapi.WriteError(w, r, http.StatusConflict, "invalid_state", "the deployment is already terminal")
		return
	case store.DeploymentStatusSwitching, store.DeploymentStatusFinishing:
		httpapi.WriteError(w, r, http.StatusConflict, "invalid_state", "the traffic switch has started — the deployment can no longer be cancelled (§21.1)")
		return
	}

	// Queued deployments cancel immediately; running ones abort at the next
	// cooperative checkpoint of the worker (§2.6).
	if _, err := a.Store.CancelQueuedDeployment(r.Context(), d.ID); err != nil {
		a.internalError(w, r, "cancel deployment", err)
		return
	}
	if _, err := a.Store.RequestDeploymentJobCancel(r.Context(), d.ID); err != nil {
		a.internalError(w, r, "cancel deployment", err)
		return
	}
	a.recordAudit(r, id, "deployment.cancel", "deployment", d.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.DeploymentAccepted{
		DeploymentUuid: uuidString(d.Uuid),
		StatusUrl:      "/deployments/" + uuidString(d.Uuid),
	})
}

// ListApplicationDeployments implements GET
// /applications/{application_uuid}/deployments (permission: read).
func (a *API) ListApplicationDeployments(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.ListApplicationDeploymentsParams) {
	id, ok := a.require(w, r, auth.PermDeploymentsRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	limit, ok := pageLimit(w, r, params.Limit)
	if !ok {
		return
	}
	after, ok := afterID(w, r, params.Cursor)
	if !ok {
		return
	}
	rows, err := a.Store.ListDeploymentsForResource(r.Context(), store.ListDeploymentsForResourceParams{
		ResourceID: row.Resource.ID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list deployments", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(d store.ListDeploymentsForResourceRow) int64 { return d.Deployment.ID })

	appUUID := uuidString(row.Resource.Uuid)
	data := make([]api.Deployment, 0, len(rows))
	for _, d := range rows {
		data = append(data, deploymentToAPI(d.Deployment, appUUID, d.PrID, "", ""))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.Deployment `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}{data, cursor})
}
