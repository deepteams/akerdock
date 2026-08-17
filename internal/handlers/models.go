package handlers

// Models — first-class inference resources (ADR-080). The handlers mirror
// the databases family: same resource anchoring, same lifecycle enqueue,
// same credential envelope; what is new is the GPU placement guard
// (ADR-079), the soft occupied-GPU start guard with its one-click swap, and
// the serve command spoken both ways through internal/inference.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/inference"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// modelRow flattens the two generated row shapes (get / list) into one.
type modelRow struct {
	Resource        store.Resource
	Model           store.Model
	EnvironmentUUID string
	ProjectUUID     string
	ServerUUID      string
	ServerHost      string
	ServerName      string
	ServerGpuName   *string
}

func modelRowFromGet(r store.GetModelByUUIDRow) modelRow {
	return modelRow{
		Resource: r.Resource, Model: r.Model,
		EnvironmentUUID: uuidString(r.EnvironmentUuid), ProjectUUID: uuidString(r.ProjectUuid),
		ServerUUID: uuidString(r.ServerUuid), ServerHost: r.ServerHost,
		ServerName: r.ServerName, ServerGpuName: r.ServerGpuName,
	}
}

func modelRowFromList(r store.ListModelsPageRow) modelRow {
	return modelRow{
		Resource: r.Resource, Model: r.Model,
		EnvironmentUUID: uuidString(r.EnvironmentUuid), ProjectUUID: uuidString(r.ProjectUuid),
		ServerUUID: uuidString(r.ServerUuid), ServerHost: r.ServerHost,
		ServerName: r.ServerName, ServerGpuName: r.ServerGpuName,
	}
}

