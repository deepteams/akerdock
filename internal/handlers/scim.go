package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/scim"
	"github.com/deepteams/akerdock/internal/store"
)

// SCIM 2.0 provisioning (ADR-038 bis, ISO A.5.16/A.5.18). Lives OUTSIDE /api/v1
// like /auth: it speaks the SCIM JSON dialect, not the v1 contract, and it
// authenticates with a per-team SCIM token (Bearer), never a session or an API
// token. Scope: a token acts only within its own team. This slice covers Users
// (provisioning + DEPROVISIONING — the access-lifecycle core); Groups→roles is
// the next slice.

// writeSCIM writes a SCIM JSON response with the SCIM media type.
func writeSCIM(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", scim.ContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func scimError(w http.ResponseWriter, status int, detail string) {
	writeSCIM(w, status, scim.NewError(status, detail))
}

// scimTeam authenticates the SCIM bearer token and returns the team it is scoped
// to. It writes the SCIM 401 itself on failure.
func (a *API) scimTeam(w http.ResponseWriter, r *http.Request) (store.GetScimTokenByHashRow, bool) {
	authz := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok || token == "" {
		scimError(w, http.StatusUnauthorized, "missing bearer token")
		return store.GetScimTokenByHashRow{}, false
	}
	row, err := a.Store.GetScimTokenByHash(r.Context(), auth.HashToken(token))
	if err != nil {
		scimError(w, http.StatusUnauthorized, "invalid SCIM token")
		return store.GetScimTokenByHashRow{}, false
	}
	go func(id int64) {
		_ = a.Store.TouchScimTokenUsed(context.Background(), id)
	}(row.ID)
	return row, true
}

// scimUser builds a SCIM User resource from a membership row.
func scimUser(userUUID pgtype.UUID, email, name, externalID string, active bool) scim.User {
	id := uuidString(userUUID)
	return scim.User{
		Schemas:     []string{scim.SchemaUser},
		ID:          id,
		ExternalID:  externalID,
		UserName:    email,
		DisplayName: name,
		Emails:      []scim.Email{{Value: email, Primary: true}},
		Active:      active,
		Meta:        &scim.Meta{ResourceType: "User", Location: "/scim/v2/Users/" + id},
	}
}

// ScimServiceProviderConfig implements GET /scim/v2/ServiceProviderConfig.
func (a *API) ScimServiceProviderConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.scimTeam(w, r); !ok {
		return
	}
	writeSCIM(w, http.StatusOK, scim.ServiceProviderConfig())
}

// ScimListUsers implements GET /scim/v2/Users. Supports the one filter IdPs rely
// on to test existence: `userName eq "x"`.
func (a *API) ScimListUsers(w http.ResponseWriter, r *http.Request) {
	team, ok := a.scimTeam(w, r)
	if !ok {
		return
	}
	rows, err := a.Store.ListTeamMembersForScim(r.Context(), team.TeamID)
	if err != nil {
		scimError(w, http.StatusInternalServerError, "internal error")
		return
	}
	wanted := scimFilterUserName(r.URL.Query().Get("filter"))
	resources := make([]any, 0, len(rows))
	for _, m := range rows {
		if wanted != "" && !strings.EqualFold(m.Email, wanted) {
			continue
		}
		resources = append(resources, scimUser(m.UserUuid, m.Email, m.Name, derefStr(m.ExternalID), true))
	}
	writeSCIM(w, http.StatusOK, scim.NewListResponse(resources, 1))
}

// scimFilterUserName extracts x from `userName eq "x"` (the only filter we honor).
func scimFilterUserName(filter string) string {
	filter = strings.TrimSpace(filter)
	lower := strings.ToLower(filter)
	if !strings.HasPrefix(lower, "username eq ") {
		return ""
	}
	rest := strings.TrimSpace(filter[len("userName eq "):])
	return strings.Trim(rest, `"`)
}

