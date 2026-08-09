// Coverage tests for the network-session handler slice (external endpoints,
// ingress endpoints/sessions, port-forwards, terminal, tunnel sessions,
// relay). They reuse the flow_test.go philosophy — sqlc still performs every
// generated Scan — but with a steerable per-query fake so one query can fail,
// return no rows, or return a crafted row while its neighbours keep working.
//
// Every top-level identifier is prefixed netcov (concurrent-agent rule).
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/events"
	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// ---------------------------------------------------------------------------
// Steerable protocol fake
// ---------------------------------------------------------------------------

// netcovRule steers one query, matched by a substring of the SQL text (sqlc
// embeds "-- name: X" comments, so matches are precise).
type netcovRule struct {
	match  string
	err    error       // Query/QueryRow/Exec return this error
	noRows bool        // QueryRow → pgx.ErrNoRows; Query → zero rows
	rows   int         // number of rows Query emits (0 → default 1)
	tag    string      // Exec command tag (default "UPDATE 1")
	set    map[int]any // after default fill, dest[i] = value (positional)
	typed  []any       // after default fill, every dest of this exact type = value
}

type netcovDB struct {
	mu     sync.Mutex
	rules  []netcovRule
	truthy bool
}

func (db *netcovDB) rule(r netcovRule) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.rules = append(db.rules, r)
}

func (db *netcovDB) find(sql string) *netcovRule {
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := range db.rules {
		if strings.Contains(sql, db.rules[i].match) {
			return &db.rules[i]
		}
	}
	return nil
}

// netcovRowOf turns a flat sqlc row struct into a positional set map, so a
// test can hand a query the exact row it should return.
func netcovRowOf(src any) map[int]any {
	v := reflect.ValueOf(src)
	out := make(map[int]any, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		out[i] = v.Field(i).Interface()
	}
	return out
}

func netcovFill(dest []any, sql string, truthy bool, rule *netcovRule) error {
	zeroScalar := strings.Contains(strings.ToLower(sql), "count(")
	for _, d := range dest {
		if err := fillScanDestination(d, zeroScalar, truthy); err != nil {
			return err
		}
	}
	if rule == nil {
		return nil
	}
	for _, tv := range rule.typed {
		want := reflect.TypeOf(tv)
		for _, d := range dest {
			dv := reflect.ValueOf(d)
			if dv.Kind() == reflect.Pointer && dv.Elem().Type() == want {
				dv.Elem().Set(reflect.ValueOf(tv))
			}
		}
	}
	for i, v := range rule.set {
		if i >= len(dest) {
			return fmt.Errorf("netcov: set index %d out of %d scan targets for %q", i, len(dest), rule.match)
		}
		dv := reflect.ValueOf(dest[i]).Elem()
		if v == nil {
			dv.Set(reflect.Zero(dv.Type()))
			continue
		}
		sv := reflect.ValueOf(v)
		if !sv.Type().AssignableTo(dv.Type()) {
			if !sv.Type().ConvertibleTo(dv.Type()) {
				return fmt.Errorf("netcov: cannot assign %T to %s (index %d, %q)", v, dv.Type(), i, rule.match)
			}
			sv = sv.Convert(dv.Type())
		}
		dv.Set(sv)
	}
	return nil
}

