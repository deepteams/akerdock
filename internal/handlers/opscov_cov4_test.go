package handlers

// Coverage tests for jobs.go, teams.go, audit.go, systeminstance.go and
// dnscredentials.go, on top of the opscov scaffolding.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// --- jobs.go ----------------------------------------------------------------

func TestOpscovListJobs(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListJobs(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListJobsParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListJobs(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListJobsParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	status := api.JobStatus("dead_letter")
	rec = httptest.NewRecorder()
	a.ListJobs(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListJobsParams{
		Status: &status, Queue: ptr("default"), Type: ptr("deploy"),
	})
	if rec.Code != http.StatusOK {
		t.Errorf("filtered list = %d, want 200: %s", rec.Code, rec.Body)
	}
	db.on("ListJobsPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListJobs(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListJobsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}

func opscovJobStatusFill(status store.JobStatus) func(int, any) bool {
	return func(_ int, dest any) bool {
		if p, ok := dest.(*store.JobStatus); ok {
			*p = status
			return true
		}
		return false
	}
}

func TestOpscovRetryJob(t *testing.T) {
	run := func(t *testing.T, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.RetryJob(rec, opscovRequest(http.MethodPost, "/x", "{}"), fixtureUUID, api.RetryJobParams{})
		return rec
	}

	if rec := run(t, func(db *opscovDB) {
		db.on("GetJobByUUIDForTeam").fill = opscovJobStatusFill(store.JobStatusQueued)
	}); rec.Code != http.StatusConflict {
		t.Errorf("non-dead-letter retry = %d, want 409", rec.Code)
	}
	// The fixture queue name "unit" is refused by the enqueue guard: the retry
	// must carry a queue a worker actually consumes.
	knownQueue := func(i int, dest any) bool {
		if i == 2 {
			if p, ok := dest.(*string); ok {
				*p = "default"
				return true
			}
		}
		return false
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("GetJobByUUIDForTeam").fill = knownQueue
		db.on("EnqueueJob").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("enqueue failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("GetJobByUUIDForTeam").fill = knownQueue
	}); rec.Code != http.StatusAccepted {
		t.Errorf("happy path = %d, want 202: %s", rec.Code, rec.Body)
	}
	// An unconsumable stored queue surfaces as an internal error.
	if rec := run(t, nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("unknown queue = %d, want 500", rec.Code)
	}
}

func TestOpscovForgetJob(t *testing.T) {
	noRemnants := func(_ int, dest any) bool {
		if b, ok := dest.(*[]byte); ok {
			*b = []byte("null")
			return true
		}
		return false
	}
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.ForgetJob(rec, opscovRequest(http.MethodPost, "/x", body), fixtureUUID)
		return rec
	}

	if rec := run(t, "", func(db *opscovDB) {
		db.on("GetJobByUUIDForTeam").fill = opscovJobStatusFill(store.JobStatusQueued)
	}); rec.Code != http.StatusConflict {
		t.Errorf("non-dead-letter forget = %d, want 409", rec.Code)
	}
	// The fixture resource records remnants ("{}" is not "null"): forgetting
	// without acknowledgement is refused.
	if rec := run(t, "", nil); rec.Code != http.StatusConflict {
		t.Errorf("unacknowledged remnants = %d, want 409", rec.Code)
	}
	if rec := run(t, `{"acknowledge_remnants":true}`, nil); rec.Code != http.StatusOK {
		t.Errorf("acknowledged remnants = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, "", func(db *opscovDB) {
		db.on("GetResourceRemnants").fill = noRemnants
	}); rec.Code != http.StatusOK {
		t.Errorf("no remnants = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, "", func(db *opscovDB) {
		db.on("GetResourceRemnants").fill = noRemnants
		db.on("ForgetDeadLetterJob").tag = "UPDATE 0"
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("zero-row forget = %d, want 500", rec.Code)
	}
	if rec := run(t, "", func(db *opscovDB) {
		db.on("GetResourceRemnants").fill = noRemnants
		s := db.on("GetJobByUUIDForTeam")
		s.skip = 1
		s.err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("reload failure = %d, want 500", rec.Code)
	}
}

func TestOpscovRemnantsOf(t *testing.T) {
	a, db := opscovAPI(t)
	r := opscovRequest(http.MethodGet, "/x", "")
	if got := a.remnantsOf(r, store.Job{}); got != nil {
		t.Errorf("job without resource -> %s, want nil", got)
	}
	db.on("GetResourceRemnants").err = opscovBoom()
	if got := a.remnantsOf(r, store.Job{ResourceID: ptr(int64(1))}); got != nil {
		t.Errorf("lookup failure -> %s, want nil", got)
	}
}

func TestOpscovRemnantDetails(t *testing.T) {
	if got := remnantDetails([]byte("{invalid")); len(got) != 1 {
		t.Errorf("invalid inventory -> %d details, want 1", len(got))
	}
	got := remnantDetails([]byte(`{"containers":["c1"],"volumes":["v1"],"files":["/srv/f"],"error":"partial"}`))
	if len(got) != 4 {
		t.Errorf("full inventory -> %d details, want 4", len(got))
	}
	if got := remnantDetails([]byte(`{}`)); len(got) != 1 || *got[0].Code != "remnant" {
		t.Errorf("empty inventory -> %+v, want the fallback detail", got)
	}
}

// --- teams.go ---------------------------------------------------------------

func TestOpscovListTeams(t *testing.T) {
	// A root token lists every team; a plain reader gets its own.
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListTeams(rec, opscovRequestAs(opscovIdentity(auth.PermTeamRead), http.MethodGet, "/x", ""), api.ListTeamsParams{})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), fixtureUUID) {
		t.Errorf("member list = %d %s, want the caller's team", rec.Code, rec.Body)
	}
	// A non-root caller beyond the first page gets nothing more.
	rec = httptest.NewRecorder()
	a.ListTeams(rec, opscovRequestAs(opscovIdentity(auth.PermTeamRead), http.MethodGet, "/x", ""),
		api.ListTeamsParams{Cursor: ptr(encodeCursor(9))})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Errorf("member second page = %d %s, want empty data", rec.Code, rec.Body)
	}
	db.on("ListTeamsPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListTeams(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListTeamsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("root list failure = %d, want 500", rec.Code)
	}
}

func TestOpscovCreateTeam(t *testing.T) {
	run := func(t *testing.T, body string, withSession bool, prep func(*API, *opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if withSession {
			a.Sessions = &session.Manager{Store: a.Store}
		}
		if prep != nil {
			prep(a, db)
		}
		rec := httptest.NewRecorder()
		req := opscovRequest(http.MethodPost, "/x", body)
		if withSession {
			req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "opscov-session"})
		}
		a.CreateTeam(rec, req)
		return rec
	}

	if rec := run(t, "{not json", false, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"name":""}`, false, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty name = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"Platform"}`, false, func(_ *API, db *opscovDB) {
		db.on("CreateTeam").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("create failure = %d, want 500", rec.Code)
	}
	// No session manager: the team is created without a first member.
	if rec := run(t, `{"name":"Platform"}`, false, nil); rec.Code != http.StatusCreated {
		t.Errorf("token creation = %d, want 201: %s", rec.Code, rec.Body)
	}
	// A session cookie resolves and the creator joins as admin.
	if rec := run(t, `{"name":"Platform"}`, true, nil); rec.Code != http.StatusCreated {
		t.Errorf("session creation = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"name":"Platform"}`, true, func(_ *API, db *opscovDB) {
		db.on("AddTeamMember").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("membership failure = %d, want 500", rec.Code)
	}
}

func TestOpscovGetTeamIsolation(t *testing.T) {
	a, _ := opscovAPI(t)
	// A non-root caller of ANOTHER team must see the uniform 404 (INV-002).
	stranger := opscovIdentity(auth.PermTeamRead)
	stranger.TeamID = 2
	rec := httptest.NewRecorder()
	a.GetTeam(rec, opscovRequestAs(stranger, http.MethodGet, "/x", ""), fixtureUUID)
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign team = %d, want 404", rec.Code)
	}
}

func TestOpscovUpdateTeam(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateTeam(rec, opscovRequest(http.MethodPatch, "/x", body), fixtureUUID)
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"name":"!!!"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unsluggable name = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"Platform","description":null}`, nil); rec.Code != http.StatusOK {
		t.Errorf("happy path = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"name":"Platform"}`, func(db *opscovDB) {
		db.on("UpdateTeam").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("update failure = %d, want 500", rec.Code)
	}
}

