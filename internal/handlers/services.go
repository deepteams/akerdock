// Compose stacks (compose-spec.md, data dictionary §9.1): inline compose
// files deployed as multi-service resources — the "service" resource type.
// The file is validated at every save: a stack that cannot deploy is refused
// where the operator writes it, not discovered at deployment time.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/compose"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

func serviceToAPI(row store.GetServiceStackByUUIDRow) api.Service {
	return api.Service{
		Uuid:                       ptr(uuidString(row.Resource.Uuid)),
		Name:                       row.Resource.Name,
		Description:                row.Resource.Description,
		ProjectUuid:                ptr(uuidString(row.ProjectUuid)),
		EnvironmentUuid:            ptr(uuidString(row.EnvironmentUuid)),
		ServerUuid:                 ptr(uuidString(row.ServerUuid)),
		ComposeContent:             row.Service.ComposeContent,
		ConnectToPredefinedNetwork: ptr(row.Service.ConnectToPredefinedNetwork),
		DesiredStatus:              api.DesiredStatus(row.Resource.DesiredStatus),
		ObservedStatus:             api.ObservedStatus(row.Resource.ObservedStatus),
		ObservedAt:                 timePtr(row.Resource.ObservedAt),
		Version:                    ptr(int(row.Resource.Version)),
		CreatedAt:                  timePtr(row.Resource.CreatedAt),
		UpdatedAt:                  timePtr(row.Resource.UpdatedAt),
	}
}

func (a *API) resolveServiceStack(w http.ResponseWriter, r *http.Request, id *auth.Identity, serviceUUID string) (store.GetServiceStackByUUIDRow, bool) {
	var u pgtype.UUID
	if err := u.Scan(serviceUUID); err == nil {
		row, err := a.Store.GetServiceStackByUUID(r.Context(), store.GetServiceStackByUUIDParams{Uuid: u, TeamID: id.TeamID})
		if err == nil {
			return row, true
		}
	}
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "service not found")
	return store.GetServiceStackByUUIDRow{}, false
}

// validateComposeContent runs the control-plane validation of compose-spec
// §1–5 and translates the findings into 422 details. Inline stacks have no
// source to build from, so `build:` is refused here, not at deploy time.
func validateComposeContent(ctx context.Context, content, stackUUID string, policy compose.Policy) []api.ErrorDetail {
	res, err := compose.Load(ctx, compose.Input{Content: content, StackUUID: stackUUID, Variables: map[string]string{}, Policy: policy})
	if err != nil {
		return []api.ErrorDetail{{Field: ptr("compose_content"), Code: ptr("compose_parse_error"), Message: err.Error()}}
	}
	var details []api.ErrorDetail
	for _, f := range res.Findings {
		if f.Severity != compose.Error {
			continue
		}
		f := f
		details = append(details, api.ErrorDetail{Field: ptr("compose_content"), Code: ptr(f.Code), Message: f.Message})
	}
	if res.Plan != nil {
		for _, sp := range res.Plan.Services {
			if sp.Build {
				details = append(details, api.ErrorDetail{
					Field: ptr("compose_content"), Code: ptr("compose_build_unsupported"),
					Message: "service " + sp.Name + ": build requires a git source — inline stacks deploy images (use an application with the compose build pack)",
				})
			}
		}
	}
	return details
}

