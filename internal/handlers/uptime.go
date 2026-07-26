package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// Uptime monitoring (ADR-017): CRUD of the checks and their raw history.
// The probing itself lives in the scheduler — these handlers never touch
// the network.

func uptimeCheckToAPI(c store.UptimeCheck, resourceUUID *string) api.UptimeCheck {
	out := api.UptimeCheck{
		Uuid:             ptr(uuidString(c.Uuid)),
		ResourceUuid:     resourceUUID,
		Name:             c.Name,
		Kind:             api.UptimeCheckKind(c.Kind),
		Target:           c.Target,
		IntervalSeconds:  int(c.IntervalSeconds),
		TimeoutSeconds:   int(c.TimeoutSeconds),
		FailureThreshold: int(c.FailureThreshold),
		SuccessThreshold: int(c.SuccessThreshold),
		Enabled:          c.Enabled,
		Status:           ptr(api.UptimeCheckStatus(c.Status)),
		StatusSince:      timePtr(c.StatusSince),
		LastCheckedAt:    timePtr(c.LastCheckedAt),
		LastError:        c.LastError,
		Version:          ptr(int(c.Version)),
		CreatedAt:        timePtr(c.CreatedAt),
	}
	if c.LastLatencyMs != nil {
		out.LastLatencyMs = ptr(int(*c.LastLatencyMs))
	}
	return out
}

// validateUptimeTarget refuses a target the prober could never speak to —
// at the edge, where the operator sees it, not at the first silent probe.
func validateUptimeTarget(kind, target string) *api.ErrorDetail {
	switch kind {
	case "http":
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return &api.ErrorDetail{Field: ptr("target"), Code: ptr("invalid"),
				Message: "an http check needs an absolute http(s) URL"}
		}
	case "tcp":
		host, port, ok := strings.Cut(target, ":")
		if !ok || host == "" {
			return &api.ErrorDetail{Field: ptr("target"), Code: ptr("invalid"),
				Message: "a tcp check needs a host:port target"}
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return &api.ErrorDetail{Field: ptr("target"), Code: ptr("invalid"),
				Message: "a tcp check needs a valid port (1-65535)"}
		}
	default:
		return &api.ErrorDetail{Field: ptr("kind"), Code: ptr("invalid"), Message: "kind must be http or tcp"}
	}
	return nil
}

func (a *API) resolveUptimeCheck(w http.ResponseWriter, r *http.Request, id *auth.Identity, checkUUID string) (store.UptimeCheck, bool) {
	var u pgtype.UUID
	if err := u.Scan(checkUUID); err == nil {
		check, err := a.Store.GetUptimeCheckByUUID(r.Context(), store.GetUptimeCheckByUUIDParams{Uuid: u, TeamID: id.TeamID})
		if err == nil {
			return check, true
		}
	}
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "uptime check not found")
	return store.UptimeCheck{}, false
}

// resourceUUIDOf renders the optional resource link of a check.
func (a *API) uptimeResourceUUIDOf(r *http.Request, c store.UptimeCheck) *string {
	if c.ResourceID == nil {
		return nil
	}
	resource, err := a.Store.GetResourceByID(r.Context(), *c.ResourceID)
	if err != nil {
		return nil
	}
	return ptr(uuidString(resource.Uuid))
}

// ListUptimeChecks implements GET /uptime-checks (permission: read).
func (a *API) ListUptimeChecks(w http.ResponseWriter, r *http.Request, params api.ListUptimeChecksParams) {
	id, ok := a.require(w, r, auth.PermUptimeRead)
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
	rows, err := a.Store.ListUptimeChecksPage(r.Context(), store.ListUptimeChecksPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list uptime checks", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(c store.UptimeCheck) int64 { return c.ID })
	data := make([]api.UptimeCheck, 0, len(rows))
	for _, c := range rows {
		data = append(data, uptimeCheckToAPI(c, a.uptimeResourceUUIDOf(r, c)))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.UptimeCheck `json:"data"`
		NextCursor *string           `json:"next_cursor"`
	}{data, cursor})
}

