package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// Shared variables (§5.4, §3.1): hierarchical values interpolated into
// resource variables at deploy time. The value is envelope-encrypted and
// only revealed with read:sensitive (INV-003), like any other variable.

var sharedKeyFormat = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (a *API) sharedVariableToAPI(r *http.Request, id *auth.Identity, v store.SharedVariable) api.SharedVariable {
	var value *string
	if auth.Has(id.Permissions, auth.PermReadSensitive) {
		if plaintext, err := a.Keyring.Decrypt("shared_variables", "value_enc", uuidString(v.Uuid), v.ValueEnc); err == nil {
			value = ptr(string(plaintext))
		}
	}
	out := api.SharedVariable{
		Uuid:       ptr(uuidString(v.Uuid)),
		Scope:      api.SharedVariableScope(v.Scope),
		Key:        v.Key,
		Value:      value,
		IsRedacted: ptr(value == nil),
		IsSecret:   v.IsSecret,
		CreatedAt:  timePtr(v.CreatedAt),
		UpdatedAt:  timePtr(v.UpdatedAt),
	}
	// Parents rendered as uuids, best effort — a vanished parent means the
	// row is about to CASCADE away anyway.
	if v.ProjectID != nil {
		if p, err := a.Store.GetProjectByID(r.Context(), *v.ProjectID); err == nil {
			out.ProjectUuid = ptr(uuidString(p.Uuid))
		}
	}
	if v.EnvironmentID != nil {
		if e, err := a.Store.GetEnvironmentByID(r.Context(), *v.EnvironmentID); err == nil {
			out.EnvironmentUuid = ptr(uuidString(e.Uuid))
		}
	}
	if v.ServerID != nil {
		if s, err := a.Store.GetServerByID(r.Context(), *v.ServerID); err == nil {
			out.ServerUuid = ptr(uuidString(s.Uuid))
		}
	}
	return out
}

// ListSharedVariables implements GET /shared-variables (permission: read).
func (a *API) ListSharedVariables(w http.ResponseWriter, r *http.Request, params api.ListSharedVariablesParams) {
	id, ok := a.require(w, r, auth.PermSecretsRead)
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
	var scope *store.SharedVariableScope
	if params.Scope != nil {
		s := store.SharedVariableScope(*params.Scope)
		scope = &s
	}
	rows, err := a.Store.ListSharedVariablesPage(r.Context(), store.ListSharedVariablesPageParams{
		TeamID: id.TeamID, Scope: scope, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list shared variables", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(v store.SharedVariable) int64 { return v.ID })
	data := make([]api.SharedVariable, 0, len(rows))
	for _, v := range rows {
		data = append(data, a.sharedVariableToAPI(r, id, v))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.SharedVariable `json:"data"`
		NextCursor *string              `json:"next_cursor"`
	}{data, cursor})
}

// CreateSharedVariable implements POST /shared-variables (permission: write).
func (a *API) CreateSharedVariable(w http.ResponseWriter, r *http.Request, params api.CreateSharedVariableParams) {
	id, ok := a.require(w, r, auth.PermSecretsWrite)
	if !ok {
		return
	}
	var body api.SharedVariableCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	if !sharedKeyFormat.MatchString(body.Key) {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("key"), Code: ptr("invalid"),
			Message: "key must match [A-Za-z_][A-Za-z0-9_]* (INV-012)",
		}})
		return
	}

	// The scope names its parent — required, team-scoped, resolved with the
	// uniform 404-as-422 semantics of a bad reference.
	var projectID, environmentID, serverID *int64
	requireParent := func(field string, given *string) (pgtype.UUID, bool) {
		if given == nil || *given == "" {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr(field), Code: ptr("required"),
				Message: field + " is required for this scope",
			}})
			return pgtype.UUID{}, false
		}
		var u pgtype.UUID
		if err := u.Scan(*given); err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr(field), Code: ptr("invalid"), Message: "unknown " + field,
			}})
			return pgtype.UUID{}, false
		}
		return u, true
	}
	switch body.Scope {
	case api.SharedVariableCreateScopeTeam:
		// no parent
	case api.SharedVariableCreateScopeProject:
		u, ok := requireParent("project_uuid", body.ProjectUuid)
		if !ok {
			return
		}
		p, err := a.Store.GetProjectByUUID(r.Context(), store.GetProjectByUUIDParams{Uuid: u, TeamID: id.TeamID})
		if err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("project_uuid"), Code: ptr("invalid"), Message: "unknown project"}})
			return
		}
		projectID = &p.ID
	case api.SharedVariableCreateScopeEnvironment:
		u, ok := requireParent("environment_uuid", body.EnvironmentUuid)
		if !ok {
			return
		}
		e, err := a.Store.GetEnvironmentByUUIDForTeam(r.Context(), store.GetEnvironmentByUUIDForTeamParams{Uuid: u, TeamID: id.TeamID})
		if err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("environment_uuid"), Code: ptr("invalid"), Message: "unknown environment"}})
			return
		}
		environmentID = &e.ID
	case api.SharedVariableCreateScopeServer:
		u, ok := requireParent("server_uuid", body.ServerUuid)
		if !ok {
			return
		}
		s, err := a.Store.GetServerByUUID(r.Context(), store.GetServerByUUIDParams{Uuid: u, TeamID: id.TeamID})
		if err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("server_uuid"), Code: ptr("invalid"), Message: "unknown server"}})
			return
		}
		serverID = &s.ID
	default:
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("scope"), Code: ptr("invalid"), Message: "scope must be team, project, environment or server"}})
		return
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create shared variable", err)
		return
	}
	enc, err := a.Keyring.Encrypt("shared_variables", "value_enc", pguuid.String(u), []byte(body.Value))
	if err != nil {
		a.internalError(w, r, "create shared variable", err)
		return
	}
	isSecret := false
	if body.IsSecret != nil {
		isSecret = *body.IsSecret
	}
	v, err := a.Store.CreateSharedVariable(r.Context(), store.CreateSharedVariableParams{
		Uuid: u, TeamID: id.TeamID, Scope: store.SharedVariableScope(body.Scope),
		ProjectID: projectID, EnvironmentID: environmentID, ServerID: serverID,
		Key: body.Key, ValueEnc: enc, IsSecret: isSecret,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this key already exists at this scope")
			return
		}
		a.internalError(w, r, "create shared variable", err)
		return
	}
	a.recordAudit(r, id, "shared_variable.create", "shared_variable", v.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, a.sharedVariableToAPI(r, id, v))
}

