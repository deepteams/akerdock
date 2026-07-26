package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/deepteams/akerdock/internal/adoption"
	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// Adoption of existing Docker resources (§20.7, ADR-013/ADR-023): scan a
// server's unmanaged inventory, adopt candidates without a restart, disown
// without destroying.

// CreateAdoptionScan implements POST /servers/{uuid}/adoption-scans
// (permission: write): 202 + job, plus the scan uuid to read afterwards.
func (a *API) CreateAdoptionScan(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid, params api.CreateAdoptionScanParams) {
	id, ok := a.require(w, r, auth.PermResourcesAdopt)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	lockKey := "adoption:scan:" + uuidString(server.Uuid)
	if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err != nil {
		a.internalError(w, r, "create adoption scan", err)
		return
	} else if active > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "operation_in_progress", "a scan of this server is already running")
		return
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create adoption scan", err)
		return
	}
	scan, err := a.Store.CreateAdoptionScan(r.Context(), store.CreateAdoptionScanParams{
		Uuid: u, TeamID: id.TeamID, ServerID: server.ID,
	})
	if err != nil {
		a.internalError(w, r, "create adoption scan", err)
		return
	}
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue: "maintenance", Type: jobs.TypeAdoptionScan,
		Payload: jobs.AdoptionScanPayload{ScanID: scan.ID},
		LockKey: &lockKey, TeamID: ptr(id.TeamID),
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "create adoption scan", err)
		return
	}
	a.recordAudit(r, id, "adoption.scan", "server", server.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.AdoptionScanAccepted{
		JobUuid:          uuidString(job.Uuid),
		StatusUrl:        "/jobs/" + uuidString(job.Uuid),
		AdoptionScanUuid: uuidString(scan.Uuid),
	})
}

// ListAdoptionScans implements GET /servers/{uuid}/adoption-scans
// (permission: read).
func (a *API) ListAdoptionScans(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid, params api.ListAdoptionScansParams) {
	id, ok := a.require(w, r, auth.PermResourcesRead)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
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
	var beforeID *int64
	if after > 0 {
		beforeID = &after
	}
	rows, err := a.Store.ListAdoptionScansForServer(r.Context(), store.ListAdoptionScansForServerParams{
		ServerID: server.ID, BeforeID: beforeID, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list adoption scans", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(s store.AdoptionScan) int64 { return s.ID })
	data := make([]api.AdoptionScan, 0, len(rows))
	for _, s := range rows {
		data = append(data, adoptionScanToAPI(s, uuidString(server.Uuid)))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.AdoptionScan `json:"data"`
		NextCursor *string            `json:"next_cursor,omitempty"`
	}{Data: data, NextCursor: cursor})
}

// GetAdoptionScan implements GET /adoption-scans/{uuid} (permission: read).
func (a *API) GetAdoptionScan(w http.ResponseWriter, r *http.Request, adoptionScanUuid api.AdoptionScanUuid) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	row, err := a.Store.GetAdoptionScanByUUIDForTeam(r.Context(), store.GetAdoptionScanByUUIDForTeamParams{
		Uuid: pguuid.MustParse(adoptionScanUuid), TeamID: id.TeamID,
	})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "adoption scan not found")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, adoptionScanToAPI(row.AdoptionScan, uuidString(row.ServerUuid)))
}