// ScimCreateUser implements POST /scim/v2/Users: provision a user into the team.
func (a *API) ScimCreateUser(w http.ResponseWriter, r *http.Request) {
	team, ok := a.scimTeam(w, r)
	if !ok {
		return
	}
	var body scim.User
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		scimError(w, http.StatusBadRequest, "invalid SCIM body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.PrimaryEmail()))
	if email == "" {
		scimError(w, http.StatusBadRequest, "userName (email) is required")
		return
	}

	user, err := a.Store.GetUserByEmail(r.Context(), email)
	if errors.Is(err, pgx.ErrNoRows) {
		user, err = a.Store.CreateUser(r.Context(), store.CreateUserParams{
			Email: email, Name: body.DisplayNameOr(email), PasswordHash: nil, IsRoot: false,
		})
	}
	if err != nil {
		scimError(w, http.StatusInternalServerError, "could not resolve the user")
		return
	}

	// Already a member? SCIM expects 409 so the IdP switches to update.
	if _, err := a.Store.GetScimMember(r.Context(), store.GetScimMemberParams{TeamID: team.TeamID, Uuid: user.Uuid}); err == nil {
		scimError(w, http.StatusConflict, "user already provisioned in this team")
		return
	}
	if err := a.Store.AddTeamMember(r.Context(), store.AddTeamMemberParams{
		TeamID: team.TeamID, UserID: user.ID, Role: store.TeamRoleMember,
	}); err != nil {
		scimError(w, http.StatusInternalServerError, "could not add the member")
		return
	}
	if body.ExternalID != "" {
		_ = a.Store.SetMembershipExternalID(r.Context(), store.SetMembershipExternalIDParams{
			ExternalID: &body.ExternalID, UserUuid: user.Uuid, TeamID: team.TeamID,
		})
	}
	a.Audit.System(r.Context(), &team.TeamID, "scim.user.provision", "user", user.Uuid, store.AuditResultSuccess)
	writeSCIM(w, http.StatusCreated, scimUser(user.Uuid, email, body.DisplayNameOr(email), body.ExternalID, true))
}

// ScimGetUser implements GET /scim/v2/Users/{id}.
func (a *API) ScimGetUser(w http.ResponseWriter, r *http.Request) {
	team, ok := a.scimTeam(w, r)
	if !ok {
		return
	}
	m, ok := a.scimMember(w, r, team.TeamID)
	if !ok {
		return
	}
	writeSCIM(w, http.StatusOK, scimUser(m.UserUuid, m.Email, m.Name, derefStr(m.ExternalID), true))
}

// ScimReplaceUser implements PUT /scim/v2/Users/{id}: active=false deprovisions.
func (a *API) ScimReplaceUser(w http.ResponseWriter, r *http.Request) {
	team, ok := a.scimTeam(w, r)
	if !ok {
		return
	}
	m, ok := a.scimMember(w, r, team.TeamID)
	if !ok {
		return
	}
	var body scim.User
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		scimError(w, http.StatusBadRequest, "invalid SCIM body")
		return
	}
	if !body.Active {
		a.scimDeprovision(r, team.TeamID, m)
		writeSCIM(w, http.StatusOK, scimUser(m.UserUuid, m.Email, m.Name, derefStr(m.ExternalID), false))
		return
	}
	writeSCIM(w, http.StatusOK, scimUser(m.UserUuid, m.Email, m.Name, derefStr(m.ExternalID), true))
}

// ScimPatchUser implements PATCH /scim/v2/Users/{id}: the IdP's deactivate path
// (Okta/Azure send replace active=false).
func (a *API) ScimPatchUser(w http.ResponseWriter, r *http.Request) {
	team, ok := a.scimTeam(w, r)
	if !ok {
		return
	}
	m, ok := a.scimMember(w, r, team.TeamID)
	if !ok {
		return
	}
	var patch scim.PatchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		scimError(w, http.StatusBadRequest, "invalid SCIM body")
		return
	}
	active := true
	found := false
	for _, op := range patch.Operations {
		if a, ok := patchActiveValue(op); ok {
			active, found = a, true
		}
	}
	if found && !active {
		a.scimDeprovision(r, team.TeamID, m)
	}
	writeSCIM(w, http.StatusOK, scimUser(m.UserUuid, m.Email, m.Name, derefStr(m.ExternalID), active))
}

// patchActiveValue reads the `active` boolean from a PATCH op, whether it comes
// as {path:"active", value:false} or {value:{"active":false}}.
func patchActiveValue(op scim.PatchOperation) (bool, bool) {
	if strings.EqualFold(op.Op, "remove") {
		return false, false
	}
	if strings.EqualFold(op.Path, "active") {
		var b bool
		if json.Unmarshal(op.Value, &b) == nil {
			return b, true
		}
	}
	var obj struct {
		Active *bool `json:"active"`
	}
	if json.Unmarshal(op.Value, &obj) == nil && obj.Active != nil {
		return *obj.Active, true
	}
	return false, false
}

