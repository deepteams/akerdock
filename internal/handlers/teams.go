package handlers

import (
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
	id, ok := a.require(w, r, auth.PermRead)
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

// GetTeam implements GET /teams/{team_uuid} (permission: read).
func (a *API) GetTeam(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid) {
	id, ok := a.require(w, r, auth.PermRead)
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
	id, ok := a.require(w, r, auth.PermWrite)
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
	id, ok := a.require(w, r, auth.PermRead)
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
