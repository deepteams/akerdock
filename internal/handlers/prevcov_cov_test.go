// Coverage tests for previews.go, previewauth.go, gitwebhooks.go and
// githubapps.go. They reuse the flow_test.go protocol fake, adding a thin
// per-query routing layer (prevcovDB) so a single query can be steered — a
// specific row, pgx.ErrNoRows, an error, a zero-rows command tag — while
// every other query keeps the flow defaults. GitHub itself is an httptest
// server; sqlc still performs every real Scan.
package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// ---------------------------------------------------------------------------
// Steerable per-query database fake
// ---------------------------------------------------------------------------

// prevcovRule steers ONE sqlc query (matched by its `-- name:` comment).
type prevcovRule struct {
	err      error
	errAfter int   // fail only from call N+1 on (0 = fail always when err is set)
	noRow    bool  // QueryRow answers pgx.ErrNoRows; Query answers zero rows
	rows     []any // fixtures scanned positionally into the row's fields
	tag      string

	mu    sync.Mutex
	calls int
}

// failing reports whether this call must answer the configured error.
func (r *prevcovRule) failing() bool {
	if r.err == nil {
		return false
	}
	if r.errAfter == 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls > r.errAfter
}

func (r *prevcovRule) called() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type prevcovDB struct {
	mu    sync.Mutex
	flow  *flowDB
	rules map[string]*prevcovRule
}

