package session

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/password"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

func validPasswordUser(t *testing.T) store.User {
	t.Helper()
	hash, err := password.Hash("correct password")
	if err != nil {
		t.Fatal(err)
	}
	return store.User{ID: 10, Uuid: pguuid.MustParse("11111111-2222-4333-8444-555555555555"),
		Email: "user@example.test", Name: "User", PasswordHash: &hash}
}

func loginRequest() *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	request.RemoteAddr = "192.0.2.10:4321"
	request.Header.Set("User-Agent", "unit-test")
	return request
}

func TestLoginFailureModes(t *testing.T) {
	user := validPasswordUser(t)
	t.Run("unknown account", func(t *testing.T) {
		manager := &Manager{Store: &fakeSessionStore{errs: map[string]error{"userByEmail": errors.New("missing")}}}
		if _, _, err := manager.Login(context.Background(), loginRequest(), " Nobody@Example.test ", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("Login = %v", err)
		}
	})
	t.Run("locked", func(t *testing.T) {
		locked := user
		locked.LockedUntil = pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
		manager := &Manager{Store: &fakeSessionStore{user: locked}}
		if _, _, err := manager.Login(context.Background(), loginRequest(), locked.Email, "correct password"); !errors.Is(err, ErrAccountLocked) {
			t.Fatalf("Login = %v", err)
		}
	})
	t.Run("oauth-only and wrong password count failures", func(t *testing.T) {
		for _, candidate := range []store.User{
			{ID: 11, Email: "oauth@example.test"},
			user,
		} {
			database := &fakeSessionStore{user: candidate}
			manager := &Manager{Store: database}
			if _, _, err := manager.Login(context.Background(), loginRequest(), candidate.Email, "wrong"); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("Login = %v", err)
			}
			if len(database.failedLogins) != 1 ||
				database.failedLogins[0].MaxAttempts != MaxFailedLogins {
				t.Fatalf("failed logins = %#v", database.failedLogins)
			}
		}
	})
	t.Run("failure counter write", func(t *testing.T) {
		database := &fakeSessionStore{
			user: user, errs: map[string]error{"recordFailed": errors.New("database")},
		}
		if _, _, err := (&Manager{Store: database}).Login(
			context.Background(), loginRequest(), user.Email, "wrong",
		); err == nil || errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("database error = %v", err)
		}
	})
}

func TestLoginSuccessAndMFAChallenge(t *testing.T) {
	user := validPasswordUser(t)
	t.Run("session", func(t *testing.T) {
		database := &fakeSessionStore{
			user: user,
			membership: store.GetTeamMembershipForUserRow{
				TeamID: 20, TeamUuid: pguuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"),
				Role: store.TeamRoleAdmin,
			},
		}
		session, token, err := (&Manager{Store: database}).Login(
			context.Background(), loginRequest(), " USER@EXAMPLE.TEST ", "correct password",
		)
		if err != nil || session.TeamID != 20 || token == "" ||
			len(database.clearedLogins) != 1 || len(database.sessionCreates) != 1 {
			t.Fatalf("Login = %#v, token=%q, err=%v, database=%#v", session, token, err, database)
		}
	})
	t.Run("clear failures error", func(t *testing.T) {
		database := &fakeSessionStore{
			user: user, errs: map[string]error{"clearFailed": errors.New("database")},
		}
		if _, _, err := (&Manager{Store: database}).Login(
			context.Background(), loginRequest(), user.Email, "correct password",
		); err == nil {
			t.Fatal("clear failure error was hidden")
		}
	})
	t.Run("confirmed TOTP creates challenge only", func(t *testing.T) {
		database := &fakeSessionStore{
			user: user,
			factor: store.MfaFactor{
				ConfirmedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			},
		}
		session, challenge, err := (&Manager{Store: database}).Login(
			context.Background(), loginRequest(), user.Email, "correct password",
		)
		if session != nil || challenge == "" || !errors.Is(err, ErrMFARequired) ||
			len(database.challengeCreates) != 1 || len(database.sessionCreates) != 0 {
			t.Fatalf("MFA Login = %#v, %q, %v", session, challenge, err)
		}
	})
}

