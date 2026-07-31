package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

func projectToAPI(p store.Project, teamUUID string, envs []store.Environment, counts map[int64]int) api.Project {
	var summary *[]api.Environment
	if envs != nil {
		list := make([]api.Environment, 0, len(envs))
		for _, e := range envs {
			list = append(list, environmentToAPI(e, uuidString(p.Uuid), counts[e.ID]))
		}
		summary = &list
	}
	return api.Project{
		Uuid:         ptr(uuidString(p.Uuid)),
		Name:         p.Name,
		Description:  p.Description,
		TeamUuid:     ptr(teamUUID),
		Environments: summary,
		Version:      ptr(int(p.Version)),
		CreatedAt:    timePtr(p.CreatedAt),
		UpdatedAt:    timePtr(p.UpdatedAt),
	}
}

// resolveProject loads a project by public UUID within the token's team;
// any miss is the uniform 404 (INV-002).
func (a *API) resolveProject(w http.ResponseWriter, r *http.Request, id *auth.Identity, projectUUID string) (store.Project, bool) {
	u, ok := a.scanUUID(w, r, projectUUID, "project")
	if !ok {
		return store.Project{}, false
	}
	project, err := a.Store.GetProjectByUUID(r.Context(), store.GetProjectByUUIDParams{Uuid: u, TeamID: id.TeamID})
	return resolveRow(a, w, r, "project", project, err)
}

// ListProjects implements GET /projects (permission: read).
func (a *API) ListProjects(w http.ResponseWriter, r *http.Request, params api.ListProjectsParams) {
	id, ok := a.require(w, r, auth.PermProjectsRead)
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
	rows, err := a.Store.ListProjectsPage(r.Context(), store.ListProjectsPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list projects", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(p store.Project) int64 { return p.ID })

	data := make([]api.Project, 0, len(rows))
	for _, p := range rows {
		data = append(data, projectToAPI(p, id.TeamUUID, nil, nil))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.Project `json:"data"`
		NextCursor *string       `json:"next_cursor"`
	}{data, cursor})
}