func prevcovQueryName(sql string) string {
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

func (db *prevcovDB) match(sql string) *prevcovRule {
	db.mu.Lock()
	rule := db.rules[prevcovQueryName(sql)]
	db.mu.Unlock()
	if rule != nil {
		rule.mu.Lock()
		rule.calls++
		rule.mu.Unlock()
	}
	return rule
}

func (db *prevcovDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if rule := db.match(sql); rule != nil {
		if rule.failing() {
			return pgconn.CommandTag{}, rule.err
		}
		tag := rule.tag
		if tag == "" {
			tag = "UPDATE 1"
		}
		return pgconn.NewCommandTag(tag), nil
	}
	return db.flow.Exec(ctx, sql, args...)
}

func (db *prevcovDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if rule := db.match(sql); rule != nil {
		if rule.failing() {
			return nil, rule.err
		}
		return &prevcovRows{fixtures: rule.rows}, nil
	}
	return db.flow.Query(ctx, sql, args...)
}

func (db *prevcovDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if rule := db.match(sql); rule != nil {
		if rule.failing() {
			return prevcovRow{err: rule.err}
		}
		if rule.noRow || len(rule.rows) == 0 {
			return prevcovRow{err: pgx.ErrNoRows}
		}
		rule.mu.Lock()
		idx := rule.calls - 1
		rule.mu.Unlock()
		if idx >= len(rule.rows) {
			idx = len(rule.rows) - 1
		}
		return prevcovRow{fixture: rule.rows[idx]}
	}
	return db.flow.QueryRow(ctx, sql, args...)
}

type prevcovRow struct {
	err     error
	fixture any
}

func (r prevcovRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return prevcovFillFixture(r.fixture, dest)
}

type prevcovRows struct {
	fixtures []any
	next     int
	current  any
	err      error
}

func (r *prevcovRows) Close()                                       {}
func (r *prevcovRows) Err() error                                   { return r.err }
func (r *prevcovRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (r *prevcovRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *prevcovRows) Values() ([]any, error)                       { return nil, nil }
func (r *prevcovRows) RawValues() [][]byte                          { return nil }
func (r *prevcovRows) Conn() *pgx.Conn                              { return nil }

func (r *prevcovRows) Next() bool {
	if r.next >= len(r.fixtures) {
		return false
	}
	r.current = r.fixtures[r.next]
	r.next++
	return true
}

func (r *prevcovRows) Scan(dest ...any) error {
	if err := prevcovFillFixture(r.current, dest); err != nil {
		r.err = err
		return err
	}
	return nil
}

// prevcovFillFixture copies a fixture into scan destinations: a scalar for a
// single destination, otherwise struct fields in declaration order — exactly
// the order sqlc generates its Scan calls in.
func prevcovFillFixture(fixture any, dest []any) error {
	v := reflect.ValueOf(fixture)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if len(dest) == 1 {
		d := reflect.ValueOf(dest[0])
		if d.Kind() == reflect.Pointer && !d.IsNil() && v.Type().AssignableTo(d.Elem().Type()) {
			d.Elem().Set(v)
			return nil
		}
	}
	if v.Kind() != reflect.Struct || v.NumField() != len(dest) {
		return fmt.Errorf("prevcov: fixture %T does not fit %d scan destinations", fixture, len(dest))
	}
	for i := range dest {
		d := reflect.ValueOf(dest[i])
		if d.Kind() != reflect.Pointer || d.IsNil() {
			return errors.New("prevcov: scan destination is not a non-nil pointer")
		}
		f := v.Field(i)
		if !f.Type().AssignableTo(d.Elem().Type()) {
			return fmt.Errorf("prevcov: fixture %T field %d (%s) is not assignable to %s",
				fixture, i, f.Type(), d.Elem().Type())
		}
		d.Elem().Set(f)
	}
	return nil
}

var (
	_ store.DBTX = (*prevcovDB)(nil)
	_ pgx.Rows   = (*prevcovRows)(nil)
)

// prevcovPool mirrors flowPool over the routing fake so transactional paths
// stay steerable too.
type prevcovPool struct {
	db store.DBTX
}

func (p prevcovPool) Begin(context.Context) (pgx.Tx, error) { return &prevcovTx{db: p.db}, nil }
func (prevcovPool) Ping(context.Context) error              { return nil }

type prevcovTx struct {
	db store.DBTX
}

func (t *prevcovTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (*prevcovTx) Commit(context.Context) error            { return nil }
func (*prevcovTx) Rollback(context.Context) error          { return nil }
func (*prevcovTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 1, nil
}
func (*prevcovTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return flowBatch{} }
func (*prevcovTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (*prevcovTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return &pgconn.StatementDescription{}, nil
}

func (t *prevcovTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}

func (t *prevcovTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}

func (t *prevcovTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (*prevcovTx) Conn() *pgx.Conn { return nil }

var _ pgx.Tx = (*prevcovTx)(nil)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

func prevcovAPI(t *testing.T, rules map[string]*prevcovRule) (*API, *prevcovDB) {
	t.Helper()
	a, flow := flowAPI(t)
	if rules == nil {
		rules = map[string]*prevcovRule{}
	}
	db := &prevcovDB{flow: flow, rules: rules}
	q := store.New(db)
	a.Store = q
	a.Pool = prevcovPool{db: db}
	a.Settings = instance.NewCache(q)
	a.Audit = &audit.Recorder{Store: q, Logger: a.Logger}
	return a, db
}

func prevcovIdentity() *auth.Identity {
	return &auth.Identity{
		TokenID: 1, TokenUUID: fixtureUUID, TeamID: 1, TeamUUID: fixtureUUID,
		Permissions:  []string{string(auth.PermRoot)},
		InstanceRoot: true,
		UserID:       ptr(int64(1)),
	}
}

func prevcovReq(method, target, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(auth.WithIdentity(req.Context(), prevcovIdentity()))
}

func prevcovChi(req *http.Request, params map[string]string) *http.Request {
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func prevcovUUIDv() pgtype.UUID {
	var u pgtype.UUID
	_ = u.Scan(fixtureUUID)
	return u
}

func prevcovTS() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true}
}

func prevcovPreview(mut func(*store.Preview)) store.Preview {
	p := store.Preview{
		ID: 1, Uuid: prevcovUUIDv(), ApplicationID: 1,
		Provider: store.GitProviderGithub, PrID: 7,
		SourceBranch: ptr("feat"), HeadSha: ptr("abc123"),
		Status:     store.PreviewStatusActive,
		Fqdn:       ptr("pr-7.previews.example.test"),
		RandomSlug: ptr("ab12cd"),
		CreatedAt:  prevcovTS(), UpdatedAt: prevcovTS(),
	}
	if mut != nil {
		mut(&p)
	}
	return p
}

var (
	prevcovRSAOnce sync.Once
	prevcovRSAPEM  []byte
)

func prevcovPEM(t *testing.T) []byte {
	t.Helper()
	prevcovRSAOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		prevcovRSAPEM = pem.EncodeToMemory(&pem.Block{
			Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
		})
	})
	return prevcovRSAPEM
}

func prevcovEncrypt(t *testing.T, a *API, table, column string, value []byte) []byte {
	t.Helper()
	enc, err := a.Keyring.Encrypt(table, column, fixtureUUID, value)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

const prevcovHookSecret = "prevcov-hook-secret"

// prevcovGithubApp is a converted, installed app whose private key decrypts
// to a real RSA PEM — enough for JWT minting against an httptest GitHub.
func prevcovGithubApp(t *testing.T, a *API, apiURL string, mut func(*store.GithubApp)) store.GithubApp {
	t.Helper()
	row := store.GithubApp{
		ID: 1, Uuid: prevcovUUIDv(), TeamID: 1, Name: "unit-app",
		AppID: ptr(int64(99)), Slug: ptr("unit-app"), InstallationID: ptr(int64(5)),
		ClientID:         ptr("client"),
		WebhookSecretEnc: prevcovEncrypt(t, a, "github_apps", "webhook_secret_enc", []byte(prevcovHookSecret)),
		AppPrivateKeyEnc: prevcovEncrypt(t, a, "github_apps", "app_private_key_enc", prevcovPEM(t)),
		ApiUrl:           apiURL, HtmlUrl: "https://github.example.test",
		CreatedAt: prevcovTS(), UpdatedAt: prevcovTS(), Version: 1,
	}
	if mut != nil {
		mut(&row)
	}
	return row
}

func prevcovServer(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(fn)
	t.Cleanup(srv.Close)
	return srv
}

// prevcovGithubOK answers the token mint and delegates the rest.
func prevcovGithubOK(rest http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"inst-token","expires_at":"2030-01-01T00:00:00Z"}`))
			return
		}
		if rest != nil {
			rest(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

func prevcovStatus(t *testing.T, rec *httptest.ResponseRecorder, want int, label string) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("%s: status = %d, want %d — body %s", label, rec.Code, want, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// previewauth.go
// ---------------------------------------------------------------------------

func TestPrevcovPreviewOwnsHost(t *testing.T) {
	cases := []struct {
		fqdn, host string
		want       bool
	}{
		{"", "x", false},
		{"x", "", false},
		{"pr-7.example.test", "PR-7.Example.Test", true},
		{"pr-7.example.test", "web-pr-7.example.test", true},
		{"pr-7.example.test", "evil.test", false},
	}
	for _, c := range cases {
		if got := previewOwnsHost(c.fqdn, c.host); got != c.want {
			t.Fatalf("previewOwnsHost(%q, %q) = %v, want %v", c.fqdn, c.host, got, c.want)
		}
	}
}

func TestPrevcovForwardedURL(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/webhooks/previews/forward-auth", nil)
	req.Header.Set("X-Forwarded-Host", "pr.example.test")
	host, u := forwardedURL(req)
	if host != "pr.example.test" || u.Path != "/" {
		t.Fatalf("forwardedURL = %q %q", host, u.String())
	}
	// An unparsable host falls back to a bare https URL.
	req2 := httptest.NewRequest(http.MethodGet, "/webhooks/previews/forward-auth", nil)
	req2.Header["X-Forwarded-Host"] = []string{"exa mple.test"}
	req2.Header.Set("X-Forwarded-Uri", "/x")
	_, u2 := forwardedURL(req2)
	if u2.Path != "/" {
		t.Fatalf("fallback URL path = %q, want /", u2.Path)
	}
}

func TestPrevcovPreviewForwardAuthReferenceErrors(t *testing.T) {
	a, _ := prevcovAPI(t, nil)

	rec := httptest.NewRecorder()
	a.PreviewForwardAuth(rec, prevcovReq(http.MethodGet, "/webhooks/previews/forward-auth?preview=not-a-uuid", ""))
	prevcovStatus(t, rec, http.StatusForbidden, "invalid preview reference")

	rec = httptest.NewRecorder()
	a.PreviewForwardAuth(rec, prevcovReq(http.MethodGet, "/webhooks/previews/forward-auth", ""))
	prevcovStatus(t, rec, http.StatusBadRequest, "missing preview reference")

	a2, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByHost": {err: pgx.ErrNoRows},
	})
	req := prevcovReq(http.MethodGet, "/webhooks/previews/forward-auth", "")
	req.Header.Set("X-Forwarded-Host", "unknown.example.test")
	rec = httptest.NewRecorder()
	a2.PreviewForwardAuth(rec, req)
	prevcovStatus(t, rec, http.StatusForbidden, "unknown preview host")
}

func TestPrevcovPreviewForwardAuthValidCookie(t *testing.T) {
	preview := prevcovPreview(nil)
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUID":            {rows: []any{preview}},
		"GetPreviewAccessTokenByHash": {rows: []any{store.PreviewAccessToken{ID: 1, TokenHash: "h", PreviewID: ptr(int64(1)), ExpiresAt: prevcovTS(), CreatedAt: prevcovTS()}}},
	})
	req := prevcovReq(http.MethodGet, "/webhooks/previews/forward-auth?preview="+fixtureUUID, "")
	req.Header.Set("X-Forwarded-Host", *preview.Fqdn)
	req.AddCookie(&http.Cookie{Name: previewCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	a.PreviewForwardAuth(rec, req)
	prevcovStatus(t, rec, http.StatusOK, "valid preview cookie")
}

func TestPrevcovPreviewForwardAuthWrongCookieNonNavigate(t *testing.T) {
	preview := prevcovPreview(nil)
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUID":            {rows: []any{preview}},
		"GetPreviewAccessTokenByHash": {rows: []any{store.PreviewAccessToken{ID: 1, TokenHash: "h", PreviewID: ptr(int64(2)), ExpiresAt: prevcovTS(), CreatedAt: prevcovTS()}}},
	})
	req := prevcovReq(http.MethodGet, "/webhooks/previews/forward-auth?preview="+fixtureUUID, "")
	req.Header.Set("X-Forwarded-Host", *preview.Fqdn)
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.AddCookie(&http.Cookie{Name: previewCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	a.PreviewForwardAuth(rec, req)
	prevcovStatus(t, rec, http.StatusUnauthorized, "fetch without valid cookie")
}

func TestPrevcovPreviewForwardAuthMissingInstanceFqdn(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUID":            {rows: []any{prevcovPreview(nil)}},
		"GetPreviewAccessTokenByHash": {err: pgx.ErrNoRows},
		"GetInstanceSettings":         {rows: []any{store.InstanceSetting{}}},
	})
	req := prevcovReq(http.MethodGet, "/webhooks/previews/forward-auth?preview="+fixtureUUID, "")
	req.AddCookie(&http.Cookie{Name: previewCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	a.PreviewForwardAuth(rec, req)
	prevcovStatus(t, rec, http.StatusForbidden, "no instance fqdn")
}

func TestPrevcovPreviewForwardAuthRedirectsToAuthorize(t *testing.T) {
	preview := prevcovPreview(nil)
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByHost":    {rows: []any{preview}},
		"GetInstanceSettings": {rows: []any{store.InstanceSetting{Fqdn: ptr("panel.example.test")}}},
	})
	// The forwarded host is NOT one of the preview's hosts: the redirect must
	// land on the preview's own fqdn, not on the rewritten host.
	req := prevcovReq(http.MethodGet, "/webhooks/previews/forward-auth", "")
	req.Header.Set("X-Forwarded-Host", "panel.example.test")
	req.Header.Set("X-Forwarded-Uri", "/deep/page?x=1")
	rec := httptest.NewRecorder()
	a.PreviewForwardAuth(rec, req)
	prevcovStatus(t, rec, http.StatusFound, "login redirect")
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Host != "panel.example.test" || loc.Path != "/webhooks/previews/authorize" {
		t.Fatalf("redirect landed on %s", loc.String())
	}
	redirect := loc.Query().Get("redirect")
	if !strings.Contains(redirect, *preview.Fqdn) {
		t.Fatalf("redirect %q does not target the preview fqdn", redirect)
	}
}

func TestPrevcovPreviewAuthorizeRejectsBadRedirects(t *testing.T) {
	a, _ := prevcovAPI(t, nil)
	for _, redirect := range []string{"", "http://pr.example.test/", "%zz"} {
		rec := httptest.NewRecorder()
		a.PreviewAuthorize(rec, prevcovReq(http.MethodGet, "/webhooks/previews/authorize?redirect="+url.QueryEscape(redirect), ""))
		prevcovStatus(t, rec, http.StatusBadRequest, "bad redirect "+redirect)
	}
}

func TestPrevcovPreviewAuthorizeUnknownHost(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByHost": {err: pgx.ErrNoRows},
	})
	rec := httptest.NewRecorder()
	a.PreviewAuthorize(rec, prevcovReq(http.MethodGet, "/webhooks/previews/authorize?redirect="+url.QueryEscape("https://x.example.test/"), ""))
	prevcovStatus(t, rec, http.StatusNotFound, "unknown preview host")
}

func TestPrevcovPreviewAuthorizeWithoutSessions(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByHost": {rows: []any{prevcovPreview(nil)}},
	})
	a.Sessions = nil
	rec := httptest.NewRecorder()
	a.PreviewAuthorize(rec, prevcovReq(http.MethodGet, "/webhooks/previews/authorize?redirect="+url.QueryEscape("https://pr-7.previews.example.test/"), ""))
	prevcovStatus(t, rec, http.StatusConflict, "sessions unavailable")
}

func TestPrevcovPreviewAuthorizeAnonymousGoesToLogin(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByHost": {rows: []any{prevcovPreview(nil)}},
	})
	a.Sessions = &session.Manager{Store: a.Store}
	rec := httptest.NewRecorder()
	a.PreviewAuthorize(rec, prevcovReq(http.MethodGet, "/webhooks/previews/authorize?redirect="+url.QueryEscape("https://pr-7.previews.example.test/"), ""))
	prevcovStatus(t, rec, http.StatusFound, "anonymous authorize")
	if rec.Header().Get("Location") != "/" {
		t.Fatalf("anonymous redirect = %q, want /", rec.Header().Get("Location"))
	}
}

func prevcovSessionReq(target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "session-token"})
	return req
}

func TestPrevcovPreviewAuthorizeApplicationGone(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByHost":   {rows: []any{prevcovPreview(nil)}},
		"GetApplicationByID": {err: pgx.ErrNoRows},
	})
	a.Sessions = &session.Manager{Store: a.Store}
	rec := httptest.NewRecorder()
	a.PreviewAuthorize(rec, prevcovSessionReq("/webhooks/previews/authorize?redirect="+url.QueryEscape("https://pr-7.previews.example.test/")))
	prevcovStatus(t, rec, http.StatusNotFound, "application gone")
}

func TestPrevcovPreviewAuthorizeForeignTeam(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByHost": {rows: []any{prevcovPreview(nil)}},
		"GetTeamMembershipForUser": {rows: []any{store.GetTeamMembershipForUserRow{
			TeamID: 2, Role: store.TeamRoleOwner, TeamUuid: prevcovUUIDv(), TeamName: "other",
		}}},
	})
	a.Sessions = &session.Manager{Store: a.Store}
	rec := httptest.NewRecorder()
	a.PreviewAuthorize(rec, prevcovSessionReq("/webhooks/previews/authorize?redirect="+url.QueryEscape("https://pr-7.previews.example.test/")))
	prevcovStatus(t, rec, http.StatusForbidden, "foreign team")
}

func TestPrevcovPreviewAuthorizeTokenPersistFailure(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByHost":         {rows: []any{prevcovPreview(nil)}},
		"CreatePreviewAccessToken": {err: errors.New("insert failed")},
	})
	a.Sessions = &session.Manager{Store: a.Store}
	rec := httptest.NewRecorder()
	a.PreviewAuthorize(rec, prevcovSessionReq("/webhooks/previews/authorize?redirect="+url.QueryEscape("https://pr-7.previews.example.test/")))
	prevcovStatus(t, rec, http.StatusInternalServerError, "token persist failure")
}

func TestPrevcovPreviewAuthorizeMintsCallback(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByHost": {rows: []any{prevcovPreview(nil)}},
	})
	a.Sessions = &session.Manager{Store: a.Store}
	rec := httptest.NewRecorder()
	a.PreviewAuthorize(rec, prevcovSessionReq("/webhooks/previews/authorize?redirect="+url.QueryEscape("https://pr-7.previews.example.test/deep?x=1")))
	prevcovStatus(t, rec, http.StatusFound, "authorize success")
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Host != "pr-7.previews.example.test" || loc.Path != "/.akerdock/preview-callback" {
		t.Fatalf("callback landed on %s", loc.String())
	}
	if loc.Query().Get("token") == "" {
		t.Fatal("callback carries no token")
	}
	if next := loc.Query().Get("next"); next != "/deep?x=1" {
		t.Fatalf("next = %q", next)
	}
}

func TestPrevcovPreviewCallback(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewAccessTokenByHash": {rows: []any{store.PreviewAccessToken{
			ID: 1, TokenHash: "h", PreviewID: ptr(int64(1)), ExpiresAt: prevcovTS(), CreatedAt: prevcovTS(),
		}}},
	})

	rec := httptest.NewRecorder()
	a.PreviewCallback(rec, prevcovReq(http.MethodGet, "/.akerdock/preview-callback", ""))
	prevcovStatus(t, rec, http.StatusBadRequest, "missing token")

	for next, want := range map[string]string{
		"/ok?a=1": "/ok?a=1",
		"":        "/",
		"//evil":  "/",
		"nope":    "/",
	} {
		rec = httptest.NewRecorder()
		a.PreviewCallback(rec, prevcovReq(http.MethodGet, "/.akerdock/preview-callback?token=tok&next="+url.QueryEscape(next), ""))
		prevcovStatus(t, rec, http.StatusFound, "callback next="+next)
		if got := rec.Header().Get("Location"); got != want {
			t.Fatalf("next %q redirected to %q, want %q", next, got, want)
		}
		cookies := rec.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Name != previewCookieName || cookies[0].Value != "tok" {
			t.Fatalf("callback cookie missing: %v", cookies)
		}
	}

	bad, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewAccessTokenByHash": {err: pgx.ErrNoRows},
	})
	rec = httptest.NewRecorder()
	bad.PreviewCallback(rec, prevcovReq(http.MethodGet, "/.akerdock/preview-callback?token=tok", ""))
	prevcovStatus(t, rec, http.StatusForbidden, "expired token")
}

// ---------------------------------------------------------------------------
// gitwebhooks.go
// ---------------------------------------------------------------------------

func prevcovEndpointRow(t *testing.T, a *API, mut func(*store.GetWebhookEndpointByUUIDRow)) store.GetWebhookEndpointByUUIDRow {
	t.Helper()
	row := store.GetWebhookEndpointByUUIDRow{
		ID: 1, Uuid: prevcovUUIDv(), ApplicationID: 1,
		Provider:  store.WebhookProviderGithub,
		SecretEnc: prevcovEncrypt(t, a, "webhook_endpoints", "secret_enc", []byte(prevcovHookSecret)),
		Enabled:   true, CreatedAt: prevcovTS(), UpdatedAt: prevcovTS(),
		TeamID: 1, ApplicationUuid: prevcovUUIDv(),
	}
	if mut != nil {
		mut(&row)
	}
	return row
}

func prevcovHookReq(provider, endpointUUID string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/"+provider+"/"+endpointUUID, bytes.NewReader(body))
	return prevcovChi(req, map[string]string{"provider": provider, "endpoint_uuid": endpointUUID})
}

func prevcovSignGithub(req *http.Request, body []byte, secret string) {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Delivery", "delivery-1")
	req.Header.Set("X-GitHub-Event", "push")
}

func TestPrevcovReceiveGitWebhookRouting(t *testing.T) {
	a, _ := prevcovAPI(t, nil)

	rec := httptest.NewRecorder()
	a.ReceiveGitWebhook(rec, prevcovHookReq("svn", fixtureUUID, []byte("{}")))
	prevcovStatus(t, rec, http.StatusNotFound, "unsupported provider")

	rec = httptest.NewRecorder()
	big := bytes.Repeat([]byte("a"), (2<<20)+16)
	a.ReceiveGitWebhook(rec, prevcovHookReq("github", fixtureUUID, big))
	prevcovStatus(t, rec, http.StatusRequestEntityTooLarge, "oversized delivery")

	rec = httptest.NewRecorder()
	a.ReceiveGitWebhook(rec, prevcovHookReq("github", "not-a-uuid", []byte("{}")))
	prevcovStatus(t, rec, http.StatusNotFound, "invalid endpoint uuid")

	unknown, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetWebhookEndpointByUUID": {err: pgx.ErrNoRows},
	})
	rec = httptest.NewRecorder()
	unknown.ReceiveGitWebhook(rec, prevcovHookReq("github", fixtureUUID, []byte("{}")))
	prevcovStatus(t, rec, http.StatusNotFound, "unknown endpoint")
}

func TestPrevcovReceiveGitWebhookProviderMismatch(t *testing.T) {
	a, db := prevcovAPI(t, nil)
	db.rules["GetWebhookEndpointByUUID"] = &prevcovRule{rows: []any{prevcovEndpointRow(t, a, nil)}}
	rec := httptest.NewRecorder()
	a.ReceiveGitWebhook(rec, prevcovHookReq("gitlab", fixtureUUID, []byte("{}")))
	prevcovStatus(t, rec, http.StatusNotFound, "provider mismatch")
}

func TestPrevcovReceiveGitWebhookSecretUndecryptable(t *testing.T) {
	a, db := prevcovAPI(t, nil)
	db.rules["GetWebhookEndpointByUUID"] = &prevcovRule{rows: []any{prevcovEndpointRow(t, a, func(r *store.GetWebhookEndpointByUUIDRow) {
		r.SecretEnc = []byte("garbage")
	})}}
	rec := httptest.NewRecorder()
	a.ReceiveGitWebhook(rec, prevcovHookReq("github", fixtureUUID, []byte("{}")))
	prevcovStatus(t, rec, http.StatusInternalServerError, "undecryptable secret")
}

func TestPrevcovReceiveGitWebhookMissingDeliveryID(t *testing.T) {
	a, db := prevcovAPI(t, nil)
	db.rules["GetWebhookEndpointByUUID"] = &prevcovRule{rows: []any{prevcovEndpointRow(t, a, nil)}}
	rec := httptest.NewRecorder()
	a.ReceiveGitWebhook(rec, prevcovHookReq("github", fixtureUUID, []byte("{}")))
	prevcovStatus(t, rec, http.StatusUnauthorized, "no delivery id")
}

func TestPrevcovReceiveGitWebhookBadSignature(t *testing.T) {
	a, db := prevcovAPI(t, nil)
	db.rules["GetWebhookEndpointByUUID"] = &prevcovRule{rows: []any{prevcovEndpointRow(t, a, nil)}}
	body := []byte(`{"ref":"refs/heads/main"}`)
	req := prevcovHookReq("github", fixtureUUID, body)
	prevcovSignGithub(req, body, "wrong-secret")
	rec := httptest.NewRecorder()
	a.ReceiveGitWebhook(rec, req)
	prevcovStatus(t, rec, http.StatusUnauthorized, "invalid signature")
}

func TestPrevcovReceiveGitWebhookDuplicateAndFailures(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)

	dup, db := prevcovAPI(t, map[string]*prevcovRule{
		"CreateWebhookDelivery": {noRow: true},
	})
	db.rules["GetWebhookEndpointByUUID"] = &prevcovRule{rows: []any{prevcovEndpointRow(t, dup, nil)}}
	req := prevcovHookReq("github", fixtureUUID, body)
	prevcovSignGithub(req, body, prevcovHookSecret)
	rec := httptest.NewRecorder()
	dup.ReceiveGitWebhook(rec, req)
	prevcovStatus(t, rec, http.StatusOK, "duplicate delivery")

	broken, db2 := prevcovAPI(t, map[string]*prevcovRule{
		"CreateWebhookDelivery": {err: errors.New("insert failed")},
	})
	db2.rules["GetWebhookEndpointByUUID"] = &prevcovRule{rows: []any{prevcovEndpointRow(t, broken, nil)}}
	req = prevcovHookReq("github", fixtureUUID, body)
	prevcovSignGithub(req, body, prevcovHookSecret)
	rec = httptest.NewRecorder()
	broken.ReceiveGitWebhook(rec, req)
	prevcovStatus(t, rec, http.StatusInternalServerError, "delivery persist failure")

	full, db3 := prevcovAPI(t, map[string]*prevcovRule{
		"EnqueueJob": {err: errors.New("queue closed")},
	})
	db3.rules["GetWebhookEndpointByUUID"] = &prevcovRule{rows: []any{prevcovEndpointRow(t, full, nil)}}
	req = prevcovHookReq("github", fixtureUUID, body)
	prevcovSignGithub(req, body, prevcovHookSecret)
	rec = httptest.NewRecorder()
	full.ReceiveGitWebhook(rec, req)
	prevcovStatus(t, rec, http.StatusInternalServerError, "enqueue failure")
}

func TestPrevcovReceiveGitWebhookAccepted(t *testing.T) {
	a, db := prevcovAPI(t, nil)
	db.rules["GetWebhookEndpointByUUID"] = &prevcovRule{rows: []any{prevcovEndpointRow(t, a, nil)}}
	body := []byte(`{"ref":"refs/heads/main"}`)
	req := prevcovHookReq("github", fixtureUUID, body)
	prevcovSignGithub(req, body, prevcovHookSecret)
	rec := httptest.NewRecorder()
	a.ReceiveGitWebhook(rec, req)
	prevcovStatus(t, rec, http.StatusOK, "accepted delivery")
	if !strings.Contains(rec.Body.String(), "received") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestPrevcovReceiveGitWebhookGitlabToken(t *testing.T) {
	a, db := prevcovAPI(t, nil)
	db.rules["GetWebhookEndpointByUUID"] = &prevcovRule{rows: []any{prevcovEndpointRow(t, a, func(r *store.GetWebhookEndpointByUUIDRow) {
		r.Provider = store.WebhookProviderGitlab
	})}}
	body := []byte(`{"object_kind":"push"}`)
	req := prevcovHookReq("gitlab", fixtureUUID, body)
	req.Header.Set("X-Gitlab-Event-UUID", "gl-1")
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	req.Header.Set("X-Gitlab-Token", prevcovHookSecret)
	rec := httptest.NewRecorder()
	a.ReceiveGitWebhook(rec, req)
	prevcovStatus(t, rec, http.StatusOK, "gitlab delivery")
}

func TestPrevcovTruncatedPayload(t *testing.T) {
	if got := string(truncatedPayload(bytes.Repeat([]byte("x"), (512<<10)+1))); got != `{"truncated":true}` {
		t.Fatalf("oversized payload stored as %s", got)
	}
	if got := string(truncatedPayload([]byte("not json"))); got != `{"unparsable":true}` {
		t.Fatalf("unparsable payload stored as %s", got)
	}
	if got := string(truncatedPayload([]byte(`{"ok":1}`))); got != `{"ok":1}` {
		t.Fatalf("valid payload stored as %s", got)
	}
}

func TestPrevcovCreateWebhookEndpointValidation(t *testing.T) {
	a, _ := prevcovAPI(t, nil)

	rec := httptest.NewRecorder()
	a.CreateWebhookEndpoint(rec, prevcovReq(http.MethodPost, "/x", "{nope"), fixtureUUID)
	prevcovStatus(t, rec, http.StatusBadRequest, "invalid endpoint body")

	rec = httptest.NewRecorder()
	a.CreateWebhookEndpoint(rec, prevcovReq(http.MethodPost, "/x", `{"provider":"svn"}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusUnprocessableEntity, "unsupported endpoint provider")
}

func TestPrevcovCreateWebhookEndpointConflictAndFailure(t *testing.T) {
	conflict, _ := prevcovAPI(t, map[string]*prevcovRule{
		"CreateWebhookEndpoint": {err: &pgconn.PgError{Code: "23505"}},
	})
	rec := httptest.NewRecorder()
	conflict.CreateWebhookEndpoint(rec, prevcovReq(http.MethodPost, "/x", `{"provider":"github"}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusConflict, "duplicate endpoint")

	broken, _ := prevcovAPI(t, map[string]*prevcovRule{
		"CreateWebhookEndpoint": {err: errors.New("insert failed")},
	})
	rec = httptest.NewRecorder()
	broken.CreateWebhookEndpoint(rec, prevcovReq(http.MethodPost, "/x", `{"provider":"github"}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusInternalServerError, "endpoint persist failure")
}

func TestPrevcovCreateWebhookEndpointURLForms(t *testing.T) {
	// With a configured FQDN the URL is absolute…
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetInstanceSettings": {rows: []any{store.InstanceSetting{Fqdn: ptr("panel.example.test")}}},
	})
	rec := httptest.NewRecorder()
	a.CreateWebhookEndpoint(rec, prevcovReq(http.MethodPost, "/x", `{"provider":"github"}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusCreated, "endpoint created")
	var out api.WebhookEndpoint
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Url, "https://panel.example.test/webhooks/github/") {
		t.Fatalf("endpoint url = %q", out.Url)
	}
	if out.Secret == nil || *out.Secret == "" {
		t.Fatal("the one-shot secret is missing")
	}

	// …without one it stays relative.
	bare, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetInstanceSettings": {rows: []any{store.InstanceSetting{}}},
	})
	rec = httptest.NewRecorder()
	bare.CreateWebhookEndpoint(rec, prevcovReq(http.MethodPost, "/x", `{"provider":"github"}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusCreated, "endpoint created without fqdn")
	out = api.WebhookEndpoint{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Url, "/webhooks/github/") {
		t.Fatalf("relative endpoint url = %q", out.Url)
	}
}

func TestPrevcovDeleteWebhookEndpoint(t *testing.T) {
	missing, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetWebhookEndpointForApplication": {err: pgx.ErrNoRows},
	})
	rec := httptest.NewRecorder()
	missing.DeleteWebhookEndpoint(rec, prevcovReq(http.MethodDelete, "/x", ""), fixtureUUID, api.DeleteWebhookEndpointParams{Provider: "github"})
	prevcovStatus(t, rec, http.StatusNotFound, "endpoint not found")

	broken, _ := prevcovAPI(t, map[string]*prevcovRule{
		"DeleteWebhookEndpoint": {err: errors.New("delete failed")},
	})
	rec = httptest.NewRecorder()
	broken.DeleteWebhookEndpoint(rec, prevcovReq(http.MethodDelete, "/x", ""), fixtureUUID, api.DeleteWebhookEndpointParams{Provider: "github"})
	prevcovStatus(t, rec, http.StatusInternalServerError, "endpoint delete failure")

	ok, _ := prevcovAPI(t, nil)
	rec = httptest.NewRecorder()
	ok.DeleteWebhookEndpoint(rec, prevcovReq(http.MethodDelete, "/x", ""), fixtureUUID, api.DeleteWebhookEndpointParams{Provider: "github"})
	prevcovStatus(t, rec, http.StatusNoContent, "endpoint deleted")
}