func (db *netcovDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if rule := db.find(sql); rule != nil {
		if rule.err != nil {
			return pgconn.CommandTag{}, rule.err
		}
		if rule.tag != "" {
			return pgconn.NewCommandTag(rule.tag), nil
		}
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (db *netcovDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	rule := db.find(sql)
	if rule != nil && rule.err != nil {
		return nil, rule.err
	}
	remaining := 1
	if rule != nil {
		if rule.noRows {
			remaining = 0
		} else if rule.rows > 0 {
			remaining = rule.rows
		}
	}
	return &netcovRows{db: db, sql: sql, rule: rule, remaining: remaining}, nil
}

func (db *netcovDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return netcovRow{db: db, sql: sql, rule: db.find(sql)}
}

type netcovRow struct {
	db   *netcovDB
	sql  string
	rule *netcovRule
}

func (r netcovRow) Scan(dest ...any) error {
	if r.rule != nil {
		if r.rule.err != nil {
			return r.rule.err
		}
		if r.rule.noRows {
			return pgx.ErrNoRows
		}
	}
	return netcovFill(dest, r.sql, r.db.truthy, r.rule)
}

type netcovRows struct {
	db        *netcovDB
	sql       string
	rule      *netcovRule
	remaining int
	current   bool
	err       error
}

func (r *netcovRows) Close()                                       {}
func (r *netcovRows) Err() error                                   { return r.err }
func (r *netcovRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (r *netcovRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *netcovRows) Values() ([]any, error)                       { return nil, nil }
func (r *netcovRows) RawValues() [][]byte                          { return nil }
func (r *netcovRows) Conn() *pgx.Conn                              { return nil }
func (r *netcovRows) Next() bool {
	if r.remaining == 0 {
		r.current = false
		return false
	}
	r.remaining--
	r.current = true
	return true
}

func (r *netcovRows) Scan(dest ...any) error {
	if !r.current {
		return errors.New("netcov: Scan before Next")
	}
	if err := netcovFill(dest, r.sql, r.db.truthy, r.rule); err != nil {
		r.err = err
		return err
	}
	return nil
}

var _ store.DBTX = (*netcovDB)(nil)

// netcovPool satisfies handlerPool over the fake, like flowPool does.
type netcovPool struct{ db *netcovDB }

func (p netcovPool) Begin(context.Context) (pgx.Tx, error) { return &netcovTx{db: p.db}, nil }
func (netcovPool) Ping(context.Context) error              { return nil }

type netcovTx struct{ db *netcovDB }

func (t *netcovTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (*netcovTx) Commit(context.Context) error            { return nil }
func (*netcovTx) Rollback(context.Context) error          { return nil }
func (*netcovTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 1, nil
}
func (*netcovTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return flowBatch{} }
func (*netcovTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (*netcovTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return &pgconn.StatementDescription{}, nil
}

func (t *netcovTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}

func (t *netcovTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}

func (t *netcovTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (*netcovTx) Conn() *pgx.Conn { return nil }

var _ pgx.Tx = (*netcovTx)(nil)

// ---------------------------------------------------------------------------
// API + request scaffolding
// ---------------------------------------------------------------------------

func netcovLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func netcovAPI(t *testing.T) (*API, *netcovDB) {
	t.Helper()
	db := &netcovDB{}
	q := store.New(db)
	logger := netcovLogger()
	key := strings.Repeat("A", 43) + "=" // base64 of 32 bytes
	keyring, err := envelope.Parse([]byte("1:" + key + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	a := &API{
		Store:    q,
		Pool:     netcovPool{db: db},
		Settings: instance.NewCache(q),
		Keyring:  keyring,
		Audit:    &audit.Recorder{Store: q, Logger: logger},
		Events:   events.NewBroker(),
		Version:  "unit",
		Logger:   logger,
	}
	a.Sessions = &session.Manager{Store: q}
	return a, db
}

// netcovIdentity is a root token identity acting for user 1.
func netcovIdentity() *auth.Identity {
	uid := int64(1)
	return &auth.Identity{
		TokenID: 1, TokenUUID: fixtureUUID, TeamID: 1, TeamUUID: fixtureUUID,
		Permissions: []string{string(auth.PermRoot)},
		UserID:      &uid,
	}
}

func netcovSessionIdentity() *auth.Identity {
	id := netcovIdentity()
	id.Session = true
	return id
}

func netcovRequest(t *testing.T, method, target string, body any, id *auth.Identity) *http.Request {
	t.Helper()
	var reader *strings.Reader
	switch b := body.(type) {
	case nil:
		reader = strings.NewReader("")
	case string:
		reader = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
	}
	r := httptest.NewRequest(method, target, reader)
	r.Header.Set("Content-Type", "application/json")
	if id != nil {
		r = r.WithContext(auth.WithIdentity(r.Context(), id))
		if id.Session {
			r.AddCookie(&http.Cookie{Name: session.CookieName, Value: "netcov-session"})
		}
	}
	return r
}

func netcovStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, want, rec.Body.String())
	}
}

// netcovFreshSessionRow makes GetSessionByTokenHash return a session whose
// step-up markers are recent, so freshFactor passes.
func netcovFreshSessionRow(db *netcovDB) {
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	db.rule(netcovRule{
		match: "-- name: GetSessionByTokenHash ",
		set:   map[int]any{5: now, 14: now}, // MfaVerifiedAt, TotpVerifiedAt
	})
}

var netcovOtherUUID = "22222222-2222-4222-8222-222222222222"

func netcovUniqueViolation() error {
	return &pgconn.PgError{Code: "23505", Message: "duplicate key"}
}

// ---------------------------------------------------------------------------
// externalendpoints.go
// ---------------------------------------------------------------------------

func TestNetcovEndpointScopeVariants(t *testing.T) {
	valid := api.ExternalEndpointCreate{
		Name: "prod", Host: "10.0.0.7", Port: 5432, ServerUuid: fixtureUUID,
	}

	t.Run("project and environment resolve", func(t *testing.T) {
		a, _ := netcovAPI(t)
		body := valid
		body.ProjectUuid = ptr(fixtureUUID)
		body.EnvironmentUuid = ptr(fixtureUUID)
		rec := httptest.NewRecorder()
		a.CreateExternalEndpoint(rec, netcovRequest(t, http.MethodPost, "/external-endpoints", body, netcovIdentity()))
		netcovStatus(t, rec, http.StatusCreated)
	})

	t.Run("environment without project is refused", func(t *testing.T) {
		a, _ := netcovAPI(t)
		body := valid
		body.EnvironmentUuid = ptr(fixtureUUID)
		rec := httptest.NewRecorder()
		a.CreateExternalEndpoint(rec, netcovRequest(t, http.MethodPost, "/external-endpoints", body, netcovIdentity()))
		netcovStatus(t, rec, http.StatusBadRequest)
	})

	t.Run("unknown project 404s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetProjectByUUID ", noRows: true})
		body := valid
		body.ProjectUuid = ptr(fixtureUUID)
		rec := httptest.NewRecorder()
		a.CreateExternalEndpoint(rec, netcovRequest(t, http.MethodPost, "/external-endpoints", body, netcovIdentity()))
		netcovStatus(t, rec, http.StatusNotFound)
	})

	t.Run("unknown environment 404s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetEnvironmentByUUID", noRows: true})
		body := valid
		body.ProjectUuid = ptr(fixtureUUID)
		body.EnvironmentUuid = ptr(fixtureUUID)
		rec := httptest.NewRecorder()
		a.CreateExternalEndpoint(rec, netcovRequest(t, http.MethodPost, "/external-endpoints", body, netcovIdentity()))
		netcovStatus(t, rec, http.StatusNotFound)
	})
}

func TestNetcovCreateExternalEndpointErrors(t *testing.T) {
	t.Run("invalid JSON", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateExternalEndpoint(rec, netcovRequest(t, http.MethodPost, "/external-endpoints", "{", netcovIdentity()))
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("duplicate name 409s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CreateExternalEndpoint ", err: netcovUniqueViolation()})
		body := api.ExternalEndpointCreate{Name: "prod", Host: "db", Port: 5432, ServerUuid: fixtureUUID}
		rec := httptest.NewRecorder()
		a.CreateExternalEndpoint(rec, netcovRequest(t, http.MethodPost, "/external-endpoints", body, netcovIdentity()))
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("store failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CreateExternalEndpoint ", err: errors.New("down")})
		body := api.ExternalEndpointCreate{Name: "prod", Host: "db", Port: 5432, ServerUuid: fixtureUUID}
		rec := httptest.NewRecorder()
		a.CreateExternalEndpoint(rec, netcovRequest(t, http.MethodPost, "/external-endpoints", body, netcovIdentity()))
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
}

