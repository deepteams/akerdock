package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// Backups of the internal databases of compose stacks (compose-spec §10):
// the exact mirror of the database-plan operations, targeted at a
// service_component classified as a database by image detection. The dump,
// restore, drill and retention machinery is shared — only the target
// resolution differs.

// resolveServiceComponent loads a component by uuid, team-scoped (INV-002).
func (a *API) resolveServiceComponent(w http.ResponseWriter, r *http.Request, id *auth.Identity, componentUUID string) (store.ServiceComponent, bool) {
	u, ok := a.scanUUID(w, r, componentUUID, "service component")
	if !ok {
		return store.ServiceComponent{}, false
	}
	sc, err := a.Store.GetServiceComponentByUUID(r.Context(), store.GetServiceComponentByUUIDParams{
		Uuid: u, TeamID: id.TeamID,
	})
	return resolveRow(a, w, r, "service component", sc, err)
}

func (a *API) resolveComponentBackupPlan(w http.ResponseWriter, r *http.Request, id *auth.Identity, componentUUID, planUUID string) (store.ServiceComponent, store.DatabaseBackupPlan, bool) {
	sc, ok := a.resolveServiceComponent(w, r, id, componentUUID)
	if !ok {
		return sc, store.DatabaseBackupPlan{}, false
	}
	u, ok := a.scanUUID(w, r, planUUID, "backup plan")
	if !ok {
		return sc, store.DatabaseBackupPlan{}, false
	}
	plan, err := a.Store.GetBackupPlanByUUIDForComponent(r.Context(), store.GetBackupPlanByUUIDForComponentParams{
		Uuid: u, ServiceComponentID: ptr(sc.ID),
	})
	plan, ok = resolveRow(a, w, r, "backup plan", plan, err)
	return sc, plan, ok
}

// ListComponentBackupPlans implements GET
// /service-components/{uuid}/backups (permission: read).
func (a *API) ListComponentBackupPlans(w http.ResponseWriter, r *http.Request, serviceComponentUuid api.ServiceComponentUuid, params api.ListComponentBackupPlansParams) {
	id, ok := a.require(w, r, auth.PermBackupsRead)
	if !ok {
		return
	}
	sc, ok := a.resolveServiceComponent(w, r, id, serviceComponentUuid)
	if !ok {
		return
	}
	plans, err := a.Store.ListBackupPlansForComponent(r.Context(), ptr(sc.ID))
	if err != nil {
		a.internalError(w, r, "list component backup plans", err)
		return
	}
	data := make([]api.BackupPlan, 0, len(plans))
	for _, p := range plans {
		data = append(data, backupPlanToAPI(p, a.s3StorageUUIDOf(r, p)))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data []api.BackupPlan `json:"data"`
	}{data})
}

