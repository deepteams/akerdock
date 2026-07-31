package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/cronexpr"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// cronAliases are the shorthand frequencies of §7.1.
var cronAliases = map[string]string{
	"every_minute": "* * * * *",
	"hourly":       "0 * * * *",
	"daily":        "0 3 * * *",
	"weekly":       "0 3 * * 0",
	"monthly":      "0 3 1 * *",
	"yearly":       "0 3 1 1 *",
}

// cronFormat rejects anything that is not a plain 5-field expression (§23.3):
// no shell metacharacter reaches the scheduler. It is a filter, not the
// grammar — cronexpr.Parse is the authority on what can actually be
// scheduled, so the API never accepts a plan the scheduler cannot fire.
var cronFormat = regexp.MustCompile(`^[\d*/,\-]+ [\d*/,\-]+ [\d*/,\-]+ [\d*/,\-]+ [\d*/,\-]+$`)

// normalizeCron resolves an alias or validates a raw expression.
func normalizeCron(freq string) (string, bool) {
	freq = strings.TrimSpace(freq)
	if expr, ok := cronAliases[freq]; ok {
		return expr, true
	}
	if !cronFormat.MatchString(freq) {
		return "", false
	}
	if _, err := cronexpr.Parse(freq); err != nil {
		return "", false
	}
	return freq, true
}

// drillInterval keeps the CHECK (> 0) of the column out of a 500: an absent or
// nonsensical interval falls back to the default rather than reaching the
// database as a zero.
func drillInterval(v *int, def int32) int32 {
	if v != nil && *v > 0 {
		return int32(*v)
	}
	if def > 0 {
		return def
	}
	return 7
}

// validTimezone keeps an unknown IANA name out of the scheduler, where it
// would silently stall the plan.
func validTimezone(name string) bool {
	_, err := time.LoadLocation(name)
	return err == nil
}

// backupPlanToAPI renders a plan. s3UUID is the destination bucket, resolved
// by the caller; `save_s3` is not a stored column — a plan saves to S3
// exactly when it has a destination.
func backupPlanToAPI(p store.DatabaseBackupPlan, s3UUID *string) api.BackupPlan {
	out := api.BackupPlan{
		S3StorageUuid: s3UUID,
		SaveS3:        ptr(p.S3StorageID != nil),
		Uuid:          ptr(uuidString(p.Uuid)),
		Frequency:     p.CronExpression,
		Timezone:      ptr(p.Timezone),
		Enabled:       p.Enabled,
		DumpAll:       ptr(p.DumpAll),
		SaveLocal:     ptr(p.SaveLocal),
		S3Only:        ptr(p.S3Only),
		LocalRetention: &api.RetentionPolicy{
			MaxCount:   ptr(int(p.RetentionLocalMaxCount)),
			MaxAgeDays: ptr(int(p.RetentionLocalMaxDays)),
		},
		S3Retention: &api.RetentionPolicy{
			MaxCount:   ptr(int(p.RetentionS3MaxCount)),
			MaxAgeDays: ptr(int(p.RetentionS3MaxDays)),
		},
		CreatedAt: timePtr(p.CreatedAt),
		UpdatedAt: timePtr(p.UpdatedAt),
	}
	// next_run_at is owned by the scheduler and is only known once it has
	// seen the plan; until then the field is absent rather than guessed.
	if p.NextRunAt.Valid {
		out.NextRunAt = timePtr(p.NextRunAt)
	}
	out.DrillEnabled = ptr(p.DrillEnabled)
	out.DrillIntervalDays = ptr(int(p.DrillIntervalDays))
	if p.LastDrillAt.Valid {
		out.LastDrillAt = timePtr(p.LastDrillAt)
	}
	if p.LastDrillStatus != nil {
		out.LastDrillStatus = ptr(api.BackupPlanLastDrillStatus(*p.LastDrillStatus))
	}
	return out
}

