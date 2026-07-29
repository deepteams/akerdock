package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// twoTeams is a user who belongs to two teams with DIFFERENT roles — the case
// the single-team code could not express: admin of the older team, reviewer of
// the newer one.
func twoTeams() []store.ListTeamMembershipsForUserRow {
	return []store.ListTeamMembershipsForUserRow{
		{
			TeamID: 1, Role: store.TeamRoleAdmin, TeamName: "Acme",
			TeamUuid: pguuid.MustParse("11111111-1111-4111-8111-111111111111"),
		},
		{
			TeamID: 2, Role: store.TeamRoleReviewer, TeamName: "Beta",
			TeamUuid: pguuid.MustParse("22222222-2222-4222-8222-222222222222"),
		},
	}
}

func sessionRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "clear"})
	return r
}

// The switch is what the whole feature rests on: the identity of the very next
// request must describe the NEW team — its id, its uuid AND the permissions of
// the role held THERE. Carrying the old team's role into the new team would be
// a privilege escalation dressed up as a UI feature.
func TestSwitchTeamMovesTheSessionAndItsPermissions(t *testing.T) {
	teams := twoTeams()
	database := &fakeSessionStore{
		memberships: teams,
		sessionRow: store.GetSessionByTokenHashRow{
			ID: 3, Uuid: pguuid.MustParse("33333333-3333-4333-8333-333333333333"),
			UserID: 4, CurrentTeamID: &teams[0].TeamID,
		},
	}
	manager := &Manager{Store: database}
	ctx := context.Background()

	// Before: the session acts in team 1, where the user is admin.
	identity := manager.Authenticate(ctx, sessionRequest())
	if identity == nil || identity.TeamID != 1 || !auth.Has(identity.Permissions, "applications:create") {
		t.Fatalf("identity before the switch = %#v", identity)
	}

	target := uuidString(teams[1].TeamUuid)
	moved, err := manager.SwitchTeam(ctx, 4, 3, target)
	if err != nil || moved.TeamID != 2 {
		t.Fatalf("SwitchTeam = %#v, %v", moved, err)
	}
	if len(database.sessionTeamSets) != 1 || *database.sessionTeamSets[0].CurrentTeamID != 2 ||
		database.sessionTeamSets[0].ID != 3 {
		t.Fatalf("session was not moved: %#v", database.sessionTeamSets)
	}
	// The choice survives the session: the next login opens on team 2.
	if len(database.lastTeamSets) != 1 || *database.lastTeamSets[0].LastTeamID != 2 ||
		database.lastTeamSets[0].ID != 4 {
		t.Fatalf("the team was not remembered: %#v", database.lastTeamSets)
	}

	// After: same cookie, new team — and the reviewer role that comes with it.
	database.sessionRow.CurrentTeamID = &teams[1].TeamID
	identity = manager.Authenticate(ctx, sessionRequest())
	if identity == nil || identity.TeamID != 2 || identity.TeamUUID != target {
		t.Fatalf("identity after the switch = %#v", identity)
	}
	if auth.Has(identity.Permissions, "applications:create") || !auth.Has(identity.Permissions, "previews:read") {
		t.Fatal("the admin permissions of the previous team crossed the boundary")
	}
}

// Switching is the one call that widens what a live session reaches, so a team
// the user does not belong to must be refused — and refused the same way a
// team that does not exist is, or the endpoint becomes a directory of the
// instance's team UUIDs (INV-002).
func TestSwitchTeamRefusesTeamsTheUserIsNotIn(t *testing.T) {
	database := &fakeSessionStore{memberships: twoTeams()}
	manager := &Manager{Store: database}

	for _, target := range []string{
		"99999999-9999-4999-8999-999999999999", // a real team, somebody else's
		"not-a-uuid",
		"",
	} {
		if _, err := manager.SwitchTeam(context.Background(), 4, 3, target); !errors.Is(err, ErrNotAMember) {
			t.Fatalf("SwitchTeam(%q) = %v, want ErrNotAMember", target, err)
		}
	}
	if len(database.sessionTeamSets) != 0 {
		t.Fatalf("a refused switch still moved the session: %#v", database.sessionTeamSets)
	}
}

// A session pinned to a team the user has since been removed from must LOSE
// that team, not keep acting in it: the membership is the authority, the
// session row is only a preference.
func TestAuthenticateFallsBackWhenTheMembershipIsGone(t *testing.T) {
	teams := twoTeams()[:1] // the user is only in team 1 any more
	stale := int64(2)
	database := &fakeSessionStore{
		memberships: teams,
		sessionRow: store.GetSessionByTokenHashRow{
			ID: 3, Uuid: pguuid.MustParse("33333333-3333-4333-8333-333333333333"),
			UserID: 4, CurrentTeamID: &stale,
		},
	}

	identity := (&Manager{Store: database}).Authenticate(context.Background(), sessionRequest())
	if identity == nil || identity.TeamID != 1 || identity.TeamUUID != uuidString(teams[0].TeamUuid) {
		t.Fatalf("stale session team survived removal: %#v", identity)
	}
}

// A brand-new session opens on the team the user last acted in (PRD §37) —
// otherwise switching, signing out and signing back in silently undoes the
// switch, which is the bug this feature exists to fix.
func TestOpenResumesTheLastTeam(t *testing.T) {
	teams := twoTeams()
	database := &fakeSessionStore{memberships: teams, session: store.Session{ID: 70}}
	manager := &Manager{Store: database}
	last := int64(2)

	sess, _, err := manager.Open(context.Background(), httptest.NewRequest(http.MethodPost, "/auth/login", nil),
		store.User{ID: 4, Email: "u@example.test", LastTeamID: &last}, true)
	if err != nil || sess == nil {
		t.Fatalf("Open = %#v, %v", sess, err)
	}
	if sess.TeamID != 2 || sess.Role != store.TeamRoleReviewer {
		t.Fatalf("session opened in team %d as %q, want team 2 as reviewer", sess.TeamID, sess.Role)
	}
	if len(database.sessionCreates) != 1 || *database.sessionCreates[0].CurrentTeamID != 2 {
		t.Fatalf("the new session was not pinned to the remembered team: %#v", database.sessionCreates)
	}
}

// Teams() feeds the switcher, so it must offer memberships and nothing else.
func TestTeamsListsMemberships(t *testing.T) {
	database := &fakeSessionStore{memberships: twoTeams()}
	got, err := (&Manager{Store: database}).Teams(context.Background(), 4)
	if err != nil || len(got) != 2 {
		t.Fatalf("Teams = %#v, %v", got, err)
	}
	if got[0].Name != "Acme" || got[0].Role != store.TeamRoleAdmin ||
		got[1].UUID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("Teams returned %#v", got)
	}
}