func TestOpenSessionBranches(t *testing.T) {
	user := store.User{ID: 1, Email: "user@example.test", Name: "User"}
	t.Run("no team", func(t *testing.T) {
		manager := &Manager{Store: &fakeSessionStore{errs: map[string]error{"membership": errors.New("missing")}}}
		if _, _, err := manager.Open(context.Background(), loginRequest(), user); err == nil ||
			!strings.Contains(err.Error(), "no team") {
			t.Fatalf("Open = %v", err)
		}
	})
	t.Run("create error", func(t *testing.T) {
		database := &fakeSessionStore{
			membership: store.GetTeamMembershipForUserRow{TeamID: 2},
			errs:       map[string]error{"createSession": errors.New("insert")},
		}
		if _, _, err := (&Manager{Store: database}).Open(context.Background(), loginRequest(), user); err == nil {
			t.Fatal("session insert error was hidden")
		}
	})
	t.Run("success captures metadata", func(t *testing.T) {
		database := &fakeSessionStore{
			membership: store.GetTeamMembershipForUserRow{TeamID: 2, Role: store.TeamRoleOwner},
			session:    store.Session{ID: 3},
		}
		session, token, err := (&Manager{Store: database}).Open(context.Background(), loginRequest(), user)
		if err != nil || token == "" || session.ID != 3 || session.CSRFToken == "" {
			t.Fatalf("Open = %#v, %q, %v", session, token, err)
		}
		arg := database.sessionCreates[0]
		if arg.Ip == nil || arg.Ip.String() != "192.0.2.10" ||
			arg.UserAgent == nil || *arg.UserAgent != "unit-test" ||
			arg.CsrfToken == nil || arg.TokenHash == token {
			t.Fatalf("CreateSession = %#v", arg)
		}
	})
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestTokenEntropyFailure(t *testing.T) {
	old := randomReader
	randomReader = failingReader{err: errors.New("entropy unavailable")}
	defer func() { randomReader = old }()

	if token, hash, err := newToken(); err == nil || token != "" || hash != "" {
		t.Fatalf("newToken = %q, %q, %v", token, hash, err)
	}
	database := &fakeSessionStore{membership: store.GetTeamMembershipForUserRow{TeamID: 1}}
	if _, _, err := (&Manager{Store: database}).Open(
		context.Background(), loginRequest(), store.User{ID: 1},
	); err == nil {
		t.Fatal("Open hid entropy failure")
	}
	if _, err := (&Manager{Store: database}).CreateChallenge(context.Background(), 1); err == nil {
		t.Fatal("CreateChallenge hid entropy failure")
	}
}

func TestSessionLookupAndAuthentication(t *testing.T) {
	uuid := pguuid.MustParse("11111111-2222-4333-8444-555555555555")
	teamUUID := pguuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	current := int64(9)
	database := &fakeSessionStore{
		sessionRow: store.GetSessionByTokenHashRow{
			ID: 3, Uuid: uuid, UserID: 4, CurrentTeamID: &current,
		},
		membership: store.GetTeamMembershipForUserRow{
			// An instance-root user (users.is_root): its session carries the coarse
			// `root` wildcard (ADR-038 §1), which is what IsRoot() below checks.
			TeamID: 5, TeamUuid: teamUUID, Role: store.TeamRoleAdmin, IsRoot: true,
		},
	}
	manager := &Manager{Store: database}

	noCookie := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := manager.SessionFromRequest(context.Background(), noCookie); err == nil ||
		manager.Authenticate(context.Background(), noCookie) != nil {
		t.Fatal("missing cookie was authenticated")
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: CookieName, Value: "clear"})
	row, err := manager.SessionFromRequest(context.Background(), request)
	if err != nil || row.ID != 3 {
		t.Fatalf("SessionFromRequest = %#v, %v", row, err)
	}
	identity := manager.Authenticate(context.Background(), request)
	if identity == nil || identity.TeamID != current || identity.TeamUUID == "" ||
		!identity.IsRoot() || !identity.Session || len(database.touchedSessions) != 1 {
		t.Fatalf("Authenticate = %#v", identity)
	}

	database.sessionRow.CurrentTeamID = nil
	identity = manager.Authenticate(context.Background(), request)
	if identity == nil || identity.TeamID != database.membership.TeamID {
		t.Fatalf("fallback team identity = %#v", identity)
	}
	database.errs = map[string]error{"getSession": errors.New("expired")}
	if _, err := manager.SessionFromRequest(context.Background(), request); err == nil ||
		manager.Authenticate(context.Background(), request) != nil {
		t.Fatal("expired session was authenticated")
	}
	database.errs = map[string]error{"membership": errors.New("removed")}
	if manager.Authenticate(context.Background(), request) != nil {
		t.Fatal("user without membership was authenticated")
	}
}

