package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/oidc"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

type fakeOAuthClient struct {
	endpoints *oidc.Endpoints
	token     *oidc.TokenResponse
	identity  *oidc.Identity
	errs      map[string]error
	discover  []string
	exchanges int
	verified  int
	fetched   int
}

func (f *fakeOAuthClient) err(name string) error {
	if f.errs == nil {
		return nil
	}
	return f.errs[name]
}

func (f *fakeOAuthClient) Discover(_ context.Context, issuer string) (*oidc.Endpoints, error) {
	f.discover = append(f.discover, issuer)
	if f.endpoints == nil {
		f.endpoints = &oidc.Endpoints{Issuer: issuer, AuthorizeURL: "https://idp.example.test/auth", TokenURL: "https://idp.example.test/token"}
	}
	return f.endpoints, f.err("discover")
}

func (f *fakeOAuthClient) Exchange(context.Context, *oidc.Endpoints, string, string, string, string, string) (*oidc.TokenResponse, error) {
	f.exchanges++
	if f.token == nil {
		f.token = &oidc.TokenResponse{AccessToken: "access", IDToken: "id"}
	}
	return f.token, f.err("exchange")
}

func (f *fakeOAuthClient) VerifyIDToken(context.Context, *oidc.Endpoints, string, string, string, time.Time) (*oidc.Identity, error) {
	f.verified++
	return f.identity, f.err("verify")
}

func (f *fakeOAuthClient) FetchOAuth2Identity(context.Context, string, *oidc.Endpoints, string) (*oidc.Identity, error) {
	f.fetched++
	return f.identity, f.err("fetch")
}

type fakeInstanceSettings struct {
	value store.InstanceSetting
	err   error
}

func (f *fakeInstanceSettings) GetInstanceSettings(context.Context) (store.InstanceSetting, error) {
	return f.value, f.err
}

