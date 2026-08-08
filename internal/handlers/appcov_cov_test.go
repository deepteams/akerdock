package handlers

// Coverage tests for applications.go, applicationupdate.go, applicationlogs.go
// and applicationauth.go. All identifiers are prefixed appcov (concurrent
// agents add tests to this package); the steerable protocol fake mirrors
// flowDB but adds per-query steering keyed on the sqlc "-- name:" header.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/events"
	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// appcovStep makes a steering rule fire only after `skip` earlier matches —
// e.g. "the second GetApplicationByUUID fails" for reload-error branches.
type appcovStep struct {
	skip int
	err  error
}

// appcovFill carries how one query's row is materialized.
type appcovFill struct {
	zero       bool
	truthy     bool
	intVal     int64
	nilPtr     bool
	emptyBytes bool
	enums      map[string]string
}

// appcovDB is flowDB with per-query steering: rules match the lowercase SQL
// text, keyed on the sqlc "-- name: <query> " header so one query can fail,
// return no rows, or fill differently while the rest of the flow stays green.
type appcovDB struct {
	mu           sync.Mutex
	truthy       bool
	countOne     bool
	enums        map[string]string
	intOn        map[string]int64
	rowsOn       map[string]int
	errOn        map[string]*appcovStep
	noRowsOn     []string
	execZeroOn   []string
	nilPtrOn     []string
	emptyBytesOn []string
	beginErr     error
	commitErr    error
}

func appcovKey(query string) string { return "-- name: " + strings.ToLower(query) + " " }

func (db *appcovDB) failOn(query string, err error) { db.failOnAfter(query, 0, err) }

func (db *appcovDB) failOnAfter(query string, skip int, err error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.errOn == nil {
		db.errOn = map[string]*appcovStep{}
	}
	db.errOn[appcovKey(query)] = &appcovStep{skip: skip, err: err}
}

func (db *appcovDB) noRows(query string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.noRowsOn = append(db.noRowsOn, appcovKey(query))
}

func (db *appcovDB) execZero(query string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.execZeroOn = append(db.execZeroOn, appcovKey(query))
}

func (db *appcovDB) nilPointers(query string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.nilPtrOn = append(db.nilPtrOn, appcovKey(query))
}

func (db *appcovDB) emptyBytes(query string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.emptyBytesOn = append(db.emptyBytesOn, appcovKey(query))
}

func (db *appcovDB) intFor(query string, v int64) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.intOn == nil {
		db.intOn = map[string]int64{}
	}
	db.intOn[appcovKey(query)] = v
}

func (db *appcovDB) rowsFor(query string, n int) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.rowsOn == nil {
		db.rowsOn = map[string]int{}
	}
	db.rowsOn[appcovKey(query)] = n
}

func (db *appcovDB) setEnum(typeName, value string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.enums == nil {
		db.enums = map[string]string{}
	}
	db.enums[typeName] = value
}

func (db *appcovDB) steer(sql string) (appcovFill, bool, bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	l := strings.ToLower(sql)
	opts := appcovFill{truthy: db.truthy, intVal: 1, enums: db.enums}
	var err error
	for key, step := range db.errOn {
		if strings.Contains(l, key) {
			if step.skip > 0 {
				step.skip--
				continue
			}
			err = step.err
		}
	}
	var noRows, execZero bool
	for _, key := range db.noRowsOn {
		if strings.Contains(l, key) {
			noRows = true
		}
	}
	for _, key := range db.execZeroOn {
		if strings.Contains(l, key) {
			execZero = true
		}
	}
	for _, key := range db.nilPtrOn {
		if strings.Contains(l, key) {
			opts.nilPtr = true
		}
	}
	for _, key := range db.emptyBytesOn {
		if strings.Contains(l, key) {
			opts.emptyBytes = true
		}
	}
	for key, v := range db.intOn {
		if strings.Contains(l, key) {
			opts.intVal = v
		}
	}
	opts.zero = strings.Contains(l, "count(") && !db.countOne
	return opts, noRows, execZero, err
}