// ---------------------------------------------------------------------------
// githubapps.go
// ---------------------------------------------------------------------------

func TestPrevcovCreateGithubAppValidation(t *testing.T) {
	a, _ := prevcovAPI(t, nil)

	rec := httptest.NewRecorder()
	a.CreateGithubApp(rec, prevcovReq(http.MethodPost, "/github-apps", "{nope"))
	prevcovStatus(t, rec, http.StatusBadRequest, "invalid app body")

	rec = httptest.NewRecorder()
	a.CreateGithubApp(rec, prevcovReq(http.MethodPost, "/github-apps", `{"api_url":"http://insecure.test"}`))
	prevcovStatus(t, rec, http.StatusUnprocessableEntity, "non-https api url")

	rec = httptest.NewRecorder()
	a.CreateGithubApp(rec, prevcovReq(http.MethodPost, "/github-apps", `{"html_url":"http://insecure.test"}`))
	prevcovStatus(t, rec, http.StatusUnprocessableEntity, "non-https html url")

	noFqdn, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetInstanceSettings": {rows: []any{store.InstanceSetting{}}},
	})
	rec = httptest.NewRecorder()
	noFqdn.CreateGithubApp(rec, prevcovReq(http.MethodPost, "/github-apps", `{}`))
	prevcovStatus(t, rec, http.StatusUnprocessableEntity, "missing instance fqdn")
}