// writeJobAccepted answers the 202 every lifecycle enqueue shares.
func writeJobAccepted(w http.ResponseWriter, job store.Job) {
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

// derefTime turns the pgtype pointer into the schema's non-null time.
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// modelEndpoint is the OpenAI-compatible base URL on the server's address.
func modelEndpoint(row modelRow) string {
	return fmt.Sprintf("http://%s:%d/v1", row.ServerHost, row.Model.PublishedPort)
}

func engineFlagsToAPI(raw []byte) []api.EngineFlag {
	var flags []inference.Flag
	_ = json.Unmarshal(raw, &flags)
	out := make([]api.EngineFlag, 0, len(flags))
	for _, f := range flags {
		flag := api.EngineFlag{Flag: f.Name}
		if f.Value != "" {
			flag.Value = ptr(f.Value)
		}
		out = append(out, flag)
	}
	return out
}

func engineFlagsFromAPI(in *[]api.EngineFlag) []inference.Flag {
	if in == nil {
		return nil
	}
	flags := make([]inference.Flag, 0, len(*in))
	for _, f := range *in {
		flag := inference.Flag{Name: f.Flag}
		if f.Value != nil {
			flag.Value = *f.Value
		}
		flags = append(flags, flag)
	}
	return flags
}

func (a *API) modelToAPI(row modelRow) api.Model {
	m := api.Model{
		Uuid:               ptr(uuidString(row.Resource.Uuid)),
		Name:               row.Resource.Name,
		Description:        row.Resource.Description,
		Engine:             api.ModelEngine(row.Model.Engine),
		ModelId:            row.Model.ModelID,
		ServedModelName:    row.Model.ServedModelName,
		Quantization:       row.Model.Quantization,
		TensorParallelSize: ptr(int(row.Model.TensorParallelSize)),
		Image:              row.Model.Image,
		ImageTag:           row.Model.ImageTag,
		EngineFlags:        ptr(engineFlagsToAPI(row.Model.EngineFlags)),
		PublishedPort:      int(row.Model.PublishedPort),
		Endpoint:           ptr(modelEndpoint(row)),
		ProjectUuid:        ptr(row.ProjectUUID),
		EnvironmentUuid:    ptr(row.EnvironmentUUID),
		ServerUuid:         row.ServerUUID,
		ServerName:         ptr(row.ServerName),
		ServerGpuName:      row.ServerGpuName,
		Status:             string(row.Resource.DesiredStatus),
		ObservedStatus:     ptr(api.ObservedStatus(row.Resource.ObservedStatus)),
		ObservedAt:         timePtr(row.Resource.ObservedAt),
		CreatedAt:          derefTime(timePtr(row.Resource.CreatedAt)),
		UpdatedAt:          timePtr(row.Resource.UpdatedAt),
		Version:            int(row.Resource.Version),
	}
	if row.Model.MaxModelLen != nil {
		m.MaxModelLen = ptr(int(*row.Model.MaxModelLen))
	}
	if row.Model.MemoryFraction != nil {
		m.MemoryFraction = row.Model.MemoryFraction
	}
	if row.Model.ShmSizeMb != nil {
		m.ShmSizeMb = ptr(int(*row.Model.ShmSizeMb))
	}
	return m
}

func (a *API) resolveModel(w http.ResponseWriter, r *http.Request, id *auth.Identity, modelUUID string) (store.GetModelByUUIDRow, bool) {
	u, ok := a.scanUUID(w, r, modelUUID, "model")
	if !ok {
		return store.GetModelByUUIDRow{}, false
	}
	row, err := a.Store.GetModelByUUID(r.Context(), store.GetModelByUUIDParams{Uuid: u, TeamID: id.TeamID})
	return resolveRow(a, w, r, "model", row, err)
}

// ListModels implements GET /models (permission: models:read) — the
// transverse view of the Models section (ADR-080 §6), team-wide.
func (a *API) ListModels(w http.ResponseWriter, r *http.Request, params api.ListModelsParams) {
	id, ok := a.require(w, r, auth.PermModelsRead)
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
	rows, err := a.Store.ListModelsPage(r.Context(), store.ListModelsPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list models", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(m store.ListModelsPageRow) int64 { return m.Resource.ID })
	data := make([]api.Model, 0, len(rows))
	for _, m := range rows {
		data = append(data, a.modelToAPI(modelRowFromList(m)))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.Model `json:"data"`
		NextCursor *string     `json:"next_cursor"`
	}{data, cursor})
}

// CreateModel implements POST /models (permission: models:create):
// placement requires an observed GPU (ADR-079), the API key is generated and
// enveloped, the tier-2 flags are validated by shape and by reservation.
func (a *API) CreateModel(w http.ResponseWriter, r *http.Request, params api.CreateModelParams) {
	id, ok := a.require(w, r, auth.PermModelsCreate)
	if !ok {
		return
	}
	var body api.ModelCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	var details []api.ErrorDetail
	if body.Name == "" || len(body.Name) > 255 {
		details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"})
	}
	if strings.TrimSpace(body.ModelId) == "" {
		details = append(details, api.ErrorDetail{Field: ptr("model_id"), Code: ptr("required"), Message: "model_id is required (a Hugging Face reference)"})
	}
	if body.Engine != api.ModelCreateEngineVllm && body.Engine != api.ModelCreateEngineSglang {
		details = append(details, api.ErrorDetail{Field: ptr("engine"), Code: ptr("out_of_range"), Message: "engine must be vllm or sglang"})
	}
	flags := engineFlagsFromAPI(body.EngineFlags)
	if err := inference.ValidateFlags(flags); err != nil {
		details = append(details, api.ErrorDetail{Field: ptr("engine_flags"), Code: ptr("invalid"), Message: err.Error()})
	}
	if body.MemoryFraction != nil && (*body.MemoryFraction <= 0 || *body.MemoryFraction > 1) {
		details = append(details, api.ErrorDetail{Field: ptr("memory_fraction"), Code: ptr("out_of_range"), Message: "memory_fraction must be in (0, 1]"})
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
	// The ADR-079 placement guard: a GPU workload on a server with no
	// observed GPU would start and die with "no CUDA device" on someone
	// else's schedule — refuse it here, where the operator is looking.
	if server.GpuName == nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("server_uuid"), Code: ptr("invalid_state"),
			Message: "this server has no observed GPU (ADR-079) — validate it with the NVIDIA runtime installed, or pick a GPU server",
		}})
		return
	}
	dest, err := a.defaultDestination(r, server.ID)
	if err != nil {
		a.internalError(w, r, "create model", err)
		return
	}

	port := int32(0)
	if body.PublishedPort != nil {
		port = int32(*body.PublishedPort)
	} else {
		next, err := a.Store.NextFreeModelPort(r.Context(), server.ID)
		if err != nil {
			a.internalError(w, r, "allocate model port", err)
			return
		}
		port = next
	}

	apiKey, err := generatePassword()
	if err != nil {
		a.internalError(w, r, "create model", err)
		return
	}
	apiKey = "akm_" + apiKey

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create model", err)
		return
	}
	enc, err := a.Keyring.Encrypt("models", "api_key_enc", pguuid.String(u), []byte(apiKey))
	if err != nil {
		a.internalError(w, r, "create model", err)
		return
	}
	flagsJSON, err := json.Marshal(flags)
	if err != nil {
		a.internalError(w, r, "create model", err)
		return
	}

	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		a.internalError(w, r, "create model", err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	qtx := a.Store.WithTx(tx)

	resource, err := qtx.CreateResource(r.Context(), store.CreateResourceParams{
		Uuid: u, TeamID: id.TeamID, EnvironmentID: env.ID, DestinationID: dest.ID,
		ResourceType: store.ResourceTypeModel, Name: body.Name, Description: body.Description,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a resource with this name already exists in this environment")
			return
		}
		a.internalError(w, r, "create model", err)
		return
	}
	tensorParallel := int32(1)
	if body.TensorParallelSize != nil && *body.TensorParallelSize > 0 {
		tensorParallel = int32(*body.TensorParallelSize)
	}
	var maxLen *int32
	if body.MaxModelLen != nil {
		maxLen = ptr(int32(*body.MaxModelLen))
	}
	memFrac := body.MemoryFraction
	var shm *int32
	if body.ShmSizeMb != nil {
		shm = ptr(int32(*body.ShmSizeMb))
	}
	if err := qtx.CreateModelRow(r.Context(), store.CreateModelRowParams{
		ID: resource.ID, Engine: store.InferenceEngine(body.Engine), ModelID: strings.TrimSpace(body.ModelId),
		ServedModelName: body.ServedModelName, Quantization: body.Quantization,
		MaxModelLen: maxLen, TensorParallelSize: tensorParallel, MemoryFraction: memFrac,
		Image: body.Image, ImageTag: body.ImageTag, EngineFlags: flagsJSON,
		ApiKeyEnc: enc, ShmSizeMb: shm, PublishedPort: port, ServerID: server.ID,
	}); err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this port is already reserved on this server (§22.3)")
			return
		}
		a.internalError(w, r, "create model", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.internalError(w, r, "create model", err)
		return
	}

	row, err := a.Store.GetModelByUUID(r.Context(), store.GetModelByUUIDParams{Uuid: resource.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "reload model", err)
		return
	}
	if body.InstantStart != nil && *body.InstantStart {
		if _, err := a.enqueueModelJob(r, id, resource.ID, resource.Uuid, jobs.TypeModelProvision, "provision", 0); err != nil {
			a.Logger.Warn("instant start failed to enqueue", "error", err)
		}
	}
	a.recordAudit(r, id, "model.create", "model", resource.Uuid)
	w.Header().Set("ETag", etagFor(resource.Version))
	httpapi.WriteJSON(w, http.StatusCreated, a.modelToAPI(modelRowFromGet(row)))
}

