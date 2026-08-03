package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// browserSessionStore embeds the full browser-auth boundary and implements
// only the calls exercised here. A new session dependency therefore makes
// these tests fail at the exact missing method instead of requiring Postgres.
type browserSessionStore struct {
	session.Store
	sessionRow store.GetSessionByTokenHashRow
	user       store.User
	factor     store.MfaFactor
	// memberships drives the team switcher (PRD §37); nil means the single
	// team every other test in this file assumes.
	memberships []store.ListTeamMembershipsForUserRow
	switched    []store.SetSessionCurrentTeamParams
	// Role inspection (ADR-058): what the session was put into, and the one
	// custom role these tests know about.
	viewAsSets []store.SetSessionViewAsParams
	customRole store.CustomRole
}

func newBrowserSessionStore(t *testing.T) *browserSessionStore {
	t.Helper()
	csrf := "unit-csrf"
	teamID := int64(1)
	return &browserSessionStore{
		sessionRow: store.GetSessionByTokenHashRow{
			ID: 1, Uuid: fixturePGUUID(t), UserID: 1, CurrentTeamID: &teamID,
			CsrfToken: &csrf, Email: "unit@example.test", UserName: "Unit",
		},
		user: store.User{
			ID: 1, Uuid: fixturePGUUID(t), Email: "unit@example.test", Name: "Unit",
		},
	}
}

func (s *browserSessionStore) GetSessionByTokenHash(context.Context, string) (store.GetSessionByTokenHashRow, error) {
	return s.sessionRow, nil
}

func (s *browserSessionStore) GetTeamMembershipForUser(_ context.Context, arg store.GetTeamMembershipForUserParams) (store.GetTeamMembershipForUserRow, error) {
	for _, m := range s.teams() {
		if m.TeamID == arg.PreferredTeamID {
			return store.GetTeamMembershipForUserRow{
				TeamID: m.TeamID, Role: m.Role, TeamUuid: m.TeamUuid, TeamName: m.TeamName,
			}, nil
		}
	}
	first := s.teams()[0]
	return store.GetTeamMembershipForUserRow{
		TeamID: first.TeamID, Role: first.Role, TeamUuid: first.TeamUuid, TeamName: first.TeamName,
	}, nil
}

func (s *browserSessionStore) teams() []store.ListTeamMembershipsForUserRow {
	if len(s.memberships) > 0 {
		return s.memberships
	}
	return []store.ListTeamMembershipsForUserRow{
		{TeamID: 1, Role: store.TeamRoleOwner, TeamUuid: s.sessionRow.Uuid, TeamName: "Unit"},
	}
}

func (s *browserSessionStore) ListTeamMembershipsForUser(context.Context, int64) ([]store.ListTeamMembershipsForUserRow, error) {
	return s.teams(), nil
}

func (s *browserSessionStore) SetSessionViewAs(_ context.Context, arg store.SetSessionViewAsParams) error {
	s.viewAsSets = append(s.viewAsSets, arg)
	return nil
}

func (s *browserSessionStore) GetCustomRoleByID(_ context.Context, id int64) (store.CustomRole, error) {
	if s.customRole.ID != id {
		return store.CustomRole{}, pgx.ErrNoRows
	}
	return s.customRole, nil
}

func (s *browserSessionStore) GetCustomRoleByUUID(_ context.Context, arg store.GetCustomRoleByUUIDParams) (store.CustomRole, error) {
	if s.customRole.TeamID != arg.TeamID {
		return store.CustomRole{}, pgx.ErrNoRows
	}
	return s.customRole, nil
}

func (s *browserSessionStore) SetSessionCurrentTeam(_ context.Context, arg store.SetSessionCurrentTeamParams) (int64, error) {
	s.switched = append(s.switched, arg)
	return 1, nil
}

func (*browserSessionStore) SetUserLastTeam(context.Context, store.SetUserLastTeamParams) error {
	return nil
}
func (*browserSessionStore) TouchSession(context.Context, int64) error  { return nil }
func (*browserSessionStore) RevokeSession(context.Context, int64) error { return nil }
func (s *browserSessionStore) GetUserByID(context.Context, int64) (store.User, error) {
	return s.user, nil
}

func (*browserSessionStore) ListPasskeysForUser(context.Context, int64) ([]store.PasskeyCredential, error) {
	return nil, nil
}

func (*browserSessionStore) PurgeExpiredPasskeyCeremonies(context.Context) (int64, error) {
	return 0, nil
}

func (*browserSessionStore) CreatePasskeyCeremony(context.Context, store.CreatePasskeyCeremonyParams) error {
	return nil
}

func (s *browserSessionStore) GetMfaFactorForUser(context.Context, int64) (store.MfaFactor, error) {
	if s.factor.ID == 0 {
		return store.MfaFactor{}, pgx.ErrNoRows
	}
	return s.factor, nil
}

func (s *browserSessionStore) UpsertUnconfirmedMfaFactor(_ context.Context, p store.UpsertUnconfirmedMfaFactorParams) (store.MfaFactor, error) {
	s.factor = store.MfaFactor{
		ID: 1, Uuid: p.Uuid, UserID: p.UserID, Type: store.MfaTypeTotp, SecretEnc: p.SecretEnc,
	}
	return s.factor, nil
}

func (*browserSessionStore) RecordFailedLogin(context.Context, store.RecordFailedLoginParams) (store.RecordFailedLoginRow, error) {
	return store.RecordFailedLoginRow{}, nil
}