// ScimDeleteUser implements DELETE /scim/v2/Users/{id}: full deprovision.
func (a *API) ScimDeleteUser(w http.ResponseWriter, r *http.Request) {
	team, ok := a.scimTeam(w, r)
	if !ok {
		return
	}
	m, ok := a.scimMember(w, r, team.TeamID)
	if !ok {
		return
	}
	a.scimDeprovision(r, team.TeamID, m)
	w.WriteHeader(http.StatusNoContent)
}

// scimMember resolves the {id} path (a user UUID) to a member of the team, or
// writes the SCIM 404.
func (a *API) scimMember(w http.ResponseWriter, r *http.Request, teamID int64) (store.GetScimMemberRow, bool) {
	var u pgtype.UUID
	if err := u.Scan(chi.URLParam(r, "id")); err != nil {
		scimError(w, http.StatusNotFound, "user not found")
		return store.GetScimMemberRow{}, false
	}
	m, err := a.Store.GetScimMember(r.Context(), store.GetScimMemberParams{TeamID: teamID, Uuid: u})
	if err != nil {
		scimError(w, http.StatusNotFound, "user not found")
		return store.GetScimMemberRow{}, false
	}
	return m, true
}

// scimDeprovision removes the member from the team AND revokes their live access:
// sessions and the team's API tokens they hold. The user ACCOUNT is left intact
// (they may belong to other teams, or be the instance root) — only their access
// to THIS team is cut. This is the offboarding control auditors ask for.
func (a *API) scimDeprovision(r *http.Request, teamID int64, m store.GetScimMemberRow) {
	if _, err := a.Store.RemoveTeamMemberByUUID(r.Context(), store.RemoveTeamMemberByUUIDParams{Uuid: m.UserUuid, TeamID: teamID}); err != nil {
		a.Logger.Error("scim deprovision: remove member", "error", err)
	}
	if _, err := a.Store.RevokeAllSessionsOfUser(r.Context(), m.UserID); err != nil {
		a.Logger.Error("scim deprovision: revoke sessions", "error", err)
	}
	if _, err := a.Store.RevokeApiTokensForUserInTeam(r.Context(), store.RevokeApiTokensForUserInTeamParams{TeamID: teamID, CreatedBy: &m.UserID}); err != nil {
		a.Logger.Error("scim deprovision: revoke tokens", "error", err)
	}
	a.Audit.System(r.Context(), &teamID, "scim.user.deprovision", "user", m.UserUuid, store.AuditResultSuccess)
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- SCIM Groups (roles as virtual groups) ------------------------------------

// systemRoleGroups are the built-in roles exposed as SCIM groups.
var systemRoleGroups = []store.TeamRole{store.TeamRoleAdmin, store.TeamRoleMember, store.TeamRoleReviewer}

// scimGroups assembles the team's roles as SCIM groups, each populated with the
// members currently holding that role (custom-role members group under the
// custom role, everyone else under their system role).
func (a *API) scimGroups(ctx context.Context, teamID int64) ([]scim.Group, error) {
	members, err := a.Store.ListTeamMembersForScim(ctx, teamID)
	if err != nil {
		return nil, err
	}
	customRoles, err := a.Store.ListCustomRolesPage(ctx, store.ListCustomRolesPageParams{TeamID: teamID, AfterID: 0, PageLimit: 200})
	if err != nil {
		return nil, err
	}

	groups := make([]scim.Group, 0, len(systemRoleGroups)+len(customRoles))
	index := map[string]int{}
	add := func(id, name string) {
		index[id] = len(groups)
		groups = append(groups, scim.Group{
			Schemas: []string{scim.SchemaGroup}, ID: id, DisplayName: name,
			Members: []scim.GroupMember{},
			Meta:    &scim.Meta{ResourceType: "Group", Location: "/scim/v2/Groups/" + id},
		})
	}
	for _, role := range systemRoleGroups {
		add(scim.SystemRoleGroupID(string(role)), string(role))
	}
	for _, cr := range customRoles {
		add(uuidString(cr.Uuid), cr.Name)
	}

	for _, m := range members {
		id := scim.SystemRoleGroupID(string(m.Role))
		if m.CustomRoleUuid.Valid {
			id = uuidString(m.CustomRoleUuid)
		}
		if i, ok := index[id]; ok {
			groups[i].Members = append(groups[i].Members, scim.GroupMember{Value: uuidString(m.UserUuid), Display: m.Email})
		}
	}
	return groups, nil
}

// ScimListGroups implements GET /scim/v2/Groups.
func (a *API) ScimListGroups(w http.ResponseWriter, r *http.Request) {
	team, ok := a.scimTeam(w, r)
	if !ok {
		return
	}
	groups, err := a.scimGroups(r.Context(), team.TeamID)
	if err != nil {
		scimError(w, http.StatusInternalServerError, "internal error")
		return
	}
	wanted := scimFilterDisplayName(r.URL.Query().Get("filter"))
	resources := make([]any, 0, len(groups))
	for _, g := range groups {
		if wanted != "" && !strings.EqualFold(g.DisplayName, wanted) {
			continue
		}
		resources = append(resources, g)
	}
	writeSCIM(w, http.StatusOK, scim.NewListResponse(resources, 1))
}

// ScimGetGroup implements GET /scim/v2/Groups/{id}.
func (a *API) ScimGetGroup(w http.ResponseWriter, r *http.Request) {
	team, ok := a.scimTeam(w, r)
	if !ok {
		return
	}
	groups, err := a.scimGroups(r.Context(), team.TeamID)
	if err != nil {
		scimError(w, http.StatusInternalServerError, "internal error")
		return
	}
	id := chi.URLParam(r, "id")
	for _, g := range groups {
		if g.ID == id {
			writeSCIM(w, http.StatusOK, g)
			return
		}
	}
	scimError(w, http.StatusNotFound, "group not found")
}

// ScimCreateGroup implements POST /scim/v2/Groups: roles are managed in
// AkerDock, not created via SCIM — so this only ACKNOWLEDGES a group whose
// displayName matches an existing role (idempotent), and refuses the rest.
func (a *API) ScimCreateGroup(w http.ResponseWriter, r *http.Request) {
	team, ok := a.scimTeam(w, r)
	if !ok {
		return
	}
	var body scim.Group
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		scimError(w, http.StatusBadRequest, "invalid SCIM body")
		return
	}
	groups, err := a.scimGroups(r.Context(), team.TeamID)
	if err != nil {
		scimError(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, g := range groups {
		if strings.EqualFold(g.DisplayName, body.DisplayName) {
			writeSCIM(w, http.StatusOK, g)
			return
		}
	}
	scimError(w, http.StatusBadRequest, "roles are managed in AkerDock; no role matches this group name")
}

// ScimPatchGroup implements PATCH /scim/v2/Groups/{id}: the IdP's assign/unassign
// path. add/replace member → set the member's role to this group; remove → reset
// to the default member role.
func (a *API) ScimPatchGroup(w http.ResponseWriter, r *http.Request) {
	team, ok := a.scimTeam(w, r)
	if !ok {
		return
	}
	groupID := chi.URLParam(r, "id")
	if !a.scimGroupExists(r.Context(), team.TeamID, groupID) {
		scimError(w, http.StatusNotFound, "group not found")
		return
	}
	var patch scim.PatchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		scimError(w, http.StatusBadRequest, "invalid SCIM body")
		return
	}
	for _, op := range patch.Operations {
		if op.Path != "members" && !strings.HasPrefix(op.Path, "members[") {
			continue
		}
		grant := !strings.EqualFold(op.Op, "remove")
		for _, member := range scim.MemberValuesFromOp(op) {
			a.scimSetMemberRole(r.Context(), team.TeamID, member, groupID, grant)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) scimGroupExists(ctx context.Context, teamID int64, id string) bool {
	groups, err := a.scimGroups(ctx, teamID)
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g.ID == id {
			return true
		}
	}
	return false
}

// scimSetMemberRole applies a group assignment: grant sets the member's role to
// the group's role; !grant resets to the default member role. Guards against
// demoting the last admin (anti-lockout). Best-effort; logs on error.
func (a *API) scimSetMemberRole(ctx context.Context, teamID int64, userUUID, groupID string, grant bool) {
	var uid pgtype.UUID
	if uid.Scan(userUUID) != nil {
		return
	}
	params := store.UpdateTeamMemberRoleParams{TeamID: teamID, UserUuid: uid, Role: store.TeamRoleMember}
	if grant {
		sys, custom := scim.ParseGroupID(groupID)
		switch {
		case custom != "":
			var cu pgtype.UUID
			if cu.Scan(custom) != nil {
				return
			}
			role, err := a.Store.GetCustomRoleByUUID(ctx, store.GetCustomRoleByUUIDParams{Uuid: cu, TeamID: teamID})
			if err != nil {
				return
			}
			params.CustomRoleID = &role.ID
		case sys == string(store.TeamRoleAdmin) || sys == string(store.TeamRoleMember) || sys == string(store.TeamRoleReviewer):
			params.Role = store.TeamRole(sys)
		default:
			return // unknown group
		}
	} else if a.scimWouldOrphanLastAdmin(ctx, teamID, uid) {
		a.Logger.Warn("scim: refused to demote the last admin", "team_id", teamID)
		return
	}
	if _, err := a.Store.UpdateTeamMemberRole(ctx, params); err != nil {
		a.Logger.Error("scim: set member role", "error", err)
	}
}

// scimWouldOrphanLastAdmin reports whether resetting this member would remove the
// team's last admin.
func (a *API) scimWouldOrphanLastAdmin(ctx context.Context, teamID int64, userUUID pgtype.UUID) bool {
	m, err := a.Store.GetScimMember(ctx, store.GetScimMemberParams{TeamID: teamID, Uuid: userUUID})
	if err != nil || m.Role != store.TeamRoleAdmin {
		return false
	}
	admins, err := a.Store.CountTeamAdmins(ctx, teamID)
	return err == nil && admins <= 1
}

// scimFilterDisplayName extracts x from `displayName eq "x"`.
func scimFilterDisplayName(filter string) string {
	filter = strings.TrimSpace(filter)
	if !strings.HasPrefix(strings.ToLower(filter), "displayname eq ") {
		return ""
	}
	return strings.Trim(strings.TrimSpace(filter[len("displayName eq "):]), `"`)
}

// --- SCIM token management (in /api/v1, session/token authenticated) ----------

func scimTokenToAPI(t store.ScimToken) api.ScimToken {
	return api.ScimToken{
		Uuid:       uuidString(t.Uuid),
		Name:       t.Name,
		CreatedAt:  t.CreatedAt.Time.UTC(),
		LastUsedAt: timePtr(t.LastUsedAt),
	}
}

// ListScimTokens implements GET /teams/{team_uuid}/scim-tokens.
func (a *API) ListScimTokens(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid) {
	id, ok := a.require(w, r, auth.PermMembersManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	rows, err := a.Store.ListScimTokensPage(r.Context(), team.ID)
	if err != nil {
		a.internalError(w, r, "list scim tokens", err)
		return
	}
	data := make([]api.ScimToken, 0, len(rows))
	for _, t := range rows {
		data = append(data, scimTokenToAPI(t))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data []api.ScimToken `json:"data"`
	}{data})
}

// CreateScimToken implements POST /teams/{team_uuid}/scim-tokens. The clear
// value is returned exactly once; only its SHA-256 is stored.
func (a *API) CreateScimToken(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid) {
	id, ok := a.require(w, r, auth.PermMembersManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	var body api.ScimTokenCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("name"), Code: ptr("required"), Message: "name is required"}})
		return
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		a.internalError(w, r, "create scim token", err)
		return
	}
	token := "akscim_" + hex.EncodeToString(raw)
	created, err := a.Store.CreateScimToken(r.Context(), store.CreateScimTokenParams{
		TeamID: team.ID, Name: strings.TrimSpace(body.Name), TokenHash: auth.HashToken(token),
	})
	if err != nil {
		a.internalError(w, r, "create scim token", err)
		return
	}
	base := "/scim/v2"
	if settings, err := a.Settings.Get(r.Context()); err == nil && settings.Fqdn != nil && *settings.Fqdn != "" {
		base = "https://" + *settings.Fqdn + "/scim/v2"
	}
	a.recordAudit(r, id, "scim_token.create", "scim_token", created.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, api.ScimTokenCreated{
		Uuid: uuidString(created.Uuid), Name: created.Name, Token: token,
		ScimBaseUrl: base, CreatedAt: created.CreatedAt.Time.UTC(),
	})
}

// RevokeScimToken implements DELETE /teams/{team_uuid}/scim-tokens/{scim_token_uuid}.
func (a *API) RevokeScimToken(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, scimTokenUuid string) {
	id, ok := a.require(w, r, auth.PermMembersManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	u, ok := a.scanUUID(w, r, scimTokenUuid, "scim token")
	if !ok {
		return
	}
	n, err := a.Store.RevokeScimToken(r.Context(), store.RevokeScimTokenParams{Uuid: u, TeamID: team.ID})
	if err != nil {
		a.internalError(w, r, "revoke scim token", err)
		return
	}
	if n == 0 {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "scim token not found")
		return
	}
	a.recordAudit(r, id, "scim_token.revoke", "scim_token", u)
	w.WriteHeader(http.StatusNoContent)
}
