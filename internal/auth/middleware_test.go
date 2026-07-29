package auth

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

type fakeSessions struct {
	identity *Identity
	csrfErr  error
}

func (f *fakeSessions) Authenticate(context.Context, *http.Request) *Identity { return f.identity }
func (f *fakeSessions) VerifyCSRF(context.Context, *http.Request) error       { return f.csrfErr }

type fakeSettings struct {
	settings store.InstanceSetting
	err      error
}

func (f *fakeSettings) Get(context.Context) (store.InstanceSetting, error) {
	return f.settings, f.err
}

type fakeTokenStore struct {
	get   func(string) ([]store.GetActiveApiTokensByPrefixRow, error)
	touch func(int64) error
	// authority is what the token's creator holds; zero value means "no such
	// member", which caps the token at nothing (rbac-matrix §4.2).
	authority *store.GetTokenCreatorAuthorityRow
}

func (f *fakeTokenStore) GetActiveApiTokensByPrefix(_ context.Context, prefix string) ([]store.GetActiveApiTokensByPrefixRow, error) {
	return f.get(prefix)
}

func (f *fakeTokenStore) TouchApiTokenLastUsed(_ context.Context, id int64) error {
	if f.touch == nil {
		return nil
	}
	return f.touch(id)
}

func (f *fakeTokenStore) GetTokenCreatorAuthority(context.Context, store.GetTokenCreatorAuthorityParams) (store.GetTokenCreatorAuthorityRow, error) {
	if f.authority == nil {
		return store.GetTokenCreatorAuthorityRow{}, pgx.ErrNoRows
	}
	return *f.authority, nil
}

func authLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The api_enabled gate governs the public token API (PRD §10.3), not the
// dashboard: a session must ride through it, or the settings page that
// re-enables the API would itself be unreachable.
func TestApiGateExempt(t *testing.T) {
	session := &Identity{Session: true}
	token := &Identity{}

	if !apiGateExempt(session, "/api/v1/servers") {
		t.Error("a dashboard session must bypass the api_enabled gate")
	}
	if apiGateExempt(token, "/api/v1/servers") {
		t.Error("a bearer token must be subject to the api_enabled gate")
	}
	if !apiGateExempt(token, "/api/v1/system/api/enable") {
		t.Error("the re-enable endpoint must stay reachable for tokens")
	}
}

func TestHandlerSessionPassesWhileApiDisabled(t *testing.T) {
	m := &Middleware{
		Settings: &fakeSettings{settings: store.InstanceSetting{ApiEnabled: false}},
		Sessions: &fakeSessions{identity: &Identity{TeamID: 1, Session: true}},
	}

	reached := false
	handler := m.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if id, ok := FromContext(r.Context()); !ok || !id.Session {
			t.Error("the session identity must be attached to the context")
		}
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil))

	if !reached {
		t.Fatalf("a session request must pass while the API is disabled, got status %d: %s",
			rec.Code, rec.Body.String())
	}
}

func TestHandlerUnauthenticatedIs401(t *testing.T) {
	m := &Middleware{
		Settings: &fakeSettings{settings: store.InstanceSetting{ApiEnabled: false}},
		Sessions: &fakeSessions{identity: nil},
	}

	handler := m.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an unauthenticated request must not reach the handler")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandlerHealthStaysOpen(t *testing.T) {
	m := &Middleware{
		Settings: &fakeSettings{settings: store.InstanceSetting{ApiEnabled: false}},
		Sessions: &fakeSessions{identity: nil},
	}

	reached := false
	handler := m.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if !reached {
		t.Fatalf("health must stay reachable, got status %d", rec.Code)
	}
}

func TestHandlerRejectsSessionCSRF(t *testing.T) {
	m := &Middleware{
		Settings: &fakeSettings{settings: store.InstanceSetting{ApiEnabled: true}},
		Sessions: &fakeSessions{
			identity: &Identity{TeamID: 1, Session: true},
			csrfErr:  errors.New("mismatch"),
		},
	}
	rec := httptest.NewRecorder()
	m.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid CSRF request reached the handler")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/apps", nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "csrf_failed") {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
}

func tokenFixture(t *testing.T) (string, store.GetActiveApiTokensByPrefixRow) {
	t.Helper()
	token, prefix, _, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return token, store.GetActiveApiTokensByPrefixRow{
		ID:          7,
		Uuid:        pguuid.MustParse("11111111-1111-4111-8111-111111111111"),
		TeamID:      42,
		TeamUuid:    pguuid.MustParse("22222222-2222-4222-8222-222222222222"),
		TokenPrefix: prefix,
		TokenHash:   HashToken(token),
		Permissions: []string{"read", "deploy"},
		LastUsedAt:  pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
}

func authenticateRequest(t *testing.T, middleware *Middleware, token, remoteAddr string) (*Identity, *httptest.ResponseRecorder) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
	request.RemoteAddr = remoteAddr
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	return middleware.authenticate(rec, request), rec
}

