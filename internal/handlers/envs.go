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

var envKeyFormat = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// envToAPI renders a variable, revealing the value only when INV-003
// allows: read:sensitive AND not locked (locked values are write-only for
// everyone).
func (a *API) envToAPI(id *auth.Identity, v store.EnvironmentVariable) api.EnvironmentVariable {
	var value *string
	if !v.IsLocked && auth.Has(id.Permissions, auth.PermReadSensitive) {
		if plaintext, err := a.Keyring.Decrypt("environment_variables", "value_enc", uuidString(v.Uuid), v.ValueEnc); err == nil {
			value = ptr(string(plaintext))
		}
	}
	return api.EnvironmentVariable{
		Uuid:              ptr(uuidString(v.Uuid)),
		IsPreviewOverride: ptr(v.PreviewID != nil),
		Key:               v.Key,
		Value:             value,
		IsRedacted:        ptr(value == nil),
		IsBuildTime:       v.IsBuildTime,
		IsSecret:          ptr(v.IsSecret),
		IsLiteral:         v.IsLiteral,
		IsMultiline:       v.IsMultiline,
		IsLocked:          v.IsLocked,
		CreatedAt:         timePtr(v.CreatedAt),
		UpdatedAt:         timePtr(v.UpdatedAt),
	}
}

// ListApplicationEnvs implements GET /applications/{uuid}/envs
// (permission: read).
func (a *API) ListApplicationEnvs(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.ListApplicationEnvsParams) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
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
	// The preview set is a DIFFERENT set (INV-010), not a filter on the same
	// one: it is what a preview instance runs with, and where the generated
	// preview credentials live.
	preview := params.Preview != nil && *params.Preview
	rows, err := a.Store.ListEnvVarsPage(r.Context(), store.ListEnvVarsPageParams{
		ResourceID: row.Resource.ID, IsPreview: preview, AfterID: after, PageLimit: limit + 1,
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
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.EnvironmentVariable `json:"data"`
		NextCursor *string                   `json:"next_cursor"`
	}{data, cursor})
}

// CreateApplicationEnv implements POST /applications/{uuid}/envs
// (permission: write). Values are envelope encrypted at rest, whatever
// their sensitivity (data-dictionary §8.5).
func (a *API) CreateApplicationEnv(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.CreateApplicationEnvParams) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	var body api.EnvironmentVariableCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	// preview=true writes into the DEDICATED preview set (INV-010): the only
	// way a PR instance gets its keys — production values are never copied.
	preview := params.Preview != nil && *params.Preview
	created, err := a.insertEnvVar(r, row.Resource.ID, body, preview, nil)
	if err != nil {
		a.writeEnvError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusCreated, a.envToAPI(id, created))
}

