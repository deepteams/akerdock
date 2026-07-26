package handlers

import (
	"net/http"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/queue"
)

// RunServerCleanup implements POST /servers/{uuid}/cleanup (permission:
// write): the manual trigger of the §3.7 cleanup — 202 + job. The job itself
// enforces the safety rules (INV-015, never during a deployment).
func (a *API) RunServerCleanup(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid, params api.RunServerCleanupParams) {
	id, ok := a.require(w, r, auth.PermServersMaintain)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	lockKey := "server:cleanup:" + uuidString(server.Uuid)
	if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err != nil {
		a.internalError(w, r, "run server cleanup", err)
		return
	} else if active > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "operation_in_progress", "a cleanup of this server is already running")
		return
	}
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:          "cleanup",
		Type:           jobs.TypeServerCleanup,
		Payload:        jobs.ServerCleanupPayload{ServerID: server.ID, Reason: "manual"},
		LockKey:        &lockKey,
		TeamID:         ptr(id.TeamID),
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "run server cleanup", err)
		return
	}
	a.recordAudit(r, id, "server.cleanup", "server", server.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}
