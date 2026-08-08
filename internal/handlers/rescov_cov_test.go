package handlers

// Coverage tests for services.go and databases.go, plus the rescov steerable
// database fake shared with rescov_cov2_test.go. The fake mirrors flowDB but is
// steerable PER QUERY NAME (the `-- name:` comment sqlc embeds in every SQL
// constant), so a test can fail exactly one statement of a multi-step handler
// and reach the error branch behind it.

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/compose"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/events"
	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/store"
)

var rescovErr = errors.New("rescov: steered failure")

// rescovPgUnique is a PostgreSQL unique violation, for the 409 branches.
var rescovPgUnique = &pgconn.PgError{Code: "23505"}

// rescovDB is a protocol-level pgx fake steerable per sqlc query name.
type rescovDB struct {
	truthy     bool
	countOne   bool
	nilPtrs    bool // leave every pointer-typed column NULL
	emptyBytes bool // leave every bytea column empty
	engine     string
	errOn      map[string]error
	errAt      map[string]int // fail only the Nth call of that query
	noRowsOn   map[string]bool
	zeroOn     map[string]bool
	rowsOn     map[string]int
	execTagOn  map[string]string
	beginErr   error
	commitErr  error
	calls      map[string]int
}

func rescovNewDB() *rescovDB {
	return &rescovDB{
		errOn:     map[string]error{},
		errAt:     map[string]int{},
		noRowsOn:  map[string]bool{},
		zeroOn:    map[string]bool{},
		rowsOn:    map[string]int{},
		execTagOn: map[string]string{},
		calls:     map[string]int{},
	}
}

