package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

// auditEventToAPI renders one audit row. Sensitive values never appear: the diff
// is stored already redacted (INV-003), so it is passed through as-is.
func auditEventToAPI(e store.AuditEvent) api.AuditEvent {
	out := api.AuditEvent{
		Uuid:         uuidString(e.Uuid),
		OccurredAt:   e.OccurredAt.Time.UTC(),
		ActorKind:    ptr(api.AuditEventActorKind(e.ActorKind)),
		ActorDisplay: e.ActorDisplay,
		Action:       e.Action,
		TargetKind:   e.TargetKind,
		Result:       api.AuditEventResult(e.Result),
		UserAgent:    e.UserAgent,
	}
	if e.ActorUuid.Valid {
		out.ActorUuid = ptr(uuidString(e.ActorUuid))
	}
	if e.TargetUuid.Valid {
		out.TargetUuid = ptr(uuidString(e.TargetUuid))
	}
	if e.Ip != nil {
		out.Ip = ptr(e.Ip.String())
	}
	if len(e.DiffRedacted) > 0 {
		var diff map[string]any
		if err := json.Unmarshal(e.DiffRedacted, &diff); err == nil {
			out.Diff = &diff
		}
	}
	return out
}

// ListInstanceAudit implements GET /system/audit (§23.4): the instance-wide
// audit trail, reserved to the instance root — it includes the system/instance
// actions that carry no team and so appear in no team-scoped view.
func (a *API) ListInstanceAudit(w http.ResponseWriter, r *http.Request, params api.ListInstanceAuditParams) {
	if _, ok := a.requireInstanceRoot(w, r); !ok {
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

	qp := store.ListInstanceAuditEventsPageParams{AfterID: after, PageLimit: limit + 1}
	if params.Action != nil && *params.Action != "" {
		qp.Action = params.Action
	}
	if params.Result != nil {
		res := store.AuditResult(*params.Result)
		qp.Result = &res
	}
	if params.ActorUuid != nil && *params.ActorUuid != "" {
		if err := qp.ActorUuid.Scan(*params.ActorUuid); err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("actor_uuid"), Code: ptr("invalid"), Message: "actor_uuid must be a UUID"}})
			return
		}
	}
	if params.From != nil {
		qp.FromTime = pgtype.Timestamptz{Time: *params.From, Valid: true}
	}
	if params.To != nil {
		qp.ToTime = pgtype.Timestamptz{Time: *params.To, Valid: true}
	}

	rows, err := a.Store.ListInstanceAuditEventsPage(r.Context(), qp)
	if err != nil {
		a.internalError(w, r, "list instance audit events", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(e store.AuditEvent) int64 { return e.ID })
	data := make([]api.AuditEvent, 0, len(rows))
	for _, e := range rows {
		data = append(data, auditEventToAPI(e))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.AuditEvent `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}{data, cursor})
}

// ListTeamAudit implements GET /teams/{team_uuid}/audit (§23.4): the append-only
// audit trail, paginated, filtered and scriptable. Read-only — no mutation of
// the trail is ever exposed.
func (a *API) ListTeamAudit(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, params api.ListTeamAuditParams) {
	id, ok := a.require(w, r, auth.PermAuditRead)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
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

	qp := store.ListAuditEventsPageParams{TeamID: &team.ID, AfterID: after, PageLimit: limit + 1}
	if params.Action != nil && *params.Action != "" {
		qp.Action = params.Action
	}
	if params.Result != nil {
		res := store.AuditResult(*params.Result)
		qp.Result = &res
	}
	if params.ActorUuid != nil && *params.ActorUuid != "" {
		if err := qp.ActorUuid.Scan(*params.ActorUuid); err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("actor_uuid"), Code: ptr("invalid"), Message: "actor_uuid must be a UUID"}})
			return
		}
	}
	if params.TargetUuid != nil && *params.TargetUuid != "" {
		if err := qp.TargetUuid.Scan(*params.TargetUuid); err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("target_uuid"), Code: ptr("invalid"), Message: "target_uuid must be a UUID"}})
			return
		}
	}
	if params.From != nil {
		qp.FromTime = pgtype.Timestamptz{Time: *params.From, Valid: true}
	}
	if params.To != nil {
		qp.ToTime = pgtype.Timestamptz{Time: *params.To, Valid: true}
	}

	rows, err := a.Store.ListAuditEventsPage(r.Context(), qp)
	if err != nil {
		a.internalError(w, r, "list audit events", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(e store.AuditEvent) int64 { return e.ID })
	data := make([]api.AuditEvent, 0, len(rows))
	for _, e := range rows {
		data = append(data, auditEventToAPI(e))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.AuditEvent `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}{data, cursor})
}
