package handlers

// Coverage tests for servers.go, backups.go, componentbackups.go and
// s3storages.go, using the rescov harness from rescov_cov_test.go.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/store"
)

// ---------------------------------------------------------------------------
// servers.go
// ---------------------------------------------------------------------------

func TestRescovResolveServerBadUUID(t *testing.T) {
	a, _ := rescovAPI(t)
	rec := httptest.NewRecorder()
	a.GetServer(rec, rescovReq(http.MethodGet, "/servers/zz", ""), "zz")
	rescovWant(t, rec, http.StatusNotFound)
}

func TestRescovGetServerLookupFallbacks(t *testing.T) {
	// The private key and DNS credential lookups are best-effort: a failure
	// renders as absent, never as an error.
	a, db := rescovAPI(t)
	db.errOn["GetPrivateKeyByID"] = rescovErr
	db.errOn["GetDNSCredentialByID"] = rescovErr
	rec := httptest.NewRecorder()
	a.GetServer(rec, rescovReq(http.MethodGet, "/servers/"+fixtureUUID, ""), fixtureUUID)
	rescovWant(t, rec, http.StatusOK)
}

func TestRescovListServersParamErrors(t *testing.T) {
	a, db := rescovAPI(t)

	rec := httptest.NewRecorder()
	a.ListServers(rec, rescovReq(http.MethodGet, "/servers", ""), api.ListServersParams{Limit: ptr(0)})
	rescovWant(t, rec, http.StatusBadRequest)

	rec = httptest.NewRecorder()
	a.ListServers(rec, rescovReq(http.MethodGet, "/servers", ""), api.ListServersParams{Cursor: ptr("@@")})
	rescovWant(t, rec, http.StatusBadRequest)

	db.rowsOn["ListServersPage"] = 2
	rec = httptest.NewRecorder()
	a.ListServers(rec, rescovReq(http.MethodGet, "/servers", ""), api.ListServersParams{Limit: ptr(1)})
	rescovWant(t, rec, http.StatusOK)
}