// CreateUptimeCheck implements POST /uptime-checks (permission: write).
func (a *API) CreateUptimeCheck(w http.ResponseWriter, r *http.Request, params api.CreateUptimeCheckParams) {
	id, ok := a.require(w, r, auth.PermUptimeManage)
	if !ok {
		return
	}
	var body api.UptimeCheckCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	var details []api.ErrorDetail
	if body.Name == "" || len(body.Name) > 255 {
		details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"})
	}
	if d := validateUptimeTarget(string(body.Kind), body.Target); d != nil {
		details = append(details, *d)
	}
	intOr := func(p *int, def, min, max int, field string) int32 {
		if p == nil {
			return int32(def)
		}
		if *p < min || *p > max {
			details = append(details, api.ErrorDetail{Field: ptr(field), Code: ptr("out_of_range"),
				Message: field + " must be between " + strconv.Itoa(min) + " and " + strconv.Itoa(max)})
			return int32(def)
		}
		return int32(*p)
	}
	interval := intOr(body.IntervalSeconds, 60, 10, 86400, "interval_seconds")
	timeout := intOr(body.TimeoutSeconds, 10, 1, 60, "timeout_seconds")
	failThreshold := intOr(body.FailureThreshold, 3, 1, 100, "failure_threshold")
	successThreshold := intOr(body.SuccessThreshold, 2, 1, 100, "success_threshold")

	var resourceID *int64
	if body.ResourceUuid != nil && *body.ResourceUuid != "" {
		var ru pgtype.UUID
		if err := ru.Scan(*body.ResourceUuid); err != nil {
			details = append(details, api.ErrorDetail{Field: ptr("resource_uuid"), Code: ptr("invalid"), Message: "unknown resource"})
		} else if resource, err := a.Store.GetResourceByUUIDForTeam(r.Context(), store.GetResourceByUUIDForTeamParams{Uuid: ru, TeamID: id.TeamID}); err != nil {
			details = append(details, api.ErrorDetail{Field: ptr("resource_uuid"), Code: ptr("invalid"), Message: "unknown resource"})
		} else {
			resourceID = &resource.ID
		}
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create uptime check", err)
		return
	}
	check, err := a.Store.CreateUptimeCheck(r.Context(), store.CreateUptimeCheckParams{
		Uuid: u, TeamID: id.TeamID, ResourceID: resourceID,
		Name: body.Name, Kind: store.UptimeCheckKind(body.Kind), Target: body.Target,
		IntervalSeconds: interval, TimeoutSeconds: timeout,
		FailureThreshold: failThreshold, SuccessThreshold: successThreshold,
		Enabled: enabled,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "an uptime check with this name already exists in this team")
			return
		}
		a.internalError(w, r, "create uptime check", err)
		return
	}
	a.recordAudit(r, id, "uptime.check_create", "uptime_check", check.Uuid)
	w.Header().Set("ETag", etagFor(check.Version))
	httpapi.WriteJSON(w, http.StatusCreated, uptimeCheckToAPI(check, a.uptimeResourceUUIDOf(r, check)))
}

// GetUptimeCheck implements GET /uptime-checks/{uuid} (permission: read).
func (a *API) GetUptimeCheck(w http.ResponseWriter, r *http.Request, uptimeCheckUuid api.UptimeCheckUuid) {
	id, ok := a.require(w, r, auth.PermUptimeRead)
	if !ok {
		return
	}
	check, ok := a.resolveUptimeCheck(w, r, id, uptimeCheckUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(check.Version))
	httpapi.WriteJSON(w, http.StatusOK, uptimeCheckToAPI(check, a.uptimeResourceUUIDOf(r, check)))
}

