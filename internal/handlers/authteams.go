package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// ListMyTeams implements GET /auth/teams — a session endpoint outside the v1
// contract, like the rest of /auth: the teams the signed-in user may act in,
// and which one the session is currently in.
//
// It is deliberately NOT GET /teams. That operation lists the teams of the
// INSTANCE for the instance root (rbac-matrix §3.5), which is the right answer
// for an administration screen and the wrong one for a switcher: offering a
// team nobody added you to would turn switching into a way in.
func (a *API) ListMyTeams(w http.ResponseWriter, r *http.Request) {
	if a.Sessions == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, err := a.Sessions.SessionFromRequest(r.Context(), r)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "no active session")
		return
	}
	teams, err := a.Sessions.Teams(r.Context(), sess.UserID)
	if err != nil {
		a.internalError(w, r, "list teams of user", err)
		return
	}

	// Which team the session ACTS in — resolved the same way every request
	// resolves it, so the switcher's checkmark can never disagree with the data
	// the pages behind it show.
	current := ""
	if identity := a.Sessions.Authenticate(r.Context(), r); identity != nil {
		current = identity.TeamUUID
	}

	data := make([]map[string]any, 0, len(teams))
	for _, t := range teams {
		data = append(data, map[string]any{
			"uuid": t.UUID, "name": t.Name, "role": string(t.Role),
			"personal": t.Personal, "current": t.UUID == current,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": data, "current_team_uuid": current})
}

// SwitchTeam implements POST /auth/session/team: moves the session into
// another of the user's teams.
//
// Session + CSRF like every mutating /auth endpoint. Crossing a team boundary
// is a security-relevant act (§23.1) — it changes which data the very next
// request reaches — so it is explicit, audited, and refused for any team the
// user does not belong to, the instance root included: seeing every team
// (GET /teams) is not being a member of every team.
func (a *API) SwitchTeam(w http.ResponseWriter, r *http.Request) {
	if a.Sessions == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, ok := a.sessionUser(w, r)
	if !ok {
		return
	}
	var body struct {
		TeamUUID string `json:"team_uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.TeamUUID) == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "a team_uuid is required")
		return
	}

	team, err := a.Sessions.SwitchTeam(r.Context(), sess.UserID, sess.ID, strings.TrimSpace(body.TeamUUID))
	if err != nil {
		if errors.Is(err, session.ErrNotAMember) {
			// Same 404 for "no such team" and "not yours" (INV-002).
			a.auditTeamSwitch(r, sess, store.AuditResultDenied)
			httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "team not found")
			return
		}
		a.internalError(w, r, "switch team", err)
		return
	}

	a.auditTeamSwitch(r, sess, store.AuditResultSuccess)
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"team_uuid": team.UUID, "name": team.Name, "role": string(team.Role),
	})
}

// auditTeamSwitch records the boundary crossing (§23.4). The event is recorded
// against the team the session is LEAVING: that is the team whose administrator
// is entitled to see who stepped out of it, and the destination is not theirs
// to read.
func (a *API) auditTeamSwitch(r *http.Request, sess *store.GetSessionByTokenHashRow, result store.AuditResult) {
	a.auditAuth(r, "auth.team.switch", result, sess.UserID, sess.Email, sess.CurrentTeamID)
}
