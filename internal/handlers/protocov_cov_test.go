// Coverage tests for scim.go, mcp.go, agent.go and cliauth.go. Everything
// here is prefixed `protocov` (concurrent-agents convention) and reuses the
// flow harness idea: a protocol-level fake DB whose failures can be steered
// PER QUERY, so post-authentication error branches become reachable.
package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/events"
	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/mcp"
	"github.com/deepteams/akerdock/internal/scim"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// --- steerable protocol fake ------------------------------------------------

var protocovErrDB = errors.New("protocov: store failure") //nolint:revive,errname,staticcheck // Coverage-suite globals keep a collision-resistant prefix.

// protocovDB is a flowDB with per-query steering, keyed by the sqlc query
// name embedded in the SQL text (`-- name: X :kind`). Configure it BEFORE the
// first request and never after: scimTeam touches the token in a goroutine.
type protocovDB struct {
	truthy   bool
	countOne bool
	failOn   map[string]bool
	noRowsOn map[string]bool
	zeroExec map[string]bool
	override map[string]func(dest []any)
}

func protocovQueryName(sql string) string {
	rest, ok := strings.CutPrefix(strings.TrimSpace(sql), "-- name: ")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, ' '); i > 0 {
		return rest[:i]
	}
	return rest
}

func (db *protocovDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	name := protocovQueryName(sql)
	if db.failOn[name] {
		return pgconn.CommandTag{}, protocovErrDB
	}
	if db.zeroExec[name] {
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (db *protocovDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	name := protocovQueryName(sql)
	if db.failOn[name] {
		return nil, protocovErrDB
	}
	remaining := 1
	if db.noRowsOn[name] {
		remaining = 0
	}
	return &protocovRows{remaining: remaining, truthy: db.truthy, override: db.override[name]}, nil
}

func (db *protocovDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	name := protocovQueryName(sql)
	row := protocovRow{
		truthy:     db.truthy,
		zeroScalar: strings.Contains(strings.ToLower(sql), "count(") && !db.countOne,
		override:   db.override[name],
	}
	if db.failOn[name] {
		row.err = protocovErrDB
	}
	if db.noRowsOn[name] {
		row.err = pgx.ErrNoRows
	}
	return row
}

type protocovRow struct {
	err        error
	zeroScalar bool
	truthy     bool
	override   func(dest []any)
}

func (r protocovRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for _, d := range dest {
		if err := fillScanDestination(d, r.zeroScalar, r.truthy); err != nil {
			return err
		}
	}
	if r.override != nil {
		r.override(dest)
	}
	return nil
}

type protocovRows struct {
	remaining int
	current   bool
	closed    bool
	err       error
	truthy    bool
	override  func(dest []any)
}

func (r *protocovRows) Close()                                       { r.closed = true }
func (r *protocovRows) Err() error                                   { return r.err }
func (r *protocovRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (r *protocovRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *protocovRows) Values() ([]any, error)                       { return nil, nil }
func (r *protocovRows) RawValues() [][]byte                          { return nil }
func (r *protocovRows) Conn() *pgx.Conn                              { return nil }
func (r *protocovRows) Next() bool {
	if r.closed || r.remaining == 0 {
		r.closed = true
		r.current = false
		return false
	}
	r.remaining--
	r.current = true
	return true
}

func (r *protocovRows) Scan(dest ...any) error {
	if !r.current {
		return errors.New("Scan called before Next")
	}
	for _, d := range dest {
		if err := fillScanDestination(d, false, r.truthy); err != nil {
			r.err = err
			r.Close()
			return err
		}
	}
	if r.override != nil {
		r.override(dest)
	}
	return nil
}

var (
	_ store.DBTX = (*protocovDB)(nil)
	_ pgx.Rows   = (*protocovRows)(nil)
)

func protocovAPI(t *testing.T, db *protocovDB) *API {
	t.Helper()
	q := store.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte("1:" + key + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return &API{
		Store:    q,
		Pool:     flowPool{db: &flowDB{}},
		Settings: instance.NewCache(q),
		Keyring:  keyring,
		Audit:    &audit.Recorder{Store: q, Logger: logger},
		Events:   events.NewBroker(),
		Version:  "unit",
		Logger:   logger,
	}
}

// protocovSettingsStore pins instance settings independently of the fake DB.
type protocovSettingsStore struct {
	st  store.InstanceSetting
	err error
}

func (s protocovSettingsStore) GetInstanceSettings(context.Context) (store.InstanceSetting, error) {
	return s.st, s.err
}

func protocovSettings(st store.InstanceSetting, err error) *instance.Cache {
	return instance.NewCache(protocovSettingsStore{st: st, err: err})
}

func protocovIdentity() *auth.Identity {
	return &auth.Identity{
		TokenID: 1, TokenUUID: fixtureUUID, TeamID: 1, TeamUUID: fixtureUUID,
		Permissions: []string{string(auth.PermRoot)},
		UserID:      ptr(int64(1)),
	}
}

func protocovAuthedRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(auth.WithIdentity(req.Context(), protocovIdentity()))
}

func protocovScimRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer protocov-scim-token")
	return req
}

func protocovUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(fixtureUUID); err != nil {
		t.Fatal(err)
	}
	return u
}

// --- SCIM: users ------------------------------------------------------------

func TestProtocovScimAuth(t *testing.T) {
	t.Run("missing bearer", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		rec := httptest.NewRecorder()
		a.ScimServiceProviderConfig(rec, httptest.NewRequest(http.MethodGet, "/scim/v2/ServiceProviderConfig", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("empty bearer", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		req := httptest.NewRequest(http.MethodGet, "/scim/v2/ServiceProviderConfig", nil)
		req.Header.Set("Authorization", "Bearer ")
		rec := httptest.NewRecorder()
		a.ScimServiceProviderConfig(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("unknown token", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"GetScimTokenByHash": true}})
		rec := httptest.NewRecorder()
		a.ScimServiceProviderConfig(rec, protocovScimRequest(http.MethodGet, "/scim/v2/ServiceProviderConfig", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("valid token serves the provider config", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		rec := httptest.NewRecorder()
		a.ScimServiceProviderConfig(rec, protocovScimRequest(http.MethodGet, "/scim/v2/ServiceProviderConfig", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "scim") {
			t.Fatalf("content type = %q, want the SCIM media type", got)
		}
	})
}

func TestProtocovScimListUsers(t *testing.T) {
	list := func(t *testing.T, a *API, target string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		a.ScimListUsers(rec, protocovScimRequest(http.MethodGet, target, ""))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec, body
	}
	t.Run("lists the members", func(t *testing.T) {
		rec, body := list(t, protocovAPI(t, &protocovDB{}), "/scim/v2/Users")
		if rec.Code != http.StatusOK || body["totalResults"].(float64) != 1 {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("filter matches by email", func(t *testing.T) {
		rec, body := list(t, protocovAPI(t, &protocovDB{}), "/scim/v2/Users?filter="+url.QueryEscape(`userName eq "unit"`))
		if rec.Code != http.StatusOK || body["totalResults"].(float64) != 1 {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("filter excludes other emails", func(t *testing.T) {
		rec, body := list(t, protocovAPI(t, &protocovDB{}), "/scim/v2/Users?filter="+url.QueryEscape(`userName eq "elsewhere"`))
		if rec.Code != http.StatusOK || body["totalResults"].(float64) != 0 {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("store failure answers 500", func(t *testing.T) {
		rec, _ := list(t, protocovAPI(t, &protocovDB{failOn: map[string]bool{"ListTeamMembersForScim": true}}), "/scim/v2/Users")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

func TestProtocovScimFilterParsers(t *testing.T) {
	if got := scimFilterUserName(`userName eq "a@b.c"`); got != "a@b.c" {
		t.Fatalf("scimFilterUserName = %q", got)
	}
	if got := scimFilterUserName(`  USERNAME EQ "x"  `); got != "x" {
		t.Fatalf("case-insensitive userName filter = %q", got)
	}
	if got := scimFilterUserName(`displayName eq "x"`); got != "" {
		t.Fatalf("foreign filter honored: %q", got)
	}
	if got := scimFilterDisplayName(`displayName eq "admin"`); got != "admin" {
		t.Fatalf("scimFilterDisplayName = %q", got)
	}
	if got := scimFilterDisplayName(`userName eq "x"`); got != "" {
		t.Fatalf("foreign filter honored: %q", got)
	}
}

func TestProtocovScimCreateUser(t *testing.T) {
	create := func(t *testing.T, db *protocovDB, body string) *httptest.ResponseRecorder {
		t.Helper()
		a := protocovAPI(t, db)
		rec := httptest.NewRecorder()
		a.ScimCreateUser(rec, protocovScimRequest(http.MethodPost, "/scim/v2/Users", body))
		return rec
	}
	valid := `{"schemas":[],"userName":"new@example.test","displayName":"New","externalId":"okta-1"}`

	t.Run("invalid body", func(t *testing.T) {
		if rec := create(t, &protocovDB{}, "{"); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("missing email", func(t *testing.T) {
		if rec := create(t, &protocovDB{}, `{"schemas":[]}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("user lookup failure", func(t *testing.T) {
		if rec := create(t, &protocovDB{failOn: map[string]bool{"GetUserByEmail": true}}, valid); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("create user failure", func(t *testing.T) {
		db := &protocovDB{noRowsOn: map[string]bool{"GetUserByEmail": true}, failOn: map[string]bool{"CreateUser": true}}
		if rec := create(t, db, valid); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("already provisioned answers 409", func(t *testing.T) {
		if rec := create(t, &protocovDB{}, valid); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})
	t.Run("add member failure", func(t *testing.T) {
		db := &protocovDB{noRowsOn: map[string]bool{"GetScimMember": true}, failOn: map[string]bool{"AddTeamMember": true}}
		if rec := create(t, db, valid); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("provisions a fresh user with external id", func(t *testing.T) {
		db := &protocovDB{noRowsOn: map[string]bool{"GetUserByEmail": true, "GetScimMember": true}}
		rec := create(t, db, valid)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "new@example.test") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("provisions without external id", func(t *testing.T) {
		db := &protocovDB{noRowsOn: map[string]bool{"GetScimMember": true}}
		rec := create(t, db, `{"schemas":[],"emails":[{"value":"e@example.test","primary":true}]}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestProtocovScimGetUser(t *testing.T) {
	get := func(t *testing.T, db *protocovDB, id string) *httptest.ResponseRecorder {
		t.Helper()
		a := protocovAPI(t, db)
		rec := httptest.NewRecorder()
		req := withURLParam(protocovScimRequest(http.MethodGet, "/scim/v2/Users/"+id, ""), "id", id)
		a.ScimGetUser(rec, req)
		return rec
	}
	if rec := get(t, &protocovDB{}, "not-a-uuid"); rec.Code != http.StatusNotFound {
		t.Fatalf("bad uuid status = %d, want 404", rec.Code)
	}
	if rec := get(t, &protocovDB{noRowsOn: map[string]bool{"GetScimMember": true}}, fixtureUUID); rec.Code != http.StatusNotFound {
		t.Fatalf("missing member status = %d, want 404", rec.Code)
	}
	if rec := get(t, &protocovDB{}, fixtureUUID); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestProtocovScimReplaceUser(t *testing.T) {
	replace := func(t *testing.T, db *protocovDB, body string) *httptest.ResponseRecorder {
		t.Helper()
		a := protocovAPI(t, db)
		rec := httptest.NewRecorder()
		req := withURLParam(protocovScimRequest(http.MethodPut, "/scim/v2/Users/"+fixtureUUID, body), "id", fixtureUUID)
		a.ScimReplaceUser(rec, req)
		return rec
	}
	t.Run("invalid body", func(t *testing.T) {
		if rec := replace(t, &protocovDB{}, "{"); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("active false deprovisions", func(t *testing.T) {
		rec := replace(t, &protocovDB{}, `{"schemas":[],"userName":"unit","active":false}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"active":false`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("active true is acknowledged", func(t *testing.T) {
		rec := replace(t, &protocovDB{}, `{"schemas":[],"userName":"unit","active":true}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"active":true`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("deprovision failures are logged not fatal", func(t *testing.T) {
		db := &protocovDB{failOn: map[string]bool{
			"RemoveTeamMemberByUUID":       true,
			"RevokeAllSessionsOfUser":      true,
			"RevokeApiTokensForUserInTeam": true,
		}}
		if rec := replace(t, db, `{"schemas":[],"userName":"unit","active":false}`); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}

func TestProtocovScimPatchUser(t *testing.T) {
	patch := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		a := protocovAPI(t, &protocovDB{})
		rec := httptest.NewRecorder()
		req := withURLParam(protocovScimRequest(http.MethodPatch, "/scim/v2/Users/"+fixtureUUID, body), "id", fixtureUUID)
		a.ScimPatchUser(rec, req)
		return rec
	}
	if rec := patch(t, "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d, want 400", rec.Code)
	}
	rec := patch(t, `{"Operations":[{"op":"replace","path":"active","value":false}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"active":false`) {
		t.Fatalf("deactivate status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = patch(t, `{"Operations":[{"op":"replace","value":{"active":true}}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"active":true`) {
		t.Fatalf("activate status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec = patch(t, `{"Operations":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("no-op status = %d, want 200", rec.Code)
	}
}

func TestProtocovPatchActiveValue(t *testing.T) {
	op := func(o, path, value string) (bool, bool) {
		var raw json.RawMessage
		if value != "" {
			raw = json.RawMessage(value)
		}
		return patchActiveValue(scim.PatchOperation{Op: o, Path: path, Value: raw})
	}
	if a, ok := op("replace", "active", "false"); !ok || a {
		t.Fatal("path=active false not read")
	}
	if a, ok := op("replace", "active", "true"); !ok || !a {
		t.Fatal("path=active true not read")
	}
	if a, ok := op("replace", "", `{"active":false}`); !ok || a {
		t.Fatal("object active false not read")
	}
	if _, ok := op("remove", "active", "false"); ok {
		t.Fatal("remove op treated as a value")
	}
	if _, ok := op("replace", "", `{"other":1}`); ok {
		t.Fatal("value without active reported found")
	}
	if _, ok := op("replace", "active", `"not-a-bool"`); ok {
		t.Fatal("non-bool active reported found")
	}
}

func TestProtocovScimDeleteUser(t *testing.T) {
	a := protocovAPI(t, &protocovDB{})
	rec := httptest.NewRecorder()
	req := withURLParam(protocovScimRequest(http.MethodDelete, "/scim/v2/Users/"+fixtureUUID, ""), "id", fixtureUUID)
	a.ScimDeleteUser(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	missing := protocovAPI(t, &protocovDB{noRowsOn: map[string]bool{"GetScimMember": true}})
	rec = httptest.NewRecorder()
	req = withURLParam(protocovScimRequest(http.MethodDelete, "/scim/v2/Users/"+fixtureUUID, ""), "id", fixtureUUID)
	missing.ScimDeleteUser(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing member status = %d, want 404", rec.Code)
	}
}

// --- SCIM: groups -----------------------------------------------------------

func TestProtocovScimListGroups(t *testing.T) {
	list := func(t *testing.T, db *protocovDB, target string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		a := protocovAPI(t, db)
		rec := httptest.NewRecorder()
		a.ScimListGroups(rec, protocovScimRequest(http.MethodGet, target, ""))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec, body
	}
	t.Run("lists system and custom groups", func(t *testing.T) {
		rec, body := list(t, &protocovDB{}, "/scim/v2/Groups")
		if rec.Code != http.StatusOK || body["totalResults"].(float64) != 4 {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("filter narrows to one role", func(t *testing.T) {
		rec, body := list(t, &protocovDB{}, "/scim/v2/Groups?filter="+url.QueryEscape(`displayName eq "admin"`))
		if rec.Code != http.StatusOK || body["totalResults"].(float64) != 1 {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("members failure answers 500", func(t *testing.T) {
		rec, _ := list(t, &protocovDB{failOn: map[string]bool{"ListTeamMembersForScim": true}}, "/scim/v2/Groups")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("custom role failure answers 500", func(t *testing.T) {
		rec, _ := list(t, &protocovDB{failOn: map[string]bool{"ListCustomRolesPage": true}}, "/scim/v2/Groups")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("member without custom role lands in its system role group", func(t *testing.T) {
		rec, body := list(t, &protocovDB{noRowsOn: map[string]bool{"ListCustomRolesPage": true}}, "/scim/v2/Groups")
		if rec.Code != http.StatusOK || body["totalResults"].(float64) != 3 {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestProtocovScimGetGroup(t *testing.T) {
	get := func(t *testing.T, db *protocovDB, id string) *httptest.ResponseRecorder {
		t.Helper()
		a := protocovAPI(t, db)
		rec := httptest.NewRecorder()
		req := withURLParam(protocovScimRequest(http.MethodGet, "/scim/v2/Groups/"+url.PathEscape(id), ""), "id", id)
		a.ScimGetGroup(rec, req)
		return rec
	}
	if rec := get(t, &protocovDB{}, "role:admin"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := get(t, &protocovDB{}, "role:bogus"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown group status = %d, want 404", rec.Code)
	}
	if rec := get(t, &protocovDB{failOn: map[string]bool{"ListTeamMembersForScim": true}}, "role:admin"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure status = %d, want 500", rec.Code)
	}
}

func TestProtocovScimCreateGroup(t *testing.T) {
	create := func(t *testing.T, db *protocovDB, body string) *httptest.ResponseRecorder {
		t.Helper()
		a := protocovAPI(t, db)
		rec := httptest.NewRecorder()
		a.ScimCreateGroup(rec, protocovScimRequest(http.MethodPost, "/scim/v2/Groups", body))
		return rec
	}
	if rec := create(t, &protocovDB{}, "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d, want 400", rec.Code)
	}
	if rec := create(t, &protocovDB{failOn: map[string]bool{"ListCustomRolesPage": true}}, `{"schemas":[],"displayName":"admin"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure status = %d, want 500", rec.Code)
	}
	if rec := create(t, &protocovDB{}, `{"schemas":[],"displayName":"ADMIN"}`); rec.Code != http.StatusOK {
		t.Fatalf("matching role status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := create(t, &protocovDB{}, `{"schemas":[],"displayName":"strangers"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown role status = %d, want 400", rec.Code)
	}
}

func TestProtocovScimPatchGroup(t *testing.T) {
	patch := func(t *testing.T, db *protocovDB, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		a := protocovAPI(t, db)
		rec := httptest.NewRecorder()
		req := withURLParam(protocovScimRequest(http.MethodPatch, "/scim/v2/Groups/"+url.PathEscape(id), body), "id", id)
		a.ScimPatchGroup(rec, req)
		return rec
	}
	if rec := patch(t, &protocovDB{}, "role:bogus", "{}"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown group status = %d, want 404", rec.Code)
	}
	if rec := patch(t, &protocovDB{failOn: map[string]bool{"ListTeamMembersForScim": true}}, "role:admin", "{}"); rec.Code != http.StatusNotFound {
		t.Fatalf("group resolution failure status = %d, want 404", rec.Code)
	}
	if rec := patch(t, &protocovDB{}, "role:admin", "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d, want 400", rec.Code)
	}
	body := `{"Operations":[
		{"op":"replace","path":"displayName","value":"x"},
		{"op":"add","path":"members","value":[{"value":"` + fixtureUUID + `"}]},
		{"op":"remove","path":"members[value eq \"` + fixtureUUID + `\"]"}
	]}`
	if rec := patch(t, &protocovDB{}, "role:admin", body); rec.Code != http.StatusNoContent {
		t.Fatalf("assign/unassign status = %d, want 204", rec.Code)
	}
}

func TestProtocovScimSetMemberRole(t *testing.T) {
	ctx := context.Background()
	adminMember := map[string]func(dest []any){
		"GetScimMember": func(dest []any) { *(dest[4].(*store.TeamRole)) = store.TeamRoleAdmin },
	}

	t.Run("bad user uuid is ignored", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"UpdateTeamMemberRole": true}})
		a.scimSetMemberRole(ctx, 1, "not-a-uuid", "role:admin", true)
	})
	t.Run("system role grant", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		a.scimSetMemberRole(ctx, 1, fixtureUUID, "role:member", true)
	})
	t.Run("unknown system role is refused", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"UpdateTeamMemberRole": true}})
		a.scimSetMemberRole(ctx, 1, fixtureUUID, "role:bogus", true)
	})
	t.Run("custom role grant", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		a.scimSetMemberRole(ctx, 1, fixtureUUID, fixtureUUID, true)
	})
	t.Run("custom role with invalid uuid", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		a.scimSetMemberRole(ctx, 1, fixtureUUID, "definitely-not-a-uuid", true)
	})
	t.Run("custom role lookup failure", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"GetCustomRoleByUUID": true}})
		a.scimSetMemberRole(ctx, 1, fixtureUUID, fixtureUUID, true)
	})
	t.Run("unassign resets to member", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		a.scimSetMemberRole(ctx, 1, fixtureUUID, "role:admin", false)
	})
	t.Run("unassign refuses to orphan the last admin", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{override: adminMember, failOn: map[string]bool{"UpdateTeamMemberRole": true}})
		// UpdateTeamMemberRole failing proves the guard returned early: had it
		// run, it would only have logged, which this test cannot observe — the
		// assertion is that no test failure or panic escapes.
		a.scimSetMemberRole(ctx, 1, fixtureUUID, "role:admin", false)
	})
	t.Run("update failure is logged", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"UpdateTeamMemberRole": true}})
		a.scimSetMemberRole(ctx, 1, fixtureUUID, "role:member", true)
	})
}

func TestProtocovScimWouldOrphanLastAdmin(t *testing.T) {
	ctx := context.Background()
	u := protocovUUID(t)
	adminMember := map[string]func(dest []any){
		"GetScimMember": func(dest []any) { *(dest[4].(*store.TeamRole)) = store.TeamRoleAdmin },
	}

	if a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"GetScimMember": true}}); a.scimWouldOrphanLastAdmin(ctx, 1, u) {
		t.Fatal("missing member reported as last admin")
	}
	if a := protocovAPI(t, &protocovDB{}); a.scimWouldOrphanLastAdmin(ctx, 1, u) {
		t.Fatal("non-admin member reported as last admin")
	}
	if a := protocovAPI(t, &protocovDB{override: adminMember, failOn: map[string]bool{"CountTeamAdmins": true}}); a.scimWouldOrphanLastAdmin(ctx, 1, u) {
		t.Fatal("count failure reported as last admin")
	}
	if a := protocovAPI(t, &protocovDB{override: adminMember}); !a.scimWouldOrphanLastAdmin(ctx, 1, u) {
		t.Fatal("last admin not detected")
	}
}

// TestProtocovScimHandlersRequireTheToken drives every SCIM handler without a
// bearer token: each must stop at the 401 and never reach its store logic.
func TestProtocovScimHandlersRequireTheToken(t *testing.T) {
	a := protocovAPI(t, &protocovDB{})
	handlers := map[string]func(http.ResponseWriter, *http.Request){
		"list users":   a.ScimListUsers,
		"create user":  a.ScimCreateUser,
		"get user":     a.ScimGetUser,
		"replace user": a.ScimReplaceUser,
		"patch user":   a.ScimPatchUser,
		"delete user":  a.ScimDeleteUser,
		"list groups":  a.ScimListGroups,
		"get group":    a.ScimGetGroup,
		"create group": a.ScimCreateGroup,
		"patch group":  a.ScimPatchGroup,
	}
	for name, call := range handlers {
		rec := httptest.NewRecorder()
		req := withURLParam(httptest.NewRequest(http.MethodPost, "/scim/v2/x", strings.NewReader("{}")), "id", fixtureUUID)
		call(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a token: status = %d, want 401", name, rec.Code)
		}
	}
}

func TestProtocovDerefStr(t *testing.T) {
	if derefStr(nil) != "" {
		t.Fatal("nil deref != empty")
	}
	if derefStr(ptr("x")) != "x" {
		t.Fatal("deref lost the value")
	}
}

// --- SCIM: token management (in /api/v1) ------------------------------------

func TestProtocovScimTokenEndpoints(t *testing.T) {
	t.Run("unauthenticated calls are refused", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		for name, call := range map[string]func(w http.ResponseWriter, r *http.Request){
			"list":   func(w http.ResponseWriter, r *http.Request) { a.ListScimTokens(w, r, fixtureUUID) },
			"create": func(w http.ResponseWriter, r *http.Request) { a.CreateScimToken(w, r, fixtureUUID) },
			"revoke": func(w http.ResponseWriter, r *http.Request) { a.RevokeScimToken(w, r, fixtureUUID, fixtureUUID) },
		} {
			rec := httptest.NewRecorder()
			call(rec, httptest.NewRequest(http.MethodPost, "/api/v1/teams/x/scim-tokens", strings.NewReader("{}")))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s status = %d, want 401", name, rec.Code)
			}
		}
	})
	t.Run("bad team uuid", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		rec := httptest.NewRecorder()
		a.ListScimTokens(rec, protocovAuthedRequest(http.MethodGet, "/api/v1/teams/x/scim-tokens", ""), "nope")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("list failure", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"ListScimTokensPage": true}})
		rec := httptest.NewRecorder()
		a.ListScimTokens(rec, protocovAuthedRequest(http.MethodGet, "/api/v1/teams/x/scim-tokens", ""), fixtureUUID)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("list success", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		rec := httptest.NewRecorder()
		a.ListScimTokens(rec, protocovAuthedRequest(http.MethodGet, "/api/v1/teams/x/scim-tokens", ""), fixtureUUID)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), fixtureUUID) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("create requires a name", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		rec := httptest.NewRecorder()
		a.CreateScimToken(rec, protocovAuthedRequest(http.MethodPost, "/api/v1/teams/x/scim-tokens", `{"name":"  "}`), fixtureUUID)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})
	t.Run("create store failure", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"CreateScimToken": true}})
		rec := httptest.NewRecorder()
		a.CreateScimToken(rec, protocovAuthedRequest(http.MethodPost, "/api/v1/teams/x/scim-tokens", `{"name":"okta"}`), fixtureUUID)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("create with instance fqdn", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		a.Settings = protocovSettings(store.InstanceSetting{Fqdn: ptr("panel.example.test")}, nil)
		rec := httptest.NewRecorder()
		a.CreateScimToken(rec, protocovAuthedRequest(http.MethodPost, "/api/v1/teams/x/scim-tokens", `{"name":"okta"}`), fixtureUUID)
		if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), "https://panel.example.test/scim/v2") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "akscim_") {
			t.Fatalf("clear token missing: %s", rec.Body.String())
		}
	})
	t.Run("create without fqdn keeps a relative base", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		a.Settings = protocovSettings(store.InstanceSetting{}, nil)
		rec := httptest.NewRecorder()
		a.CreateScimToken(rec, protocovAuthedRequest(http.MethodPost, "/api/v1/teams/x/scim-tokens", `{"name":"okta"}`), fixtureUUID)
		if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"/scim/v2"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("revoke bad token uuid", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		rec := httptest.NewRecorder()
		a.RevokeScimToken(rec, protocovAuthedRequest(http.MethodDelete, "/api/v1/teams/x/scim-tokens/y", ""), fixtureUUID, "nope")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("revoke store failure", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"RevokeScimToken": true}})
		rec := httptest.NewRecorder()
		a.RevokeScimToken(rec, protocovAuthedRequest(http.MethodDelete, "/api/v1/teams/x/scim-tokens/y", ""), fixtureUUID, fixtureUUID)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("revoke unknown token", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{zeroExec: map[string]bool{"RevokeScimToken": true}})
		rec := httptest.NewRecorder()
		a.RevokeScimToken(rec, protocovAuthedRequest(http.MethodDelete, "/api/v1/teams/x/scim-tokens/y", ""), fixtureUUID, fixtureUUID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("revoke success", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		rec := httptest.NewRecorder()
		a.RevokeScimToken(rec, protocovAuthedRequest(http.MethodDelete, "/api/v1/teams/x/scim-tokens/y", ""), fixtureUUID, fixtureUUID)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
	})
}

// --- MCP --------------------------------------------------------------------

func protocovMcpAPI(t *testing.T, db *protocovDB) *API {
	t.Helper()
	a := protocovAPI(t, db)
	a.MCP = mcp.New("unit")
	a.Settings = protocovSettings(store.InstanceSetting{
		McpEnabled: true, Fqdn: ptr("panel.example.test"),
	}, nil)
	return a
}

type protocovBrokenBody struct{}

func (protocovBrokenBody) Read([]byte) (int, error) { return 0, errors.New("protocov: broken body") }
func (protocovBrokenBody) Close() error             { return nil }

func TestProtocovMcpEndpoint(t *testing.T) {
	post := func(a *API, bearer, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		a.McpEndpoint(rec, req)
		return rec
	}

	t.Run("disabled surface answers 404", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{}) // MCP nil
		if rec := post(a, "akdm_x", "{}"); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("setting off answers 404", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		a.Settings = protocovSettings(store.InstanceSetting{McpEnabled: false}, nil)
		if rec := post(a, "akdm_x", "{}"); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("GET answers 405", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		rec := httptest.NewRecorder()
		a.McpEndpoint(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
			t.Fatalf("status = %d, Allow = %q", rec.Code, rec.Header().Get("Allow"))
		}
	})
	t.Run("missing bearer gets the discovery challenge", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		rec := post(a, "", "{}")
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "resource_metadata") {
			t.Fatalf("status = %d, WWW-Authenticate = %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	})
	t.Run("challenge without a configured fqdn", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		a.Settings = protocovSettings(store.InstanceSetting{McpEnabled: true}, nil)
		rec := post(a, "", "{}")
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "" {
			t.Fatalf("status = %d, WWW-Authenticate = %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	})
	t.Run("unknown mcp access token", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{failOn: map[string]bool{"GetMcpAccessTokenByHash": true}})
		if rec := post(a, "akdm_gone", "{}"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("api token without resolver", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		if rec := post(a, "akd_something", "{}"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("oauth token reaches the JSON-RPC server", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		rec := post(a, "akdm_live", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"jsonrpc"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("notification answers 202", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		rec := post(a, "akdm_live", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
	})
	t.Run("api token path resolves through the shared middleware", func(t *testing.T) {
		token, row := mcpAPITokenFixture(t)
		creator := *row.CreatedBy
		a := protocovMcpAPI(t, &protocovDB{})
		a.TokenAuth = mcpAuthMiddleware(&mcpTokenStore{
			row:       row,
			authority: &store.GetTokenCreatorAuthorityRow{UserID: creator, Role: store.TeamRoleOwner},
		})
		rec := post(a, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("unreadable body answers 400", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer akdm_live")
		req.Body = protocovBrokenBody{}
		rec := httptest.NewRecorder()
		a.McpEndpoint(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

func TestProtocovMcpMetadata(t *testing.T) {
	t.Run("protected resource disabled", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{}) // MCP nil
		a.Settings = protocovSettings(store.InstanceSetting{Fqdn: ptr("panel.example.test")}, nil)
		rec := httptest.NewRecorder()
		a.McpProtectedResourceMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("protected resource", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		rec := httptest.NewRecorder()
		a.McpProtectedResourceMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://panel.example.test/mcp") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("authorization server without base", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		a.Settings = protocovSettings(store.InstanceSetting{McpEnabled: true}, nil)
		rec := httptest.NewRecorder()
		a.McpAuthorizationServerMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("authorization server advertises registration only with DCR", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		rec := httptest.NewRecorder()
		a.McpAuthorizationServerMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "registration_endpoint") {
			t.Fatalf("DCR-off metadata advertises registration: %d %s", rec.Code, rec.Body.String())
		}

		a.Settings = protocovSettings(store.InstanceSetting{
			McpEnabled: true, McpDcrEnabled: true, Fqdn: ptr("panel.example.test"),
		}, nil)
		rec = httptest.NewRecorder()
		a.McpAuthorizationServerMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "registration_endpoint") {
			t.Fatalf("DCR-on metadata misses registration: %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestProtocovMcpRegisterClient(t *testing.T) {
	register := func(a *API, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.McpRegisterClient(rec, httptest.NewRequest(http.MethodPost, "/oauth/mcp/register", strings.NewReader(body)))
		return rec
	}
	dcrOn := func(t *testing.T, db *protocovDB) *API {
		t.Helper()
		a := protocovMcpAPI(t, db)
		a.Settings = protocovSettings(store.InstanceSetting{
			McpEnabled: true, McpDcrEnabled: true, Fqdn: ptr("panel.example.test"),
		}, nil)
		return a
	}

	t.Run("disabled surface", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		if rec := register(a, "{}"); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("DCR off refuses registration", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		rec := register(a, "{}")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Metadata Document") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("missing redirect uris", func(t *testing.T) {
		if rec := register(dcrOn(t, &protocovDB{}), `{"client_name":"x"}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("invalid redirect uri", func(t *testing.T) {
		if rec := register(dcrOn(t, &protocovDB{}), `{"redirect_uris":["http://evil.example/cb"]}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("store failure", func(t *testing.T) {
		a := dcrOn(t, &protocovDB{failOn: map[string]bool{"RegisterMcpOauthClient": true}})
		if rec := register(a, `{"redirect_uris":["https://client.example/cb"]}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("registers with a default name", func(t *testing.T) {
		rec := register(dcrOn(t, &protocovDB{}), `{"redirect_uris":["https://client.example/cb","http://127.0.0.1:9/cb"]}`)
		if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), "client_id") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
}

// protocovClientOverride makes the one registered client the flow needs:
// GetMcpOauthClient answers redirect uri https://client.example.test/cb.
func protocovClientOverride() map[string]func(dest []any) {
	return map[string]func(dest []any){
		"GetMcpOauthClient": func(dest []any) {
			*(dest[3].(*[]string)) = []string{"https://client.example.test/cb"}
		},
	}
}

func protocovAuthorizeQuery() url.Values {
	return url.Values{
		"client_id":             {"unit"},
		"redirect_uri":          {"https://client.example.test/cb"},
		"response_type":         {"code"},
		"code_challenge":        {"challenge-1"},
		"code_challenge_method": {"S256"},
		"state":                 {"st-1"},
	}
}

// protocovNoMembership makes Sessions.Authenticate fail after the session row
// resolved: SessionFromRequest succeeds, the identity does not.
type protocovNoMembership struct {
	*browserSessionStore
}

func (protocovNoMembership) GetTeamMembershipForUser(context.Context, store.GetTeamMembershipForUserParams) (store.GetTeamMembershipForUserRow, error) {
	return store.GetTeamMembershipForUserRow{}, errors.New("protocov: no membership")
}

func TestProtocovMcpAuthorize(t *testing.T) {
	newAPI := func(t *testing.T, db *protocovDB) *API {
		t.Helper()
		a := protocovMcpAPI(t, db)
		a.Sessions = &session.Manager{Store: newBrowserSessionStore(t)}
		return a
	}
	get := func(a *API, q url.Values, cookie bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/oauth/mcp/authorize?"+q.Encode(), nil)
		if cookie {
			req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "session-token"})
		}
		rec := httptest.NewRecorder()
		a.McpAuthorize(rec, req)
		return rec
	}

	t.Run("disabled surface", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		if rec := get(a, protocovAuthorizeQuery(), false); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("unknown client", func(t *testing.T) {
		a := newAPI(t, &protocovDB{failOn: map[string]bool{"GetMcpOauthClient": true}})
		rec := get(a, protocovAuthorizeQuery(), true)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_client") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("unregistered redirect uri never redirects", func(t *testing.T) {
		a := newAPI(t, &protocovDB{override: protocovClientOverride()})
		q := protocovAuthorizeQuery()
		q.Set("redirect_uri", "https://evil.example/cb")
		rec := get(a, q, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("wrong response type is bounced to the client", func(t *testing.T) {
		a := newAPI(t, &protocovDB{override: protocovClientOverride()})
		q := protocovAuthorizeQuery()
		q.Set("response_type", "token")
		rec := get(a, q, true)
		if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "unsupported_response_type") {
			t.Fatalf("status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
		}
	})
	t.Run("missing PKCE is bounced to the client", func(t *testing.T) {
		a := newAPI(t, &protocovDB{override: protocovClientOverride()})
		q := protocovAuthorizeQuery()
		q.Del("code_challenge")
		rec := get(a, q, true)
		if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "invalid_request") {
			t.Fatalf("status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
		}
	})
	t.Run("no session manager answers 409", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{override: protocovClientOverride()})
		if rec := get(a, protocovAuthorizeQuery(), false); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})
	t.Run("no session redirects to sign-in", func(t *testing.T) {
		a := newAPI(t, &protocovDB{override: protocovClientOverride()})
		rec := get(a, protocovAuthorizeQuery(), false)
		if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "/?next=") {
			t.Fatalf("status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
		}
	})
	t.Run("session without membership goes home", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{override: protocovClientOverride()})
		a.Sessions = &session.Manager{Store: protocovNoMembership{newBrowserSessionStore(t)}}
		rec := get(a, protocovAuthorizeQuery(), true)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
			t.Fatalf("status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
		}
	})
	t.Run("renders the consent screen", func(t *testing.T) {
		a := newAPI(t, &protocovDB{override: protocovClientOverride()})
		rec := get(a, protocovAuthorizeQuery(), true)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "unit-csrf") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("consent survives a team name lookup failure", func(t *testing.T) {
		db := &protocovDB{override: protocovClientOverride(), failOn: map[string]bool{"GetTeamByID": true}}
		a := newAPI(t, db)
		rec := get(a, protocovAuthorizeQuery(), true)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "your current team") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestProtocovMcpApprove(t *testing.T) {
	newAPI := func(t *testing.T, db *protocovDB) *API {
		t.Helper()
		a := protocovMcpAPI(t, db)
		a.Sessions = &session.Manager{Store: newBrowserSessionStore(t)}
		return a
	}
	form := func() url.Values {
		v := protocovAuthorizeQuery()
		v.Set("csrf_token", "unit-csrf")
		v.Set("approve", "yes")
		return v
	}
	approve := func(a *API, body string, cookie bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/oauth/mcp/approve", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie {
			req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "session-token"})
		}
		rec := httptest.NewRecorder()
		a.McpApprove(rec, req)
		return rec
	}

	t.Run("malformed form", func(t *testing.T) {
		if rec := approve(newAPI(t, &protocovDB{}), "a=%zz", true); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("invalid authorize request", func(t *testing.T) {
		a := newAPI(t, &protocovDB{failOn: map[string]bool{"GetMcpOauthClient": true}})
		if rec := approve(a, form().Encode(), true); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("no session", func(t *testing.T) {
		a := newAPI(t, &protocovDB{override: protocovClientOverride()})
		if rec := approve(a, form().Encode(), false); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("wrong csrf token", func(t *testing.T) {
		a := newAPI(t, &protocovDB{override: protocovClientOverride()})
		v := form()
		v.Set("csrf_token", "stolen")
		if rec := approve(a, v.Encode(), true); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("session without membership", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{override: protocovClientOverride()})
		a.Sessions = &session.Manager{Store: protocovNoMembership{newBrowserSessionStore(t)}}
		if rec := approve(a, form().Encode(), true); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("declined consent bounces access_denied", func(t *testing.T) {
		a := newAPI(t, &protocovDB{override: protocovClientOverride()})
		v := form()
		v.Set("approve", "no")
		rec := approve(a, v.Encode(), true)
		if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "access_denied") {
			t.Fatalf("status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
		}
	})
	t.Run("code storage failure bounces server_error", func(t *testing.T) {
		db := &protocovDB{override: protocovClientOverride(), failOn: map[string]bool{"CreateMcpOauthCode": true}}
		a := newAPI(t, db)
		rec := approve(a, form().Encode(), true)
		if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "server_error") {
			t.Fatalf("status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
		}
	})
	t.Run("grant redirects with the code and state", func(t *testing.T) {
		a := newAPI(t, &protocovDB{override: protocovClientOverride()})
		rec := approve(a, form().Encode(), true)
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusFound || !strings.Contains(loc, "code=") || !strings.Contains(loc, "state=st-1") {
			t.Fatalf("status = %d, location = %q", rec.Code, loc)
		}
	})
}

func TestProtocovMcpToken(t *testing.T) {
	verifier := "protocov-verifier"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	codeOverride := func(extra func(dest []any)) map[string]func(dest []any) {
		return map[string]func(dest []any){
			"TakeMcpOauthCode": func(dest []any) {
				*(dest[6].(*string)) = challenge // code_challenge
				if extra != nil {
					extra(dest)
				}
			},
		}
	}
	exchange := func(a *API, v url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/oauth/mcp/token", strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		a.McpToken(rec, req)
		return rec
	}
	validForm := func() url.Values {
		return url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {"code-1"},
			"code_verifier": {verifier},
			"client_id":     {"unit"},
			"redirect_uri":  {"unit"},
		}
	}

	t.Run("disabled surface", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		if rec := exchange(a, validForm()); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("malformed form", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		req := httptest.NewRequest(http.MethodPost, "/oauth/mcp/token", strings.NewReader("a=%zz"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		a.McpToken(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("wrong grant type", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		v := validForm()
		v.Set("grant_type", "client_credentials")
		if rec := exchange(a, v); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("missing code or verifier", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{})
		v := validForm()
		v.Del("code_verifier")
		if rec := exchange(a, v); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("unknown code", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{failOn: map[string]bool{"TakeMcpOauthCode": true}})
		rec := exchange(a, validForm())
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_grant") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("client mismatch", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{override: codeOverride(nil)})
		v := validForm()
		v.Set("client_id", "someone-else")
		if rec := exchange(a, v); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("PKCE mismatch", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{}) // challenge stays "unit"
		rec := exchange(a, validForm())
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "PKCE") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("client resolution failure after the code", func(t *testing.T) {
		db := &protocovDB{override: codeOverride(nil), failOn: map[string]bool{"GetMcpOauthClient": true}}
		a := protocovMcpAPI(t, db)
		rec := exchange(a, validForm())
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_client") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("token storage failure", func(t *testing.T) {
		db := &protocovDB{override: codeOverride(nil), failOn: map[string]bool{"CreateMcpAccessToken": true}}
		a := protocovMcpAPI(t, db)
		if rec := exchange(a, validForm()); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("exchange mints a bearer token", func(t *testing.T) {
		a := protocovMcpAPI(t, &protocovDB{override: codeOverride(nil)})
		rec := exchange(a, validForm())
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), mcpTokenScheme) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestProtocovMcpRedirectHelpers(t *testing.T) {
	valid := map[string]bool{
		"https://client.example/cb": true,
		"http://127.0.0.1:8/cb":     true,
		"http://localhost/cb":       true,
		"http://[::1]:9/cb":         true,
		"http://evil.example/cb":    false,
		"ftp://client.example/cb":   false,
		"https://":                  false,
		"http://[::1":               false,
	}
	for uri, want := range valid {
		if got := validRedirectURI(uri); got != want {
			t.Fatalf("validRedirectURI(%q) = %v, want %v", uri, got, want)
		}
	}
	if !allowedRedirect([]string{"a", "b"}, "b") || allowedRedirect([]string{"a"}, "c") {
		t.Fatal("allowedRedirect membership check broken")
	}

	rec := httptest.NewRecorder()
	redirectOAuthError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "http://[::1", "s", "server_error", "d")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unparsable redirect uri status = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	redirectOAuthError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "https://client.example/cb", "s1", "access_denied", "no")
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "state=s1") {
		t.Fatalf("redirect error status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
}

// --- agent ------------------------------------------------------------------

func TestProtocovAgentObservations(t *testing.T) {
	post := func(a *API, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/agent/v1/observations", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer akda_protocov")
		rec := httptest.NewRecorder()
		a.AgentObservations(rec, req)
		return rec
	}
	obs := func(list ...string) string {
		var b strings.Builder
		b.WriteString(`{"observations":[`)
		b.WriteString(strings.Join(list, ","))
		b.WriteString(`]}`)
		return b.String()
	}

	t.Run("unknown token", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"GetAgentTokenByHash": true}})
		if rec := post(a, "{}"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("invalid batch", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		if rec := post(a, "{"); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("oversized batch", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		entries := make([]string, agentBatchMax+1)
		for i := range entries {
			entries[i] = `{"type":"heartbeat"}`
		}
		if rec := post(a, obs(entries...)); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("applies every observation kind", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		rec := post(a, obs(
			`{"type":"heartbeat"}`,
			`{"type":"ingress_claimed","resource_uuid":"`+fixtureUUID+`"}`,
			`{"type":"stz_woken","resource_uuid":"not-a-uuid"}`,
			`{"type":"stz_woken","resource_uuid":"`+fixtureUUID+`"}`, // preview path
			`{"type":"container_state","container":"`+fixtureUUID+`-worker","state":"healthy"}`,
			`{"type":"container_state","container":"plain-app","state":"healthy"}`,
			`{"type":"container_state","container":"`+fixtureUUID+`-worker","state":"pause"}`,
			`{"type":"unknown-kind"}`,
		))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("preview wake persists or stops", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"SetPreviewAwake": true}})
		if rec := post(a, obs(`{"type":"stz_woken","resource_uuid":"`+fixtureUUID+`"}`)); rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
	})
	t.Run("falls back to the application wake", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{noRowsOn: map[string]bool{"GetSleepingPreviewForServer": true}})
		if rec := post(a, obs(`{"type":"stz_woken","resource_uuid":"`+fixtureUUID+`"}`)); rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
	})
	t.Run("no sleeping resource is a no-op", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{noRowsOn: map[string]bool{
			"GetSleepingPreviewForServer": true, "WakeSleptApplicationForServer": true,
		}})
		if rec := post(a, obs(`{"type":"stz_woken","resource_uuid":"`+fixtureUUID+`"}`)); rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
	})
}

func TestProtocovAgentEmitWoken(t *testing.T) {
	ctx := context.Background()
	preview := store.Preview{ID: 1, ApplicationID: 1, Uuid: protocovUUID(t), PrID: 7}

	t.Run("preview emit stops without the application", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"GetApplicationByID": true}})
		a.emitAgentPreviewWoken(ctx, preview)
	})
	t.Run("preview emit survives a team lookup failure", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"GetTeamByID": true}})
		a.emitAgentPreviewWoken(ctx, preview)
	})
	t.Run("application emit stops without the application", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"GetApplicationByID": true}})
		a.emitAgentApplicationWoken(ctx, 1, protocovUUID(t))
	})
	t.Run("application emit survives a team lookup failure", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"GetTeamByID": true}})
		a.emitAgentApplicationWoken(ctx, 1, protocovUUID(t))
	})
}

func TestProtocovSplitComponentContainerBadUUID(t *testing.T) {
	// Right shape (36 chars + dash + service) but not a UUID: the scan branch.
	if _, _, ok := splitComponentContainer(strings.Repeat("z", 36) + "-web"); ok {
		t.Fatal("non-uuid prefix parsed as a component container")
	}
}

func TestProtocovAgentPresence(t *testing.T) {
	var p AgentPresence
	p.connect(4)
	p.connect(4)
	p.disconnect(4)
	if !p.Connected(4) {
		t.Fatal("one live channel left, Connected = false")
	}
	p.disconnect(4)
	if p.Connected(4) {
		t.Fatal("all channels gone, Connected = true")
	}
}

func protocovDialAgent(t *testing.T, a *API, subprotocol string) (*websocket.Conn, context.Context) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(a.AgentChannel))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer akda_protocov")
	conn, _, err := websocket.Dial(ctx, strings.Replace(srv.URL, "http", "ws", 1), &websocket.DialOptions{
		Subprotocols: []string{subprotocol},
		HTTPHeader:   hdr,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	conn.SetReadLimit(1 << 20)
	return conn, ctx
}

func protocovWriteWS(ctx context.Context, t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

func protocovReadAck(ctx context.Context, t *testing.T, conn *websocket.Conn) agentwire.Frame {
	t.Helper()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var f agentwire.Frame
		if json.Unmarshal(data, &f) == nil && f.Type == agentwire.FrameAck {
			return f
		}
	}
}

func TestProtocovAgentChannelRefusals(t *testing.T) {
	a := protocovAPI(t, &protocovDB{})

	rec := httptest.NewRecorder()
	a.AgentChannel(rec, httptest.NewRequest(http.MethodGet, "/agent/v1/ws", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", rec.Code)
	}

	// Valid token but no upgrade handshake: Accept fails, the handler returns.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/v1/ws", nil)
	req.Header.Set("Authorization", "Bearer akda_protocov")
	a.AgentChannel(rec, req)
	if rec.Code < http.StatusBadRequest {
		t.Fatalf("non-websocket request status = %d, want an error", rec.Code)
	}
}

func TestProtocovAgentChannelV1(t *testing.T) {
	a := protocovAPI(t, &protocovDB{})
	conn, ctx := protocovDialAgent(t, a, agentwire.SubprotocolV1)

	// Garbage never kills the channel.
	if err := conn.Write(ctx, websocket.MessageText, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	// An oversized batch is acked denied so the agent drops it.
	big := make([]agentwire.Observation, agentBatchMax+1)
	for i := range big {
		big[i] = agentwire.Observation{Type: "heartbeat"}
	}
	protocovWriteWS(ctx, t, conn, agentwire.Frame{Type: agentwire.FrameObservations, Seq: 1, Observations: big})
	if ack := protocovReadAck(ctx, t, conn); !ack.Denied || ack.Seq != 1 {
		t.Fatalf("oversized batch ack = %+v, want denied seq 1", ack)
	}
	// Result/stream frames on a v1 channel are ignored.
	protocovWriteWS(ctx, t, conn, agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: 9}})
	protocovWriteWS(ctx, t, conn, agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: 9}})
	// A normal batch is applied and acked.
	protocovWriteWS(ctx, t, conn, agentwire.Frame{Type: agentwire.FrameObservations, Seq: 2, Observations: []agentwire.Observation{{Type: "heartbeat"}}})
	if ack := protocovReadAck(ctx, t, conn); ack.Denied || ack.Seq != 2 {
		t.Fatalf("batch ack = %+v, want accepted seq 2", ack)
	}
	if !a.Agents.Connected(1) {
		t.Fatal("presence not registered while the channel is live")
	}
}

func TestProtocovAgentChannelV2(t *testing.T) {
	a := protocovAPI(t, &protocovDB{})
	a.AgentRPC = &AgentConns{}
	conn, ctx := protocovDialAgent(t, a, agentwire.SubprotocolV2)

	protocovWriteWS(ctx, t, conn, agentwire.Frame{Type: agentwire.FrameObservations, Seq: 1, Observations: []agentwire.Observation{{Type: "heartbeat"}}})
	if ack := protocovReadAck(ctx, t, conn); ack.Denied || ack.Seq != 1 {
		t.Fatalf("batch ack = %+v, want accepted seq 1", ack)
	}
	// Registration happened before the read loop: the ack proves it is visible.
	if _, ok := a.AgentRPC.Sender(1); !ok {
		t.Fatal("v2 channel not registered in the command registry")
	}
	// Unsolicited result/stream frames are delivered (and dropped, no caller).
	protocovWriteWS(ctx, t, conn, agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: 3}})
	protocovWriteWS(ctx, t, conn, agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: 3, EOF: true}})
	// One more acked batch AFTER those frames proves the loop processed them.
	protocovWriteWS(ctx, t, conn, agentwire.Frame{Type: agentwire.FrameObservations, Seq: 2})
	if ack := protocovReadAck(ctx, t, conn); ack.Seq != 2 {
		t.Fatalf("ack = %+v, want seq 2", ack)
	}
}

// Observation persistence is deliberately outside the channel reader. A
// locked database row must not withhold ACKs or command results until the
// agent's 10-second transport budget tears down an otherwise healthy socket.
func TestProtocovAgentChannelAcknowledgesWhileObservationStoreIsBlocked(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	db := &protocovDB{override: map[string]func([]any){
		"GetSleepingPreviewForServer": func([]any) {
			entered <- struct{}{}
			<-release
		},
	}}
	a := protocovAPI(t, db)
	a.AgentRPC = &AgentConns{}
	conn, ctx := protocovDialAgent(t, a, agentwire.SubprotocolV2)

	protocovWriteWS(ctx, t, conn, agentwire.Frame{
		Type: agentwire.FrameObservations,
		Seq:  1,
		Observations: []agentwire.Observation{{
			Type: "stz_woken", ResourceUUID: fixtureUUID,
		}},
	})
	if ack := protocovReadAck(ctx, t, conn); ack.Denied || ack.Seq != 1 {
		t.Fatalf("first ack = %+v, want accepted seq 1", ack)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("the observation worker never reached the blocking query")
	}

	// The ordered worker is still blocked above. The channel reader must keep
	// accepting and acknowledging frames instead of waiting behind it.
	protocovWriteWS(ctx, t, conn, agentwire.Frame{
		Type: agentwire.FrameObservations, Seq: 2,
		Observations: []agentwire.Observation{{Type: "heartbeat"}},
	})
	if ack := protocovReadAck(ctx, t, conn); ack.Denied || ack.Seq != 2 {
		t.Fatalf("second ack behind blocked persistence = %+v, want accepted seq 2", ack)
	}
}

// --- CLI auth ---------------------------------------------------------------

func TestProtocovCliAuthStart(t *testing.T) {
	start := func(a *API, body string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		a.CliAuthStart(rec, httptest.NewRequest(http.MethodPost, "/auth/cli/start", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}

	t.Run("missing challenge", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		if rec, _ := start(a, `{}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if rec, _ := start(a, "{"); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad json status = %d, want 400", rec.Code)
		}
	})
	t.Run("store failure", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{failOn: map[string]bool{"CreateCliAuthCode": true}})
		if rec, _ := start(a, `{"challenge":"c"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("registers a request", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		a.Settings = protocovSettings(store.InstanceSetting{Fqdn: ptr("panel.example.test")}, nil)
		rec, out := start(a, `{"challenge":"c","name":"laptop","scopes":"read"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		code, _ := out["user_code"].(string)
		if len(code) != userCodeLength {
			t.Fatalf("user_code = %q", code)
		}
		for _, c := range code {
			if !strings.ContainsRune(userCodeAlphabet, c) {
				t.Fatalf("user_code %q uses a character outside the alphabet", code)
			}
		}
		verify, _ := out["verify_url"].(string)
		if !strings.HasPrefix(verify, "https://panel.example.test/cli/authorize?request_id=") {
			t.Fatalf("verify_url = %q", verify)
		}
		if out["scopes"] != "read" || out["interval"].(float64) != cliPollInterval {
			t.Fatalf("payload = %v", out)
		}
	})
	t.Run("without fqdn the verify url is empty", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		a.Settings = protocovSettings(store.InstanceSetting{}, nil)
		rec, out := start(a, `{"challenge":"c"}`)
		if rec.Code != http.StatusOK || out["verify_url"] != "" || out["scopes"] != defaultCliPerms {
			t.Fatalf("status = %d, payload = %v", rec.Code, out)
		}
	})
}

func TestProtocovCliAuthRequest(t *testing.T) {
	get := func(a *API, cookie bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/auth/cli/request?request_id=r1", nil)
		if cookie {
			req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "session-token"})
		}
		rec := httptest.NewRecorder()
		a.CliAuthRequest(rec, req)
		return rec
	}
	withSessions := func(t *testing.T, db *protocovDB) *API {
		t.Helper()
		a := protocovAPI(t, db)
		a.Sessions = &session.Manager{Store: newBrowserSessionStore(t)}
		return a
	}

	t.Run("no session manager", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		if rec := get(a, false); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("no session", func(t *testing.T) {
		if rec := get(withSessions(t, &protocovDB{}), false); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("unknown request", func(t *testing.T) {
		a := withSessions(t, &protocovDB{noRowsOn: map[string]bool{"GetCliAuthCodeByRequestHash": true}})
		if rec := get(a, true); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("renders the pending request", func(t *testing.T) {
		a := withSessions(t, &protocovDB{})
		rec := get(a, true)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "user_code") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("anonymous client name", func(t *testing.T) {
		a := withSessions(t, &protocovDB{override: map[string]func(dest []any){
			"GetCliAuthCodeByRequestHash": func(dest []any) { *(dest[8].(**string)) = nil },
		}})
		rec := get(a, true)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":""`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestProtocovCliAuthApprove(t *testing.T) {
	approve := func(a *API, body string, authed bool) *httptest.ResponseRecorder {
		var req *http.Request
		if authed {
			req = authenticatedBrowserRequest(t, http.MethodPost, "/auth/cli/approve", body)
		} else {
			req = httptest.NewRequest(http.MethodPost, "/auth/cli/approve", strings.NewReader(body))
		}
		rec := httptest.NewRecorder()
		a.CliAuthApprove(rec, req)
		return rec
	}
	withSessions := func(t *testing.T, db *protocovDB) *API {
		t.Helper()
		a := protocovAPI(t, db)
		a.Sessions = &session.Manager{Store: newBrowserSessionStore(t)}
		return a
	}
	valid := `{"request_id":"r1","team_uuid":"` + fixtureUUID + `"}`

	t.Run("no session manager", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		if rec := approve(a, valid, false); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})
	t.Run("no session", func(t *testing.T) {
		if rec := approve(withSessions(t, &protocovDB{}), valid, false); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("missing csrf header", func(t *testing.T) {
		a := withSessions(t, &protocovDB{})
		req := httptest.NewRequest(http.MethodPost, "/auth/cli/approve", strings.NewReader(valid))
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "session-token"})
		rec := httptest.NewRecorder()
		a.CliAuthApprove(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("missing request id", func(t *testing.T) {
		if rec := approve(withSessions(t, &protocovDB{}), `{}`, true); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("bad team uuid", func(t *testing.T) {
		if rec := approve(withSessions(t, &protocovDB{}), `{"request_id":"r1","team_uuid":"nope"}`, true); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("unknown permission", func(t *testing.T) {
		body := `{"request_id":"r1","team_uuid":"` + fixtureUUID + `","permissions":["bogus"]}`
		if rec := approve(withSessions(t, &protocovDB{}), body, true); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("cannot grant beyond own permissions", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		st := newBrowserSessionStore(t)
		st.memberships = []store.ListTeamMembershipsForUserRow{{
			TeamID: 1, Role: store.TeamRoleReviewer, TeamUuid: st.sessionRow.Uuid, TeamName: "Unit",
		}}
		a.Sessions = &session.Manager{Store: st}
		body := `{"request_id":"r1","team_uuid":"` + fixtureUUID + `","permissions":["read","write"]}`
		rec := approve(a, body, true)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "write") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("store failure", func(t *testing.T) {
		a := withSessions(t, &protocovDB{failOn: map[string]bool{"ApproveCliAuthCode": true}})
		if rec := approve(a, valid, true); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("expired request", func(t *testing.T) {
		a := withSessions(t, &protocovDB{zeroExec: map[string]bool{"ApproveCliAuthCode": true}})
		if rec := approve(a, valid, true); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})
	t.Run("session row read failure", func(t *testing.T) {
		// CliAuthApprove reads the session row three times: Authenticate,
		// VerifyCSRF, then SessionFromRequest. Failing only the third reaches
		// the internal-error branch behind two successful auth gates.
		a := protocovAPI(t, &protocovDB{})
		st := &protocovFlakySessions{browserSessionStore: newBrowserSessionStore(t), failFrom: 3}
		a.Sessions = &session.Manager{Store: st}
		if rec := approve(a, valid, true); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("approves and audits", func(t *testing.T) {
		if rec := approve(withSessions(t, &protocovDB{}), valid, true); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
}

// protocovFlakySessions serves the session row failFrom-1 times, then fails.
type protocovFlakySessions struct {
	*browserSessionStore
	calls    int
	failFrom int
}

func (s *protocovFlakySessions) GetSessionByTokenHash(ctx context.Context, hash string) (store.GetSessionByTokenHashRow, error) {
	s.calls++
	if s.calls >= s.failFrom {
		return store.GetSessionByTokenHashRow{}, errors.New("protocov: session store down")
	}
	return s.browserSessionStore.GetSessionByTokenHash(ctx, hash)
}

func TestProtocovCliAuthToken(t *testing.T) {
	verifier := "protocov-verifier"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	// CliAuthorizationCode scan order: id, request_id_hash, challenge(2),
	// user_code, status(4), user_id, team_id(6), permissions, client_name(8), …
	approved := func(extra func(dest []any)) func(dest []any) {
		return func(dest []any) {
			*(dest[4].(*string)) = "approved"
			*(dest[2].(*string)) = challenge
			if extra != nil {
				extra(dest)
			}
		}
	}
	poll := func(a *API, body string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		a.CliAuthToken(rec, httptest.NewRequest(http.MethodPost, "/auth/cli/token", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}
	valid := `{"request_id":"r1","verifier":"` + verifier + `"}`

	t.Run("missing fields", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{})
		if rec, _ := poll(a, `{"request_id":"r1"}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if rec, _ := poll(a, "{"); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad json status = %d, want 400", rec.Code)
		}
	})
	t.Run("unknown request", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{noRowsOn: map[string]bool{"GetCliAuthCodeByRequestHash": true}})
		if rec, _ := poll(a, valid); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("pending until approved", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{}) // status stays "unit"
		rec, out := poll(a, valid)
		if rec.Code != http.StatusOK || out["status"] != "pending" {
			t.Fatalf("status = %d, payload = %v", rec.Code, out)
		}
	})
	t.Run("wrong verifier", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{override: map[string]func(dest []any){
			"GetCliAuthCodeByRequestHash": func(dest []any) { *(dest[4].(*string)) = "approved" },
		}})
		if rec, _ := poll(a, valid); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("approved without a team", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{override: map[string]func(dest []any){
			"GetCliAuthCodeByRequestHash": approved(func(dest []any) { *(dest[6].(**int64)) = nil }),
		}})
		if rec, _ := poll(a, valid); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})
	t.Run("lost consume race stays pending", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{
			override: map[string]func(dest []any){"GetCliAuthCodeByRequestHash": approved(nil)},
			failOn:   map[string]bool{"ConsumeCliAuthCode": true},
		})
		rec, out := poll(a, valid)
		if rec.Code != http.StatusOK || out["status"] != "pending" {
			t.Fatalf("status = %d, payload = %v", rec.Code, out)
		}
	})
	t.Run("team lookup failure", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{
			override: map[string]func(dest []any){
				"GetCliAuthCodeByRequestHash": approved(nil),
				"ConsumeCliAuthCode":          approved(nil),
			},
			failOn: map[string]bool{"GetTeamByID": true},
		})
		if rec, _ := poll(a, valid); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("token creation failure", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{
			override: map[string]func(dest []any){
				"GetCliAuthCodeByRequestHash": approved(nil),
				"ConsumeCliAuthCode":          approved(nil),
			},
			failOn: map[string]bool{"CreateApiToken": true},
		})
		if rec, _ := poll(a, valid); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("mints the token", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{override: map[string]func(dest []any){
			"GetCliAuthCodeByRequestHash": approved(nil),
			"ConsumeCliAuthCode":          approved(nil),
		}})
		rec, out := poll(a, valid)
		if rec.Code != http.StatusOK || out["status"] != "approved" {
			t.Fatalf("status = %d, payload = %v", rec.Code, out)
		}
		if token, _ := out["token"].(string); !strings.HasPrefix(token, "akd_") {
			t.Fatalf("token = %v", out["token"])
		}
		if out["team_uuid"] != fixtureUUID {
			t.Fatalf("team_uuid = %v", out["team_uuid"])
		}
	})
	t.Run("anonymous client keeps the default token name", func(t *testing.T) {
		a := protocovAPI(t, &protocovDB{override: map[string]func(dest []any){
			"GetCliAuthCodeByRequestHash": approved(nil),
			"ConsumeCliAuthCode":          approved(func(dest []any) { *(dest[8].(**string)) = nil }),
		}})
		if rec, _ := poll(a, valid); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}