// CreateComponentBackupPlan implements POST
// /service-components/{uuid}/backups (permission: write). The component must
// be a database the engine knows how to dump — refused HERE with the reason,
// never accepted and then failing at the first backup (compose-spec §10).
func (a *API) CreateComponentBackupPlan(w http.ResponseWriter, r *http.Request, serviceComponentUuid api.ServiceComponentUuid, params api.CreateComponentBackupPlanParams) {
	id, ok := a.require(w, r, auth.PermBackupsManage)
	if !ok {
		return
	}
	sc, ok := a.resolveServiceComponent(w, r, id, serviceComponentUuid)
	if !ok {
		return
	}
	if !sc.IsDatabase || sc.DatabaseEngine == nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("service_component_uuid"), Code: ptr("not_a_database"),
			Message: "this component was not classified as a database by image detection (compose-spec §10) — only database components are backupable",
		}})
		return
	}
	if *sc.DatabaseEngine != store.DbEnginePostgresql {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("service_component_uuid"), Code: ptr("engine_not_supported"),
			Message: "component engine " + string(*sc.DatabaseEngine) + " is not supported yet — PostgreSQL only in v1 (compose-spec §10)",
		}})
		return
	}

	var body api.BackupPlanCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	cron, valid := normalizeCron(body.Frequency)
	if !valid {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("frequency"), Code: ptr("invalid"),
			Message: "frequency must be a 5-field cron expression or one of: every_minute, hourly, daily, weekly, monthly, yearly",
		}})
		return
	}
	s3ID, ok := a.resolveBackupS3Storage(w, r, id, body.S3StorageUuid, body.SaveS3, body.S3Only)
	if !ok {
		return
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create component backup plan", err)
		return
	}
	boolOr := func(p *bool, def bool) bool {
		if p != nil {
			return *p
		}
		return def
	}
	timezone := "UTC"
	if body.Timezone != nil && *body.Timezone != "" {
		timezone = *body.Timezone
	}
	if !validTimezone(timezone) {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("timezone"), Code: ptr("invalid"),
			Message: "timezone must be an IANA name, e.g. Europe/Paris",
		}})
		return
	}
	plan, err := a.Store.CreateBackupPlan(r.Context(), store.CreateBackupPlanParams{
		Uuid: u, ServiceComponentID: ptr(sc.ID),
		CronExpression: cron, Timezone: timezone,
		Enabled: boolOr(body.Enabled, true), DumpAll: boolOr(body.DumpAll, false),
		TimeoutSeconds: 3600, S3StorageID: s3ID,
		S3Only: boolOr(body.S3Only, false), SaveLocal: boolOr(body.SaveLocal, true),
		RetentionLocalMaxCount: retentionCount(body.LocalRetention, 0),
		RetentionLocalMaxDays:  retentionDays(body.LocalRetention, 0),
		RetentionS3MaxCount:    retentionCount(body.S3Retention, 0),
		RetentionS3MaxDays:     retentionDays(body.S3Retention, 0),
		DrillEnabled:           boolOr(body.DrillEnabled, false),
		DrillIntervalDays:      drillInterval(body.DrillIntervalDays, 7),
	})
	if err != nil {
		a.internalError(w, r, "create component backup plan", err)
		return
	}
	a.recordAudit(r, id, "backup.plan_create", "service_component", sc.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, backupPlanToAPI(plan, a.s3StorageUUIDOf(r, plan)))
}

// GetComponentBackupPlan implements GET
// /service-components/{uuid}/backups/{plan_uuid}.
func (a *API) GetComponentBackupPlan(w http.ResponseWriter, r *http.Request, serviceComponentUuid api.ServiceComponentUuid, backupPlanUuid api.BackupPlanUuid) {
	id, ok := a.require(w, r, auth.PermBackupsRead)
	if !ok {
		return
	}
	_, plan, ok := a.resolveComponentBackupPlan(w, r, id, serviceComponentUuid, backupPlanUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(plan.Version))
	httpapi.WriteJSON(w, http.StatusOK, backupPlanToAPI(plan, a.s3StorageUUIDOf(r, plan)))
}

// UpdateComponentBackupPlan implements PATCH
// /service-components/{uuid}/backups/{plan_uuid} (permission: write).
func (a *API) UpdateComponentBackupPlan(w http.ResponseWriter, r *http.Request, serviceComponentUuid api.ServiceComponentUuid, backupPlanUuid api.BackupPlanUuid, params api.UpdateComponentBackupPlanParams) {
	id, ok := a.require(w, r, auth.PermBackupsManage)
	if !ok {
		return
	}
	_, plan, ok := a.resolveComponentBackupPlan(w, r, id, serviceComponentUuid, backupPlanUuid)
	if !ok {
		return
	}
	expected, err := strconv.Atoi(strings.Trim(strings.TrimSpace(params.IfMatch), `"`))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid If-Match header")
		return
	}
	var body api.BackupPlanUpdate
	if _, ok := decodePatch(w, r, &body); !ok {
		return
	}
	a.applyBackupPlanUpdate(w, r, id, plan, body, int32(expected))
}

