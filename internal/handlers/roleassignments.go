// Scoped role assignments (ADR-046 §1): the exceptions to a member's team-wide
// base role. Assigning is an administrative act — it draws the boundary the
// enforcement then applies — so it carries `members:manage` and the same
// anti-elevation rule as composing a custom role.
package handlers

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// scopeString renders the canonical notation the API and the audit trail speak
// (ADR-046 §10): one string, sortable by specificity, readable in an audit row
// without a join.
func scopeString(projectUUID, environmentUUID pgtype.UUID) string {
	switch {
	case environmentUUID.Valid && projectUUID.Valid:
		return "project:" + uuidString(projectUUID) + "/environment:" + uuidString(environmentUUID)
	case environmentUUID.Valid:
		return "environment:" + uuidString(environmentUUID)
	case projectUUID.Valid:
		return "project:" + uuidString(projectUUID)
	default:
		return scopeTeam
	}
}

// notConferred names the permissions of a role that a scoped assignment cannot
// grant. Reported rather than refused: assigning `admin` on a project is a
// reasonable thing to do — it means "admin of this project" — but an admin who
// is not told what was dropped believes they delegated more than they did.
func notConferred(perms []string) []string {
	var out []string
	for _, p := range perms {
		if auth.ClassOf(p) == auth.ClassTeamOnly {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// ListRoleAssignments implements GET /teams/{uuid}/role-assignments.
func (a *API) ListRoleAssignments(w http.ResponseWriter, r *http.Request, teamUUID api.TeamUuid) {
	id, ok := a.require(w, r, auth.PermMembersRead)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUUID)
	if !ok {
		return
	}
	rows, err := a.Store.ListRoleAssignmentsForTeam(r.Context(), team.ID)
	if err != nil {
		a.internalError(w, r, "list role assignments", err)
		return
	}
	data := make([]api.RoleAssignment, 0, len(rows))
	for _, row := range rows {
		data = append(data, roleAssignmentToAPI(row))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data []api.RoleAssignment `json:"data"`
	}{data})
}

func roleAssignmentToAPI(row store.ListRoleAssignmentsForTeamRow) api.RoleAssignment {
	perms := row.CustomPermissions
	name := "custom"
	if row.CustomRoleName != nil {
		name = *row.CustomRoleName
	}
	if row.Role != nil {
		perms = session.PermissionsForRole(*row.Role)
		name = string(*row.Role)
	}
	out := api.RoleAssignment{
		Uuid:         uuidString(row.Uuid),
		UserUuid:     uuidString(row.UserUuid),
		UserEmail:    row.Email,
		Role:         name,
		Scope:        scopeString(row.ProjectUuid, row.EnvironmentUuid),
		NotConferred: ptr(notConferred(auth.ExpandGranular(perms))),
		CreatedAt:    ptr(row.CreatedAt.Time),
	}
	if row.CustomRoleUuid.Valid {
		out.CustomRoleUuid = ptr(uuidString(row.CustomRoleUuid))
	}
	switch {
	case row.EnvironmentName != nil:
		out.ScopeLabel = row.EnvironmentName
	case row.ProjectName != nil:
		out.ScopeLabel = row.ProjectName
	}
	return out
}

// CreateRoleAssignment implements POST /teams/{uuid}/role-assignments.
func (a *API) CreateRoleAssignment(w http.ResponseWriter, r *http.Request, teamUUID api.TeamUuid) {
	id, ok := a.require(w, r, auth.PermMembersManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUUID)
	if !ok {
		return
	}
	var body api.RoleAssignmentCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	member, ok := a.resolveMemberForAssignment(w, r, team.ID, body.UserUuid)
	if !ok {
		return
	}

	// Exactly one scope. The team scope is the member's base role, changed
	// through the member endpoint — accepting it here would give two ways to
	// set the same thing, and they would disagree eventually.
	projectID, environmentID, ok := a.assignmentScope(w, r, id, body)
	if !ok {
		return
	}

	role, customRoleID, perms, ok := a.assignmentRole(w, r, id, team.ID, body)
	if !ok {
		return
	}

	// Anti-elevation (ADR-046 §8): you cannot grant what you do not hold at the
	// target scope. The check is on the CLOSED set, so a prerequisite the
	// assigner lacks is caught too.
	scope := auth.Scope{ProjectID: projectID, EnvironmentID: environmentID}
	for _, p := range perms {
		if auth.ClassOf(p) == auth.ClassTeamOnly {
			continue // not conferred anyway; refusing on it would be noise
		}
		if !id.CanOnScope(scope, auth.Permission(p)) {
			httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden,
				"you cannot grant "+p+" here, a permission you do not hold at this scope")
			return
		}
	}

	row, err := a.Store.CreateRoleAssignment(r.Context(), store.CreateRoleAssignmentParams{
		TeamID:        team.ID,
		UserID:        member,
		Role:          role,
		CustomRoleID:  customRoleID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		CreatedBy:     a.sessionUserID(r, id),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
				"this member already holds this role at this scope")
			return
		}
		a.internalError(w, r, "create role assignment", err)
		return
	}
	// "Who could reach production last March" is an audit question, and it is
	// answerable only if the assignment history is in the trail (§23.4).
	a.recordAudit(r, id, "role-assignment.create", "role_assignment", row.Uuid)

	httpapi.WriteJSON(w, http.StatusCreated, api.RoleAssignment{
		Uuid:         uuidString(row.Uuid),
		UserUuid:     body.UserUuid,
		UserEmail:    "",
		Role:         assignmentRoleName(role, body),
		Scope:        scopeStringFromBody(body),
		NotConferred: ptr(notConferred(perms)),
		CreatedAt:    ptr(row.CreatedAt.Time),
	})
}