// UpdateSharedVariable implements PATCH /shared-variables/{uuid}
// (permission: write). Key and scope are identity — immutable.
func (a *API) UpdateSharedVariable(w http.ResponseWriter, r *http.Request, sharedVariableUuid api.SharedVariableUuid) {
	id, ok := a.require(w, r, auth.PermSecretsWrite)
	if !ok {
		return
	}
	u, ok := a.scanUUID(w, r, sharedVariableUuid, "shared variable")
	if !ok {
		return
	}
	row, err := a.Store.GetSharedVariableByUUID(r.Context(), store.GetSharedVariableByUUIDParams{Uuid: u, TeamID: id.TeamID})
	v, ok := resolveRow(a, w, r, "shared variable", row, err)
	if !ok {
		return
	}
	var body api.SharedVariableUpdate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	valueEnc := v.ValueEnc
	if body.Value != nil {
		valueEnc, err = a.Keyring.Encrypt("shared_variables", "value_enc", uuidString(v.Uuid), []byte(*body.Value))
		if err != nil {
			a.internalError(w, r, "update shared variable", err)
			return
		}
	}
	isSecret := v.IsSecret
	if body.IsSecret != nil {
		isSecret = *body.IsSecret
	}
	if _, err := a.Store.UpdateSharedVariable(r.Context(), store.UpdateSharedVariableParams{
		ID: v.ID, ValueEnc: valueEnc, IsSecret: isSecret,
	}); err != nil {
		a.internalError(w, r, "update shared variable", err)
		return
	}
	updated, err := a.Store.GetSharedVariableByUUID(r.Context(), store.GetSharedVariableByUUIDParams{Uuid: v.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "reload shared variable", err)
		return
	}
	a.recordAudit(r, id, "shared_variable.update", "shared_variable", v.Uuid)
	httpapi.WriteJSON(w, http.StatusOK, a.sharedVariableToAPI(r, id, updated))
}

// DeleteSharedVariable implements DELETE /shared-variables/{uuid}
// (permission: write).
func (a *API) DeleteSharedVariable(w http.ResponseWriter, r *http.Request, sharedVariableUuid api.SharedVariableUuid) {
	id, ok := a.require(w, r, auth.PermSecretsWrite)
	if !ok {
		return
	}
	u, ok := a.scanUUID(w, r, sharedVariableUuid, "shared variable")
	if !ok {
		return
	}
	row, err := a.Store.GetSharedVariableByUUID(r.Context(), store.GetSharedVariableByUUIDParams{Uuid: u, TeamID: id.TeamID})
	v, ok := resolveRow(a, w, r, "shared variable", row, err)
	if !ok {
		return
	}
	if _, err := a.Store.DeleteSharedVariable(r.Context(), v.ID); err != nil {
		a.internalError(w, r, "delete shared variable", err)
		return
	}
	a.recordAudit(r, id, "shared_variable.delete", "shared_variable", v.Uuid)
	w.WriteHeader(http.StatusNoContent)
}