func backupExecutionToAPI(e store.BackupExecution) api.BackupExecution {
	// message carries the failure detail or the partial-upload warning —
	// never a secret (§20.5).
	message := e.ErrorMessage
	if message == nil {
		message = e.S3UploadError
	}
	out := api.BackupExecution{
		Uuid:           ptr(uuidString(e.Uuid)),
		Status:         ptr(api.BackupExecutionStatus(e.Status)),
		Filename:       e.Filename,
		Checksum:       e.ChecksumSha256,
		S3Uploaded:     ptr(e.UploadedToS3),
		LocalAvailable: ptr(!e.LocalDeletedAt.Valid && e.Filename != nil),
		Message:        message,
		StartedAt:      timePtr(e.StartedAt),
		FinishedAt:     timePtr(e.FinishedAt),
		CreatedAt:      timePtr(e.CreatedAt),
	}
	if e.SizeBytes != nil {
		out.SizeBytes = ptr(int(*e.SizeBytes))
	}
	return out
}

func (a *API) resolveBackupPlan(w http.ResponseWriter, r *http.Request, id *auth.Identity, dbUUID, planUUID string) (store.GetDatabaseByUUIDRow, store.DatabaseBackupPlan, bool) {
	row, ok := a.resolveDatabase(w, r, id, dbUUID)
	if !ok {
		return row, store.DatabaseBackupPlan{}, false
	}
	u, ok := a.scanUUID(w, r, planUUID, "backup plan")
	if !ok {
		return row, store.DatabaseBackupPlan{}, false
	}
	plan, err := a.Store.GetBackupPlanByUUID(r.Context(), store.GetBackupPlanByUUIDParams{
		Uuid: u, DatabaseID: ptr(row.Resource.ID),
	})
	plan, ok = resolveRow(a, w, r, "backup plan", plan, err)
	return row, plan, ok
}