// DeleteComponentBackupPlan implements DELETE
// /service-components/{uuid}/backups/{plan_uuid} (permission: write).
func (a *API) DeleteComponentBackupPlan(w http.ResponseWriter, r *http.Request, serviceComponentUuid api.ServiceComponentUuid, backupPlanUuid api.BackupPlanUuid) {
	id, ok := a.require(w, r, auth.PermBackupsManage)
	if !ok {
		return
	}
	_, plan, ok := a.resolveComponentBackupPlan(w, r, id, serviceComponentUuid, backupPlanUuid)
	if !ok {
		return
	}
	if rows, err := a.Store.SoftDeleteBackupPlan(r.Context(), plan.ID); err != nil || rows == 0 {
		a.internalError(w, r, "delete component backup plan", err)
		return
	}
	a.recordAudit(r, id, "backup.plan_delete", "backup_plan", plan.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// ExecuteComponentBackupPlan implements POST
// /service-components/{uuid}/backups/{plan}/execute: 202 + job.
func (a *API) ExecuteComponentBackupPlan(w http.ResponseWriter, r *http.Request, serviceComponentUuid api.ServiceComponentUuid, backupPlanUuid api.BackupPlanUuid, params api.ExecuteComponentBackupPlanParams) {
	id, ok := a.require(w, r, auth.PermBackupsManage)
	if !ok {
		return
	}
	sc, plan, ok := a.resolveComponentBackupPlan(w, r, id, serviceComponentUuid, backupPlanUuid)
	if !ok {
		return
	}
	lockKey := "backup:plan:" + uuidString(plan.Uuid)
	if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err != nil {
		a.internalError(w, r, "execute component backup", err)
		return
	} else if active > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "operation_in_progress", "a backup of this plan is already running")
		return
	}
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:          "backup",
		Type:           jobs.TypeBackupExecute,
		Payload:        jobs.BackupPayload{PlanID: plan.ID},
		LockKey:        &lockKey,
		TeamID:         ptr(id.TeamID),
		ResourceID:     ptr(sc.ResourceID),
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "execute component backup", err)
		return
	}
	a.recordAudit(r, id, "backup.execute", "backup_plan", plan.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

// ListComponentBackupExecutions implements GET
// /service-components/{uuid}/backups/{plan}/executions.
func (a *API) ListComponentBackupExecutions(w http.ResponseWriter, r *http.Request, serviceComponentUuid api.ServiceComponentUuid, backupPlanUuid api.BackupPlanUuid, params api.ListComponentBackupExecutionsParams) {
	id, ok := a.require(w, r, auth.PermBackupsRead)
	if !ok {
		return
	}
	_, plan, ok := a.resolveComponentBackupPlan(w, r, id, serviceComponentUuid, backupPlanUuid)
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
	rows, err := a.Store.ListBackupExecutionsPage(r.Context(), store.ListBackupExecutionsPageParams{
		PlanID: plan.ID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list component backup executions", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(e store.BackupExecution) int64 { return e.ID })
	data := make([]api.BackupExecution, 0, len(rows))
	for _, e := range rows {
		data = append(data, backupExecutionToAPI(e))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.BackupExecution `json:"data"`
		NextCursor *string               `json:"next_cursor"`
	}{data, cursor})
}

// RestoreComponentBackupExecution implements POST
// /service-components/{uuid}/backups/{plan}/executions/{execution}/restore.
func (a *API) RestoreComponentBackupExecution(w http.ResponseWriter, r *http.Request, serviceComponentUuid api.ServiceComponentUuid, backupPlanUuid api.BackupPlanUuid, executionUuid api.ExecutionUuid, params api.RestoreComponentBackupExecutionParams) {
	id, ok := a.require(w, r, auth.PermBackupsRestore)
	if !ok {
		return
	}
	sc, plan, ok := a.resolveComponentBackupPlan(w, r, id, serviceComponentUuid, backupPlanUuid)
	if !ok {
		return
	}
	if !a.confirmRestoreBody(w, r) {
		return
	}
	u, ok := a.scanUUID(w, r, executionUuid, "backup execution")
	if !ok {
		return
	}
	row, err := a.Store.GetBackupExecutionByUUID(r.Context(), store.GetBackupExecutionByUUIDParams{
		Uuid: u, BackupPlanID: plan.ID,
	})
	exec, ok := resolveRow(a, w, r, "backup execution", row, err)
	if !ok {
		return
	}
	if exec.Status == store.BackupExecutionStatusFailed || exec.Filename == nil {
		httpapi.WriteError(w, r, http.StatusConflict, "invalid_state", "this backup has no usable dump")
		return
	}

	// A restore overwrites data: serialized with the stack's own operations
	// (§3.1) and audited (§23.4). The stack resource is the lock scope — a
	// component restore during a stack deployment would race the recreate.
	stackUUID, ok := a.stackUUIDOf(w, r, sc)
	if !ok {
		return
	}
	lockKey := "resource:" + stackUUID
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:          "backup",
		Type:           jobs.TypeBackupRestore,
		Payload:        jobs.BackupPayload{PlanID: plan.ID, ExecutionID: exec.ID},
		LockKey:        &lockKey,
		TeamID:         ptr(id.TeamID),
		ResourceID:     ptr(sc.ResourceID),
		MaxAttempts:    1, // never replay a restore blindly
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "restore component backup", err)
		return
	}
	a.recordAudit(r, id, "backup.restore", "service_component", sc.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

// RunComponentRestoreDrill implements POST
// /service-components/{uuid}/backups/{plan}/drill.
func (a *API) RunComponentRestoreDrill(w http.ResponseWriter, r *http.Request, serviceComponentUuid api.ServiceComponentUuid, backupPlanUuid api.BackupPlanUuid, params api.RunComponentRestoreDrillParams) {
	id, ok := a.require(w, r, auth.PermBackupsRestore)
	if !ok {
		return
	}
	sc, plan, ok := a.resolveComponentBackupPlan(w, r, id, serviceComponentUuid, backupPlanUuid)
	if !ok {
		return
	}
	if _, err := a.Store.GetLatestSuccessfulBackupExecution(r.Context(), plan.ID); err != nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
			"this plan has no successful backup to restore yet")
		return
	}
	lockKey := "backup:plan:" + uuidString(plan.Uuid)
	if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err != nil {
		a.internalError(w, r, "run component restore drill", err)
		return
	} else if active > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "operation_in_progress",
			"a backup or a drill of this plan is already running")
		return
	}
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:          "backup",
		Type:           jobs.TypeBackupDrill,
		Payload:        jobs.BackupPayload{PlanID: plan.ID},
		LockKey:        &lockKey,
		TeamID:         ptr(id.TeamID),
		ResourceID:     ptr(sc.ResourceID),
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "run component restore drill", err)
		return
	}
	a.recordAudit(r, id, "backup.drill", "service_component", sc.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{JobUuid: uuidString(job.Uuid)})
}

