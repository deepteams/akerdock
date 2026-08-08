package handlers

// Coverage tests for the team-administration handlers: roles.go, tokens.go and
// invitations.go. Shares the authcov fakes declared in authcov_cov_test.go.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/notify"
	"github.com/deepteams/akerdock/internal/store"
)

// ---------------------------------------------------------------------------
// roles.go

func TestAuthcovCustomRoleToAPINilPermissions(t *testing.T) {
	out := customRoleToAPI(store.CustomRole{Name: "n"}, nil)
	if out.Permissions == nil || len(out.Permissions) != 0 {
		t.Fatalf("nil permissions must render as an empty list, got %#v", out.Permissions)
	}
}

func TestAuthcovRoleUUIDPathsAnswer404(t *testing.T) {
	a := authcovAPI(t, &authcovDB{})

	get := httptest.NewRecorder()
	a.GetTeamRole(get, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID, "not-a-uuid")
	if get.Code != http.StatusNotFound {
		t.Fatalf("GetTeamRole bad uuid = %d, want 404: %s", get.Code, get.Body)
	}

	update := httptest.NewRecorder()
	a.UpdateTeamRole(update, authcovRequest(http.MethodPatch, "/t", `{}`), fixtureUUID, "not-a-uuid")
	if update.Code != http.StatusNotFound {
		t.Fatalf("UpdateTeamRole bad uuid = %d, want 404: %s", update.Code, update.Body)
	}

	del := httptest.NewRecorder()
	a.DeleteTeamRole(del, authcovRequest(http.MethodDelete, "/t", ""), fixtureUUID, "not-a-uuid")
	if del.Code != http.StatusNotFound {
		t.Fatalf("DeleteTeamRole bad uuid = %d, want 404: %s", del.Code, del.Body)
	}
}