func (*browserSessionStore) ListEnabledOauthProviderConfigs(context.Context) ([]store.ListEnabledOauthProviderConfigsRow, error) {
	name := "GitHub"
	return []store.ListEnabledOauthProviderConfigsRow{{
		Provider: store.OauthProviderGithub, DisplayName: &name,
	}}, nil
}

func (*browserSessionStore) GetOauthProviderConfig(context.Context, store.OauthProvider) (store.OauthProviderConfig, error) {
	return store.OauthProviderConfig{}, pgx.ErrNoRows
}

func (*browserSessionStore) CountCredentialsForUser(context.Context, int64) (int32, error) {
	return 1, nil
}

func authenticatedBrowserRequest(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "session-token"})
	req.AddCookie(&http.Cookie{Name: session.CSRFCookieName, Value: "unit-csrf"})
	req.Header.Set(session.CSRFHeader, "unit-csrf")
	return req
}

func withURLParam(req *http.Request, key, value string) *http.Request {
	route := chi.NewRouteContext()
	route.URLParams.Add(key, value)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
}

func TestBrowserAuthenticationSuccessAndDomainBranches(t *testing.T) {
	a, db := flowAPI(t)
	db.countOne = true
	st := newBrowserSessionStore(t)
	manager := &session.Manager{Store: st}
	passkeys, err := session.NewPasskeys(st, manager, "localhost", "AkerDock", []string{"http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	a.Sessions = manager
	a.Passkeys = passkeys
	a.MFA = &session.TOTP{Store: st, Sessions: manager, Keyring: a.Keyring}
	a.OAuth = &session.OAuth{
		Store: st, Sessions: manager, Keyring: a.Keyring, Settings: a.Settings,
		BaseURL: "http://localhost",
	}

	t.Run("me resolves active session", func(t *testing.T) {
		rec := httptest.NewRecorder()
		a.Me(rec, authenticatedBrowserRequest(t, http.MethodGet, "/auth/me", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("mfa status and setup", func(t *testing.T) {
		status := httptest.NewRecorder()
		a.MFAStatus(status, authenticatedBrowserRequest(t, http.MethodGet, "/auth/mfa", ""))
		if status.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", status.Code, status.Body.String())
		}

		setup := httptest.NewRecorder()
		a.SetupMFATOTP(setup, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/setup", "{}"))
		if setup.Code != http.StatusOK {
			t.Fatalf("setup = %d, body = %s", setup.Code, setup.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(setup.Body.Bytes(), &payload); err != nil || payload["secret"] == "" {
			t.Fatalf("setup payload = %s, err = %v", setup.Body.String(), err)
		}

		confirm := httptest.NewRecorder()
		a.ConfirmMFATOTP(confirm, authenticatedBrowserRequest(
			t, http.MethodPost, "/auth/mfa/totp/confirm", `{"code":"not-a-code"}`))
		if confirm.Code != http.StatusBadRequest {
			t.Fatalf("confirm = %d, body = %s", confirm.Code, confirm.Body.String())
		}
	})

	t.Run("passkey begins and management", func(t *testing.T) {
		for name, call := range map[string]func(http.ResponseWriter, *http.Request){
			"registration": a.BeginPasskeyRegistration,
			"login":        a.BeginPasskeyLogin,
			"stepup":       a.BeginPasskeyStepUp,
		} {
			rec := httptest.NewRecorder()
			call(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkey/"+name, "{}"))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, body = %s", name, rec.Code, rec.Body.String())
			}
		}

		list := httptest.NewRecorder()
		a.ListPasskeys(list, authenticatedBrowserRequest(t, http.MethodGet, "/auth/passkeys", ""))
		if list.Code != http.StatusOK {
			t.Fatalf("list = %d, body = %s", list.Code, list.Body.String())
		}

		remove := httptest.NewRecorder()
		req := withURLParam(
			authenticatedBrowserRequest(t, http.MethodDelete, "/auth/passkeys/"+fixtureUUID, ""),
			"passkey_uuid", fixtureUUID,
		)
		a.DeletePasskey(remove, req)
		if remove.Code != http.StatusNoContent {
			t.Fatalf("delete = %d, body = %s", remove.Code, remove.Body.String())
		}
	})

	t.Run("oauth public list and guarded branches", func(t *testing.T) {
		providers := httptest.NewRecorder()
		a.OauthProviders(providers, httptest.NewRequest(http.MethodGet, "/auth/oauth/providers", nil))
		if providers.Code != http.StatusOK || !strings.Contains(providers.Body.String(), "github") {
			t.Fatalf("providers = %d, body = %s", providers.Code, providers.Body.String())
		}

		start := httptest.NewRecorder()
		req := withURLParam(httptest.NewRequest(http.MethodPost, "/auth/oauth/github/start", nil), "oauth_provider", "github")
		a.StartOauth(start, req)
		if start.Code != http.StatusNotFound {
			t.Fatalf("start = %d, body = %s", start.Code, start.Body.String())
		}

		identities := httptest.NewRecorder()
		a.ListIdentities(identities, authenticatedBrowserRequest(t, http.MethodGet, "/auth/identities", ""))
		if identities.Code != http.StatusOK {
			t.Fatalf("identities = %d, body = %s", identities.Code, identities.Body.String())
		}

		remove := httptest.NewRecorder()
		req = withURLParam(
			authenticatedBrowserRequest(t, http.MethodDelete, "/auth/identities/"+fixtureUUID, ""),
			"identity_uuid", fixtureUUID,
		)
		a.DeleteIdentity(remove, req)
		if remove.Code != http.StatusConflict {
			t.Fatalf("unlink = %d, body = %s", remove.Code, remove.Body.String())
		}
	})
}

var _ session.Store = (*browserSessionStore)(nil)
