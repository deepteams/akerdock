package handlers

import (
	"encoding/json"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

func teamToAPI(t store.Team) api.Team {
	return api.Team{
		Uuid:        ptr(uuidString(t.Uuid)),
		Name:        t.Name,
		Description: t.Description,
		Personal:    ptr(t.Personal),
		CreatedAt:   timePtr(t.CreatedAt),
		UpdatedAt:   timePtr(t.UpdatedAt),
	}
}

// ListTeams implements GET /teams (permission: read). A token is
// team-scoped so the list usually has one entry; a root token lists every
// team of the instance.
func (a *API) ListTeams(w http.ResponseWriter, r *http.Request, params api.ListTeamsParams) {
	id, ok := a.require(w, r, auth.PermTeamRead)
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

	var (
		teams  []store.Team
		cursor *string
	)
	if id.IsRoot() {
		rows, err := a.Store.ListTeamsPage(r.Context(), store.ListTeamsPageParams{AfterID: after, PageLimit: limit + 1})
		if err != nil {
			a.internalError(w, r, "list teams", err)
			return
		}
		teams, cursor = nextCursor(rows, limit, func(t store.Team) int64 { return t.ID })
	} else if after == 0 {
		team, err := a.Store.GetTeamByID(r.Context(), id.TeamID)
		if err == nil {
			teams = []store.Team{team}
		}
	}

	data := make([]api.Team, 0, len(teams))
	for _, t := range teams {
		data = append(data, teamToAPI(t))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.Team `json:"data"`
		NextCursor *string    `json:"next_cursor"`
	}{data, cursor})
}

// CreateTeam implements POST /teams (permission: instance:manage). Teams are
// the isolation boundary of every resource (INV-002), so creating one is an
// instance-level act reserved to the root — and the caller joins it as
// `admin`, otherwise the instance would gain a team nobody can enter.
func (a *API) CreateTeam(w http.ResponseWriter, r *http.Request) {
	// A SESSION of the instance root, like every instance-scoped operation
	// (rbac-matrix §3.5): a team owner's team-scoped `root` permission is not
	// enough, and API tokens are team-bound so never qualify.
	id, ok := a.requireInstanceRoot(w, r)
	if !ok {
		return
	}
	var body api.TeamCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	name, ok := validateName(w, r, body.Name)
	if !ok {
		return
	}
	team, err := a.Store.CreateTeam(r.Context(), store.CreateTeamParams{
		Name: name, Description: body.Description,
	})
	if err != nil {
		a.internalError(w, r, "create team", err)
		return
	}
	// The creator joins as `admin`, the top team role (ADR-038) — otherwise
	// the instance gains a team nobody can enter. Only a SESSION carries a
	// user; an API token has none, and its team is then joined from the
	// members page like any other.
	if a.Sessions != nil {
		if sess, err := a.Sessions.SessionFromRequest(r.Context(), r); err == nil {
			if err := a.Store.AddTeamMember(r.Context(), store.AddTeamMemberParams{
				TeamID: team.ID, UserID: sess.UserID, Role: store.TeamRoleAdmin,
			}); err != nil {
				a.internalError(w, r, "create team", err)
				return
			}
		}
	}
	a.recordAudit(r, id, "team.create", "team", team.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, teamToAPI(team))
}

// GetTeam implements GET /teams/{team_uuid} (permission: read).
func (a *API) GetTeam(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid) {
	id, ok := a.require(w, r, auth.PermTeamRead)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, teamToAPI(team))
}

// UpdateTeam implements PATCH /teams/{team_uuid} (permission: write): partial
// update of the team's name and description.
func (a *API) UpdateTeam(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid) {
	id, ok := a.require(w, r, auth.PermTeamManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	var body api.TeamUpdate
	patch, ok := decodePatch(w, r, &body)
	if !ok {
		return
	}
	params := store.UpdateTeamParams{ID: team.ID}
	if body.Name != nil {
		if _, ok := validateName(w, r, *body.Name); !ok {
			return
		}
		params.Name = body.Name
	}
	if patch.Has("description") {
		params.SetDescription = true
		params.Description = body.Description
	}
	updated, err := a.Store.UpdateTeam(r.Context(), params)
	if err != nil {
		a.internalError(w, r, "update team", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, teamToAPI(updated))
}

// ListTeamMembers implements GET /teams/{team_uuid}/members (permission: read).
func (a *API) ListTeamMembers(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, params api.ListTeamMembersParams) {
	id, ok := a.require(w, r, auth.PermMembersRead)
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

	rows, err := a.Store.ListTeamMembersPage(r.Context(), store.ListTeamMembersPageParams{
		TeamID: team.ID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list team members", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(m store.ListTeamMembersPageRow) int64 { return m.MembershipID })

	data := make([]api.TeamMember, 0, len(rows))
	for _, m := range rows {
		joined := m.JoinedAt.Time.UTC()
		data = append(data, api.TeamMember{
			UserUuid: uuidString(m.UserUuid),
			Email:    openapi_types.Email(m.Email),
			Name:     ptr(m.Name),
			Role:     api.TeamMemberRole(m.Role),
			JoinedAt: joined,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.TeamMember `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}{data, cursor})
}

func (a *API) internalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	a.Logger.Error("handler error", "op", op, "error", err)
	httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "internal error")
}