func TestPrevcovCreateGithubAppOutcomes(t *testing.T) {
	a, _ := prevcovAPI(t, nil)
	rec := httptest.NewRecorder()
	a.CreateGithubApp(rec, prevcovReq(http.MethodPost, "/github-apps", `{"name":"My App","organization":"acme"}`))
	prevcovStatus(t, rec, http.StatusCreated, "draft app")
	var out api.GithubAppManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.TargetUrl, "/organizations/acme/settings/apps/new") {
		t.Fatalf("target url = %q", out.TargetUrl)
	}
	if out.State == "" {
		t.Fatal("no one-shot state")
	}

	// Without a name the instance host names the draft.
	rec = httptest.NewRecorder()
	a.CreateGithubApp(rec, prevcovReq(http.MethodPost, "/github-apps", `{}`))
	prevcovStatus(t, rec, http.StatusCreated, "default-named draft app")

	conflict, _ := prevcovAPI(t, map[string]*prevcovRule{
		"CreateDraftGithubApp": {err: &pgconn.PgError{Code: "23505"}},
	})
	rec = httptest.NewRecorder()
	conflict.CreateGithubApp(rec, prevcovReq(http.MethodPost, "/github-apps", `{}`))
	prevcovStatus(t, rec, http.StatusConflict, "duplicate app name")

	broken, _ := prevcovAPI(t, map[string]*prevcovRule{
		"CreateDraftGithubApp": {err: errors.New("insert failed")},
	})
	rec = httptest.NewRecorder()
	broken.CreateGithubApp(rec, prevcovReq(http.MethodPost, "/github-apps", `{}`))
	prevcovStatus(t, rec, http.StatusInternalServerError, "draft persist failure")
}

func TestPrevcovListGithubApps(t *testing.T) {
	broken, _ := prevcovAPI(t, map[string]*prevcovRule{
		"ListGithubAppsPage": {err: errors.New("select failed")},
	})
	rec := httptest.NewRecorder()
	broken.ListGithubApps(rec, prevcovReq(http.MethodGet, "/github-apps", ""), api.ListGithubAppsParams{})
	prevcovStatus(t, rec, http.StatusInternalServerError, "list failure")

	a, db := prevcovAPI(t, nil)
	db.rules["ListGithubAppsPage"] = &prevcovRule{rows: []any{
		prevcovGithubApp(t, a, "https://api.github.com", func(r *store.GithubApp) { r.ID = 1 }),
		prevcovGithubApp(t, a, "https://api.github.com", func(r *store.GithubApp) { r.ID = 2 }),
	}}
	limit := 1
	rec = httptest.NewRecorder()
	a.ListGithubApps(rec, prevcovReq(http.MethodGet, "/github-apps", ""), api.ListGithubAppsParams{Limit: &limit})
	prevcovStatus(t, rec, http.StatusOK, "paged list")
	if !strings.Contains(rec.Body.String(), "next_cursor") || strings.Contains(rec.Body.String(), `"next_cursor":null`) {
		t.Fatalf("expected a next cursor: %s", rec.Body.String())
	}
}

func TestPrevcovDeleteGithubApp(t *testing.T) {
	restricted, _ := prevcovAPI(t, map[string]*prevcovRule{
		"DeleteGithubApp": {err: &pgconn.PgError{Code: "23503"}},
	})
	rec := httptest.NewRecorder()
	restricted.DeleteGithubApp(rec, prevcovReq(http.MethodDelete, "/x", ""), fixtureUUID)
	prevcovStatus(t, rec, http.StatusConflict, "app still referenced")

	broken, _ := prevcovAPI(t, map[string]*prevcovRule{
		"DeleteGithubApp": {err: errors.New("delete failed")},
	})
	rec = httptest.NewRecorder()
	broken.DeleteGithubApp(rec, prevcovReq(http.MethodDelete, "/x", ""), fixtureUUID)
	prevcovStatus(t, rec, http.StatusInternalServerError, "app delete failure")
}

func TestPrevcovGithubClientFor(t *testing.T) {
	a, _ := prevcovAPI(t, nil)

	if _, _, err := a.githubClientFor(store.GithubApp{}); err == nil {
		t.Fatal("unconverted app must not yield a client")
	}
	if _, _, err := a.githubClientFor(prevcovGithubApp(t, a, "https://api.github.com", func(r *store.GithubApp) {
		r.AppPrivateKeyEnc = []byte("garbage")
	})); err == nil {
		t.Fatal("undecryptable key must not yield a client")
	}
	client, tokens, err := a.githubClientFor(prevcovGithubApp(t, a, "https://api.example.test", nil))
	if err != nil || client == nil || tokens == nil {
		t.Fatalf("githubClientFor: %v", err)
	}
}

func TestPrevcovListGithubAppRepositories(t *testing.T) {
	notInstalled, db := prevcovAPI(t, nil)
	db.rules["GetGithubAppByUUID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, notInstalled, "https://api.github.com", func(r *store.GithubApp) {
		r.InstallationID = nil
	})}}
	rec := httptest.NewRecorder()
	notInstalled.ListGithubAppRepositories(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.ListGithubAppRepositoriesParams{})
	prevcovStatus(t, rec, http.StatusConflict, "app not installed")

	noSource, db2 := prevcovAPI(t, map[string]*prevcovRule{
		"GetGitSourceForGithubApp": {err: pgx.ErrNoRows},
	})
	db2.rules["GetGithubAppByUUID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, noSource, "https://api.github.com", nil)}}
	rec = httptest.NewRecorder()
	noSource.ListGithubAppRepositories(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.ListGithubAppRepositoriesParams{})
	prevcovStatus(t, rec, http.StatusInternalServerError, "source missing")

	badCache, db3 := prevcovAPI(t, map[string]*prevcovRule{
		"ListRepositoriesForSource": {err: errors.New("select failed")},
	})
	db3.rules["GetGithubAppByUUID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, badCache, "https://api.github.com", nil)}}
	rec = httptest.NewRecorder()
	badCache.ListGithubAppRepositories(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.ListGithubAppRepositoriesParams{})
	prevcovStatus(t, rec, http.StatusInternalServerError, "cache read failure")
}

func TestPrevcovListGithubAppRepositoriesRefresh(t *testing.T) {
	srv := prevcovServer(t, prevcovGithubOK(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/installation/repositories") {
			_, _ = w.Write([]byte(`{"total_count":1,"repositories":[{"id":42,"full_name":"acme/unit","default_branch":"main","html_url":"https://github.example.test/acme/unit"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	a, db := prevcovAPI(t, nil)
	db.rules["GetGithubAppByUUID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, a, srv.URL, nil)}}
	refresh := true
	rec := httptest.NewRecorder()
	a.ListGithubAppRepositories(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.ListGithubAppRepositoriesParams{Refresh: &refresh})
	prevcovStatus(t, rec, http.StatusOK, "refreshed repositories")
	if !strings.Contains(rec.Body.String(), "data") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestPrevcovListGithubAppRepositoriesRefreshFailures(t *testing.T) {
	// The installation token mint fails: the sync error surfaces as a 500.
	tokenDown := prevcovServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	a, db := prevcovAPI(t, map[string]*prevcovRule{
		"ListRepositoriesForSource": {}, // empty cache forces the sync
	})
	db.rules["GetGithubAppByUUID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, a, tokenDown.URL, nil)}}
	rec := httptest.NewRecorder()
	a.ListGithubAppRepositories(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.ListGithubAppRepositoriesParams{})
	prevcovStatus(t, rec, http.StatusInternalServerError, "token mint failure")

	// Token OK, repository listing fails.
	listDown := prevcovServer(t, prevcovGithubOK(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	b, db2 := prevcovAPI(t, map[string]*prevcovRule{
		"ListRepositoriesForSource": {},
	})
	db2.rules["GetGithubAppByUUID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, b, listDown.URL, nil)}}
	rec = httptest.NewRecorder()
	b.ListGithubAppRepositories(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.ListGithubAppRepositoriesParams{})
	prevcovStatus(t, rec, http.StatusInternalServerError, "repository list failure")

	// Token and listing OK, the upsert fails.
	okSrv := prevcovServer(t, prevcovGithubOK(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/installation/repositories") {
			_, _ = w.Write([]byte(`{"total_count":1,"repositories":[{"id":42,"full_name":"acme/unit","default_branch":"main","html_url":"https://x.test"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	c, db3 := prevcovAPI(t, map[string]*prevcovRule{
		"ListRepositoriesForSource": {},
		"UpsertRepository":          {err: errors.New("upsert failed")},
	})
	db3.rules["GetGithubAppByUUID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, c, okSrv.URL, nil)}}
	rec = httptest.NewRecorder()
	c.ListGithubAppRepositories(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.ListGithubAppRepositoriesParams{})
	prevcovStatus(t, rec, http.StatusInternalServerError, "upsert failure")
}

func TestPrevcovGithubManifestCallback(t *testing.T) {
	a, _ := prevcovAPI(t, nil)
	rec := httptest.NewRecorder()
	a.GithubManifestCallback(rec, prevcovReq(http.MethodGet, "/webhooks/github/manifest/callback", ""))
	prevcovStatus(t, rec, http.StatusBadRequest, "missing code/state")

	unknown, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetGithubAppByStateHash": {err: pgx.ErrNoRows},
	})
	rec = httptest.NewRecorder()
	unknown.GithubManifestCallback(rec, prevcovReq(http.MethodGet, "/webhooks/github/manifest/callback?code=c&state=s", ""))
	prevcovStatus(t, rec, http.StatusNotFound, "unknown state")
}

func prevcovManifestServer(t *testing.T, status int, creds string) *httptest.Server {
	t.Helper()
	return prevcovServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/app-manifests/") {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(creds))
			return
		}
		http.NotFound(w, r)
	})
}

func prevcovDraftApp(t *testing.T, a *API, apiURL string) store.GithubApp {
	t.Helper()
	return prevcovGithubApp(t, a, apiURL, func(r *store.GithubApp) {
		r.AppID, r.Slug, r.InstallationID = nil, nil, nil
		r.WebhookSecretEnc, r.AppPrivateKeyEnc = nil, nil
		r.ManifestStateHash = ptr("hash")
	})
}

func TestPrevcovGithubManifestCallbackConversionFails(t *testing.T) {
	srv := prevcovManifestServer(t, http.StatusUnprocessableEntity, `{"message":"bad code"}`)
	a, db := prevcovAPI(t, nil)
	db.rules["GetGithubAppByStateHash"] = &prevcovRule{rows: []any{prevcovDraftApp(t, a, srv.URL)}}
	rec := httptest.NewRecorder()
	a.GithubManifestCallback(rec, prevcovReq(http.MethodGet, "/webhooks/github/manifest/callback?code=c&state=s", ""))
	prevcovStatus(t, rec, http.StatusBadGateway, "conversion failure")
}

const prevcovCreds = `{"id":99,"slug":"unit-app","name":"unit-app","client_id":"cid","client_secret":"cs","webhook_secret":"ws","pem":"PEMPEM","html_url":"https://github.example.test/apps/unit-app"}`

func TestPrevcovGithubManifestCallbackOutcomes(t *testing.T) {
	srv := prevcovManifestServer(t, http.StatusCreated, prevcovCreds)

	// Happy: converted, source created, redirected to the install page.
	a, db := prevcovAPI(t, nil)
	db.rules["GetGithubAppByStateHash"] = &prevcovRule{rows: []any{prevcovDraftApp(t, a, srv.URL)}}
	db.rules["CompleteGithubAppConversion"] = &prevcovRule{rows: []any{prevcovGithubApp(t, a, srv.URL, func(r *store.GithubApp) {
		r.HtmlUrl = "https://github.example.test"
	})}}
	rec := httptest.NewRecorder()
	a.GithubManifestCallback(rec, prevcovReq(http.MethodGet, "/webhooks/github/manifest/callback?code=c&state=s", ""))
	prevcovStatus(t, rec, http.StatusFound, "conversion completed")
	if loc := rec.Header().Get("Location"); loc != "https://github.example.test/apps/unit-app/installations/new" {
		t.Fatalf("install redirect = %q", loc)
	}

	// A slug-less conversion goes back to the dashboard.
	b, db2 := prevcovAPI(t, nil)
	db2.rules["GetGithubAppByStateHash"] = &prevcovRule{rows: []any{prevcovDraftApp(t, b, srv.URL)}}
	db2.rules["CompleteGithubAppConversion"] = &prevcovRule{rows: []any{prevcovGithubApp(t, b, srv.URL, func(r *store.GithubApp) {
		r.Slug = nil
	})}}
	rec = httptest.NewRecorder()
	b.GithubManifestCallback(rec, prevcovReq(http.MethodGet, "/webhooks/github/manifest/callback?code=c&state=s", ""))
	prevcovStatus(t, rec, http.StatusFound, "slugless conversion")
	if loc := rec.Header().Get("Location"); loc != "/github-apps" {
		t.Fatalf("slugless redirect = %q", loc)
	}

	// A raced replay: the state was consumed between read and completion.
	raced, db3 := prevcovAPI(t, map[string]*prevcovRule{
		"CompleteGithubAppConversion": {err: pgx.ErrNoRows},
	})
	db3.rules["GetGithubAppByStateHash"] = &prevcovRule{rows: []any{prevcovDraftApp(t, raced, srv.URL)}}
	rec = httptest.NewRecorder()
	raced.GithubManifestCallback(rec, prevcovReq(http.MethodGet, "/webhooks/github/manifest/callback?code=c&state=s", ""))
	prevcovStatus(t, rec, http.StatusNotFound, "raced replay")

	// A plain database failure.
	broken, db4 := prevcovAPI(t, map[string]*prevcovRule{
		"CompleteGithubAppConversion": {err: errors.New("update failed")},
	})
	db4.rules["GetGithubAppByStateHash"] = &prevcovRule{rows: []any{prevcovDraftApp(t, broken, srv.URL)}}
	rec = httptest.NewRecorder()
	broken.GithubManifestCallback(rec, prevcovReq(http.MethodGet, "/webhooks/github/manifest/callback?code=c&state=s", ""))
	prevcovStatus(t, rec, http.StatusInternalServerError, "conversion persist failure")

	// The git source insert fails for a non-unique reason.
	sourceless, db5 := prevcovAPI(t, map[string]*prevcovRule{
		"CreateGithubAppSource": {err: errors.New("insert failed")},
	})
	db5.rules["GetGithubAppByStateHash"] = &prevcovRule{rows: []any{prevcovDraftApp(t, sourceless, srv.URL)}}
	db5.rules["CompleteGithubAppConversion"] = &prevcovRule{rows: []any{prevcovGithubApp(t, sourceless, srv.URL, nil)}}
	rec = httptest.NewRecorder()
	sourceless.GithubManifestCallback(rec, prevcovReq(http.MethodGet, "/webhooks/github/manifest/callback?code=c&state=s", ""))
	prevcovStatus(t, rec, http.StatusInternalServerError, "source persist failure")
}

func TestPrevcovGithubAppSetup(t *testing.T) {
	a, _ := prevcovAPI(t, nil)
	rec := httptest.NewRecorder()
	a.GithubAppSetup(rec, prevcovChi(prevcovReq(http.MethodGet, "/webhooks/github/apps/x/setup", ""), map[string]string{"app_uuid": "not-a-uuid"}))
	prevcovStatus(t, rec, http.StatusNotFound, "invalid setup uuid")

	unknown, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetGithubAppByUUIDAny": {err: pgx.ErrNoRows},
	})
	rec = httptest.NewRecorder()
	unknown.GithubAppSetup(rec, prevcovChi(prevcovReq(http.MethodGet, "/webhooks/github/apps/x/setup", ""), map[string]string{"app_uuid": fixtureUUID}))
	prevcovStatus(t, rec, http.StatusNotFound, "unknown setup app")

	pending, db := prevcovAPI(t, nil)
	db.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, pending, "https://api.github.com", func(r *store.GithubApp) {
		r.InstallationID = nil
	})}}
	set := &prevcovRule{}
	db.rules["SetGithubAppInstallation"] = set
	rec = httptest.NewRecorder()
	pending.GithubAppSetup(rec, prevcovChi(prevcovReq(http.MethodGet, "/webhooks/github/apps/x/setup?installation_id=42", ""), map[string]string{"app_uuid": fixtureUUID}))
	prevcovStatus(t, rec, http.StatusFound, "setup with installation id")
	if set.called() == 0 {
		t.Fatal("the installation id was not recorded")
	}

	installed, db2 := prevcovAPI(t, nil)
	db2.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, installed, "https://api.github.com", nil)}}
	rec = httptest.NewRecorder()
	installed.GithubAppSetup(rec, prevcovChi(prevcovReq(http.MethodGet, "/webhooks/github/apps/x/setup", ""), map[string]string{"app_uuid": fixtureUUID}))
	prevcovStatus(t, rec, http.StatusFound, "setup without installation id")
}

