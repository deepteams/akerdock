package handlers

// Remaining reachable branches: full pages that mint a next_cursor, malformed
// path UUIDs, and rule-scope parents that resolve to nothing.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/api"
)

// TestOpscovFullPagesMintNextCursor drives every paginated list of the ops
// slice with limit=1 while the store returns limit+1 rows, so the page is
// truncated and the cursor lambda runs.
func TestOpscovFullPagesMintNextCursor(t *testing.T) {
	fullPage := func(t *testing.T, query string, call func(*API, *httptest.ResponseRecorder)) {
		t.Helper()
		t.Run(query, func(t *testing.T) {
			a, db := opscovAPI(t)
			db.on(query).rows = 2
			rec := httptest.NewRecorder()
			call(a, rec)
			if rec.Code != http.StatusOK {
				t.Fatalf("full page = %d, want 200: %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), `"next_cursor":"`) {
				t.Errorf("full page must carry a next_cursor: %s", rec.Body)
			}
		})
	}
	one := ptr(1)

	fullPage(t, "ListNotificationChannelsPage", func(a *API, rec *httptest.ResponseRecorder) {
		a.ListNotificationChannels(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListNotificationChannelsParams{Limit: one})
	})
	fullPage(t, "ListPrivateKeysPage", func(a *API, rec *httptest.ResponseRecorder) {
		a.ListPrivateKeys(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListPrivateKeysParams{Limit: one})
	})
	fullPage(t, "ListDNSCredentialsPage", func(a *API, rec *httptest.ResponseRecorder) {
		a.ListDnsCredentials(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListDnsCredentialsParams{Limit: one})
	})
	fullPage(t, "ListRegistryCredentialsPage", func(a *API, rec *httptest.ResponseRecorder) {
		a.ListRegistryCredentials(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListRegistryCredentialsParams{Limit: one})
	})
	fullPage(t, "ListSharedVariablesPage", func(a *API, rec *httptest.ResponseRecorder) {
		a.ListSharedVariables(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListSharedVariablesParams{Limit: one})
	})
	fullPage(t, "ListEnvVarsPage", func(a *API, rec *httptest.ResponseRecorder) {
		a.ListApplicationEnvs(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListApplicationEnvsParams{Limit: one})
	})
}

func TestOpscovMalformedPathUUIDs(t *testing.T) {
	a, _ := opscovAPI(t)

	rec := httptest.NewRecorder()
	a.GetPrivateKey(rec, opscovRequest(http.MethodGet, "/x", ""), "not-a-uuid")
	if rec.Code != http.StatusNotFound {
		t.Errorf("private key uuid = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	a.GetRegistryCredential(rec, opscovRequest(http.MethodGet, "/x", ""), "not-a-uuid")
	if rec.Code != http.StatusNotFound {
		t.Errorf("registry credential uuid = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	a.UpdateApplicationEnv(rec, opscovRequest(http.MethodPatch, "/x", `{}`), fixtureUUID, "not-a-uuid")
	if rec.Code != http.StatusNotFound {
		t.Errorf("env uuid = %d, want 404", rec.Code)
	}
}

func TestOpscovSharedVariableParentUUIDGrammar(t *testing.T) {
	a, _ := opscovAPI(t)

	rec := httptest.NewRecorder()
	a.CreateSharedVariable(rec, opscovRequest(http.MethodPost, "/x",
		`{"scope":"environment","key":"KEY","value":"x","environment_uuid":"nope"}`), api.CreateSharedVariableParams{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad environment uuid = %d, want 422", rec.Code)
	}

	rec = httptest.NewRecorder()
	a.CreateSharedVariable(rec, opscovRequest(http.MethodPost, "/x",
		`{"scope":"server","key":"KEY","value":"x","server_uuid":"nope"}`), api.CreateSharedVariableParams{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad server uuid = %d, want 422", rec.Code)
	}
}

func TestOpscovCreateNotificationRuleUnresolvedScopes(t *testing.T) {
	body := `{"event_type":"deploy.failed.v1","project_uuid":"` + fixtureUUID + `","environment_uuid":"` + fixtureUUID + `"}`

	a, db := opscovAPI(t)
	db.on("GetProjectByUUID").noRows = true
	rec := httptest.NewRecorder()
	a.CreateNotificationRule(rec, opscovRequest(http.MethodPost, "/x", body), fixtureUUID)
	if rec.Code != http.StatusNotFound {
		t.Errorf("vanished project = %d, want 404", rec.Code)
	}

	a, db = opscovAPI(t)
	db.on("GetEnvironmentByUUID").noRows = true
	rec = httptest.NewRecorder()
	a.CreateNotificationRule(rec, opscovRequest(http.MethodPost, "/x", body), fixtureUUID)
	if rec.Code != http.StatusNotFound {
		t.Errorf("vanished environment = %d, want 404", rec.Code)
	}
}

func TestOpscovListPrivateKeysParamErrors(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListPrivateKeys(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListPrivateKeysParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListPrivateKeys(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListPrivateKeysParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	db.on("ListPrivateKeysPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListPrivateKeys(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListPrivateKeysParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}