// PermissionsForRole returns GRANULAR sets now (ADR-038), so the test checks the
// structural properties of the model rather than pinning an exact 70-item list
// (which would be a brittle copy of the code): admin holds every team-scoped
// permission and nothing instance-scoped; owner maps onto the same admin set;
// member is a strict subset that manages resources but not the team; reviewer
// sees previews only.
func TestPermissionsForEveryRole(t *testing.T) {
	admin := PermissionsForRole(store.TeamRoleAdmin)
	owner := PermissionsForRole(store.TeamRoleOwner)
	member := PermissionsForRole(store.TeamRoleMember)
	reviewer := PermissionsForRole(store.TeamRoleReviewer)

	has := func(set []string, perm string) bool { return slices.Contains(set, perm) }

	// admin == owner == every catalogue permission except the instance-scoped ones.
	if !reflect.DeepEqual(admin, owner) {
		t.Errorf("owner must map onto the admin set, got %v vs %v", owner, admin)
	}
	for name, socle := range auth.Catalog {
		if socle == auth.PermRoot {
			if has(admin, name) {
				t.Errorf("admin must not hold instance-scoped %q", name)
			}
			continue
		}
		if !has(admin, name) {
			t.Errorf("admin must hold every team permission, missing %q", name)
		}
	}

	// member manages resources but never administers the team or infrastructure.
	for _, want := range []string{"applications:update", "databases:create", "secrets:write", "previews:manage", "deployments:cancel"} {
		if !has(member, want) {
			t.Errorf("member should hold %q", want)
		}
	}
	for _, forbidden := range []string{"team:manage", "members:manage", "roles:manage", "tokens:create", "servers:manage", "keys:manage", "instance:manage"} {
		if has(member, forbidden) {
			t.Errorf("member must NOT hold %q", forbidden)
		}
	}
	// Every member permission is also an admin permission (member ⊆ admin).
	for _, p := range member {
		if !has(admin, p) {
			t.Errorf("member permission %q leaks outside the admin set", p)
		}
	}

	// reviewer sees PR previews and nothing else.
	if !reflect.DeepEqual(reviewer, []string{"previews:read"}) {
		t.Errorf("reviewer = %v, want just [previews:read]", reviewer)
	}
}

func TestCSRFFullDecisionTable(t *testing.T) {
	csrf := "csrf-secret"
	base := func() *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/", nil)
		request.AddCookie(&http.Cookie{Name: CookieName, Value: "session"})
		return request
	}
	for _, tc := range []struct {
		name string
		db   *fakeSessionStore
		edit func(*http.Request)
		ok   bool
	}{
		{"store error", &fakeSessionStore{errs: map[string]error{"getSession": errors.New("x")}}, nil, false},
		{"missing stored token", &fakeSessionStore{}, nil, false},
		{"missing header", &fakeSessionStore{sessionRow: store.GetSessionByTokenHashRow{CsrfToken: &csrf}}, nil, false},
		{"wrong header", &fakeSessionStore{sessionRow: store.GetSessionByTokenHashRow{CsrfToken: &csrf}}, func(r *http.Request) {
			r.Header.Set(CSRFHeader, "wrong")
		}, false},
		{"valid", &fakeSessionStore{sessionRow: store.GetSessionByTokenHashRow{CsrfToken: &csrf}}, func(r *http.Request) {
			r.Header.Set(CSRFHeader, csrf)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := base()
			if tc.edit != nil {
				tc.edit(request)
			}
			err := (&Manager{Store: tc.db}).VerifyCSRF(context.Background(), request)
			if (err == nil) != tc.ok {
				t.Fatalf("VerifyCSRF = %v", err)
			}
		})
	}
	empty := httptest.NewRequest(http.MethodPost, "/", nil)
	empty.AddCookie(&http.Cookie{Name: CookieName})
	if err := (&Manager{}).VerifyCSRF(context.Background(), empty); err != nil {
		t.Fatalf("empty cookie should behave as absent: %v", err)
	}
}