func prevcovAppHookReq(t *testing.T, event string, body []byte, sign bool) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github/apps/"+fixtureUUID, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", "app-delivery-1")
	req.Header.Set("X-GitHub-Event", event)
	if sign {
		mac := hmac.New(sha256.New, []byte(prevcovHookSecret))
		mac.Write(body)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return prevcovChi(req, map[string]string{"app_uuid": fixtureUUID})
}

func TestPrevcovReceiveGithubAppWebhookGuards(t *testing.T) {
	a, _ := prevcovAPI(t, nil)
	rec := httptest.NewRecorder()
	a.ReceiveGithubAppWebhook(rec, prevcovChi(prevcovReq(http.MethodPost, "/x", "{}"), map[string]string{"app_uuid": "nope"}))
	prevcovStatus(t, rec, http.StatusNotFound, "invalid app uuid")

	unknown, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetGithubAppByUUIDAny": {err: pgx.ErrNoRows},
	})
	rec = httptest.NewRecorder()
	unknown.ReceiveGithubAppWebhook(rec, prevcovAppHookReq(t, "push", []byte("{}"), true))
	prevcovStatus(t, rec, http.StatusNotFound, "unknown app")

	draft, db := prevcovAPI(t, nil)
	db.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovDraftApp(t, draft, "https://api.github.com")}}
	rec = httptest.NewRecorder()
	draft.ReceiveGithubAppWebhook(rec, prevcovAppHookReq(t, "push", []byte("{}"), true))
	prevcovStatus(t, rec, http.StatusUnauthorized, "draft app without secret")

	oversized, db2 := prevcovAPI(t, nil)
	db2.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, oversized, "https://api.github.com", nil)}}
	rec = httptest.NewRecorder()
	oversized.ReceiveGithubAppWebhook(rec, prevcovAppHookReq(t, "push", bytes.Repeat([]byte("a"), (2<<20)+16), false))
	prevcovStatus(t, rec, http.StatusRequestEntityTooLarge, "oversized app delivery")

	undecryptable, db3 := prevcovAPI(t, nil)
	db3.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, undecryptable, "https://api.github.com", func(r *store.GithubApp) {
		r.WebhookSecretEnc = []byte("garbage")
	})}}
	rec = httptest.NewRecorder()
	undecryptable.ReceiveGithubAppWebhook(rec, prevcovAppHookReq(t, "push", []byte("{}"), true))
	prevcovStatus(t, rec, http.StatusInternalServerError, "undecryptable app secret")

	noDelivery, db4 := prevcovAPI(t, nil)
	db4.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, noDelivery, "https://api.github.com", nil)}}
	req := prevcovAppHookReq(t, "push", []byte("{}"), true)
	req.Header.Del("X-GitHub-Delivery")
	rec = httptest.NewRecorder()
	noDelivery.ReceiveGithubAppWebhook(rec, req)
	prevcovStatus(t, rec, http.StatusUnauthorized, "app delivery without id")

	unsigned, db5 := prevcovAPI(t, nil)
	db5.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, unsigned, "https://api.github.com", nil)}}
	rec = httptest.NewRecorder()
	unsigned.ReceiveGithubAppWebhook(rec, prevcovAppHookReq(t, "push", []byte("{}"), false))
	prevcovStatus(t, rec, http.StatusUnauthorized, "unsigned app delivery")

	dup, db6 := prevcovAPI(t, map[string]*prevcovRule{
		"CreateWebhookDelivery": {noRow: true},
	})
	db6.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, dup, "https://api.github.com", nil)}}
	rec = httptest.NewRecorder()
	dup.ReceiveGithubAppWebhook(rec, prevcovAppHookReq(t, "push", []byte("{}"), true))
	prevcovStatus(t, rec, http.StatusOK, "duplicate app delivery")

	broken, db7 := prevcovAPI(t, map[string]*prevcovRule{
		"CreateWebhookDelivery": {err: errors.New("insert failed")},
	})
	db7.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, broken, "https://api.github.com", nil)}}
	rec = httptest.NewRecorder()
	broken.ReceiveGithubAppWebhook(rec, prevcovAppHookReq(t, "push", []byte("{}"), true))
	prevcovStatus(t, rec, http.StatusInternalServerError, "app delivery persist failure")
}

func TestPrevcovReceiveGithubAppWebhookEvents(t *testing.T) {
	// installation created / deleted / unrelated action.
	for _, tc := range []struct {
		action string
		expect string // rule expected to fire
	}{
		{`{"action":"created","installation":{"id":77}}`, "SetGithubAppInstallation"},
		{`{"action":"deleted"}`, "ClearGithubAppInstallation"},
		{`{"action":"whatever"}`, ""},
	} {
		a, db := prevcovAPI(t, nil)
		db.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, a, "https://api.github.com", func(r *store.GithubApp) {
			r.InstallationID = nil
		})}}
		set := &prevcovRule{}
		clearRule := &prevcovRule{}
		db.rules["SetGithubAppInstallation"] = set
		db.rules["ClearGithubAppInstallation"] = clearRule
		rec := httptest.NewRecorder()
		a.ReceiveGithubAppWebhook(rec, prevcovAppHookReq(t, "installation", []byte(tc.action), true))
		prevcovStatus(t, rec, http.StatusOK, "installation event "+tc.action)
		switch tc.expect {
		case "SetGithubAppInstallation":
			if set.called() == 0 {
				t.Fatalf("installation id not recorded for %s", tc.action)
			}
		case "ClearGithubAppInstallation":
			if clearRule.called() == 0 {
				t.Fatalf("installation not cleared for %s", tc.action)
			}
		}
	}

	// installation_repositories resyncs (the sync itself fails silently here —
	// the app's API host is unreachable — and that is the contract).
	repos, db := prevcovAPI(t, nil)
	db.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, repos, "https://127.0.0.1:1", nil)}}
	rec := httptest.NewRecorder()
	repos.ReceiveGithubAppWebhook(rec, prevcovAppHookReq(t, "installation_repositories", []byte(`{}`), true))
	prevcovStatus(t, rec, http.StatusOK, "installation_repositories event")

	// push, pull_request, issue_comment enqueue their jobs.
	for _, event := range []string{"push", "pull_request", "issue_comment"} {
		a, db := prevcovAPI(t, nil)
		db.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, a, "https://api.github.com", nil)}}
		rec := httptest.NewRecorder()
		a.ReceiveGithubAppWebhook(rec, prevcovAppHookReq(t, event, []byte(`{"repository":{"id":42}}`), true))
		prevcovStatus(t, rec, http.StatusOK, event+" event")

		bad, db2 := prevcovAPI(t, map[string]*prevcovRule{
			"EnqueueJob": {err: errors.New("queue closed")},
		})
		db2.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, bad, "https://api.github.com", nil)}}
		rec = httptest.NewRecorder()
		bad.ReceiveGithubAppWebhook(rec, prevcovAppHookReq(t, event, []byte(`{"repository":{"id":42}}`), true))
		prevcovStatus(t, rec, http.StatusInternalServerError, event+" enqueue failure")
	}

	// An unhandled event type is recorded and ignored.
	other, db3 := prevcovAPI(t, nil)
	db3.rules["GetGithubAppByUUIDAny"] = &prevcovRule{rows: []any{prevcovGithubApp(t, other, "https://api.github.com", nil)}}
	rec = httptest.NewRecorder()
	other.ReceiveGithubAppWebhook(rec, prevcovAppHookReq(t, "star", []byte(`{}`), true))
	prevcovStatus(t, rec, http.StatusOK, "ignored event")
}