// GetModel implements GET /models/{model_uuid} (permission: models:read).
func (a *API) GetModel(w http.ResponseWriter, r *http.Request, modelUuid api.ModelUuid) {
	id, ok := a.require(w, r, auth.PermModelsRead)
	if !ok {
		return
	}
	row, ok := a.resolveModel(w, r, id, modelUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(row.Resource.Version))
	httpapi.WriteJSON(w, http.StatusOK, a.modelToAPI(modelRowFromGet(row)))
}

// UpdateModel implements PATCH /models/{model_uuid} (permission:
// models:update). Engine, server and port are immutable; changes take effect
// at the next start — serve flags are read once, at process start (ADR-080 §5).
func (a *API) UpdateModel(w http.ResponseWriter, r *http.Request, modelUuid api.ModelUuid, params api.UpdateModelParams) {
	id, ok := a.require(w, r, auth.PermModelsUpdate)
	if !ok {
		return
	}
	row, ok := a.resolveModel(w, r, id, modelUuid)
	if !ok {
		return
	}
	expected, err := strconv.Atoi(strings.Trim(strings.TrimSpace(params.IfMatch), `"`))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid If-Match header")
		return
	}
	var body api.ModelUpdate
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

	next := row.Model
	if body.ModelId != nil && strings.TrimSpace(*body.ModelId) != "" {
		next.ModelID = strings.TrimSpace(*body.ModelId)
	}
	if patch.Has("served_model_name") {
		next.ServedModelName = body.ServedModelName
	}
	if patch.Has("quantization") {
		next.Quantization = body.Quantization
	}
	if patch.Has("max_model_len") {
		next.MaxModelLen = nil
		if body.MaxModelLen != nil {
			next.MaxModelLen = ptr(int32(*body.MaxModelLen))
		}
	}
	if body.TensorParallelSize != nil && *body.TensorParallelSize > 0 {
		next.TensorParallelSize = int32(*body.TensorParallelSize)
	}
	if patch.Has("memory_fraction") {
		next.MemoryFraction = nil
		if body.MemoryFraction != nil {
			if *body.MemoryFraction <= 0 || *body.MemoryFraction > 1 {
				httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("memory_fraction"), Code: ptr("out_of_range"), Message: "memory_fraction must be in (0, 1]"}})
				return
			}
			next.MemoryFraction = body.MemoryFraction
		}
	}
	if patch.Has("image") {
		next.Image = body.Image
	}
	if patch.Has("image_tag") {
		next.ImageTag = body.ImageTag
	}
	if patch.Has("shm_size_mb") {
		next.ShmSizeMb = nil
		if body.ShmSizeMb != nil {
			next.ShmSizeMb = ptr(int32(*body.ShmSizeMb))
		}
	}
	flagsJSON := next.EngineFlags
	if body.EngineFlags != nil {
		flags := engineFlagsFromAPI(body.EngineFlags)
		if err := inference.ValidateFlags(flags); err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("engine_flags"), Code: ptr("invalid"), Message: err.Error()}})
			return
		}
		flagsJSON, err = json.Marshal(flags)
		if err != nil {
			a.internalError(w, r, "update model", err)
			return
		}
	}

	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		a.internalError(w, r, "update model", err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	qtx := a.Store.WithTx(tx)

	rows, err := qtx.UpdateResourceMeta(r.Context(), store.UpdateResourceMetaParams{
		ID: row.Resource.ID, Name: name, Description: description, ExpectedVersion: int32(expected),
	})
	if err != nil {
		a.internalError(w, r, "update model", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, row.Resource.Version)
		return
	}
	if err := qtx.UpdateModelRow(r.Context(), store.UpdateModelRowParams{
		ID: row.Resource.ID, ModelID: next.ModelID, ServedModelName: next.ServedModelName,
		Quantization: next.Quantization, MaxModelLen: next.MaxModelLen,
		TensorParallelSize: next.TensorParallelSize, MemoryFraction: next.MemoryFraction,
		Image: next.Image, ImageTag: next.ImageTag, EngineFlags: flagsJSON, ShmSizeMb: next.ShmSizeMb,
	}); err != nil {
		a.internalError(w, r, "update model", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.internalError(w, r, "update model", err)
		return
	}

	updated, err := a.Store.GetModelByUUID(r.Context(), store.GetModelByUUIDParams{Uuid: row.Resource.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "reload model", err)
		return
	}
	a.recordAudit(r, id, "model.update", "model", row.Resource.Uuid)
	w.Header().Set("ETag", etagFor(updated.Resource.Version))
	httpapi.WriteJSON(w, http.StatusOK, a.modelToAPI(modelRowFromGet(updated)))
}

// DeleteModel implements DELETE /models/{model_uuid} (permission:
// models:delete). The HF cache volume is untouched (ADR-080 §4).
func (a *API) DeleteModel(w http.ResponseWriter, r *http.Request, modelUuid api.ModelUuid) {
	id, ok := a.require(w, r, auth.PermModelsDelete)
	if !ok {
		return
	}
	row, ok := a.resolveModel(w, r, id, modelUuid)
	if !ok {
		return
	}
	job, err := a.enqueueModelJob(r, id, row.Resource.ID, row.Resource.Uuid, jobs.TypeModelDelete, "delete", 0)
	if err != nil {
		a.internalError(w, r, "delete model", err)
		return
	}
	a.recordAudit(r, id, "model.delete", "model", row.Resource.Uuid)
	writeJobAccepted(w, job)
}

// StartModel implements POST /models/{model_uuid}/start (permission:
// models:lifecycle) — with the ADR-080 §5 soft guard: starting into an
// occupied GPU answers 409 naming the running model, and `swap=true` stops
// it and starts this one inside a single ordered job.
func (a *API) StartModel(w http.ResponseWriter, r *http.Request, modelUuid api.ModelUuid) {
	id, ok := a.require(w, r, auth.PermModelsLifecycle)
	if !ok {
		return
	}
	row, ok := a.resolveModel(w, r, id, modelUuid)
	if !ok {
		return
	}
	var body struct {
		Swap bool `json:"swap"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // absent body = no swap
	}

	running, err := a.Store.ListRunningModelsOnServer(r.Context(), store.ListRunningModelsOnServerParams{
		ServerID: row.Model.ServerID, ModelID: row.Resource.ID,
	})
	if err != nil {
		a.internalError(w, r, "start model", err)
		return
	}
	var stopFirst int64
	if len(running) > 0 {
		if !body.Swap {
			names := make([]string, 0, len(running))
			for _, m := range running {
				names = append(names, m.Name)
			}
			httpapi.WriteError(w, r, http.StatusConflict, "gpu_busy",
				fmt.Sprintf("%s is running on this GPU server — two models rarely fit one GPU; retry with swap=true to stop it and start this one", strings.Join(names, ", ")))
			return
		}
		// The one-click swap: the job stops the neighbour FIRST, in order.
		first, err := a.Store.GetModelByUUID(r.Context(), store.GetModelByUUIDParams{Uuid: running[0].Uuid, TeamID: id.TeamID})
		if err == nil {
			stopFirst = first.Resource.ID
		}
	}

	job, err := a.enqueueModelJob(r, id, row.Resource.ID, row.Resource.Uuid, jobs.TypeModelStart, "start", stopFirst)
	if err != nil {
		a.internalError(w, r, "start model", err)
		return
	}
	a.recordAudit(r, id, "model.start", "model", row.Resource.Uuid)
	writeJobAccepted(w, job)
}

// StopModel implements POST /models/{model_uuid}/stop (permission:
// models:lifecycle). An explicit stop is a state, never a defect (ADR-080 §5).
func (a *API) StopModel(w http.ResponseWriter, r *http.Request, modelUuid api.ModelUuid) {
	a.modelLifecycle(w, r, modelUuid, jobs.TypeModelStop, "stop")
}

// RestartModel implements POST /models/{model_uuid}/restart.
func (a *API) RestartModel(w http.ResponseWriter, r *http.Request, modelUuid api.ModelUuid) {
	a.modelLifecycle(w, r, modelUuid, jobs.TypeModelRestart, "restart")
}

func (a *API) modelLifecycle(w http.ResponseWriter, r *http.Request, modelUuid, jobType, action string) {
	id, ok := a.require(w, r, auth.PermModelsLifecycle)
	if !ok {
		return
	}
	row, ok := a.resolveModel(w, r, id, modelUuid)
	if !ok {
		return
	}
	job, err := a.enqueueModelJob(r, id, row.Resource.ID, row.Resource.Uuid, jobType, action, 0)
	if err != nil {
		a.internalError(w, r, action+" model", err)
		return
	}
	a.recordAudit(r, id, "model."+action, "model", row.Resource.Uuid)
	writeJobAccepted(w, job)
}

func (a *API) enqueueModelJob(r *http.Request, id *auth.Identity, resourceID int64, resourceUUID pgtype.UUID, jobType, action string, stopFirst int64) (store.Job, error) {
	lockKey := "deploy:model:" + uuidString(resourceUUID)
	return queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:      "deploy",
		Type:       jobType,
		Payload:    jobs.ModelPayload{ResourceID: resourceID, Action: action, StopResourceID: stopFirst},
		LockKey:    &lockKey,
		TeamID:     ptr(id.TeamID),
		ResourceID: ptr(resourceID),
	})
}

// GetModelCommand implements GET /models/{model_uuid}/command — the export
// half of ADR-080 §3bis, by the deployment's own renderer. The key is masked
// unless reveal=true AND the caller holds models:credentials (audited).
func (a *API) GetModelCommand(w http.ResponseWriter, r *http.Request, modelUuid api.ModelUuid, params api.GetModelCommandParams) {
	id, ok := a.require(w, r, auth.PermModelsRead)
	if !ok {
		return
	}
	row, ok := a.resolveModel(w, r, id, modelUuid)
	if !ok {
		return
	}
	// The permission refusal comes BEFORE any rendering work: a caller who
	// may not see the key learns nothing else on the way out either.
	key := inference.MaskedKey
	masked := true
	reveal := params.Reveal != nil && *params.Reveal
	if reveal && !auth.Has(id.Permissions, auth.PermModelsCredentials) {
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden, "revealing the key requires models:credentials")
		return
	}
	cfg, err := jobs.ModelInferenceConfig(row.Model)
	if err != nil {
		a.internalError(w, r, "render command", err)
		return
	}
	if reveal {
		plain, err := a.Keyring.Decrypt("models", "api_key_enc", uuidString(row.Resource.Uuid), row.Model.ApiKeyEnc)
		if err != nil {
			a.internalError(w, r, "reveal command", err)
			return
		}
		key, masked = string(plain), false
		a.recordAudit(r, id, "model.credentials.reveal", "model", row.Resource.Uuid)
	}
	httpapi.WriteJSON(w, http.StatusOK, api.ModelCommand{
		Command: inference.HumanCommand(cfg, key), Masked: masked,
	})
}

// GetModelCredentials implements GET /models/{model_uuid}/credentials
// (permission: models:credentials, audited): the managed key, readable
// because being put in a client's configuration is its purpose (ADR-080 §2).
func (a *API) GetModelCredentials(w http.ResponseWriter, r *http.Request, modelUuid api.ModelUuid) {
	id, ok := a.require(w, r, auth.PermModelsCredentials)
	if !ok {
		return
	}
	row, ok := a.resolveModel(w, r, id, modelUuid)
	if !ok {
		return
	}
	plain, err := a.Keyring.Decrypt("models", "api_key_enc", uuidString(row.Resource.Uuid), row.Model.ApiKeyEnc)
	if err != nil {
		a.internalError(w, r, "model credentials", err)
		return
	}
	a.recordAudit(r, id, "model.credentials.reveal", "model", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusOK, api.ModelCredentials{
		ApiKey: string(plain), Endpoint: modelEndpoint(modelRowFromGet(row)),
	})
}

// PreviewModelCommand implements POST /models/preview-command — the form's
// live preview (ADR-080 §3bis): THE renderer on a configuration that does
// not exist yet, never a UI approximation. Always masked (no model, no key).
func (a *API) PreviewModelCommand(w http.ResponseWriter, r *http.Request) {
	_, ok := a.require(w, r, auth.PermModelsRead)
	if !ok {
		return
	}
	var body api.ModelCommandPreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.ModelId) == "" ||
		(body.Engine != api.ModelCommandPreviewRequestEngineVllm && body.Engine != api.ModelCommandPreviewRequestEngineSglang) {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("engine"), Code: ptr("required"),
			Message: "engine (vllm|sglang) and model_id are required",
		}})
		return
	}
	flags := engineFlagsFromAPI(body.EngineFlags)
	if err := inference.ValidateFlags(flags); err != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("engine_flags"), Code: ptr("invalid"), Message: err.Error()}})
		return
	}
	cfg := inference.Config{
		Engine:  inference.Engine(body.Engine),
		ModelID: strings.TrimSpace(body.ModelId),
		Flags:   flags,
	}
	if body.ServedModelName != nil {
		cfg.ServedModelName = *body.ServedModelName
	}
	if body.Quantization != nil {
		cfg.Quantization = *body.Quantization
	}
	if body.MaxModelLen != nil {
		cfg.MaxModelLen = *body.MaxModelLen
	}
	if body.TensorParallelSize != nil {
		cfg.TensorParallel = *body.TensorParallelSize
	}
	if body.MemoryFraction != nil {
		cfg.MemoryFraction = float64(*body.MemoryFraction)
	}
	httpapi.WriteJSON(w, http.StatusOK, api.ModelCommand{
		Command: inference.HumanCommand(cfg, inference.MaskedKey), Masked: true,
	})
}

// ParseModelCommand implements POST /models/parse-command — the import half
// of ADR-080 §3bis. Pure: nothing is persisted.
func (a *API) ParseModelCommand(w http.ResponseWriter, r *http.Request) {
	_, ok := a.require(w, r, auth.PermModelsRead)
	if !ok {
		return
	}
	var body struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Command) == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "a command is required")
		return
	}
	res, err := inference.Parse(body.Command)
	if err != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("command"), Code: ptr("invalid"), Message: err.Error()}})
		return
	}
	out := api.ModelParseResult{
		Engine:  api.ModelParseResultEngine(res.Config.Engine),
		ModelId: res.Config.ModelID,
		Notices: res.Notices,
	}
	if out.Notices == nil {
		out.Notices = []string{}
	}
	if res.Config.ServedModelName != "" {
		out.ServedModelName = ptr(res.Config.ServedModelName)
	}
	if res.Config.Quantization != "" {
		out.Quantization = ptr(res.Config.Quantization)
	}
	if res.Config.MaxModelLen > 0 {
		out.MaxModelLen = ptr(res.Config.MaxModelLen)
	}
	if res.Config.TensorParallel > 0 {
		out.TensorParallelSize = ptr(res.Config.TensorParallel)
	}
	if res.Config.MemoryFraction > 0 {
		out.MemoryFraction = ptr(res.Config.MemoryFraction)
	}
	flags := make([]api.EngineFlag, 0, len(res.Config.Flags))
	for _, f := range res.Config.Flags {
		flag := api.EngineFlag{Flag: f.Name}
		if f.Value != "" {
			flag.Value = ptr(f.Value)
		}
		flags = append(flags, flag)
	}
	out.EngineFlags = flags
	httpapi.WriteJSON(w, http.StatusOK, out)
}

// SearchModelHub implements GET /models/search — the live Hub search
// proxied by the control plane (ADR-080 §3): fixed host, text-generation
// models, the instance's token for gated ones; offline degrades to an empty
// page, never a broken widget.
func (a *API) SearchModelHub(w http.ResponseWriter, r *http.Request, params api.SearchModelHubParams) {
	_, ok := a.require(w, r, auth.PermModelsRead)
	if !ok {
		return
	}
	q := ""
	if params.Q != nil {
		q = strings.TrimSpace(*params.Q)
	}
	if len(q) < 2 {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "q must be at least 2 characters")
		return
	}
	search := a.hubSearch
	if search == nil {
		search = a.hubSearchHTTP
	}
	raw, err := search(r.Context(), q)
	if err != nil {
		// Offline, rate-limited, DNS-less: the search degrades to the free
		// field — an empty page, never an error the form has to explain.
		a.Logger.Warn("hub search unavailable", "error", err)
		httpapi.WriteJSON(w, http.StatusOK, struct {
			Data []api.HubModel `json:"data"`
		}{[]api.HubModel{}})
		return
	}
	var hub []struct {
		ID        string `json:"id"`
		Downloads *int   `json:"downloads"`
		Likes     *int   `json:"likes"`
		Gated     any    `json:"gated"`
	}
	if err := json.Unmarshal(raw, &hub); err != nil {
		a.internalError(w, r, "hub search decode", err)
		return
	}
	data := make([]api.HubModel, 0, len(hub))
	for _, m := range hub {
		item := api.HubModel{Id: m.ID, Downloads: m.Downloads, Likes: m.Likes}
		// The Hub reports gated as false | "auto" | "manual".
		if g, isBool := m.Gated.(bool); isBool {
			item.Gated = ptr(g)
		} else if m.Gated != nil {
			item.Gated = ptr(true)
		}
		data = append(data, item)
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data []api.HubModel `json:"data"`
	}{data})
}

// hubSearchHTTP queries huggingface.co — the ONE host this proxy ever dials.
func (a *API) hubSearchHTTP(ctx context.Context, q string) ([]byte, error) {
	u := "https://huggingface.co/api/models?limit=10&sort=downloads&pipeline_tag=text-generation&search=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if a.HFToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.HFToken)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub answered %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