func TestOpscovListTeamMembersErrors(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListTeamMembers(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListTeamMembersParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListTeamMembers(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListTeamMembersParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	db.on("ListTeamMembersPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListTeamMembers(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListTeamMembersParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}

// --- audit.go ---------------------------------------------------------------

func TestOpscovListInstanceAudit(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	result := api.ListInstanceAuditParamsResult("success")

	a, db := opscovAPI(t)
	outsider := opscovIdentity()
	outsider.InstanceRoot = false
	rec := httptest.NewRecorder()
	a.ListInstanceAudit(rec, opscovRequestAs(outsider, http.MethodGet, "/x", ""), api.ListInstanceAuditParams{})
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-root caller = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	a.ListInstanceAudit(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListInstanceAuditParams{
		Action: ptr("auth.login"), Result: &result, ActorUuid: ptr(fixtureUUID), From: &from, To: &to,
	})
	if rec.Code != http.StatusOK {
		t.Errorf("filtered list = %d, want 200: %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	a.ListInstanceAudit(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListInstanceAuditParams{ActorUuid: ptr("nope")})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad actor uuid = %d, want 422", rec.Code)
	}

	rec = httptest.NewRecorder()
	a.ListInstanceAudit(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListInstanceAuditParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListInstanceAudit(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListInstanceAuditParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}

	db.on("ListInstanceAuditEventsPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListInstanceAudit(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListInstanceAuditParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}

func TestOpscovListTeamAudit(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	result := api.ListTeamAuditParamsResult("success")

	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListTeamAudit(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListTeamAuditParams{
		Action: ptr("secret.reveal"), ActionPrefix: &[]string{" port-forward. ", ""},
		Result: &result, ActorUuid: ptr(fixtureUUID), TargetUuid: ptr(fixtureUUID),
		From: &from, To: &to,
	})
	if rec.Code != http.StatusOK {
		t.Errorf("filtered list = %d, want 200: %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	a.ListTeamAudit(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListTeamAuditParams{ActorUuid: ptr("nope")})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad actor uuid = %d, want 422", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListTeamAudit(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListTeamAuditParams{TargetUuid: ptr("nope")})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad target uuid = %d, want 422", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListTeamAudit(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListTeamAuditParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListTeamAudit(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListTeamAuditParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}

	db.on("ListAuditEventsPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListTeamAudit(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListTeamAuditParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}

// --- systeminstance.go ------------------------------------------------------

func TestOpscovSetInstanceSettings(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*API, *opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(a, db)
		}
		rec := httptest.NewRecorder()
		a.SetInstanceSettings(rec, opscovRequest(http.MethodPut, "/x", body))
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"fqdn":"https://deploy.example.com"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("fqdn with scheme = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"fqdn":"-bad-.example"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("malformed fqdn = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"acme_email":"not-an-email"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad acme email = %d, want 422", rec.Code)
	}
	if rec := run(t, `{}`, func(_ *API, db *opscovDB) {
		db.on("SetInstanceIdentity").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("identity write failure = %d, want 500", rec.Code)
	}
	for _, tc := range []struct{ field, query string }{
		{`"registration_enabled":true`, "SetRegistrationEnabled"},
		{`"mfa_required":true`, "SetMfaRequired"},
		{`"mcp_enabled":true`, "SetInstanceMcpEnabled"},
		{`"mcp_dcr_enabled":true`, "SetInstanceMcpDcrEnabled"},
		{`"password_login_disabled":false`, "SetPasswordLoginDisabled"},
		{`"image_retention_count":3`, "SetImageRetentionCount"},
	} {
		if rec := run(t, `{`+tc.field+`}`, func(_ *API, db *opscovDB) {
			db.on(tc.query).err = opscovBoom()
		}); rec.Code != http.StatusInternalServerError {
			t.Errorf("%s failure = %d, want 500", tc.query, rec.Code)
		}
	}
	// SSO-only without any OIDC provider is refused; with one it goes through.
	if rec := run(t, `{"password_login_disabled":true}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("SSO-only without provider = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"password_login_disabled":true}`, func(a *API, _ *opscovDB) {
		a.OAuth = &session.OAuth{Store: a.Store}
	}); rec.Code != http.StatusOK {
		t.Errorf("SSO-only with provider = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"image_retention_count":0}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("retention 0 = %d, want 422", rec.Code)
	}
	full := `{"fqdn":" Deploy.Example.Com ","acme_email":"ops@example.com","registration_enabled":true,` +
		`"mfa_required":false,"mcp_enabled":true,"mcp_dcr_enabled":false,"password_login_disabled":false,` +
		`"image_retention_count":5}`
	if rec := run(t, full, nil); rec.Code != http.StatusOK {
		t.Errorf("full update = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{}`, func(_ *API, db *opscovDB) {
		db.on("GetInstanceSettings").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("final read failure = %d, want 500", rec.Code)
	}
}

func TestOpscovGetInstanceSettingsFailure(t *testing.T) {
	a, db := opscovAPI(t)
	db.on("GetInstanceSettings").err = opscovBoom()
	rec := httptest.NewRecorder()
	a.GetInstanceSettings(rec, opscovRequest(http.MethodGet, "/x", ""))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("settings read failure = %d, want 500", rec.Code)
	}

	outsider := opscovIdentity()
	outsider.InstanceRoot = false
	rec = httptest.NewRecorder()
	a.GetInstanceSettings(rec, opscovRequestAs(outsider, http.MethodGet, "/x", ""))
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-root caller = %d, want 403", rec.Code)
	}
}

func TestOpscovHasEnabledOAuthProvider(t *testing.T) {
	a, db := opscovAPI(t)
	r := opscovRequest(http.MethodGet, "/x", "")
	if a.hasEnabledOAuthProvider(r) {
		t.Error("nil OAuth engine must report no provider")
	}
	a.OAuth = &session.OAuth{Store: a.Store}
	if !a.hasEnabledOAuthProvider(r) {
		t.Error("an enabled github row must count as a provider")
	}
	db.on("ListEnabledOauthProviderConfigs").err = opscovBoom()
	if a.hasEnabledOAuthProvider(r) {
		t.Error("a store failure must read as no provider")
	}
}

// --- dnscredentials.go ------------------------------------------------------

func TestOpscovCreateDnsCredential(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CreateDnsCredential(rec, opscovRequest(http.MethodPost, "/x", body), api.CreateDnsCredentialParams{})
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"name":" ","provider":"Not-A-Provider!","config":{}}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("all-invalid body = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"cf","provider":"cloudflare","config":{"lower_case":"x"}}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad variable name = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"cf","provider":"cloudflare","config":{"CF_API_TOKEN":"bad\nvalue"}}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("newline in value = %d, want 422", rec.Code)
	}
	good := `{"name":"cf","provider":"cloudflare","config":{"CF_API_TOKEN":"tok"}}`
	if rec := run(t, good, nil); rec.Code != http.StatusCreated {
		t.Errorf("happy path = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, good, func(db *opscovDB) {
		db.on("CreateDNSCredential").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", rec.Code)
	}
	if rec := run(t, good, func(db *opscovDB) {
		db.on("CreateDNSCredential").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
}

func TestOpscovDeleteDnsCredential(t *testing.T) {
	run := func(t *testing.T, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.DeleteDnsCredential(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID)
		return rec
	}

	if rec := run(t, func(db *opscovDB) {
		db.on("CountDNSCredentialUsage").countOne = true
	}); rec.Code != http.StatusConflict {
		t.Errorf("credential in use = %d, want 409", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("SoftDeleteDNSCredential").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("delete failure = %d, want 500", rec.Code)
	}
	if rec := run(t, nil); rec.Code != http.StatusNoContent {
		t.Errorf("happy path = %d, want 204", rec.Code)
	}
}

func TestOpscovDnsCredentialReads(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.GetDnsCredential(rec, opscovRequest(http.MethodGet, "/x", ""), "not-a-uuid")
	if rec.Code != http.StatusNotFound {
		t.Errorf("malformed uuid = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	a.ListDnsCredentials(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListDnsCredentialsParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListDnsCredentials(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListDnsCredentialsParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	db.on("ListDNSCredentialsPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListDnsCredentials(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListDnsCredentialsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}