// ---------------------------------------------------------------------------
// previews.go
// ---------------------------------------------------------------------------

func TestPrevcovListApplicationPreviewsFailure(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"ListPreviewsForApplication": {err: errors.New("select failed")},
	})
	rec := httptest.NewRecorder()
	a.ListApplicationPreviews(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID)
	prevcovStatus(t, rec, http.StatusInternalServerError, "preview list failure")
}

func TestPrevcovApprovePreviewFork(t *testing.T) {
	// The preview belongs to another application of the same team.
	foreign, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(func(p *store.Preview) { p.ApplicationID = 2 })}},
	})
	rec := httptest.NewRecorder()
	foreign.ApprovePreviewFork(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusNotFound, "foreign preview")

	// Not a fork: nothing to approve.
	branch, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	rec = httptest.NewRecorder()
	branch.ApprovePreviewFork(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusConflict, "branch preview needs no approval")

	// Already approved.
	approved, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(func(p *store.Preview) {
			p.IsFork = true
			p.ForkApprovedAt = prevcovTS()
		})}},
	})
	rec = httptest.NewRecorder()
	approved.ApprovePreviewFork(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusConflict, "already approved")

	// The UPDATE matched nothing: a lost race is an internal error.
	raced, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(func(p *store.Preview) { p.IsFork = true })}},
		"ApprovePreviewFork":      {tag: "UPDATE 0"},
	})
	rec = httptest.NewRecorder()
	raced.ApprovePreviewFork(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusInternalServerError, "approve raced")

	// Happy: approved, promoted, 202 with the refreshed preview.
	happy, db := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(func(p *store.Preview) { p.IsFork = true })}},
		"GetPreviewByID": {rows: []any{prevcovPreview(func(p *store.Preview) {
			p.IsFork = true
			p.ForkApprovedAt = prevcovTS()
		})}},
	})
	db.flow.truthy = false
	rec = httptest.NewRecorder()
	happy.ApprovePreviewFork(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusAccepted, "fork approved")
	if !strings.Contains(rec.Body.String(), `"fork_approved":true`) {
		t.Fatalf("approval response: %s", rec.Body.String())
	}

	// The refresh after approval failing must not fail the approval.
	degraded, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(func(p *store.Preview) { p.IsFork = true })}},
		"GetApplicationByID":      {err: errors.New("select failed")},
		"GetPreviewByID":          {err: errors.New("select failed")},
	})
	rec = httptest.NewRecorder()
	degraded.ApprovePreviewFork(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusAccepted, "approval despite refresh failure")
}

func TestPrevcovResolvePreviewErrors(t *testing.T) {
	a, _ := prevcovAPI(t, nil)
	rec := httptest.NewRecorder()
	a.DestroyPreview(rec, prevcovReq(http.MethodDelete, "/x", ""), fixtureUUID, "not-a-uuid")
	prevcovStatus(t, rec, http.StatusNotFound, "invalid preview uuid")

	missing, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {err: pgx.ErrNoRows},
	})
	rec = httptest.NewRecorder()
	missing.DestroyPreview(rec, prevcovReq(http.MethodDelete, "/x", ""), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusNotFound, "unknown preview")

	foreign, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(func(p *store.Preview) { p.ApplicationID = 2 })}},
	})
	rec = httptest.NewRecorder()
	foreign.DestroyPreview(rec, prevcovReq(http.MethodDelete, "/x", ""), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusNotFound, "preview of another application")
}

func TestPrevcovDestroyPreview(t *testing.T) {
	gone, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(func(p *store.Preview) { p.Status = store.PreviewStatusDestroyed })}},
	})
	rec := httptest.NewRecorder()
	gone.DestroyPreview(rec, prevcovReq(http.MethodDelete, "/x", ""), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusConflict, "already destroyed")

	broken, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
		"EnqueueJob":              {err: errors.New("queue closed")},
	})
	rec = httptest.NewRecorder()
	broken.DestroyPreview(rec, prevcovReq(http.MethodDelete, "/x", ""), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusInternalServerError, "destroy enqueue failure")

	ok, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	rec = httptest.NewRecorder()
	ok.DestroyPreview(rec, prevcovReq(http.MethodDelete, "/x", ""), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusAccepted, "destroy accepted")
}

func TestPrevcovRedeployPreviewGuards(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	rec := httptest.NewRecorder()
	a.RedeployPreview(rec, prevcovReq(http.MethodPost, "/x", `{"force_rebuild":true,"skip_build":true}`), fixtureUUID, fixtureUUID, api.RedeployPreviewParams{})
	prevcovStatus(t, rec, http.StatusUnprocessableEntity, "contradictory deploy body")

	gone, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(func(p *store.Preview) { p.Status = store.PreviewStatusDestroying })}},
	})
	rec = httptest.NewRecorder()
	gone.RedeployPreview(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID, api.RedeployPreviewParams{})
	prevcovStatus(t, rec, http.StatusConflict, "destroyed preview redeploy")

	fork, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(func(p *store.Preview) { p.IsFork = true })}},
	})
	rec = httptest.NewRecorder()
	fork.RedeployPreview(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID, api.RedeployPreviewParams{})
	prevcovStatus(t, rec, http.StatusConflict, "unapproved fork redeploy")
}

func TestPrevcovRedeployPreviewSkipBuild(t *testing.T) {
	// Nothing ever deployed: 409, not 500.
	empty, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam":           {rows: []any{prevcovPreview(nil)}},
		"GetLastSucceededPreviewDeployment": {err: pgx.ErrNoRows},
	})
	rec := httptest.NewRecorder()
	empty.RedeployPreview(rec, prevcovReq(http.MethodPost, "/x", `{"skip_build":true}`), fixtureUUID, fixtureUUID, api.RedeployPreviewParams{})
	prevcovStatus(t, rec, http.StatusConflict, "nothing to redeploy")

	// Server queue full: 429 with Retry-After.
	full, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam":         {rows: []any{prevcovPreview(nil)}},
		"CountActiveDeploymentsForServer": {rows: []any{int64(50)}},
	})
	rec = httptest.NewRecorder()
	full.RedeployPreview(rec, prevcovReq(http.MethodPost, "/x", `{"skip_build":true}`), fixtureUUID, fixtureUUID, api.RedeployPreviewParams{})
	prevcovStatus(t, rec, http.StatusTooManyRequests, "deploy queue full")
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}

	// Happy: the config-apply deployment is queued.
	ok, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	rec = httptest.NewRecorder()
	ok.RedeployPreview(rec, prevcovReq(http.MethodPost, "/x", `{"skip_build":true}`), fixtureUUID, fixtureUUID, api.RedeployPreviewParams{})
	prevcovStatus(t, rec, http.StatusAccepted, "skip-build redeploy accepted")
	if !strings.Contains(rec.Body.String(), "deployment_uuid") {
		t.Fatalf("skip-build response: %s", rec.Body.String())
	}
}

func TestPrevcovRedeployPreviewFull(t *testing.T) {
	// The application row vanishing mid-request is an internal error.
	gone, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
		"GetApplicationByID":      {err: errors.New("select failed")},
	})
	rec := httptest.NewRecorder()
	gone.RedeployPreview(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID, api.RedeployPreviewParams{})
	prevcovStatus(t, rec, http.StatusInternalServerError, "application read failure")

	// Concurrency cap reached: promotion refuses with a reason.
	capped, db := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam":         {rows: []any{prevcovPreview(nil)}},
		"CountLivePreviewsForApplication": {rows: []any{int64(9)}},
	})
	db.flow.truthy = true // PreviewMaxConcurrent stays a small non-nil limit
	rec = httptest.NewRecorder()
	capped.RedeployPreview(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID, api.RedeployPreviewParams{})
	prevcovStatus(t, rec, http.StatusConflict, "concurrency cap")

	// A promotion error is an internal error.
	broken, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
		"GetDestinationByID":      {err: errors.New("select failed")},
	})
	rec = httptest.NewRecorder()
	broken.RedeployPreview(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID, api.RedeployPreviewParams{})
	prevcovStatus(t, rec, http.StatusInternalServerError, "promotion failure")

	// Happy: promoted, 202.
	ok, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	rec = httptest.NewRecorder()
	ok.RedeployPreview(rec, prevcovReq(http.MethodPost, "/x", `{"force_rebuild":true}`), fixtureUUID, fixtureUUID, api.RedeployPreviewParams{})
	prevcovStatus(t, rec, http.StatusAccepted, "full redeploy accepted")
}

func TestPrevcovGetPreviewLogsGuards(t *testing.T) {
	// Unknown compose component.
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	lines := 50
	rec := httptest.NewRecorder()
	a.GetPreviewLogs(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, fixtureUUID, api.GetPreviewLogsParams{Lines: &lines, Component: ptr("nope")})
	prevcovStatus(t, rec, http.StatusNotFound, "unknown component")

	// Component listing failure.
	broken, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
		"ListServiceComponents":   {err: errors.New("select failed")},
	})
	rec = httptest.NewRecorder()
	broken.GetPreviewLogs(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, fixtureUUID, api.GetPreviewLogsParams{Component: ptr("web")})
	prevcovStatus(t, rec, http.StatusInternalServerError, "component list failure")

	// No agent connected: 409.
	off, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	rec = httptest.NewRecorder()
	off.GetPreviewLogs(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, fixtureUUID, api.GetPreviewLogsParams{Component: ptr("unit")})
	prevcovStatus(t, rec, http.StatusConflict, "agent not connected")
}

// prevcovScriptAgent registers a live agent channel for server 1 and answers
// the inspect (and optionally the logs stream) like a real agent would.
func prevcovScriptAgent(t *testing.T, a *API, inspectErr *agentwire.Error) {
	t.Helper()
	ac, agent := dialPair(t)
	a.AgentRPC = &AgentConns{}
	a.AgentRPC.register(1, ac)
	go func() {
		cmd, err := readCommand(agent)
		if err != nil {
			return
		}
		if inspectErr != nil {
			_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID, Err: inspectErr}})
			return
		}
		_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID, Body: json.RawMessage(`{"Config":{"Tty":true}}`)}})
		logs, err := readCommand(agent)
		if err != nil {
			return
		}
		_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: logs.ID}})
		_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: logs.ID, Data: []byte("line one\nline two\n")}})
		_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: logs.ID, EOF: true}})
	}()
}

