package handlers

// Coverage tests for scheduledtasks.go, uptime.go, privatekeys.go and envs.go,
// on top of the opscov scaffolding in opscov_cov_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/sshkey"
	"github.com/deepteams/akerdock/internal/store"
)

// --- scheduledtasks.go ------------------------------------------------------

func TestOpscovCreateScheduledTask(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CreateScheduledTask(rec, opscovRequest(http.MethodPost, "/x", body), fixtureUUID, api.CreateScheduledTaskParams{})
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	// Every validation lights up at once: blank name/command, bad cron, bad
	// timezone, timeout below 1.
	bad := `{"name":" ","command":" ","cron_expression":"whenever","timezone":"Mars/Olympus","timeout_seconds":0}`
	if rec := run(t, bad, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("all-invalid body = %d, want 422", rec.Code)
	}
	good := `{"name":"purge","command":"purge --all","cron_expression":"daily","timezone":"Europe/Paris",` +
		`"timeout_seconds":60,"enabled":false,"overlap_policy":"queue","missed_run_policy":"skip"}`
	if rec := run(t, good, nil); rec.Code != http.StatusCreated {
		t.Errorf("happy path = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, good, func(db *opscovDB) {
		db.on("CreateScheduledTask").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", rec.Code)
	}
	if rec := run(t, good, func(db *opscovDB) {
		db.on("CreateScheduledTask").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
}

func TestOpscovUpdateScheduledTask(t *testing.T) {
	run := func(t *testing.T, ifMatch, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateScheduledTask(rec, opscovRequest(http.MethodPatch, "/x", body), fixtureUUID,
			api.UpdateScheduledTaskParams{IfMatch: ifMatch})
		return rec
	}

	if rec := run(t, "zz", `{}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad If-Match = %d, want 400", rec.Code)
	}
	if rec := run(t, `"1"`, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", rec.Code)
	}
	if rec := run(t, `"1"`, `{"cron_expression":"whenever"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad cron = %d, want 422", rec.Code)
	}
	if rec := run(t, `"1"`, `{"timezone":"Mars/Olympus"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad timezone = %d, want 422", rec.Code)
	}
	if rec := run(t, `"1"`, `{"timeout_seconds":0}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("timeout 0 = %d, want 422", rec.Code)
	}
	full := `{"name":"purge","command":"purge","container":null,"cron_expression":"hourly","timezone":"UTC",` +
		`"timeout_seconds":30,"enabled":true,"overlap_policy":"queue","missed_run_policy":"run"}`
	if rec := run(t, `"1"`, full, nil); rec.Code != http.StatusOK {
		t.Errorf("happy path = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `"1"`, `{"name":"x"}`, func(db *opscovDB) {
		db.on("UpdateScheduledTask").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"x"}`, func(db *opscovDB) {
		db.on("UpdateScheduledTask").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("update failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"x"}`, func(db *opscovDB) {
		db.on("UpdateScheduledTask").tag = "UPDATE 0"
	}); rec.Code != http.StatusConflict {
		t.Errorf("version conflict = %d, want 409", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"x"}`, func(db *opscovDB) {
		s := db.on("GetScheduledTaskByUUID")
		s.skip = 1
		s.err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("reload failure = %d, want 500", rec.Code)
	}
}

func TestOpscovRunScheduledTask(t *testing.T) {
	overlapSkip := func(_ int, dest any) bool {
		if p, ok := dest.(*store.TaskOverlapPolicy); ok {
			*p = store.TaskOverlapPolicySkip
			return true
		}
		return false
	}
	run := func(t *testing.T, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.RunScheduledTask(rec, opscovRequest(http.MethodPost, "/x", "{}"), fixtureUUID, api.RunScheduledTaskParams{})
		return rec
	}

	if rec := run(t, func(db *opscovDB) {
		db.on("GetScheduledTaskByUUID").fill = overlapSkip
		db.on("CountRunningTaskExecutions").countOne = true
	}); rec.Code != http.StatusConflict {
		t.Errorf("running + skip policy = %d, want 409", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("GetScheduledTaskByUUID").fill = overlapSkip
		db.on("CountRunningTaskExecutions").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("count failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("GetScheduledTaskByUUID").fill = overlapSkip
	}); rec.Code != http.StatusAccepted {
		t.Errorf("idle + skip policy = %d, want 202: %s", rec.Code, rec.Body)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("CreateTaskExecution").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("execution insert failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("EnqueueJob").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("enqueue failure = %d, want 500", rec.Code)
	}
}

func TestOpscovScheduledTaskLists(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListScheduledTasks(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListScheduledTasksParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListScheduledTasks(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListScheduledTasksParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	db.on("ListScheduledTasksPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListScheduledTasks(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListScheduledTasksParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}

	a, db = opscovAPI(t)
	rec = httptest.NewRecorder()
	a.ListTaskExecutions(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListTaskExecutionsParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("executions limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListTaskExecutions(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListTaskExecutionsParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("executions bad cursor = %d, want 400", rec.Code)
	}
	db.on("ListTaskExecutionsPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListTaskExecutions(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListTaskExecutionsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("executions list failure = %d, want 500", rec.Code)
	}
}

func TestOpscovDeleteScheduledTaskFailure(t *testing.T) {
	a, db := opscovAPI(t)
	db.on("SoftDeleteScheduledTask").err = opscovBoom()
	rec := httptest.NewRecorder()
	a.DeleteScheduledTask(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("delete failure = %d, want 500", rec.Code)
	}
}

// --- uptime.go --------------------------------------------------------------

func TestOpscovCreateUptimeCheck(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CreateUptimeCheck(rec, opscovRequest(http.MethodPost, "/x", body), api.CreateUptimeCheckParams{})
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	bad := `{"name":"","kind":"http","target":"ftp://x","interval_seconds":5,"timeout_seconds":0,` +
		`"failure_threshold":0,"success_threshold":0}`
	if rec := run(t, bad, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("all-invalid body = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"up","kind":"http","target":"https://s.example.test/h","resource_uuid":"nope"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad resource uuid = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"up","kind":"http","target":"https://s.example.test/h","resource_uuid":"`+fixtureUUID+`"}`,
		func(db *opscovDB) { db.on("GetResourceByUUIDForTeam").noRows = true }); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown resource = %d, want 422", rec.Code)
	}
	good := `{"name":"up","kind":"tcp","target":"db.example.test:5432","resource_uuid":"` + fixtureUUID + `",` +
		`"interval_seconds":60,"timeout_seconds":5,"failure_threshold":2,"success_threshold":1,"enabled":false}`
	if rec := run(t, good, nil); rec.Code != http.StatusCreated {
		t.Errorf("happy path = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, good, func(db *opscovDB) {
		db.on("CreateUptimeCheck").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", rec.Code)
	}
	if rec := run(t, good, func(db *opscovDB) {
		db.on("CreateUptimeCheck").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
}

func TestOpscovUpdateUptimeCheck(t *testing.T) {
	run := func(t *testing.T, ifMatch, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateUptimeCheck(rec, opscovRequest(http.MethodPatch, "/x", body), fixtureUUID,
			api.UpdateUptimeCheckParams{IfMatch: ifMatch})
		return rec
	}

	// The stored fixture target is "unit"; give every mutation a valid one so
	// only the branch under test misfires.
	target := `"target":"https://s.example.test/h"`

	if rec := run(t, "zz", `{}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad If-Match = %d, want 400", rec.Code)
	}
	if rec := run(t, `"1"`, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"",`+target+`}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("blank name = %d, want 422", rec.Code)
	}
	if rec := run(t, `"1"`, `{"target":"https://127.0.0.1/h"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("blocked target = %d, want 422", rec.Code)
	}
	if rec := run(t, `"1"`, `{"interval_seconds":5,`+target+`}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("interval too small = %d, want 422", rec.Code)
	}
	full := `{"name":"up",` + target + `,"interval_seconds":120,"timeout_seconds":30,` +
		`"failure_threshold":4,"success_threshold":2,"enabled":false}`
	if rec := run(t, `"1"`, full, nil); rec.Code != http.StatusOK {
		t.Errorf("happy path = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `"1"`, `{`+target+`}`, func(db *opscovDB) {
		db.on("UpdateUptimeCheck").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", rec.Code)
	}
	if rec := run(t, `"1"`, `{`+target+`}`, func(db *opscovDB) {
		db.on("UpdateUptimeCheck").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("update failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `"1"`, `{`+target+`}`, func(db *opscovDB) {
		db.on("UpdateUptimeCheck").tag = "UPDATE 0"
	}); rec.Code != http.StatusConflict {
		t.Errorf("version conflict = %d, want 409", rec.Code)
	}
	if rec := run(t, `"1"`, `{`+target+`}`, func(db *opscovDB) {
		s := db.on("GetUptimeCheckByUUID")
		s.skip = 1
		s.err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("reload failure = %d, want 500", rec.Code)
	}
}

func TestOpscovDeleteUptimeCheckFailure(t *testing.T) {
	a, db := opscovAPI(t)
	db.on("SoftDeleteUptimeCheck").tag = "UPDATE 0"
	rec := httptest.NewRecorder()
	a.DeleteUptimeCheck(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("zero-row delete = %d, want 500", rec.Code)
	}
}

func TestOpscovUptimeResourceUUIDOf(t *testing.T) {
	a, db := opscovAPI(t)
	r := opscovRequest(http.MethodGet, "/x", "")
	if got := a.uptimeResourceUUIDOf(r, store.UptimeCheck{}); got != nil {
		t.Errorf("nil resource id -> %v, want nil", got)
	}
	db.on("GetResourceByID").err = opscovBoom()
	if got := a.uptimeResourceUUIDOf(r, store.UptimeCheck{ResourceID: ptr(int64(1))}); got != nil {
		t.Errorf("resource lookup failure -> %v, want nil", got)
	}
}

func TestOpscovUptimeLists(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListUptimeChecks(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListUptimeChecksParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListUptimeChecks(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListUptimeChecksParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	db.on("ListUptimeChecksPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListUptimeChecks(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListUptimeChecksParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}

	a, db = opscovAPI(t)
	rec = httptest.NewRecorder()
	a.ListUptimeResults(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListUptimeResultsParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("results limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListUptimeResults(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListUptimeResultsParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("results bad cursor = %d, want 400", rec.Code)
	}
	db.on("ListUptimeResultsPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListUptimeResults(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListUptimeResultsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("results list failure = %d, want 500", rec.Code)
	}
}

// --- privatekeys.go ---------------------------------------------------------

func opscovTestKey(t *testing.T) *sshkey.Material {
	t.Helper()
	material, err := sshkey.GenerateEd25519("opscov")
	if err != nil {
		t.Fatal(err)
	}
	return material
}

func TestOpscovCreatePrivateKey(t *testing.T) {
	pem := strings.ReplaceAll(opscovTestKey(t).PrivatePEM, "\n", "\\n")
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CreatePrivateKey(rec, opscovRequest(http.MethodPost, "/x", body), api.CreatePrivateKeyParams{})
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"name":""}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("missing everything = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"name":"deploy","private_key":"garbage"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid material = %d, want 422", rec.Code)
	}
	good := `{"name":"deploy","description":"ops","private_key":"` + pem + `"}`
	if rec := run(t, good, nil); rec.Code != http.StatusCreated {
		t.Errorf("happy path = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, good, func(db *opscovDB) {
		db.on("CreatePrivateKey").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate fingerprint = %d, want 409", rec.Code)
	}
	if rec := run(t, good, func(db *opscovDB) {
		db.on("CreatePrivateKey").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
}

// ADR-075: there is no reveal — the response never carries private material,
// whatever the caller's permissions. The fake stores decryptable ciphertext
// so a leak, if one existed, would be visible in the body.
func TestOpscovGetPrivateKeyNeverServesMaterial(t *testing.T) {
	a, db := opscovAPI(t)
	blob := opscovEncrypt(t, a, "private_keys", "private_key_enc", []byte("PEM MATERIAL"))
	db.on("GetPrivateKeyByUUID").fill = opscovBytesFill(blob)
	rec := httptest.NewRecorder()
	a.GetPrivateKey(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "PEM MATERIAL") || strings.Contains(rec.Body.String(), `"private_key"`) {
		t.Errorf("response carries private material: %s", rec.Body)
	}
}

func TestOpscovGeneratePrivateKey(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.GeneratePrivateKey(rec, opscovRequest(http.MethodPost, "/x", body), api.GeneratePrivateKeyParams{})
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"name":""}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("blank name = %d, want 422", rec.Code)
	}
	rec := run(t, `{"name":"prod-cluster","description":"ops"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("happy path = %d, want 201: %s", rec.Code, rec.Body)
	}
	// The fake echoes the inserted row's zero values; what matters is that the
	// response schema never carries private material.
	if strings.Contains(rec.Body.String(), "PRIVATE KEY") {
		t.Errorf("generation response leaks private material: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"private_key"`) {
		t.Errorf("generation response should omit private_key entirely: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"public_key"`) {
		t.Errorf("generation response should carry the public key: %s", rec.Body)
	}
	if rec := run(t, `{"name":"x"}`, func(db *opscovDB) {
		db.on("CreatePrivateKey").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate fingerprint = %d, want 409", rec.Code)
	}
	if rec := run(t, `{"name":"x"}`, func(db *opscovDB) {
		db.on("CreatePrivateKey").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
}

func TestOpscovUpdatePrivateKey(t *testing.T) {
	pem := strings.ReplaceAll(opscovTestKey(t).PrivatePEM, "\n", "\\n")
	run := func(t *testing.T, ifMatch, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.UpdatePrivateKey(rec, opscovRequest(http.MethodPatch, "/x", body), fixtureUUID,
			api.UpdatePrivateKeyParams{IfMatch: ifMatch})
		return rec
	}

	if rec := run(t, "zz", `{}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad If-Match = %d, want 400", rec.Code)
	}
	if rec := run(t, `"1"`, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":""}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("blank name = %d, want 422", rec.Code)
	}
	if rec := run(t, `"1"`, `{"private_key":"garbage"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad rotation material = %d, want 422", rec.Code)
	}
	rotate := `{"name":"deploy","description":null,"private_key":"` + pem + `"}`
	if rec := run(t, `"1"`, rotate, nil); rec.Code != http.StatusOK {
		t.Errorf("rotation = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `"1"`, `{"name":"x"}`, func(db *opscovDB) {
		db.on("UpdatePrivateKey").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate fingerprint = %d, want 409", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"x"}`, func(db *opscovDB) {
		db.on("UpdatePrivateKey").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("update failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"x"}`, func(db *opscovDB) {
		db.on("UpdatePrivateKey").tag = "UPDATE 0"
	}); rec.Code != http.StatusConflict {
		t.Errorf("version conflict = %d, want 409", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"x"}`, func(db *opscovDB) {
		s := db.on("GetPrivateKeyByUUID")
		s.skip = 1
		s.err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("reload failure = %d, want 500", rec.Code)
	}
}

func TestOpscovDeletePrivateKey(t *testing.T) {
	run := func(t *testing.T, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.DeletePrivateKey(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID)
		return rec
	}

	if rec := run(t, func(db *opscovDB) {
		db.on("CountServersUsingPrivateKey").countOne = true
	}); rec.Code != http.StatusConflict {
		t.Errorf("server dependency = %d, want 409", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("CountServersUsingPrivateKey").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("server count failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("CountApplicationsUsingPrivateKey").countOne = true
	}); rec.Code != http.StatusConflict {
		t.Errorf("application dependency = %d, want 409", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("CountApplicationsUsingPrivateKey").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("application count failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("DeletePrivateKey").tag = "DELETE 0"
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("zero-row delete = %d, want 500", rec.Code)
	}
	if rec := run(t, nil); rec.Code != http.StatusNoContent {
		t.Errorf("happy path = %d, want 204", rec.Code)
	}
}

func TestOpscovSanitizeKeyError(t *testing.T) {
	if got := sanitizeKeyError(opscovBoom()); got != "not a valid PEM/OpenSSH private key" {
		t.Errorf("generic error -> %q", got)
	}
	if got := sanitizeKeyError(opscovPassphraseError{}); !strings.Contains(got, "passphrase") {
		t.Errorf("passphrase error -> %q", got)
	}
}

type opscovPassphraseError struct{}

func (opscovPassphraseError) Error() string {
	return "sshkey: passphrase-protected keys are not supported"
}

func TestOpscovKeysInUseFailureIsInformational(t *testing.T) {
	a, db := opscovAPI(t)
	db.on("ListPrivateKeyIDsInUse").err = opscovBoom()
	used := a.keysInUse(opscovRequest(http.MethodGet, "/x", ""), []int64{1, 2})
	if len(used) != 0 {
		t.Errorf("usage on failure = %v, want empty", used)
	}
}

// --- envs.go ----------------------------------------------------------------

func TestOpscovCreateApplicationEnv(t *testing.T) {
	run := func(t *testing.T, body string, preview *bool, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CreateApplicationEnv(rec, opscovRequest(http.MethodPost, "/x", body), fixtureUUID,
			api.CreateApplicationEnvParams{Preview: preview})
		return rec
	}

	if rec := run(t, "{not json", nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"key":"9bad","value":"x"}`, nil, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad key = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"key":"GOOD"}`, nil, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("missing value = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"key":"GOOD","value":"x","is_secret":true}`, ptr(true), nil); rec.Code != http.StatusCreated {
		t.Errorf("happy path = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"key":"GOOD","value":"x"}`, nil, func(db *opscovDB) {
		db.on("CreateEnvVar").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate key = %d, want 409", rec.Code)
	}
	if rec := run(t, `{"key":"GOOD","value":"x"}`, nil, func(db *opscovDB) {
		db.on("CreateEnvVar").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
}

func TestOpscovEnvToAPIRevealsDecryptableValue(t *testing.T) {
	a, db := opscovAPI(t)
	blob := opscovEncrypt(t, a, "environment_variables", "value_enc", []byte("plain-value"))
	db.on("ListEnvVarsPage").fill = opscovBytesFill(blob)
	rec := httptest.NewRecorder()
	a.ListApplicationEnvs(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListApplicationEnvsParams{Preview: ptr(true)})
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "plain-value") {
		t.Errorf("list body misses the decrypted value: %s", rec.Body)
	}
}

func TestOpscovListApplicationEnvsErrors(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListApplicationEnvs(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListApplicationEnvsParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ListApplicationEnvs(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListApplicationEnvsParams{Cursor: ptr("##")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	db.on("ListEnvVarsPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListApplicationEnvs(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, api.ListApplicationEnvsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}

func TestOpscovReplaceApplicationEnvs(t *testing.T) {
	run := func(t *testing.T, body string, truthy bool, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		db.truthy = truthy
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.ReplaceApplicationEnvs(rec, opscovRequest(http.MethodPut, "/x", body), fixtureUUID)
		return rec
	}
	one := `{"data":[{"key":"GOOD","value":"x"}]}`

	if rec := run(t, "{not json", false, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, one, false, func(db *opscovDB) {
		db.on("DeleteEnvVarsNotInKeys").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("delete failure = %d, want 500", rec.Code)
	}
	// truthy=true: the existing row is locked and survives untouched (§5.4).
	if rec := run(t, one, true, nil); rec.Code != http.StatusOK {
		t.Errorf("locked row skipped = %d, want 200: %s", rec.Code, rec.Body)
	}
	// Existing row not locked: it is re-encrypted and updated.
	if rec := run(t, one, false, nil); rec.Code != http.StatusOK {
		t.Errorf("update path = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, one, false, func(db *opscovDB) {
		db.on("UpdateEnvVar").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("update failure = %d, want 500", rec.Code)
	}
	// No existing row: the variable is inserted.
	if rec := run(t, one, false, func(db *opscovDB) {
		db.on("GetEnvVarByKey").noRows = true
	}); rec.Code != http.StatusOK {
		t.Errorf("insert path = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, one, false, func(db *opscovDB) {
		db.on("GetEnvVarByKey").noRows = true
		db.on("CreateEnvVar").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("insert conflict = %d, want 409", rec.Code)
	}
	if rec := run(t, one, false, func(db *opscovDB) {
		db.on("ListEnvVarsForDeploy").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("final list failure = %d, want 500", rec.Code)
	}
}

func TestOpscovUpdateApplicationEnv(t *testing.T) {
	run := func(t *testing.T, body string, truthy bool, prep func(*testing.T, *API, *opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		db.truthy = truthy
		if prep != nil {
			prep(t, a, db)
		}
		rec := httptest.NewRecorder()
		a.UpdateApplicationEnv(rec, opscovRequest(http.MethodPatch, "/x", body), fixtureUUID, fixtureUUID)
		return rec
	}

	if rec := run(t, `{"value":"x"}`, true, nil); rec.Code != http.StatusConflict {
		t.Errorf("locked variable = %d, want 409", rec.Code)
	}
	if rec := run(t, "{not json", false, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"value":"x","is_build_time":true}`, false, nil); rec.Code != http.StatusOK {
		t.Errorf("explicit value = %d, want 200: %s", rec.Code, rec.Body)
	}
	// No value in the patch: the current one is decrypted and re-used. The
	// default fixture blob cannot be decrypted…
	if rec := run(t, `{"is_build_time":true}`, false, nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("undecryptable current value = %d, want 500", rec.Code)
	}
	// …while a real ciphertext lets the flag flip go through.
	if rec := run(t, `{"is_build_time":true}`, false, func(t *testing.T, a *API, db *opscovDB) {
		blob := opscovEncrypt(t, a, "environment_variables", "value_enc", []byte("keep-me"))
		db.on("GetEnvVarByUUID").fill = opscovBytesFill(blob)
	}); rec.Code != http.StatusOK {
		t.Errorf("kept value = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"value":"x"}`, false, func(t *testing.T, a *API, db *opscovDB) {
		db.on("UpdateEnvVar").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("update failure = %d, want 500", rec.Code)
	}
}

func TestOpscovDeleteApplicationEnv(t *testing.T) {
	a, db := opscovAPI(t)
	db.on("DeleteEnvVar").tag = "DELETE 0"
	rec := httptest.NewRecorder()
	a.DeleteApplicationEnv(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID, fixtureUUID)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("zero-row delete = %d, want 500", rec.Code)
	}

	a, _ = opscovAPI(t)
	rec = httptest.NewRecorder()
	a.DeleteApplicationEnv(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID, "not-a-uuid")
	if rec.Code != http.StatusNotFound {
		t.Errorf("malformed env uuid = %d, want 404", rec.Code)
	}
}

func TestOpscovWriteEnvErrorMapping(t *testing.T) {
	a, _ := opscovAPI(t)
	r := opscovRequest(http.MethodPost, "/x", "")

	rec := httptest.NewRecorder()
	a.writeEnvError(rec, r, &envValidationError{detail: api.ErrorDetail{Message: "bad"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("validation error = %d, want 422", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.writeEnvError(rec, r, &envConflictError{})
	if rec.Code != http.StatusConflict {
		t.Errorf("conflict error = %d, want 409", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.writeEnvError(rec, r, opscovBoom())
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("generic error = %d, want 500", rec.Code)
	}
	if (&envValidationError{detail: api.ErrorDetail{Message: "bad"}}).Error() != "bad" {
		t.Error("envValidationError.Error() must surface the detail message")
	}
	if (&envConflictError{}).Error() == "" {
		t.Error("envConflictError.Error() must not be empty")
	}
}
