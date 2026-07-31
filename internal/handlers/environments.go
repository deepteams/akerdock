package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

func environmentToAPI(e store.Environment, projectUUID string, resourceCount int) api.Environment {
	return api.Environment{
		Uuid:          ptr(uuidString(e.Uuid)),
		Name:          e.Name,
		Description:   e.Description,
		ProjectUuid:   ptr(projectUUID),
		ResourceCount: ptr(resourceCount),
		Version:       ptr(int(e.Version)),
		CreatedAt:     timePtr(e.CreatedAt),
		UpdatedAt:     timePtr(e.UpdatedAt),
	}
}

// resourceCounts answers "how many live resources" for a batch of environments
// in one query. A count per environment would fan out into one query per row on
// every project listing.
func (a *API) resourceCounts(r *http.Request, ids []int64) map[int64]int {
	counts := make(map[int64]int, len(ids))
	if len(ids) == 0 {
		return counts
	}
	rows, err := a.Store.CountResourcesByEnvironment(r.Context(), ids)
	if err != nil {
		a.Logger.Warn("could not count environment resources", "error", err)
		return counts
	}
	for _, row := range rows {
		counts[row.EnvironmentID] = int(row.Resources)
	}
	return counts
}

// resourceCountOf is the single-environment case.
func (a *API) resourceCountOf(r *http.Request, id int64) int {
	return a.resourceCounts(r, []int64{id})[id]
}

func (a *API) resolveEnvironment(w http.ResponseWriter, r *http.Request, project store.Project, envUUID string) (store.Environment, bool) {
	u, ok := a.scanUUID(w, r, envUUID, "environment")
	if !ok {
		return store.Environment{}, false
	}
	env, err := a.Store.GetEnvironmentByUUID(r.Context(), store.GetEnvironmentByUUIDParams{Uuid: u, ProjectID: project.ID})
	return resolveRow(a, w, r, "environment", env, err)
}

// ListEnvironments implements GET /projects/{project_uuid}/environments
// (permission: read).
func (a *API) ListEnvironments(w http.ResponseWriter, r *http.Request, projectUuid api.ProjectUuid, params api.ListEnvironmentsParams) {
	id, ok := a.require(w, r, auth.PermEnvironmentsRead)
	if !ok {
		return
	}
	project, ok := a.resolveProject(w, r, id, projectUuid)
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
	rows, err := a.Store.ListEnvironmentsPage(r.Context(), store.ListEnvironmentsPageParams{
		ProjectID: project.ID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list environments", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(e store.Environment) int64 { return e.ID })

	projectUUID := uuidString(project.Uuid)
	ids := make([]int64, 0, len(rows))
	for _, e := range rows {
		ids = append(ids, e.ID)
	}
	counts := a.resourceCounts(r, ids)
	data := make([]api.Environment, 0, len(rows))
	for _, e := range rows {
		data = append(data, environmentToAPI(e, projectUUID, counts[e.ID]))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.Environment `json:"data"`
		NextCursor *string           `json:"next_cursor"`
	}{data, cursor})
}

// CreateEnvironment implements POST /projects/{project_uuid}/environments
// (permission: write).
func (a *API) CreateEnvironment(w http.ResponseWriter, r *http.Request, projectUuid api.ProjectUuid, params api.CreateEnvironmentParams) {
	id, ok := a.require(w, r, auth.PermEnvironmentsManage)
	if !ok {
		return
	}
	project, ok := a.resolveProject(w, r, id, projectUuid)
	if !ok {
		return
	}
	var body api.EnvironmentCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	slug, ok := validateName(w, r, body.Name)
	if !ok {
		return
	}

	env, err := a.Store.CreateEnvironment(r.Context(), store.CreateEnvironmentParams{
		ProjectID: project.ID, Name: body.Name, Slug: slug, Description: body.Description,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "an environment with a similar name already exists in this project")
			return
		}
		a.internalError(w, r, "create environment", err)
		return
	}
	w.Header().Set("ETag", etagFor(env.Version))
	httpapi.WriteJSON(w, http.StatusCreated, environmentToAPI(env, uuidString(project.Uuid), 0))
}