// rescovQueryName extracts the sqlc query name from the `-- name: X :kind`
// comment embedded in every generated SQL constant.
func rescovQueryName(sql string) string {
	const marker = "-- name: "
	i := strings.Index(sql, marker)
	if i < 0 {
		return ""
	}
	rest := sql[i+len(marker):]
	if j := strings.IndexAny(rest, " \n"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func (db *rescovDB) failure(name string) error {
	db.calls[name]++
	if err, ok := db.errOn[name]; ok {
		return err
	}
	if n, ok := db.errAt[name]; ok && db.calls[name] == n {
		return rescovErr
	}
	return nil
}

func (db *rescovDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	name := rescovQueryName(sql)
	if err := db.failure(name); err != nil {
		return pgconn.CommandTag{}, err
	}
	tag := "UPDATE 1"
	if t, ok := db.execTagOn[name]; ok {
		tag = t
	}
	return pgconn.NewCommandTag(tag), nil
}

func (db *rescovDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	name := rescovQueryName(sql)
	if err := db.failure(name); err != nil {
		return nil, err
	}
	remaining := 1
	if n, ok := db.rowsOn[name]; ok {
		remaining = n
	}
	if db.noRowsOn[name] {
		remaining = 0
	}
	return &rescovRows{db: db, remaining: remaining}, nil
}

func (db *rescovDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	name := rescovQueryName(sql)
	err := db.failure(name)
	if err == nil && db.noRowsOn[name] {
		err = pgx.ErrNoRows
	}
	zero := db.zeroOn[name] ||
		(strings.Contains(strings.ToLower(sql), "count(") && !db.countOne)
	return rescovRow{db: db, err: err, zero: zero}
}

func rescovFill(db *rescovDB, dest any, zero bool) error {
	if d, ok := dest.(**store.DbEngine); ok && db.engine != "" {
		e := store.DbEngine(db.engine)
		*d = &e
		return nil
	}
	if db.emptyBytes {
		if d, ok := dest.(*[]byte); ok {
			*d = nil
			return nil
		}
	}
	if db.nilPtrs {
		v := reflect.ValueOf(dest)
		if v.Kind() == reflect.Pointer && !v.IsNil() && v.Elem().Kind() == reflect.Pointer {
			v.Elem().SetZero()
			return nil
		}
	}
	return fillScanDestination(dest, zero, db.truthy)
}

type rescovRow struct {
	db   *rescovDB
	err  error
	zero bool
}

func (r rescovRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for _, d := range dest {
		if err := rescovFill(r.db, d, r.zero); err != nil {
			return err
		}
	}
	return nil
}

type rescovRows struct {
	db        *rescovDB
	remaining int
	current   bool
	closed    bool
	err       error
}

func (r *rescovRows) Close()                                       { r.closed = true }
func (r *rescovRows) Err() error                                   { return r.err }
func (r *rescovRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (r *rescovRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *rescovRows) Values() ([]any, error)                       { return nil, nil }
func (r *rescovRows) RawValues() [][]byte                          { return nil }
func (r *rescovRows) Conn() *pgx.Conn                              { return nil }
func (r *rescovRows) Next() bool {
	if r.closed || r.remaining == 0 {
		r.closed = true
		r.current = false
		return false
	}
	r.remaining--
	r.current = true
	return true
}

func (r *rescovRows) Scan(dest ...any) error {
	if !r.current {
		return errors.New("Scan called before Next")
	}
	for _, d := range dest {
		if err := rescovFill(r.db, d, false); err != nil {
			r.err = err
			r.Close()
			return err
		}
	}
	return nil
}

type rescovPool struct{ db *rescovDB }

func (p rescovPool) Begin(context.Context) (pgx.Tx, error) {
	if p.db.beginErr != nil {
		return nil, p.db.beginErr
	}
	return &rescovTx{db: p.db}, nil
}
func (rescovPool) Ping(context.Context) error { return nil }

type rescovTx struct{ db *rescovDB }

func (t *rescovTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (t *rescovTx) Commit(context.Context) error          { return t.db.commitErr }
func (*rescovTx) Rollback(context.Context) error          { return nil }
func (*rescovTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 1, nil
}
func (*rescovTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return flowBatch{} }
func (*rescovTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (*rescovTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return &pgconn.StatementDescription{}, nil
}

func (t *rescovTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}

func (t *rescovTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}

func (t *rescovTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (*rescovTx) Conn() *pgx.Conn { return nil }

var (
	_ store.DBTX = (*rescovDB)(nil)
	_ pgx.Rows   = (*rescovRows)(nil)
	_ pgx.Tx     = (*rescovTx)(nil)
)

func rescovAPI(t *testing.T) (*API, *rescovDB) {
	t.Helper()
	db := rescovNewDB()
	q := store.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte("1:" + key + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return &API{
		Store:    q,
		Pool:     rescovPool{db: db},
		Settings: instance.NewCache(q),
		Keyring:  keyring,
		Audit:    &audit.Recorder{Store: q, Logger: logger},
		Events:   events.NewBroker(),
		Version:  "unit",
		Logger:   logger,
	}, db
}

func rescovIdentity(perms ...string) *auth.Identity {
	if len(perms) == 0 {
		perms = []string{string(auth.PermRoot)}
	}
	return &auth.Identity{
		TokenID: 1, TokenUUID: fixtureUUID, TeamID: 1, TeamUUID: fixtureUUID,
		Permissions: perms, InstanceRoot: true, UserID: ptr(int64(1)),
	}
}

func rescovReq(method, target, body string, perms ...string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r.WithContext(auth.WithIdentity(r.Context(), rescovIdentity(perms...)))
}

func rescovWant(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d — body %s", rec.Code, want, rec.Body.String())
	}
}

const (
	rescovComposeOK    = "services:\n  app:\n    image: nginx:1.27\n"
	rescovComposeBuild = "services:\n  app:\n    build: .\n"
	rescovComposeExt   = "services:\n  app:\n    image: nginx:1.27\nnetworks:\n  ext:\n    external: true\n"
)

// ---------------------------------------------------------------------------
// services.go
// ---------------------------------------------------------------------------

func TestRescovValidateComposeContent(t *testing.T) {
	ctx := context.Background()
	if details := validateComposeContent(ctx, "{{{", fixtureUUID, compose.Policy{}); len(details) == 0 {
		t.Fatal("parse error produced no details")
	}
	if details := validateComposeContent(ctx, rescovComposeExt, fixtureUUID, compose.Policy{}); len(details) == 0 {
		t.Fatal("external network produced no error details")
	}
	if details := validateComposeContent(ctx, rescovComposeExt, fixtureUUID, compose.Policy{AllowExternalObjects: true}); len(details) != 0 {
		t.Fatalf("allowed external network still refused: %v", details)
	}
	// Warning-only findings (an obsolete version key) never block: the
	// severity filter skips them.
	if details := validateComposeContent(ctx, "version: \"3\"\n"+rescovComposeOK, fixtureUUID, compose.Policy{}); len(details) != 0 {
		t.Fatalf("warning-only compose refused: %v", details)
	}
	details := validateComposeContent(ctx, rescovComposeBuild, fixtureUUID, compose.Policy{})
	found := false
	for _, d := range details {
		if d.Code != nil && *d.Code == "compose_build_unsupported" {
			found = true
		}
	}
	if !found {
		t.Fatalf("build service not refused: %v", details)
	}
}

func TestRescovListServicesParamErrors(t *testing.T) {
	a, db := rescovAPI(t)

	rec := httptest.NewRecorder()
	a.ListServices(rec, rescovReq(http.MethodGet, "/services", ""), api.ListServicesParams{Limit: ptr(0)})
	rescovWant(t, rec, http.StatusBadRequest)

	rec = httptest.NewRecorder()
	a.ListServices(rec, rescovReq(http.MethodGet, "/services", ""), api.ListServicesParams{Cursor: ptr("!!bad!!")})
	rescovWant(t, rec, http.StatusBadRequest)

	// limit+1 rows returned: the pagination closure and next_cursor branch.
	db.rowsOn["ListServiceStacksPage"] = 2
	rec = httptest.NewRecorder()
	a.ListServices(rec, rescovReq(http.MethodGet, "/services", ""), api.ListServicesParams{Limit: ptr(1)})
	rescovWant(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "next_cursor") {
		t.Fatalf("next_cursor missing: %s", rec.Body.String())
	}
}

func rescovServiceCreateBody(extra string) string {
	return `{"name":"svc","compose_content":"services:\n  app:\n    image: nginx:1.27\n",` +
		`"project_uuid":"` + fixtureUUID + `","environment_uuid":"` + fixtureUUID + `",` +
		`"server_uuid":"` + fixtureUUID + `"` + extra + `}`
}

func TestRescovCreateService(t *testing.T) {
	run := func(t *testing.T, body string, want int, steer func(*rescovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.CreateService(rec, rescovReq(http.MethodPost, "/services", body), api.CreateServiceParams{})
		rescovWant(t, rec, want)
		return rec
	}

	t.Run("invalid json", func(t *testing.T) {
		run(t, "not json", http.StatusBadRequest, nil)
	})
	t.Run("validation details", func(t *testing.T) {
		run(t, `{"name":"","compose_content":" ","access_protection":"bogus","access_basic_auth":"nocolon",`+
			`"project_uuid":"`+fixtureUUID+`","environment_uuid":"`+fixtureUUID+`","server_uuid":"`+fixtureUUID+`"}`,
			http.StatusUnprocessableEntity, nil)
	})
	t.Run("environment not found", func(t *testing.T) {
		run(t, rescovServiceCreateBody(""), http.StatusNotFound, func(db *rescovDB) {
			db.noRowsOn["GetEnvironmentByUUID"] = true
		})
	})
	t.Run("server not found", func(t *testing.T) {
		run(t, rescovServiceCreateBody(""), http.StatusNotFound, func(db *rescovDB) {
			db.noRowsOn["GetServerByUUID"] = true
		})
	})
	t.Run("destination error", func(t *testing.T) {
		run(t, rescovServiceCreateBody(""), http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["GetDefaultDestination"] = rescovErr
			db.errOn["CreateDestination"] = rescovErr
		})
	})
	t.Run("compose refused", func(t *testing.T) {
		run(t, `{"name":"svc","compose_content":"services:\n  app:\n    build: .\n",`+
			`"project_uuid":"`+fixtureUUID+`","environment_uuid":"`+fixtureUUID+`","server_uuid":"`+fixtureUUID+`"}`,
			http.StatusUnprocessableEntity, nil)
	})
	t.Run("begin error", func(t *testing.T) {
		run(t, rescovServiceCreateBody(""), http.StatusInternalServerError, func(db *rescovDB) {
			db.beginErr = rescovErr
		})
	})
	t.Run("resource unique violation", func(t *testing.T) {
		run(t, rescovServiceCreateBody(""), http.StatusConflict, func(db *rescovDB) {
			db.errOn["CreateResource"] = rescovPgUnique
		})
	})
	t.Run("resource generic error", func(t *testing.T) {
		run(t, rescovServiceCreateBody(""), http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["CreateResource"] = rescovErr
		})
	})
	t.Run("service row error", func(t *testing.T) {
		run(t, rescovServiceCreateBody(`,"connect_to_predefined_network":true,"noindex":true`),
			http.StatusInternalServerError, func(db *rescovDB) {
				db.errOn["CreateServiceRow"] = rescovErr
			})
	})
	t.Run("access protection error", func(t *testing.T) {
		run(t, rescovServiceCreateBody(`,"access_protection":"sso"`),
			http.StatusInternalServerError, func(db *rescovDB) {
				db.errOn["SetServiceAccessProtection"] = rescovErr
			})
	})
	t.Run("generated credentials store error", func(t *testing.T) {
		run(t, rescovServiceCreateBody(`,"access_protection":"basic_auth"`),
			http.StatusInternalServerError, func(db *rescovDB) {
				db.errOn["SetServiceAccessBasicAuth"] = rescovErr
			})
	})
	t.Run("commit error", func(t *testing.T) {
		run(t, rescovServiceCreateBody(`,"access_basic_auth":"user:pass"`),
			http.StatusInternalServerError, func(db *rescovDB) {
				db.commitErr = rescovErr
			})
	})
	t.Run("reload error", func(t *testing.T) {
		run(t, rescovServiceCreateBody(""), http.StatusInternalServerError, func(db *rescovDB) {
			db.errAt["GetServiceStackByUUID"] = 1
		})
	})
	t.Run("instant deploy warn", func(t *testing.T) {
		run(t, rescovServiceCreateBody(`,"instant_deploy":true`), http.StatusCreated, func(db *rescovDB) {
			db.errOn["CountActiveDeploymentsForServer"] = rescovErr
		})
	})
	t.Run("instant deploy success", func(t *testing.T) {
		run(t, rescovServiceCreateBody(`,"instant_deploy":true`), http.StatusCreated, nil)
	})
}

func TestRescovUpdateService(t *testing.T) {
	patch := func(t *testing.T, ifMatch, body string, want int, steer func(*rescovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateService(rec, rescovReq(http.MethodPatch, "/services/"+fixtureUUID, body),
			fixtureUUID, api.UpdateServiceParams{IfMatch: ifMatch})
		rescovWant(t, rec, want)
		return rec
	}

	t.Run("bad if-match", func(t *testing.T) {
		patch(t, `"abc"`, `{}`, http.StatusBadRequest, nil)
	})
	t.Run("invalid patch body", func(t *testing.T) {
		patch(t, `"1"`, `nope`, http.StatusBadRequest, nil)
	})
	t.Run("begin error", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"n2"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.beginErr = rescovErr
		})
	})
	t.Run("bad name", func(t *testing.T) {
		patch(t, `"1"`, `{"name":""}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("bad compose", func(t *testing.T) {
		patch(t, `"1"`, `{"compose_content":"{{{"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("bad access protection", func(t *testing.T) {
		patch(t, `"1"`, `{"access_protection":"bogus"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("bad basic auth", func(t *testing.T) {
		patch(t, `"1"`, `{"access_basic_auth":"nocolon"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("meta unique violation", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"n2","description":"d"}`, http.StatusConflict, func(db *rescovDB) {
			db.errOn["UpdateResourceMeta"] = rescovPgUnique
		})
	})
	t.Run("meta generic error", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"n2"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["UpdateResourceMeta"] = rescovErr
		})
	})
	t.Run("version conflict", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"n2"}`, http.StatusConflict, func(db *rescovDB) {
			db.execTagOn["UpdateResourceMeta"] = "UPDATE 0"
		})
	})
	t.Run("compose update error", func(t *testing.T) {
		body := `{"compose_content":"services:\n  app:\n    image: nginx:1.27\n","connect_to_predefined_network":true}`
		patch(t, `"1"`, body, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["UpdateServiceCompose"] = rescovErr
		})
	})
	t.Run("protection update error", func(t *testing.T) {
		patch(t, `"1"`, `{"access_protection":"sso"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["SetServiceAccessProtection"] = rescovErr
		})
	})
	t.Run("noindex update error", func(t *testing.T) {
		patch(t, `"1"`, `{"noindex":true}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["SetServiceNoindex"] = rescovErr
		})
	})
	t.Run("credentials update error", func(t *testing.T) {
		patch(t, `"1"`, `{"access_basic_auth":"user:pass"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["SetServiceAccessBasicAuth"] = rescovErr
		})
	})
	t.Run("generated credentials and routing warn", func(t *testing.T) {
		// No stored credentials: switching to basic_auth generates a pair; the
		// routing regeneration enqueue fails and is only logged.
		patch(t, `"1"`, `{"access_protection":"basic_auth"}`, http.StatusOK, func(db *rescovDB) {
			db.emptyBytes = true
			db.errOn["EnqueueJob"] = rescovErr
		})
	})
	t.Run("commit error", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"n2"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.commitErr = rescovErr
		})
	})
	t.Run("reload error", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"n2"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errAt["GetServiceStackByUUID"] = 2
		})
	})
	t.Run("noindex success with routing", func(t *testing.T) {
		patch(t, `"1"`, `{"noindex":false}`, http.StatusOK, nil)
	})
}

func TestRescovDeleteService(t *testing.T) {
	t.Run("desired status error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["SetResourceDesiredStatus"] = rescovErr
		rec := httptest.NewRecorder()
		a.DeleteService(rec, rescovReq(http.MethodDelete, "/services/"+fixtureUUID, ""),
			fixtureUUID, api.DeleteServiceParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("enqueue error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["EnqueueJob"] = rescovErr
		rec := httptest.NewRecorder()
		a.DeleteService(rec, rescovReq(http.MethodDelete, "/services/"+fixtureUUID, ""),
			fixtureUUID, api.DeleteServiceParams{DeleteVolumes: ptr(true)})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
}

func TestRescovGetServiceBadUUID(t *testing.T) {
	a, _ := rescovAPI(t)
	rec := httptest.NewRecorder()
	a.GetService(rec, rescovReq(http.MethodGet, "/services/zz", ""), "zz")
	rescovWant(t, rec, http.StatusNotFound)
}

func TestRescovListServiceComponentsError(t *testing.T) {
	a, db := rescovAPI(t)
	db.errOn["ListServiceComponents"] = rescovErr
	rec := httptest.NewRecorder()
	a.ListServiceComponents(rec, rescovReq(http.MethodGet, "/services/"+fixtureUUID+"/components", ""), fixtureUUID)
	rescovWant(t, rec, http.StatusInternalServerError)
}

func TestRescovDeployServiceEnqueueError(t *testing.T) {
	a, db := rescovAPI(t)
	db.errOn["CountActiveDeploymentsForServer"] = rescovErr
	rec := httptest.NewRecorder()
	a.DeployService(rec, rescovReq(http.MethodPost, "/services/"+fixtureUUID+"/deploy", "{}"),
		fixtureUUID, api.DeployServiceParams{})
	rescovWant(t, rec, http.StatusInternalServerError)
}

func TestRescovServiceLifecycleEnqueueError(t *testing.T) {
	a, db := rescovAPI(t)
	db.errOn["EnqueueJob"] = rescovErr
	rec := httptest.NewRecorder()
	a.StartService(rec, rescovReq(http.MethodPost, "/services/"+fixtureUUID+"/start", "{}"), fixtureUUID)
	rescovWant(t, rec, http.StatusInternalServerError)
}

func TestRescovServiceEnvs(t *testing.T) {
	t.Run("list bad limit", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListServiceEnvs(rec, rescovReq(http.MethodGet, "/services/"+fixtureUUID+"/envs", ""),
			fixtureUUID, api.ListServiceEnvsParams{Limit: ptr(500)})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("list bad cursor", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListServiceEnvs(rec, rescovReq(http.MethodGet, "/services/"+fixtureUUID+"/envs", ""),
			fixtureUUID, api.ListServiceEnvsParams{Cursor: ptr("@@")})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("list store error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["ListEnvVarsPage"] = rescovErr
		rec := httptest.NewRecorder()
		a.ListServiceEnvs(rec, rescovReq(http.MethodGet, "/services/"+fixtureUUID+"/envs", ""),
			fixtureUUID, api.ListServiceEnvsParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("list next page", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.rowsOn["ListEnvVarsPage"] = 2
		rec := httptest.NewRecorder()
		a.ListServiceEnvs(rec, rescovReq(http.MethodGet, "/services/"+fixtureUUID+"/envs", ""),
			fixtureUUID, api.ListServiceEnvsParams{Limit: ptr(1)})
		rescovWant(t, rec, http.StatusOK)
	})
	t.Run("create invalid json", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateServiceEnv(rec, rescovReq(http.MethodPost, "/services/"+fixtureUUID+"/envs", "nope"), fixtureUUID)
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("create invalid key", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateServiceEnv(rec, rescovReq(http.MethodPost, "/services/"+fixtureUUID+"/envs",
			`{"key":"9 bad","value":"v"}`), fixtureUUID)
		rescovWant(t, rec, http.StatusUnprocessableEntity)
	})
	t.Run("update env not found", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.UpdateServiceEnv(rec, rescovReq(http.MethodPatch, "/services/"+fixtureUUID+"/envs/bad", `{}`),
			fixtureUUID, "not-a-uuid")
		rescovWant(t, rec, http.StatusNotFound)
	})
	t.Run("update invalid json", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.UpdateServiceEnv(rec, rescovReq(http.MethodPatch, "/services/"+fixtureUUID+"/envs/"+fixtureUUID, "nope"),
			fixtureUUID, fixtureUUID)
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("update store error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["UpdateEnvVar"] = rescovErr
		rec := httptest.NewRecorder()
		a.UpdateServiceEnv(rec, rescovReq(http.MethodPatch, "/services/"+fixtureUUID+"/envs/"+fixtureUUID,
			`{"value":"v2"}`), fixtureUUID, fixtureUUID)
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("delete env not found", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.DeleteServiceEnv(rec, rescovReq(http.MethodDelete, "/services/"+fixtureUUID+"/envs/bad", ""),
			fixtureUUID, "not-a-uuid")
		rescovWant(t, rec, http.StatusNotFound)
	})
	t.Run("delete store error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["DeleteEnvVar"] = rescovErr
		rec := httptest.NewRecorder()
		a.DeleteServiceEnv(rec, rescovReq(http.MethodDelete, "/services/"+fixtureUUID+"/envs/"+fixtureUUID, ""),
			fixtureUUID, fixtureUUID)
		rescovWant(t, rec, http.StatusInternalServerError)
	})
}

func TestRescovListServiceDeployments(t *testing.T) {
	t.Run("bad limit", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListServiceDeployments(rec, rescovReq(http.MethodGet, "/services/"+fixtureUUID+"/deployments", ""),
			fixtureUUID, api.ListServiceDeploymentsParams{Limit: ptr(0)})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("bad cursor", func(t *testing.T) {
		a, _ := rescovAPI(t)
		rec := httptest.NewRecorder()
		a.ListServiceDeployments(rec, rescovReq(http.MethodGet, "/services/"+fixtureUUID+"/deployments", ""),
			fixtureUUID, api.ListServiceDeploymentsParams{Cursor: ptr("@@")})
		rescovWant(t, rec, http.StatusBadRequest)
	})
	t.Run("store error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["ListDeploymentsForResource"] = rescovErr
		rec := httptest.NewRecorder()
		a.ListServiceDeployments(rec, rescovReq(http.MethodGet, "/services/"+fixtureUUID+"/deployments", ""),
			fixtureUUID, api.ListServiceDeploymentsParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("next page", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.rowsOn["ListDeploymentsForResource"] = 2
		rec := httptest.NewRecorder()
		a.ListServiceDeployments(rec, rescovReq(http.MethodGet, "/services/"+fixtureUUID+"/deployments", ""),
			fixtureUUID, api.ListServiceDeploymentsParams{Limit: ptr(1)})
		rescovWant(t, rec, http.StatusOK)
	})
}

// ---------------------------------------------------------------------------
// databases.go
// ---------------------------------------------------------------------------

func rescovDBRow(t *testing.T, a *API, withPassword bool) dbRow {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(fixtureUUID); err != nil {
		t.Fatal(err)
	}
	row := dbRow{
		Resource: store.Resource{Uuid: u, Name: "db"},
		Database: store.Database{Engine: store.DbEnginePostgresql},
		DatabaseCredential: store.DatabaseCredential{
			Uuid: u, Username: "postgres", PasswordEnc: []byte("garbage"),
		},
		EnvironmentUuid: u, ProjectUuid: u, ServerUuid: u, ServerHost: "db.example.test",
	}
	if withPassword {
		enc, err := a.Keyring.Encrypt("database_credentials", "password_enc", uuidString(u), []byte("s3cret"))
		if err != nil {
			t.Fatal(err)
		}
		row.DatabaseCredential.PasswordEnc = enc
	}
	return row
}

func TestRescovDatabaseToAPI(t *testing.T) {
	a, _ := rescovAPI(t)

	t.Run("without read sensitive", func(t *testing.T) {
		out := a.databaseToAPI(rescovDBRow(t, a, true), rescovIdentity("read"))
		if out.PostgresPassword != nil {
			t.Fatal("password revealed without read:sensitive")
		}
		if out.IsRedacted == nil || !*out.IsRedacted {
			t.Fatal("expected redacted output")
		}
	})
	t.Run("decrypt failure stays redacted", func(t *testing.T) {
		out := a.databaseToAPI(rescovDBRow(t, a, false), rescovIdentity())
		if out.PostgresPassword != nil {
			t.Fatal("password revealed from bad ciphertext")
		}
	})
	t.Run("full public urls", func(t *testing.T) {
		row := rescovDBRow(t, a, true)
		row.Database.IsPublic = true
		row.Database.PublicPort = ptr(int32(5433))
		row.Database.PublicAccessMode = ptr(store.PublicAccessModePortMapping)
		row.Database.SslMode = ptr("require")
		row.DatabaseCredential.DbName = ptr("appdb")
		out := a.databaseToAPI(row, rescovIdentity())
		if out.PostgresPassword == nil || *out.PostgresPassword != "s3cret" {
			t.Fatal("password not revealed")
		}
		if out.InternalUrl == nil || !strings.Contains(*out.InternalUrl, "appdb") {
			t.Fatalf("internal url: %v", out.InternalUrl)
		}
		if out.ExternalUrl == nil || !strings.Contains(*out.ExternalUrl, "sslmode=require") {
			t.Fatalf("external url: %v", out.ExternalUrl)
		}
	})
	t.Run("private database has no external url", func(t *testing.T) {
		out := a.databaseToAPI(rescovDBRow(t, a, true), rescovIdentity())
		if out.ExternalUrl != nil {
			t.Fatal("external url published for a private database")
		}
	})
}

func TestRescovGetDatabaseBadUUID(t *testing.T) {
	a, _ := rescovAPI(t)
	rec := httptest.NewRecorder()
	a.GetDatabase(rec, rescovReq(http.MethodGet, "/databases/zz", ""), "zz")
	rescovWant(t, rec, http.StatusNotFound)
}

func TestRescovListDatabasesParamErrors(t *testing.T) {
	a, db := rescovAPI(t)

	rec := httptest.NewRecorder()
	a.ListDatabases(rec, rescovReq(http.MethodGet, "/databases", ""), api.ListDatabasesParams{Limit: ptr(0)})
	rescovWant(t, rec, http.StatusBadRequest)

	rec = httptest.NewRecorder()
	a.ListDatabases(rec, rescovReq(http.MethodGet, "/databases", ""), api.ListDatabasesParams{Cursor: ptr("@@")})
	rescovWant(t, rec, http.StatusBadRequest)

	db.rowsOn["ListDatabasesPage"] = 2
	rec = httptest.NewRecorder()
	a.ListDatabases(rec, rescovReq(http.MethodGet, "/databases", ""), api.ListDatabasesParams{Limit: ptr(1)})
	rescovWant(t, rec, http.StatusOK)
}

func rescovDatabaseCreateBody(extra string) string {
	return `{"name":"db","project_uuid":"` + fixtureUUID + `","environment_uuid":"` + fixtureUUID + `",` +
		`"server_uuid":"` + fixtureUUID + `"` + extra + `}`
}

func TestRescovCreatePostgresqlDatabase(t *testing.T) {
	run := func(t *testing.T, body string, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.CreatePostgresqlDatabase(rec, rescovReq(http.MethodPost, "/databases/postgresql", body),
			api.CreatePostgresqlDatabaseParams{})
		rescovWant(t, rec, want)
	}

	t.Run("invalid json", func(t *testing.T) {
		run(t, "nope", http.StatusBadRequest, nil)
	})
	t.Run("validation details", func(t *testing.T) {
		run(t, `{"name":"","image":"BAD IMAGE!","postgres_user":"9bad;","postgres_db":"also bad;",`+
			`"project_uuid":"`+fixtureUUID+`","environment_uuid":"`+fixtureUUID+`","server_uuid":"`+fixtureUUID+`"}`,
			http.StatusUnprocessableEntity, nil)
	})
	t.Run("environment not found", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(""), http.StatusNotFound, func(db *rescovDB) {
			db.noRowsOn["GetEnvironmentByUUID"] = true
		})
	})
	t.Run("server not found", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(""), http.StatusNotFound, func(db *rescovDB) {
			db.noRowsOn["GetServerByUUID"] = true
		})
	})
	t.Run("destination error", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(""), http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["GetDefaultDestination"] = rescovErr
			db.errOn["CreateDestination"] = rescovErr
		})
	})
	t.Run("public port allocation error", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(`,"is_public":true`), http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["NextFreePublicPort"] = rescovErr
		})
	})
	t.Run("public with allocated port", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(`,"is_public":true`), http.StatusCreated, nil)
	})
	t.Run("public with explicit port and tcp proxy", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(`,"is_public":true,"public_port":5433,"public_access_mode":"tcp_proxy",`+
			`"postgres_password":"pw","image":"postgres:16","postgres_user":"app","postgres_db":"appdb",`+
			`"ssl_mode":"require","ssl_enabled":true`), http.StatusCreated, nil)
	})
	t.Run("begin error", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(""), http.StatusInternalServerError, func(db *rescovDB) {
			db.beginErr = rescovErr
		})
	})
	t.Run("resource unique violation", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(""), http.StatusConflict, func(db *rescovDB) {
			db.errOn["CreateResource"] = rescovPgUnique
		})
	})
	t.Run("resource generic error", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(""), http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["CreateResource"] = rescovErr
		})
	})
	t.Run("database row port conflict", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(""), http.StatusConflict, func(db *rescovDB) {
			db.errOn["CreateDatabaseRow"] = rescovPgUnique
		})
	})
	t.Run("database row generic error", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(""), http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["CreateDatabaseRow"] = rescovErr
		})
	})
	t.Run("credential error", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(""), http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["CreateDatabaseCredential"] = rescovErr
		})
	})
	t.Run("commit error", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(""), http.StatusInternalServerError, func(db *rescovDB) {
			db.commitErr = rescovErr
		})
	})
	t.Run("reload error", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(""), http.StatusInternalServerError, func(db *rescovDB) {
			db.errAt["GetDatabaseByUUID"] = 1
		})
	})
	t.Run("instant start warn", func(t *testing.T) {
		run(t, rescovDatabaseCreateBody(`,"instant_start":true`), http.StatusCreated, func(db *rescovDB) {
			db.errOn["EnqueueJob"] = rescovErr
		})
	})
}

func TestRescovUpdateDatabase(t *testing.T) {
	patch := func(t *testing.T, ifMatch, body string, want int, steer func(*rescovDB)) {
		t.Helper()
		a, db := rescovAPI(t)
		if steer != nil {
			steer(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateDatabase(rec, rescovReq(http.MethodPatch, "/databases/"+fixtureUUID, body),
			fixtureUUID, api.UpdateDatabaseParams{IfMatch: ifMatch})
		rescovWant(t, rec, want)
	}

	t.Run("bad if-match", func(t *testing.T) {
		patch(t, `"x"`, `{}`, http.StatusBadRequest, nil)
	})
	t.Run("invalid patch body", func(t *testing.T) {
		patch(t, `"1"`, `nope`, http.StatusBadRequest, nil)
	})
	t.Run("bad name", func(t *testing.T) {
		patch(t, `"1"`, `{"name":""}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("bad image", func(t *testing.T) {
		patch(t, `"1"`, `{"image":"BAD IMAGE!"}`, http.StatusUnprocessableEntity, nil)
	})
	t.Run("public port allocation error", func(t *testing.T) {
		patch(t, `"1"`, `{"is_public":true}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.nilPtrs = true
			db.errOn["NextFreePublicPort"] = rescovErr
		})
	})
	t.Run("public port allocated", func(t *testing.T) {
		patch(t, `"1"`, `{"is_public":true,"postgres_conf":"max_connections=10","ssl_mode":"require"}`,
			http.StatusOK, func(db *rescovDB) {
				db.nilPtrs = true
			})
	})
	t.Run("begin error", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"db2"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.beginErr = rescovErr
		})
	})
	t.Run("meta error", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"db2","description":"d"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["UpdateResourceMeta"] = rescovErr
		})
	})
	t.Run("version conflict", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"db2"}`, http.StatusConflict, func(db *rescovDB) {
			db.execTagOn["UpdateResourceMeta"] = "UPDATE 0"
		})
	})
	t.Run("row port conflict", func(t *testing.T) {
		patch(t, `"1"`, `{"public_port":5433}`, http.StatusConflict, func(db *rescovDB) {
			db.errOn["UpdateDatabaseRow"] = rescovPgUnique
		})
	})
	t.Run("row generic error", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"db2"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["UpdateDatabaseRow"] = rescovErr
		})
	})
	t.Run("password update error", func(t *testing.T) {
		patch(t, `"1"`, `{"postgres_password":"newpw"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errOn["UpdateDatabasePassword"] = rescovErr
		})
	})
	t.Run("commit error", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"db2"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.commitErr = rescovErr
		})
	})
	t.Run("reload error", func(t *testing.T) {
		patch(t, `"1"`, `{"name":"db2"}`, http.StatusInternalServerError, func(db *rescovDB) {
			db.errAt["GetDatabaseByUUID"] = 2
		})
	})
	t.Run("password and image update success", func(t *testing.T) {
		patch(t, `"1"`, `{"postgres_password":"newpw","image":"postgres:16"}`, http.StatusOK, nil)
	})
}

func TestRescovDeleteDatabase(t *testing.T) {
	t.Run("desired status error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["SetResourceDesiredStatus"] = rescovErr
		rec := httptest.NewRecorder()
		a.DeleteDatabase(rec, rescovReq(http.MethodDelete, "/databases/"+fixtureUUID, ""),
			fixtureUUID, api.DeleteDatabaseParams{})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
	t.Run("enqueue error", func(t *testing.T) {
		a, db := rescovAPI(t)
		db.errOn["EnqueueJob"] = rescovErr
		rec := httptest.NewRecorder()
		a.DeleteDatabase(rec, rescovReq(http.MethodDelete, "/databases/"+fixtureUUID, ""),
			fixtureUUID, api.DeleteDatabaseParams{DeleteVolumes: ptr(true)})
		rescovWant(t, rec, http.StatusInternalServerError)
	})
}

func TestRescovDatabaseLifecycleEnqueueError(t *testing.T) {
	a, db := rescovAPI(t)
	db.errOn["EnqueueJob"] = rescovErr
	rec := httptest.NewRecorder()
	a.RestartDatabase(rec, rescovReq(http.MethodPost, "/databases/"+fixtureUUID+"/restart", "{}"), fixtureUUID)
	rescovWant(t, rec, http.StatusInternalServerError)
}

func TestRescovGeneratePassword(t *testing.T) {
	pw, err := generatePassword()
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != 64 {
		t.Fatalf("password length = %d, want 64", len(pw))
	}
}