func TestAuthcovListTeamRolesBranches(t *testing.T) {
	t.Run("limit out of range", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{})
		rec := httptest.NewRecorder()
		a.ListTeamRoles(rec, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID,
			api.ListTeamRolesParams{Limit: ptr(0)})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("invalid cursor", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{})
		rec := httptest.NewRecorder()
		a.ListTeamRoles(rec, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID,
			api.ListTeamRolesParams{Cursor: ptr("!!not-base64!!")})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"ListCustomRolesPage": {err: errors.New("db down")},
		}})
		rec := httptest.NewRecorder()
		a.ListTeamRoles(rec, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID, api.ListTeamRolesParams{})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("an over-full page emits the next cursor", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{rowsN: map[string]int{"ListCustomRolesPage": 2}})
		rec := httptest.NewRecorder()
		a.ListTeamRoles(rec, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID,
			api.ListTeamRolesParams{Limit: ptr(1)})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"next_cursor":"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovCreateTeamRoleBranches(t *testing.T) {
	post := func(a *API, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.CreateTeamRole(rec, authcovRequest(http.MethodPost, "/t", body), fixtureUUID)
		return rec
	}

	t.Run("malformed body", func(t *testing.T) {
		if rec := post(authcovAPI(t, &authcovDB{}), "{"); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("empty name", func(t *testing.T) {
		if rec := post(authcovAPI(t, &authcovDB{}), `{"name":"","permissions":["projects:read"]}`); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
		}
	})

	t.Run("unknown permission", func(t *testing.T) {
		if rec := post(authcovAPI(t, &authcovDB{}), `{"name":"n","permissions":["nope:nope"]}`); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
		}
	})

	t.Run("duplicate name", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"CreateCustomRole": {err: authcovUniqueViolation()},
		}})
		if rec := post(a, `{"name":"n","permissions":["projects:read"]}`); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"CreateCustomRole": {err: errors.New("db down")},
		}})
		if rec := post(a, `{"name":"n","permissions":["projects:read"]}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("valid role is created", func(t *testing.T) {
		if rec := post(authcovAPI(t, &authcovDB{}), `{"name":"n","permissions":["projects:read"]}`); rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovUpdateTeamRoleBranches(t *testing.T) {
	patch := func(a *API, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.UpdateTeamRole(rec, authcovRequest(http.MethodPatch, "/t", body), fixtureUUID, fixtureUUID)
		return rec
	}

	t.Run("malformed body", func(t *testing.T) {
		if rec := patch(authcovAPI(t, &authcovDB{}), "{"); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("empty name", func(t *testing.T) {
		if rec := patch(authcovAPI(t, &authcovDB{}), `{"name":""}`); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
		}
	})

	t.Run("unknown permission", func(t *testing.T) {
		if rec := patch(authcovAPI(t, &authcovDB{}), `{"permissions":["nope:nope"]}`); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
		}
	})

	t.Run("missing role", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{noRowsOn: []string{"UpdateCustomRole"}})
		if rec := patch(a, `{"name":"n"}`); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("duplicate name", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"UpdateCustomRole": {err: authcovUniqueViolation()},
		}})
		if rec := patch(a, `{"name":"n"}`); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"UpdateCustomRole": {err: errors.New("db down")},
		}})
		if rec := patch(a, `{"name":"n"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("full patch is applied", func(t *testing.T) {
		rec := patch(authcovAPI(t, &authcovDB{}),
			`{"name":"n","description":null,"permissions":["projects:read"]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovDeleteTeamRoleBranches(t *testing.T) {
	t.Run("store failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"DeleteCustomRole": {err: errors.New("db down")},
		}})
		rec := httptest.NewRecorder()
		a.DeleteTeamRole(rec, authcovRequest(http.MethodDelete, "/t", ""), fixtureUUID, fixtureUUID)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("missing role", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{execTag: map[string]string{"DeleteCustomRole": "DELETE 0"}})
		rec := httptest.NewRecorder()
		a.DeleteTeamRole(rec, authcovRequest(http.MethodDelete, "/t", ""), fixtureUUID, fixtureUUID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})
}

// authcovAdminMemberFill makes every member row scan as a plain `admin` with
// no custom role, so the last-admin guard is reachable; count says how many
// admins CountTeamAdmins reports.
func authcovAdminMemberFill(count int64) func(dest any) bool {
	return func(dest any) bool {
		switch d := dest.(type) {
		case *store.TeamRole:
			*d = store.TeamRoleAdmin
			return true
		case **int64: // CustomRoleID and friends stay null
			return true
		case **string: // CustomRoleName stays null
			return true
		case *int64:
			*d = count
			return true
		}
		return false
	}
}

func TestAuthcovUpdateTeamMemberBranches(t *testing.T) {
	patch := func(a *API, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.UpdateTeamMember(rec, authcovRequest(http.MethodPatch, "/t", body), fixtureUUID, fixtureUUID)
		return rec
	}

	t.Run("malformed body", func(t *testing.T) {
		if rec := patch(authcovAPI(t, &authcovDB{}), "{"); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("unknown role", func(t *testing.T) {
		if rec := patch(authcovAPI(t, &authcovDB{}), `{"role":"emperor"}`); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
		}
	})

	t.Run("bad member uuid", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{})
		rec := httptest.NewRecorder()
		a.UpdateTeamMember(rec, authcovRequest(http.MethodPatch, "/t", `{"role":"member"}`), fixtureUUID, "not-a-uuid")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("unknown member", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{noRowsOn: []string{"GetTeamMemberByUUID"}})
		if rec := patch(a, `{"role":"member"}`); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("member lookup failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"GetTeamMemberByUUID": {err: errors.New("db down")},
		}})
		if rec := patch(a, `{"role":"member"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("custom role requires its uuid", func(t *testing.T) {
		if rec := patch(authcovAPI(t, &authcovDB{}), `{"role":"custom"}`); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
		}
	})

	t.Run("custom role bad uuid", func(t *testing.T) {
		if rec := patch(authcovAPI(t, &authcovDB{}), `{"role":"custom","custom_role_uuid":"nope"}`); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("custom role unknown", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{noRowsOn: []string{"GetCustomRoleByUUID"}})
		if rec := patch(a, `{"role":"custom","custom_role_uuid":"`+fixtureUUID+`"}`); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("custom role lookup failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"GetCustomRoleByUUID": {err: errors.New("db down")},
		}})
		if rec := patch(a, `{"role":"custom","custom_role_uuid":"`+fixtureUUID+`"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("custom role assignment succeeds", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{})
		if rec := patch(a, `{"role":"custom","custom_role_uuid":"`+fixtureUUID+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
	})

	t.Run("the last admin cannot be demoted", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{fill: authcovAdminMemberFill(1)})
		if rec := patch(a, `{"role":"member"}`); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
		}
	})

	t.Run("admin count failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{
			fill:  authcovAdminMemberFill(1),
			errOn: map[string]*authcovFail{"CountTeamAdmins": {err: errors.New("db down")}},
		})
		if rec := patch(a, `{"role":"member"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("a spare admin may be demoted", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{fill: authcovAdminMemberFill(2)})
		if rec := patch(a, `{"role":"member"}`); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
	})

	t.Run("update failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{
			fill:  authcovAdminMemberFill(2),
			errOn: map[string]*authcovFail{"UpdateTeamMemberRole": {err: errors.New("db down")}},
		})
		if rec := patch(a, `{"role":"member"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("member vanished during update", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{
			fill:    authcovAdminMemberFill(2),
			execTag: map[string]string{"UpdateTeamMemberRole": "UPDATE 0"},
		})
		if rec := patch(a, `{"role":"member"}`); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("re-read failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{
			fill:  authcovAdminMemberFill(2),
			errOn: map[string]*authcovFail{"GetTeamMemberByUUID": {err: errors.New("db down"), skip: 1}},
		})
		if rec := patch(a, `{"role":"member"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})
}

// ---------------------------------------------------------------------------
// tokens.go

func TestAuthcovTokenToAPIRendersDetails(t *testing.T) {
	out := tokenToAPI(store.ApiToken{
		Uuid: pguuidFromAuthcov(t, fixtureUUID), Name: "t",
		Permissions: []string{"read", "write"},
		IpAllowlist: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	})
	if len(out.Permissions) != 2 || len(*out.IpAllowlist) != 1 {
		t.Fatalf("tokenToAPI dropped details: %#v", out)
	}
}

func TestAuthcovListApiTokensBranches(t *testing.T) {
	t.Run("limit out of range", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{})
		rec := httptest.NewRecorder()
		a.ListApiTokens(rec, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID,
			api.ListApiTokensParams{Limit: ptr(0)})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("invalid cursor", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{})
		rec := httptest.NewRecorder()
		a.ListApiTokens(rec, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID,
			api.ListApiTokensParams{Cursor: ptr("!!not-base64!!")})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"ListApiTokensPage": {err: errors.New("db down")},
		}})
		rec := httptest.NewRecorder()
		a.ListApiTokens(rec, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID, api.ListApiTokensParams{})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("an over-full page emits the next cursor", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{rowsN: map[string]int{"ListApiTokensPage": 2}})
		rec := httptest.NewRecorder()
		a.ListApiTokens(rec, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID,
			api.ListApiTokensParams{Limit: ptr(1)})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"next_cursor":"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("a creatorless token gets an empty personal list", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{})
		id := authcovRootIdentity()
		id.UserID = nil
		rec := httptest.NewRecorder()
		a.ListApiTokens(rec, authcovRequestAs(id, http.MethodGet, "/t", ""), fixtureUUID, api.ListApiTokensParams{})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovCreateApiTokenBranches(t *testing.T) {
	post := func(a *API, id *auth.Identity, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.CreateApiToken(rec, authcovRequestAs(id, http.MethodPost, "/t", body), fixtureUUID, api.CreateApiTokenParams{})
		return rec
	}

	t.Run("creatorless caller is refused", func(t *testing.T) {
		id := authcovRootIdentity()
		id.UserID = nil
		rec := post(authcovAPI(t, &authcovDB{}), id, `{"name":"t","permissions":["read"]}`)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), codeTokenWithoutCreator) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		if rec := post(authcovAPI(t, &authcovDB{}), authcovRootIdentity(), "{"); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("validation details accumulate", func(t *testing.T) {
		// Empty name, an unknown permission and a bad CIDR, all at once.
		rec := post(authcovAPI(t, &authcovDB{}), authcovRootIdentity(),
			`{"name":"","permissions":["nope"],"ip_allowlist":["not-a-cidr"]}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
		}
		for _, field := range []string{"name", "permissions", "ip_allowlist"} {
			if !strings.Contains(rec.Body.String(), field) {
				t.Fatalf("validation details omit %q: %s", field, rec.Body)
			}
		}
	})

	t.Run("no permissions at all", func(t *testing.T) {
		rec := post(authcovAPI(t, &authcovDB{}), authcovRootIdentity(), `{"name":"t","permissions":[]}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
		}
	})

	t.Run("a token cannot outgrow its creator", func(t *testing.T) {
		id := authcovRootIdentity()
		id.Permissions = []string{string(auth.PermTokensCreate)}
		rec := post(authcovAPI(t, &authcovDB{}), id, `{"name":"t","permissions":["deploy"]}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"CreateApiToken": {err: errors.New("db down")},
		}})
		rec := post(a, authcovRootIdentity(), `{"name":"t","permissions":["read"]}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("expiry and allowlist are persisted", func(t *testing.T) {
		rec := post(authcovAPI(t, &authcovDB{}), authcovRootIdentity(),
			`{"name":"t","permissions":["read"],"ip_allowlist":["10.0.0.0/8"],"expires_at":"2027-01-02T03:04:05Z"}`)
		if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"token"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovRevokeApiTokenBranches(t *testing.T) {
	t.Run("bad uuid", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{})
		rec := httptest.NewRecorder()
		a.RevokeApiToken(rec, authcovRequest(http.MethodDelete, "/t", ""), fixtureUUID, "not-a-uuid")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"RevokeApiTokenByUUID": {err: errors.New("db down")},
		}})
		rec := httptest.NewRecorder()
		a.RevokeApiToken(rec, authcovRequest(http.MethodDelete, "/t", ""), fixtureUUID, fixtureUUID)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("missing token", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{execTag: map[string]string{"RevokeApiTokenByUUID": "UPDATE 0"}})
		rec := httptest.NewRecorder()
		a.RevokeApiToken(rec, authcovRequest(http.MethodDelete, "/t", ""), fixtureUUID, fixtureUUID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})
}

// ---------------------------------------------------------------------------
// invitations.go

// authcovEmailFill scans every plain string column as a well-formed address:
// the invitation payload renders its email through types.Email, which refuses
// to marshal the default "unit" fixture.
func authcovEmailFill(dest any) bool {
	if s, ok := dest.(*string); ok {
		*s = "unit@example.test"
		return true
	}
	return false
}

func TestAuthcovInvitationStatusOf(t *testing.T) {
	now := time.Now()
	valid := fixtureTimestamp(now.Add(time.Hour))
	cases := []struct {
		name                       string
		accepted, revoked, expires pgtype.Timestamptz
		want                       api.InvitationStatus
	}{
		{"accepted", fixtureTimestamp(now), pgtype.Timestamptz{}, valid, "accepted"},
		{"revoked", pgtype.Timestamptz{}, fixtureTimestamp(now), valid, "revoked"},
		{"expired", pgtype.Timestamptz{}, pgtype.Timestamptz{}, fixtureTimestamp(now.Add(-time.Hour)), "expired"},
		{"pending", pgtype.Timestamptz{}, pgtype.Timestamptz{}, valid, "pending"},
	}
	for _, c := range cases {
		if got := invitationStatusOf(c.accepted, c.revoked, c.expires); got != c.want {
			t.Errorf("%s: status = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAuthcovListTeamInvitationsBranches(t *testing.T) {
	t.Run("limit out of range", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{})
		rec := httptest.NewRecorder()
		a.ListTeamInvitations(rec, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID,
			api.ListTeamInvitationsParams{Limit: ptr(101)})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("invalid cursor", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{})
		rec := httptest.NewRecorder()
		a.ListTeamInvitations(rec, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID,
			api.ListTeamInvitationsParams{Cursor: ptr("!!not-base64!!")})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"ListInvitationsPage": {err: errors.New("db down")},
		}})
		rec := httptest.NewRecorder()
		a.ListTeamInvitations(rec, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID, api.ListTeamInvitationsParams{})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("an over-full page emits the next cursor", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{rowsN: map[string]int{"ListInvitationsPage": 2}, fill: authcovEmailFill})
		rec := httptest.NewRecorder()
		a.ListTeamInvitations(rec, authcovRequest(http.MethodGet, "/t", ""), fixtureUUID,
			api.ListTeamInvitationsParams{Limit: ptr(1)})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"next_cursor":"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovCreateTeamInvitationBranches(t *testing.T) {
	post := func(a *API, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.CreateTeamInvitation(rec, authcovRequest(http.MethodPost, "/t", body), fixtureUUID, api.CreateTeamInvitationParams{})
		return rec
	}

	t.Run("malformed body", func(t *testing.T) {
		if rec := post(authcovAPI(t, &authcovDB{}), "{"); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("invalid email", func(t *testing.T) {
		// A quoted local part survives the JSON-level types.Email validation
		// with a raw space inside — exactly what the handler must still refuse.
		if rec := post(authcovAPI(t, &authcovDB{}), `{"email":"\"a b\"@example.test"}`); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
		}
	})

	t.Run("admin and reviewer roles", func(t *testing.T) {
		for _, role := range []string{"admin", "reviewer"} {
			rec := post(authcovAPI(t, &authcovDB{}), `{"email":"x@example.test","role":"`+role+`"}`)
			if rec.Code != http.StatusCreated {
				t.Fatalf("role %s: status = %d, want 201: %s", role, rec.Code, rec.Body)
			}
		}
	})

	t.Run("custom role requires its uuid", func(t *testing.T) {
		if rec := post(authcovAPI(t, &authcovDB{}), `{"email":"x@example.test","role":"custom"}`); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
		}
	})

	t.Run("custom role bad uuid", func(t *testing.T) {
		if rec := post(authcovAPI(t, &authcovDB{}), `{"email":"x@example.test","role":"custom","custom_role_uuid":"nope"}`); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("custom role unknown", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{noRowsOn: []string{"GetCustomRoleByUUID"}})
		if rec := post(a, `{"email":"x@example.test","role":"custom","custom_role_uuid":"`+fixtureUUID+`"}`); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("custom role invitation carries the role", func(t *testing.T) {
		rec := post(authcovAPI(t, &authcovDB{fill: authcovEmailFill}), `{"email":"x@example.test","role":"custom","custom_role_uuid":"`+fixtureUUID+`"}`)
		if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"role":"custom"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("expiry out of range", func(t *testing.T) {
		if rec := post(authcovAPI(t, &authcovDB{}), `{"email":"x@example.test","expires_in_hours":0}`); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
		}
	})

	t.Run("custom expiry accepted", func(t *testing.T) {
		if rec := post(authcovAPI(t, &authcovDB{}), `{"email":"x@example.test","expires_in_hours":24}`); rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
		}
	})

	t.Run("duplicate active invitation", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"CreateInvitation": {err: authcovUniqueViolation()},
		}})
		if rec := post(a, `{"email":"x@example.test"}`); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"CreateInvitation": {err: errors.New("db down")},
		}})
		if rec := post(a, `{"email":"x@example.test"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovRevokeTeamInvitationBranches(t *testing.T) {
	t.Run("bad uuid", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{})
		rec := httptest.NewRecorder()
		a.RevokeTeamInvitation(rec, authcovRequest(http.MethodDelete, "/t", ""), fixtureUUID, "not-a-uuid")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"RevokeInvitation": {err: errors.New("db down")},
		}})
		rec := httptest.NewRecorder()
		a.RevokeTeamInvitation(rec, authcovRequest(http.MethodDelete, "/t", ""), fixtureUUID, fixtureUUID)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("missing invitation", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{execTag: map[string]string{"RevokeInvitation": "UPDATE 0"}})
		rec := httptest.NewRecorder()
		a.RevokeTeamInvitation(rec, authcovRequest(http.MethodDelete, "/t", ""), fixtureUUID, fixtureUUID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovResendTeamInvitationBranches(t *testing.T) {
	t.Run("bad uuid", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{})
		rec := httptest.NewRecorder()
		a.ResendTeamInvitation(rec, authcovRequest(http.MethodPost, "/t", ""), fixtureUUID, "not-a-uuid")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("nothing pending answers 404", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{noRowsOn: []string{"RotateInvitation"}})
		rec := httptest.NewRecorder()
		a.ResendTeamInvitation(rec, authcovRequest(http.MethodPost, "/t", ""), fixtureUUID, fixtureUUID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{errOn: map[string]*authcovFail{
			"RotateInvitation": {err: errors.New("db down")},
		}})
		rec := httptest.NewRecorder()
		a.ResendTeamInvitation(rec, authcovRequest(http.MethodPost, "/t", ""), fixtureUUID, fixtureUUID)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})
}

