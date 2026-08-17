package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

func (a *API) jobToAPI(r *http.Request, j store.Job) api.Job {
	var steps []api.JobStep
	_ = json.Unmarshal(j.Steps, &steps)

	var result *map[string]interface{}
	if len(j.Result) > 0 {
		m := map[string]interface{}{}
		if err := json.Unmarshal(j.Result, &m); err == nil {
			result = &m
		}
	}
	var jobErr *api.Error
	if j.LastError != nil && (j.Status == store.JobStatusDeadLetter || j.Status == store.JobStatusCancelled) {
		jobErr = &api.Error{Code: httpapi.CodeInternal, Message: *j.LastError}
	}
	var retryOf *string
	if j.RetryOfID != nil {
		if u, err := a.Store.GetJobUUIDByID(r.Context(), *j.RetryOfID); err == nil {
			retryOf = ptr(uuidString(u))
		}
	}
	return api.Job{
		Uuid:           ptr(uuidString(j.Uuid)),
		Type:           ptr(j.JobType),
		Queue:          ptr(j.Queue),
		Status:         api.JobStatus(j.Status),
		Steps:          &steps,
		Attempt:        ptr(int(j.Attempt)),
		Result:         result,
		Error:          jobErr,
		RetryOfUuid:    retryOf,
		DeadLetteredAt: timePtr(j.DeadLetteredAt),
		CreatedAt:      timePtr(j.CreatedAt),
		UpdatedAt:      timePtr(j.UpdatedAt),
		FinishedAt:     timePtr(j.FinishedAt),
	}
}

func (a *API) resolveJob(w http.ResponseWriter, r *http.Request, id *auth.Identity, jobUUID string) (store.Job, bool) {
	u, ok := a.scanUUID(w, r, jobUUID, "job")
	if !ok {
		return store.Job{}, false
	}
	job, err := a.Store.GetJobByUUIDForTeam(r.Context(), store.GetJobByUUIDForTeamParams{Uuid: u, TeamID: ptr(id.TeamID)})
	return resolveRow(a, w, r, "job", job, err)
}

// ListJobs implements GET /jobs (permission: read) — team-scoped, mainly
// for the dead-letter inventory (§21.3).
func (a *API) ListJobs(w http.ResponseWriter, r *http.Request, params api.ListJobsParams) {
	id, ok := a.require(w, r, auth.PermDeploymentsRead)
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
	var statusFilter *store.JobStatus
	if params.Status != nil {
		statusFilter = ptr(store.JobStatus(*params.Status))
	}
	rows, err := a.Store.ListJobsPage(r.Context(), store.ListJobsPageParams{
		TeamID:       ptr(id.TeamID),
		StatusFilter: statusFilter,
		QueueFilter:  params.Queue,
		TypeFilter:   params.Type,
		AfterID:      after,
		PageLimit:    limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list jobs", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(j store.Job) int64 { return j.ID })

	data := make([]api.Job, 0, len(rows))
	for _, j := range rows {
		data = append(data, a.jobToAPI(r, j))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.Job `json:"data"`
		NextCursor *string   `json:"next_cursor"`
	}{data, cursor})
}

// GetJob implements GET /jobs/{job_uuid} (permission: read).
func (a *API) GetJob(w http.ResponseWriter, r *http.Request, jobUuid api.JobUuid) {
	id, ok := a.require(w, r, auth.PermDeploymentsRead)
	if !ok {
		return
	}
	job, ok := a.resolveJob(w, r, id, jobUuid)
	if !ok {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, a.jobToAPI(r, job))
}

// RetryJob implements POST /jobs/{job_uuid}/retry (permission: write):
// creates a linked new attempt — the original job is never re-queued
// (deployment-engine §2.4).
func (a *API) RetryJob(w http.ResponseWriter, r *http.Request, jobUuid api.JobUuid, params api.RetryJobParams) {
	id, ok := a.require(w, r, auth.PermJobsManage)
	if !ok {
		return
	}
	job, ok := a.resolveJob(w, r, id, jobUuid)
	if !ok {
		return
	}
	if job.Status != store.JobStatusDeadLetter {
		httpapi.WriteError(w, r, http.StatusConflict, "invalid_state", "only dead-letter jobs can be retried")
		return
	}

	var payload any
	_ = json.Unmarshal(job.Payload, &payload)
	retry, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:          job.Queue,
		Type:           job.JobType,
		Payload:        payload,
		Priority:       job.Priority,
		MaxAttempts:    job.MaxAttempts,
		LockKey:        job.LockKey,
		TeamID:         job.TeamID,
		ResourceID:     job.ResourceID,
		RetryOfID:      ptr(job.ID),
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "retry job", err)
		return
	}
	a.recordAudit(r, id, "job.retry", "job", job.Uuid)
	a.Logger.Info("dead-letter job retried", "job_uuid", jobUuid, "new_job_uuid", uuidString(retry.Uuid), "token_uuid", id.TokenUUID)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(retry.Uuid),
		StatusUrl: "/jobs/" + uuidString(retry.Uuid),
	})
}