func TestCookieWritingClearingAndLogout(t *testing.T) {
	manager := &Manager{Secure: true}
	writer := httptest.NewRecorder()
	manager.SetCookies(writer, "session", "csrf")
	response := writer.Result()
	cookies := response.Cookies()
	if len(cookies) != 2 || !cookies[0].HttpOnly || cookies[1].HttpOnly ||
		!cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookies = %#v", cookies)
	}

	writer = httptest.NewRecorder()
	manager.ClearCookies(writer)
	cookies = writer.Result().Cookies()
	if len(cookies) != 2 || cookies[0].MaxAge != -1 || cookies[1].MaxAge != -1 {
		t.Fatalf("cleared cookies = %#v", cookies)
	}

	database := &fakeSessionStore{sessionRow: store.GetSessionByTokenHashRow{ID: 12}}
	manager.Store = database
	manager.Logout(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil))
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.AddCookie(&http.Cookie{Name: CookieName, Value: "token"})
	manager.Logout(context.Background(), request)
	if !reflect.DeepEqual(database.revokedSessions, []int64{12}) {
		t.Fatalf("revoked = %v", database.revokedSessions)
	}
	database.errs = map[string]error{"getSession": errors.New("gone")}
	manager.Logout(context.Background(), request)
}

func TestTokenAndRequestHelpers(t *testing.T) {
	token, hash, err := newToken()
	if err != nil || token == "" || hash != hashToken(token) || token == hash {
		t.Fatalf("newToken = %q, %q, %v", token, hash, err)
	}
	if clientIP(&http.Request{RemoteAddr: "bad"}) != nil ||
		clientIP(&http.Request{RemoteAddr: "bad:80"}) != nil {
		t.Fatal("invalid remote address parsed")
	}
	request := &http.Request{RemoteAddr: "[2001:db8::1]:443"}
	if got := clientIP(request); got == nil || got.String() != "2001:db8::1" {
		t.Fatalf("clientIP = %v", got)
	}
	if uuidString(pgtype.UUID{}) != "" || uuidString(pguuid.MustParse("11111111-2222-4333-8444-555555555555")) == "" {
		t.Fatal("uuidString failed")
	}
	if got := ptr("x"); got == nil || *got != "x" {
		t.Fatal("ptr failed")
	}
	if _, err := io.ReadAll(strings.NewReader(token)); err != nil {
		t.Fatal(err)
	}
}

// A Secure cookie set over plain HTTP is stored and then never sent back: the
// login must be refused before that happens, not diagnosed from a 401 loop.
func TestCookiesWouldBeDropped(t *testing.T) {
	plain := httptest.NewRequest(http.MethodPost, "http://manager.example/auth/login", nil)
	forwarded := httptest.NewRequest(http.MethodPost, "http://manager.example/auth/login", nil)
	forwarded.Header.Set("X-Forwarded-Proto", "https")
	tls := httptest.NewRequest(http.MethodPost, "https://manager.example/auth/login", nil)

	insecure := &Manager{Secure: false}
	secure := &Manager{Secure: true}

	if insecure.CookiesWouldBeDropped(plain) {
		t.Fatal("non-Secure cookies survive plain HTTP — nothing to refuse")
	}
	if !secure.CookiesWouldBeDropped(plain) {
		t.Fatal("Secure cookie over plain HTTP is undeliverable and must be refused")
	}
	if secure.CookiesWouldBeDropped(forwarded) {
		t.Fatal("X-Forwarded-Proto: https marks TLS terminated upstream — deliverable")
	}
	if secure.CookiesWouldBeDropped(tls) {
		t.Fatal("a direct TLS request is deliverable")
	}

	// Loopback is the emergency door (ssh -L when the TLS front is down):
	// browsers deliver Secure cookies on http://localhost, so the guard must
	// let it through.
	for _, target := range []string{
		"http://localhost:8080/auth/login",
		"http://127.0.0.1:8080/auth/login",
		"http://[::1]:8080/auth/login",
		"http://app.localhost/auth/login",
	} {
		if secure.CookiesWouldBeDropped(httptest.NewRequest(http.MethodPost, target, nil)) {
			t.Fatalf("%s is a secure context — the emergency door must stay open", target)
		}
	}
}