func TestNetcovUpdateExternalEndpointErrors(t *testing.T) {
	body := api.ExternalEndpointCreate{Name: "prod", Host: "db", Port: 5432, ServerUuid: fixtureUUID}

	t.Run("invalid JSON", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.UpdateExternalEndpoint(rec, netcovRequest(t, http.MethodPut, "/external-endpoints/x", "{", netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("invalid body fields", func(t *testing.T) {
		a, _ := netcovAPI(t)
		bad := body
		bad.Host = "a b"
		rec := httptest.NewRecorder()
		a.UpdateExternalEndpoint(rec, netcovRequest(t, http.MethodPut, "/external-endpoints/x", bad, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("duplicate name 409s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: UpdateExternalEndpoint ", err: netcovUniqueViolation()})
		rec := httptest.NewRecorder()
		a.UpdateExternalEndpoint(rec, netcovRequest(t, http.MethodPut, "/external-endpoints/x", body, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("store failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: UpdateExternalEndpoint ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.UpdateExternalEndpoint(rec, netcovRequest(t, http.MethodPut, "/external-endpoints/x", body, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("endpoint gone 404s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetExternalEndpointByUUID ", noRows: true})
		rec := httptest.NewRecorder()
		a.UpdateExternalEndpoint(rec, netcovRequest(t, http.MethodPut, "/external-endpoints/x", body, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNotFound)
	})
}

func TestNetcovDeleteExternalEndpointStoreFailure(t *testing.T) {
	a, db := netcovAPI(t)
	db.rule(netcovRule{match: "-- name: DeleteExternalEndpoint ", err: errors.New("down")})
	rec := httptest.NewRecorder()
	a.DeleteExternalEndpoint(rec, netcovRequest(t, http.MethodDelete, "/external-endpoints/x", nil, netcovIdentity()), fixtureUUID)
	netcovStatus(t, rec, http.StatusInternalServerError)
}

func TestNetcovListExternalEndpointsParamAndStoreErrors(t *testing.T) {
	t.Run("bad limit", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.ListExternalEndpoints(rec, netcovRequest(t, http.MethodGet, "/external-endpoints", nil, netcovIdentity()),
			api.ListExternalEndpointsParams{Limit: ptr(0)})
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("bad cursor", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.ListExternalEndpoints(rec, netcovRequest(t, http.MethodGet, "/external-endpoints", nil, netcovIdentity()),
			api.ListExternalEndpointsParams{Cursor: ptr("!!!not-base64!!!")})
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("store failure", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListExternalEndpointsPage ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.ListExternalEndpoints(rec, netcovRequest(t, http.MethodGet, "/external-endpoints", nil, netcovIdentity()),
			api.ListExternalEndpointsParams{})
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("next cursor emitted when the page overflows", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListExternalEndpointsPage ", rows: 2})
		rec := httptest.NewRecorder()
		a.ListExternalEndpoints(rec, netcovRequest(t, http.MethodGet, "/external-endpoints", nil, netcovIdentity()),
			api.ListExternalEndpointsParams{Limit: ptr(1)})
		netcovStatus(t, rec, http.StatusOK)
		if !strings.Contains(rec.Body.String(), "next_cursor") || strings.Contains(rec.Body.String(), `"next_cursor":null`) {
			t.Fatalf("expected a non-null next_cursor, got %s", rec.Body.String())
		}
	})
}

func TestNetcovLiveGrantFor(t *testing.T) {
	a, db := netcovAPI(t)
	row := store.ExternalEndpoint{ID: 7}

	// No acting human: no badge.
	if g := a.liveGrantFor(netcovRequest(t, http.MethodGet, "/x", nil, nil), row, &auth.Identity{}); g != nil {
		t.Fatal("an identity without a user must not resolve a grant")
	}
	// Lookup failure: no badge either.
	db.rule(netcovRule{match: "-- name: GetLiveExternalEndpointGrant ", err: errors.New("down")})
	if g := a.liveGrantFor(netcovRequest(t, http.MethodGet, "/x", nil, nil), row, netcovIdentity()); g != nil {
		t.Fatal("a failing lookup must not resolve a grant")
	}
}

func TestNetcovRequiredFactorPrefersPasskey(t *testing.T) {
	t.Run("a passkey outranks everything", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CountPasskeysForUser ", set: map[int]any{0: int64(2)}})
		if got := a.requiredFactor(netcovRequest(t, http.MethodGet, "/x", nil, nil), 1); got != "passkey" {
			t.Fatalf("factor = %q, want passkey", got)
		}
	})
	t.Run("a confirmed TOTP is the fallback", func(t *testing.T) {
		a, _ := netcovAPI(t)
		// Passkey count scans 0 by default; the MFA factor row scans confirmed.
		if got := a.requiredFactor(netcovRequest(t, http.MethodGet, "/x", nil, nil), 1); got != "totp" {
			t.Fatalf("factor = %q, want totp", got)
		}
	})
	t.Run("no factor at all", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetMfaFactorForUser ", err: pgx.ErrNoRows})
		if got := a.requiredFactor(netcovRequest(t, http.MethodGet, "/x", nil, nil), 1); got != "" {
			t.Fatalf("factor = %q, want none", got)
		}
	})
}

func TestNetcovRequestGrantRefusals(t *testing.T) {
	grant := api.ExternalEndpointGrantCreate{Reason: "debug prod incident", DurationMinutes: 30}

	t.Run("token callers are refused", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.RequestExternalEndpointGrant(rec, netcovRequest(t, http.MethodPost, "/g", grant, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusForbidden)
		if !strings.Contains(rec.Body.String(), "stepup_required") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("session cookie missing", func(t *testing.T) {
		a, _ := netcovAPI(t)
		id := netcovSessionIdentity()
		r := netcovRequest(t, http.MethodPost, "/g", grant, nil)
		r = r.WithContext(auth.WithIdentity(r.Context(), id)) // no cookie added
		rec := httptest.NewRecorder()
		a.RequestExternalEndpointGrant(rec, r, fixtureUUID)
		netcovStatus(t, rec, http.StatusUnauthorized)
	})
	t.Run("invalid JSON", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.RequestExternalEndpointGrant(rec, netcovRequest(t, http.MethodPost, "/g", "{", netcovSessionIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("empty reason", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.RequestExternalEndpointGrant(rec, netcovRequest(t, http.MethodPost, "/g",
			api.ExternalEndpointGrantCreate{Reason: "  ", DurationMinutes: 5}, netcovSessionIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("zero duration", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.RequestExternalEndpointGrant(rec, netcovRequest(t, http.MethodPost, "/g",
			api.ExternalEndpointGrantCreate{Reason: "x", DurationMinutes: 0}, netcovSessionIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("no enrolled factor is a final refusal", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetMfaFactorForUser ", err: pgx.ErrNoRows})
		rec := httptest.NewRecorder()
		a.RequestExternalEndpointGrant(rec, netcovRequest(t, http.MethodPost, "/g", grant, netcovSessionIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusForbidden)
		if !strings.Contains(rec.Body.String(), "enrol") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("a stale ceremony is refused", func(t *testing.T) {
		a, _ := netcovAPI(t)
		// Default session row carries the 2026-01-02 fixture stamp: stale.
		rec := httptest.NewRecorder()
		a.RequestExternalEndpointGrant(rec, netcovRequest(t, http.MethodPost, "/g", grant, netcovSessionIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusForbidden)
		if !strings.Contains(rec.Body.String(), "re-authentication") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("endpoint gone", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetExternalEndpointByUUID ", noRows: true})
		rec := httptest.NewRecorder()
		a.RequestExternalEndpointGrant(rec, netcovRequest(t, http.MethodPost, "/g", grant, netcovSessionIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNotFound)
	})
}

func TestNetcovRequestGrantRenewsALiveGrant(t *testing.T) {
	a, db := netcovAPI(t)
	netcovFreshSessionRow(db)
	// The endpoint is sensitive so the renewal notifies the team channel.
	db.rule(netcovRule{match: "-- name: GetExternalEndpointByUUID ", typed: []any{store.ExternalEndpointCriticalitySensitive}})
	rec := httptest.NewRecorder()
	a.RequestExternalEndpointGrant(rec, netcovRequest(t, http.MethodPost, "/g",
		api.ExternalEndpointGrantCreate{Reason: "renewing", DurationMinutes: 9999}, netcovSessionIdentity()), fixtureUUID)
	netcovStatus(t, rec, http.StatusCreated)
	var out api.ExternalEndpointGrant
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
}

func TestNetcovRequestGrantRenewalStoreFailure(t *testing.T) {
	a, db := netcovAPI(t)
	netcovFreshSessionRow(db)
	db.rule(netcovRule{match: "-- name: ExtendExternalEndpointGrant ", err: errors.New("down")})
	rec := httptest.NewRecorder()
	a.RequestExternalEndpointGrant(rec, netcovRequest(t, http.MethodPost, "/g",
		api.ExternalEndpointGrantCreate{Reason: "renewing", DurationMinutes: 5}, netcovSessionIdentity()), fixtureUUID)
	netcovStatus(t, rec, http.StatusInternalServerError)
}

func TestNetcovRequestGrantCreatesWhenNoneLive(t *testing.T) {
	t.Run("creation succeeds", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovFreshSessionRow(db)
		db.rule(netcovRule{match: "-- name: GetLiveExternalEndpointGrant ", noRows: true})
		db.rule(netcovRule{match: "-- name: GetExternalEndpointByUUID ", typed: []any{store.ExternalEndpointCriticalitySensitive}})
		rec := httptest.NewRecorder()
		a.RequestExternalEndpointGrant(rec, netcovRequest(t, http.MethodPost, "/g",
			api.ExternalEndpointGrantCreate{Reason: "first", DurationMinutes: 5}, netcovSessionIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusCreated)
	})
	t.Run("creation store failure", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovFreshSessionRow(db)
		db.rule(netcovRule{match: "-- name: GetLiveExternalEndpointGrant ", noRows: true})
		db.rule(netcovRule{match: "-- name: CreateExternalEndpointGrant ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.RequestExternalEndpointGrant(rec, netcovRequest(t, http.MethodPost, "/g",
			api.ExternalEndpointGrantCreate{Reason: "first", DurationMinutes: 5}, netcovSessionIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
}

func TestNetcovEndpointInScope(t *testing.T) {
	a, db := netcovAPI(t)
	r := netcovRequest(t, http.MethodGet, "/x", nil, nil)
	id := netcovIdentity()

	// No declared project: always in scope.
	if !a.endpointInScope(httptest.NewRecorder(), r, id, store.ExternalEndpoint{}) {
		t.Fatal("an unscoped endpoint must pass")
	}
	// Project of the caller's team: in scope (default fill answers TeamID 1).
	if !a.endpointInScope(httptest.NewRecorder(), r, id, store.ExternalEndpoint{ProjectID: ptr(int64(1))}) {
		t.Fatal("a same-team project must pass")
	}
	// Foreign team: 404.
	other := netcovIdentity()
	other.TeamID = 99
	rec := httptest.NewRecorder()
	if a.endpointInScope(rec, r, other, store.ExternalEndpoint{ProjectID: ptr(int64(1))}) {
		t.Fatal("a foreign-team project must be 'not found'")
	}
	netcovStatus(t, rec, http.StatusNotFound)
	// Lookup failure: 404 too.
	db.rule(netcovRule{match: "-- name: GetProjectByID ", err: errors.New("down")})
	rec = httptest.NewRecorder()
	if a.endpointInScope(rec, r, id, store.ExternalEndpoint{ProjectID: ptr(int64(1))}) {
		t.Fatal("a failing project lookup must be 'not found'")
	}
	netcovStatus(t, rec, http.StatusNotFound)
}

func TestNetcovExtendAndEndSessionsOfGrant(t *testing.T) {
	t.Run("extension pushes deadlines and logs write failures", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: SetPortForwardAuthorizedUntil ", err: errors.New("down")})
		a.extendSessionsOfGrant(netcovRequest(t, http.MethodGet, "/x", nil, nil), store.ExternalEndpointGrant{ID: 1})
	})
	t.Run("extension list failure returns quietly", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListLivePortForwardSessionsByGrant ", err: errors.New("down")})
		a.extendSessionsOfGrant(netcovRequest(t, http.MethodGet, "/x", nil, nil), store.ExternalEndpointGrant{ID: 1})
	})
	t.Run("extension success", func(t *testing.T) {
		a, _ := netcovAPI(t)
		a.extendSessionsOfGrant(netcovRequest(t, http.MethodGet, "/x", nil, nil), store.ExternalEndpointGrant{ID: 1})
	})
	t.Run("ending sessions warns on real errors only", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListLivePortForwardSessionsByGrant ", err: errors.New("down")})
		a.endSessionsOfGrant(netcovRequest(t, http.MethodGet, "/x", nil, nil), 1)
	})
	t.Run("ending sessions cuts live bridges", func(t *testing.T) {
		a, _ := netcovAPI(t)
		a.endSessionsOfGrant(netcovRequest(t, http.MethodGet, "/x", nil, nil), 1)
	})
}

func TestNetcovNotifyGrant(t *testing.T) {
	a, _ := netcovAPI(t)
	id := netcovIdentity()
	r := netcovRequest(t, http.MethodGet, "/x", nil, nil)
	sensitive := store.ExternalEndpoint{Criticality: store.ExternalEndpointCriticalitySensitive, Name: "prod"}
	standard := store.ExternalEndpoint{Criticality: store.ExternalEndpointCriticalityStandard}
	grant := store.ExternalEndpointGrant{
		Reason: "x", Factor: "passkey",
		ExpiresAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}

	a.notifyGrant(r, id, standard, grant, false)  // silent: not sensitive
	a.notifyGrant(r, id, sensitive, grant, false) // outboxed
	a.notifyGrant(r, id, sensitive, grant, true)  // renewed event type
}

func TestNetcovRevokeGrantBranches(t *testing.T) {
	t.Run("unknown grant 404s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetExternalEndpointGrantByUUID ", noRows: true})
		rec := httptest.NewRecorder()
		a.RevokeExternalEndpointGrant(rec, netcovRequest(t, http.MethodDelete, "/g", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNotFound)
	})
	t.Run("foreign team 404s", func(t *testing.T) {
		a, _ := netcovAPI(t)
		id := netcovIdentity()
		id.TeamID = 99
		rec := httptest.NewRecorder()
		a.RevokeExternalEndpointGrant(rec, netcovRequest(t, http.MethodDelete, "/g", nil, id), fixtureUUID)
		netcovStatus(t, rec, http.StatusNotFound)
	})
	t.Run("already revoked answers 204", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: RevokeExternalEndpointGrant ", noRows: true})
		rec := httptest.NewRecorder()
		a.RevokeExternalEndpointGrant(rec, netcovRequest(t, http.MethodDelete, "/g", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNoContent)
	})
	t.Run("revocation tears sessions down", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.RevokeExternalEndpointGrant(rec, netcovRequest(t, http.MethodDelete, "/g", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNoContent)
	})
	t.Run("bad uuid is not found", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.RevokeExternalEndpointGrant(rec, netcovRequest(t, http.MethodDelete, "/g", nil, netcovIdentity()), "not-a-uuid")
		netcovStatus(t, rec, http.StatusNotFound)
	})
}

func TestNetcovListGrantsBranches(t *testing.T) {
	t.Run("bad limit", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.ListExternalEndpointGrants(rec, netcovRequest(t, http.MethodGet, "/g", nil, netcovIdentity()), fixtureUUID,
			api.ListExternalEndpointGrantsParams{Limit: ptr(101)})
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("bad cursor", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.ListExternalEndpointGrants(rec, netcovRequest(t, http.MethodGet, "/g", nil, netcovIdentity()), fixtureUUID,
			api.ListExternalEndpointGrantsParams{Cursor: ptr("###")})
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("store failure", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListExternalEndpointGrantsPage ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.ListExternalEndpointGrants(rec, netcovRequest(t, http.MethodGet, "/g", nil, netcovIdentity()), fixtureUUID,
			api.ListExternalEndpointGrantsParams{})
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
}

func TestNetcovMintEndpointPortForward(t *testing.T) {
	t.Run("standard endpoint mints without a grant", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetExternalEndpointByUUID ", typed: []any{store.ExternalEndpointCriticalityStandard}})
		rec := httptest.NewRecorder()
		a.CreateExternalEndpointPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusCreated)
		var out api.PortForwardSession
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.AuthorizedUntil == nil {
			t.Fatal("a standard mint must announce the default deadline")
		}
	})
	t.Run("sensitive endpoint with a live grant mints with the grant deadline", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetExternalEndpointByUUID ", typed: []any{store.ExternalEndpointCriticalitySensitive}})
		rec := httptest.NewRecorder()
		a.CreateExternalEndpointPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusCreated)
		if !strings.Contains(rec.Body.String(), "authorized_until") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("sensitive endpoint without a grant polls", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetExternalEndpointByUUID ", typed: []any{store.ExternalEndpointCriticalitySensitive}})
		db.rule(netcovRule{match: "-- name: GetLiveExternalEndpointGrant ", noRows: true})
		rec := httptest.NewRecorder()
		a.CreateExternalEndpointPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusForbidden)
		if !strings.Contains(rec.Body.String(), codeAccessRequestRequired) {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("a creatorless token is refused finally", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetExternalEndpointByUUID ", typed: []any{store.ExternalEndpointCriticalitySensitive}})
		id := netcovIdentity()
		id.UserID = nil
		rec := httptest.NewRecorder()
		a.CreateExternalEndpointPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", nil, id), fixtureUUID)
		netcovStatus(t, rec, http.StatusForbidden)
		if strings.Contains(rec.Body.String(), codeAccessRequestRequired) {
			t.Fatalf("the CLI polls on this code; body = %s", rec.Body.String())
		}
	})
	t.Run("count failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetExternalEndpointByUUID ", typed: []any{store.ExternalEndpointCriticalityStandard}})
		db.rule(netcovRule{match: "-- name: CountOpenPortForwardSessions ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CreateExternalEndpointPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("team cap 409s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetExternalEndpointByUUID ", typed: []any{store.ExternalEndpointCriticalityStandard}})
		db.rule(netcovRule{match: "-- name: CountOpenPortForwardSessions ", set: map[int]any{0: int64(portForwardTeamCap)}})
		rec := httptest.NewRecorder()
		a.CreateExternalEndpointPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("create failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetExternalEndpointByUUID ", typed: []any{store.ExternalEndpointCriticalityStandard}})
		db.rule(netcovRule{match: "-- name: CreateEndpointPortForwardSession ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CreateExternalEndpointPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
}

func TestNetcovIntOrDefaultTreatsZeroAsAbsent(t *testing.T) {
	if got := intOrDefault(ptr(0), 240); got != 240 {
		t.Fatalf("zero = %d, want the fallback", got)
	}
	if got := intOrDefault(ptr(7), 240); got != 7 {
		t.Fatalf("explicit = %d, want 7", got)
	}
	if got := intOrDefault(nil, 240); got != 240 {
		t.Fatalf("nil = %d, want the fallback", got)
	}
}

// ---------------------------------------------------------------------------
// tunnelsessions.go
// ---------------------------------------------------------------------------

func TestNetcovTunnelPresenceWaitExpires(t *testing.T) {
	var p TunnelPresence
	bridge := p.register(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p.Wait(ctx) {
		t.Fatal("Wait must report failure when the context expires with live bridges")
	}
	p.unregister(1, bridge)
	if !p.Wait(context.Background()) {
		t.Fatal("Wait must succeed once every bridge unregistered")
	}
}

func TestNetcovListPortForwardSessionsBranches(t *testing.T) {
	t.Run("bad limit", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.ListPortForwardSessions(rec, netcovRequest(t, http.MethodGet, "/pf", nil, netcovIdentity()),
			api.ListPortForwardSessionsParams{Limit: ptr(0)})
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("bad cursor", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.ListPortForwardSessions(rec, netcovRequest(t, http.MethodGet, "/pf", nil, netcovIdentity()),
			api.ListPortForwardSessionsParams{Cursor: ptr("###")})
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("unknown endpoint filter 404s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetExternalEndpointByUUID ", noRows: true})
		rec := httptest.NewRecorder()
		a.ListPortForwardSessions(rec, netcovRequest(t, http.MethodGet, "/pf", nil, netcovIdentity()),
			api.ListPortForwardSessionsParams{ExternalEndpointUuid: ptr(fixtureUUID)})
		netcovStatus(t, rec, http.StatusNotFound)
	})
	t.Run("endpoint filter narrows the page", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.ListPortForwardSessions(rec, netcovRequest(t, http.MethodGet, "/pf", nil, netcovIdentity()),
			api.ListPortForwardSessionsParams{ExternalEndpointUuid: ptr(fixtureUUID), Active: ptr(false)})
		netcovStatus(t, rec, http.StatusOK)
	})
	t.Run("store failure", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListPortForwardSessionsPage ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.ListPortForwardSessions(rec, netcovRequest(t, http.MethodGet, "/pf", nil, netcovIdentity()),
			api.ListPortForwardSessionsParams{})
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
}

func TestNetcovPortForwardSessionToAPITargetKinds(t *testing.T) {
	a, db := netcovAPI(t)
	r := netcovRequest(t, http.MethodGet, "/pf", nil, nil)

	preview := store.ListPortForwardSessionsPageRow{PreviewID: ptr(int64(1))}
	if got := a.portForwardSessionToAPI(r, preview); got.TargetKind != api.PortForwardSessionInfoTargetKindPreview {
		t.Fatalf("preview kind = %q", got.TargetKind)
	}
	resource := store.ListPortForwardSessionsPageRow{ResourceID: ptr(int64(1))}
	if got := a.portForwardSessionToAPI(r, resource); got.TargetKind != api.PortForwardSessionInfoTargetKindApplication {
		t.Fatalf("resource kind = %q", got.TargetKind)
	}
	// A vanished resource reads unknown, not an error.
	db.rule(netcovRule{match: "-- name: GetResourceByID ", err: pgx.ErrNoRows})
	if got := a.portForwardSessionToAPI(r, resource); got.TargetKind != api.PortForwardSessionInfoTargetKindUnknown {
		t.Fatalf("gone resource kind = %q", got.TargetKind)
	}
	if got := a.portForwardSessionToAPI(r, store.ListPortForwardSessionsPageRow{}); got.TargetKind != api.PortForwardSessionInfoTargetKindUnknown {
		t.Fatalf("empty row kind = %q", got.TargetKind)
	}
}

func TestNetcovClosePortForwardSessionBranches(t *testing.T) {
	live := func() map[int]any {
		row := store.PortForwardSession{ID: 5, TeamID: 1, UserID: ptr(int64(1)), TargetName: "db", TargetPort: 5432}
		_ = row.Uuid.Scan(fixtureUUID)
		return netcovRowOf(row)
	}

	t.Run("bad uuid", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.ClosePortForwardSession(rec, netcovRequest(t, http.MethodDelete, "/pf/x", nil, netcovIdentity()), "junk")
		netcovStatus(t, rec, http.StatusNotFound)
	})
	t.Run("unknown session 404s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetPortForwardSessionByUUID ", noRows: true})
		rec := httptest.NewRecorder()
		a.ClosePortForwardSession(rec, netcovRequest(t, http.MethodDelete, "/pf/x", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNotFound)
	})
	t.Run("already ended answers 204", func(t *testing.T) {
		a, _ := netcovAPI(t) // default fill: EndedAt valid
		rec := httptest.NewRecorder()
		a.ClosePortForwardSession(rec, netcovRequest(t, http.MethodDelete, "/pf/x", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNoContent)
	})
	t.Run("owner closes a live session", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetPortForwardSessionByUUID ", set: live()})
		rec := httptest.NewRecorder()
		a.ClosePortForwardSession(rec, netcovRequest(t, http.MethodDelete, "/pf/x", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNoContent)
	})
	t.Run("a stranger with manage revokes", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetPortForwardSessionByUUID ", set: live()})
		id := netcovIdentity()
		id.UserID = ptr(int64(42))
		rec := httptest.NewRecorder()
		a.ClosePortForwardSession(rec, netcovRequest(t, http.MethodDelete, "/pf/x", nil, id), fixtureUUID)
		netcovStatus(t, rec, http.StatusNoContent)
	})
	t.Run("a stranger without manage is refused", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetPortForwardSessionByUUID ", set: live()})
		id := netcovIdentity()
		id.UserID = ptr(int64(42))
		id.Permissions = []string{string(auth.PermPortForwardsOpen)}
		rec := httptest.NewRecorder()
		a.ClosePortForwardSession(rec, netcovRequest(t, http.MethodDelete, "/pf/x", nil, id), fixtureUUID)
		netcovStatus(t, rec, http.StatusForbidden)
	})
}

// ---------------------------------------------------------------------------
// ingresssessions.go
// ---------------------------------------------------------------------------

func TestNetcovListIngressSessionsBranches(t *testing.T) {
	t.Run("bad limit", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.ListIngressTunnelSessions(rec, netcovRequest(t, http.MethodGet, "/is", nil, netcovIdentity()),
			api.ListIngressTunnelSessionsParams{Limit: ptr(0)})
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("bad cursor", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.ListIngressTunnelSessions(rec, netcovRequest(t, http.MethodGet, "/is", nil, netcovIdentity()),
			api.ListIngressTunnelSessionsParams{Cursor: ptr("###")})
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("unknown endpoint filter 404s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetIngressEndpointByUUID ", noRows: true})
		rec := httptest.NewRecorder()
		a.ListIngressTunnelSessions(rec, netcovRequest(t, http.MethodGet, "/is", nil, netcovIdentity()),
			api.ListIngressTunnelSessionsParams{IngressEndpointUuid: ptr(fixtureUUID)})
		netcovStatus(t, rec, http.StatusNotFound)
	})
	t.Run("endpoint filter narrows the page", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.ListIngressTunnelSessions(rec, netcovRequest(t, http.MethodGet, "/is", nil, netcovIdentity()),
			api.ListIngressTunnelSessionsParams{IngressEndpointUuid: ptr(fixtureUUID), Active: ptr(false)})
		netcovStatus(t, rec, http.StatusOK)
	})
	t.Run("store failure", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListIngressSessionsPage ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.ListIngressTunnelSessions(rec, netcovRequest(t, http.MethodGet, "/is", nil, netcovIdentity()),
			api.ListIngressTunnelSessionsParams{})
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
}

func TestNetcovIngressSessionActive(t *testing.T) {
	now := time.Now()
	ts := func(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

	cases := map[string]struct {
		row  store.ListIngressSessionsPageRow
		want bool
	}{
		"ended":                 {store.ListIngressSessionsPageRow{EndedAt: ts(now)}, false},
		"unclaimed, redeemable": {store.ListIngressSessionsPageRow{TokenExpiresAt: ts(now.Add(time.Minute))}, true},
		"unclaimed, expired":    {store.ListIngressSessionsPageRow{TokenExpiresAt: ts(now.Add(-time.Minute))}, false},
		"claimed, never seen":   {store.ListIngressSessionsPageRow{ClaimedAt: ts(now)}, true},
		"claimed, fresh report": {store.ListIngressSessionsPageRow{ClaimedAt: ts(now), LastSeenAt: ts(now)}, true},
		"claimed, stale report": {store.ListIngressSessionsPageRow{ClaimedAt: ts(now), LastSeenAt: ts(now.Add(-2 * ingressSeenStaleAfter))}, false},
	}
	for name, tc := range cases {
		if got := ingressSessionActive(tc.row); got != tc.want {
			t.Errorf("%s: active = %v, want %v", name, got, tc.want)
		}
	}
}

func TestNetcovIngressSessionOwnedBy(t *testing.T) {
	one := int64(1)
	if ingressSessionOwnedBy(netcovIdentity(), &store.ListIngressSessionsPageRow{}) {
		t.Fatal("a session with no user is owned by nobody")
	}
	if !ingressSessionOwnedBy(netcovIdentity(), &store.ListIngressSessionsPageRow{UserID: &one}) {
		t.Fatal("the opener owns their session")
	}
	anon := &auth.Identity{}
	if ingressSessionOwnedBy(anon, &store.ListIngressSessionsPageRow{UserID: &one}) {
		t.Fatal("an identity without a user owns nothing")
	}
}

func TestNetcovCloseIngressSessionBranches(t *testing.T) {
	liveRow := func(userID int64) map[int]any {
		row := store.ListIngressSessionsPageRow{
			ID: 3, TeamID: 1, UserID: &userID,
			EndpointName: ptr("dev"), EndpointFqdn: ptr("dev.example.test"),
		}
		_ = row.Uuid.Scan(fixtureUUID)
		_ = row.EndpointUuid.Scan(fixtureUUID)
		return netcovRowOf(row)
	}

	t.Run("bad uuid", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CloseIngressTunnelSession(rec, netcovRequest(t, http.MethodDelete, "/is/x", nil, netcovIdentity()), "junk")
		netcovStatus(t, rec, http.StatusNotFound)
	})
	t.Run("list failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListIngressSessionsPage ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CloseIngressTunnelSession(rec, netcovRequest(t, http.MethodDelete, "/is/x", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("unknown session 404s", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CloseIngressTunnelSession(rec, netcovRequest(t, http.MethodDelete, "/is/x", nil, netcovIdentity()), netcovOtherUUID)
		netcovStatus(t, rec, http.StatusNotFound)
	})
	t.Run("already ended answers 204", func(t *testing.T) {
		a, _ := netcovAPI(t) // default fill: EndedAt valid
		rec := httptest.NewRecorder()
		a.CloseIngressTunnelSession(rec, netcovRequest(t, http.MethodDelete, "/is/x", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNoContent)
	})
	t.Run("owner closes a live session", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListIngressSessionsPage ", set: liveRow(1)})
		rec := httptest.NewRecorder()
		a.CloseIngressTunnelSession(rec, netcovRequest(t, http.MethodDelete, "/is/x", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNoContent)
	})
	t.Run("a stranger with manage revokes", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListIngressSessionsPage ", set: liveRow(42)})
		rec := httptest.NewRecorder()
		a.CloseIngressTunnelSession(rec, netcovRequest(t, http.MethodDelete, "/is/x", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNoContent)
	})
	t.Run("a stranger without manage is refused", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListIngressSessionsPage ", set: liveRow(42)})
		id := netcovIdentity()
		id.Permissions = []string{string(auth.PermIngressTunnelsOpen)}
		rec := httptest.NewRecorder()
		a.CloseIngressTunnelSession(rec, netcovRequest(t, http.MethodDelete, "/is/x", nil, id), fixtureUUID)
		netcovStatus(t, rec, http.StatusForbidden)
	})
	t.Run("finalize failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListIngressSessionsPage ", set: liveRow(1)})
		db.rule(netcovRule{match: "-- name: EndIngressSessionByUUID ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CloseIngressTunnelSession(rec, netcovRequest(t, http.MethodDelete, "/is/x", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
}

func TestNetcovApplyIngressObservation(t *testing.T) {
	ctx := context.Background()

	t.Run("malformed uuid is ignored", func(t *testing.T) {
		a, _ := netcovAPI(t)
		a.applyIngressObservation(ctx, 1, ingressObservation{Type: "ingress_claimed", SessionUUID: "junk"})
	})
	t.Run("claim stamps the row", func(t *testing.T) {
		a, _ := netcovAPI(t)
		a.applyIngressObservation(ctx, 1, ingressObservation{Type: "ingress_claimed", SessionUUID: fixtureUUID})
	})
	t.Run("alive touches a live row", func(t *testing.T) {
		a, _ := netcovAPI(t)
		a.applyIngressObservation(ctx, 1, ingressObservation{Type: "ingress_alive", SessionUUID: fixtureUUID})
	})
	t.Run("alive on a finalized row cuts the socket", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: TouchIngressSession ", tag: "UPDATE 0"})
		a.applyIngressObservation(ctx, 1, ingressObservation{Type: "ingress_alive", SessionUUID: fixtureUUID})
	})
	t.Run("close records the agent's reason", func(t *testing.T) {
		a, _ := netcovAPI(t)
		a.applyIngressObservation(ctx, 1, ingressObservation{Type: "ingress_closed", SessionUUID: fixtureUUID, State: "idle_timeout"})
	})
	t.Run("close without a reason reads disconnect", func(t *testing.T) {
		a, _ := netcovAPI(t)
		a.applyIngressObservation(ctx, 1, ingressObservation{Type: "ingress_closed", SessionUUID: fixtureUUID})
	})
	t.Run("close on an unknown row is a no-op", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetOpenIngressSessionByUUID ", noRows: true})
		a.applyIngressObservation(ctx, 1, ingressObservation{Type: "ingress_closed", SessionUUID: fixtureUUID})
	})
}

// ---------------------------------------------------------------------------
// portforward.go mints (the WebSocket redeem lives in netcov_cov2_test.go)
// ---------------------------------------------------------------------------

func TestNetcovCreateApplicationPortForwardBranches(t *testing.T) {
	body := api.PortForwardCreate{Port: 5432}

	t.Run("bad body", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateApplicationPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", "{", netcovIdentity()), fixtureUUID,
			api.CreateApplicationPortForwardParams{})
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("port out of range", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateApplicationPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", api.PortForwardCreate{Port: 70000}, netcovIdentity()), fixtureUUID,
			api.CreateApplicationPortForwardParams{})
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("known component mints", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateApplicationPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()), fixtureUUID,
			api.CreateApplicationPortForwardParams{Component: ptr("unit")})
		netcovStatus(t, rec, http.StatusCreated)
	})
	t.Run("unknown component 404s", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateApplicationPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()), fixtureUUID,
			api.CreateApplicationPortForwardParams{Component: ptr("ghost")})
		netcovStatus(t, rec, http.StatusNotFound)
	})
	t.Run("component listing failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListServiceComponents ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CreateApplicationPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()), fixtureUUID,
			api.CreateApplicationPortForwardParams{Component: ptr("unit")})
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("session callers record their user", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovFreshSessionRow(db)
		rec := httptest.NewRecorder()
		a.CreateApplicationPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovSessionIdentity()), fixtureUUID,
			api.CreateApplicationPortForwardParams{})
		netcovStatus(t, rec, http.StatusCreated)
	})
	t.Run("cap reached 409s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CountOpenPortForwardSessions ", set: map[int]any{0: int64(portForwardTeamCap)}})
		rec := httptest.NewRecorder()
		a.CreateApplicationPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()), fixtureUUID,
			api.CreateApplicationPortForwardParams{})
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("count failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CountOpenPortForwardSessions ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CreateApplicationPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()), fixtureUUID,
			api.CreateApplicationPortForwardParams{})
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("create failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CreatePortForwardSession ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CreateApplicationPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()), fixtureUUID,
			api.CreateApplicationPortForwardParams{})
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
}

func TestNetcovCreateDatabasePortForwardBranches(t *testing.T) {
	body := api.PortForwardCreate{Port: 5432}

	t.Run("bad body", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateDatabasePortForward(rec, netcovRequest(t, http.MethodPost, "/pf", "{", netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("destination lookup failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetDestinationByID ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CreateDatabasePortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("mint succeeds", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateDatabasePortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusCreated)
	})
}

func TestNetcovCreatePreviewPortForwardBranches(t *testing.T) {
	body := api.PortForwardCreate{Port: 3000}

	t.Run("a destroyed preview 409s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetPreviewByUUIDForTeam ", typed: []any{store.PreviewStatusDestroyed}})
		rec := httptest.NewRecorder()
		a.CreatePreviewPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()), fixtureUUID, fixtureUUID,
			api.CreatePreviewPortForwardParams{})
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("bad body", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreatePreviewPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", "{", netcovIdentity()), fixtureUUID, fixtureUUID,
			api.CreatePreviewPortForwardParams{})
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("mint with a component", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreatePreviewPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()), fixtureUUID, fixtureUUID,
			api.CreatePreviewPortForwardParams{Component: ptr("unit")})
		netcovStatus(t, rec, http.StatusCreated)
	})
	t.Run("unknown component 404s", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreatePreviewPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()), fixtureUUID, fixtureUUID,
			api.CreatePreviewPortForwardParams{Component: ptr("ghost")})
		netcovStatus(t, rec, http.StatusNotFound)
	})
	t.Run("unknown preview 404s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetPreviewByUUIDForTeam ", noRows: true})
		rec := httptest.NewRecorder()
		a.CreatePreviewPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()), fixtureUUID, fixtureUUID,
			api.CreatePreviewPortForwardParams{})
		netcovStatus(t, rec, http.StatusNotFound)
	})
}