// CancelJob implements POST /jobs/{job_uuid}/cancel (permission:
// jobs:manage): the enqueue you regret. Only a job that has not started is
// cancellable — model and database jobs have no cooperative checkpoint, and
// killing one mid-mutation would leave the server in a state nobody asked
// for.
func (a *API) CancelJob(w http.ResponseWriter, r *http.Request, jobUuid api.JobUuid) {
	id, ok := a.require(w, r, auth.PermJobsManage)
	if !ok {
		return
	}
	job, ok := a.resolveJob(w, r, id, jobUuid)
	if !ok {
		return
	}
	rows, err := a.Store.CancelQueuedJob(r.Context(), job.ID)
	if err != nil {
		a.internalError(w, r, "cancel job", err)
		return
	}
	if rows == 0 {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
			"this job already started (or finished) — only scheduled, queued and retry_wait jobs can be cancelled")
		return
	}
	a.recordAudit(r, id, "job.cancel", "job", job.Uuid)
	job.Status = store.JobStatusCancelled
	httpapi.WriteJSON(w, http.StatusOK, a.jobToAPI(r, job))
}

// ForgetJob implements POST /jobs/{job_uuid}/forget (permission: write):
// audited final closure of a dead-letter job (→ cancelled).
func (a *API) ForgetJob(w http.ResponseWriter, r *http.Request, jobUuid api.JobUuid) {
	id, ok := a.require(w, r, auth.PermJobsManage)
	if !ok {
		return
	}
	job, ok := a.resolveJob(w, r, id, jobUuid)
	if !ok {
		return
	}
	if job.Status != store.JobStatusDeadLetter {
		httpapi.WriteError(w, r, http.StatusConflict, "invalid_state", "only dead-letter jobs can be forgotten")
		return
	}
	// A job that left remote remnants cannot be forgotten silently (§20.6.4):
	// forgetting cleans up NOTHING on the server — it only stops tracking the
	// job. Orphan containers and volumes keep consuming the machine, so the
	// operator must say, in writing, that they know.
	if remnants := a.remnantsOf(r, job); remnants != nil {
		var body api.JobForgetRequest
		if r.ContentLength > 0 {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		if body.AcknowledgeRemnants == nil || !*body.AcknowledgeRemnants {
			httpapi.WriteErrorDetails(w, r, http.StatusConflict, "remnants_present",
				"this job left objects behind on the server; forgetting it does not remove them — "+
					"retry the job, or repeat with acknowledge_remnants=true",
				remnantDetails(remnants))
			return
		}
		a.Logger.Warn("dead-letter job forgotten WITH remnants left on the server",
			"job_uuid", jobUuid, "token_uuid", id.TokenUUID, "remnants", string(remnants))
	}

	rows, err := a.Store.ForgetDeadLetterJob(r.Context(), job.ID)
	if err != nil || rows == 0 {
		a.internalError(w, r, "forget job", err)
		return
	}
	a.recordAudit(r, id, "job.forget", "job", job.Uuid)
	a.Logger.Info("dead-letter job forgotten", "job_uuid", jobUuid, "token_uuid", id.TokenUUID)

	updated, err := a.Store.GetJobByUUIDForTeam(r.Context(), store.GetJobByUUIDForTeamParams{Uuid: job.Uuid, TeamID: ptr(id.TeamID)})
	if err != nil {
		a.internalError(w, r, "reload job", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, a.jobToAPI(r, updated))
}

// remnantsOf returns the remote leftovers recorded by the job's resource, or
// nil when the job left nothing behind (or targets no resource at all).
func (a *API) remnantsOf(r *http.Request, job store.Job) []byte {
	if job.ResourceID == nil {
		return nil
	}
	raw, err := a.Store.GetResourceRemnants(r.Context(), *job.ResourceID)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

// remnantDetails turns the recorded inventory into the `details` of the 409 —
// the operator must see WHAT is left, not merely that something is.
func remnantDetails(raw []byte) []api.ErrorDetail {
	var inventory struct {
		Containers []string `json:"containers"`
		Volumes    []string `json:"volumes"`
		Files      []string `json:"files"`
		Error      string   `json:"error"`
	}
	if err := json.Unmarshal(raw, &inventory); err != nil {
		return []api.ErrorDetail{{Code: ptr("remnants_present"), Message: "the server holds objects from this job"}}
	}
	details := make([]api.ErrorDetail, 0, 3)
	add := func(kind string, items []string) {
		for _, item := range items {
			details = append(details, api.ErrorDetail{
				Field: ptr(kind), Code: ptr("remnant"), Message: item,
			})
		}
	}
	add("container", inventory.Containers)
	add("volume", inventory.Volumes)
	add("files", inventory.Files)
	if inventory.Error != "" {
		details = append(details, api.ErrorDetail{Code: ptr("unknown"), Message: inventory.Error})
	}
	if len(details) == 0 {
		details = append(details, api.ErrorDetail{Code: ptr("remnant"), Message: "the deletion failed; the server state is unverified"})
	}
	return details
}
