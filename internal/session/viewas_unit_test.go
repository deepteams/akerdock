package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

const customRoleUUID = "33333333-3333-4333-8333-333333333333"

// inspectorStore is a root admin's session, ready to be put into view-as mode.
func inspectorStore(t *testing.T) *fakeSessionStore {
	t.Helper()
	team := int64(5)
	return &fakeSessionStore{
		sessionRow: store.GetSessionByTokenHashRow{
			ID: 3, Uuid: pguuid.MustParse("11111111-2222-4333-8444-555555555555"),
			UserID: 4, CurrentTeamID: &team,
		},
		membership: store.GetTeamMembershipForUserRow{
			TeamID: 5, TeamUuid: pguuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"),
			Role: store.TeamRoleAdmin, IsRoot: true,
		},
		customRole: store.CustomRole{
			ID: 7, TeamID: 5, Uuid: pguuid.MustParse(customRoleUUID), Name: "Auditor",
			Permissions: []string{string(auth.PermAuditRead)},
		},
	}
}

func authenticateWith(t *testing.T, database *fakeSessionStore) *auth.Identity {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: CookieName, Value: "clear"})
	return (&Manager{Store: database}).Authenticate(context.Background(), request)
}

func TestViewAsNarrowsARootSessionToTheSimulatedRole(t *testing.T) {
	database := inspectorStore(t)
	reviewer := store.TeamRoleReviewer
	database.sessionRow.ViewAsRole = &reviewer

	identity := authenticateWith(t, database)
	if identity == nil {
		t.Fatal("the session stopped authenticating in view-as mode")
	}
	if identity.ViewAs != "reviewer" {
		t.Fatalf("ViewAs = %q, want reviewer", identity.ViewAs)
	}
	// The whole point: root loses its wildcard, or the inspection would show
	// the root's own view under someone else's name.
	if identity.IsRoot() || identity.InstanceRoot {
		t.Fatalf("a simulated reviewer kept root: %#v", identity.Permissions)
	}
	if !slices.Contains(identity.Permissions, string(auth.PermPreviewsRead)) {
		t.Fatalf("a reviewer must still read previews: %#v", identity.Permissions)
	}
	for _, forbidden := range []string{
		string(auth.PermApplicationsUpdate), string(auth.PermSecretsRead), string(auth.PermMembersRead),
	} {
		if slices.Contains(identity.Permissions, forbidden) {
			t.Fatalf("a simulated reviewer kept %s: %#v", forbidden, identity.Permissions)
		}
	}
}

func TestViewAsCannotGrantWhatTheSessionDoesNotHold(t *testing.T) {
	// A plain member simulating `admin` must not become one: the mode is an
	// intersection, never a substitution.
	database := inspectorStore(t)
	database.membership.Role = store.TeamRoleMember
	database.membership.IsRoot = false
	admin := store.TeamRoleAdmin
	database.sessionRow.ViewAsRole = &admin

	identity := authenticateWith(t, database)
	if identity == nil {
		t.Fatal("no identity")
	}
	for _, escalated := range []string{
		string(auth.PermMembersManage), string(auth.PermTokensCreate), string(auth.PermRoot),
	} {
		if slices.Contains(identity.Permissions, escalated) {
			t.Fatalf("simulating admin granted %s: %#v", escalated, identity.Permissions)
		}
	}
}

func TestViewAsCustomRoleAndItsDisappearance(t *testing.T) {
	database := inspectorStore(t)
	roleID := int64(7)
	database.sessionRow.ViewAsCustomRoleID = &roleID

	identity := authenticateWith(t, database)
	if identity == nil || identity.ViewAs != "Auditor" {
		t.Fatalf("custom role not simulated: %#v", identity)
	}
	if !slices.Contains(identity.Permissions, string(auth.PermAuditRead)) {
		t.Fatalf("the custom role's own permission is missing: %#v", identity.Permissions)
	}

	// The role vanished (or the session moved teams): granting nothing beats
	// silently handing the real powers back under a banner that still claims a
	// simulated role.
	database.customRole = store.CustomRole{}
	identity = authenticateWith(t, database)
	if identity == nil || len(identity.Permissions) != 0 || identity.IsRoot() {
		t.Fatalf("a stale simulation kept permissions: %#v", identity)
	}
}

func TestSetViewAsIsReservedToAdminsAndAlwaysLeavable(t *testing.T) {
	database := inspectorStore(t)
	manager := &Manager{Store: database}
	ctx := context.Background()

	if _, err := manager.SetViewAs(ctx, 4, 3, 5, "reviewer", ""); err != nil {
		t.Fatalf("an admin could not enter the mode: %v", err)
	}
	if len(database.viewAsSets) != 1 || database.viewAsSets[0].ViewAsRole == nil ||
		*database.viewAsSets[0].ViewAsRole != store.TeamRoleReviewer {
		t.Fatalf("view-as not stored: %#v", database.viewAsSets)
	}

	// Leaving reads the REAL membership, not the narrowed permissions: an admin
	// inspecting `reviewer` must never be locked into a session they cannot
	// restore.
	if _, err := manager.SetViewAs(ctx, 4, 3, 5, "", ""); err != nil {
		t.Fatalf("leaving the mode failed: %v", err)
	}
	last := database.viewAsSets[len(database.viewAsSets)-1]
	if last.ViewAsRole != nil || last.ViewAsCustomRoleID != nil {
		t.Fatalf("leaving did not clear the mode: %#v", last)
	}

	if label, err := manager.SetViewAs(ctx, 4, 3, 5, "", customRoleUUID); err != nil || label != "Auditor" {
		t.Fatalf("custom role = %q, %v", label, err)
	}
	if _, err := manager.SetViewAs(ctx, 4, 3, 5, "sorcerer", ""); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("unknown role = %v, want ErrRoleNotFound", err)
	}

	database.membership.Role = store.TeamRoleMember
	database.membership.IsRoot = false
	if _, err := manager.SetViewAs(ctx, 4, 3, 5, "reviewer", ""); !errors.Is(err, ErrNotAllowedToViewAs) {
		t.Fatalf("a member entered the inspection mode: %v", err)
	}
	// ...but leaving still works for them: an admin demoted mid-inspection must
	// not stay stuck in a role they can no longer choose. Restoring one's own
	// authority grants nothing, so it needs no authority to ask for.
	before := len(database.viewAsSets)
	if _, err := manager.SetViewAs(ctx, 4, 3, 5, "", ""); err != nil {
		t.Fatalf("a demoted admin could not leave the mode: %v", err)
	}
	if len(database.viewAsSets) != before+1 {
		t.Fatal("leaving was refused")
	}
}

func TestSwitchingTeamLeavesTheInspectionMode(t *testing.T) {
	database := inspectorStore(t)
	database.memberships = []store.ListTeamMembershipsForUserRow{
		{TeamID: 5, TeamUuid: database.membership.TeamUuid, TeamName: "Acme", Role: store.TeamRoleAdmin},
	}
	manager := &Manager{Store: database}

	if _, err := manager.SwitchTeam(context.Background(), 4, 3, uuidString(database.membership.TeamUuid)); err != nil {
		t.Fatalf("SwitchTeam: %v", err)
	}
	// A simulated role belongs to the team it was chosen in — crossing the
	// boundary with it would show one team's data through another's role.
	if len(database.viewAsSets) != 1 || database.viewAsSets[0].ViewAsRole != nil {
		t.Fatalf("the team switch kept the simulated role: %#v", database.viewAsSets)
	}
}