// ListBackupPlans implements GET /databases/{uuid}/backups (permission: read).
func (a *API) ListBackupPlans(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid, params api.ListBackupPlansParams) {
	id, ok := a.require(w, r, auth.PermBackupsRead)
	if !ok {
		return
	}
	row, ok := a.resolveDatabase(w, r, id, databaseUuid)
	if !ok {
		return
	}
	plans, err := a.Store.ListBackupPlansForDatabase(r.Context(), ptr(row.Resource.ID))
	if err != nil {
		a.internalError(w, r, "list backup plans", err)
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

// CreateBackupPlan implements POST /databases/{uuid}/backups (permission:
// write).
func (a *API) CreateBackupPlan(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid, params api.CreateBackupPlanParams) {
	id, ok := a.require(w, r, auth.PermBackupsManage)
	if !ok {
		return
	}
	row, ok := a.resolveDatabase(w, r, id, databaseUuid)
	if !ok {
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
	// An S3 destination must exist, belong to the team, and have passed its
	// connectivity check: a plan pointing at an unusable bucket would only
	// fail at the first backup, when it is too late to notice.
	s3ID, ok := a.resolveBackupS3Storage(w, r, id, body.S3StorageUuid, body.SaveS3, body.S3Only)
	if !ok {
		return
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create backup plan", err)
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
		Uuid: u, DatabaseID: ptr(row.Resource.ID),
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
		a.internalError(w, r, "create backup plan", err)
		return
	}
	a.recordAudit(r, id, "backup.plan_create", "database", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, backupPlanToAPI(plan, a.s3StorageUUIDOf(r, plan)))
}

// GetBackupPlan implements GET /databases/{uuid}/backups/{plan_uuid}.
func (a *API) GetBackupPlan(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid, backupPlanUuid api.BackupPlanUuid) {
	id, ok := a.require(w, r, auth.PermBackupsRead)
	if !ok {
		return
	}
	_, plan, ok := a.resolveBackupPlan(w, r, id, databaseUuid, backupPlanUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(plan.Version))
	httpapi.WriteJSON(w, http.StatusOK, backupPlanToAPI(plan, a.s3StorageUUIDOf(r, plan)))
}

// UpdateBackupPlan implements PATCH /databases/{uuid}/backups/{plan_uuid}
// (permission: write).
func (a *API) UpdateBackupPlan(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid, backupPlanUuid api.BackupPlanUuid, params api.UpdateBackupPlanParams) {
	id, ok := a.require(w, r, auth.PermBackupsManage)
	if !ok {
		return
	}
	_, plan, ok := a.resolveBackupPlan(w, r, id, databaseUuid, backupPlanUuid)
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

// applyBackupPlanUpdate is the shared PATCH tail of the database and
// component plan handlers: same fields, same rules, same optimistic locking.
func (a *API) applyBackupPlanUpdate(w http.ResponseWriter, r *http.Request, id *auth.Identity, plan store.DatabaseBackupPlan, body api.BackupPlanUpdate, expected int32) {
	var ok bool
	cron := plan.CronExpression
	if body.Frequency != nil {
		normalized, valid := normalizeCron(*body.Frequency)
		if !valid {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("frequency"), Code: ptr("invalid"), Message: "invalid cron frequency"}})
			return
		}
		cron = normalized
	}
	boolOr := func(p *bool, cur bool) bool {
		if p != nil {
			return *p
		}
		return cur
	}
	timezone := plan.Timezone
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

	s3ID := plan.S3StorageID
	if body.S3StorageUuid != nil {
		s3ID, ok = a.resolveBackupS3Storage(w, r, id, body.S3StorageUuid, body.SaveS3, body.S3Only)
		if !ok {
			return
		}
	}

	rows, err := a.Store.UpdateBackupPlan(r.Context(), store.UpdateBackupPlanParams{
		ID: plan.ID, CronExpression: cron, Timezone: timezone,
		Enabled: boolOr(body.Enabled, plan.Enabled), DumpAll: boolOr(body.DumpAll, plan.DumpAll),
		S3StorageID: s3ID,
		S3Only:      boolOr(body.S3Only, plan.S3Only), SaveLocal: boolOr(body.SaveLocal, plan.SaveLocal),
		RetentionLocalMaxCount: retentionCount(body.LocalRetention, plan.RetentionLocalMaxCount),
		RetentionLocalMaxDays:  retentionDays(body.LocalRetention, plan.RetentionLocalMaxDays),
		RetentionS3MaxCount:    retentionCount(body.S3Retention, plan.RetentionS3MaxCount),
		RetentionS3MaxDays:     retentionDays(body.S3Retention, plan.RetentionS3MaxDays),
		DrillEnabled:           boolOr(body.DrillEnabled, plan.DrillEnabled),
		DrillIntervalDays:      drillInterval(body.DrillIntervalDays, plan.DrillIntervalDays),
		ExpectedVersion:        expected,
	})
	if err != nil {
		a.internalError(w, r, "update backup plan", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, plan.Version)
		return
	}
	updated, err := a.Store.GetBackupPlanByID(r.Context(), plan.ID)
	if err != nil {
		a.internalError(w, r, "reload backup plan", err)
		return
	}
	w.Header().Set("ETag", etagFor(updated.Version))
	httpapi.WriteJSON(w, http.StatusOK, backupPlanToAPI(updated, a.s3StorageUUIDOf(r, updated)))
}

// DeleteBackupPlan implements DELETE /databases/{uuid}/backups/{plan_uuid}
// (permission: write). Soft delete — the executed backups stay on disk.
func (a *API) DeleteBackupPlan(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid, backupPlanUuid api.BackupPlanUuid) {
	id, ok := a.require(w, r, auth.PermBackupsManage)
	if !ok {
		return
	}
	_, plan, ok := a.resolveBackupPlan(w, r, id, databaseUuid, backupPlanUuid)
	if !ok {
		return
	}
	if rows, err := a.Store.SoftDeleteBackupPlan(r.Context(), plan.ID); err != nil || rows == 0 {
		a.internalError(w, r, "delete backup plan", err)
		return
	}
	a.recordAudit(r, id, "backup.plan_delete", "backup_plan", plan.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// ExecuteBackupPlan implements POST /databases/{uuid}/backups/{plan}/execute
// (permission: write): long operation — 202 + job.
func (a *API) ExecuteBackupPlan(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid, backupPlanUuid api.BackupPlanUuid, params api.ExecuteBackupPlanParams) {
	id, ok := a.require(w, r, auth.PermBackupsManage)
	if !ok {
		return
	}
	row, plan, ok := a.resolveBackupPlan(w, r, id, databaseUuid, backupPlanUuid)
	if !ok {
		return
	}
	// One backup at a time per plan (§3.1).
	lockKey := "backup:plan:" + uuidString(plan.Uuid)
	if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err != nil {
		a.internalError(w, r, "execute backup", err)
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
		ResourceID:     ptr(row.Resource.ID),
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "execute backup", err)
		return
	}
	a.recordAudit(r, id, "backup.execute", "backup_plan", plan.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

// ListBackupExecutions implements GET
// /databases/{uuid}/backups/{plan}/executions (permission: read).
func (a *API) ListBackupExecutions(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid, backupPlanUuid api.BackupPlanUuid, params api.ListBackupExecutionsParams) {
	id, ok := a.require(w, r, auth.PermBackupsRead)
	if !ok {
		return
	}
	_, plan, ok := a.resolveBackupPlan(w, r, id, databaseUuid, backupPlanUuid)
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
		a.internalError(w, r, "list backup executions", err)
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

// RestoreBackupExecution implements POST
// /databases/{uuid}/backups/{plan}/executions/{execution}/restore
// (permission: write): 202 + job. The dump checksum is verified before any
// data is replayed (§20.5).
func (a *API) RestoreBackupExecution(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid, backupPlanUuid api.BackupPlanUuid, executionUuid api.ExecutionUuid, params api.RestoreBackupExecutionParams) {
	id, ok := a.require(w, r, auth.PermBackupsRestore)
	if !ok {
		return
	}
	row, plan, ok := a.resolveBackupPlan(w, r, id, databaseUuid, backupPlanUuid)
	if !ok {
		return
	}
	// The contract requires confirm=true (§20.5). The check used to be
	// missing: the promise existed in the spec and nowhere else.
	if !a.confirmRestoreBody(w, r) {
		return
	}
	u, ok := a.scanUUID(w, r, executionUuid, "backup execution")
	if !ok {
		return
	}
	res, err := a.Store.GetBackupExecutionByUUID(r.Context(), store.GetBackupExecutionByUUIDParams{
		Uuid: u, BackupPlanID: plan.ID,
	})
	exec, ok := resolveRow(a, w, r, "backup execution", res, err)
	if !ok {
		return
	}
	if exec.Status == store.BackupExecutionStatusFailed || exec.Filename == nil {
		httpapi.WriteError(w, r, http.StatusConflict, "invalid_state", "this backup has no usable dump")
		return
	}

	// A restore overwrites data: it is serialized with the database's own
	// operations (§3.1) and audited (§23.4).
	lockKey := "deploy:db:" + uuidString(row.Resource.Uuid)
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:          "backup",
		Type:           jobs.TypeBackupRestore,
		Payload:        jobs.BackupPayload{PlanID: plan.ID, ExecutionID: exec.ID},
		LockKey:        &lockKey,
		TeamID:         ptr(id.TeamID),
		ResourceID:     ptr(row.Resource.ID),
		MaxAttempts:    1, // never replay a restore blindly
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "restore backup", err)
		return
	}
	a.recordAudit(r, id, "backup.restore", "database", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

// confirmRestoreBody enforces the §20.5 contract: a restore body MUST carry
// confirm=true, anything else is 422. The confirmation is the API-level
// guard against a scripted restore fired by accident.
func (a *API) confirmRestoreBody(w http.ResponseWriter, r *http.Request) bool {
	var body api.RestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return false
	}
	if !body.Confirm {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("confirm"), Code: ptr("required"),
			Message: "a restore overwrites data: the body must carry confirm=true (§20.5)",
		}})
		return false
	}
	return true
}

// retentionCount and retentionDays read the cumulative retention policy of
// §7.2 (0 = unlimited).
func retentionCount(p *api.RetentionPolicy, def int32) int32 {
	if p != nil && p.MaxCount != nil {
		return int32(*p.MaxCount)
	}
	return def
}

func retentionDays(p *api.RetentionPolicy, def int32) int32 {
	if p != nil && p.MaxAgeDays != nil {
		return int32(*p.MaxAgeDays)
	}
	return def
}

// resolveBackupS3Storage validates the S3 destination of a plan: it must
// belong to the team (INV-002) and be usable (§20.5). An s3_only plan without
// a destination would silently keep nothing at all, so it is refused.
func (a *API) resolveBackupS3Storage(w http.ResponseWriter, r *http.Request, id *auth.Identity, storageUUID *string, saveS3, s3Only *bool) (*int64, bool) {
	wantsS3 := (saveS3 != nil && *saveS3) || (s3Only != nil && *s3Only)
	if storageUUID == nil || *storageUUID == "" {
		if wantsS3 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("s3_storage_uuid"), Code: ptr("required"),
				Message: "s3_storage_uuid is required when save_s3 or s3_only is set",
			}})
			return nil, false
		}
		return nil, true
	}
	storage, ok := a.resolveS3Storage(w, r, id, *storageUUID)
	if !ok {
		return nil, false
	}
	if !storage.IsUsable {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("s3_storage_uuid"), Code: ptr("invalid_state"),
			Message: "the S3 storage did not pass its connectivity check — validate it first",
		}})
		return nil, false
	}
	return ptr(storage.ID), true
}