func oauthService(t *testing.T, database *fakeSessionStore, client *fakeOAuthClient, registration bool) *OAuth {
	t.Helper()
	keyring := sessionKeyring(t)
	uuid := pguuid.MustParse("11111111-2222-4333-8444-555555555555")
	secret, err := keyring.Encrypt(
		"oauth_provider_configs", "client_secret_enc", pguuid.String(uuid), []byte("client-secret"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if database.oauthConfig.Provider == "" {
		database.oauthConfig = store.OauthProviderConfig{
			Uuid: uuid, Provider: store.OauthProviderGithub, ClientID: "client-id",
			ClientSecretEnc: secret, Enabled: true,
		}
	}
	settings := instance.NewCache(&fakeInstanceSettings{
		value: store.InstanceSetting{RegistrationEnabled: registration},
	})
	return &OAuth{
		Store: database, Sessions: &Manager{Store: database}, Keyring: keyring,
		Settings: settings, Client: client, BaseURL: "https://dock.example.test",
	}
}

func TestEnabledOAuthProviders(t *testing.T) {
	custom := "My GitHub"
	database := &fakeSessionStore{oauthConfigs: []store.ListEnabledOauthProviderConfigsRow{
		{Provider: store.OauthProviderGithub, DisplayName: &custom},
		{Provider: store.OauthProviderGoogle},
		{Provider: store.OauthProvider("unknown")},
	}}
	got, err := oauthService(t, database, &fakeOAuthClient{}, false).EnabledProviders(context.Background())
	if err != nil || len(got) != 2 || got[0].Name != custom || got[1].Name != "Google" {
		t.Fatalf("EnabledProviders = %#v, %v", got, err)
	}
	database.errs = map[string]error{"oauthConfigs": errors.New("x")}
	if _, err := oauthService(t, database, &fakeOAuthClient{}, false).EnabledProviders(context.Background()); err == nil {
		t.Fatal("provider list error hidden")
	}
}

func TestOAuthStart(t *testing.T) {
	database := &fakeSessionStore{}
	service := oauthService(t, database, &fakeOAuthClient{}, false)
	authorize, err := service.Start(context.Background(), "github", oauthPurposeLogin, nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authorize)
	if err != nil || parsed.Host != "github.com" ||
		parsed.Query().Get("state") == "" || parsed.Query().Get("nonce") == "" ||
		parsed.Query().Get("code_challenge") == "" ||
		len(database.oauthStateCreates) != 1 {
		t.Fatalf("authorize = %q, state=%#v, err=%v", authorize, database.oauthStateCreates, err)
	}
	state := database.oauthStateCreates[0]
	if state.StateHash == parsed.Query().Get("state") || state.Purpose != oauthPurposeLogin ||
		!state.ExpiresAt.Valid {
		t.Fatalf("stored state = %#v", state)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*OAuth, *fakeSessionStore, *fakeOAuthClient)
	}{
		{"unknown provider", func(_ *OAuth, db *fakeSessionStore, _ *fakeOAuthClient) {
			db.oauthConfig.Provider = "unknown"
		}},
		{"disabled", func(_ *OAuth, db *fakeSessionStore, _ *fakeOAuthClient) {
			db.oauthConfig.Enabled = false
		}},
		{"state insert", func(_ *OAuth, db *fakeSessionStore, _ *fakeOAuthClient) {
			db.errs = map[string]error{"createOauthState": errors.New("x")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeSessionStore{}
			client := &fakeOAuthClient{}
			oauth := oauthService(t, db, client, false)
			tc.mutate(oauth, db, client)
			provider := "github"
			if tc.name == "unknown provider" {
				provider = "unknown"
			}
			if _, err := oauth.Start(context.Background(), provider, oauthPurposeLogin, nil); err == nil {
				t.Fatal("Start error hidden")
			}
		})
	}

	old := randomReader
	randomReader = failingReader{err: errors.New("entropy")}
	defer func() { randomReader = old }()
	if _, err := oauthService(t, &fakeSessionStore{}, &fakeOAuthClient{}, false).
		Start(context.Background(), "github", oauthPurposeLogin, nil); err == nil {
		t.Fatal("state entropy failure hidden")
	}
}

func TestOAuthProviderConfigAndEndpoints(t *testing.T) {
	database := &fakeSessionStore{}
	client := &fakeOAuthClient{}
	service := oauthService(t, database, client, false)
	if _, _, err := service.providerConfig(context.Background(), "unknown"); !errors.Is(err, ErrOAuthProviderUnavailable) {
		t.Fatalf("unknown provider = %v", err)
	}
	database.errs = map[string]error{"oauthConfig": errors.New("x")}
	if _, _, err := service.providerConfig(context.Background(), "github"); !errors.Is(err, ErrOAuthProviderUnavailable) {
		t.Fatalf("missing config = %v", err)
	}
	database.errs = nil
	cfg, profile, err := service.providerConfig(context.Background(), "github")
	if err != nil {
		t.Fatal(err)
	}
	endpoints, err := service.endpoints(context.Background(), cfg, profile)
	if err != nil || endpoints.UserinfoURL == "" {
		t.Fatalf("OAuth2 endpoints = %#v, %v", endpoints, err)
	}
	oidcProfile := oidc.Profiles["oidc"]
	if _, err := service.endpoints(context.Background(), cfg, oidcProfile); err == nil {
		t.Fatal("generic OIDC without issuer accepted")
	}
	issuer := "https://idp.example.test"
	cfg.IssuerUrl = &issuer
	if _, err := service.endpoints(context.Background(), cfg, oidcProfile); err != nil ||
		len(client.discover) != 1 {
		t.Fatalf("OIDC endpoints = %v, discoveries=%v", err, client.discover)
	}
	client.errs = map[string]error{"discover": errors.New("x")}
	if _, err := service.endpoints(context.Background(), cfg, oidcProfile); err == nil {
		t.Fatal("discovery error hidden")
	}
}

func TestResolveOAuthLoginUser(t *testing.T) {
	who := &oidc.Identity{
		Subject: "subject", Email: "user@example.test", EmailVerified: true, Name: "User",
	}
	t.Run("known subject", func(t *testing.T) {
		database := &fakeSessionStore{
			identity: store.Identity{UserID: 7}, user: store.User{ID: 7},
		}
		got, err := oauthService(t, database, &fakeOAuthClient{}, false).
			resolveLoginUser(context.Background(), "github", who)
		if err != nil || got.ID != 7 {
			t.Fatalf("resolve = %#v, %v", got, err)
		}
	})
	t.Run("unverified or absent email", func(t *testing.T) {
		for _, candidate := range []*oidc.Identity{
			{Subject: "x"},
			{Subject: "x", Email: "x@example.test", EmailVerified: false},
		} {
			database := &fakeSessionStore{errs: map[string]error{"identity": errors.New("missing")}}
			if _, err := oauthService(t, database, &fakeOAuthClient{}, false).
				resolveLoginUser(context.Background(), "github", candidate); !errors.Is(err, ErrOAuthEmailMissing) {
				t.Fatalf("resolve = %v", err)
			}
		}
	})
	t.Run("email collision", func(t *testing.T) {
		database := &fakeSessionStore{errs: map[string]error{"identity": errors.New("missing")}}
		if _, err := oauthService(t, database, &fakeOAuthClient{}, false).
			resolveLoginUser(context.Background(), "github", who); !errors.Is(err, ErrOAuthAccountCollision) {
			t.Fatalf("resolve = %v", err)
		}
	})
	t.Run("registration disabled", func(t *testing.T) {
		database := &fakeSessionStore{errs: map[string]error{
			"identity": errors.New("missing"), "userIncludingDeleted": errors.New("missing"),
		}}
		if _, err := oauthService(t, database, &fakeOAuthClient{}, false).
			resolveLoginUser(context.Background(), "github", who); !errors.Is(err, ErrOAuthRegistrationDisabled) {
			t.Fatalf("resolve = %v", err)
		}
	})
	t.Run("registration creates account and team", func(t *testing.T) {
		database := &fakeSessionStore{
			user: store.User{ID: 7}, team: store.Team{ID: 8},
			errs: map[string]error{
				"identity": errors.New("missing"), "userIncludingDeleted": errors.New("missing"),
			},
		}
		got, err := oauthService(t, database, &fakeOAuthClient{}, true).
			resolveLoginUser(context.Background(), "github", who)
		if err != nil || got.ID != 7 || len(database.userCreates) != 1 ||
			len(database.membershipCreates) != 1 || len(database.identityCreates) != 1 {
			t.Fatalf("resolve = %#v, %v db=%#v", got, err, database)
		}
		noName := *who
		noName.Name = ""
		database = &fakeSessionStore{
			user: store.User{ID: 9}, team: store.Team{ID: 10},
			errs: map[string]error{
				"identity": errors.New("missing"), "userIncludingDeleted": errors.New("missing"),
			},
		}
		_, _ = oauthService(t, database, &fakeOAuthClient{}, true).
			resolveLoginUser(context.Background(), "github", &noName)
		if database.userCreates[0].Name != who.Email {
			t.Fatalf("fallback name = %q", database.userCreates[0].Name)
		}
	})
	t.Run("a pending invitation authorizes signup even when registration is closed", func(t *testing.T) {
		database := &fakeSessionStore{
			user: store.User{ID: 7},
			pendingInvites: []store.ListPendingInvitationsByEmailRow{
				{ID: 1, TeamID: 42, Role: store.TeamRoleMember},
			},
			errs: map[string]error{
				"identity": errors.New("missing"), "userIncludingDeleted": errors.New("missing"),
			},
		}
		// registration=false: without the invitation this path returns
		// ErrOAuthRegistrationDisabled.
		got, err := oauthService(t, database, &fakeOAuthClient{}, false).
			resolveLoginUser(context.Background(), "github", who)
		if err != nil {
			t.Fatalf("invited signup refused: %v", err)
		}
		if got.ID != 7 || len(database.acceptedInvites) != 1 || database.acceptedInvites[0] != 1 {
			t.Fatalf("invitation not claimed atomically: %#v", database.acceptedInvites)
		}
		// Joined the invited team with the invited role — not a personal team.
		if len(database.membershipCreates) != 1 || database.membershipCreates[0].TeamID != 42 ||
			database.membershipCreates[0].Role != store.TeamRoleMember {
			t.Fatalf("did not join the invited team with its role: %#v", database.membershipCreates)
		}
	})
	t.Run("registration stage failures", func(t *testing.T) {
		for _, key := range []string{"createUser", "createTeam", "addMember", "createIdentity"} {
			database := &fakeSessionStore{
				user: store.User{ID: 7}, team: store.Team{ID: 8},
				errs: map[string]error{
					"identity": errors.New("missing"), "userIncludingDeleted": errors.New("missing"),
					key: errors.New(key),
				},
			}
			if _, err := oauthService(t, database, &fakeOAuthClient{}, true).
				resolveLoginUser(context.Background(), "github", who); err == nil {
				t.Errorf("%s error hidden", key)
			}
		}
	})
}

func TestOAuthLinkAndUnlink(t *testing.T) {
	who := &oidc.Identity{Subject: "subject", Email: "user@example.test"}
	for _, tc := range []struct {
		name   string
		db     *fakeSessionStore
		userID int64
		want   error
	}{
		{"same", &fakeSessionStore{identity: store.Identity{UserID: 7}}, 7, nil},
		{"taken", &fakeSessionStore{identity: store.Identity{UserID: 8}}, 7, ErrOAuthIdentityTaken},
		{"new", &fakeSessionStore{errs: map[string]error{"identity": errors.New("missing")}}, 7, nil},
		{"create error", &fakeSessionStore{errs: map[string]error{
			"identity": errors.New("missing"), "createIdentity": errors.New("x"),
		}}, 7, errors.New("want error")},
	} {
		err := oauthService(t, tc.db, &fakeOAuthClient{}, false).
			link(context.Background(), tc.userID, "github", who)
		if (err != nil) != (tc.want != nil) ||
			(tc.want == ErrOAuthIdentityTaken && !errors.Is(err, tc.want)) {
			t.Errorf("%s link = %v", tc.name, err)
		}
	}

	for _, tc := range []struct {
		db   *fakeSessionStore
		want error
	}{
		{&fakeSessionStore{errs: map[string]error{"credentials": errors.New("x")}}, errors.New("x")},
		{&fakeSessionStore{ints: map[string]int64{"credentials": 1}}, ErrLastCredential},
		{&fakeSessionStore{ints: map[string]int64{"credentials": 2}, errs: map[string]error{"deleteIdentity": errors.New("x")}}, errors.New("x")},
		{&fakeSessionStore{ints: map[string]int64{"credentials": 2}}, errors.New("not found")},
		{&fakeSessionStore{ints: map[string]int64{"credentials": 2, "deleteIdentity": 1}}, nil},
	} {
		err := oauthService(t, tc.db, &fakeOAuthClient{}, false).
			Unlink(context.Background(), 7, pguuid.MustParse("11111111-2222-4333-8444-555555555555"))
		if (err != nil) != (tc.want != nil) {
			t.Errorf("Unlink = %v", err)
		}
	}
}

func TestOAuthCallbackLoginAndLink(t *testing.T) {
	who := &oidc.Identity{
		Subject: "subject", Email: "user@example.test", EmailVerified: true, Name: "User",
	}
	t.Run("login", func(t *testing.T) {
		database := &fakeSessionStore{
			oauthStates: map[string]store.OauthLoginState{
				oauthPurposeLogin: {Purpose: oauthPurposeLogin, PkceVerifier: "verifier"},
			},
			identity: store.Identity{UserID: 7},
			user:     store.User{ID: 7, Email: who.Email, Name: who.Name},
			membership: store.GetTeamMembershipForUserRow{
				TeamID: 8, Role: store.TeamRoleOwner,
			},
		}
		client := &fakeOAuthClient{identity: who}
		result, err := oauthService(t, database, client, false).Callback(
			context.Background(), httptest.NewRequest(http.MethodGet, "/", nil),
			"github", "state", "code",
		)
		if err != nil || result.Purpose != oauthPurposeLogin ||
			result.Session == nil || result.SessionToken == "" ||
			client.fetched != 1 || len(database.clearedLogins) != 1 {
			t.Fatalf("Callback = %#v, %v", result, err)
		}
	})
	t.Run("link fallback purpose", func(t *testing.T) {
		userID := int64(7)
		database := &fakeSessionStore{
			oauthStates: map[string]store.OauthLoginState{
				oauthPurposeLink: {Purpose: oauthPurposeLink, UserID: &userID},
			},
			errs: map[string]error{"consumeOauth": errors.New("login purpose missing"), "identity": errors.New("missing")},
		}
		client := &fakeOAuthClient{identity: who}
		result, err := oauthService(t, database, client, false).Callback(
			context.Background(), httptest.NewRequest(http.MethodGet, "/", nil),
			"github", "state", "code",
		)
		if err != nil || result.Purpose != oauthPurposeLink || len(database.identityCreates) != 1 {
			t.Fatalf("Callback = %#v, %v", result, err)
		}
	})
}

func TestOAuthCallbackFailureTable(t *testing.T) {
	if _, err := oauthService(t, &fakeSessionStore{}, &fakeOAuthClient{}, false).
		Callback(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil), "github", "", ""); !errors.Is(err, ErrOAuthStateInvalid) {
		t.Fatalf("empty callback = %v", err)
	}

	who := &oidc.Identity{Subject: "subject"}
	cases := []struct {
		name   string
		mutate func(*OAuth, *fakeSessionStore, *fakeOAuthClient)
	}{
		{"state", func(_ *OAuth, db *fakeSessionStore, _ *fakeOAuthClient) {
			db.oauthStates = nil
			db.errs = map[string]error{"consumeOauth": errors.New("missing")}
		}},
		{"provider", func(_ *OAuth, db *fakeSessionStore, _ *fakeOAuthClient) {
			db.oauthConfig.Enabled = false
		}},
		{"decrypt", func(_ *OAuth, db *fakeSessionStore, _ *fakeOAuthClient) {
			db.oauthConfig.ClientSecretEnc = []byte("bad")
		}},
		{"exchange", func(_ *OAuth, _ *fakeSessionStore, client *fakeOAuthClient) {
			client.errs = map[string]error{"exchange": errors.New("x")}
		}},
		{"identity", func(_ *OAuth, _ *fakeSessionStore, client *fakeOAuthClient) {
			client.errs = map[string]error{"fetch": errors.New("x")}
		}},
		{"link without user", func(_ *OAuth, db *fakeSessionStore, _ *fakeOAuthClient) {
			db.oauthStates = map[string]store.OauthLoginState{
				oauthPurposeLink: {Purpose: oauthPurposeLink},
			}
			db.errs = map[string]error{"consumeOauth": errors.New("login missing")}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := &fakeSessionStore{oauthStates: map[string]store.OauthLoginState{
				oauthPurposeLogin: {Purpose: oauthPurposeLogin},
			}}
			client := &fakeOAuthClient{identity: who}
			service := oauthService(t, database, client, false)
			tc.mutate(service, database, client)
			if _, err := service.Callback(
				context.Background(), httptest.NewRequest(http.MethodGet, "/", nil),
				"github", "state", "code",
			); err == nil {
				t.Fatal("callback failure hidden")
			}
		})
	}
}

func TestOAuthOIDCCallbackUsesIDTokenVerification(t *testing.T) {
	issuer := "https://idp.example.test"
	database := &fakeSessionStore{
		oauthStates: map[string]store.OauthLoginState{
			oauthPurposeLogin: {Nonce: "nonce"},
		},
		identity: store.Identity{UserID: 7},
		user:     store.User{ID: 7},
		membership: store.GetTeamMembershipForUserRow{
			TeamID: 8,
		},
	}
	service := oauthService(t, database, &fakeOAuthClient{
		identity: &oidc.Identity{Subject: "subject"},
	}, false)
	service.Store.(*fakeSessionStore).oauthConfig.Provider = store.OauthProviderOidc
	service.Store.(*fakeSessionStore).oauthConfig.IssuerUrl = &issuer
	_, err := service.Callback(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil),
		"oidc", "state", "code")
	if err != nil || service.Client.(*fakeOAuthClient).verified != 1 {
		t.Fatalf("OIDC callback = %v, verifies=%d", err, service.Client.(*fakeOAuthClient).verified)
	}
}

func TestOAuthSmallHelpers(t *testing.T) {
	service := &OAuth{BaseURL: "https://dock.example.test"}
	if got := service.redirectURI("github"); got != "https://dock.example.test/auth/oauth/github/callback" {
		t.Fatalf("redirect = %q", got)
	}
	if emailPtr("") != nil || emailPtr("x@example.test") == nil {
		t.Fatal("emailPtr failed")
	}
	if StateLifetime <= 0 || !strings.Contains(ErrOAuthAccountCollision.Error(), "already exists") {
		t.Fatal("OAuth safety contracts changed")
	}
	_ = pgtype.UUID{}
}