func TestNetcovEndPortForwardSessionWarnsOnStoreFailure(t *testing.T) {
	a, db := netcovAPI(t)
	db.rule(netcovRule{match: "-- name: EndPortForwardSession ", err: errors.New("down")})
	a.endPortForwardSession(store.PortForwardSession{ID: 1, TeamID: 1}, tunnel.EndDisconnect)

	// And the no-op close (another replica already finalized the row).
	a2, db2 := netcovAPI(t)
	db2.rule(netcovRule{match: "-- name: EndPortForwardSession ", tag: "UPDATE 0"})
	a2.endPortForwardSession(store.PortForwardSession{ID: 1, TeamID: 1}, tunnel.EndDisconnect)
}

// ---------------------------------------------------------------------------
// terminal.go mints (the WebSocket redeem lives in netcov_cov2_test.go)
// ---------------------------------------------------------------------------

func TestNetcovCreateApplicationTerminalSessionBranches(t *testing.T) {
	t.Run("plain mint", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateApplicationTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovIdentity()), fixtureUUID,
			api.CreateApplicationTerminalSessionParams{})
		netcovStatus(t, rec, http.StatusCreated)
	})
	t.Run("known component", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateApplicationTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovIdentity()), fixtureUUID,
			api.CreateApplicationTerminalSessionParams{Component: ptr("unit")})
		netcovStatus(t, rec, http.StatusCreated)
	})
	t.Run("unknown component 404s", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateApplicationTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovIdentity()), fixtureUUID,
			api.CreateApplicationTerminalSessionParams{Component: ptr("ghost")})
		netcovStatus(t, rec, http.StatusNotFound)
	})
	t.Run("component listing failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ListServiceComponents ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CreateApplicationTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovIdentity()), fixtureUUID,
			api.CreateApplicationTerminalSessionParams{Component: ptr("unit")})
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("cap reached 409s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CountOpenTerminalSessions ", set: map[int]any{0: int64(terminalTeamCap)}})
		rec := httptest.NewRecorder()
		a.CreateApplicationTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovIdentity()), fixtureUUID,
			api.CreateApplicationTerminalSessionParams{})
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("count failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CountOpenTerminalSessions ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CreateApplicationTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovIdentity()), fixtureUUID,
			api.CreateApplicationTerminalSessionParams{})
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("create failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CreateTerminalSession ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CreateApplicationTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovIdentity()), fixtureUUID,
			api.CreateApplicationTerminalSessionParams{})
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("session callers record their user", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovFreshSessionRow(db)
		rec := httptest.NewRecorder()
		a.CreateApplicationTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovSessionIdentity()), fixtureUUID,
			api.CreateApplicationTerminalSessionParams{})
		netcovStatus(t, rec, http.StatusCreated)
	})
}

