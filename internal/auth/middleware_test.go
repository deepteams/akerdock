package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepteams/akerdock/internal/store"
)

type fakeSessions struct {
	identity *Identity
}

func (f *fakeSessions) Authenticate(context.Context, *http.Request) *Identity { return f.identity }
func (f *fakeSessions) VerifyCSRF(context.Context, *http.Request) error       { return nil }

type fakeSettings struct {
	settings store.InstanceSetting
}

func (f *fakeSettings) Get(context.Context) (store.InstanceSetting, error) {
	return f.settings, nil
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
