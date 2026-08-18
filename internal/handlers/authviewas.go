package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// ListViewAsRoles implements GET /auth/session/view-as: the roles this session
// may inspect — the system ones, plus the team's custom roles.
//
// It is a SESSION endpoint, not /api/v1/roles, for one reason that matters: a
// session already inspecting `reviewer` holds no `roles:read`, and would get a
// 403 on the very listing it needs to switch roles or read its own state. The
// authority checked here is the session's real membership, never the narrowed
// permissions it currently presents.
func (a *API) ListViewAsRoles(w http.ResponseWriter, r *http.Request) {
	if a.Sessions == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, err := a.Sessions.SessionFromRequest(r.Context(), r)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "no active session")
		return
	}
	allowed, teamID, err := a.Sessions.MayInspectRoles(r.Context(), sess.UserID, deref64(sess.CurrentTeamID))
	if err != nil {
		a.internalError(w, r, "list inspectable roles", err)
		return
	}
	if !allowed {
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden,
			"inspecting roles is reserved to team admins and the instance root")
		return
	}

	roles := []map[string]any{
		{"role": "admin", "name": "Admin"},
		{"role": "member", "name": "Member"},
		{"role": "reviewer", "name": "Reviewer"},
	}
	customs, err := a.Store.ListCustomRolesPage(r.Context(), store.ListCustomRolesPageParams{
		TeamID: teamID, PageLimit: 100,
	})
	if err != nil {
		a.internalError(w, r, "list inspectable roles", err)
		return
	}
	for _, c := range customs {
		roles = append(roles, map[string]any{"custom_role_uuid": uuidString(c.Uuid), "name": c.Name})
	}

	current := ""
	if identity := a.Sessions.Authenticate(r.Context(), r); identity != nil {
		current = identity.ViewAs
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": roles, "view_as": nullableString(current)})
}

// ViewAs implements POST /auth/session/view-as: enter the role-inspection mode
// (ADR-058), or leave it by sending both fields empty.
//
// Session + CSRF like every mutating /auth endpoint, and audited: a session
// whose permissions change shape mid-life is exactly what an audit reader needs
// to see to make sense of the actions that follow.
func (a *API) ViewAs(w http.ResponseWriter, r *http.Request) {
	if a.Sessions == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, ok := a.sessionUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Role           string `json:"role"`
		CustomRoleUUID string `json:"custom_role_uuid"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid body")
		return
	}

	label, err := a.Sessions.SetViewAs(r.Context(), sess.UserID, sess.ID,
		deref64(sess.CurrentTeamID), body.Role, body.CustomRoleUUID)
	switch {
	case errors.Is(err, session.ErrNotAllowedToViewAs):
		a.auditAuth(r, "auth.session.view_as", store.AuditResultDenied, sess.UserID, sess.Email, sess.CurrentTeamID)
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden,
			"inspecting roles is reserved to team admins and the instance root")
		return
	case errors.Is(err, session.ErrRoleNotFound):
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "role not found")
		return
	case err != nil:
		a.internalError(w, r, "set view-as role", err)
		return
	}

	a.auditAuth(r, "auth.session.view_as", store.AuditResultSuccess, sess.UserID, sess.Email, sess.CurrentTeamID)
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"view_as": nullableString(label)})
}

// deref64 reads a nullable team id as "no preference" when absent.
func deref64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// nullableString renders an empty label as JSON null: the dashboard tests
// `view_as === null` for "acting as myself", and an empty string would read as
// a role whose name happens to be blank.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