func TestNetcovCreateDatabaseTerminalSessionBranches(t *testing.T) {
	t.Run("destination failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetDestinationByID ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CreateDatabaseTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("mint succeeds", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateDatabaseTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusCreated)
	})
}

func TestNetcovCreateServerTerminalSessionStepUp(t *testing.T) {
	t.Run("a root token passes", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateServerTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusCreated)
	})
	t.Run("a non-root token is refused", func(t *testing.T) {
		a, _ := netcovAPI(t)
		id := netcovIdentity()
		id.Permissions = []string{string(auth.PermTerminalRoot)}
		rec := httptest.NewRecorder()
		a.CreateServerTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, id), fixtureUUID)
		netcovStatus(t, rec, http.StatusForbidden)
	})
	t.Run("a session with a fresh passkey passes", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovFreshSessionRow(db)
		rec := httptest.NewRecorder()
		a.CreateServerTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovSessionIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusCreated)
	})
	t.Run("a stale step-up is refused", func(t *testing.T) {
		a, _ := netcovAPI(t) // default session row: 2026-01-02, stale
		rec := httptest.NewRecorder()
		a.CreateServerTerminalSession(rec, netcovRequest(t, http.MethodPost, "/ts", nil, netcovSessionIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusForbidden)
		if !strings.Contains(rec.Body.String(), "stepup_required") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("no cookie means no session", func(t *testing.T) {
		a, _ := netcovAPI(t)
		id := netcovSessionIdentity()
		r := netcovRequest(t, http.MethodPost, "/ts", nil, nil)
		r = r.WithContext(auth.WithIdentity(r.Context(), id))
		rec := httptest.NewRecorder()
		a.CreateServerTerminalSession(rec, r, fixtureUUID)
		netcovStatus(t, rec, http.StatusUnauthorized)
	})
}

func TestNetcovTerminalBoundsPreferConfiguredValues(t *testing.T) {
	a, _ := netcovAPI(t)
	a.TerminalIdleTimeout = 3 * time.Minute
	a.TerminalMaxDuration = 7 * time.Minute
	if a.terminalIdleTimeout() != 3*time.Minute || a.terminalMaxDuration() != 7*time.Minute {
		t.Fatal("configured bounds must win over the defaults")
	}
}

func TestNetcovTerminalGeometry(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/terminal/ws?cols=120&rows=40", nil)
	if c, rw := terminalGeometry(r); c != 120 || rw != 40 {
		t.Fatalf("geometry = %d×%d", c, rw)
	}
	r = httptest.NewRequest(http.MethodGet, "/terminal/ws?cols=0&rows=5000", nil)
	if c, rw := terminalGeometry(r); c != 80 || rw != 24 {
		t.Fatalf("out-of-range geometry = %d×%d, want the defaults", c, rw)
	}
	r = httptest.NewRequest(http.MethodGet, "/terminal/ws", nil)
	if c, rw := terminalGeometry(r); c != 80 || rw != 24 {
		t.Fatalf("absent geometry = %d×%d, want the defaults", c, rw)
	}
}

func TestNetcovEndTerminalSessionWarnsOnStoreFailure(t *testing.T) {
	a, db := netcovAPI(t)
	db.rule(netcovRule{match: "-- name: EndTerminalSession ", err: errors.New("down")})
	a.endTerminalSession(store.TerminalSession{ID: 1, TeamID: 1}, "disconnect")

	a2, db2 := netcovAPI(t)
	db2.rule(netcovRule{match: "-- name: EndTerminalSession ", tag: "UPDATE 0"})
	a2.endTerminalSession(store.TerminalSession{ID: 1, TeamID: 1}, "disconnect")
}