func TestAuthenticateBearerToken(t *testing.T) {
	token, row := tokenFixture(t)
	storeFake := &fakeTokenStore{
		get: func(prefix string) ([]store.GetActiveApiTokensByPrefixRow, error) {
			if prefix != row.TokenPrefix {
				t.Fatalf("prefix = %q", prefix)
			}
			return []store.GetActiveApiTokensByPrefixRow{row}, nil
		},
	}
	middleware := &Middleware{Store: storeFake, Logger: authLogger()}
	identity, rec := authenticateRequest(t, middleware, token, "192.0.2.10:5000")
	if identity == nil || identity.TokenID != 7 || identity.TeamID != 42 ||
		identity.TokenUUID != "11111111-1111-4111-8111-111111111111" ||
		identity.TeamUUID != "22222222-2222-4222-8222-222222222222" ||
		rec.Code != http.StatusOK {
		t.Fatalf("identity = %+v, response = %d %q", identity, rec.Code, rec.Body.String())
	}
	// The token's coarse scopes {read, deploy} are kept AND expanded to the
	// granular set they hold (ADR-038): both coarse and granular checks pass.
	for _, want := range []string{"read", "deploy", "applications:read", "applications:deploy"} {
		if !slices.Contains(identity.Permissions, want) {
			t.Errorf("identity missing expected permission %q; got %v", want, identity.Permissions)
		}
	}
	// A read/deploy token holds no write or reveal permission.
	for _, forbidden := range []string{"write", "applications:update", "secrets:reveal"} {
		if slices.Contains(identity.Permissions, forbidden) {
			t.Errorf("identity should not hold %q; got %v", forbidden, identity.Permissions)
		}
	}
}

func TestAuthenticateBearerFailures(t *testing.T) {
	token, valid := tokenFixture(t)
	expired := valid
	expired.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}
	wrongHash := valid
	wrongHash.TokenHash = HashToken("different")
	restricted := valid
	restricted.IpAllowlist = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}

	tests := []struct {
		name   string
		rows   []store.GetActiveApiTokensByPrefixRow
		getErr error
		status int
	}{
		{name: "store failure", getErr: errors.New("database unavailable"), status: http.StatusInternalServerError},
		{name: "no matching hash", rows: []store.GetActiveApiTokensByPrefixRow{wrongHash}, status: http.StatusUnauthorized},
		{name: "expired", rows: []store.GetActiveApiTokensByPrefixRow{expired}, status: http.StatusUnauthorized},
		{name: "IP denied", rows: []store.GetActiveApiTokensByPrefixRow{restricted}, status: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			middleware := &Middleware{
				Store: &fakeTokenStore{get: func(string) ([]store.GetActiveApiTokensByPrefixRow, error) {
					return test.rows, test.getErr
				}},
				Logger: authLogger(),
			}
			identity, rec := authenticateRequest(t, middleware, token, "192.0.2.10:5000")
			if identity != nil || rec.Code != test.status {
				t.Fatalf("identity = %+v, response = %d %q", identity, rec.Code, rec.Body.String())
			}
			if test.status == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("401 response is missing WWW-Authenticate")
			}
		})
	}
}

func TestAuthenticateAllowlistAndLazyTouch(t *testing.T) {
	token, row := tokenFixture(t)
	row.IpAllowlist = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	row.LastUsedAt = pgtype.Timestamptz{}
	touched := make(chan int64, 1)
	middleware := &Middleware{
		Store: &fakeTokenStore{
			get: func(string) ([]store.GetActiveApiTokensByPrefixRow, error) {
				return []store.GetActiveApiTokensByPrefixRow{row}, nil
			},
			touch: func(id int64) error {
				touched <- id
				return errors.New("best-effort update failed")
			},
		},
		Logger: authLogger(),
	}
	identity, _ := authenticateRequest(t, middleware, token, "192.0.2.15:5000")
	if identity == nil {
		t.Fatal("allowlisted address was rejected")
	}
	select {
	case id := <-touched:
		if id != row.ID {
			t.Fatalf("touched token %d", id)
		}
	case <-time.After(time.Second):
		t.Fatal("lazy last_used_at update did not run")
	}
}

