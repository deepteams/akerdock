package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

// Custom roles (ADR-038): a team composes named permission sets from the
// granular catalogue. Enforcement is unchanged — a member carrying a custom role
// simply resolves to that role's granular permissions (session.Authenticate).
// The invariants live at write time: never instance-scoped, never above the
// composer's own permissions, always closed under prerequisites
// (auth.ValidateCustomPermissions).

func customRoleToAPI(role store.CustomRole, memberCount *int) api.CustomRole {
	out := api.CustomRole{
		Uuid:        ptr(uuidString(role.Uuid)),
		Name:        role.Name,
		Description: role.Description,
		Permissions: role.Permissions,
		MemberCount: memberCount,
		CreatedAt:   timePtr(role.CreatedAt),
		UpdatedAt:   timePtr(role.UpdatedAt),
	}
	if out.Permissions == nil {
		out.Permissions = []string{}
	}
	return out
}

// scanUUID turns a path UUID into its binary form, answering 404 on a malformed
// value — a bad UUID cannot name anything, exactly like a missing one.
func (a *API) scanUUID(w http.ResponseWriter, r *http.Request, raw, kind string) (pgtype.UUID, bool) {
	var u pgtype.UUID
	if err := u.Scan(raw); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, kind+" not found")
		return u, false
	}
	return u, true
}