// mailInvitation is best-effort by contract: every branch ends without failing
// the caller, so what is asserted here is only which transport was chosen.
func TestAuthcovMailInvitationBranches(t *testing.T) {
	build := func(t *testing.T, cfg instanceEmail) *API {
		t.Helper()
		db := &authcovDB{}
		a := authcovAPI(t, db)
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		enc, err := a.Keyring.Encrypt("instance_settings", "transactional_email_config_enc", "1", raw)
		if err != nil {
			t.Fatal(err)
		}
		db.fill = func(dest any) bool {
			if b, ok := dest.(*[]byte); ok {
				*b = enc
				return true
			}
			return false
		}
		return a
	}
	req := httptest.NewRequest(http.MethodPost, "/t", nil)

	t.Run("not configured is a no-op", func(t *testing.T) {
		a := authcovAPI(t, &authcovDB{}) // "{}" ciphertext never decrypts
		a.mailInvitation(req, "x@example.test", "https://link")
	})

	t.Run("smtp failure is logged, never fatal", func(t *testing.T) {
		a := build(t, instanceEmail{Kind: "smtp", From: "noreply@example.test", Config: notify.Config{
			SMTP: &notify.SMTPConfig{
				Host: "127.0.0.1", Port: 1, From: "noreply@example.test", Encryption: "none",
			},
		}})
		a.mailInvitation(req, "x@example.test", "https://link")
	})

	t.Run("resend config rides the resend branch", func(t *testing.T) {
		// The kind is unknown on purpose: notify refuses it before any network
		// I/O, and the handler's only duty is to have addressed the invitee.
		a := build(t, instanceEmail{Kind: "unknown-kind", From: "noreply@example.test", Config: notify.Config{
			Resend: &notify.ResendConfig{APIKey: "k", From: "noreply@example.test"},
		}})
		a.mailInvitation(req, "x@example.test", "https://link")
	})

	t.Run("no transport at all returns quietly", func(t *testing.T) {
		a := build(t, instanceEmail{Kind: "smtp", From: "noreply@example.test"})
		a.mailInvitation(req, "x@example.test", "https://link")
	})
}