func (db *appcovDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	_, _, execZero, err := db.steer(sql)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	if execZero {
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (db *appcovDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	opts, noRows, _, err := db.steer(sql)
	if err != nil {
		return nil, err
	}
	remaining := 1
	db.mu.Lock()
	for key, n := range db.rowsOn {
		if strings.Contains(strings.ToLower(sql), key) {
			remaining = n
		}
	}
	db.mu.Unlock()
	if noRows {
		remaining = 0
	}
	return &appcovRows{remaining: remaining, opts: opts}, nil
}

func (db *appcovDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	opts, noRows, _, err := db.steer(sql)
	if err == nil && noRows {
		err = pgx.ErrNoRows
	}
	return appcovRow{err: err, opts: opts}
}

type appcovRow struct {
	err  error
	opts appcovFill
}

func (r appcovRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for _, d := range dest {
		if err := appcovFillDest(d, r.opts); err != nil {
			return err
		}
	}
	return nil
}

type appcovRows struct {
	remaining int
	current   bool
	closed    bool
	err       error
	opts      appcovFill
}

func (r *appcovRows) Close()                                       { r.closed = true }
func (r *appcovRows) Err() error                                   { return r.err }
func (r *appcovRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (r *appcovRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *appcovRows) Values() ([]any, error)                       { return nil, nil }
func (r *appcovRows) RawValues() [][]byte                          { return nil }
func (r *appcovRows) Conn() *pgx.Conn                              { return nil }

func (r *appcovRows) Next() bool {
	if r.closed || r.remaining == 0 {
		r.closed = true
		r.current = false
		return false
	}
	r.remaining--
	r.current = true
	return true
}

func (r *appcovRows) Scan(dest ...any) error {
	if !r.current {
		return errors.New("appcov: Scan called before Next")
	}
	for _, d := range dest {
		if err := appcovFillDest(d, r.opts); err != nil {
			r.err = err
			r.Close()
			return err
		}
	}
	return nil
}

func appcovFillDest(dest any, o appcovFill) error {
	if dest == nil {
		return nil
	}
	switch dest.(type) {
	case *time.Time, *netip.Addr, *netip.Prefix, *pgtype.UUID, *pgtype.Timestamptz,
		*pgtype.Timestamp, *pgtype.Date, *pgtype.Time, *pgtype.Text, *pgtype.Bool,
		*pgtype.Int2, *pgtype.Int4, *pgtype.Int8, *pgtype.Float4, *pgtype.Float8,
		*pgtype.Numeric:
		return fillScanDestination(dest, o.zero, o.truthy)
	}
	v := reflect.ValueOf(dest)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return errors.New("appcov: scan destination is not a non-nil pointer")
	}
	return appcovFillValue(v.Elem(), o)
}

func appcovFillValue(v reflect.Value, o appcovFill) error {
	if !v.CanSet() {
		return nil
	}
	if v.Kind() == reflect.Pointer {
		if o.nilPtr {
			v.Set(reflect.Zero(v.Type()))
			return nil
		}
		v.Set(reflect.New(v.Type().Elem()))
		return appcovFillValue(v.Elem(), o)
	}
	if v.Kind() == reflect.String {
		if fixture, ok := o.enums[v.Type().Name()]; ok {
			v.SetString(fixture)
			return nil
		}
		if fixture, ok := enumFixtures[v.Type().Name()]; ok {
			v.SetString(fixture)
			return nil
		}
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("unit")
	case reflect.Bool:
		v.SetBool(o.truthy)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if o.zero {
			v.SetInt(0)
		} else {
			v.SetInt(o.intVal)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if o.emptyBytes {
				v.SetBytes(nil)
			} else {
				v.SetBytes([]byte("{}"))
			}
		} else {
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
	case reflect.Struct:
		if valid := v.FieldByName("Valid"); valid.IsValid() && valid.CanSet() && valid.Kind() == reflect.Bool {
			valid.SetBool(true)
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).Name != "Valid" {
					_ = appcovFillValue(v.Field(i), o)
				}
			}
		}
	}
	return nil
}

type appcovPool struct {
	db *appcovDB
}

func (p appcovPool) Begin(context.Context) (pgx.Tx, error) {
	if p.db.beginErr != nil {
		return nil, p.db.beginErr
	}
	return &appcovTx{db: p.db}, nil
}
func (appcovPool) Ping(context.Context) error { return nil }

type appcovTx struct {
	db *appcovDB
}

func (t *appcovTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (t *appcovTx) Commit(context.Context) error          { return t.db.commitErr }
func (*appcovTx) Rollback(context.Context) error          { return nil }
func (*appcovTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 1, nil
}
func (*appcovTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return flowBatch{} }
func (*appcovTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (*appcovTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return &pgconn.StatementDescription{}, nil
}

func (t *appcovTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}

func (t *appcovTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}

func (t *appcovTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (*appcovTx) Conn() *pgx.Conn { return nil }

var (
	_ store.DBTX = (*appcovDB)(nil)
	_ pgx.Rows   = (*appcovRows)(nil)
	_ pgx.Tx     = (*appcovTx)(nil)
)

func appcovAPI(t *testing.T) (*API, *appcovDB) {
	t.Helper()
	db := &appcovDB{}
	q := store.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte("1:" + key + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return &API{
		Store:    q,
		Pool:     appcovPool{db: db},
		Settings: instance.NewCache(q),
		Keyring:  keyring,
		Audit:    &audit.Recorder{Store: q, Logger: logger},
		Events:   events.NewBroker(),
		Version:  "unit",
		Logger:   logger,
	}, db
}

func appcovIdentity() *auth.Identity {
	return &auth.Identity{
		TokenID: 1, TokenUUID: fixtureUUID, TeamID: 1, TeamUUID: fixtureUUID,
		Permissions:  []string{string(auth.PermRoot)},
		InstanceRoot: true,
		UserID:       ptr(int64(1)),
	}
}

func appcovReq(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(auth.WithIdentity(req.Context(), appcovIdentity()))
}

// --- applicationauth.go -----------------------------------------------------

func appcovDBErr() error { return errors.New("database unavailable") }

func TestAppcovForwardAuthMissingReference(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.ApplicationForwardAuth(rec, httptest.NewRequest(http.MethodGet, "/webhooks/applications/auth", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAppcovForwardAuthInvalidReference(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.ApplicationForwardAuth(rec, httptest.NewRequest(http.MethodGet, "/webhooks/applications/auth?resource=not-a-uuid", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAppcovForwardAuthUnknownResource(t *testing.T) {
	a, db := appcovAPI(t)
	db.failOn("GetResourceAccessByUUID", appcovDBErr())
	rec := httptest.NewRecorder()
	a.ApplicationForwardAuth(rec, httptest.NewRequest(http.MethodGet, "/webhooks/applications/auth?resource="+fixtureUUID, nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAppcovForwardAuthLegacyApplicationParamNotSso(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	// Default enum fixture is "none": the resource is not sso protected.
	a.ApplicationForwardAuth(rec, httptest.NewRequest(http.MethodGet, "/webhooks/applications/auth?application="+fixtureUUID, nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not sso protected") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppcovForwardAuthValidCookieAnswers200(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("PreviewProtection", "sso")
	req := httptest.NewRequest(http.MethodGet, "/webhooks/applications/auth?resource="+fixtureUUID, nil)
	req.AddCookie(&http.Cookie{Name: applicationCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	a.ApplicationForwardAuth(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestAppcovForwardAuthFetchGets401(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("PreviewProtection", "sso")
	req := httptest.NewRequest(http.MethodGet, "/webhooks/applications/auth?resource="+fixtureUUID, nil)
	req.Header.Set("Sec-Fetch-Mode", "cors")
	rec := httptest.NewRecorder()
	a.ApplicationForwardAuth(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAppcovForwardAuthRedirectsNavigationToAuthorize(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("PreviewProtection", "sso")
	// A stale cookie whose token lookup fails must fall through to the dance.
	db.failOn("GetPreviewAccessTokenByHash", appcovDBErr())
	req := httptest.NewRequest(http.MethodGet, "/webhooks/applications/auth?resource="+fixtureUUID, nil)
	req.AddCookie(&http.Cookie{Name: applicationCookieName, Value: "stale"})
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("X-Forwarded-Host", "app.example.test")
	req.Header.Set("X-Forwarded-Uri", "/deep?x=1")
	rec := httptest.NewRecorder()
	a.ApplicationForwardAuth(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d body = %s, want 302", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/webhooks/applications/authorize") || !strings.Contains(loc, "app.example.test") {
		t.Fatalf("location = %q", loc)
	}
}

func TestAppcovForwardAuthWithoutInstanceFqdn(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("PreviewProtection", "sso")
	db.failOn("GetInstanceSettings", appcovDBErr())
	rec := httptest.NewRecorder()
	a.ApplicationForwardAuth(rec, httptest.NewRequest(http.MethodGet, "/webhooks/applications/auth?resource="+fixtureUUID, nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "FQDN") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppcovAuthorizeInvalidRedirect(t *testing.T) {
	a, _ := appcovAPI(t)
	for _, redirect := range []string{"", "http://plain.example.test/x", "%zz"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/webhooks/applications/authorize?redirect="+redirect, nil)
		a.ApplicationAuthorize(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("redirect %q: status = %d, want 400", redirect, rec.Code)
		}
	}
}

func appcovAuthorizeReq(redirect string) *http.Request {
	q := "redirect=" + strings.ReplaceAll(redirect, "&", "%26")
	return httptest.NewRequest(http.MethodGet, "/webhooks/applications/authorize?"+q, nil)
}

func TestAppcovAuthorizeUnknownHost(t *testing.T) {
	a, db := appcovAPI(t)
	db.failOn("GetResourceByRoutedHost", appcovDBErr())
	rec := httptest.NewRecorder()
	a.ApplicationAuthorize(rec, appcovAuthorizeReq("https://app.example.test/x"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAppcovAuthorizeNotSsoProtected(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.ApplicationAuthorize(rec, appcovAuthorizeReq("https://app.example.test/x"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAppcovAuthorizeWithoutSessions(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("PreviewProtection", "sso")
	rec := httptest.NewRecorder()
	a.ApplicationAuthorize(rec, appcovAuthorizeReq("https://app.example.test/x"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestAppcovAuthorizeAnonymousGoesToLogin(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("PreviewProtection", "sso")
	a.Sessions = &session.Manager{Store: a.Store}
	rec := httptest.NewRecorder()
	a.ApplicationAuthorize(rec, appcovAuthorizeReq("https://app.example.test/x"))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestAppcovAuthorizeForeignTeamIsRefused(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("PreviewProtection", "sso")
	// The session identity acts in team 1 as a plain member; the routed
	// resource belongs to team 2.
	db.setEnum("TeamRole", "member")
	db.intFor("GetResourceByRoutedHost", 2)
	a.Sessions = &session.Manager{Store: a.Store}
	req := appcovAuthorizeReq("https://app.example.test/x")
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "sess"})
	rec := httptest.NewRecorder()
	a.ApplicationAuthorize(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body = %s, want 403", rec.Code, rec.Body.String())
	}
}

func TestAppcovAuthorizeMintsTokenAndRedirects(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("PreviewProtection", "sso")
	a.Sessions = &session.Manager{Store: a.Store}
	req := appcovAuthorizeReq("https://app.example.test/deep/path?x=1")
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "sess"})
	rec := httptest.NewRecorder()
	a.ApplicationAuthorize(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d body = %s, want 302", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/.akerdock/app-callback") || !strings.Contains(loc, "token=") {
		t.Fatalf("location = %q", loc)
	}
}

func TestAppcovAuthorizeBareHostRedirectsToRoot(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("PreviewProtection", "sso")
	a.Sessions = &session.Manager{Store: a.Store}
	req := appcovAuthorizeReq("https://app.example.test")
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "sess"})
	rec := httptest.NewRecorder()
	a.ApplicationAuthorize(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d body = %s, want 302", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Location"), "next=%2F") {
		t.Fatalf("location = %q", rec.Header().Get("Location"))
	}
}

func TestAppcovAuthorizeTokenPersistenceFailure(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("PreviewProtection", "sso")
	db.failOn("CreateResourceAccessToken", appcovDBErr())
	a.Sessions = &session.Manager{Store: a.Store}
	req := appcovAuthorizeReq("https://app.example.test/x")
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "sess"})
	rec := httptest.NewRecorder()
	a.ApplicationAuthorize(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestAppcovCallbackMissingToken(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.ApplicationCallback(rec, httptest.NewRequest(http.MethodGet, "/.akerdock/app-callback", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAppcovCallbackUnknownToken(t *testing.T) {
	a, db := appcovAPI(t)
	db.failOn("GetPreviewAccessTokenByHash", appcovDBErr())
	rec := httptest.NewRecorder()
	a.ApplicationCallback(rec, httptest.NewRequest(http.MethodGet, "/.akerdock/app-callback?token=zz", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAppcovCallbackSetsCookieAndRedirects(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.ApplicationCallback(rec, httptest.NewRequest(http.MethodGet, "/.akerdock/app-callback?token=tok&next=/ok", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ok" {
		t.Fatalf("status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), applicationCookieName+"=tok") {
		t.Fatalf("cookie = %q", rec.Header().Get("Set-Cookie"))
	}
}

func TestAppcovCallbackRejectsProtocolRelativeNext(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.ApplicationCallback(rec, httptest.NewRequest(http.MethodGet, "/.akerdock/app-callback?token=tok&next=//evil.example.test", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestAppcovGenerateBasicAuthCredentials(t *testing.T) {
	creds, err := generateBasicAuthCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if !validBasicAuthCredentials(creds) || !strings.HasPrefix(creds, "akerdock:") {
		t.Fatalf("credentials = %q", creds)
	}
}

// --- applicationlogs.go -----------------------------------------------------

// appcovAgent wires a live loopback agent channel onto server 1 and answers
// each received command with the frames the script returns.
func appcovAgent(t *testing.T, a *API, script func(cmd agentwire.Command) []agentwire.Frame) {
	t.Helper()
	ac, agent := dialPair(t)
	a.AgentRPC = &AgentConns{}
	a.AgentRPC.register(1, ac)
	go func() {
		for {
			cmd, err := readCommand(agent)
			if err != nil {
				return
			}
			for _, f := range script(cmd) {
				if agentWrite(agent, f) != nil {
					return
				}
			}
		}
	}()
}

func appcovResult(id int64, body string) agentwire.Frame {
	return agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: id, Body: json.RawMessage(body)}}
}

func appcovResultErr(id int64, code string) agentwire.Frame {
	return agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: id, Err: &agentwire.Error{Code: code, Message: "scripted"}}}
}

func appcovChunk(id int64, data string) agentwire.Frame {
	return agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: id, Data: []byte(data)}}
}

func appcovEOF(id int64) agentwire.Frame {
	return agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: id, EOF: true}}
}

// appcovLogsScript answers ContainerInspect (tty on) and streams the given
// payload for ContainerLogs.
func appcovLogsScript(payload string) func(cmd agentwire.Command) []agentwire.Frame {
	return func(cmd agentwire.Command) []agentwire.Frame {
		switch cmd.Method {
		case agentwire.MethodContainerInspect:
			return []agentwire.Frame{appcovResult(cmd.ID, `{"Config":{"Tty":true}}`)}
		case agentwire.MethodContainerLogs:
			return []agentwire.Frame{
				appcovResult(cmd.ID, `{}`),
				appcovChunk(cmd.ID, payload),
				appcovEOF(cmd.ID),
			}
		default:
			return []agentwire.Frame{appcovResultErr(cmd.ID, agentwire.CodeUnimplemented)}
		}
	}
}

func TestAppcovGetApplicationLogsSnapshot(t *testing.T) {
	a, _ := appcovAPI(t)
	appcovAgent(t, a, appcovLogsScript("line one\nline two\n"))
	rec := httptest.NewRecorder()
	a.GetApplicationLogs(rec, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.GetApplicationLogsParams{Lines: ptr(50)})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "line one") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppcovGetApplicationLogsComponent(t *testing.T) {
	a, _ := appcovAPI(t)
	var target string
	var mu sync.Mutex
	appcovAgent(t, a, func(cmd agentwire.Command) []agentwire.Frame {
		if cmd.Method == agentwire.MethodContainerInspect {
			var p agentwire.NameParams
			_ = json.Unmarshal(cmd.Params, &p)
			mu.Lock()
			target = p.Name
			mu.Unlock()
		}
		return appcovLogsScript("svc\n")(cmd)
	})
	rec := httptest.NewRecorder()
	// The components fixture row is named "unit".
	a.GetApplicationLogs(rec, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.GetApplicationLogsParams{Component: ptr("unit")})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if target != fixtureUUID+"-unit" {
		t.Fatalf("inspected container = %q", target)
	}
}

func TestAppcovGetApplicationLogsUnknownComponent(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.GetApplicationLogs(rec, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.GetApplicationLogsParams{Component: ptr("nope")})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAppcovGetApplicationLogsComponentListFailure(t *testing.T) {
	a, db := appcovAPI(t)
	db.failOn("ListServiceComponents", appcovDBErr())
	rec := httptest.NewRecorder()
	a.GetApplicationLogs(rec, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.GetApplicationLogsParams{Component: ptr("unit")})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestAppcovGetApplicationLogsMissingContainer(t *testing.T) {
	a, _ := appcovAPI(t)
	appcovAgent(t, a, func(cmd agentwire.Command) []agentwire.Frame {
		return []agentwire.Frame{appcovResultErr(cmd.ID, agentwire.CodeNotFound)}
	})
	rec := httptest.NewRecorder()
	a.GetApplicationLogs(rec, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.GetApplicationLogsParams{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
}

func TestAppcovGetApplicationLogsAgentFailure(t *testing.T) {
	a, _ := appcovAPI(t)
	appcovAgent(t, a, func(cmd agentwire.Command) []agentwire.Frame {
		return []agentwire.Frame{appcovResultErr(cmd.ID, agentwire.CodeInternal)}
	})
	rec := httptest.NewRecorder()
	a.GetApplicationLogs(rec, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.GetApplicationLogsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestAppcovGetApplicationLogsAgentDisconnected(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.GetApplicationLogs(rec, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.GetApplicationLogsParams{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestAppcovContainerLogsSnapshotErrors(t *testing.T) {
	boom := errors.New("boom")

	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return container.InspectResponse{}, boom
	}
	if _, err := containerLogsSnapshot(context.Background(), rt, "c", 10); !errors.Is(err, boom) {
		t.Fatalf("inspect error = %v", err)
	}

	rt = &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return container.InspectResponse{Config: &container.Config{Tty: true}}, nil
	}
	rt.ContainerLogsFn = func(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
		return nil, boom
	}
	if _, err := containerLogsSnapshot(context.Background(), rt, "c", 10); !errors.Is(err, boom) {
		t.Fatalf("logs error = %v", err)
	}

	// tty off: the stream is stdcopy-framed, and garbage must surface as an
	// error rather than silent truncation.
	rt = &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return container.InspectResponse{Config: &container.Config{Tty: false}}, nil
	}
	rt.ContainerLogsFn = func(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("\x09\x00\x00\x00\x00\x00\x00\x01x")), nil
	}
	if _, err := containerLogsSnapshot(context.Background(), rt, "c", 10); err == nil {
		t.Fatal("malformed frame did not error")
	}

	rt = &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return container.InspectResponse{Config: &container.Config{Tty: true}}, nil
	}
	rt.ContainerLogsFn = func(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("hello\n")), nil
	}
	out, err := containerLogsSnapshot(context.Background(), rt, "c", 10)
	if err != nil || out != "hello\n" {
		t.Fatalf("out = %q err = %v", out, err)
	}
}

// appcovNoFlush hides the recorder's Flush method so the SSE handler sees a
// writer that cannot stream.
type appcovNoFlush struct {
	http.ResponseWriter
}

func TestAppcovStreamApplicationLogsRequiresFlusher(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.StreamApplicationLogs(appcovNoFlush{rec}, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.StreamApplicationLogsParams{})
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "streaming unsupported") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppcovStreamApplicationLogsUnknownComponent(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.StreamApplicationLogs(rec, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.StreamApplicationLogsParams{Component: ptr("nope")})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAppcovStreamApplicationLogsMissingContainer(t *testing.T) {
	a, _ := appcovAPI(t)
	appcovAgent(t, a, func(cmd agentwire.Command) []agentwire.Frame {
		return []agentwire.Frame{appcovResultErr(cmd.ID, agentwire.CodeNotFound)}
	})
	rec := httptest.NewRecorder()
	a.StreamApplicationLogs(rec, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.StreamApplicationLogsParams{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestAppcovStreamApplicationLogsInspectFailure(t *testing.T) {
	a, _ := appcovAPI(t)
	appcovAgent(t, a, func(cmd agentwire.Command) []agentwire.Frame {
		return []agentwire.Frame{appcovResultErr(cmd.ID, agentwire.CodeInternal)}
	})
	rec := httptest.NewRecorder()
	a.StreamApplicationLogs(rec, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.StreamApplicationLogsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestAppcovStreamApplicationLogsFollowFailure(t *testing.T) {
	a, _ := appcovAPI(t)
	appcovAgent(t, a, func(cmd agentwire.Command) []agentwire.Frame {
		if cmd.Method == agentwire.MethodContainerInspect {
			return []agentwire.Frame{appcovResult(cmd.ID, `{"Config":{"Tty":true}}`)}
		}
		return []agentwire.Frame{appcovResultErr(cmd.ID, agentwire.CodeInternal)}
	})
	rec := httptest.NewRecorder()
	a.StreamApplicationLogs(rec, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.StreamApplicationLogsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestAppcovStreamApplicationLogsEmitsSSE(t *testing.T) {
	a, _ := appcovAPI(t)
	appcovAgent(t, a, func(cmd agentwire.Command) []agentwire.Frame {
		switch cmd.Method {
		case agentwire.MethodContainerInspect:
			return []agentwire.Frame{appcovResult(cmd.ID, `{"Config":{"Tty":true}}`)}
		case agentwire.MethodContainerLogs:
			return []agentwire.Frame{
				appcovResult(cmd.ID, `{}`),
				appcovChunk(cmd.ID, "first\r\nsec"),
				appcovChunk(cmd.ID, "ond\ntail-without-newline"),
				appcovEOF(cmd.ID),
			}
		default:
			return []agentwire.Frame{appcovResultErr(cmd.ID, agentwire.CodeUnimplemented)}
		}
	})
	rec := httptest.NewRecorder()
	a.StreamApplicationLogs(rec, appcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.StreamApplicationLogsParams{Component: ptr("unit")})
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, body)
	}
	if !strings.Contains(body, ": connected") || !strings.Contains(body, "event: log") {
		t.Fatalf("body = %q", body)
	}
	if !strings.Contains(body, "first") || !strings.Contains(body, "second") {
		t.Fatalf("body = %q", body)
	}
	if strings.Contains(body, "tail-without-newline") {
		t.Fatalf("unterminated line must stay in the carry: %q", body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type = %q", got)
	}
}