// ListComponentRestoreDrills implements GET
// /service-components/{uuid}/backups/{plan}/drills.
func (a *API) ListComponentRestoreDrills(w http.ResponseWriter, r *http.Request, serviceComponentUuid api.ServiceComponentUuid, backupPlanUuid api.BackupPlanUuid, params api.ListComponentRestoreDrillsParams) {
	id, ok := a.require(w, r, auth.PermBackupsRead)
	if !ok {
		return
	}
	_, plan, ok := a.resolveComponentBackupPlan(w, r, id, serviceComponentUuid, backupPlanUuid)
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
	rows, err := a.Store.ListRestoreDrillsPage(r.Context(), store.ListRestoreDrillsPageParams{
		PlanID: plan.ID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list component restore drills", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(d store.ListRestoreDrillsPageRow) int64 { return d.RestoreDrill.ID })
	data := make([]api.RestoreDrill, 0, len(rows))
	for _, d := range rows {
		data = append(data, restoreDrillToAPI(d.RestoreDrill, d.ExecutionUuid))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.RestoreDrill `json:"data"`
		NextCursor *string            `json:"next_cursor"`
	}{data, cursor})
}

// stackUUIDOf resolves the stack resource uuid of a component (lock scope).
func (a *API) stackUUIDOf(w http.ResponseWriter, r *http.Request, sc store.ServiceComponent) (string, bool) {
	resource, err := a.Store.GetResourceByID(r.Context(), sc.ResourceID)
	if err != nil {
		a.internalError(w, r, "resolve component stack", err)
		return "", false
	}
	return uuidString(resource.Uuid), true
}