// CreateProject implements POST /projects (permission: write): creates the
// project plus its default production environment in one transaction (§2).
func (a *API) CreateProject(w http.ResponseWriter, r *http.Request, params api.CreateProjectParams) {
	id, ok := a.require(w, r, auth.PermProjectsManage)
	if !ok {
		return
	}
	var body api.ProjectCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	slug, ok := validateName(w, r, body.Name)
	if !ok {
		return
	}

	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		a.internalError(w, r, "create project", err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	qtx := a.Store.WithTx(tx)

	project, err := qtx.CreateProject(r.Context(), store.CreateProjectParams{
		TeamID: id.TeamID, Name: body.Name, Slug: slug, Description: body.Description,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a project with a similar name already exists in this team")
			return
		}
		a.internalError(w, r, "create project", err)
		return
	}
	env, err := qtx.CreateEnvironment(r.Context(), store.CreateEnvironmentParams{
		ProjectID: project.ID, Name: "production", Slug: "production",
	})
	if err != nil {
		a.internalError(w, r, "create default environment", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.internalError(w, r, "create project", err)
		return
	}

	w.Header().Set("ETag", etagFor(project.Version))
	httpapi.WriteJSON(w, http.StatusCreated, projectToAPI(project, id.TeamUUID, []store.Environment{env}, nil))
}

// GetProject implements GET /projects/{project_uuid} (permission: read).
func (a *API) GetProject(w http.ResponseWriter, r *http.Request, projectUuid api.ProjectUuid) {
	id, ok := a.require(w, r, auth.PermProjectsRead)
	if !ok {
		return
	}
	project, ok := a.resolveProject(w, r, id, projectUuid)
	if !ok {
		return
	}
	envs, err := a.Store.ListEnvironmentsSummary(r.Context(), project.ID)
	if err != nil {
		a.internalError(w, r, "list environments", err)
		return
	}
	w.Header().Set("ETag", etagFor(project.Version))
	httpapi.WriteJSON(w, http.StatusOK, projectToAPI(project, id.TeamUUID, envs, a.resourceCounts(r, environmentIDs(envs))))
}

// UpdateProject implements PATCH /projects/{project_uuid} (permission:
// write) with optimistic concurrency: If-Match, when present, must match
// the current version. The slug is stable — renaming does not change it.
func (a *API) UpdateProject(w http.ResponseWriter, r *http.Request, projectUuid api.ProjectUuid) {
	id, ok := a.require(w, r, auth.PermProjectsManage)
	if !ok {
		return
	}
	project, ok := a.resolveProject(w, r, id, projectUuid)
	if !ok {
		return
	}

	var body api.ProjectUpdate
	patch, ok := decodePatch(w, r, &body)
	if !ok {
		return
	}
	name, description := project.Name, project.Description
	if body.Name != nil {
		if _, ok := validateName(w, r, *body.Name); !ok {
			return
		}
		name = *body.Name
	}
	if patch.Has("description") {
		description = body.Description // nil when explicitly null
	}

	expected := ifMatchVersion(r, project.Version)
	rows, err := a.Store.UpdateProject(r.Context(), store.UpdateProjectParams{
		ID: project.ID, Name: name, Slug: project.Slug, Description: description, ExpectedVersion: expected,
	})
	if err != nil {
		a.internalError(w, r, "update project", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, project.Version)
		return
	}

	updated, err := a.Store.GetProjectByUUID(r.Context(), store.GetProjectByUUIDParams{Uuid: project.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "reload project", err)
		return
	}
	envs, err := a.Store.ListEnvironmentsSummary(r.Context(), updated.ID)
	if err != nil {
		a.internalError(w, r, "list environments", err)
		return
	}
	w.Header().Set("ETag", etagFor(updated.Version))
	httpapi.WriteJSON(w, http.StatusOK, projectToAPI(updated, id.TeamUUID, envs, a.resourceCounts(r, environmentIDs(envs))))
}

// DeleteProject implements DELETE /projects/{project_uuid} (permission:
// write): tombstones the project and its environments. Refused with 409
// once environments contain resources (§19.2) — enforced when the
// resources table lands.
func (a *API) DeleteProject(w http.ResponseWriter, r *http.Request, projectUuid api.ProjectUuid) {
	id, ok := a.require(w, r, auth.PermProjectsManage)
	if !ok {
		return
	}
	project, ok := a.resolveProject(w, r, id, projectUuid)
	if !ok {
		return
	}
	if count, err := a.Store.CountResourcesInProject(r.Context(), project.ID); err != nil {
		a.internalError(w, r, "delete project", err)
		return
	} else if count > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "dependency_exists", "an environment of this project still contains resources — delete them first (§19.2)")
		return
	}

	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		a.internalError(w, r, "delete project", err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	qtx := a.Store.WithTx(tx)

	if err := qtx.SoftDeleteProjectEnvironments(r.Context(), project.ID); err != nil {
		a.internalError(w, r, "delete project environments", err)
		return
	}
	rows, err := qtx.SoftDeleteProject(r.Context(), project.ID)
	if err != nil || rows == 0 {
		a.internalError(w, r, "delete project", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.internalError(w, r, "delete project", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateName checks a project/environment name and derives its slug.
func validateName(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	if name == "" || len(name) > 255 {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("name"), Code: ptr("required"),
			Message: "name must be non-empty and at most 255 characters",
		}})
		return "", false
	}
	slug := slugify(name)
	if slug == "" {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("name"), Code: ptr("invalid"),
			Message: "name must contain at least one alphanumeric character",
		}})
		return "", false
	}
	return slug, true
}

// environmentIDs collects the ids of a slice of environments, for the batched
// resource count.
func environmentIDs(envs []store.Environment) []int64 {
	ids := make([]int64, 0, len(envs))
	for _, e := range envs {
		ids = append(ids, e.ID)
	}
	return ids
}