// GetEnvironment implements GET
// /projects/{project_uuid}/environments/{environment_uuid} (permission: read).
func (a *API) GetEnvironment(w http.ResponseWriter, r *http.Request, projectUuid api.ProjectUuid, environmentUuid api.EnvironmentUuid) {
	id, ok := a.require(w, r, auth.PermEnvironmentsRead)
	if !ok {
		return
	}
	project, ok := a.resolveProject(w, r, id, projectUuid)
	if !ok {
		return
	}
	env, ok := a.resolveEnvironment(w, r, project, environmentUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(env.Version))
	httpapi.WriteJSON(w, http.StatusOK, environmentToAPI(env, uuidString(project.Uuid), a.resourceCountOf(r, env.ID)))
}

// UpdateEnvironment implements PATCH
// /projects/{project_uuid}/environments/{environment_uuid} (permission: write).
func (a *API) UpdateEnvironment(w http.ResponseWriter, r *http.Request, projectUuid api.ProjectUuid, environmentUuid api.EnvironmentUuid) {
	id, ok := a.require(w, r, auth.PermEnvironmentsManage)
	if !ok {
		return
	}
	project, ok := a.resolveProject(w, r, id, projectUuid)
	if !ok {
		return
	}
	env, ok := a.resolveEnvironment(w, r, project, environmentUuid)
	if !ok {
		return
	}

	var body api.EnvironmentUpdate
	patch, ok := decodePatch(w, r, &body)
	if !ok {
		return
	}
	name, description := env.Name, env.Description
	if body.Name != nil {
		if _, ok := validateName(w, r, *body.Name); !ok {
			return
		}
		name = *body.Name
	}
	if patch.Has("description") {
		description = body.Description
	}

	expected := ifMatchVersion(r, env.Version)
	rows, err := a.Store.UpdateEnvironment(r.Context(), store.UpdateEnvironmentParams{
		ID: env.ID, Name: name, Slug: env.Slug, Description: description, ExpectedVersion: expected,
	})
	if err != nil {
		a.internalError(w, r, "update environment", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, env.Version)
		return
	}

	updated, err := a.Store.GetEnvironmentByUUID(r.Context(), store.GetEnvironmentByUUIDParams{Uuid: env.Uuid, ProjectID: project.ID})
	if err != nil {
		a.internalError(w, r, "reload environment", err)
		return
	}
	w.Header().Set("ETag", etagFor(updated.Version))
	httpapi.WriteJSON(w, http.StatusOK, environmentToAPI(updated, uuidString(project.Uuid), a.resourceCountOf(r, updated.ID)))
}

// DeleteEnvironment implements DELETE
// /projects/{project_uuid}/environments/{environment_uuid} (permission:
// write). Refused with 409 once the environment contains resources (§19.2)
// — enforced when the resources table lands.
func (a *API) DeleteEnvironment(w http.ResponseWriter, r *http.Request, projectUuid api.ProjectUuid, environmentUuid api.EnvironmentUuid) {
	id, ok := a.require(w, r, auth.PermEnvironmentsManage)
	if !ok {
		return
	}
	project, ok := a.resolveProject(w, r, id, projectUuid)
	if !ok {
		return
	}
	env, ok := a.resolveEnvironment(w, r, project, environmentUuid)
	if !ok {
		return
	}
	if count, err := a.Store.CountResourcesInEnvironment(r.Context(), env.ID); err != nil {
		a.internalError(w, r, "delete environment", err)
		return
	} else if count > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "dependency_exists", "the environment still contains resources — delete them first (§19.2)")
		return
	}
	rows, err := a.Store.SoftDeleteEnvironment(r.Context(), env.ID)
	if err != nil || rows == 0 {
		a.internalError(w, r, "delete environment", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