// ListServices implements GET /services (permission: read).
func (a *API) ListServices(w http.ResponseWriter, r *http.Request, params api.ListServicesParams) {
	id, ok := a.require(w, r, auth.PermRead)
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
	rows, err := a.Store.ListServiceStacksPage(r.Context(), store.ListServiceStacksPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list services", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(s store.ListServiceStacksPageRow) int64 { return s.Resource.ID })
	data := make([]api.Service, 0, len(rows))
	for _, row := range rows {
		data = append(data, serviceToAPI(store.GetServiceStackByUUIDRow{
			Resource: row.Resource, Service: row.Service,
			EnvironmentUuid: row.EnvironmentUuid, ProjectUuid: row.ProjectUuid,
			DestinationUuid: row.DestinationUuid, ServerUuid: row.ServerUuid,
		}))
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": data, "next_cursor": cursor})
}

// CreateService implements POST /services (permission: write).
func (a *API) CreateService(w http.ResponseWriter, r *http.Request, params api.CreateServiceParams) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	var body api.ServiceCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	var details []api.ErrorDetail
	if body.Name == "" || len(body.Name) > 255 {
		details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"})
	}
	if strings.TrimSpace(body.ComposeContent) == "" {
		details = append(details, api.ErrorDetail{Field: ptr("compose_content"), Code: ptr("required"), Message: "compose_content is required"})
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	project, ok := a.resolveProject(w, r, id, body.ProjectUuid)
	if !ok {
		return
	}
	env, ok := a.resolveEnvironment(w, r, project, body.EnvironmentUuid)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, body.ServerUuid)
	if !ok {
		return
	}
	if server.Status != store.ServerStatusReady || server.IsBuildServer {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("server_uuid"), Code: ptr("invalid_state"), Message: "the target server must be ready (validated) and not a build server"}})
		return
	}
	dest, err := a.defaultDestination(r, server.ID)
	if err != nil {
		a.internalError(w, r, "resolve destination", err)
		return
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create service", err)
		return
	}
	// The file is validated against the plan it would produce (§11): every
	// blocking finding is named, with its stable code, before anything exists.
	if details := validateComposeContent(r.Context(), body.ComposeContent, pguuid.String(u), compose.Policy{}); len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		a.internalError(w, r, "create service", err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	qtx := a.Store.WithTx(tx)

	resource, err := qtx.CreateResource(r.Context(), store.CreateResourceParams{
		Uuid: u, TeamID: id.TeamID, EnvironmentID: env.ID, DestinationID: dest.ID,
		ResourceType: store.ResourceTypeService, Name: body.Name, Description: body.Description,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a resource with this name already exists in this environment")
			return
		}
		a.internalError(w, r, "create service", err)
		return
	}
	connect := body.ConnectToPredefinedNetwork != nil && *body.ConnectToPredefinedNetwork
	if err := qtx.CreateServiceRow(r.Context(), store.CreateServiceRowParams{
		ID: resource.ID, ComposeContent: body.ComposeContent, ConnectToPredefinedNetwork: connect,
	}); err != nil {
		a.internalError(w, r, "create service", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.internalError(w, r, "create service", err)
		return
	}
	a.recordAudit(r, id, "service.create", "service", resource.Uuid)

	row, err := a.Store.GetServiceStackByUUID(r.Context(), store.GetServiceStackByUUIDParams{Uuid: resource.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "create service", err)
		return
	}
	if body.InstantDeploy != nil && *body.InstantDeploy {
		if _, err := a.enqueueDeployment(r, id, stackRow(row), false, nil); err != nil {
			a.Logger.Warn("instant deploy failed to enqueue", "error", err)
		}
	}
	w.Header().Set("ETag", etagFor(row.Resource.Version))
	httpapi.WriteJSON(w, http.StatusCreated, serviceToAPI(row))
}

// stackRow adapts a stack lookup to the deployment enqueue path: same
// resources, same queue, same locks — a stack deployment IS a deployment.
func stackRow(row store.GetServiceStackByUUIDRow) appRow {
	return appRow{Resource: row.Resource, ServerRowID: row.ServerRowID}
}

// GetService implements GET /services/{service_uuid} (permission: read).
func (a *API) GetService(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	row, ok := a.resolveServiceStack(w, r, id, serviceUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(row.Resource.Version))
	httpapi.WriteJSON(w, http.StatusOK, serviceToAPI(row))
}

// UpdateService implements PATCH /services/{service_uuid} (permission: write).
func (a *API) UpdateService(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid, params api.UpdateServiceParams) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveServiceStack(w, r, id, serviceUuid)
	if !ok {
		return
	}
	expected, err := strconv.Atoi(strings.Trim(strings.TrimSpace(params.IfMatch), `"`))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid If-Match header")
		return
	}
	var body api.ServiceUpdate
	patch, ok := decodePatch(w, r, &body)
	if !ok {
		return
	}
	name := row.Resource.Name
	if body.Name != nil {
		if *body.Name == "" || len(*body.Name) > 255 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"}})
			return
		}
		name = *body.Name
	}
	description := row.Resource.Description
	if patch.Has("description") {
		description = body.Description
	}
	content := row.Service.ComposeContent
	if body.ComposeContent != nil {
		content = *body.ComposeContent
		// An adopted stack legitimately declares its volumes external — that
		// is how its data survived the migration (§20.7); everything else
		// keeps the strict default policy.
		policy := compose.Policy{AllowExternalObjects: row.Resource.AdoptedAt.Valid}
		if details := validateComposeContent(r.Context(), content, uuidString(row.Resource.Uuid), policy); len(details) > 0 {
			httpapi.WriteValidationError(w, r, details)
			return
		}
	}
	connect := row.Service.ConnectToPredefinedNetwork
	if body.ConnectToPredefinedNetwork != nil {
		connect = *body.ConnectToPredefinedNetwork
	}

	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		a.internalError(w, r, "update service", err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	qtx := a.Store.WithTx(tx)

	rows, err := qtx.UpdateResourceMeta(r.Context(), store.UpdateResourceMetaParams{
		ID: row.Resource.ID, Name: name, Description: description, ExpectedVersion: int32(expected),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a resource with this name already exists in this environment")
			return
		}
		a.internalError(w, r, "update service", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, row.Resource.Version)
		return
	}
	if _, err := qtx.UpdateServiceCompose(r.Context(), store.UpdateServiceComposeParams{
		ID: row.Resource.ID, ComposeContent: content, ConnectToPredefinedNetwork: connect,
	}); err != nil {
		a.internalError(w, r, "update service", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.internalError(w, r, "update service", err)
		return
	}
	a.recordAudit(r, id, "service.update", "service", row.Resource.Uuid)

	updated, err := a.Store.GetServiceStackByUUID(r.Context(), store.GetServiceStackByUUIDParams{Uuid: row.Resource.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "update service", err)
		return
	}
	w.Header().Set("ETag", etagFor(updated.Resource.Version))
	httpapi.WriteJSON(w, http.StatusOK, serviceToAPI(updated))
}

// DeleteService implements DELETE /services/{service_uuid} (permission:
// write): asynchronous, same job as applications (§20.6).
func (a *API) DeleteService(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid, params api.DeleteServiceParams) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveServiceStack(w, r, id, serviceUuid)
	if !ok {
		return
	}
	deleteVolumes := params.DeleteVolumes != nil && *params.DeleteVolumes
	if err := a.Store.SetResourceDesiredStatus(r.Context(), store.SetResourceDesiredStatusParams{
		ID: row.Resource.ID, DesiredStatus: store.ResourceDesiredStatusDeleting,
	}); err != nil {
		a.internalError(w, r, "delete service", err)
		return
	}
	lockKey := "resource:delete:" + uuidString(row.Resource.Uuid)
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:      "default",
		Type:       jobs.TypeApplicationDelete,
		Payload:    jobs.ApplicationDeletePayload{ResourceID: row.Resource.ID, DeleteVolumes: deleteVolumes},
		LockKey:    &lockKey,
		TeamID:     ptr(id.TeamID),
		ResourceID: ptr(row.Resource.ID),
	})
	if err != nil {
		a.internalError(w, r, "enqueue service deletion", err)
		return
	}
	a.recordAudit(r, id, "service.delete", "service", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

// ListServiceComponents implements GET /services/{service_uuid}/components.
func (a *API) ListServiceComponents(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	row, ok := a.resolveServiceStack(w, r, id, serviceUuid)
	if !ok {
		return
	}
	components, err := a.Store.ListServiceComponents(r.Context(), row.Resource.ID)
	if err != nil {
		a.internalError(w, r, "list components", err)
		return
	}
	data := make([]api.ServiceComponent, 0, len(components))
	for _, c := range components {
		data = append(data, componentToAPI(c))
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

// DeployService implements POST /services/{service_uuid}/deploy (permission:
// deploy) — same engine, same queue, same locks as the compose build pack.
func (a *API) DeployService(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid, params api.DeployServiceParams) {
	id, ok := a.require(w, r, auth.PermDeploy)
	if !ok {
		return
	}
	row, ok := a.resolveServiceStack(w, r, id, serviceUuid)
	if !ok {
		return
	}
	deployment, err := a.enqueueDeployment(r, id, stackRow(row), false, params.IdempotencyKey)
	if err != nil {
		if err == errQueueFull {
			w.Header().Set("Retry-After", "30")
			httpapi.WriteError(w, r, http.StatusTooManyRequests, "rate_limited", "the server deployment queue is full — retry later (§5.5)")
			return
		}
		a.internalError(w, r, "enqueue deployment", err)
		return
	}
	a.recordAudit(r, id, "deployment.trigger", "deployment", deployment.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.DeploymentAccepted{
		DeploymentUuid: uuidString(deployment.Uuid),
		StatusUrl:      "/deployments/" + uuidString(deployment.Uuid),
	})
}

// StartService / StopService / RestartService — 202 + job, per component.
func (a *API) StartService(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid) {
	a.serviceLifecycle(w, r, serviceUuid, "start", jobs.TypeApplicationStart)
}

func (a *API) StopService(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid) {
	a.serviceLifecycle(w, r, serviceUuid, "stop", jobs.TypeApplicationStop)
}

func (a *API) RestartService(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid) {
	a.serviceLifecycle(w, r, serviceUuid, "restart", jobs.TypeApplicationRestart)
}

func (a *API) serviceLifecycle(w http.ResponseWriter, r *http.Request, serviceUuid, action, jobType string) {
	id, ok := a.require(w, r, auth.PermDeploy)
	if !ok {
		return
	}
	row, ok := a.resolveServiceStack(w, r, id, serviceUuid)
	if !ok {
		return
	}
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
		a.internalError(w, r, action+" service", err)
		return
	}
	a.recordAudit(r, id, "service."+action, "service", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

// ListServiceEnvs / CreateServiceEnv / UpdateServiceEnv / DeleteServiceEnv:
// the stack's variable set (compose-spec §3.2) — same helpers as
// applications, resolved on the service resource.
func (a *API) ListServiceEnvs(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid, params api.ListServiceEnvsParams) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	row, ok := a.resolveServiceStack(w, r, id, serviceUuid)
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
	rows, err := a.Store.ListEnvVarsPage(r.Context(), store.ListEnvVarsPageParams{
		ResourceID: row.Resource.ID, IsPreview: false, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list envs", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(v store.EnvironmentVariable) int64 { return v.ID })
	data := make([]api.EnvironmentVariable, 0, len(rows))
	for _, v := range rows {
		data = append(data, a.envToAPI(id, v))
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": data, "next_cursor": cursor})
}

func (a *API) CreateServiceEnv(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveServiceStack(w, r, id, serviceUuid)
	if !ok {
		return
	}
	var body api.EnvironmentVariableCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	created, err := a.insertEnvVar(r, row.Resource.ID, body, false)
	if err != nil {
		a.writeEnvError(w, r, err)
		return
	}
	a.recordAudit(r, id, "service.env.create", "service", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, a.envToAPI(id, created))
}

func (a *API) UpdateServiceEnv(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid, envUuid api.EnvUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveServiceStack(w, r, id, serviceUuid)
	if !ok {
		return
	}
	v, ok := a.resolveEnvVar(w, r, row.Resource.ID, envUuid)
	if !ok {
		return
	}
	var body api.EnvironmentVariableUpdate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	value := ""
	if body.Value != nil {
		value = *body.Value
	}
	updated, err := a.updateEnvVar(r, v, value, body.IsBuildTime, body.IsLiteral, body.IsMultiline, body.IsLocked)
	if err != nil {
		a.writeEnvError(w, r, err)
		return
	}
	a.recordAudit(r, id, "service.env.update", "service", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusOK, a.envToAPI(id, updated))
}

func (a *API) DeleteServiceEnv(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid, envUuid api.EnvUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveServiceStack(w, r, id, serviceUuid)
	if !ok {
		return
	}
	v, ok := a.resolveEnvVar(w, r, row.Resource.ID, envUuid)
	if !ok {
		return
	}
	if rows, err := a.Store.DeleteEnvVar(r.Context(), v.ID); err != nil || rows == 0 {
		a.internalError(w, r, "delete env", err)
		return
	}
	a.recordAudit(r, id, "service.env.delete", "service", row.Resource.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// ListServiceDeployments implements GET /services/{service_uuid}/deployments.
func (a *API) ListServiceDeployments(w http.ResponseWriter, r *http.Request, serviceUuid api.ServiceUuid, params api.ListServiceDeploymentsParams) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	row, ok := a.resolveServiceStack(w, r, id, serviceUuid)
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
	stackUUID := uuidString(row.Resource.Uuid)
	data := make([]api.Deployment, 0, len(rows))
	for _, d := range rows {
		data = append(data, deploymentToAPI(d, stackUUID))
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": data, "next_cursor": cursor})
}
