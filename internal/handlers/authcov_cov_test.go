package handlers

// Coverage tests for the auth-side handlers (authmfa.go, authpasskeys.go,
// authoauth.go, authinvitations.go) plus the shared steerable fakes reused by
// authcov_cov2_test.go (roles.go, tokens.go, invitations.go).
//
// Every top-level identifier declared here is prefixed authcov, per the
// concurrent-agents convention of this package.

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/oidc"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/totp"
)

// ---------------------------------------------------------------------------
// Steerable protocol fake: like flowDB, but failures, missing rows and row
// counts are keyed by a substring of the SQL (sqlc embeds the query name in
// every statement), so ONE query of a handler can fail while the rest of the
// request keeps working.

type authcovFail struct {
	err  error
	skip int // matches to let through before failing
}

type authcovDB struct {
	truthy   bool
	countOne bool
	errOn    map[string]*authcovFail
	noRowsOn []string
	execTag  map[string]string
	rowsN    map[string]int
	// fill lets one test override how a scan destination is filled (return
	// true when handled); everything else falls back to fillScanDestination.
	fill func(dest any) bool
}

func (db *authcovDB) failure(sql string) error {
	for k, f := range db.errOn {
		if strings.Contains(sql, k) {
			if f.skip > 0 {
				f.skip--
				return nil
			}
			return f.err
		}
	}
	return nil
}