func TestRescovCreateServer(t *testing.T) {
	run := func(t *testing.T, body string, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.CreateServer(rec, rescovReq(http.MethodPost, "/servers", body), api.CreateServerParams{})
		rescovWant(t, rec, want)
	}

	t.Run("invalid json", func(t *testing.T) {
		run(t, "nope", http.StatusBadRequest, nil)
	})
	t.Run("validation details", func(t *testing.T) {
		run(t, `{"name":"","host":" ","port":0,"ssh_timeout_seconds":0,"proxy_type":"caddy",`+
			`"proxy_http_port":0,"private_key_uuid":"bad"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("dns credential not found", func(t *testing.T) {
		run(t, `{"name":"s","host":"h","private_key_uuid":"`+fixtureUUID+`","dns_credential_uuid":"`+fixtureUUID+`"}`,
			http.StatusNotFound, func(db *rescovDB) {
				db.noRowsOn["GetDNSCredentialByUUID"] = true
			})
	})
	t.Run("private key lookup fails", func(t *testing.T) {
		run(t, `{"name":"s","host":"h","private_key_uuid":"`+fixtureUUID+`"}`,
			http.StatusUnprocessableEntity, func(db *rescovDB) {
				db.noRowsOn["GetPrivateKeyByUUID"] = true
			})
	})
	t.Run("unique violation", func(t *testing.T) {
		run(t, `{"name":"s","host":"h","proxy_type":"traefik","private_key_uuid":"`+fixtureUUID+`"}`,
			http.StatusConflict, func(db *rescovDB) {
				db.errOn["CreateServer"] = rescovPgUnique
			})
	})
	t.Run("generic error", func(t *testing.T) {
		run(t, `{"name":"s","host":"h","private_key_uuid":"`+fixtureUUID+`"}`,
			http.StatusInternalServerError, func(db *rescovDB) {
				db.errOn["CreateServer"] = rescovErr
			})
	})
	t.Run("success with every optional field", func(t *testing.T) {
		run(t, `{"name":"s","host":"h","port":2222,"user":"deploy","ssh_timeout_seconds":60,`+
			`"proxy_type":"none","proxy_http_port":81,"proxy_https_port":444,"is_build_server":true,`+
			`"wildcard_domain":"*.apps.example.test","private_key_uuid":"`+fixtureUUID+`",`+
			`"dns_credential_uuid":"`+fixtureUUID+`"}`, http.StatusCreated, nil)
	})
}

func TestRescovUpdateServer(t *testing.T) {
	patch := func(t *testing.T, body string, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateServer(rec, rescovReq(http.MethodPatch, "/servers/"+fixtureUUID, body),
			fixtureUUID, api.UpdateServerParams{IfMatch: `"1"`})
		rescovWant(t, rec, want)
	}

	t.Run("bad if-match", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.UpdateServer(rec, rescovReq(http.MethodPatch, "/servers/"+fixtureUUID, `{}`),
			fixtureUUID, api.UpdateServerParams{IfMatch: `"x"`})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("invalid patch body", func(t *testing.T) {
		patch(t, `nope`, http.StatusBadRequest, nil)
	})
	t.Run("port out of range", func(t *testing.T) {
		patch(t, `{"port":70000}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("bad private key uuid", func(t *testing.T) {
		patch(t, `{"private_key_uuid":"bad"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("private key lookup fails", func(t *testing.T) {
		patch(t, `{"private_key_uuid":"`+fixtureUUID+`"}`, http.StatusUnprocessableEntity, func(db *rescovDB) {
			db.noRowsOn["GetPrivateKeyByUUID"] = true
		})
	})
	t.Run("private key changed", func(t *testing.T) {
		// The looked-up key resolves to a different internal id than the
		// server's: connectivity changed, server goes back to pending.
		patch(t, `{"private_key_uuid":"`+fixtureUUID+`"}`, http.StatusOK, func(db *rescovDB) {
			db.zeroOn["GetPrivateKeyByUUID"] = true
		})
	})
	t.Run("dns credential not found", func(t *testing.T) {
		patch(t, `{"dns_credential_uuid":"`+fixtureUUID+`"}`, http.StatusNotFound, func(db *rescovDB) {
			db.noRowsOn["GetDNSCredentialByUUID"] = true
		})
	})
	t.Run("dns credential set", func(t *testing.T) {
		patch(t, `{"dns_credential_uuid":"`+fixtureUUID+`","proxy_type":"traefik","cleanup_cron":null}`,
			http.StatusOK, nil)
	})
	t.Run("invalid cleanup cron", func(t *testing.T) {
		patch(t, `{"cleanup_cron":"@bogus"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("invalid disk threshold", func(t *testing.T) {
		patch(t, `{"cleanup_disk_threshold_pct":0}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("unique violation", func(t *testing.T) {
		patch(t, `{"name":"n2"}`, http.StatusConflict, func(db *rescovDB) {
			db.errOn["UpdateServer"] = rescovPgUnique
		})
	})
	t.Run("generic error", func(t *testing.T) {
		patch(t, `{"name":"n2"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["UpdateServer"] = rescovErr
		})
	})
	t.Run("version conflict", func(t *testing.T) {
		patch(t, `{"name":"n2"}`, http.StatusConflict, func(db *rescovDB) {
			db.execTagOn["UpdateServer"] = "UPDATE 0"
		})
	})
	t.Run("reload error", func(t *testing.T) {
		patch(t, `{"name":"n2"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errAt["GetServerByUUID"] = 2
		})
	})
	t.Run("full update success", func(t *testing.T) {
		patch(t, `{"name":"n2","description":"d","host":"h2","port":2222,"user":"u2",`+
			`"ssh_timeout_seconds":60,"is_build_server":true,"wildcard_domain":"*.x.test",`+
			`"proxy_type":"none","proxy_http_port":81,"proxy_https_port":444,`+
			`"dns_credential_uuid":"","cleanup_enabled":true,"cleanup_cron":"daily",`+
			`"cleanup_disk_threshold_pct":50,"cleanup_prune_volumes":true,"cleanup_prune_networks":true}`,
			http.StatusOK, nil)
	})
}

func TestRescovDeleteServer(t *testing.T) {
	t.Run("count error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["CountResourcesOnServer"] = rescovErr
		rec := httptest.NewRecorder()
		a.DeleteServer(rec, rescovReq(http.MethodDelete, "/servers/"+fixtureUUID, ""), fixtureUUID)
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("soft delete error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["SoftDeleteServer"] = rescovErr
		rec := httptest.NewRecorder()
		a.DeleteServer(rec, rescovReq(http.MethodDelete, "/servers/"+fixtureUUID, ""), fixtureUUID)
		rescovWant(t, rec, http.StatusInternalServerError)
	})
}

func TestRescovValidateServer(t *testing.T) {
	t.Run("count error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["CountActiveJobsByLockKey"] = rescovErr
		rec := httptest.NewRecorder()
		a.ValidateServer(rec, rescovReq(http.MethodPost, "/servers/"+fixtureUUID+"/validate", "{}"),
			fixtureUUID, api.ValidateServerParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("enqueue error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["EnqueueJob"] = rescovErr
		rec := httptest.NewRecorder()
		a.ValidateServer(rec, rescovReq(http.MethodPost, "/servers/"+fixtureUUID+"/validate", "{}"),
			fixtureUUID, api.ValidateServerParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
}

func TestRescovDNSCredentialUUIDByID(t *testing.T) {
	a, db := rescovAPI(t)
	r := rescovReq(http.MethodGet, "/", "")
	if got := a.dnsCredentialUUIDByID(r, nil); got != nil {
		t.Fatalf("nil id resolved to %v", *got)
	}
	db.errOn["GetDNSCredentialByID"] = rescovErr
	if got := a.dnsCredentialUUIDByID(r, ptr(int64(1))); got != nil {
		t.Fatalf("failed lookup resolved to %v", *got)
	}
}

// ---------------------------------------------------------------------------
// backups.go
// ---------------------------------------------------------------------------

func TestRescovNormalizeCronRejectsUnschedulable(t *testing.T) {
	// Passes the shape filter but not the scheduler's parser.
	if _, ok := normalizeCron("99 * * * *"); ok {
		t.Fatal("minute 99 accepted")
	}
}

func TestRescovDrillIntervalAndRetention(t *testing.T) {
	if got := drillInterval(nil, 0); got != 7 {
		t.Fatalf("drillInterval(nil, 0) = %d, want 7", got)
	}
	if got := drillInterval(ptr(3), 7); got != 3 {
		t.Fatalf("drillInterval(3, 7) = %d, want 3", got)
	}
	if got := retentionCount(&api.RetentionPolicy{MaxCount: ptr(4)}, 0); got != 4 {
		t.Fatalf("retentionCount = %d, want 4", got)
	}
	if got := retentionDays(&api.RetentionPolicy{MaxAgeDays: ptr(9)}, 0); got != 9 {
		t.Fatalf("retentionDays = %d, want 9", got)
	}
}

func TestRescovBackupExecutionToAPIMessageFallback(t *testing.T) {
	out := backupExecutionToAPI(store.BackupExecution{
		Status:        store.BackupExecutionStatusSucceeded,
		S3UploadError: ptr("partial upload"),
		SizeBytes:     ptr(int64(42)),
	})
	if out.Message == nil || *out.Message != "partial upload" {
		t.Fatalf("message = %v, want the s3 upload error", out.Message)
	}
	if out.SizeBytes == nil || *out.SizeBytes != 42 {
		t.Fatalf("size = %v, want 42", out.SizeBytes)
	}
}

func TestRescovResolveBackupPlanBadPlanUUID(t *testing.T) {
	a, _ := rescovAPI(t)
	rec := httptest.NewRecorder()
	a.GetBackupPlan(rec, rescovReq(http.MethodGet, "/databases/"+fixtureUUID+"/backups/bad", ""),
		fixtureUUID, "not-a-uuid")
	rescovWant(t, rec, http.StatusNotFound)
}

func TestRescovListBackupPlansError(t *testing.T) {
	a, db := rescovAPI(t)
	db.errOn["ListBackupPlansForDatabase"] = rescovErr
	rec := httptest.NewRecorder()
	a.ListBackupPlans(rec, rescovReq(http.MethodGet, "/databases/"+fixtureUUID+"/backups", ""),
		fixtureUUID, api.ListBackupPlansParams{})
	rescovWant(t, rec, http.StatusInternalServerError)
}

func TestRescovCreateBackupPlan(t *testing.T) {
	run := func(t *testing.T, body string, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.CreateBackupPlan(rec, rescovReq(http.MethodPost, "/databases/"+fixtureUUID+"/backups", body),
			fixtureUUID, api.CreateBackupPlanParams{})
		rescovWant(t, rec, want)
	}

	t.Run("invalid json", func(t *testing.T) {
		run(t, "nope", http.StatusBadRequest, nil)
	})
	t.Run("bad frequency", func(t *testing.T) {
		run(t, `{"frequency":"whenever; rm -rf /"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("save_s3 without storage", func(t *testing.T) {
		run(t, `{"frequency":"daily","save_s3":true}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("unusable storage", func(t *testing.T) {
		run(t, `{"frequency":"daily","save_s3":true,"s3_storage_uuid":"`+fixtureUUID+`"}`,
			http.StatusUnprocessableEntity, nil) // truthy=false → is_usable=false
	})
	t.Run("bad timezone", func(t *testing.T) {
		run(t, `{"frequency":"daily","timezone":"Not/AZone"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("store error", func(t *testing.T) {
		run(t, `{"frequency":"daily"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["CreateBackupPlan"] = rescovErr
		})
	})
	t.Run("success with full options", func(t *testing.T) {
		run(t, `{"frequency":"0 3 * * *","timezone":"Europe/Paris","enabled":false,"dump_all":true,`+
			`"save_s3":true,"s3_storage_uuid":"`+fixtureUUID+`","s3_only":true,"save_local":false,`+
			`"local_retention":{"max_count":2,"max_age_days":3},"s3_retention":{"max_count":4,"max_age_days":5},`+
			`"drill_enabled":true,"drill_interval_days":10}`, http.StatusCreated, func(db *rescovDB) {
			db.truthy = true // the S3 storage passed its connectivity check
		})
	})
}

func TestRescovUpdateBackupPlan(t *testing.T) {
	patch := func(t *testing.T, ifMatch, body string, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateBackupPlan(rec, rescovReq(http.MethodPatch, "/databases/"+fixtureUUID+"/backups/"+fixtureUUID, body),
			fixtureUUID, fixtureUUID, api.UpdateBackupPlanParams{IfMatch: ifMatch})
		rescovWant(t, rec, want)
	}

	t.Run("bad if-match", func(t *testing.T) {
		patch(t, `"x"`, `{}`, http.StatusBadRequest, nil)
	})
	t.Run("invalid patch body", func(t *testing.T) {
		patch(t, `"1"`, `nope`, http.StatusBadRequest, nil)
	})
	t.Run("bad frequency", func(t *testing.T) {
		patch(t, `"1"`, `{"frequency":"@bogus"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("bad timezone", func(t *testing.T) {
		patch(t, `"1"`, `{"timezone":"Not/AZone"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("unusable storage", func(t *testing.T) {
		patch(t, `"1"`, `{"timezone":"UTC","s3_storage_uuid":"`+fixtureUUID+`","save_s3":true}`,
			http.StatusUnprocessableEntity, nil)
	})
	t.Run("update error", func(t *testing.T) {
		patch(t, `"1"`, `{"frequency":"daily","timezone":"UTC"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["UpdateBackupPlan"] = rescovErr
		})
	})
	t.Run("version conflict", func(t *testing.T) {
		patch(t, `"1"`, `{"frequency":"daily","timezone":"UTC"}`, http.StatusConflict, func(db *rescovDB) {
			db.execTagOn["UpdateBackupPlan"] = "UPDATE 0"
		})
	})
	t.Run("reload error", func(t *testing.T) {
		patch(t, `"1"`, `{"frequency":"daily","timezone":"UTC"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["GetBackupPlanByID"] = rescovErr
		})
	})
	t.Run("full success", func(t *testing.T) {
		patch(t, `"1"`, `{"frequency":"daily","timezone":"Europe/Paris","enabled":false,`+
			`"s3_storage_uuid":"`+fixtureUUID+`","save_s3":true,`+
			`"local_retention":{"max_count":1,"max_age_days":2},"s3_retention":{"max_count":3,"max_age_days":4},`+
			`"drill_enabled":true,"drill_interval_days":14}`, http.StatusOK, func(db *rescovDB) {
			db.truthy = true
		})
	})
}

func TestRescovDeleteBackupPlanError(t *testing.T) {
	a, db := rescovAPI(t)
	db.errOn["SoftDeleteBackupPlan"] = rescovErr
	rec := httptest.NewRecorder()
	a.DeleteBackupPlan(rec, rescovReq(http.MethodDelete, "/databases/"+fixtureUUID+"/backups/"+fixtureUUID, ""),
		fixtureUUID, fixtureUUID)
	rescovWant(t, rec, http.StatusInternalServerError)
}

func TestRescovExecuteBackupPlanErrors(t *testing.T) {
	t.Run("count error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["CountActiveJobsByLockKey"] = rescovErr
		rec := httptest.NewRecorder()
		a.ExecuteBackupPlan(rec, rescovReq(http.MethodPost, "/databases/"+fixtureUUID+"/backups/"+fixtureUUID+"/execute", "{}"),
			fixtureUUID, fixtureUUID, api.ExecuteBackupPlanParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("enqueue error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["EnqueueJob"] = rescovErr
		rec := httptest.NewRecorder()
		a.ExecuteBackupPlan(rec, rescovReq(http.MethodPost, "/databases/"+fixtureUUID+"/backups/"+fixtureUUID+"/execute", "{}"),
			fixtureUUID, fixtureUUID, api.ExecuteBackupPlanParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
}

func TestRescovListBackupExecutions(t *testing.T) {
	base := "/databases/" + fixtureUUID + "/backups/" + fixtureUUID + "/executions"
	t.Run("bad limit", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListBackupExecutions(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListBackupExecutionsParams{Limit: ptr(0)})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("bad cursor", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListBackupExecutions(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListBackupExecutionsParams{Cursor: ptr("@@")})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("store error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["ListBackupExecutionsPage"] = rescovErr
		rec := httptest.NewRecorder()
		a.ListBackupExecutions(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListBackupExecutionsParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("next page", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.rowsOn["ListBackupExecutionsPage"] = 2
		rec := httptest.NewRecorder()
		a.ListBackupExecutions(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListBackupExecutionsParams{Limit: ptr(1)})
		rescovWant(t, rec, http.StatusOK)
	})
}

func TestRescovRestoreBackupExecution(t *testing.T) {
	base := "/databases/" + fixtureUUID + "/backups/" + fixtureUUID + "/executions/" + fixtureUUID + "/restore"
	run := func(t *testing.T, execUUID, body string, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.RestoreBackupExecution(rec, rescovReq(http.MethodPost, base, body),
			fixtureUUID, fixtureUUID, execUUID, api.RestoreBackupExecutionParams{})
		rescovWant(t, rec, want)
	}

	t.Run("invalid json body", func(t *testing.T) {
		run(t, fixtureUUID, "nope", http.StatusBadRequest, nil)
	})
	t.Run("missing confirm", func(t *testing.T) {
		run(t, fixtureUUID, `{"confirm":false}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("bad execution uuid", func(t *testing.T) {
		run(t, "not-a-uuid", `{"confirm":true}`, http.StatusNotFound, nil)
	})
	t.Run("execution not found", func(t *testing.T) {
		run(t, fixtureUUID, `{"confirm":true}`, http.StatusNotFound, func(db *rescovDB) {
			db.noRowsOn["GetBackupExecutionByUUID"] = true
		})
	})
	t.Run("no usable dump", func(t *testing.T) {
		run(t, fixtureUUID, `{"confirm":true}`, http.StatusConflict, func(db *rescovDB) {
			db.nilPtrs = true // filename is NULL: nothing to restore
		})
	})
	t.Run("enqueue error", func(t *testing.T) {
		run(t, fixtureUUID, `{"confirm":true}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["EnqueueJob"] = rescovErr
		})
	})
}

func TestRescovResolveBackupS3Storage(t *testing.T) {
	newReq := func() (*httptest.ResponseRecorder, *http.Request) {
		return httptest.NewRecorder(), rescovReq(http.MethodPost, "/", "")
	}

	t.Run("wants s3 without storage", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec, r := newReq()
		if _, ok := a.resolveBackupS3Storage(rec, r, rescovIdentity(), nil, ptr(true), nil); ok {
			t.Fatal("accepted save_s3 without a storage")
		}
		rescovWant(t, rec, http.StatusUnprocessableEntity)
	})
	t.Run("no s3 at all", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec, r := newReq()
		id, ok := a.resolveBackupS3Storage(rec, r, rescovIdentity(), ptr(""), nil, ptr(false))
		if !ok || id != nil {
			t.Fatalf("plan without S3 refused: ok=%v id=%v", ok, id)
		}
	})
	t.Run("bad storage uuid", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec, r := newReq()
		if _, ok := a.resolveBackupS3Storage(rec, r, rescovIdentity(), ptr("bad"), ptr(true), nil); ok {
			t.Fatal("accepted an unresolvable storage")
		}
		rescovWant(t, rec, http.StatusNotFound)
	})
	t.Run("unusable storage", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec, r := newReq()
		if _, ok := a.resolveBackupS3Storage(rec, r, rescovIdentity(), ptr(fixtureUUID), ptr(true), nil); ok {
			t.Fatal("accepted an unusable storage")
		}
		rescovWant(t, rec, http.StatusUnprocessableEntity)
	})
	t.Run("usable storage", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.truthy = true
		rec, r := newReq()
		id, ok := a.resolveBackupS3Storage(rec, r, rescovIdentity(), ptr(fixtureUUID), ptr(true), nil)
		if !ok || id == nil {
			t.Fatalf("usable storage refused: ok=%v", ok)
		}
	})
}

func TestRescovS3StorageUUIDOf(t *testing.T) {
	a, db := rescovAPI(t)
	r := rescovReq(http.MethodGet, "/", "")
	if got := a.s3StorageUUIDOf(r, store.DatabaseBackupPlan{}); got != nil {
		t.Fatalf("plan without storage resolved to %v", *got)
	}
	db.errOn["GetS3StorageByID"] = rescovErr
	if got := a.s3StorageUUIDOf(r, store.DatabaseBackupPlan{S3StorageID: ptr(int64(1))}); got != nil {
		t.Fatalf("failed lookup resolved to %v", *got)
	}
}

func TestRescovRunRestoreDrill(t *testing.T) {
	base := "/databases/" + fixtureUUID + "/backups/" + fixtureUUID + "/drill"
	run := func(t *testing.T, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.RunRestoreDrill(rec, rescovReq(http.MethodPost, base, "{}"),
			fixtureUUID, fixtureUUID, api.RunRestoreDrillParams{})
		rescovWant(t, rec, want)
	}

	t.Run("no successful backup", func(t *testing.T) {
		run(t, http.StatusConflict, func(db *rescovDB) {
			db.noRowsOn["GetLatestSuccessfulBackupExecution"] = true
		})
	})
	t.Run("count error", func(t *testing.T) {
		run(t, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["CountActiveJobsByLockKey"] = rescovErr
		})
	})
	t.Run("enqueue error", func(t *testing.T) {
		run(t, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["EnqueueJob"] = rescovErr
		})
	})
}

func TestRescovListRestoreDrills(t *testing.T) {
	base := "/databases/" + fixtureUUID + "/backups/" + fixtureUUID + "/drills"
	t.Run("bad limit", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListRestoreDrills(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListRestoreDrillsParams{Limit: ptr(0)})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("bad cursor", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListRestoreDrills(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListRestoreDrillsParams{Cursor: ptr("@@")})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("store error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["ListRestoreDrillsPage"] = rescovErr
		rec := httptest.NewRecorder()
		a.ListRestoreDrills(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListRestoreDrillsParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("next page", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.rowsOn["ListRestoreDrillsPage"] = 2
		rec := httptest.NewRecorder()
		a.ListRestoreDrills(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListRestoreDrillsParams{Limit: ptr(1)})
		rescovWant(t, rec, http.StatusOK)
	})
}

// ---------------------------------------------------------------------------
// componentbackups.go
// ---------------------------------------------------------------------------

func TestRescovResolveComponentBadUUIDs(t *testing.T) {
	a, _ := rescovAPI(t)

	rec := httptest.NewRecorder()
	a.ListComponentBackupPlans(rec, rescovReq(http.MethodGet, "/service-components/bad/backups", ""),
		"not-a-uuid", api.ListComponentBackupPlansParams{})
	rescovWant(t, rec, http.StatusNotFound)

	rec = httptest.NewRecorder()
	a.GetComponentBackupPlan(rec, rescovReq(http.MethodGet, "/service-components/"+fixtureUUID+"/backups/bad", ""),
		fixtureUUID, "not-a-uuid")
	rescovWant(t, rec, http.StatusNotFound)
}

func TestRescovListComponentBackupPlansError(t *testing.T) {
	a, db := rescovAPI(t)
	db.errOn["ListBackupPlansForComponent"] = rescovErr
	rec := httptest.NewRecorder()
	a.ListComponentBackupPlans(rec, rescovReq(http.MethodGet, "/service-components/"+fixtureUUID+"/backups", ""),
		fixtureUUID, api.ListComponentBackupPlansParams{})
	rescovWant(t, rec, http.StatusInternalServerError)
}

func TestRescovCreateComponentBackupPlan(t *testing.T) {
	base := "/service-components/" + fixtureUUID + "/backups"
	run := func(t *testing.T, body string, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		db.truthy = true // is_database must be true past the classification gate
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.CreateComponentBackupPlan(rec, rescovReq(http.MethodPost, base, body),
			fixtureUUID, api.CreateComponentBackupPlanParams{})
		rescovWant(t, rec, want)
	}

	t.Run("not a database", func(t *testing.T) {
		a, _ := rescovAPI(t) // truthy=false → is_database=false
		rec := httptest.NewRecorder()
		a.CreateComponentBackupPlan(rec, rescovReq(http.MethodPost, base, `{"frequency":"daily"}`),
			fixtureUUID, api.CreateComponentBackupPlanParams{})
		rescovWant(t, rec, http.StatusUnprocessableEntity)
	})
	t.Run("unsupported engine", func(t *testing.T) {
		run(t, `{"frequency":"daily"}`, http.StatusUnprocessableEntity, func(db *rescovDB) {
			db.engine = "mysql"
		})
	})
	t.Run("invalid json", func(t *testing.T) {
		run(t, "nope", http.StatusBadRequest, nil)
	})
	t.Run("bad frequency", func(t *testing.T) {
		run(t, `{"frequency":"@bogus"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("save_s3 without storage", func(t *testing.T) {
		run(t, `{"frequency":"daily","save_s3":true}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("bad timezone", func(t *testing.T) {
		run(t, `{"frequency":"daily","timezone":"Not/AZone"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("store error", func(t *testing.T) {
		run(t, `{"frequency":"daily"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["CreateBackupPlan"] = rescovErr
		})
	})
	t.Run("success with full options", func(t *testing.T) {
		run(t, `{"frequency":"hourly","timezone":"Europe/Paris","enabled":false,"dump_all":true,`+
			`"save_s3":true,"s3_storage_uuid":"`+fixtureUUID+`","s3_only":true,"save_local":false,`+
			`"local_retention":{"max_count":2,"max_age_days":3},"s3_retention":{"max_count":4,"max_age_days":5},`+
			`"drill_enabled":true,"drill_interval_days":10}`, http.StatusCreated, nil)
	})
}

func TestRescovUpdateComponentBackupPlan(t *testing.T) {
	base := "/service-components/" + fixtureUUID + "/backups/" + fixtureUUID
	t.Run("bad if-match", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.UpdateComponentBackupPlan(rec, rescovReq(http.MethodPatch, base, `{}`),
			fixtureUUID, fixtureUUID, api.UpdateComponentBackupPlanParams{IfMatch: `"x"`})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("invalid patch body", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.UpdateComponentBackupPlan(rec, rescovReq(http.MethodPatch, base, `nope`),
			fixtureUUID, fixtureUUID, api.UpdateComponentBackupPlanParams{IfMatch: `"1"`})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("success", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.UpdateComponentBackupPlan(rec, rescovReq(http.MethodPatch, base, `{"frequency":"daily","timezone":"UTC"}`),
			fixtureUUID, fixtureUUID, api.UpdateComponentBackupPlanParams{IfMatch: `"1"`})
		rescovWant(t, rec, http.StatusOK)
	})
}

func TestRescovDeleteComponentBackupPlanError(t *testing.T) {
	a, db := rescovAPI(t)
	db.errOn["SoftDeleteBackupPlan"] = rescovErr
	rec := httptest.NewRecorder()
	a.DeleteComponentBackupPlan(rec,
		rescovReq(http.MethodDelete, "/service-components/"+fixtureUUID+"/backups/"+fixtureUUID, ""),
		fixtureUUID, fixtureUUID)
	rescovWant(t, rec, http.StatusInternalServerError)
}

func TestRescovExecuteComponentBackupPlanErrors(t *testing.T) {
	base := "/service-components/" + fixtureUUID + "/backups/" + fixtureUUID + "/execute"
	t.Run("count error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["CountActiveJobsByLockKey"] = rescovErr
		rec := httptest.NewRecorder()
		a.ExecuteComponentBackupPlan(rec, rescovReq(http.MethodPost, base, "{}"),
			fixtureUUID, fixtureUUID, api.ExecuteComponentBackupPlanParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("enqueue error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["EnqueueJob"] = rescovErr
		rec := httptest.NewRecorder()
		a.ExecuteComponentBackupPlan(rec, rescovReq(http.MethodPost, base, "{}"),
			fixtureUUID, fixtureUUID, api.ExecuteComponentBackupPlanParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
}

func TestRescovListComponentBackupExecutions(t *testing.T) {
	base := "/service-components/" + fixtureUUID + "/backups/" + fixtureUUID + "/executions"
	t.Run("bad limit", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListComponentBackupExecutions(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListComponentBackupExecutionsParams{Limit: ptr(0)})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("bad cursor", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListComponentBackupExecutions(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListComponentBackupExecutionsParams{Cursor: ptr("@@")})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("store error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["ListBackupExecutionsPage"] = rescovErr
		rec := httptest.NewRecorder()
		a.ListComponentBackupExecutions(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListComponentBackupExecutionsParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("next page", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.rowsOn["ListBackupExecutionsPage"] = 2
		rec := httptest.NewRecorder()
		a.ListComponentBackupExecutions(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListComponentBackupExecutionsParams{Limit: ptr(1)})
		rescovWant(t, rec, http.StatusOK)
	})
}

func TestRescovRestoreComponentBackupExecution(t *testing.T) {
	base := "/service-components/" + fixtureUUID + "/backups/" + fixtureUUID + "/executions/" + fixtureUUID + "/restore"
	run := func(t *testing.T, execUUID, body string, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.RestoreComponentBackupExecution(rec, rescovReq(http.MethodPost, base, body),
			fixtureUUID, fixtureUUID, execUUID, api.RestoreComponentBackupExecutionParams{})
		rescovWant(t, rec, want)
	}

	t.Run("missing confirm", func(t *testing.T) {
		run(t, fixtureUUID, `{"confirm":false}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("bad execution uuid", func(t *testing.T) {
		run(t, "not-a-uuid", `{"confirm":true}`, http.StatusNotFound, nil)
	})
	t.Run("execution not found", func(t *testing.T) {
		run(t, fixtureUUID, `{"confirm":true}`, http.StatusNotFound, func(db *rescovDB) {
			db.noRowsOn["GetBackupExecutionByUUID"] = true
		})
	})
	t.Run("no usable dump", func(t *testing.T) {
		run(t, fixtureUUID, `{"confirm":true}`, http.StatusConflict, func(db *rescovDB) {
			db.nilPtrs = true
		})
	})
	t.Run("stack resolution error", func(t *testing.T) {
		run(t, fixtureUUID, `{"confirm":true}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["GetResourceByID"] = rescovErr
		})
	})
	t.Run("enqueue error", func(t *testing.T) {
		run(t, fixtureUUID, `{"confirm":true}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["EnqueueJob"] = rescovErr
		})
	})
	t.Run("success", func(t *testing.T) {
		run(t, fixtureUUID, `{"confirm":true}`, http.StatusAccepted, nil)
	})
}

func TestRescovRunComponentRestoreDrill(t *testing.T) {
	base := "/service-components/" + fixtureUUID + "/backups/" + fixtureUUID + "/drill"
	run := func(t *testing.T, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.RunComponentRestoreDrill(rec, rescovReq(http.MethodPost, base, "{}"),
			fixtureUUID, fixtureUUID, api.RunComponentRestoreDrillParams{})
		rescovWant(t, rec, want)
	}

	t.Run("no successful backup", func(t *testing.T) {
		run(t, http.StatusConflict, func(db *rescovDB) {
			db.noRowsOn["GetLatestSuccessfulBackupExecution"] = true
		})
	})
	t.Run("count error", func(t *testing.T) {
		run(t, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["CountActiveJobsByLockKey"] = rescovErr
		})
	})
	t.Run("enqueue error", func(t *testing.T) {
		run(t, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["EnqueueJob"] = rescovErr
		})
	})
}

func TestRescovListComponentRestoreDrills(t *testing.T) {
	base := "/service-components/" + fixtureUUID + "/backups/" + fixtureUUID + "/drills"
	t.Run("bad limit", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListComponentRestoreDrills(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListComponentRestoreDrillsParams{Limit: ptr(0)})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("bad cursor", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListComponentRestoreDrills(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListComponentRestoreDrillsParams{Cursor: ptr("@@")})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("store error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["ListRestoreDrillsPage"] = rescovErr
		rec := httptest.NewRecorder()
		a.ListComponentRestoreDrills(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListComponentRestoreDrillsParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("next page", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.rowsOn["ListRestoreDrillsPage"] = 2
		rec := httptest.NewRecorder()
		a.ListComponentRestoreDrills(rec, rescovReq(http.MethodGet, base, ""),
			fixtureUUID, fixtureUUID, api.ListComponentRestoreDrillsParams{Limit: ptr(1)})
		rescovWant(t, rec, http.StatusOK)
	})
}

// ---------------------------------------------------------------------------
// s3storages.go
// ---------------------------------------------------------------------------

// rescovS3Storage builds a storage row whose credentials the test keyring can
// actually decrypt, pointing at endpoint.
func rescovS3Storage(t *testing.T, a *API, endpoint string) store.S3Storage {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(fixtureUUID); err != nil {
		t.Fatal(err)
	}
	access, err := a.Keyring.Encrypt("s3_storages", "access_key_enc", fixtureUUID, []byte("AKIA"))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := a.Keyring.Encrypt("s3_storages", "secret_key_enc", fixtureUUID, []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	return store.S3Storage{
		ID: 1, Uuid: u, Name: "unit", Endpoint: endpoint, Bucket: "bucket",
		Region: ptr("eu-west-3"), PathPrefix: ptr("prefix"), SseAlgorithm: ptr("AES256"),
		AccessKeyEnc: access, SecretKeyEnc: secret,
	}
}

func TestRescovS3ClientFor(t *testing.T) {
	a, _ := rescovAPI(t)

	t.Run("secret decrypt failure", func(t *testing.T) {
		s := rescovS3Storage(t, a, "https://s3.example.test")
		s.SecretKeyEnc = []byte("garbage")
		if _, err := a.s3ClientFor(s); err == nil {
			t.Fatal("expected an error for undecryptable secret key")
		}
	})
	t.Run("success with all optional fields", func(t *testing.T) {
		if _, err := a.s3ClientFor(rescovS3Storage(t, a, "https://s3.example.test")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRescovCheckStorage(t *testing.T) {
	a, _ := rescovAPI(t)

	t.Run("round trip succeeds", func(t *testing.T) {
		var stored []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPut:
				stored, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusOK)
			case http.MethodGet:
				_, _ = w.Write(stored)
			case http.MethodDelete:
				w.WriteHeader(http.StatusNoContent)
			}
		}))
		defer srv.Close()
		usable, msg := a.checkStorage(rescovReq(http.MethodPost, "/", "").Context(), rescovS3Storage(t, a, srv.URL))
		if !usable || msg != nil {
			t.Fatalf("usable=%v msg=%v, want usable storage", usable, msg)
		}
	})
	t.Run("round trip fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		usable, msg := a.checkStorage(rescovReq(http.MethodPost, "/", "").Context(), rescovS3Storage(t, a, srv.URL))
		if usable || msg == nil {
			t.Fatalf("usable=%v msg=%v, want a failure reason", usable, msg)
		}
	})
	t.Run("undecryptable credentials", func(t *testing.T) {
		s := rescovS3Storage(t, a, "https://s3.example.test")
		s.AccessKeyEnc = []byte("garbage")
		usable, msg := a.checkStorage(rescovReq(http.MethodPost, "/", "").Context(), s)
		if usable || msg == nil || !strings.Contains(*msg, "decrypt") {
			t.Fatalf("usable=%v msg=%v, want decrypt failure", usable, msg)
		}
	})
}

func TestRescovListS3StoragesParamErrors(t *testing.T) {
	a, db := rescovAPI(t)

	rec := httptest.NewRecorder()
	a.ListS3Storages(rec, rescovReq(http.MethodGet, "/s3-storages", ""), api.ListS3StoragesParams{Limit: ptr(0)})
	rescovWant(t, rec, http.StatusBadRequest)

	rec = httptest.NewRecorder()
	a.ListS3Storages(rec, rescovReq(http.MethodGet, "/s3-storages", ""), api.ListS3StoragesParams{Cursor: ptr("@@")})
	rescovWant(t, rec, http.StatusBadRequest)

	db.rowsOn["ListS3StoragesPage"] = 2
	rec = httptest.NewRecorder()
	a.ListS3Storages(rec, rescovReq(http.MethodGet, "/s3-storages", ""), api.ListS3StoragesParams{Limit: ptr(1)})
	rescovWant(t, rec, http.StatusOK)
}

func TestRescovCreateS3Storage(t *testing.T) {
	body := `{"name":"unit","endpoint":"https://s3.example.test","bucket":"b","access_key":"a","secret_key":"s",` +
		`"region":"eu-west-3","path_prefix":"p","server_side_encryption":"AES256"}`
	run := func(t *testing.T, body string, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.CreateS3Storage(rec, rescovReq(http.MethodPost, "/s3-storages", body), api.CreateS3StorageParams{})
		rescovWant(t, rec, want)
	}

	t.Run("invalid json", func(t *testing.T) {
		run(t, "nope", http.StatusBadRequest, nil)
	})
	t.Run("validation details", func(t *testing.T) {
		run(t, `{"name":" ","endpoint":"ftp://x","bucket":" ","access_key":"","secret_key":""}`,
			http.StatusUnprocessableEntity, nil)
	})
	t.Run("unique violation", func(t *testing.T) {
		run(t, body, http.StatusConflict, func(db *rescovDB) {
			db.errOn["CreateS3Storage"] = rescovPgUnique
		})
	})
	t.Run("generic error", func(t *testing.T) {
		run(t, body, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["CreateS3Storage"] = rescovErr
		})
	})
	t.Run("check persist error", func(t *testing.T) {
		run(t, body, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["SetS3StorageCheck"] = rescovErr
		})
	})
	t.Run("created not usable", func(t *testing.T) {
		// The stored fake row does not decrypt, so the connectivity check
		// records is_usable=false with a reason — creation still answers 201.
		run(t, body, http.StatusCreated, nil)
	})
}

func TestRescovUpdateS3Storage(t *testing.T) {
	patch := func(t *testing.T, ifMatch, body string, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateS3Storage(rec, rescovReq(http.MethodPatch, "/s3-storages/"+fixtureUUID, body),
			fixtureUUID, api.UpdateS3StorageParams{IfMatch: ifMatch})
		rescovWant(t, rec, want)
	}

	t.Run("bad if-match", func(t *testing.T) {
		patch(t, `"x"`, `{}`, http.StatusBadRequest, nil)
	})
	t.Run("invalid patch body", func(t *testing.T) {
		patch(t, `"1"`, `nope`, http.StatusBadRequest, nil)
	})
	t.Run("bad name", func(t *testing.T) {
		patch(t, `"1"`, `{"name":" "}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("bad endpoint", func(t *testing.T) {
		patch(t, `"1"`, `{"endpoint":"not a url"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("bad bucket", func(t *testing.T) {
		patch(t, `"1"`, `{"bucket":" "}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("unique violation", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"n2"}`, http.StatusConflict, func(db *rescovDB) {
			db.errOn["UpdateS3Storage"] = rescovPgUnique
		})
	})
	t.Run("generic error", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"n2"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["UpdateS3Storage"] = rescovErr
		})
	})
	t.Run("version conflict", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"n2"}`, http.StatusConflict, func(db *rescovDB) {
			db.execTagOn["UpdateS3Storage"] = "UPDATE 0"
		})
	})
	t.Run("reload error", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"n2"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errAt["GetS3StorageByUUID"] = 2
		})
	})
	t.Run("full update success", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"n2","endpoint":"https://new.example.test","region":"us-east-1",`+
			`"bucket":"b2","path_prefix":"p2","server_side_encryption":"AES256",`+
			`"access_key":"a2","secret_key":"s2"}`, http.StatusOK, nil)
	})
	t.Run("clear encryption", func(t *testing.T) {
		patch(t, `"1"`, `{"server_side_encryption":null,"region":null,"path_prefix":null}`, http.StatusOK, nil)
	})
}

func TestRescovDeleteS3Storage(t *testing.T) {
	t.Run("count error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["CountBackupPlansUsingS3Storage"] = rescovErr
		rec := httptest.NewRecorder()
		a.DeleteS3Storage(rec, rescovReq(http.MethodDelete, "/s3-storages/"+fixtureUUID, ""), fixtureUUID)
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("delete error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["DeleteS3Storage"] = rescovErr
		rec := httptest.NewRecorder()
		a.DeleteS3Storage(rec, rescovReq(http.MethodDelete, "/s3-storages/"+fixtureUUID, ""), fixtureUUID)
		rescovWant(t, rec, http.StatusInternalServerError)
	})
}

func TestRescovValidateS3StoragePersistError(t *testing.T) {
	a, db := rescovAPI(t)
	db.errOn["SetS3StorageCheck"] = rescovErr
	rec := httptest.NewRecorder()
	a.ValidateS3Storage(rec, rescovReq(http.MethodPost, "/s3-storages/"+fixtureUUID+"/validate", "{}"), fixtureUUID)
	rescovWant(t, rec, http.StatusInternalServerError)
}