func assignmentRoleName(role *store.TeamRole, body api.RoleAssignmentCreate) string {
	if role != nil {
		return string(*role)
	}
	if body.CustomRoleUuid != nil {
		return "custom"
	}
	return ""
}

func scopeStringFromBody(body api.RoleAssignmentCreate) string {
	switch {
	case body.EnvironmentUuid != nil && body.ProjectUuid != nil:
		return "project:" + *body.ProjectUuid + "/environment:" + *body.EnvironmentUuid
	case body.EnvironmentUuid != nil:
		return "environment:" + *body.EnvironmentUuid
	case body.ProjectUuid != nil:
		return "project:" + *body.ProjectUuid
	}
	return scopeTeam
}

// resolveMemberForAssignment maps a user uuid onto a member of this team. A
// user who is not a member gets "not found": an assignment without a membership
// is a grant to somebody who is not in the room.
func (a *API) resolveMemberForAssignment(w http.ResponseWriter, r *http.Request, teamID int64, userUUID string) (int64, bool) {
	var u pgtype.UUID
	if err := u.Scan(userUUID); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "member not found")
		return 0, false
	}
	member, err := a.Store.GetTeamMemberIDByUserUUID(r.Context(), store.GetTeamMemberIDByUserUUIDParams{
		TeamID: teamID, Uuid: u,
	})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "member not found")
		return 0, false
	}
	return member, true
}

// assignmentScope resolves the target scope, refusing anything but exactly one.
func (a *API) assignmentScope(w http.ResponseWriter, r *http.Request, id *auth.Identity, body api.RoleAssignmentCreate) (projectID, environmentID *int64, ok bool) {
	hasProject := body.ProjectUuid != nil && *body.ProjectUuid != ""
	hasEnv := body.EnvironmentUuid != nil && *body.EnvironmentUuid != ""
	if !hasProject && !hasEnv {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"a scope is required: project_uuid or environment_uuid — the team scope is the member's base role")
		return nil, nil, false
	}

	project, found := a.resolveProject(w, r, id, *body.ProjectUuid)
	if !found {
		return nil, nil, false
	}
	if !hasEnv {
		return &project.ID, nil, true
	}
	env, found := a.resolveEnvironment(w, r, project, *body.EnvironmentUuid)
	if !found {
		return nil, nil, false
	}
	return nil, &env.ID, true
}

// assignmentRole resolves the role source and returns its granular permissions.
func (a *API) assignmentRole(w http.ResponseWriter, r *http.Request, id *auth.Identity, teamID int64, body api.RoleAssignmentCreate) (*store.TeamRole, *int64, []string, bool) {
	if body.CustomRoleUuid != nil && *body.CustomRoleUuid != "" {
		var u pgtype.UUID
		if err := u.Scan(*body.CustomRoleUuid); err != nil {
			httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "custom role not found")
			return nil, nil, nil, false
		}
		role, err := a.Store.GetCustomRoleByUUID(r.Context(), store.GetCustomRoleByUUIDParams{Uuid: u, TeamID: teamID})
		if err != nil {
			httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "custom role not found")
			return nil, nil, nil, false
		}
		return nil, &role.ID, auth.ExpandGranular(role.Permissions), true
	}
	if body.Role == nil || *body.Role == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"a role is required: role or custom_role_uuid")
		return nil, nil, nil, false
	}
	role := store.TeamRole(*body.Role)
	switch role {
	case store.TeamRoleAdmin, store.TeamRoleMember, store.TeamRoleReviewer, store.TeamRoleNone:
	default:
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"unknown role — expected admin, member, reviewer or none")
		return nil, nil, nil, false
	}
	return &role, nil, auth.ExpandGranular(session.PermissionsForRole(role)), true
}

// DeleteRoleAssignment implements
// DELETE /teams/{uuid}/role-assignments/{assignment_uuid}.
func (a *API) DeleteRoleAssignment(w http.ResponseWriter, r *http.Request, teamUUID api.TeamUuid, assignmentUUID string) {
	id, ok := a.require(w, r, auth.PermMembersManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUUID)
	if !ok {
		return
	}
	u, ok := a.scanUUID(w, r, assignmentUUID, "assignment")
	if !ok {
		return
	}
	n, err := a.Store.DeleteRoleAssignment(r.Context(), store.DeleteRoleAssignmentParams{Uuid: u, TeamID: team.ID})
	if err != nil {
		a.internalError(w, r, "delete role assignment", err)
		return
	}
	if n == 0 {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "assignment not found")
		return
	}
	a.recordAudit(r, id, "role-assignment.delete", "role_assignment", u)
	w.WriteHeader(http.StatusNoContent)
}
