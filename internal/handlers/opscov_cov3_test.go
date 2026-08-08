package handlers

// Coverage tests for projects.go, environments.go, sharedvars.go, adoption.go
// and registrycredentials.go, on top of the opscov scaffolding.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/store"
)

// --- projects.go ------------------------------------------------------------

func TestOpscovCreateProject(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*API, *opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(a, db)
		}
		rec := httptest.NewRecorder()
		a.CreateProject(rec, opscovRequest(http.MethodPost, "/x", body), api.CreateProjectParams{})
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"name":""}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty name = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"!!!"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unsluggable name = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"Shop"}`, func(a *API, db *opscovDB) {
		a.Pool = opscovPool{db: db, beginErr: opscovBoom()}
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("begin failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `{"name":"Shop"}`, func(_ *API, db *opscovDB) {
		db.on("CreateProject").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate slug = %d, want 409", rec.Code)
	}
	if rec := run(t, `{"name":"Shop"}`, func(_ *API, db *opscovDB) {
		db.on("CreateProject").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("create failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `{"name":"Shop"}`, func(_ *API, db *opscovDB) {
		db.on("CreateEnvironment").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("default environment failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `{"name":"Shop"}`, func(a *API, db *opscovDB) {
		a.Pool = opscovPool{db: db, commitErr: opscovBoom()}
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("commit failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `{"name":"Shop","description":"ops"}`, nil); rec.Code != http.StatusCreated {
		t.Errorf("happy path = %d, want 201: %s", rec.Code, rec.Body)
	}
}

func TestOpscovGetProjectListFailure(t *testing.T) {
	a, db := opscovAPI(t)
	db.on("ListEnvironmentsSummary").err = opscovBoom()
	rec := httptest.NewRecorder()
	a.GetProject(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("summary failure = %d, want 500", rec.Code)
	}
}

func TestOpscovUpdateProject(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		req := opscovRequest(http.MethodPatch, "/x", body)
		req.Header.Set("If-Match", `"1"`)
		a.UpdateProject(rec, req, fixtureUUID)
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"name":""}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty name = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"Shop","description":null}`, nil); rec.Code != http.StatusOK {
		t.Errorf("happy path = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"name":"Shop"}`, func(db *opscovDB) {
		db.on("UpdateProject").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("update failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `{"name":"Shop"}`, func(db *opscovDB) {
		db.on("UpdateProject").tag = "UPDATE 0"
	}); rec.Code != http.StatusConflict {
		t.Errorf("version conflict = %d, want 409", rec.Code)
	}
	if rec := run(t, `{"name":"Shop"}`, func(db *opscovDB) {
		s := db.on("GetProjectByUUID")
		s.skip = 1
		s.err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("reload failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `{"name":"Shop"}`, func(db *opscovDB) {
		db.on("ListEnvironmentsSummary").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("summary failure = %d, want 500", rec.Code)
	}
}

func TestOpscovDeleteProject(t *testing.T) {
	run := func(t *testing.T, prep func(*API, *opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(a, db)
		}
		rec := httptest.NewRecorder()
		a.DeleteProject(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID)
		return rec
	}

	if rec := run(t, func(_ *API, db *opscovDB) {
		db.on("CountResourcesInProject").countOne = true
	}); rec.Code != http.StatusConflict {
		t.Errorf("resources present = %d, want 409", rec.Code)
	}
	if rec := run(t, func(_ *API, db *opscovDB) {
		db.on("CountResourcesInProject").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("count failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(a *API, db *opscovDB) {
		a.Pool = opscovPool{db: db, beginErr: opscovBoom()}
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("begin failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(_ *API, db *opscovDB) {
		db.on("SoftDeleteProjectEnvironments").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("environment tombstone failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(_ *API, db *opscovDB) {
		db.on("SoftDeleteProject").tag = "UPDATE 0"
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("zero-row delete = %d, want 500", rec.Code)
	}
	if rec := run(t, func(a *API, db *opscovDB) {
		a.Pool = opscovPool{db: db, commitErr: opscovBoom()}
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("commit failure = %d, want 500", rec.Code)
	}
	if rec := run(t, nil); rec.Code != http.StatusNoContent {
		t.Errorf("happy path = %d, want 204", rec.Code)
	}
}

func TestOpscovListProjectsErrors(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListProjects(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListProjectsParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListProjects(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListProjectsParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	db.on("ListProjectsPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListProjects(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListProjectsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}

// --- environments.go --------------------------------------------------------

func TestOpscovCreateEnvironment(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CreateEnvironment(rec, opscovRequest(http.MethodPost, "/x", body), fixtureUUID, api.CreateEnvironmentParams{})
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"name":""}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty name = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"staging"}`, func(db *opscovDB) {
		db.on("CreateEnvironment").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate slug = %d, want 409", rec.Code)
	}
	if rec := run(t, `{"name":"staging"}`, func(db *opscovDB) {
		db.on("CreateEnvironment").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `{"name":"staging"}`, nil); rec.Code != http.StatusCreated {
		t.Errorf("happy path = %d, want 201: %s", rec.Code, rec.Body)
	}
}

func TestOpscovUpdateEnvironment(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateEnvironment(rec, opscovRequest(http.MethodPatch, "/x", body), fixtureUUID, fixtureUUID)
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"name":"!!!"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unsluggable name = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"staging","description":null}`, nil); rec.Code != http.StatusOK {
		t.Errorf("happy path = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"name":"staging"}`, func(db *opscovDB) {
		db.on("UpdateEnvironment").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("update failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `{"name":"staging"}`, func(db *opscovDB) {
		db.on("UpdateEnvironment").tag = "UPDATE 0"
	}); rec.Code != http.StatusConflict {
		t.Errorf("version conflict = %d, want 409", rec.Code)
	}
	if rec := run(t, `{"name":"staging"}`, func(db *opscovDB) {
		s := db.on("GetEnvironmentByUUID")
		s.skip = 1
		s.err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("reload failure = %d, want 500", rec.Code)
	}
}

func TestOpscovDeleteEnvironment(t *testing.T) {
	run := func(t *testing.T, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.DeleteEnvironment(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID, fixtureUUID)
		return rec
	}

	if rec := run(t, func(db *opscovDB) {
		db.on("CountResourcesInEnvironment").countOne = true
	}); rec.Code != http.StatusConflict {
		t.Errorf("resources present = %d, want 409", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("CountResourcesInEnvironment").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("count failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("SoftDeleteEnvironment").tag = "UPDATE 0"
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("zero-row delete = %d, want 500", rec.Code)
	}
	if rec := run(t, nil); rec.Code != http.StatusNoContent {
		t.Errorf("happy path = %d, want 204", rec.Code)
	}
}

func TestOpscovResourceCountsFailureIsEmpty(t *testing.T) {
	a, db := opscovAPI(t)
	db.on("CountResourcesByEnvironment").err = opscovBoom()
	counts := a.resourceCounts(opscovRequest(http.MethodGet, "/x", ""), []int64{1, 2})
	if len(counts) != 0 {
		t.Errorf("counts on failure = %v, want empty", counts)
	}
	if len(a.resourceCounts(opscovRequest(http.MethodGet, "/x", ""), nil)) != 0 {
		t.Error("no environments must produce no counts")
	}
}

func TestOpscovListEnvironmentsErrors(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListEnvironments(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListEnvironmentsParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListEnvironments(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListEnvironmentsParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	db.on("ListEnvironmentsPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListEnvironments(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListEnvironmentsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}

	a, _ = opscovAPI(t)
	rec = httptest.NewRecorder()
	a.GetEnvironment(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, "not-a-uuid")
	if rec.Code != http.StatusNotFound {
		t.Errorf("malformed environment uuid = %d, want 404", rec.Code)
	}
}

// --- sharedvars.go ----------------------------------------------------------

func TestOpscovCreateSharedVariable(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CreateSharedVariable(rec, opscovRequest(http.MethodPost, "/x", body), api.CreateSharedVariableParams{})
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"scope":"team","key":"9bad","value":"x"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad key = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"scope":"galaxy","key":"KEY","value":"x"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown scope = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"scope":"project","key":"KEY","value":"x"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("project scope without parent = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"scope":"project","key":"KEY","value":"x","project_uuid":"nope"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad project uuid = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"scope":"project","key":"KEY","value":"x","project_uuid":"`+fixtureUUID+`"}`,
		func(db *opscovDB) { db.on("GetProjectByUUID").noRows = true }); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown project = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"scope":"project","key":"KEY","value":"x","project_uuid":"`+fixtureUUID+`"}`, nil); rec.Code != http.StatusCreated {
		t.Errorf("project scope = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"scope":"environment","key":"KEY","value":"x","environment_uuid":"`+fixtureUUID+`"}`,
		func(db *opscovDB) { db.on("GetEnvironmentByUUIDForTeam").noRows = true }); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown environment = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"scope":"environment","key":"KEY","value":"x","environment_uuid":"`+fixtureUUID+`"}`, nil); rec.Code != http.StatusCreated {
		t.Errorf("environment scope = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"scope":"server","key":"KEY","value":"x","server_uuid":"`+fixtureUUID+`"}`,
		func(db *opscovDB) { db.on("GetServerByUUID").noRows = true }); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown server = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"scope":"server","key":"KEY","value":"x","server_uuid":"`+fixtureUUID+`"}`, nil); rec.Code != http.StatusCreated {
		t.Errorf("server scope = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"scope":"team","key":"KEY","value":"x","is_secret":true}`, nil); rec.Code != http.StatusCreated {
		t.Errorf("team scope = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"scope":"team","key":"KEY","value":"x"}`, func(db *opscovDB) {
		db.on("CreateSharedVariable").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate key = %d, want 409", rec.Code)
	}
	if rec := run(t, `{"scope":"team","key":"KEY","value":"x"}`, func(db *opscovDB) {
		db.on("CreateSharedVariable").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
}

func TestOpscovSharedVariableValueReveal(t *testing.T) {
	a, db := opscovAPI(t)
	blob := opscovEncrypt(t, a, "shared_variables", "value_enc", []byte("shared-plain"))
	db.on("ListSharedVariablesPage").fill = opscovBytesFill(blob)
	rec := httptest.NewRecorder()
	scope := api.ListSharedVariablesParamsScope("team")
	a.ListSharedVariables(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListSharedVariablesParams{Scope: &scope})
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "shared-plain") {
		t.Errorf("list body misses the decrypted value: %s", rec.Body)
	}
}

func TestOpscovListSharedVariablesErrors(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListSharedVariables(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListSharedVariablesParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListSharedVariables(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListSharedVariablesParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	db.on("ListSharedVariablesPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListSharedVariables(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListSharedVariablesParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}

func TestOpscovUpdateSharedVariable(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateSharedVariable(rec, opscovRequest(http.MethodPatch, "/x", body), fixtureUUID)
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"value":"new","is_secret":true}`, nil); rec.Code != http.StatusOK {
		t.Errorf("happy path = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"value":"new"}`, func(db *opscovDB) {
		db.on("UpdateSharedVariable").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("update failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `{"value":"new"}`, func(db *opscovDB) {
		s := db.on("GetSharedVariableByUUID")
		s.skip = 1
		s.err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("reload failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `{"value":"new"}`, func(db *opscovDB) {
		db.on("GetSharedVariableByUUID").noRows = true
	}); rec.Code != http.StatusNotFound {
		t.Errorf("missing variable = %d, want 404", rec.Code)
	}

	a, _ := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.UpdateSharedVariable(rec, opscovRequest(http.MethodPatch, "/x", "{}"), "not-a-uuid")
	if rec.Code != http.StatusNotFound {
		t.Errorf("malformed uuid = %d, want 404", rec.Code)
	}
}

func TestOpscovDeleteSharedVariable(t *testing.T) {
	a, db := opscovAPI(t)
	db.on("DeleteSharedVariable").err = opscovBoom()
	rec := httptest.NewRecorder()
	a.DeleteSharedVariable(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("delete failure = %d, want 500", rec.Code)
	}

	a, _ = opscovAPI(t)
	rec = httptest.NewRecorder()
	a.DeleteSharedVariable(rec, opscovRequest(http.MethodDelete, "/x", ""), "not-a-uuid")
	if rec.Code != http.StatusNotFound {
		t.Errorf("malformed uuid = %d, want 404", rec.Code)
	}
}

// --- adoption.go ------------------------------------------------------------

func TestOpscovCreateAdoptionScan(t *testing.T) {
	run := func(t *testing.T, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CreateAdoptionScan(rec, opscovRequest(http.MethodPost, "/x", "{}"), fixtureUUID, api.CreateAdoptionScanParams{})
		return rec
	}

	if rec := run(t, func(db *opscovDB) {
		db.on("CountActiveJobsByLockKey").countOne = true
	}); rec.Code != http.StatusConflict {
		t.Errorf("scan in progress = %d, want 409", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("CountActiveJobsByLockKey").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("count failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("CreateAdoptionScan").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("scan insert failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("EnqueueJob").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("enqueue failure = %d, want 500", rec.Code)
	}
	if rec := run(t, nil); rec.Code != http.StatusAccepted {
		t.Errorf("happy path = %d, want 202: %s", rec.Code, rec.Body)
	}
}

func TestOpscovListAdoptionScans(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListAdoptionScans(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListAdoptionScansParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListAdoptionScans(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListAdoptionScansParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	// A real cursor: the page starts strictly before the decoded id.
	rec = httptest.NewRecorder()
	a.ListAdoptionScans(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListAdoptionScansParams{Cursor: ptr(encodeCursor(5))})
	if rec.Code != http.StatusOK {
		t.Errorf("paged list = %d, want 200: %s", rec.Code, rec.Body)
	}
	db.on("ListAdoptionScansForServer").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListAdoptionScans(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListAdoptionScansParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}

func TestOpscovGetAdoptionScanCandidates(t *testing.T) {
	a, db := opscovAPI(t)
	db.on("GetAdoptionScanByUUIDForTeam").fill = opscovBytesFill([]byte(`[{"id":"c1","adoptable":true,"containers":[]}]`))
	rec := httptest.NewRecorder()
	a.GetAdoptionScan(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"c1"`) {
		t.Errorf("candidates missing from body: %s", rec.Body)
	}
}

func TestOpscovAdoptResources(t *testing.T) {
	candidates := opscovBytesFill([]byte(`[{"id":"c1","adoptable":true,"containers":[]},` +
		`{"id":"c2","adoptable":false,"reasons":["needs a restart"],"containers":[]}]`))
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.AdoptResources(rec, opscovRequest(http.MethodPost, "/x", body), fixtureUUID, api.AdoptResourcesParams{})
		return rec
	}
	adopt := `{"environment_uuid":"` + fixtureUUID + `","items":[{"candidate_id":"c1","name":"web"}]}`

	if rec := run(t, adopt, func(db *opscovDB) {
		db.on("GetAdoptionScanByUUIDForTeam").noRows = true
	}); rec.Code != http.StatusNotFound {
		t.Errorf("missing scan = %d, want 404", rec.Code)
	}
	if rec := run(t, adopt, func(db *opscovDB) {
		db.on("GetAdoptionScanByUUIDForTeam").fill = func(_ int, dest any) bool {
			if p, ok := dest.(*store.AdoptionScanStatus); ok {
				*p = store.AdoptionScanStatusRunning
				return true
			}
			return false
		}
	}); rec.Code != http.StatusConflict {
		t.Errorf("running scan = %d, want 409", rec.Code)
	}
	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"environment_uuid":"`+fixtureUUID+`","items":[]}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("no items = %d, want 422", rec.Code)
	}
	if rec := run(t, adopt, func(db *opscovDB) {
		db.on("GetEnvironmentByUUIDForTeam").noRows = true
	}); rec.Code != http.StatusNotFound {
		t.Errorf("missing environment = %d, want 404", rec.Code)
	}
	// The default fixture blob "{}" is not a candidate array.
	if rec := run(t, adopt, nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("corrupt candidates = %d, want 500", rec.Code)
	}
	if rec := run(t, `{"environment_uuid":"`+fixtureUUID+`","items":[{"candidate_id":"ghost"}]}`,
		func(db *opscovDB) { db.on("GetAdoptionScanByUUIDForTeam").fill = candidates }); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown candidate = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"environment_uuid":"`+fixtureUUID+`","items":[{"candidate_id":"c2"}]}`,
		func(db *opscovDB) { db.on("GetAdoptionScanByUUIDForTeam").fill = candidates }); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("non-adoptable candidate = %d, want 422", rec.Code)
	}
	if rec := run(t, adopt, func(db *opscovDB) {
		db.on("GetAdoptionScanByUUIDForTeam").fill = candidates
		db.on("EnqueueJob").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("enqueue failure = %d, want 500", rec.Code)
	}
	if rec := run(t, adopt, func(db *opscovDB) {
		db.on("GetAdoptionScanByUUIDForTeam").fill = candidates
	}); rec.Code != http.StatusAccepted {
		t.Errorf("happy path = %d, want 202: %s", rec.Code, rec.Body)
	}
}

func TestOpscovDisownConflicts(t *testing.T) {
	run := func(t *testing.T, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.DisownApplication(rec, opscovRequest(http.MethodPost, "/x", "{}"), fixtureUUID, api.DisownApplicationParams{})
		return rec
	}

	if rec := run(t, func(db *opscovDB) {
		db.on("CountActiveJobsByLockKey").countOne = true
	}); rec.Code != http.StatusConflict {
		t.Errorf("operation in progress = %d, want 409", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("CountActiveJobsByLockKey").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("count failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("EnqueueJob").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("enqueue failure = %d, want 500", rec.Code)
	}
}

// --- registrycredentials.go -------------------------------------------------

func TestOpscovCreateRegistryCredential(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CreateRegistryCredential(rec, opscovRequest(http.MethodPost, "/x", body), api.CreateRegistryCredentialParams{})
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	bad := `{"name":" ","username":" ","password":"","registry_url":"not a host;"}`
	if rec := run(t, bad, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("all-invalid body = %d, want 422", rec.Code)
	}
	good := `{"name":"ghcr","username":"robot","password":"s3cret","registry_url":"ghcr.io"}`
	if rec := run(t, good, nil); rec.Code != http.StatusCreated {
		t.Errorf("happy path = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, good, func(db *opscovDB) {
		db.on("CreateRegistryCredential").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", rec.Code)
	}
	if rec := run(t, good, func(db *opscovDB) {
		db.on("CreateRegistryCredential").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
}

func TestOpscovUpdateRegistryCredential(t *testing.T) {
	run := func(t *testing.T, ifMatch, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateRegistryCredential(rec, opscovRequest(http.MethodPatch, "/x", body), fixtureUUID,
			api.UpdateRegistryCredentialParams{IfMatch: ifMatch})
		return rec
	}

	if rec := run(t, "zz", `{}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad If-Match = %d, want 400", rec.Code)
	}
	if rec := run(t, `"1"`, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", rec.Code)
	}
	if rec := run(t, `"1"`, `{"registry_url":"not a host;"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad registry url = %d, want 422", rec.Code)
	}
	if rec := run(t, `"1"`, `{"password":""}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty password = %d, want 422", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"ghcr","registry_url":"ghcr.io","username":"robot","password":"n3w"}`, nil); rec.Code != http.StatusOK {
		t.Errorf("rotation = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `"1"`, `{"name":"ghcr"}`, func(db *opscovDB) {
		db.on("UpdateRegistryCredential").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"ghcr"}`, func(db *opscovDB) {
		db.on("UpdateRegistryCredential").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("update failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"ghcr"}`, func(db *opscovDB) {
		db.on("UpdateRegistryCredential").tag = "UPDATE 0"
	}); rec.Code != http.StatusConflict {
		t.Errorf("version conflict = %d, want 409", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"ghcr"}`, func(db *opscovDB) {
		db.on("GetRegistryCredentialByID").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("reload failure = %d, want 500", rec.Code)
	}
}

func TestOpscovDeleteRegistryCredential(t *testing.T) {
	run := func(t *testing.T, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.DeleteRegistryCredential(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID)
		return rec
	}

	if rec := run(t, func(db *opscovDB) {
		db.on("CountRegistryCredentialUsage").countOne = true
	}); rec.Code != http.StatusConflict {
		t.Errorf("credential in use = %d, want 409", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("SoftDeleteRegistryCredential").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("delete failure = %d, want 500", rec.Code)
	}
	if rec := run(t, nil); rec.Code != http.StatusNoContent {
		t.Errorf("happy path = %d, want 204", rec.Code)
	}
}

func TestOpscovListRegistryCredentialsErrors(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListRegistryCredentials(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListRegistryCredentialsParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListRegistryCredentials(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListRegistryCredentialsParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	db.on("ListRegistryCredentialsPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListRegistryCredentials(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListRegistryCredentialsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}