func TestHandlerBearerAPIGate(t *testing.T) {
	token, row := tokenFixture(t)
	storeFake := &fakeTokenStore{get: func(string) ([]store.GetActiveApiTokensByPrefixRow, error) {
		return []store.GetActiveApiTokensByPrefixRow{row}, nil
	}}
	for _, test := range []struct {
		name     string
		path     string
		settings *fakeSettings
		status   int
		reached  bool
	}{
		{name: "enabled", path: "/api/v1/apps", settings: &fakeSettings{settings: store.InstanceSetting{ApiEnabled: true}}, status: http.StatusNoContent, reached: true},
		{name: "disabled", path: "/api/v1/apps", settings: &fakeSettings{settings: store.InstanceSetting{ApiEnabled: false}}, status: http.StatusForbidden},
		{name: "settings failure", path: "/api/v1/apps", settings: &fakeSettings{err: errors.New("database unavailable")}, status: http.StatusInternalServerError},
		{name: "enable endpoint exempt", path: "/api/v1/system/api/enable", settings: &fakeSettings{settings: store.InstanceSetting{ApiEnabled: false}}, status: http.StatusNoContent, reached: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reached := false
			middleware := &Middleware{Store: storeFake, Settings: test.settings, Logger: authLogger()}
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			middleware.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				if _, ok := FromContext(r.Context()); !ok {
					t.Fatal("identity not attached")
				}
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rec, request)
			if rec.Code != test.status || reached != test.reached {
				t.Fatalf("response = %d reached=%v body=%q", rec.Code, reached, rec.Body.String())
			}
		})
	}
}

func TestIPAllowed(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.1:1234"
	if !ipAllowed(request, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}) {
		t.Fatal("address inside prefix was rejected")
	}
	request.RemoteAddr = "not-an-ip"
	if ipAllowed(request, []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}) {
		t.Fatal("malformed address was allowed")
	}
	request.RemoteAddr = "[::ffff:192.0.2.1]:1234"
	if !ipAllowed(request, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}) {
		t.Fatal("IPv4-mapped IPv6 address was not unmapped")
	}
}

func TestAuthUtilities(t *testing.T) {
	if got := uuidString(pgtype.UUID{}); got != "" {
		t.Fatalf("invalid UUID = %q", got)
	}
	if got := uuidString(pguuid.MustParse("11111111-1111-4111-8111-111111111111")); got != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("UUID = %q", got)
	}
	ctx, cancel := contextWithTimeout()
	defer cancel()
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
		t.Fatalf("unexpected timeout: %v, %v", deadline, ok)
	}
}

// A token never grants more than its creator holds, re-evaluated on every
// request (rbac-matrix §4.2, ADR-046 §7). Before this, a token was an access
// that outlived the authority that produced it: demote its creator, remove them
// from the team, scope them down — the token kept everything it was minted
// with. It was also the side door out of scoped assignments.
func TestTokenIsCappedByItsCreator(t *testing.T) {
	creator := int64(5)
	tokenRow := func() store.GetActiveApiTokensByPrefixRow {
		row := store.GetActiveApiTokensByPrefixRow{}
		row.ID, row.TeamID, row.CreatedBy = 1, 10, &creator
		// deploy included on purpose: without it the token never held
		// applications:deploy in the first place (its socle is `deploy`, not
		// `write`), and the assertions below would pass while proving nothing.
		row.Permissions = []string{string(PermRead), string(PermWrite), string(PermDeploy)}
		return row
	}

	t.Run("a demoted creator narrows the token", func(t *testing.T) {
		row := tokenRow()
		id := &Identity{TeamID: row.TeamID, Permissions: EffectivePermissions(row.Permissions)}
		m := &Middleware{Store: &fakeTokenStore{
			authority: &store.GetTokenCreatorAuthorityRow{Role: store.TeamRoleReviewer, UserID: creator},
		}}
		m.boundToCreator(httptest.NewRequest("GET", "/", nil), id, &row, id.Permissions)

		if Has(id.Permissions, PermApplicationsDeploy) {
			t.Error("a reviewer's token must not keep deploy")
		}
		if Has(id.Permissions, PermApplicationsUpdate) {
			t.Error("nor any write the creator no longer holds")
		}
	})

	t.Run("a creator who left the team empties the token", func(t *testing.T) {
		row := tokenRow()
		id := &Identity{TeamID: row.TeamID, Permissions: EffectivePermissions(row.Permissions)}
		m := &Middleware{Store: &fakeTokenStore{}} // no membership row
		m.boundToCreator(httptest.NewRequest("GET", "/", nil), id, &row, id.Permissions)

		if len(id.Permissions) != 0 {
			t.Errorf("a token whose creator left holds nothing, got %v", id.Permissions)
		}
	})

	t.Run("a token keeps its own permissions when no creator is recorded", func(t *testing.T) {
		row := tokenRow()
		row.CreatedBy = nil
		id := &Identity{TeamID: row.TeamID, Permissions: EffectivePermissions(row.Permissions)}
		m := &Middleware{Store: &fakeTokenStore{}}
		m.boundToCreator(httptest.NewRequest("GET", "/", nil), id, &row, id.Permissions)

		if !Has(id.Permissions, PermApplicationsDeploy) {
			t.Error("a token minted before the column existed must not be broken by this rule")
		}
	})
}