// UpdateUptimeCheck implements PATCH /uptime-checks/{uuid} (permission: write).
func (a *API) UpdateUptimeCheck(w http.ResponseWriter, r *http.Request, uptimeCheckUuid api.UptimeCheckUuid, params api.UpdateUptimeCheckParams) {
	id, ok := a.require(w, r, auth.PermUptimeManage)
	if !ok {
		return
	}
	check, ok := a.resolveUptimeCheck(w, r, id, uptimeCheckUuid)
	if !ok {
		return
	}
	expected, err := strconv.Atoi(strings.Trim(strings.TrimSpace(params.IfMatch), `"`))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid If-Match header")
		return
	}
	var body api.UptimeCheckUpdate
	if _, ok := decodePatch(w, r, &body); !ok {
		return
	}

	next := check
	var details []api.ErrorDetail
	if body.Name != nil {
		if *body.Name == "" || len(*body.Name) > 255 {
			details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"})
		}
		next.Name = *body.Name
	}
	if body.Target != nil {
		next.Target = *body.Target
	}
	if d := validateUptimeTarget(string(check.Kind), next.Target); d != nil {
		details = append(details, *d)
	}
	applyInt := func(p *int, cur int32, min, max int, field string) int32 {
		if p == nil {
			return cur
		}
		if *p < min || *p > max {
			details = append(details, api.ErrorDetail{Field: ptr(field), Code: ptr("out_of_range"),
				Message: field + " must be between " + strconv.Itoa(min) + " and " + strconv.Itoa(max)})
			return cur
		}
		return int32(*p)
	}
	next.IntervalSeconds = applyInt(body.IntervalSeconds, check.IntervalSeconds, 10, 86400, "interval_seconds")
	next.TimeoutSeconds = applyInt(body.TimeoutSeconds, check.TimeoutSeconds, 1, 60, "timeout_seconds")
	next.FailureThreshold = applyInt(body.FailureThreshold, check.FailureThreshold, 1, 100, "failure_threshold")
	next.SuccessThreshold = applyInt(body.SuccessThreshold, check.SuccessThreshold, 1, 100, "success_threshold")
	if body.Enabled != nil {
		next.Enabled = *body.Enabled
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	rows, err := a.Store.UpdateUptimeCheck(r.Context(), store.UpdateUptimeCheckParams{
		ID: check.ID, Name: next.Name, Target: next.Target,
		IntervalSeconds: next.IntervalSeconds, TimeoutSeconds: next.TimeoutSeconds,
		FailureThreshold: next.FailureThreshold, SuccessThreshold: next.SuccessThreshold,
		Enabled: next.Enabled, ExpectedVersion: int32(expected),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "an uptime check with this name already exists in this team")
			return
		}
		a.internalError(w, r, "update uptime check", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, check.Version)
		return
	}
	updated, err := a.Store.GetUptimeCheckByUUID(r.Context(), store.GetUptimeCheckByUUIDParams{Uuid: check.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "reload uptime check", err)
		return
	}
	w.Header().Set("ETag", etagFor(updated.Version))
	httpapi.WriteJSON(w, http.StatusOK, uptimeCheckToAPI(updated, a.uptimeResourceUUIDOf(r, updated)))
}

// DeleteUptimeCheck implements DELETE /uptime-checks/{uuid} (permission: write).
func (a *API) DeleteUptimeCheck(w http.ResponseWriter, r *http.Request, uptimeCheckUuid api.UptimeCheckUuid) {
	id, ok := a.require(w, r, auth.PermUptimeManage)
	if !ok {
		return
	}
	check, ok := a.resolveUptimeCheck(w, r, id, uptimeCheckUuid)
	if !ok {
		return
	}
	if rows, err := a.Store.SoftDeleteUptimeCheck(r.Context(), check.ID); err != nil || rows == 0 {
		a.internalError(w, r, "delete uptime check", err)
		return
	}
	a.recordAudit(r, id, "uptime.check_delete", "uptime_check", check.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// ListUptimeResults implements GET /uptime-checks/{uuid}/results
// (permission: read).
func (a *API) ListUptimeResults(w http.ResponseWriter, r *http.Request, uptimeCheckUuid api.UptimeCheckUuid, params api.ListUptimeResultsParams) {
	id, ok := a.require(w, r, auth.PermUptimeRead)
	if !ok {
		return
	}
	check, ok := a.resolveUptimeCheck(w, r, id, uptimeCheckUuid)
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
	rows, err := a.Store.ListUptimeResultsPage(r.Context(), store.ListUptimeResultsPageParams{
		CheckID: check.ID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list uptime results", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(res store.UptimeCheckResult) int64 { return res.ID })
	data := make([]api.UptimeResult, 0, len(rows))
	for _, res := range rows {
		item := api.UptimeResult{
			Ok:        res.Ok,
			CheckedAt: res.CheckedAt.Time.UTC(),
			Error:     res.Error,
		}
		if res.LatencyMs != nil {
			item.LatencyMs = ptr(int(*res.LatencyMs))
		}
		if res.StatusCode != nil {
			item.StatusCode = ptr(int(*res.StatusCode))
		}
		data = append(data, item)
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.UptimeResult `json:"data"`
		NextCursor *string            `json:"next_cursor"`
	}{data, cursor})
}