func TestPrevcovGetPreviewLogsThroughAgent(t *testing.T) {
	// The container is gone on the server.
	gone, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	prevcovScriptAgent(t, gone, &agentwire.Error{Code: agentwire.CodeNotFound, Message: "no such container"})
	rec := httptest.NewRecorder()
	gone.GetPreviewLogs(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, fixtureUUID, api.GetPreviewLogsParams{})
	prevcovStatus(t, rec, http.StatusConflict, "container gone")

	// A daemon failure is an internal error.
	broken, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	prevcovScriptAgent(t, broken, &agentwire.Error{Code: agentwire.CodeInternal, Message: "daemon on fire"})
	rec = httptest.NewRecorder()
	broken.GetPreviewLogs(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, fixtureUUID, api.GetPreviewLogsParams{})
	prevcovStatus(t, rec, http.StatusInternalServerError, "daemon failure")

	// Happy: the snapshot comes back as log lines.
	ok, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	prevcovScriptAgent(t, ok, nil)
	lines := 100
	rec = httptest.NewRecorder()
	ok.GetPreviewLogs(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, fixtureUUID, api.GetPreviewLogsParams{Lines: &lines})
	prevcovStatus(t, rec, http.StatusOK, "preview logs")
	if !strings.Contains(rec.Body.String(), "line one") {
		t.Fatalf("logs body: %s", rec.Body.String())
	}
}

func TestPrevcovCreatePreviewTerminalSession(t *testing.T) {
	gone, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(func(p *store.Preview) { p.Status = store.PreviewStatusDestroyed })}},
	})
	rec := httptest.NewRecorder()
	gone.CreatePreviewTerminalSession(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID, api.CreatePreviewTerminalSessionParams{})
	prevcovStatus(t, rec, http.StatusConflict, "terminal on destroyed preview")

	badList, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
		"ListServiceComponents":   {err: errors.New("select failed")},
	})
	rec = httptest.NewRecorder()
	badList.CreatePreviewTerminalSession(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID, api.CreatePreviewTerminalSessionParams{Component: ptr("web")})
	prevcovStatus(t, rec, http.StatusInternalServerError, "terminal component list failure")

	unknown, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	rec = httptest.NewRecorder()
	unknown.CreatePreviewTerminalSession(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID, api.CreatePreviewTerminalSessionParams{Component: ptr("nope")})
	prevcovStatus(t, rec, http.StatusNotFound, "unknown terminal component")

	ok, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	rec = httptest.NewRecorder()
	ok.CreatePreviewTerminalSession(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID, api.CreatePreviewTerminalSessionParams{Component: ptr("unit")})
	prevcovStatus(t, rec, http.StatusCreated, "terminal session created")
}

func TestPrevcovKeepPreview(t *testing.T) {
	gone, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(func(p *store.Preview) { p.Status = store.PreviewStatusDestroyed })}},
	})
	rec := httptest.NewRecorder()
	gone.KeepPreview(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusConflict, "keep destroyed preview")

	broken, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
		"KeepPreviewAlive":        {err: errors.New("update failed")},
	})
	rec = httptest.NewRecorder()
	broken.KeepPreview(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusInternalServerError, "keep failure")

	ok, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	rec = httptest.NewRecorder()
	ok.KeepPreview(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusNoContent, "keep accepted")
}

func TestPrevcovListPreviewEnvs(t *testing.T) {
	broken, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
		"ListPreviewEnvVars":      {err: errors.New("select failed")},
	})
	rec := httptest.NewRecorder()
	broken.ListPreviewEnvs(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusInternalServerError, "preview envs failure")
}

func TestPrevcovCreatePreviewEnv(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByUUIDForTeam": {rows: []any{prevcovPreview(nil)}},
	})
	rec := httptest.NewRecorder()
	a.CreatePreviewEnv(rec, prevcovReq(http.MethodPost, "/x", "{nope"), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusBadRequest, "invalid env body")

	rec = httptest.NewRecorder()
	a.CreatePreviewEnv(rec, prevcovReq(http.MethodPost, "/x", `{"key":"9bad","value":"x"}`), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusUnprocessableEntity, "invalid env key")

	rec = httptest.NewRecorder()
	a.CreatePreviewEnv(rec, prevcovReq(http.MethodPost, "/x", `{"key":"GOOD_KEY","value":"x"}`), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusCreated, "preview env created")
}

// prevcovAppRowWithSource is an application wired to a GitHub App source.
func prevcovAppRowWithSource() appRow {
	return appRow{
		Resource: store.Resource{ID: 1, Uuid: prevcovUUIDv(), TeamID: 1, Name: "unit"},
		Application: store.Application{
			ID: 1, GitSourceID: ptr(int64(1)), RepositoryID: ptr(int64(1)),
		},
	}
}

func TestPrevcovGithubForApplication(t *testing.T) {
	ctx := context.Background()

	// No GitHub App source at all.
	a, _ := prevcovAPI(t, nil)
	if _, _, _, err := a.githubForApplication(ctx, appRow{}); err == nil {
		t.Fatal("sourceless application must not resolve a client")
	}

	// The git source is not a GitHub App.
	manual, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetGitSourceByID": {rows: []any{store.GitSource{ID: 1, TeamID: 1, Name: "manual", Kind: store.GitSourceKindPublic, Provider: store.GitProviderGithub}}},
	})
	if _, _, _, err := manual.githubForApplication(ctx, prevcovAppRowWithSource()); err == nil || !strings.Contains(err.Error(), "not a GitHub App") {
		t.Fatalf("manual source error = %v", err)
	}

	// The app row is gone.
	orphan, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetGithubAppByID": {err: pgx.ErrNoRows},
	})
	if _, _, _, err := orphan.githubForApplication(ctx, prevcovAppRowWithSource()); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("orphan app error = %v", err)
	}

	// The app is a draft: manifest flow unfinished.
	draft, db := prevcovAPI(t, nil)
	db.rules["GetGithubAppByID"] = &prevcovRule{rows: []any{prevcovDraftApp(t, draft, "https://api.github.com")}}
	if _, _, _, err := draft.githubForApplication(ctx, prevcovAppRowWithSource()); err == nil || !strings.Contains(err.Error(), "not installed yet") {
		t.Fatalf("draft app error = %v", err)
	}

	// The repository row is gone.
	norepo, db2 := prevcovAPI(t, map[string]*prevcovRule{
		"GetRepositoryByID": {err: pgx.ErrNoRows},
	})
	db2.rules["GetGithubAppByID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, norepo, "https://api.github.com", nil)}}
	if _, _, _, err := norepo.githubForApplication(ctx, prevcovAppRowWithSource()); err == nil || !strings.Contains(err.Error(), "repository is not known") {
		t.Fatalf("repo-less error = %v", err)
	}

	// The private key does not decrypt.
	sealed, db3 := prevcovAPI(t, nil)
	db3.rules["GetGithubAppByID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, sealed, "https://api.github.com", func(r *store.GithubApp) {
		r.AppPrivateKeyEnc = []byte("garbage")
	})}}
	if _, _, _, err := sealed.githubForApplication(ctx, prevcovAppRowWithSource()); err == nil {
		t.Fatal("undecryptable key must fail")
	}

	// The key decrypts to something that is not a PEM.
	notPEM, db4 := prevcovAPI(t, nil)
	db4.rules["GetGithubAppByID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, notPEM, "https://api.github.com", func(r *store.GithubApp) {
		r.AppPrivateKeyEnc = prevcovEncrypt(t, notPEM, "github_apps", "app_private_key_enc", []byte("not a pem"))
	})}}
	if _, _, _, err := notPEM.githubForApplication(ctx, prevcovAppRowWithSource()); err == nil {
		t.Fatal("non-PEM key must fail")
	}

	// GitHub refuses the token mint.
	refused := prevcovServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	closed, db5 := prevcovAPI(t, nil)
	db5.rules["GetGithubAppByID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, closed, refused.URL, nil)}}
	if _, _, _, err := closed.githubForApplication(ctx, prevcovAppRowWithSource()); err == nil || !strings.Contains(err.Error(), "installation token") {
		t.Fatalf("token refusal error = %v", err)
	}

	// Happy: client, token, full name.
	srv := prevcovServer(t, prevcovGithubOK(nil))
	ok, db6 := prevcovAPI(t, map[string]*prevcovRule{
		"GetRepositoryByID": {rows: []any{store.Repository{ID: 1, Uuid: prevcovUUIDv(), GitSourceID: 1, FullName: "acme/unit", CreatedAt: prevcovTS(), UpdatedAt: prevcovTS()}}},
	})
	db6.rules["GetGithubAppByID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, ok, srv.URL, nil)}}
	client, token, fullName, err := ok.githubForApplication(ctx, prevcovAppRowWithSource())
	if err != nil || client == nil || token != "inst-token" || fullName != "acme/unit" {
		t.Fatalf("githubForApplication = %v %q %q", err, token, fullName)
	}

	// A repository name without owner/name shape still resolves (unscoped token).
	flat, db7 := prevcovAPI(t, map[string]*prevcovRule{
		"GetRepositoryByID": {rows: []any{store.Repository{ID: 1, Uuid: prevcovUUIDv(), GitSourceID: 1, FullName: "flatname", CreatedAt: prevcovTS(), UpdatedAt: prevcovTS()}}},
	})
	db7.rules["GetGithubAppByID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, flat, srv.URL, nil)}}
	if _, _, _, err := flat.githubForApplication(ctx, prevcovAppRowWithSource()); err != nil {
		t.Fatalf("flat repository name: %v", err)
	}
}

func TestPrevcovListApplicationPullRequests(t *testing.T) {
	// No GitHub App source: conflict with the explanation.
	sourceless, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetGitSourceByID": {err: pgx.ErrNoRows},
	})
	rec := httptest.NewRecorder()
	sourceless.ListApplicationPullRequests(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID)
	prevcovStatus(t, rec, http.StatusConflict, "pull requests without app source")

	// GitHub fails the listing.
	failing := prevcovServer(t, prevcovGithubOK(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	broken, db := prevcovAPI(t, map[string]*prevcovRule{
		"GetRepositoryByID": {rows: []any{store.Repository{ID: 1, Uuid: prevcovUUIDv(), GitSourceID: 1, FullName: "acme/unit", CreatedAt: prevcovTS(), UpdatedAt: prevcovTS()}}},
	})
	db.rules["GetGithubAppByID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, broken, failing.URL, nil)}}
	rec = httptest.NewRecorder()
	broken.ListApplicationPullRequests(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID)
	prevcovStatus(t, rec, http.StatusConflict, "pull request list failure")

	// Happy: open PRs come back mapped.
	srv := prevcovServer(t, prevcovGithubOK(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/acme/unit/pulls") {
			_, _ = w.Write([]byte(`[{"number":7,"title":"Add unit","state":"open","draft":false,"head":{"ref":"feat","sha":"abc","repo":{"full_name":"fork/unit"}},"base":{"repo":{"full_name":"acme/unit"}}}]`))
			return
		}
		http.NotFound(w, r)
	}))
	ok, db2 := prevcovAPI(t, map[string]*prevcovRule{
		"GetRepositoryByID": {rows: []any{store.Repository{ID: 1, Uuid: prevcovUUIDv(), GitSourceID: 1, FullName: "acme/unit", CreatedAt: prevcovTS(), UpdatedAt: prevcovTS()}}},
	})
	db2.rules["GetGithubAppByID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, ok, srv.URL, nil)}}
	rec = httptest.NewRecorder()
	ok.ListApplicationPullRequests(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID)
	prevcovStatus(t, rec, http.StatusOK, "pull request list")
	if !strings.Contains(rec.Body.String(), `"is_fork":true`) {
		t.Fatalf("pull request body: %s", rec.Body.String())
	}
}

// prevcovPRServer answers the token mint and one PR read.
func prevcovPRServer(t *testing.T, prJSON string, status int) *httptest.Server {
	t.Helper()
	return prevcovServer(t, prevcovGithubOK(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/acme/unit/pulls/") {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(prJSON))
			return
		}
		http.NotFound(w, r)
	}))
}

