package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

const otherTeamUUID = "22222222-2222-4222-8222-222222222222"

// switcherAPI wires the browser-session doubles onto the flow API, with a user
// who belongs to two teams.
func switcherAPI(t *testing.T) (*API, *browserSessionStore) {
	t.Helper()
	a, _ := flowAPI(t)
	st := newBrowserSessionStore(t)
	st.memberships = []store.ListTeamMembershipsForUserRow{
		{TeamID: 1, Role: store.TeamRoleOwner, TeamUuid: st.sessionRow.Uuid, TeamName: "Acme"},
		{TeamID: 2, Role: store.TeamRoleReviewer, TeamUuid: pguuid.MustParse(otherTeamUUID), TeamName: "Beta"},
	}
	a.Sessions = &session.Manager{Store: st}
	return a, st
}

func TestListMyTeamsOffersMembershipsAndMarksTheCurrentOne(t *testing.T) {
	a, _ := switcherAPI(t)

	rec := httptest.NewRecorder()
	a.ListMyTeams(rec, authenticatedBrowserRequest(t, http.MethodGet, "/auth/teams", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("ListMyTeams = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			UUID    string `json:"uuid"`
			Name    string `json:"name"`
			Role    string `json:"role"`
			Current bool   `json:"current"`
		} `json:"data"`
		CurrentTeamUUID string `json:"current_team_uuid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("teams = %#v", body.Data)
	}
	// Exactly one entry is marked current, and it is the team the session
	// really acts in — a switcher whose checkmark is guessed from position is
	// the bug this replaces.
	current := 0
	for _, team := range body.Data {
		if team.Current {
			current++
			if team.UUID != body.CurrentTeamUUID {
				t.Fatalf("marked %q current while the session is in %q", team.UUID, body.CurrentTeamUUID)
			}
		}
	}
	if current != 1 {
		t.Fatalf("%d teams marked current", current)
	}
}

func TestSwitchTeamMovesTheSession(t *testing.T) {
	a, st := switcherAPI(t)

	rec := httptest.NewRecorder()
	a.SwitchTeam(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/session/team",
		`{"team_uuid":"`+otherTeamUUID+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("SwitchTeam = %d, want 200: %s", rec.Code, rec.Body)
	}
	if len(st.switched) != 1 || *st.switched[0].CurrentTeamID != 2 {
		t.Fatalf("session was not moved: %#v", st.switched)
	}
}

// A team the user is not a member of answers 404 — the same answer as a team
// that does not exist, so the endpoint tells nothing about the instance's other
// teams (INV-002). The instance root is no exception: seeing every team through
// GET /teams is not being a member of every team.
func TestSwitchTeamRefusesForeignTeams(t *testing.T) {
	a, st := switcherAPI(t)

	for _, body := range []string{
		`{"team_uuid":"99999999-9999-4999-8999-999999999999"}`,
		`{"team_uuid":""}`,
		`not json`,
	} {
		rec := httptest.NewRecorder()
		a.SwitchTeam(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/session/team", body))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
			t.Fatalf("SwitchTeam(%s) = %d, want 404 or 400", body, rec.Code)
		}
	}
	if len(st.switched) != 0 {
		t.Fatalf("a refused switch still moved the session: %#v", st.switched)
	}
}

// Both endpoints are session endpoints: no cookie, no answer. And the mutating
// one needs the CSRF echo, like every other /auth mutation.
func TestTeamSwitcherRequiresASessionAndCSRF(t *testing.T) {
	a, _ := switcherAPI(t)

	rec := httptest.NewRecorder()
	a.ListMyTeams(rec, httptest.NewRequest(http.MethodGet, "/auth/teams", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous ListMyTeams = %d, want 401", rec.Code)
	}

	noCSRF := httptest.NewRequest(http.MethodPost, "/auth/session/team", nil)
	noCSRF.AddCookie(&http.Cookie{Name: session.CookieName, Value: "session-token"})
	rec = httptest.NewRecorder()
	a.SwitchTeam(rec, noCSRF)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("SwitchTeam without CSRF = %d, want 403", rec.Code)
	}
}
