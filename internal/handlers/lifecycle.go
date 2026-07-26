package handlers

import (
	"net/http"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/queue"
)

// StartApplication implements POST /applications/{uuid}/start (permission:
// deploy) — 202 + job.
func (a *API) StartApplication(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	a.lifecycle(w, r, applicationUuid, "start", jobs.TypeApplicationStart)
}

// StopApplication implements POST /applications/{uuid}/stop (permission:
// deploy) — 202 + job.
func (a *API) StopApplication(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	a.lifecycle(w, r, applicationUuid, "stop", jobs.TypeApplicationStop)
}

// RestartApplication implements POST /applications/{uuid}/restart
// (permission: deploy) — 202 + job.
func (a *API) RestartApplication(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	a.lifecycle(w, r, applicationUuid, "restart", jobs.TypeApplicationRestart)
}

func (a *API) lifecycle(w http.ResponseWriter, r *http.Request, applicationUuid, action, jobType string) {
	id, ok := a.require(w, r, auth.PermApplicationsLifecycle)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	// Same lock as deployments: lifecycle and deploys are serialized per
	// application (§3.1).
	lockKey := "deploy:app:" + uuidString(row.Resource.Uuid)
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:      "deploy",
		Type:       jobType,
		Payload:    jobs.ApplicationLifecyclePayload{ResourceID: row.Resource.ID, Action: action},
		LockKey:    &lockKey,
		TeamID:     ptr(id.TeamID),
		ResourceID: ptr(row.Resource.ID),
	})
	if err != nil {
		a.internalError(w, r, action+" application", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}
