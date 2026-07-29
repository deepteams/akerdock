package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
)

// listApiTokensAs drives the handler with one identity and one scope, and
// returns both what came out and the filter that went in.
func listApiTokensAs(t *testing.T, id *auth.Identity, scope *api.ListApiTokensParamsScope) ([]api.ApiToken, []any) {
	t.Helper()
	a, db := flowAPI(t)
	db.truthy = true

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/teams/"+fixtureUUID+"/tokens", nil)
	r = r.WithContext(auth.WithIdentity(r.Context(), id))
	a.ListApiTokens(rec, r, fixtureUUID, api.ListApiTokensParams{Scope: scope})

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var body struct {
		Data []api.ApiToken `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v (%s)", err, rec.Body)
	}
	return body.Data, db.lastArgs
}

func tokenReader(userID *int64) *auth.Identity {
	return &auth.Identity{
		TokenID: 1, TokenUUID: fixtureUUID, TeamID: 1, TeamUUID: fixtureUUID,
		Permissions: auth.EffectivePermissions([]string{string(auth.PermRead)}),
		UserID:      userID,
	}
}

// The personal page must not enumerate a colleague's credentials, so the
// DEFAULT reading is "mine": the filter goes into the query, not into a loop
// the next refactor drops. An admin asks for the team-wide one explicitly.
func TestListApiTokensDefaultsToTheCallersOwn(t *testing.T) {
	me := int64(7)
	mine := api.ListApiTokensParamsScopeMine
	team := api.ListApiTokensParamsScopeTeam

	cases := map[string]struct {
		scope      *api.ListApiTokensParamsScope
		wantFilter any
	}{
		"no scope at all": {nil, &me},
		"scope=mine":      {&mine, &me},
		// nil creator = no filter = every token of the team.
		"scope=team": {&team, (*int64)(nil)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, args := listApiTokensAs(t, tokenReader(&me), tc.scope)
			// team_id, created_by, after_id, page_limit — the creator filter is
			// the second, and it is the whole subject of this test.
			if len(args) < 2 {
				t.Fatalf("query took %d args, want the creator filter among them", len(args))
			}
			got, _ := args[1].(*int64)
			want, _ := tc.wantFilter.(*int64)
			switch {
			case want == nil && got != nil:
				t.Fatalf("creator filter = %d, want none (the whole team)", *got)
			case want != nil && (got == nil || *got != *want):
				t.Fatalf("creator filter = %v, want %d", got, *want)
			}
		})
	}
}

// Fail closed: a caller who names nobody gets an empty personal list, never
// the team's. Reading "my tokens" as "everyone's" is exactly the bug.
func TestListApiTokensGivesNothingPersonalToACallerWithNoUser(t *testing.T) {
	data, _ := listApiTokensAs(t, tokenReader(nil), nil)
	if len(data) != 0 {
		t.Fatalf("got %d tokens, want none for a caller that names nobody", len(data))
	}
}

// The owner belongs to the administrative reading. On the personal one the
// answer is "yours", and stamping the caller's own address on every row says
// nothing while widening what a page leaks.
func TestOwnerEmailIsOnlyOnTheTeamScope(t *testing.T) {
	me := int64(7)
	team := api.ListApiTokensParamsScopeTeam

	if data, _ := listApiTokensAs(t, tokenReader(&me), &team); len(data) == 0 || data[0].OwnerEmail == nil {
		t.Error("the team scope must say whose each token is")
	}
	if data, _ := listApiTokensAs(t, tokenReader(&me), nil); len(data) == 0 || data[0].OwnerEmail != nil {
		t.Error("the personal scope must not carry an owner")
	}
}