// ReplaceApplicationEnvs implements PUT /applications/{uuid}/envs
// (permission: write): full replacement — absent variables are DELETED,
// except locked ones which survive (§5.4).
func (a *API) ReplaceApplicationEnvs(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	var body struct {
		Data []api.EnvironmentVariableCreate `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	keys := make([]string, 0, len(body.Data))
	for _, item := range body.Data {
		keys = append(keys, item.Key)
	}
	if err := a.Store.DeleteEnvVarsNotInKeys(r.Context(), store.DeleteEnvVarsNotInKeysParams{
		ResourceID: row.Resource.ID, Keys: keys,
	}); err != nil {
		a.internalError(w, r, "replace envs", err)
		return
	}
	for _, item := range body.Data {
		existing, err := a.Store.GetEnvVarByKey(r.Context(), store.GetEnvVarByKeyParams{ResourceID: row.Resource.ID, Key: item.Key})
		if err == nil {
			if existing.IsLocked {
				continue // locked variables are not re-editable in bulk (§5.4)
			}
			value := ""
			if item.Value != nil {
				value = *item.Value
			}
			if _, err := a.updateEnvVar(r, existing, value, item.IsBuildTime, item.IsLiteral, item.IsMultiline, item.IsLocked); err != nil {
				a.writeEnvError(w, r, err)
				return
			}
			continue
		}
		if _, err := a.insertEnvVar(r, row.Resource.ID, item, false, nil); err != nil {
			a.writeEnvError(w, r, err)
			return
		}
	}

	rows, err := a.Store.ListEnvVarsForDeploy(r.Context(), row.Resource.ID)
	if err != nil {
		a.internalError(w, r, "replace envs", err)
		return
	}
	data := make([]api.EnvironmentVariable, 0, len(rows))
	for _, v := range rows {
		data = append(data, a.envToAPI(id, v))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data []api.EnvironmentVariable `json:"data"`
	}{data})
}

// UpdateApplicationEnv implements PATCH /applications/{uuid}/envs/{env_uuid}
// (permission: write). Locked variables are never re-editable (§5.4).
func (a *API) UpdateApplicationEnv(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, envUuid api.EnvUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	v, ok := a.resolveEnvVar(w, r, row.Resource.ID, envUuid)
	if !ok {
		return
	}
	if v.IsLocked {
		httpapi.WriteError(w, r, http.StatusConflict, "invalid_state", "a locked variable cannot be edited — delete and recreate it (§5.4)")
		return
	}
	var body api.EnvironmentVariableUpdate
	patch, ok := decodePatch(w, r, &body)
	if !ok {
		return
	}
	if body.IsLocked != nil && !*body.IsLocked && v.IsLocked {
		httpapi.WriteError(w, r, http.StatusConflict, "invalid_state", "a variable cannot be unlocked")
		return
	}

	value := ""
	if body.Value != nil {
		value = *body.Value
	} else if !patch.Has("value") {
		plaintext, err := a.Keyring.Decrypt("environment_variables", "value_enc", uuidString(v.Uuid), v.ValueEnc)
		if err != nil {
			a.internalError(w, r, "update env", err)
			return
		}
		value = string(plaintext)
	}
	pick := func(p *bool, current bool) bool {
		if p != nil {
			return *p
		}
		return current
	}
	updated, err := a.updateEnvVar(r, v, value,
		ptr(pick(body.IsBuildTime, v.IsBuildTime)),
		ptr(pick(body.IsLiteral, v.IsLiteral)),
		ptr(pick(body.IsMultiline, v.IsMultiline)),
		ptr(pick(body.IsLocked, v.IsLocked)))
	if err != nil {
		a.writeEnvError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, a.envToAPI(id, updated))
}

// DeleteApplicationEnv implements DELETE
// /applications/{uuid}/envs/{env_uuid} (permission: write).
func (a *API) DeleteApplicationEnv(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, envUuid api.EnvUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
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
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---------------------------------------------------------------

type envValidationError struct{ detail api.ErrorDetail }

func (e *envValidationError) Error() string { return e.detail.Message }

type envConflictError struct{}

func (e *envConflictError) Error() string { return "duplicate key" }

func (a *API) insertEnvVar(r *http.Request, resourceID int64, item api.EnvironmentVariableCreate, preview bool, previewID *int64) (store.EnvironmentVariable, error) {
	if !envKeyFormat.MatchString(item.Key) || len(item.Key) > 255 {
		return store.EnvironmentVariable{}, &envValidationError{api.ErrorDetail{Field: ptr("key"), Code: ptr("invalid"), Message: "key must match [A-Za-z_][A-Za-z0-9_]* and be at most 255 characters"}}
	}
	if item.Value == nil {
		return store.EnvironmentVariable{}, &envValidationError{api.ErrorDetail{Field: ptr("value"), Code: ptr("required"), Message: "value is required"}}
	}
	u, err := pguuid.New()
	if err != nil {
		return store.EnvironmentVariable{}, err
	}
	enc, err := a.Keyring.Encrypt("environment_variables", "value_enc", pguuid.String(u), []byte(*item.Value))
	if err != nil {
		return store.EnvironmentVariable{}, err
	}
	boolOr := func(p *bool) bool { return p != nil && *p }
	created, err := a.Store.CreateEnvVar(r.Context(), store.CreateEnvVarParams{
		Uuid: u, ResourceID: resourceID, Key: item.Key, ValueEnc: enc,
		IsBuildTime: boolOr(item.IsBuildTime), IsLiteral: boolOr(item.IsLiteral),
		IsMultiline: boolOr(item.IsMultiline), IsLocked: boolOr(item.IsLocked),
		// is_secret decides HOW a build-time variable reaches the build: a plain
		// one becomes an ARG (and lands in `docker history`), a secret one is
		// mounted by BuildKit and never enters a layer (§5.2, INV-003).
		IsSecret:  boolOr(item.IsSecret),
		IsPreview: preview,
		PreviewID: previewID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return store.EnvironmentVariable{}, &envConflictError{}
		}
		return store.EnvironmentVariable{}, err
	}
	return created, nil
}

func (a *API) updateEnvVar(r *http.Request, v store.EnvironmentVariable, value string, isBuildTime, isLiteral, isMultiline, isLocked *bool) (store.EnvironmentVariable, error) {
	enc, err := a.Keyring.Encrypt("environment_variables", "value_enc", uuidString(v.Uuid), []byte(value))
	if err != nil {
		return store.EnvironmentVariable{}, err
	}
	boolOr := func(p *bool, cur bool) bool {
		if p != nil {
			return *p
		}
		return cur
	}
	return a.Store.UpdateEnvVar(r.Context(), store.UpdateEnvVarParams{
		ID: v.ID, ValueEnc: enc,
		IsBuildTime: boolOr(isBuildTime, v.IsBuildTime), IsLiteral: boolOr(isLiteral, v.IsLiteral),
		IsMultiline: boolOr(isMultiline, v.IsMultiline), IsLocked: boolOr(isLocked, v.IsLocked),
	})
}

func (a *API) resolveEnvVar(w http.ResponseWriter, r *http.Request, resourceID int64, envUUID string) (store.EnvironmentVariable, bool) {
	var u pgtype.UUID
	if err := u.Scan(envUUID); err == nil {
		v, err := a.Store.GetEnvVarByUUID(r.Context(), store.GetEnvVarByUUIDParams{Uuid: u, ResourceID: resourceID})
		if err == nil {
			return v, true
		}
	}
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "environment variable not found")
	return store.EnvironmentVariable{}, false
}

func (a *API) writeEnvError(w http.ResponseWriter, r *http.Request, err error) {
	switch e := err.(type) {
	case *envValidationError:
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{e.detail})
	case *envConflictError:
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a variable with this key already exists")
	default:
		a.internalError(w, r, "env variable", err)
	}
}
