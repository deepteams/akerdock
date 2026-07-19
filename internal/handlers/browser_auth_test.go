package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

func TestNewRouterMountsHealthAndBrowserRoutes(t *testing.T) {
	a, _ := flowAPI(t)
	mw := &auth.Middleware{
		Store: a.Store, Settings: a.Settings, Logger: a.Logger,
	}
	router := NewRouter(a, mw)

	health := httptest.NewRecorder()
	router.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d, body = %s", health.Code, health.Body.String())
	}

	login := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{}`))
	req.RemoteAddr = "192.0.2.10:1234"
	router.ServeHTTP(login, req)
	if login.Code != http.StatusNotFound {
		t.Fatalf("disabled browser auth status = %d, body = %s", login.Code, login.Body.String())
	}
}

func TestBrowserHandlersHideDisabledFeatures(t *testing.T) {
	a, _ := flowAPI(t)
	tests := []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		want int
	}{
		{"login", a.Login, http.StatusNotFound},
		{"logout", a.Logout, http.StatusNotFound},
		{"me", a.Me, http.StatusNotFound},
		{"mfa verify", a.VerifyMFALogin, http.StatusNotFound},
		{"mfa status", a.MFAStatus, http.StatusNotFound},
		{"mfa setup", a.SetupMFATOTP, http.StatusNotFound},
		{"mfa confirm", a.ConfirmMFATOTP, http.StatusNotFound},
		{"mfa disable", a.DisableMFATOTP, http.StatusNotFound},
		{"mfa recovery", a.RegenerateMFARecoveryCodes, http.StatusNotFound},
		{"passkey register begin", a.BeginPasskeyRegistration, http.StatusNotFound},
		{"passkey register finish", a.FinishPasskeyRegistration, http.StatusNotFound},
		{"passkey list", a.ListPasskeys, http.StatusNotFound},
		{"passkey delete", a.DeletePasskey, http.StatusNotFound},
		{"passkey login begin", a.BeginPasskeyLogin, http.StatusNotFound},
		{"passkey login finish", a.FinishPasskeyLogin, http.StatusNotFound},
		{"passkey stepup begin", a.BeginPasskeyStepUp, http.StatusNotFound},
		{"passkey stepup finish", a.FinishPasskeyStepUp, http.StatusNotFound},
		{"oauth start", a.StartOauth, http.StatusNotFound},
		{"oauth callback", a.OauthCallback, http.StatusNotFound},
		{"identity list", a.ListIdentities, http.StatusNotFound},
		{"identity delete", a.DeleteIdentity, http.StatusNotFound},
		{"oauth provider list remains public", a.OauthProviders, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/auth/test", strings.NewReader(`{}`))
			tt.call(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

func TestBrowserSessionAndCSRFGates(t *testing.T) {
	a, _ := flowAPI(t)
	a.Sessions = &session.Manager{Store: a.Store}

	t.Run("login rejects malformed json", func(t *testing.T) {
		rec := httptest.NewRecorder()
		a.Login(rec, httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader("{")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("login keeps invalid credentials opaque", func(t *testing.T) {
		rec := httptest.NewRecorder()
		a.Login(rec, httptest.NewRequest(http.MethodPost, "/auth/login",
			strings.NewReader(`{"email":"unit@example.test","password":"wrong"}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("me needs active cookie", func(t *testing.T) {
		rec := httptest.NewRecorder()
		a.Me(rec, httptest.NewRequest(http.MethodGet, "/auth/me", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("logout always clears cookies", func(t *testing.T) {
		rec := httptest.NewRecorder()
		a.Logout(rec, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		if len(rec.Result().Cookies()) < 2 {
			t.Fatal("logout did not expire both session cookies")
		}
	})

	a.MFA = &session.TOTP{}
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"status":   a.MFAStatus,
		"setup":    a.SetupMFATOTP,
		"confirm":  a.ConfirmMFATOTP,
		"disable":  a.DisableMFATOTP,
		"recovery": a.RegenerateMFARecoveryCodes,
	} {
		t.Run("mfa "+name+" needs session", func(t *testing.T) {
			rec := httptest.NewRecorder()
			call(rec, httptest.NewRequest(http.MethodPost, "/auth/mfa", strings.NewReader(`{}`)))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	t.Run("mfa verify bounds body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		a.VerifyMFALogin(rec, httptest.NewRequest(http.MethodPost, "/auth/mfa/verify", strings.NewReader("{")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	a.Passkeys = &session.Passkeys{}
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"register begin": a.BeginPasskeyRegistration,
		"register end":   a.FinishPasskeyRegistration,
		"list":           a.ListPasskeys,
		"delete":         a.DeletePasskey,
		"stepup begin":   a.BeginPasskeyStepUp,
		"stepup end":     a.FinishPasskeyStepUp,
	} {
		t.Run("passkey "+name+" needs session", func(t *testing.T) {
			rec := httptest.NewRecorder()
			call(rec, httptest.NewRequest(http.MethodPost, "/auth/passkeys", strings.NewReader(`{}`)))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	t.Run("passkey login finish validates body first", func(t *testing.T) {
		rec := httptest.NewRecorder()
		a.FinishPasskeyLogin(rec, httptest.NewRequest(http.MethodPost, "/auth/passkey/login/finish", strings.NewReader("{")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	a.OAuth = &session.OAuth{}
	t.Run("oauth refusal redirects safely", func(t *testing.T) {
		rec := httptest.NewRecorder()
		a.OauthCallback(rec, httptest.NewRequest(http.MethodGet,
			"/auth/oauth/github/callback?error=raw-provider-output", nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/sign-in?error=provider_refused" {
			t.Fatalf("response = %d %q", rec.Code, rec.Header().Get("Location"))
		}
	})
	t.Run("identity list needs session", func(t *testing.T) {
		rec := httptest.NewRecorder()
		a.ListIdentities(rec, httptest.NewRequest(http.MethodGet, "/auth/identities", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("identity delete needs session and csrf", func(t *testing.T) {
		rec := httptest.NewRecorder()
		a.DeleteIdentity(rec, httptest.NewRequest(http.MethodDelete, "/auth/identities/x", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

func TestBrowserAuthMappingHelpers(t *testing.T) {
	if got := passkeyName("   "); got != "passkey" {
		t.Fatalf("blank passkey name = %q", got)
	}
	if got := passkeyName(strings.Repeat("x", 80)); len(got) != 64 {
		t.Fatalf("bounded passkey name length = %d", len(got))
	}

	teamID := int64(7)
	identity := sessionIdentity(&store.GetSessionByTokenHashRow{
		Uuid: fixturePGUUID(t), CurrentTeamID: &teamID,
	})
	if !identity.Session || identity.TeamID != teamID {
		t.Fatalf("session identity = %#v", identity)
	}

	now := time.Now().UTC()
	credential := store.PasskeyCredential{
		Uuid: fixturePGUUID(t), Name: "laptop",
		CreatedAt: fixtureTimestamp(now),
	}
	out := passkeyJSON(credential)
	if out["last_used_at"] != nil {
		t.Fatalf("unused passkey last_used_at = %#v", out["last_used_at"])
	}
	credential.LastUsedAt = fixtureTimestamp(now)
	if passkeyJSON(credential)["last_used_at"] == nil {
		t.Fatal("used passkey omitted last_used_at")
	}

	rec := httptest.NewRecorder()
	redirectWithError(rec, httptest.NewRequest(http.MethodGet, "/", nil), "/sign-in", "a value")
	if rec.Header().Get("Location") != "/sign-in?error=a+value" {
		t.Fatalf("unsafe redirect = %q", rec.Header().Get("Location"))
	}
}

func fixturePGUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(fixtureUUID); err != nil {
		t.Fatal(err)
	}
	return u
}

func fixtureTimestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

type panicPool struct{}

func (panicPool) Begin(context.Context) (pgx.Tx, error) { return nil, errors.New("unused") }
func (panicPool) Ping(context.Context) error            { panic("database driver panic") }

func TestNewRouterRecoversHealthPanic(t *testing.T) {
	a, _ := flowAPI(t)
	a.Pool = panicPool{}
	mw := &auth.Middleware{Store: a.Store, Settings: a.Settings, Logger: a.Logger}
	rec := httptest.NewRecorder()
	NewRouter(a, mw).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