func (db *authcovDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if err := db.failure(sql); err != nil {
		return pgconn.CommandTag{}, err
	}
	for k, tag := range db.execTag {
		if strings.Contains(sql, k) {
			return pgconn.NewCommandTag(tag), nil
		}
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (db *authcovDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if err := db.failure(sql); err != nil {
		return nil, err
	}
	n := 1
	for k, v := range db.rowsN {
		if strings.Contains(sql, k) {
			n = v
		}
	}
	for _, k := range db.noRowsOn {
		if strings.Contains(sql, k) {
			n = 0
		}
	}
	return &authcovRows{remaining: n, db: db}, nil
}

func (db *authcovDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	err := db.failure(sql)
	if err == nil {
		for _, k := range db.noRowsOn {
			if strings.Contains(sql, k) {
				err = pgx.ErrNoRows
			}
		}
	}
	return authcovRow{
		err:        err,
		zeroScalar: strings.Contains(strings.ToLower(sql), "count(") && !db.countOne,
		db:         db,
	}
}

func (db *authcovDB) fillOne(dest any, zeroScalar bool) error {
	if db.fill != nil && db.fill(dest) {
		return nil
	}
	return fillScanDestination(dest, zeroScalar, db.truthy)
}

type authcovRow struct {
	err        error
	zeroScalar bool
	db         *authcovDB
}

func (r authcovRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for _, d := range dest {
		if err := r.db.fillOne(d, r.zeroScalar); err != nil {
			return err
		}
	}
	return nil
}

type authcovRows struct {
	remaining int
	current   bool
	closed    bool
	err       error
	db        *authcovDB
}

func (r *authcovRows) Close()                                       { r.closed = true }
func (r *authcovRows) Err() error                                   { return r.err }
func (r *authcovRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (r *authcovRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *authcovRows) Values() ([]any, error)                       { return nil, nil }
func (r *authcovRows) RawValues() [][]byte                          { return nil }
func (r *authcovRows) Conn() *pgx.Conn                              { return nil }
func (r *authcovRows) Next() bool {
	if r.closed || r.remaining == 0 {
		r.closed = true
		r.current = false
		return false
	}
	r.remaining--
	r.current = true
	return true
}

func (r *authcovRows) Scan(dest ...any) error {
	if !r.current {
		return errors.New("Scan called before Next")
	}
	for _, d := range dest {
		if err := r.db.fillOne(d, false); err != nil {
			r.err = err
			r.Close()
			return err
		}
	}
	return nil
}

var _ store.DBTX = (*authcovDB)(nil)
var _ pgx.Rows = (*authcovRows)(nil)

// authcovAPI is flowAPI with the steerable database underneath.
func authcovAPI(t *testing.T, db *authcovDB) *API {
	t.Helper()
	a, _ := flowAPI(t)
	q := store.New(db)
	a.Store = q
	a.Settings = instance.NewCache(q)
	a.Audit = &audit.Recorder{Store: q, Logger: a.Logger}
	return a
}

// authcovRequest builds an /api/v1-style request carrying the most-privileged
// fixture identity, like flowRouter injects.
func authcovRequest(method, target, body string) *http.Request {
	return authcovRequestAs(authcovRootIdentity(), method, target, body)
}

func authcovRootIdentity() *auth.Identity {
	return &auth.Identity{
		TokenID: 1, TokenUUID: fixtureUUID, TeamID: 1, TeamUUID: fixtureUUID,
		Permissions: []string{string(auth.PermRoot)}, InstanceRoot: true,
		UserID: ptr(int64(1)),
	}
}

func authcovRequestAs(id *auth.Identity, method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(auth.WithIdentity(req.Context(), id))
}

// ---------------------------------------------------------------------------
// Steerable session.Store fake for the browser-auth engines (Manager, TOTP,
// Passkeys, OAuth). Unimplemented methods panic through the embedded nil.

type authcovStore struct {
	session.Store

	sessionRow store.GetSessionByTokenHashRow
	sessionErr error
	user       store.User
	userErr    error

	// MFA
	factor              store.MfaFactor
	factorErr           error
	upsertNoRows        bool
	confirmErr          error
	deleteFactorErr     error
	replaceN            int64
	replaceErr          error
	recoveryN           int64
	touchN              int64
	clearFailedErr      error
	challenge           store.MfaChallenge
	challengeErr        error
	consumeChallengeErr error
	totpVerifiedErr     error

	// Manager
	membershipErr    error
	memberships      []store.ListTeamMembershipsForUserRow
	createSessionErr error

	// Passkeys
	ceremony      store.PasskeyCeremony
	ceremonyErr   error
	passkeys      []store.PasskeyCredential
	createdCred   store.PasskeyCredential
	createCredErr error
	passkeyRow    store.GetPasskeyByCredentialIDRow
	passkeyRowErr error
	updateCredErr error

	// OAuth
	providersErr      error
	providerCfg       store.OauthProviderConfig
	providerCfgErr    error
	loginState        store.OauthLoginState
	loginStateErr     error
	linkState         store.OauthLoginState
	linkStateErr      error
	createStateErr    error
	identity          store.Identity
	identityErr       error
	createIdentityErr error
	emailUserErr      error
	pending           []store.ListPendingInvitationsByEmailRow
	credCount         int32
	deleteIdentityN   int64
}

func authcovNewStore(t *testing.T) *authcovStore {
	t.Helper()
	csrf := "unit-csrf"
	teamID := int64(1)
	return &authcovStore{
		sessionRow: store.GetSessionByTokenHashRow{
			ID: 1, Uuid: fixturePGUUID(t), UserID: 1, CurrentTeamID: &teamID,
			CsrfToken: &csrf, Email: "unit@example.test", UserName: "Unit",
		},
		user: store.User{ID: 1, Uuid: fixturePGUUID(t), Email: "unit@example.test", Name: "Unit"},
		memberships: []store.ListTeamMembershipsForUserRow{{
			TeamID: 1, Role: store.TeamRoleOwner, TeamUuid: fixturePGUUID(t), TeamName: "Unit",
		}},
		ceremony:        store.PasskeyCeremony{ID: 1, Data: []byte("{}"), UserID: ptr(int64(1))},
		replaceN:        1,
		recoveryN:       1,
		touchN:          1,
		credCount:       2,
		deleteIdentityN: 1,
	}
}

func (s *authcovStore) GetSessionByTokenHash(context.Context, string) (store.GetSessionByTokenHashRow, error) {
	if s.sessionErr != nil {
		return store.GetSessionByTokenHashRow{}, s.sessionErr
	}
	return s.sessionRow, nil
}

func (s *authcovStore) TouchSession(context.Context, int64) error  { return nil }
func (s *authcovStore) RevokeSession(context.Context, int64) error { return nil }
func (s *authcovStore) SetUserLastTeam(context.Context, store.SetUserLastTeamParams) error {
	return nil
}
func (s *authcovStore) SetSessionViewAs(context.Context, store.SetSessionViewAsParams) error {
	return nil
}

func (s *authcovStore) GetUserByID(context.Context, int64) (store.User, error) {
	return s.user, s.userErr
}

func (s *authcovStore) GetInstanceSettings(context.Context) (store.InstanceSetting, error) {
	return store.InstanceSetting{}, nil
}

func (s *authcovStore) GetTeamMembershipForUser(context.Context, store.GetTeamMembershipForUserParams) (store.GetTeamMembershipForUserRow, error) {
	if s.membershipErr != nil {
		return store.GetTeamMembershipForUserRow{}, s.membershipErr
	}
	m := s.memberships[0]
	return store.GetTeamMembershipForUserRow{
		TeamID: m.TeamID, Role: m.Role, TeamUuid: m.TeamUuid, TeamName: m.TeamName,
	}, nil
}

func (s *authcovStore) ListTeamMembershipsForUser(context.Context, int64) ([]store.ListTeamMembershipsForUserRow, error) {
	return s.memberships, nil
}

func (s *authcovStore) SetSessionCurrentTeam(context.Context, store.SetSessionCurrentTeamParams) (int64, error) {
	return 1, nil
}

func (s *authcovStore) CreateSession(context.Context, store.CreateSessionParams) (store.Session, error) {
	if s.createSessionErr != nil {
		return store.Session{}, s.createSessionErr
	}
	return store.Session{ID: 1}, nil
}

func (s *authcovStore) ClearFailedLogins(context.Context, int64) error { return s.clearFailedErr }

func (s *authcovStore) RecordFailedLogin(context.Context, store.RecordFailedLoginParams) (store.RecordFailedLoginRow, error) {
	return store.RecordFailedLoginRow{}, nil
}

func (s *authcovStore) CountPasskeysForUser(context.Context, int64) (int64, error) { return 0, nil }

func (s *authcovStore) GetMfaFactorForUser(context.Context, int64) (store.MfaFactor, error) {
	return s.factor, s.factorErr
}

func (s *authcovStore) UpsertUnconfirmedMfaFactor(_ context.Context, p store.UpsertUnconfirmedMfaFactorParams) (store.MfaFactor, error) {
	if s.upsertNoRows {
		return store.MfaFactor{}, pgx.ErrNoRows
	}
	return store.MfaFactor{ID: 1, Uuid: p.Uuid, UserID: p.UserID, SecretEnc: p.SecretEnc}, nil
}

func (s *authcovStore) ConfirmMfaFactor(context.Context, store.ConfirmMfaFactorParams) (store.MfaFactor, error) {
	return s.factor, s.confirmErr
}

func (s *authcovStore) DeleteMfaFactorForUser(context.Context, int64) (int64, error) {
	return 1, s.deleteFactorErr
}

func (s *authcovStore) ReplaceMfaRecoveryCodes(context.Context, store.ReplaceMfaRecoveryCodesParams) (int64, error) {
	return s.replaceN, s.replaceErr
}

func (s *authcovStore) ConsumeMfaRecoveryCode(context.Context, store.ConsumeMfaRecoveryCodeParams) (int64, error) {
	return s.recoveryN, nil
}

func (s *authcovStore) TouchMfaFactorUsed(context.Context, store.TouchMfaFactorUsedParams) (int64, error) {
	return s.touchN, nil
}

func (s *authcovStore) SetSessionTotpVerified(context.Context, int64) error {
	return s.totpVerifiedErr
}

func (s *authcovStore) PurgeExpiredMfaChallenges(context.Context) (int64, error) { return 0, nil }
func (s *authcovStore) CreateMfaChallenge(context.Context, store.CreateMfaChallengeParams) error {
	return nil
}

func (s *authcovStore) GetMfaChallenge(context.Context, string) (store.MfaChallenge, error) {
	return s.challenge, s.challengeErr
}

func (s *authcovStore) ConsumeMfaChallenge(context.Context, string) (store.MfaChallenge, error) {
	return s.challenge, s.consumeChallengeErr
}

func (s *authcovStore) ListPasskeysForUser(context.Context, int64) ([]store.PasskeyCredential, error) {
	return s.passkeys, nil
}

func (s *authcovStore) CreatePasskeyCredential(context.Context, store.CreatePasskeyCredentialParams) (store.PasskeyCredential, error) {
	return s.createdCred, s.createCredErr
}

func (s *authcovStore) GetPasskeyByCredentialID(context.Context, []byte) (store.GetPasskeyByCredentialIDRow, error) {
	return s.passkeyRow, s.passkeyRowErr
}

func (s *authcovStore) UpdatePasskeyCredential(context.Context, store.UpdatePasskeyCredentialParams) error {
	return s.updateCredErr
}

func (s *authcovStore) PurgeExpiredPasskeyCeremonies(context.Context) (int64, error) { return 0, nil }
func (s *authcovStore) CreatePasskeyCeremony(context.Context, store.CreatePasskeyCeremonyParams) error {
	return nil
}

func (s *authcovStore) ConsumePasskeyCeremony(context.Context, store.ConsumePasskeyCeremonyParams) (store.PasskeyCeremony, error) {
	if s.ceremonyErr != nil {
		return store.PasskeyCeremony{}, s.ceremonyErr
	}
	return s.ceremony, nil
}

func (s *authcovStore) ListEnabledOauthProviderConfigs(context.Context) ([]store.ListEnabledOauthProviderConfigsRow, error) {
	if s.providersErr != nil {
		return nil, s.providersErr
	}
	name := "GitHub"
	return []store.ListEnabledOauthProviderConfigsRow{{
		Provider: store.OauthProviderGithub, DisplayName: &name,
	}}, nil
}

func (s *authcovStore) GetOauthProviderConfig(context.Context, store.OauthProvider) (store.OauthProviderConfig, error) {
	return s.providerCfg, s.providerCfgErr
}

func (s *authcovStore) PurgeExpiredOauthLoginStates(context.Context) (int64, error) { return 0, nil }
func (s *authcovStore) CreateOauthLoginState(context.Context, store.CreateOauthLoginStateParams) error {
	return s.createStateErr
}

func (s *authcovStore) ConsumeOauthLoginState(_ context.Context, arg store.ConsumeOauthLoginStateParams) (store.OauthLoginState, error) {
	if arg.Purpose == "link" {
		return s.linkState, s.linkStateErr
	}
	return s.loginState, s.loginStateErr
}

func (s *authcovStore) GetIdentity(context.Context, store.GetIdentityParams) (store.Identity, error) {
	return s.identity, s.identityErr
}

func (s *authcovStore) CreateIdentity(context.Context, store.CreateIdentityParams) (store.Identity, error) {
	return s.identity, s.createIdentityErr
}

func (s *authcovStore) CountCredentialsForUser(context.Context, int64) (int32, error) {
	return s.credCount, nil
}

func (s *authcovStore) DeleteIdentityForUser(context.Context, store.DeleteIdentityForUserParams) (int64, error) {
	return s.deleteIdentityN, nil
}

func (s *authcovStore) GetUserByEmailIncludingDeleted(context.Context, string) (store.User, error) {
	return s.user, s.emailUserErr
}

func (s *authcovStore) CreateUser(context.Context, store.CreateUserParams) (store.User, error) {
	return s.user, nil
}

func (s *authcovStore) CreatePersonalTeam(context.Context, string) (store.Team, error) {
	return store.Team{ID: 1}, nil
}

func (s *authcovStore) AddTeamMember(context.Context, store.AddTeamMemberParams) error { return nil }

func (s *authcovStore) ListPendingInvitationsByEmail(context.Context, string) ([]store.ListPendingInvitationsByEmailRow, error) {
	return s.pending, nil
}

func (s *authcovStore) AcceptInvitationByID(context.Context, int64) (store.AcceptInvitationByIDRow, error) {
	return store.AcceptInvitationByIDRow{TeamID: 1, Role: store.TeamRoleMember}, nil
}

var _ session.Store = (*authcovStore)(nil)

// ---------------------------------------------------------------------------
// Fake WebAuthn engine: implements the session package's engine seam with
// steerable outcomes, so every FinishXxx branch is one field away.

type authcovPasskeyEngine struct {
	beginErr      error
	createErr     error
	cred          *webauthn.Credential
	validateErr   error
	invokeHandler bool
	rawID, handle []byte
}

func (e *authcovPasskeyEngine) credential() *webauthn.Credential {
	if e.cred != nil {
		return e.cred
	}
	return &webauthn.Credential{}
}

func (e *authcovPasskeyEngine) BeginRegistration(webauthn.User, ...webauthn.RegistrationOption) (*protocol.CredentialCreation, *webauthn.SessionData, error) {
	if e.beginErr != nil {
		return nil, nil, e.beginErr
	}
	return &protocol.CredentialCreation{}, &webauthn.SessionData{}, nil
}

func (e *authcovPasskeyEngine) CreateCredential(webauthn.User, webauthn.SessionData, []byte) (*webauthn.Credential, error) {
	if e.createErr != nil {
		return nil, e.createErr
	}
	return e.credential(), nil
}

func (e *authcovPasskeyEngine) BeginDiscoverableLogin(...webauthn.LoginOption) (*protocol.CredentialAssertion, *webauthn.SessionData, error) {
	if e.beginErr != nil {
		return nil, nil, e.beginErr
	}
	return &protocol.CredentialAssertion{}, &webauthn.SessionData{}, nil
}

func (e *authcovPasskeyEngine) ValidatePasskeyLogin(handler webauthn.DiscoverableUserHandler, _ webauthn.SessionData, _ []byte) (webauthn.User, *webauthn.Credential, error) {
	if e.validateErr != nil {
		return nil, nil, e.validateErr
	}
	if e.invokeHandler {
		if _, err := handler(e.rawID, e.handle); err != nil {
			return nil, nil, err
		}
	}
	return nil, e.credential(), nil
}

// ---------------------------------------------------------------------------
// Fake OIDC client.

type authcovOAuthClient struct {
	exchangeErr error
	who         *oidc.Identity
	whoErr      error
}

func (c *authcovOAuthClient) Discover(context.Context, string) (*oidc.Endpoints, error) {
	return &oidc.Endpoints{}, nil
}

func (c *authcovOAuthClient) Exchange(context.Context, *oidc.Endpoints, string, string, string, string, string) (*oidc.TokenResponse, error) {
	if c.exchangeErr != nil {
		return nil, c.exchangeErr
	}
	return &oidc.TokenResponse{AccessToken: "at", IDToken: "idt"}, nil
}

func (c *authcovOAuthClient) VerifyIDToken(context.Context, *oidc.Endpoints, string, string, string, time.Time) (*oidc.Identity, error) {
	return c.who, c.whoErr
}

func (c *authcovOAuthClient) FetchOAuth2Identity(context.Context, string, *oidc.Endpoints, string) (*oidc.Identity, error) {
	return c.who, c.whoErr
}

var _ session.OAuthClient = (*authcovOAuthClient)(nil)

// ---------------------------------------------------------------------------
// Wiring helpers.

func authcovMFAAPI(t *testing.T) (*API, *authcovStore, *authcovDB) {
	t.Helper()
	db := &authcovDB{}
	a := authcovAPI(t, db)
	st := authcovNewStore(t)
	mgr := &session.Manager{Store: st}
	a.Sessions = mgr
	a.MFA = &session.TOTP{Store: st, Sessions: mgr, Keyring: a.Keyring}
	return a, st, db
}

// authcovMFAFactor mints a factor whose secret round-trips through the fixture
// keyring, so the real TOTP engine can decrypt and validate it.
func authcovMFAFactor(t *testing.T, a *API, confirmed bool) (store.MfaFactor, string) {
	t.Helper()
	secret, err := totp.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	u := fixturePGUUID(t)
	enc, err := a.Keyring.Encrypt("mfa_factors", "secret_enc", fixtureUUID, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	f := store.MfaFactor{
		ID: 1, Uuid: u, UserID: 1, Type: store.MfaTypeTotp,
		SecretEnc: enc, RecoveryCodeHashes: []string{"h"},
	}
	if confirmed {
		f.ConfirmedAt = fixtureTimestamp(time.Now().UTC())
	}
	return f, secret
}

// authcovTOTPCode computes the RFC 6238 code for the current step, mirroring
// internal/totp's parameters (SHA-1, 6 digits, 30 s).
func authcovTOTPCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil {
		t.Fatal(err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(at.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	truncated := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", truncated%1_000_000)
}

func authcovLockedUser(t *testing.T) store.User {
	t.Helper()
	return store.User{
		ID: 1, Uuid: fixturePGUUID(t), Email: "unit@example.test", Name: "Unit",
		LockedUntil: fixtureTimestamp(time.Now().Add(time.Hour)),
	}
}

func authcovUniqueViolation() error {
	return &pgconn.PgError{Code: "23505"}
}

// ---------------------------------------------------------------------------
// authmfa.go — VerifyMFALogin.

func TestAuthcovVerifyMFALoginBranches(t *testing.T) {
	t.Run("expired challenge", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.challengeErr = errors.New("gone")
		rec := httptest.NewRecorder()
		a.VerifyMFALogin(rec, postJSON("/auth/mfa/verify", `{"challenge":"c","code":"123456"}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("locked account", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.user = authcovLockedUser(t)
		rec := httptest.NewRecorder()
		a.VerifyMFALogin(rec, postJSON("/auth/mfa/verify", `{"challenge":"c","code":"123456"}`))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body)
		}
	})

	t.Run("invalid code", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor, _ = authcovMFAFactor(t, a, true)
		st.recoveryN = 0 // spent or wrong recovery code
		rec := httptest.NewRecorder()
		a.VerifyMFALogin(rec, postJSON("/auth/mfa/verify", `{"challenge":"c","recovery_code":"nope"}`))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor, _ = authcovMFAFactor(t, a, true)
		st.clearFailedErr = errors.New("db down")
		rec := httptest.NewRecorder()
		a.VerifyMFALogin(rec, postJSON("/auth/mfa/verify", `{"challenge":"c","recovery_code":"aaaa-bbbb"}`))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("undeliverable cookie refused", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		a.Sessions.Secure = true
		st.factor, _ = authcovMFAFactor(t, a, true)
		rec := httptest.NewRecorder()
		a.VerifyMFALogin(rec, postJSON("/auth/mfa/verify", `{"challenge":"c","recovery_code":"aaaa-bbbb"}`))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "https_required") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("success opens session", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor, _ = authcovMFAFactor(t, a, true)
		rec := httptest.NewRecorder()
		a.VerifyMFALogin(rec, postJSON("/auth/mfa/verify", `{"challenge":"c","recovery_code":"aaaa-bbbb"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
		if len(rec.Result().Cookies()) < 2 {
			t.Fatal("MFA login did not set both session cookies")
		}
	})
}

// authmfa.go — status, setup, confirm.

func TestAuthcovMFAStatusBranches(t *testing.T) {
	t.Run("store failure is internal", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factorErr = errors.New("db down")
		rec := httptest.NewRecorder()
		a.MFAStatus(rec, authenticatedBrowserRequest(t, http.MethodGet, "/auth/mfa", ""))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("enabled factor exposes confirmed_at", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor, _ = authcovMFAFactor(t, a, true)
		rec := httptest.NewRecorder()
		a.MFAStatus(rec, authenticatedBrowserRequest(t, http.MethodGet, "/auth/mfa", ""))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "confirmed_at") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovSetupMFATOTPBranches(t *testing.T) {
	t.Run("already enabled", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.upsertNoRows = true
		rec := httptest.NewRecorder()
		a.SetupMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/setup", "{}"))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.userErr = errors.New("db down")
		rec := httptest.NewRecorder()
		a.SetupMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/setup", "{}"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovConfirmMFATOTPBranches(t *testing.T) {
	t.Run("malformed body", func(t *testing.T) {
		a, _, _ := authcovMFAAPI(t)
		rec := httptest.NewRecorder()
		a.ConfirmMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/confirm", "{"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("no pending setup", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factorErr = pgx.ErrNoRows
		rec := httptest.NewRecorder()
		a.ConfirmMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/confirm", `{"code":"123456"}`))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "mfa_not_configured") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("already enabled", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor, _ = authcovMFAFactor(t, a, true)
		rec := httptest.NewRecorder()
		a.ConfirmMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/confirm", `{"code":"123456"}`))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "mfa_already_enabled") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("undecryptable secret is internal", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor = store.MfaFactor{ID: 1, Uuid: fixturePGUUID(t), UserID: 1, SecretEnc: []byte("garbage")}
		rec := httptest.NewRecorder()
		a.ConfirmMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/confirm", `{"code":"123456"}`))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("valid code enables the factor", func(t *testing.T) {
		a, st, db := authcovMFAAPI(t)
		factor, secret := authcovMFAFactor(t, a, false)
		st.factor = factor
		// The pending-clear and the audit user lookup both fail: recoverable,
		// the enrolment still answers its recovery codes.
		db.errOn = map[string]*authcovFail{
			"ClearMfaPendingForUser": {err: errors.New("db down")},
			"GetUserByID":            {err: errors.New("db down")},
		}
		code := authcovTOTPCode(t, secret, time.Now())
		rec := httptest.NewRecorder()
		a.ConfirmMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/confirm",
			`{"code":"`+code+`"}`))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "recovery_codes") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})
}

// authmfa.go — disable, recovery codes, step-up.

func TestAuthcovDisableMFATOTPBranches(t *testing.T) {
	t.Run("malformed body", func(t *testing.T) {
		a, _, _ := authcovMFAAPI(t)
		rec := httptest.NewRecorder()
		a.DisableMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodDelete, "/auth/mfa/totp", "{"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("not configured", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factorErr = pgx.ErrNoRows
		rec := httptest.NewRecorder()
		a.DisableMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodDelete, "/auth/mfa/totp", `{"code":"123456"}`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("locked account", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.user = authcovLockedUser(t)
		rec := httptest.NewRecorder()
		a.DisableMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodDelete, "/auth/mfa/totp", `{"code":"123456"}`))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body)
		}
	})

	t.Run("invalid code is audited and refused", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor, _ = authcovMFAFactor(t, a, true)
		st.recoveryN = 0
		rec := httptest.NewRecorder()
		a.DisableMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodDelete, "/auth/mfa/totp", `{"recovery_code":"nope"}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor, _ = authcovMFAFactor(t, a, true)
		st.deleteFactorErr = errors.New("db down")
		rec := httptest.NewRecorder()
		a.DisableMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodDelete, "/auth/mfa/totp", `{"recovery_code":"aaaa-bbbb"}`))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("valid recovery code disables", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor, _ = authcovMFAFactor(t, a, true)
		rec := httptest.NewRecorder()
		a.DisableMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodDelete, "/auth/mfa/totp", `{"recovery_code":"aaaa-bbbb"}`))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovRegenerateRecoveryCodesBranches(t *testing.T) {
	t.Run("malformed body", func(t *testing.T) {
		a, _, _ := authcovMFAAPI(t)
		rec := httptest.NewRecorder()
		a.RegenerateMFARecoveryCodes(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/recovery-codes", "{"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("not configured", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factorErr = pgx.ErrNoRows
		rec := httptest.NewRecorder()
		a.RegenerateMFARecoveryCodes(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/recovery-codes", `{"code":"123456"}`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("locked account", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.user = authcovLockedUser(t)
		rec := httptest.NewRecorder()
		a.RegenerateMFARecoveryCodes(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/recovery-codes", `{"code":"123456"}`))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body)
		}
	})

	t.Run("invalid code", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor, _ = authcovMFAFactor(t, a, true)
		rec := httptest.NewRecorder()
		// Five digits: structurally invalid, so the refusal is deterministic.
		a.RegenerateMFARecoveryCodes(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/recovery-codes", `{"code":"12345"}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		factor, secret := authcovMFAFactor(t, a, true)
		st.factor = factor
		st.replaceErr = errors.New("db down")
		code := authcovTOTPCode(t, secret, time.Now())
		rec := httptest.NewRecorder()
		a.RegenerateMFARecoveryCodes(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/recovery-codes", `{"code":"`+code+`"}`))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("valid code regenerates", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		factor, secret := authcovMFAFactor(t, a, true)
		st.factor = factor
		code := authcovTOTPCode(t, secret, time.Now())
		rec := httptest.NewRecorder()
		a.RegenerateMFARecoveryCodes(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/recovery-codes", `{"code":"`+code+`"}`))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "recovery_codes") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovStepUpMFATOTPBranches(t *testing.T) {
	t.Run("disabled feature", func(t *testing.T) {
		a, _ := flowAPI(t)
		rec := httptest.NewRecorder()
		a.StepUpMFATOTP(rec, postJSON("/auth/mfa/totp/stepup", "{}"))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("no session", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.sessionErr = errors.New("no session")
		rec := httptest.NewRecorder()
		a.StepUpMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/stepup", "{}"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		a, _, _ := authcovMFAAPI(t)
		rec := httptest.NewRecorder()
		a.StepUpMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/stepup", "{"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("not configured", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.userErr = errors.New("gone")
		rec := httptest.NewRecorder()
		a.StepUpMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/stepup", `{"code":"123456"}`))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "mfa_not_configured") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("locked account", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.user = authcovLockedUser(t)
		rec := httptest.NewRecorder()
		a.StepUpMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/stepup", `{"code":"123456"}`))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body)
		}
	})

	t.Run("invalid code", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor, _ = authcovMFAFactor(t, a, true)
		st.recoveryN = 0
		rec := httptest.NewRecorder()
		a.StepUpMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/stepup", `{"recovery_code":"nope"}`))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
		}
	})

	t.Run("store failure is internal", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor, _ = authcovMFAFactor(t, a, true)
		st.clearFailedErr = errors.New("db down")
		rec := httptest.NewRecorder()
		a.StepUpMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/stepup", `{"recovery_code":"aaaa-bbbb"}`))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("success stamps the marker", func(t *testing.T) {
		a, st, _ := authcovMFAAPI(t)
		st.factor, _ = authcovMFAFactor(t, a, true)
		rec := httptest.NewRecorder()
		a.StepUpMFATOTP(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/mfa/totp/stepup", `{"recovery_code":"aaaa-bbbb"}`))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body)
		}
	})
}

// ---------------------------------------------------------------------------
// authpasskeys.go

func authcovPasskeyAPI(t *testing.T) (*API, *authcovStore, *authcovPasskeyEngine, *authcovDB) {
	t.Helper()
	db := &authcovDB{}
	a := authcovAPI(t, db)
	st := authcovNewStore(t)
	mgr := &session.Manager{Store: st}
	a.Sessions = mgr
	eng := &authcovPasskeyEngine{}
	a.Passkeys = &session.Passkeys{Store: st, Sessions: mgr, WA: eng}
	return a, st, eng, db
}

func TestAuthcovPasskeyRegistrationBranches(t *testing.T) {
	t.Run("begin store failure is internal", func(t *testing.T) {
		a, st, _, _ := authcovPasskeyAPI(t)
		st.userErr = errors.New("db down")
		rec := httptest.NewRecorder()
		a.BeginPasskeyRegistration(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkeys/register/begin", "{}"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("finish malformed body", func(t *testing.T) {
		a, _, _, _ := authcovPasskeyAPI(t)
		rec := httptest.NewRecorder()
		a.FinishPasskeyRegistration(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkeys/register/finish", "{"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	finishBody := `{"ceremony":"c","name":"key","credential":{}}`

	t.Run("finish expired ceremony", func(t *testing.T) {
		a, st, _, _ := authcovPasskeyAPI(t)
		st.ceremonyErr = pgx.ErrNoRows
		rec := httptest.NewRecorder()
		a.FinishPasskeyRegistration(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkeys/register/finish", finishBody))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ceremony_expired") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("finish rejected credential", func(t *testing.T) {
		a, _, eng, _ := authcovPasskeyAPI(t)
		eng.createErr = errors.New("bad attestation")
		rec := httptest.NewRecorder()
		a.FinishPasskeyRegistration(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkeys/register/finish", finishBody))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "passkey_rejected") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("finish duplicate authenticator", func(t *testing.T) {
		a, st, _, _ := authcovPasskeyAPI(t)
		st.createCredErr = authcovUniqueViolation()
		rec := httptest.NewRecorder()
		a.FinishPasskeyRegistration(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkeys/register/finish", finishBody))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
		}
	})

	t.Run("finish store failure is internal", func(t *testing.T) {
		a, st, _, _ := authcovPasskeyAPI(t)
		st.createCredErr = errors.New("db down")
		rec := httptest.NewRecorder()
		a.FinishPasskeyRegistration(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkeys/register/finish", finishBody))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("finish success enrolls", func(t *testing.T) {
		a, st, _, _ := authcovPasskeyAPI(t)
		st.createdCred = store.PasskeyCredential{
			ID: 1, Uuid: fixturePGUUID(t), Name: "key",
			CreatedAt: fixtureTimestamp(time.Now().UTC()),
		}
		rec := httptest.NewRecorder()
		a.FinishPasskeyRegistration(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkeys/register/finish", finishBody))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovPasskeyListAndDeleteBranches(t *testing.T) {
	t.Run("list store failure is internal", func(t *testing.T) {
		a, _, _, db := authcovPasskeyAPI(t)
		db.errOn = map[string]*authcovFail{"ListPasskeysForUser": {err: errors.New("db down")}}
		rec := httptest.NewRecorder()
		a.ListPasskeys(rec, authenticatedBrowserRequest(t, http.MethodGet, "/auth/passkeys", ""))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("delete malformed uuid", func(t *testing.T) {
		a, _, _, _ := authcovPasskeyAPI(t)
		rec := httptest.NewRecorder()
		req := withURLParam(authenticatedBrowserRequest(t, http.MethodDelete, "/auth/passkeys/nope", ""), "passkey_uuid", "nope")
		a.DeletePasskey(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("delete store failure is internal", func(t *testing.T) {
		a, _, _, db := authcovPasskeyAPI(t)
		db.errOn = map[string]*authcovFail{"DeletePasskeyForUser": {err: errors.New("db down")}}
		rec := httptest.NewRecorder()
		req := withURLParam(authenticatedBrowserRequest(t, http.MethodDelete, "/auth/passkeys/"+fixtureUUID, ""), "passkey_uuid", fixtureUUID)
		a.DeletePasskey(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("delete missing row", func(t *testing.T) {
		a, _, _, db := authcovPasskeyAPI(t)
		db.execTag = map[string]string{"DeletePasskeyForUser": "DELETE 0"}
		rec := httptest.NewRecorder()
		req := withURLParam(authenticatedBrowserRequest(t, http.MethodDelete, "/auth/passkeys/"+fixtureUUID, ""), "passkey_uuid", fixtureUUID)
		a.DeletePasskey(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovPasskeyLoginBranches(t *testing.T) {
	loginBody := `{"ceremony":"c","credential":{}}`

	t.Run("begin engine failure is internal", func(t *testing.T) {
		a, _, eng, _ := authcovPasskeyAPI(t)
		eng.beginErr = errors.New("engine down")
		rec := httptest.NewRecorder()
		a.BeginPasskeyLogin(rec, postJSON("/auth/passkey/login/begin", "{}"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("expired ceremony", func(t *testing.T) {
		a, st, _, _ := authcovPasskeyAPI(t)
		st.ceremonyErr = pgx.ErrNoRows
		rec := httptest.NewRecorder()
		a.FinishPasskeyLogin(rec, postJSON("/auth/passkey/login/finish", loginBody))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ceremony_expired") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("clone detection", func(t *testing.T) {
		a, _, eng, _ := authcovPasskeyAPI(t)
		eng.cred = &webauthn.Credential{}
		eng.cred.Authenticator.CloneWarning = true
		rec := httptest.NewRecorder()
		a.FinishPasskeyLogin(rec, postJSON("/auth/passkey/login/finish", loginBody))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "passkey_clone_detected") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("rejected assertion", func(t *testing.T) {
		a, _, eng, _ := authcovPasskeyAPI(t)
		eng.validateErr = errors.New("bad signature")
		rec := httptest.NewRecorder()
		a.FinishPasskeyLogin(rec, postJSON("/auth/passkey/login/finish", loginBody))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
		}
	})

	t.Run("counter persistence failure is internal", func(t *testing.T) {
		a, st, _, _ := authcovPasskeyAPI(t)
		st.updateCredErr = errors.New("db down")
		rec := httptest.NewRecorder()
		a.FinishPasskeyLogin(rec, postJSON("/auth/passkey/login/finish", loginBody))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("undeliverable cookie refused", func(t *testing.T) {
		a, _, _, _ := authcovPasskeyAPI(t)
		a.Sessions.Secure = true
		rec := httptest.NewRecorder()
		a.FinishPasskeyLogin(rec, postJSON("/auth/passkey/login/finish", loginBody))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "https_required") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("success opens session", func(t *testing.T) {
		a, _, _, _ := authcovPasskeyAPI(t)
		rec := httptest.NewRecorder()
		a.FinishPasskeyLogin(rec, postJSON("/auth/passkey/login/finish", loginBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
		if len(rec.Result().Cookies()) < 2 {
			t.Fatal("passkey login did not set both session cookies")
		}
	})
}

func TestAuthcovPasskeyStepUpBranches(t *testing.T) {
	stepUpBody := `{"ceremony":"c","credential":{}}`

	t.Run("begin engine failure is internal", func(t *testing.T) {
		a, _, eng, _ := authcovPasskeyAPI(t)
		eng.beginErr = errors.New("engine down")
		rec := httptest.NewRecorder()
		a.BeginPasskeyStepUp(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkey/stepup/begin", "{}"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		a, _, _, _ := authcovPasskeyAPI(t)
		rec := httptest.NewRecorder()
		a.FinishPasskeyStepUp(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkey/stepup/finish", "{"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("expired ceremony", func(t *testing.T) {
		a, st, _, _ := authcovPasskeyAPI(t)
		st.ceremonyErr = pgx.ErrNoRows
		rec := httptest.NewRecorder()
		a.FinishPasskeyStepUp(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkey/stepup/finish", stepUpBody))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ceremony_expired") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("clone detection", func(t *testing.T) {
		a, _, eng, _ := authcovPasskeyAPI(t)
		eng.cred = &webauthn.Credential{}
		eng.cred.Authenticator.CloneWarning = true
		rec := httptest.NewRecorder()
		a.FinishPasskeyStepUp(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkey/stepup/finish", stepUpBody))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "passkey_clone_detected") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("rejected assertion", func(t *testing.T) {
		a, _, eng, _ := authcovPasskeyAPI(t)
		eng.validateErr = errors.New("bad signature")
		rec := httptest.NewRecorder()
		a.FinishPasskeyStepUp(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkey/stepup/finish", stepUpBody))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
		}
	})

	t.Run("counter persistence failure is internal", func(t *testing.T) {
		a, st, _, _ := authcovPasskeyAPI(t)
		st.updateCredErr = errors.New("db down")
		rec := httptest.NewRecorder()
		a.FinishPasskeyStepUp(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkey/stepup/finish", stepUpBody))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("someone else's passkey elevates nothing", func(t *testing.T) {
		// The engine validates without resolving an owner: the zero owner is
		// not the session's user, which must be a 403.
		a, _, _, _ := authcovPasskeyAPI(t)
		rec := httptest.NewRecorder()
		a.FinishPasskeyStepUp(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkey/stepup/finish", stepUpBody))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body)
		}
	})

	ownStepUp := func(t *testing.T) (*API, *authcovDB) {
		t.Helper()
		a, st, eng, db := authcovPasskeyAPI(t)
		st.passkeyRow = store.GetPasskeyByCredentialIDRow{
			ID: 1, UserID: 1, UserUuid: fixturePGUUID(t),
			Credential: []byte("{}"), Email: "unit@example.test", UserName: "Unit",
		}
		eng.invokeHandler = true
		eng.rawID = []byte("cred")
		handle := fixturePGUUID(t)
		eng.handle = handle.Bytes[:]
		return a, db
	}

	t.Run("marker persistence failure is internal", func(t *testing.T) {
		a, db := ownStepUp(t)
		db.errOn = map[string]*authcovFail{"SetSessionMfaVerified": {err: errors.New("db down")}}
		rec := httptest.NewRecorder()
		a.FinishPasskeyStepUp(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkey/stepup/finish", stepUpBody))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("own passkey verifies", func(t *testing.T) {
		a, _ := ownStepUp(t)
		rec := httptest.NewRecorder()
		a.FinishPasskeyStepUp(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/passkey/stepup/finish", stepUpBody))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "verified_at") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})
}

// ---------------------------------------------------------------------------
// authoauth.go

func authcovOAuthAPI(t *testing.T) (*API, *authcovStore, *authcovOAuthClient, *authcovDB) {
	t.Helper()
	db := &authcovDB{}
	a := authcovAPI(t, db)
	st := authcovNewStore(t)
	mgr := &session.Manager{Store: st}
	a.Sessions = mgr
	enc, err := a.Keyring.Encrypt("oauth_provider_configs", "client_secret_enc", fixtureUUID, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	st.providerCfg = store.OauthProviderConfig{
		Uuid: fixturePGUUID(t), Provider: store.OauthProviderGithub,
		ClientID: "cid", ClientSecretEnc: enc, Enabled: true,
	}
	st.loginState = store.OauthLoginState{Purpose: "login", PkceVerifier: "v", Nonce: "n"}
	st.identity = store.Identity{ID: 1, Uuid: fixturePGUUID(t), UserID: 1, Provider: store.OauthProviderGithub}
	client := &authcovOAuthClient{who: &oidc.Identity{
		Subject: "sub", Email: "unit@example.test", EmailVerified: true, Name: "Unit",
	}}
	a.OAuth = &session.OAuth{
		Store: st, Sessions: mgr, Keyring: a.Keyring, Settings: a.Settings,
		Client: client, BaseURL: "http://localhost",
	}
	return a, st, client, db
}

func authcovCallbackRequest(query string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/auth/oauth/github/callback"+query, nil)
	return withURLParam(req, "oauth_provider", "github")
}

func TestAuthcovOauthProvidersInternalError(t *testing.T) {
	a, st, _, _ := authcovOAuthAPI(t)
	st.providersErr = errors.New("db down")
	rec := httptest.NewRecorder()
	a.OauthProviders(rec, httptest.NewRequest(http.MethodGet, "/auth/oauth/providers", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
}

func TestAuthcovStartOauthBranches(t *testing.T) {
	t.Run("login start answers the authorize url", func(t *testing.T) {
		a, _, _, _ := authcovOAuthAPI(t)
		rec := httptest.NewRecorder()
		req := withURLParam(postJSON("/auth/oauth/github/start", "{}"), "oauth_provider", "github")
		a.StartOauth(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "github.com/login/oauth/authorize") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("link purpose requires and uses the session", func(t *testing.T) {
		a, _, _, _ := authcovOAuthAPI(t)
		rec := httptest.NewRecorder()
		req := withURLParam(authenticatedBrowserRequest(t, http.MethodPost,
			"/auth/oauth/github/start?purpose=link", "{}"), "oauth_provider", "github")
		a.StartOauth(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("state persistence failure is internal", func(t *testing.T) {
		a, st, _, _ := authcovOAuthAPI(t)
		st.createStateErr = errors.New("db down")
		rec := httptest.NewRecorder()
		req := withURLParam(postJSON("/auth/oauth/github/start", "{}"), "oauth_provider", "github")
		a.StartOauth(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})
}

func TestAuthcovOauthCallbackErrorMapping(t *testing.T) {
	target := func(rec *httptest.ResponseRecorder) string { return rec.Header().Get("Location") }

	t.Run("invalid state", func(t *testing.T) {
		a, st, _, _ := authcovOAuthAPI(t)
		st.loginStateErr = pgx.ErrNoRows
		st.linkStateErr = pgx.ErrNoRows
		rec := httptest.NewRecorder()
		a.OauthCallback(rec, authcovCallbackRequest("?state=s&code=c"))
		if rec.Code != http.StatusSeeOther || target(rec) != "/sign-in?error=state_invalid" {
			t.Fatalf("response = %d %q", rec.Code, target(rec))
		}
	})

	t.Run("exchange refused", func(t *testing.T) {
		a, _, client, _ := authcovOAuthAPI(t)
		client.exchangeErr = errors.New("bad code")
		rec := httptest.NewRecorder()
		a.OauthCallback(rec, authcovCallbackRequest("?state=s&code=c"))
		if rec.Code != http.StatusSeeOther || target(rec) != "/sign-in?error=oauth_failed" {
			t.Fatalf("response = %d %q", rec.Code, target(rec))
		}
	})

	t.Run("account collision", func(t *testing.T) {
		a, st, _, _ := authcovOAuthAPI(t)
		st.identityErr = pgx.ErrNoRows // unknown subject, email already registered
		rec := httptest.NewRecorder()
		a.OauthCallback(rec, authcovCallbackRequest("?state=s&code=c"))
		if rec.Code != http.StatusSeeOther || target(rec) != "/sign-in?error=account_exists" {
			t.Fatalf("response = %d %q", rec.Code, target(rec))
		}
	})

	t.Run("registration disabled", func(t *testing.T) {
		a, st, _, _ := authcovOAuthAPI(t)
		st.identityErr = pgx.ErrNoRows
		st.emailUserErr = pgx.ErrNoRows
		rec := httptest.NewRecorder()
		a.OauthCallback(rec, authcovCallbackRequest("?state=s&code=c"))
		if rec.Code != http.StatusSeeOther || target(rec) != "/sign-in?error=registration_disabled" {
			t.Fatalf("response = %d %q", rec.Code, target(rec))
		}
	})

	t.Run("unverified email", func(t *testing.T) {
		a, st, client, _ := authcovOAuthAPI(t)
		st.identityErr = pgx.ErrNoRows
		client.who = &oidc.Identity{Subject: "sub"}
		rec := httptest.NewRecorder()
		a.OauthCallback(rec, authcovCallbackRequest("?state=s&code=c"))
		if rec.Code != http.StatusSeeOther || target(rec) != "/sign-in?error=email_unverified" {
			t.Fatalf("response = %d %q", rec.Code, target(rec))
		}
	})

	t.Run("identity taken lands on the security page", func(t *testing.T) {
		a, st, _, _ := authcovOAuthAPI(t)
		st.loginStateErr = pgx.ErrNoRows
		st.linkState = store.OauthLoginState{Purpose: "link", UserID: ptr(int64(1)), PkceVerifier: "v", Nonce: "n"}
		st.identity = store.Identity{ID: 2, UserID: 2}
		rec := httptest.NewRecorder()
		a.OauthCallback(rec, authcovCallbackRequest("?state=s&code=c"))
		if rec.Code != http.StatusSeeOther || target(rec) != "/security?error=identity_taken" {
			t.Fatalf("response = %d %q", rec.Code, target(rec))
		}
	})
}

func TestAuthcovOauthCallbackSuccessPaths(t *testing.T) {
	t.Run("link success without a readable session", func(t *testing.T) {
		a, st, _, _ := authcovOAuthAPI(t)
		st.loginStateErr = pgx.ErrNoRows
		st.linkState = store.OauthLoginState{Purpose: "link", UserID: ptr(int64(1)), PkceVerifier: "v", Nonce: "n"}
		st.identityErr = pgx.ErrNoRows // not linked anywhere yet: create it
		rec := httptest.NewRecorder()
		a.OauthCallback(rec, authcovCallbackRequest("?state=s&code=c"))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/security?linked=github" {
			t.Fatalf("response = %d %q", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("link success audits the signed-in linker", func(t *testing.T) {
		a, st, _, _ := authcovOAuthAPI(t)
		st.loginStateErr = pgx.ErrNoRows
		st.linkState = store.OauthLoginState{Purpose: "link", UserID: ptr(int64(1)), PkceVerifier: "v", Nonce: "n"}
		st.identityErr = pgx.ErrNoRows
		req := authcovCallbackRequest("?state=s&code=c")
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "session-token"})
		rec := httptest.NewRecorder()
		a.OauthCallback(rec, req)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/security?linked=github" {
			t.Fatalf("response = %d %q", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("plain http refuses the secure cookie", func(t *testing.T) {
		a, _, _, _ := authcovOAuthAPI(t)
		a.Sessions.Secure = true
		rec := httptest.NewRecorder()
		a.OauthCallback(rec, authcovCallbackRequest("?state=s&code=c"))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/sign-in?error=https_required" {
			t.Fatalf("response = %d %q", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("login success opens the session", func(t *testing.T) {
		a, _, _, _ := authcovOAuthAPI(t)
		rec := httptest.NewRecorder()
		a.OauthCallback(rec, authcovCallbackRequest("?state=s&code=c"))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
			t.Fatalf("response = %d %q", rec.Code, rec.Header().Get("Location"))
		}
		if len(rec.Result().Cookies()) < 2 {
			t.Fatal("oauth login did not set both session cookies")
		}
	})
}

func TestAuthcovIdentityManagementBranches(t *testing.T) {
	t.Run("list store failure is internal", func(t *testing.T) {
		a, _, _, db := authcovOAuthAPI(t)
		db.errOn = map[string]*authcovFail{"ListIdentitiesForUser": {err: errors.New("db down")}}
		rec := httptest.NewRecorder()
		a.ListIdentities(rec, authenticatedBrowserRequest(t, http.MethodGet, "/auth/identities", ""))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("delete malformed uuid", func(t *testing.T) {
		a, _, _, _ := authcovOAuthAPI(t)
		rec := httptest.NewRecorder()
		req := withURLParam(authenticatedBrowserRequest(t, http.MethodDelete, "/auth/identities/nope", ""), "identity_uuid", "nope")
		a.DeleteIdentity(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("delete unknown identity", func(t *testing.T) {
		a, st, _, _ := authcovOAuthAPI(t)
		st.deleteIdentityN = 0
		rec := httptest.NewRecorder()
		req := withURLParam(authenticatedBrowserRequest(t, http.MethodDelete, "/auth/identities/"+fixtureUUID, ""), "identity_uuid", fixtureUUID)
		a.DeleteIdentity(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("delete success", func(t *testing.T) {
		a, _, _, _ := authcovOAuthAPI(t)
		rec := httptest.NewRecorder()
		req := withURLParam(authenticatedBrowserRequest(t, http.MethodDelete, "/auth/identities/"+fixtureUUID, ""), "identity_uuid", fixtureUUID)
		a.DeleteIdentity(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body)
		}
	})
}

// ---------------------------------------------------------------------------
// authinvitations.go

func authcovInvitationAPI(t *testing.T) (*API, *authcovStore, *authcovDB) {
	t.Helper()
	db := &authcovDB{}
	a := authcovAPI(t, db)
	st := authcovNewStore(t)
	a.Sessions = &session.Manager{Store: st}
	return a, st, db
}

func TestAuthcovInvitationInfoSettingsFailure(t *testing.T) {
	a, _, db := authcovInvitationAPI(t)
	db.errOn = map[string]*authcovFail{"GetInstanceSettings": {err: errors.New("db down")}}
	rec := httptest.NewRecorder()
	a.InvitationInfo(rec, postJSON("/auth/invitations/lookup", `{"token":"t"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
}

func TestAuthcovSignUpFromInvitationBranches(t *testing.T) {
	signupBody := `{"token":"t","password":"a-long-enough-password"}`

	t.Run("undeliverable cookie refused", func(t *testing.T) {
		a, _, _ := authcovInvitationAPI(t)
		a.Sessions.Secure = true
		rec := httptest.NewRecorder()
		a.SignUpFromInvitation(rec, postJSON("/auth/invitations/signup", signupBody))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "https_required") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("settings failure is internal", func(t *testing.T) {
		a, _, db := authcovInvitationAPI(t)
		db.errOn = map[string]*authcovFail{"GetInstanceSettings": {err: errors.New("db down")}}
		rec := httptest.NewRecorder()
		a.SignUpFromInvitation(rec, postJSON("/auth/invitations/signup", signupBody))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("claim raced away", func(t *testing.T) {
		a, _, db := authcovInvitationAPI(t)
		db.noRowsOn = []string{"GetUserByEmail", "AcceptInvitation"}
		rec := httptest.NewRecorder()
		a.SignUpFromInvitation(rec, postJSON("/auth/invitations/signup", signupBody))
		if rec.Code != http.StatusGone {
			t.Fatalf("status = %d, want 410: %s", rec.Code, rec.Body)
		}
	})

	t.Run("claim failure is internal", func(t *testing.T) {
		a, _, db := authcovInvitationAPI(t)
		db.noRowsOn = []string{"GetUserByEmail"}
		db.errOn = map[string]*authcovFail{"AcceptInvitation": {err: errors.New("db down")}}
		rec := httptest.NewRecorder()
		a.SignUpFromInvitation(rec, postJSON("/auth/invitations/signup", signupBody))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("user creation failure is internal", func(t *testing.T) {
		a, _, db := authcovInvitationAPI(t)
		db.noRowsOn = []string{"GetUserByEmail"}
		db.errOn = map[string]*authcovFail{"CreateUser": {err: errors.New("db down")}}
		rec := httptest.NewRecorder()
		a.SignUpFromInvitation(rec, postJSON("/auth/invitations/signup", signupBody))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("membership failure is internal", func(t *testing.T) {
		a, _, db := authcovInvitationAPI(t)
		db.noRowsOn = []string{"GetUserByEmail"}
		db.errOn = map[string]*authcovFail{"AddTeamMember": {err: errors.New("db down")}}
		rec := httptest.NewRecorder()
		a.SignUpFromInvitation(rec, postJSON("/auth/invitations/signup", signupBody))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("session open failure is internal", func(t *testing.T) {
		a, st, db := authcovInvitationAPI(t)
		db.noRowsOn = []string{"GetUserByEmail"}
		st.membershipErr = errors.New("no membership")
		rec := httptest.NewRecorder()
		a.SignUpFromInvitation(rec, postJSON("/auth/invitations/signup", signupBody))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("success creates the account and signs in", func(t *testing.T) {
		a, _, db := authcovInvitationAPI(t)
		db.noRowsOn = []string{"GetUserByEmail"}
		rec := httptest.NewRecorder()
		// No name: the account falls back to its email address.
		a.SignUpFromInvitation(rec, postJSON("/auth/invitations/signup", signupBody))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
		}
		if len(rec.Result().Cookies()) < 2 {
			t.Fatal("signup did not set both session cookies")
		}
	})
}

func TestAuthcovAcceptInvitationBranches(t *testing.T) {
	t.Run("disabled feature", func(t *testing.T) {
		a, _ := flowAPI(t)
		rec := httptest.NewRecorder()
		a.AcceptInvitation(rec, postJSON("/auth/invitations/accept", `{"token":"t"}`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("no session", func(t *testing.T) {
		a, st, _ := authcovInvitationAPI(t)
		st.sessionErr = errors.New("no session")
		rec := httptest.NewRecorder()
		a.AcceptInvitation(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/invitations/accept", `{"token":"t"}`))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
		}
	})

	t.Run("token required", func(t *testing.T) {
		a, _, _ := authcovInvitationAPI(t)
		rec := httptest.NewRecorder()
		a.AcceptInvitation(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/invitations/accept", `{}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("dead link", func(t *testing.T) {
		a, _, db := authcovInvitationAPI(t)
		db.noRowsOn = []string{"AcceptInvitation"}
		rec := httptest.NewRecorder()
		a.AcceptInvitation(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/invitations/accept", `{"token":"t"}`))
		if rec.Code != http.StatusGone {
			t.Fatalf("status = %d, want 410: %s", rec.Code, rec.Body)
		}
	})

	t.Run("claim failure is internal", func(t *testing.T) {
		a, _, db := authcovInvitationAPI(t)
		db.errOn = map[string]*authcovFail{"AcceptInvitation": {err: errors.New("db down")}}
		rec := httptest.NewRecorder()
		a.AcceptInvitation(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/invitations/accept", `{"token":"t"}`))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("someone else's link", func(t *testing.T) {
		a, st, _ := authcovInvitationAPI(t)
		st.sessionRow.Email = "other@example.test" // the fixture invitation is for "unit"
		rec := httptest.NewRecorder()
		a.AcceptInvitation(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/invitations/accept", `{"token":"t"}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body)
		}
	})

	matching := func(t *testing.T) (*API, *authcovStore, *authcovDB) {
		t.Helper()
		a, st, db := authcovInvitationAPI(t)
		st.sessionRow.Email = "unit" // line up with the scan fixture
		return a, st, db
	}

	t.Run("membership failure is internal", func(t *testing.T) {
		a, _, db := matching(t)
		db.errOn = map[string]*authcovFail{"AddTeamMember": {err: errors.New("db down")}}
		rec := httptest.NewRecorder()
		a.AcceptInvitation(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/invitations/accept", `{"token":"t"}`))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("team load failure is internal", func(t *testing.T) {
		a, _, db := matching(t)
		db.errOn = map[string]*authcovFail{"GetTeamByID": {err: errors.New("db down")}}
		rec := httptest.NewRecorder()
		a.AcceptInvitation(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/invitations/accept", `{"token":"t"}`))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
	})

	t.Run("a failed switch still reports the join", func(t *testing.T) {
		a, st, _ := matching(t)
		// The session store knows no membership matching the joined team's
		// uuid, so the switch fails while the join stands.
		other := pguuidFromAuthcov(t, "22222222-2222-4222-8222-222222222222")
		st.memberships = []store.ListTeamMembershipsForUserRow{{
			TeamID: 9, Role: store.TeamRoleMember, TeamUuid: other, TeamName: "Elsewhere",
		}}
		rec := httptest.NewRecorder()
		a.AcceptInvitation(rec, authenticatedBrowserRequest(t, http.MethodPost, "/auth/invitations/accept", `{"token":"t"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
		var body struct {
			Switched bool `json:"switched"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Switched {
			t.Fatal("a failed switch must be reported as switched=false")
		}
	})
}

// pguuidFromAuthcov parses a literal UUID for fixtures that must differ from
// fixtureUUID.
func pguuidFromAuthcov(t *testing.T, raw string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(raw); err != nil {
		t.Fatal(err)
	}
	return u
}
