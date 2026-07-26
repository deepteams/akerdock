package handlers

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

func deploymentToAPI(d store.Deployment, applicationUUID string) api.Deployment {
	u := uuidString(d.Uuid)
	return api.Deployment{
		Uuid:            ptr(u),
		ApplicationUuid: ptr(applicationUUID),
		Status:          api.DeploymentStatus(d.Status),
		Trigger:         ptr(api.DeploymentTrigger(d.Trigger)),
		IsRollback:      ptr(d.IsRollback),
		ForceRebuild:    ptr(d.ForceRebuild),
		CommitSha:       d.CommitSha,
		ImageDigest:     d.ImageDigest,
		Attempt:         ptr(int(d.Attempt)),
		ErrorMessage:    d.ErrorMessage,
		LogsUrl:         ptr("/deployments/" + u + "/logs"),
		QueuedAt:        timePtr(d.QueuedAt),
		StartedAt:       timePtr(d.StartedAt),
		FinishedAt:      timePtr(d.FinishedAt),
		CreatedAt:       timePtr(d.CreatedAt),
	}
}

// GetDeployment implements GET /deployments/{deployment_uuid} (permission:
// read).
func (a *API) GetDeployment(w http.ResponseWriter, r *http.Request, deploymentUuid api.DeploymentUuid) {
	id, ok := a.require(w, r, auth.PermDeploymentsRead)
	if !ok {
		return
	}
	var u pgtype.UUID
	if err := u.Scan(deploymentUuid); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "deployment not found")
		return
	}
	row, err := a.Store.GetDeploymentByUUIDForTeam(r.Context(), store.GetDeploymentByUUIDForTeamParams{Uuid: u, TeamID: id.TeamID})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "deployment not found")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, deploymentToAPI(row.Deployment, uuidString(row.ResourceUuid)))
}

// CancelDeployment implements POST /deployments/{deployment_uuid}/cancel
// (permission: deploy): cooperative cancellation before the traffic switch
// (§21.1, §2.6). Only the candidate is ever removed (INV-006).
func (a *API) CancelDeployment(w http.ResponseWriter, r *http.Request, deploymentUuid api.DeploymentUuid) {
	id, ok := a.require(w, r, auth.PermDeploymentsCancel)
	if !ok {
		return
	}
	var u pgtype.UUID
	if err := u.Scan(deploymentUuid); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "deployment not found")
		return
	}
	row, err := a.Store.GetDeploymentByUUIDForTeam(r.Context(), store.GetDeploymentByUUIDForTeamParams{Uuid: u, TeamID: id.TeamID})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "deployment not found")
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
	rows, cursor := nextCursor(rows, limit, func(d store.Deployment) int64 { return d.ID })

	appUUID := uuidString(row.Resource.Uuid)
	data := make([]api.Deployment, 0, len(rows))
	for _, d := range rows {
		data = append(data, deploymentToAPI(d, appUUID))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.Deployment `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}{data, cursor})
}