// s3StorageUUIDOf resolves the destination bucket of a plan for rendering.
// A storage that vanished is reported as absent rather than failing the read.
func (a *API) s3StorageUUIDOf(r *http.Request, plan store.DatabaseBackupPlan) *string {
	if plan.S3StorageID == nil {
		return nil
	}
	storage, err := a.Store.GetS3StorageByID(r.Context(), *plan.S3StorageID)
	if err != nil {
		return nil
	}
	return ptr(uuidString(storage.Uuid))
}

// restoreDrillToAPI renders one drill.
func restoreDrillToAPI(d store.RestoreDrill, executionUUID pgtype.UUID) api.RestoreDrill {
	out := api.RestoreDrill{
		Uuid:         ptr(uuidString(d.Uuid)),
		Status:       api.RestoreDrillStatus(d.Status),
		ErrorMessage: d.ErrorMessage,
		StartedAt:    d.StartedAt.Time,
		FinishedAt:   timePtr(d.FinishedAt),
	}
	if executionUUID.Valid {
		out.ExecutionUuid = ptr(uuidString(executionUUID))
	}
	if d.TablesExpected != nil {
		out.TablesExpected = ptr(int(*d.TablesExpected))
	}
	if d.TablesRestored != nil {
		out.TablesRestored = ptr(int(*d.TablesRestored))
	}
	if d.DurationMs != nil {
		out.DurationMs = ptr(int(*d.DurationMs))
	}
	return out
}