func prevcovDeployPreviewAPI(t *testing.T, srvURL string, extra map[string]*prevcovRule) *API {
	t.Helper()
	a, db := prevcovAPI(t, extra)
	db.flow.truthy = true // previews_enabled on the application row
	db.rules["GetRepositoryByID"] = &prevcovRule{rows: []any{store.Repository{ID: 1, Uuid: prevcovUUIDv(), GitSourceID: 1, FullName: "acme/unit", CreatedAt: prevcovTS(), UpdatedAt: prevcovTS()}}}
	db.rules["GetGithubAppByID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, a, srvURL, nil)}}
	return a
}

func TestPrevcovDeployPreviewForPr(t *testing.T) {
	// Previews disabled on the application (flow default: previews_enabled=false).
	disabled, _ := prevcovAPI(t, nil)
	rec := httptest.NewRecorder()
	disabled.DeployPreviewForPr(rec, prevcovReq(http.MethodPost, "/x", `{"pr_id":9}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusConflict, "previews disabled")

	openPR := `{"number":9,"title":"t","state":"open","head":{"ref":"feat","sha":"abc","repo":{"full_name":"acme/unit"}},"base":{"repo":{"full_name":"acme/unit"}}}`

	// pr_id is mandatory and positive.
	srv := prevcovPRServer(t, openPR, http.StatusOK)
	badBody := prevcovDeployPreviewAPI(t, srv.URL, nil)
	rec = httptest.NewRecorder()
	badBody.DeployPreviewForPr(rec, prevcovReq(http.MethodPost, "/x", `{"pr_id":0}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusBadRequest, "missing pr_id")

	// The provider does not know the PR.
	gone := prevcovPRServer(t, `{"message":"Not Found"}`, http.StatusNotFound)
	missing := prevcovDeployPreviewAPI(t, gone.URL, nil)
	rec = httptest.NewRecorder()
	missing.DeployPreviewForPr(rec, prevcovReq(http.MethodPost, "/x", `{"pr_id":9}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusNotFound, "unknown pr")

	// Closed PRs do not deploy.
	closedSrv := prevcovPRServer(t, strings.Replace(openPR, `"state":"open"`, `"state":"closed"`, 1), http.StatusOK)
	closed := prevcovDeployPreviewAPI(t, closedSrv.URL, nil)
	rec = httptest.NewRecorder()
	closed.DeployPreviewForPr(rec, prevcovReq(http.MethodPost, "/x", `{"pr_id":9}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusConflict, "closed pr")

	// The application row read fails.
	appGone := prevcovDeployPreviewAPI(t, srv.URL, map[string]*prevcovRule{
		"GetApplicationByID": {err: errors.New("select failed")},
	})
	rec = httptest.NewRecorder()
	appGone.DeployPreviewForPr(rec, prevcovReq(http.MethodPost, "/x", `{"pr_id":9}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusInternalServerError, "application read failure")

	// The preview upsert fails.
	upsertBroken := prevcovDeployPreviewAPI(t, srv.URL, map[string]*prevcovRule{
		"UpsertPreview": {err: errors.New("insert failed")},
	})
	rec = httptest.NewRecorder()
	upsertBroken.DeployPreviewForPr(rec, prevcovReq(http.MethodPost, "/x", `{"pr_id":9}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusInternalServerError, "preview upsert failure")

	// Happy: the preview exists and its deployment is queued.
	ok := prevcovDeployPreviewAPI(t, srv.URL, map[string]*prevcovRule{
		"UpsertPreview":  {rows: []any{prevcovPreview(nil)}},
		"GetPreviewByID": {rows: []any{prevcovPreview(func(p *store.Preview) { p.Status = store.PreviewStatusDeploying })}},
	})
	rec = httptest.NewRecorder()
	ok.DeployPreviewForPr(rec, prevcovReq(http.MethodPost, "/x", `{"pr_id":9}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusAccepted, "preview deploy accepted")
	if !strings.Contains(rec.Body.String(), `"status":"deploying"`) {
		t.Fatalf("deploy response: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Remaining edges
// ---------------------------------------------------------------------------

// prevcovBrokenBody fails mid-read, exercising the delivery read error path.
type prevcovBrokenBody struct{}

func (prevcovBrokenBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (prevcovBrokenBody) Close() error             { return nil }

func TestPrevcovReceiveGitWebhookBodyReadFailure(t *testing.T) {
	a, _ := prevcovAPI(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github/"+fixtureUUID, nil)
	req.Body = prevcovBrokenBody{}
	req = prevcovChi(req, map[string]string{"provider": "github", "endpoint_uuid": fixtureUUID})
	rec := httptest.NewRecorder()
	a.ReceiveGitWebhook(rec, req)
	prevcovStatus(t, rec, http.StatusBadRequest, "unreadable delivery body")
}

func TestPrevcovCreateGithubAppLongNameIsTruncated(t *testing.T) {
	a, _ := prevcovAPI(t, nil)
	rec := httptest.NewRecorder()
	a.CreateGithubApp(rec, prevcovReq(http.MethodPost, "/github-apps",
		`{"name":"an-extremely-long-github-app-name-well-past-the-cap"}`))
	prevcovStatus(t, rec, http.StatusCreated, "long app name")
	var out api.GithubAppManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if name, _ := out.Manifest["name"].(string); len(name) > 34 {
		t.Fatalf("manifest name %q exceeds GitHub's 34-character cap", name)
	}
}

func TestPrevcovListGithubAppsPaginationErrors(t *testing.T) {
	a, _ := prevcovAPI(t, nil)

	limit := 0
	rec := httptest.NewRecorder()
	a.ListGithubApps(rec, prevcovReq(http.MethodGet, "/github-apps", ""), api.ListGithubAppsParams{Limit: &limit})
	if rec.Code < http.StatusBadRequest {
		t.Fatalf("invalid limit accepted: %d", rec.Code)
	}

	cursor := "!!!not-a-cursor"
	rec = httptest.NewRecorder()
	a.ListGithubApps(rec, prevcovReq(http.MethodGet, "/github-apps", ""), api.ListGithubAppsParams{Cursor: &cursor})
	if rec.Code < http.StatusBadRequest {
		t.Fatalf("invalid cursor accepted: %d", rec.Code)
	}
}

func TestPrevcovGetGithubAppBadUUID(t *testing.T) {
	a, _ := prevcovAPI(t, nil)
	rec := httptest.NewRecorder()
	a.GetGithubApp(rec, prevcovReq(http.MethodGet, "/x", ""), "not-a-uuid")
	prevcovStatus(t, rec, http.StatusNotFound, "invalid github app uuid")
}

func TestPrevcovListGithubAppRepositoriesRelistFails(t *testing.T) {
	srv := prevcovServer(t, prevcovGithubOK(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/installation/repositories") {
			_, _ = w.Write([]byte(`{"total_count":0,"repositories":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	a, db := prevcovAPI(t, map[string]*prevcovRule{
		// First read: empty cache, forcing the sync. Second read: failure.
		"ListRepositoriesForSource": {err: errors.New("select failed"), errAfter: 1},
	})
	db.rules["GetGithubAppByUUID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, a, srv.URL, nil)}}
	rec := httptest.NewRecorder()
	a.ListGithubAppRepositories(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.ListGithubAppRepositoriesParams{})
	prevcovStatus(t, rec, http.StatusInternalServerError, "relist failure")
}

func TestPrevcovListGithubAppRepositoriesClientBuildFails(t *testing.T) {
	// Installed but never converted: the sync cannot build a client.
	a, db := prevcovAPI(t, map[string]*prevcovRule{
		"ListRepositoriesForSource": {},
	})
	db.rules["GetGithubAppByUUID"] = &prevcovRule{rows: []any{prevcovGithubApp(t, a, "https://api.github.com", func(r *store.GithubApp) {
		r.AppID = nil
	})}}
	rec := httptest.NewRecorder()
	a.ListGithubAppRepositories(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, api.ListGithubAppRepositoriesParams{})
	prevcovStatus(t, rec, http.StatusInternalServerError, "unconverted app sync")
}

func TestPrevcovPreviewAuthorizeRootPathRedirect(t *testing.T) {
	a, _ := prevcovAPI(t, map[string]*prevcovRule{
		"GetPreviewByHost": {rows: []any{prevcovPreview(nil)}},
	})
	a.Sessions = &session.Manager{Store: a.Store}
	rec := httptest.NewRecorder()
	// No path at all: the callback next falls back to "/".
	a.PreviewAuthorize(rec, prevcovSessionReq("/webhooks/previews/authorize?redirect="+url.QueryEscape("https://pr-7.previews.example.test")))
	prevcovStatus(t, rec, http.StatusFound, "pathless authorize")
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if next := loc.Query().Get("next"); next != "/" {
		t.Fatalf("pathless next = %q", next)
	}
}

func TestPrevcovPreviewEndpointsWithUnknownPreview(t *testing.T) {
	rules := func() map[string]*prevcovRule {
		return map[string]*prevcovRule{"GetPreviewByUUIDForTeam": {err: pgx.ErrNoRows}}
	}
	// ApprovePreviewFork resolves inline; the others go through resolvePreview.
	a, _ := prevcovAPI(t, rules())
	rec := httptest.NewRecorder()
	a.ApprovePreviewFork(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusNotFound, "approve unknown preview")

	rec = httptest.NewRecorder()
	a.ApprovePreviewFork(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, "not-a-uuid")
	prevcovStatus(t, rec, http.StatusNotFound, "approve bad preview uuid")

	b, _ := prevcovAPI(t, rules())
	rec = httptest.NewRecorder()
	b.RedeployPreview(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID, api.RedeployPreviewParams{})
	prevcovStatus(t, rec, http.StatusNotFound, "redeploy unknown preview")

	c, _ := prevcovAPI(t, rules())
	rec = httptest.NewRecorder()
	c.GetPreviewLogs(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, fixtureUUID, api.GetPreviewLogsParams{})
	prevcovStatus(t, rec, http.StatusNotFound, "logs of unknown preview")

	d, _ := prevcovAPI(t, rules())
	rec = httptest.NewRecorder()
	d.CreatePreviewTerminalSession(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID, api.CreatePreviewTerminalSessionParams{})
	prevcovStatus(t, rec, http.StatusNotFound, "terminal of unknown preview")

	e, _ := prevcovAPI(t, rules())
	rec = httptest.NewRecorder()
	e.KeepPreview(rec, prevcovReq(http.MethodPost, "/x", "{}"), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusNotFound, "keep unknown preview")

	f, _ := prevcovAPI(t, rules())
	rec = httptest.NewRecorder()
	f.ListPreviewEnvs(rec, prevcovReq(http.MethodGet, "/x", ""), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusNotFound, "envs of unknown preview")

	g, _ := prevcovAPI(t, rules())
	rec = httptest.NewRecorder()
	g.CreatePreviewEnv(rec, prevcovReq(http.MethodPost, "/x", `{"key":"K","value":"v"}`), fixtureUUID, fixtureUUID)
	prevcovStatus(t, rec, http.StatusNotFound, "env for unknown preview")
}

func TestPrevcovDeployPreviewForPrForkAwaitsApproval(t *testing.T) {
	openPR := `{"number":9,"title":"t","state":"open","head":{"ref":"feat","sha":"abc","repo":{"full_name":"fork/unit"}},"base":{"repo":{"full_name":"acme/unit"}}}`
	srv := prevcovPRServer(t, openPR, http.StatusOK)
	a := prevcovDeployPreviewAPI(t, srv.URL, map[string]*prevcovRule{
		"UpsertPreview":  {rows: []any{prevcovPreview(func(p *store.Preview) { p.IsFork = true })}},
		"GetPreviewByID": {rows: []any{prevcovPreview(func(p *store.Preview) { p.IsFork = true })}},
	})
	rec := httptest.NewRecorder()
	a.DeployPreviewForPr(rec, prevcovReq(http.MethodPost, "/x", `{"pr_id":9}`), fixtureUUID)
	prevcovStatus(t, rec, http.StatusAccepted, "fork preview recorded")
	if !strings.Contains(rec.Body.String(), `"is_fork":true`) {
		t.Fatalf("fork response: %s", rec.Body.String())
	}
}
