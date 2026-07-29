package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/session"
)

// invitationAPI wires the browser-session doubles onto the flow API. The
// default fake answers every query with a row, which is exactly the "this
// address already has an account" shape these tests need most.
func invitationAPI(t *testing.T) (*API, *flowDB) {
	t.Helper()
	a, db := flowAPI(t)
	a.Sessions = &session.Manager{Store: newBrowserSessionStore(t)}
	return a, db
}

func postJSON(target, body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
}

// The signup endpoint is the one place where an anonymous caller creates an
// account, so what it REFUSES is the specification.
func TestSignUpFromInvitationRefusals(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		truthy bool // instance settings read as "everything on" — SSO-only here
		want   int
	}{
		{
			name: "no token is not a signup",
			body: `{"name":"X","password":"a-long-enough-password"}`,
			want: http.StatusBadRequest,
		},
		{
			// The instance password policy applies here as it does anywhere a
			// password is set: an invitation is not a way around it.
			name: "weak password",
			body: `{"token":"t","name":"X","password":"short"}`,
			want: http.StatusUnprocessableEntity,
		},
		{
			// SSO-only instance: creating a password account here would be a
			// hole in the very setting that forbids password logins.
			name:   "password login disabled",
			body:   `{"token":"t","name":"X","password":"a-long-enough-password"}`,
			truthy: true,
			want:   http.StatusForbidden,
		},
		{
			// The security property that matters most: holding an invitation
			// link must never let anyone set a password on an account that
			// already exists for that address.
			name: "the address already has an account",
			body: `{"token":"t","name":"X","password":"a-long-enough-password"}`,
			want: http.StatusConflict,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, db := invitationAPI(t)
			db.truthy = tc.truthy
			rec := httptest.NewRecorder()
			a.SignUpFromInvitation(rec, postJSON("/auth/invitations/signup", tc.body))
			if rec.Code != tc.want {
				t.Fatalf("SignUpFromInvitation = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// An invalid, revoked, used or expired link is one single answer: the endpoint
// must not become a way to probe which invitations exist (INV-002).
func TestInvitationLookupHidesUnknownLinks(t *testing.T) {
	a, db := invitationAPI(t)
	db.noRows = true

	rec := httptest.NewRecorder()
	a.InvitationInfo(rec, postJSON("/auth/invitations/lookup", `{"token":"nope"}`))
	if rec.Code != http.StatusGone {
		t.Fatalf("InvitationInfo = %d, want 410: %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	a.SignUpFromInvitation(rec, postJSON("/auth/invitations/signup",
		`{"token":"nope","name":"X","password":"a-long-enough-password"}`))
	if rec.Code != http.StatusGone {
		t.Fatalf("SignUpFromInvitation on a dead link = %d, want 410", rec.Code)
	}
}

// The landing page needs enough to choose a screen — and the invitee's own
// address, which it displays rather than asks for.
func TestInvitationLookupDescribesTheInvitation(t *testing.T) {
	a, _ := invitationAPI(t)

	rec := httptest.NewRecorder()
	a.InvitationInfo(rec, postJSON("/auth/invitations/lookup", `{"token":"t"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("InvitationInfo = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"email", "team_name", "role", "account_exists", "password_login_disabled"} {
		if _, ok := body[field]; !ok {
			t.Fatalf("the landing page cannot choose a screen without %q: %v", field, body)
		}
	}

	// A token is required: an empty body must not describe anything.
	rec = httptest.NewRecorder()
	a.InvitationInfo(rec, postJSON("/auth/invitations/lookup", `{}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("InvitationInfo without a token = %d, want 400", rec.Code)
	}
}

// Both endpoints belong to the dashboard: an API-only deployment has no
// sessions to open and must not answer them at all.
func TestInvitationEndpointsNeedSessions(t *testing.T) {
	a, _ := flowAPI(t)
	a.Sessions = nil

	for _, call := range []func(http.ResponseWriter, *http.Request){
		a.InvitationInfo, a.SignUpFromInvitation,
	} {
		rec := httptest.NewRecorder()
		call(rec, postJSON("/auth/invitations/x", `{"token":"t"}`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("without sessions = %d, want 404", rec.Code)
		}
	}
}

// Accepting an invitation moves the session INTO the team just joined. Somebody
// who already belongs to another team would otherwise be left in it, looking at
// the old team's data one click after saying "join this team".
func TestAcceptInvitationEntersTheJoinedTeam(t *testing.T) {
	a, _ := invitationAPI(t)
	st := a.Sessions.Store.(*browserSessionStore)
	// The link must belong to the signed-in account: line up the session email
	// with the one the fake invitation carries.
	st.sessionRow.Email = "unit"

	rec := httptest.NewRecorder()
	a.AcceptInvitation(rec, authenticatedBrowserRequest(t, http.MethodPost,
		"/auth/invitations/accept", `{"token":"t"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("AcceptInvitation = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body struct {
		TeamUUID string `json:"team_uuid"`
		Switched bool   `json:"switched"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Switched || body.TeamUUID == "" {
		t.Fatalf("response = %#v, want a switch into the joined team", body)
	}
	if len(st.switched) != 1 {
		t.Fatalf("the session was not moved: %#v", st.switched)
	}
}