// RunRestoreDrill implements POST /databases/{uuid}/backups/{plan}/drill: the
// same path the periodic drill takes (ADR-014).
func (a *API) RunRestoreDrill(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid, backupPlanUuid api.BackupPlanUuid, params api.RunRestoreDrillParams) {
	id, ok := a.require(w, r, auth.PermBackupsRestore)
	if !ok {
		return
	}
	row, plan, ok := a.resolveBackupPlan(w, r, id, databaseUuid, backupPlanUuid)
	if !ok {
		return
	}
	// Nothing to restore means nothing to prove: refusing here is more honest
	// than starting a drill that can only fail for a reason that is not a
	// backup problem.
	if _, err := a.Store.GetLatestSuccessfulBackupExecution(r.Context(), plan.ID); err != nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
			"this plan has no successful backup to restore yet")
		return
	}
	// Shares the plan's lock with the backups: a drill and a backup of the same
	// plan would fight over the dump file the retention may be purging.
	lockKey := "backup:plan:" + uuidString(plan.Uuid)
	if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err != nil {
		a.internalError(w, r, "run restore drill", err)
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
		ResourceID:     ptr(row.Resource.ID),
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "run restore drill", err)
		return
	}
	a.recordAudit(r, id, "backup.drill", "database", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{JobUuid: uuidString(job.Uuid)})
}

// ListRestoreDrills implements GET /databases/{uuid}/backups/{plan}/drills.
func (a *API) ListRestoreDrills(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid, backupPlanUuid api.BackupPlanUuid, params api.ListRestoreDrillsParams) {
	id, ok := a.require(w, r, auth.PermBackupsRead)
	if !ok {
		return
	}
	_, plan, ok := a.resolveBackupPlan(w, r, id, databaseUuid, backupPlanUuid)
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
		a.internalError(w, r, "list restore drills", err)
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