// AdoptResources implements POST /adoption-scans/{uuid}/adopt (permission:
// write): 202 + job — the adoption itself never restarts a workload (§20.7).
func (a *API) AdoptResources(w http.ResponseWriter, r *http.Request, adoptionScanUuid api.AdoptionScanUuid, params api.AdoptResourcesParams) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, err := a.Store.GetAdoptionScanByUUIDForTeam(r.Context(), store.GetAdoptionScanByUUIDForTeamParams{
		Uuid: pguuid.MustParse(adoptionScanUuid), TeamID: id.TeamID,
	})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "adoption scan not found")
		return
	}
	scan := row.AdoptionScan
	if scan.Status != store.AdoptionScanStatusCompleted {
		httpapi.WriteError(w, r, http.StatusConflict, "invalid_state", "the scan is "+string(scan.Status)+" — adopt from a completed scan")
		return
	}

	var body api.AdoptRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	if len(body.Items) == 0 {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("items"), Code: ptr("required"), Message: "select at least one candidate",
		}})
		return
	}
	env, err := a.Store.GetEnvironmentByUUIDForTeam(r.Context(), store.GetEnvironmentByUUIDForTeamParams{
		Uuid: pguuid.MustParse(body.EnvironmentUuid), TeamID: id.TeamID,
	})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "environment not found")
		return
	}

	// Refuse unknown or non-adoptable candidates HERE, where the operator
	// gets a 422 naming the reason — never silently partial (§20.7).
	var candidates []adoption.Candidate
	if len(scan.Candidates) > 0 {
		if err := json.Unmarshal(scan.Candidates, &candidates); err != nil {
			a.internalError(w, r, "adopt resources", err)
			return
		}
	}
	byID := map[string]adoption.Candidate{}
	for _, c := range candidates {
		byID[c.ID] = c
	}
	items := make([]jobs.AdoptItem, 0, len(body.Items))
	for _, item := range body.Items {
		cand, found := byID[item.CandidateId]
		if !found {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("items"), Code: ptr("invalid"),
				Message: "candidate " + item.CandidateId + " is not in this scan",
			}})
			return
		}
		if !cand.Adoptable {
			details := make([]api.ErrorDetail, 0, len(cand.Reasons))
			for _, reason := range cand.Reasons {
				details = append(details, api.ErrorDetail{
					Field: ptr("items"), Code: ptr("not_adoptable"), Message: reason,
				})
			}
			httpapi.WriteValidationError(w, r, details)
			return
		}
		var name string
		if item.Name != nil {
			name = *item.Name
		}
		items = append(items, jobs.AdoptItem{CandidateID: item.CandidateId, Name: name})
	}

	lockKey := "adoption:adopt:" + uuidString(scan.Uuid)
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue: "default", Type: jobs.TypeAdoptionAdopt,
		Payload: jobs.AdoptPayload{ScanID: scan.ID, EnvironmentID: env.ID, Items: items},
		LockKey: &lockKey, TeamID: ptr(id.TeamID),
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "adopt resources", err)
		return
	}
	a.recordAudit(r, id, "adoption.adopt", "adoption_scan", scan.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

// DisownApplication implements POST /applications/{uuid}/disown
// (permission: write): routing detached, row released — remote objects
// untouched (§20.7 step 5).
func (a *API) DisownApplication(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.DisownApplicationParams) {
	id, ok := a.require(w, r, auth.PermResourcesAdopt)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	a.enqueueDisown(w, r, id, row.Resource, params.IdempotencyKey)
}

// DisownService implements POST /services/{uuid}/disown (permission: write).
func (a *API) DisownService(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid, params api.DisownServiceParams) {
	id, ok := a.require(w, r, auth.PermResourcesAdopt)
	if !ok {
		return
	}
	row, ok := a.resolveServiceStack(w, r, id, serviceUuid)
	if !ok {
		return
	}
	a.enqueueDisown(w, r, id, row.Resource, params.IdempotencyKey)
}

func (a *API) enqueueDisown(w http.ResponseWriter, r *http.Request, id *auth.Identity, resource store.Resource, idempotencyKey *string) {
	lockKey := "resource:" + uuidString(resource.Uuid)
	if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err != nil {
		a.internalError(w, r, "disown resource", err)
		return
	} else if active > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "operation_in_progress", "another operation on this resource is already running")
		return
	}
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue: "default", Type: jobs.TypeResourceDisown,
		Payload: jobs.DisownPayload{ResourceID: resource.ID},
		LockKey: &lockKey, TeamID: ptr(id.TeamID), ResourceID: ptr(resource.ID),
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "disown resource", err)
		return
	}
	a.recordAudit(r, id, "resource.disown", "resource", resource.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

// adoptionScanToAPI renders a scan row. The candidates JSONB round-trips
// through the generated type: scan-internal fields (compose content) are not
// part of the schema and are dropped on the way out — the API never carries
// them (INV-003).
func adoptionScanToAPI(s store.AdoptionScan, serverUUID string) api.AdoptionScan {
	out := api.AdoptionScan{
		Uuid:        uuidString(s.Uuid),
		ServerUuid:  serverUUID,
		Status:      api.AdoptionScanStatus(s.Status),
		Error:       s.Error,
		CreatedAt:   s.CreatedAt.Time.UTC(),
		CompletedAt: timePtr(s.CompletedAt),
	}
	if len(s.Candidates) > 0 {
		var candidates []api.AdoptionCandidate
		if err := json.Unmarshal(s.Candidates, &candidates); err == nil {
			out.Candidates = &candidates
		}
	}
	return out
}