// ListPermissions implements GET /permissions: the static granular catalogue
// with each permission's socle and prerequisites, so the UI can build the
// custom-role composer (dependencies included).
func (a *API) ListPermissions(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.require(w, r, auth.PermRolesRead); !ok {
		return
	}
	names := make([]string, 0, len(auth.Catalog))
	for name := range auth.Catalog {
		names = append(names, name)
	}
	sort.Strings(names)

	data := make([]api.PermissionCatalogEntry, 0, len(names))
	for _, name := range names {
		socle := auth.Catalog[name]
		prereq := auth.Prerequisites(name)
		if prereq == nil {
			prereq = []string{}
		}
		data = append(data, api.PermissionCatalogEntry{
			Permission:     name,
			Socle:          api.PermissionCatalogEntrySocle(socle),
			InstanceScoped: ptr(socle == auth.PermRoot),
			Prerequisites:  prereq,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data []api.PermissionCatalogEntry `json:"data"`
	}{data})
}

// ListTeamRoles implements GET /teams/{team_uuid}/roles.
func (a *API) ListTeamRoles(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, params api.ListTeamRolesParams) {
	id, ok := a.require(w, r, auth.PermRolesRead)
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
	rows, err := a.Store.ListCustomRolesPage(r.Context(), store.ListCustomRolesPageParams{
		TeamID: team.ID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list roles", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(role store.CustomRole) int64 { return role.ID })
	data := make([]api.CustomRole, 0, len(rows))
	for _, role := range rows {
		data = append(data, customRoleToAPI(role, nil))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.CustomRole `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}{data, cursor})
}

// CreateTeamRole implements POST /teams/{team_uuid}/roles.
func (a *API) CreateTeamRole(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid) {
	id, ok := a.require(w, r, auth.PermRolesManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	var body api.CustomRoleCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	if !validRoleName(w, r, body.Name) {
		return
	}
	perms, ok := a.validatedPermissions(w, r, id, body.Permissions)
	if !ok {
		return
	}
	role, err := a.Store.CreateCustomRole(r.Context(), store.CreateCustomRoleParams{
		TeamID: team.ID, Name: body.Name, Description: body.Description, Permissions: perms,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a role with this name already exists in this team")
			return
		}
		a.internalError(w, r, "create role", err)
		return
	}
	a.recordAudit(r, id, "role.create", "role", role.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, customRoleToAPI(role, ptr(0)))
}

// GetTeamRole implements GET /teams/{team_uuid}/roles/{role_uuid}.
func (a *API) GetTeamRole(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, roleUuid api.RoleUuid) {
	id, ok := a.require(w, r, auth.PermRolesRead)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	u, ok := a.scanUUID(w, r, roleUuid, "role")
	if !ok {
		return
	}
	role, err := a.Store.GetCustomRoleByUUID(r.Context(), store.GetCustomRoleByUUIDParams{Uuid: u, TeamID: team.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "role not found")
			return
		}
		a.internalError(w, r, "get role", err)
		return
	}
	count, _ := a.Store.CountCustomRoleMembers(r.Context(), &role.ID)
	httpapi.WriteJSON(w, http.StatusOK, customRoleToAPI(role, ptr(int(count))))
}

// UpdateTeamRole implements PATCH /teams/{team_uuid}/roles/{role_uuid}.
func (a *API) UpdateTeamRole(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, roleUuid api.RoleUuid) {
	id, ok := a.require(w, r, auth.PermRolesManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	u, ok := a.scanUUID(w, r, roleUuid, "role")
	if !ok {
		return
	}
	var body api.CustomRoleUpdate
	patch, ok := decodePatch(w, r, &body)
	if !ok {
		return
	}
	params := store.UpdateCustomRoleParams{Uuid: u, TeamID: team.ID}
	if body.Name != nil {
		if !validRoleName(w, r, *body.Name) {
			return
		}
		params.Name = body.Name
	}
	if patch.Has("description") {
		params.SetDescription = true
		params.Description = body.Description
	}
	if body.Permissions != nil {
		perms, ok := a.validatedPermissions(w, r, id, *body.Permissions)
		if !ok {
			return
		}
		params.Permissions = perms
	}
	role, err := a.Store.UpdateCustomRole(r.Context(), params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "role not found")
			return
		}
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a role with this name already exists in this team")
			return
		}
		a.internalError(w, r, "update role", err)
		return
	}
	count, _ := a.Store.CountCustomRoleMembers(r.Context(), &role.ID)
	a.recordAudit(r, id, "role.update", "role", role.Uuid)
	httpapi.WriteJSON(w, http.StatusOK, customRoleToAPI(role, ptr(int(count))))
}

// DeleteTeamRole implements DELETE /teams/{team_uuid}/roles/{role_uuid}. Members
// carrying the role fall back to their system role (ON DELETE SET NULL).
func (a *API) DeleteTeamRole(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, roleUuid api.RoleUuid) {
	id, ok := a.require(w, r, auth.PermRolesManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	u, ok := a.scanUUID(w, r, roleUuid, "role")
	if !ok {
		return
	}
	n, err := a.Store.DeleteCustomRole(r.Context(), store.DeleteCustomRoleParams{Uuid: u, TeamID: team.ID})
	if err != nil {
		a.internalError(w, r, "delete role", err)
		return
	}
	if n == 0 {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "role not found")
		return
	}
	a.recordAudit(r, id, "role.delete", "role", u)
	w.WriteHeader(http.StatusNoContent)
}

// UpdateTeamMember implements PATCH /teams/{team_uuid}/members/{user_uuid}:
// assign a system role or a custom role to an existing member (ADR-038).
func (a *API) UpdateTeamMember(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, userUuid api.UserUuid) {
	id, ok := a.require(w, r, auth.PermMembersManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	member, ok := a.scanUUID(w, r, userUuid, "member")
	if !ok {
		return
	}
	var body api.MemberRoleUpdate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	if !body.Role.Valid() {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("role"), Code: ptr("invalid"), Message: "unknown role"}})
		return
	}

	// The member must exist in this team. Loading it first also gives us the
	// current role for the last-admin guard below.
	current, err := a.Store.GetTeamMemberByUUID(r.Context(), store.GetTeamMemberByUUIDParams{TeamID: team.ID, UserUuid: member})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "member not found")
			return
		}
		a.internalError(w, r, "get member", err)
		return
	}

	// Resolve the target: a custom role overrides the system role (kept as the
	// fallback); a system role clears any custom role.
	params := store.UpdateTeamMemberRoleParams{TeamID: team.ID, UserUuid: member, Role: store.TeamRoleMember}
	newIsAdmin := false
	if body.Role == api.MemberRoleUpdateRoleCustom {
		if body.CustomRoleUuid == nil || *body.CustomRoleUuid == "" {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("custom_role_uuid"), Code: ptr("required"), Message: "custom_role_uuid is required when role is custom"}})
			return
		}
		u, ok := a.scanUUID(w, r, *body.CustomRoleUuid, "role")
		if !ok {
			return
		}
		role, err := a.Store.GetCustomRoleByUUID(r.Context(), store.GetCustomRoleByUUIDParams{Uuid: u, TeamID: team.ID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "role not found")
				return
			}
			a.internalError(w, r, "get role", err)
			return
		}
		params.CustomRoleID = &role.ID
	} else {
		params.Role = store.TeamRole(body.Role)
		newIsAdmin = body.Role == api.MemberRoleUpdateRoleAdmin
	}

	// Anti-lockout (§10.1): a team must keep at least one effective admin. If this
	// change strips admin from the last one, refuse.
	currentIsAdmin := current.Role == store.TeamRoleAdmin && current.CustomRoleID == nil
	if currentIsAdmin && !newIsAdmin {
		admins, err := a.Store.CountTeamAdmins(r.Context(), team.ID)
		if err != nil {
			a.internalError(w, r, "count admins", err)
			return
		}
		if admins <= 1 {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "the last admin of a team cannot be demoted")
			return
		}
	}

	n, err := a.Store.UpdateTeamMemberRole(r.Context(), params)
	if err != nil {
		a.internalError(w, r, "update member role", err)
		return
	}
	if n == 0 {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "member not found")
		return
	}
	a.recordAudit(r, id, "member.role.update", "user", member)

	updated, err := a.Store.GetTeamMemberByUUID(r.Context(), store.GetTeamMemberByUUIDParams{TeamID: team.ID, UserUuid: member})
	if err != nil {
		a.internalError(w, r, "get member", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, teamMemberToAPI(updated))
}

// teamMemberToAPI renders a member row, exposing a custom role as role "custom".
func teamMemberToAPI(m store.GetTeamMemberByUUIDRow) api.TeamMember {
	role := api.TeamMemberRole(m.Role)
	out := api.TeamMember{
		UserUuid: uuidString(m.UserUuid),
		Email:    openapi_types.Email(m.Email),
		Name:     ptr(m.Name),
		JoinedAt: m.JoinedAt.Time.UTC(),
	}
	if m.CustomRoleID != nil {
		role = "custom"
		out.CustomRoleUuid = ptr(uuidString(m.CustomRoleUuid))
		out.CustomRoleName = m.CustomRoleName
	}
	out.Role = role
	return out
}

// validatedPermissions runs the custom-role rules (ADR-038): known, non-instance,
// within the composer's own permissions, closed under prerequisites. Writes 422
// and returns false on the first violation.
func (a *API) validatedPermissions(w http.ResponseWriter, r *http.Request, id *auth.Identity, perms []string) ([]string, bool) {
	closed, err := auth.ValidateCustomPermissions(perms, id.Permissions)
	if err != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("permissions"), Code: ptr("invalid"), Message: err.Error()}})
		return nil, false
	}
	return closed, true
}

// validRoleName checks a custom-role display name (kept human-readable, unlike a
// slug). Writes 422 and returns false when invalid.
func validRoleName(w http.ResponseWriter, r *http.Request, name string) bool {
	if len(name) == 0 || len(name) > 255 {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"}})
		return false
	}
	return true
}
