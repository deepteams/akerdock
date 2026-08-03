package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

func inspectorAPI(t *testing.T) (*API, *browserSessionStore) {
	t.Helper()
	a, _ := flowAPI(t)
	st := newBrowserSessionStore(t)
	st.memberships = []store.ListTeamMembershipsForUserRow{
		{TeamID: 1, Role: store.TeamRoleAdmin, TeamUuid: st.sessionRow.Uuid, TeamName: "Acme"},
	}
	a.Sessions = &session.Manager{Store: st}
	return a, st
}

func TestViewAsEntersAndLeavesTheMode(t *testing.T) {
	a, st := inspectorAPI(t)

	rec := httptest.NewRecorder()
	a.ViewAs(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/session/view-as", `{"role":"reviewer"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("ViewAs = %d, want 200: %s", rec.Code, rec.Body)
	}
	var entered struct {
		ViewAs *string `json:"view_as"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &entered); err != nil {
		t.Fatal(err)
	}
	if entered.ViewAs == nil || *entered.ViewAs != "reviewer" {
		t.Fatalf("view_as = %#v", entered.ViewAs)
	}

	rec = httptest.NewRecorder()
	a.ViewAs(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/session/view-as", `{}`))
	var left struct {
		ViewAs *string `json:"view_as"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &left); err != nil {
		t.Fatal(err)
	}
	// null, not "": the dashboard reads this field to decide whether to show
	// the banner at all.
	if left.ViewAs != nil {
		t.Fatalf("leaving returned %#v", left.ViewAs)
	}
	if len(st.viewAsSets) != 2 || st.viewAsSets[1].ViewAsRole != nil {
		t.Fatalf("the mode was not cleared: %#v", st.viewAsSets)
	}
}

func TestViewAsRefusesUnknownRolesAndNonAdmins(t *testing.T) {
	a, st := inspectorAPI(t)

	rec := httptest.NewRecorder()
	a.ViewAs(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/session/view-as", `{"role":"sorcerer"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown role = %d, want 404: %s", rec.Code, rec.Body)
	}

	st.memberships[0].Role = store.TeamRoleMember
	rec = httptest.NewRecorder()
	a.ViewAs(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/session/view-as", `{"role":"reviewer"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member = %d, want 403: %s", rec.Code, rec.Body)
	}
}

func TestListViewAsRolesOffersTheSystemRoles(t *testing.T) {
	a, st := inspectorAPI(t)

	rec := httptest.NewRecorder()
	a.ListViewAsRoles(rec, authenticatedBrowserRequest(t, http.MethodGet, "/auth/session/view-as", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("ListViewAsRoles = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			Role           string `json:"role"`
			CustomRoleUUID string `json:"custom_role_uuid"`
			Name           string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) < 3 {
		t.Fatalf("roles = %#v", body.Data)
	}
	for i, want := range []string{"admin", "member", "reviewer"} {
		if body.Data[i].Role != want {
			t.Fatalf("role %d = %q, want %q", i, body.Data[i].Role, want)
		}
	}

	// A session already narrowed to `reviewer` holds no roles:read — it must
	// still be able to list, and therefore to leave.
	st.memberships[0].Role = store.TeamRoleMember
	rec = httptest.NewRecorder()
	a.ListViewAsRoles(rec, authenticatedBrowserRequest(t, http.MethodGet, "/auth/session/view-as", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member listing = %d, want 403: %s", rec.Code, rec.Body)
	}
}
