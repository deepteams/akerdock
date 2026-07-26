package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

type fakePasskeyEngine struct {
	beginRegistrationErr error
	createErr            error
	beginLoginErr        error
	validateErr          error
	credential           *webauthn.Credential
	rawID                []byte
	userHandle           []byte
	callHandler          bool
	handlerErr           error
}

func (f *fakePasskeyEngine) BeginRegistration(webauthn.User, ...webauthn.RegistrationOption) (*protocol.CredentialCreation, *webauthn.SessionData, error) {
	return &protocol.CredentialCreation{}, &webauthn.SessionData{Challenge: "registration"}, f.beginRegistrationErr
}

func (f *fakePasskeyEngine) CreateCredential(webauthn.User, webauthn.SessionData, []byte) (*webauthn.Credential, error) {
	if f.credential == nil {
		f.credential = &webauthn.Credential{ID: []byte("credential")}
	}
	return f.credential, f.createErr
}

func (f *fakePasskeyEngine) BeginDiscoverableLogin(...webauthn.LoginOption) (*protocol.CredentialAssertion, *webauthn.SessionData, error) {
	return &protocol.CredentialAssertion{}, &webauthn.SessionData{Challenge: "login"}, f.beginLoginErr
}

func (f *fakePasskeyEngine) ValidatePasskeyLogin(handler webauthn.DiscoverableUserHandler, _ webauthn.SessionData, _ []byte) (webauthn.User, *webauthn.Credential, error) {
	var user webauthn.User
	if f.callHandler {
		user, f.handlerErr = handler(f.rawID, f.userHandle)
		if f.handlerErr != nil {
			return nil, nil, f.handlerErr
		}
	}
	if f.credential == nil {
		f.credential = &webauthn.Credential{ID: []byte("credential")}
	}
	return user, f.credential, f.validateErr
}

func sessionData(t *testing.T, challenge string) []byte {
	t.Helper()
	raw, err := json.Marshal(webauthn.SessionData{Challenge: challenge})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func credentialData(t *testing.T, credential webauthn.Credential) []byte {
	t.Helper()
	raw, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func passkeyService(database *fakeSessionStore, engine passkeyEngine) *Passkeys {
	return &Passkeys{
		Store: database, Sessions: &Manager{Store: database}, WA: engine,
	}
}

func TestPasskeyUserAdapterAndAccessors(t *testing.T) {
	uuid := pguuid.MustParse("11111111-2222-4333-8444-555555555555")
	credential := webauthn.Credential{ID: []byte("credential")}
	database := &fakeSessionStore{passkeys: []store.PasskeyCredential{{
		ID: 1, Credential: credentialData(t, credential),
	}}}
	user := store.User{ID: 7, Uuid: uuid, Email: "user@example.test", Name: "User"}
	adapted, err := passkeyService(database, &fakePasskeyEngine{}).adaptUser(context.Background(), user)
	if err != nil || string(adapted.WebAuthnID()) != string(uuid.Bytes[:]) ||
		adapted.WebAuthnName() != user.Email || adapted.WebAuthnDisplayName() != user.Name ||
		len(adapted.WebAuthnCredentials()) != 1 {
		t.Fatalf("adapted = %#v, %v", adapted, err)
	}
	database.errs = map[string]error{"passkeys": errors.New("x")}
	if _, err := passkeyService(database, &fakePasskeyEngine{}).adaptUser(context.Background(), user); err == nil {
		t.Fatal("passkey list error hidden")
	}
	database.errs = nil
	database.passkeys[0].Credential = []byte("{")
	if _, err := passkeyService(database, &fakePasskeyEngine{}).adaptUser(context.Background(), user); err == nil ||
		!strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("corrupt stored credential = %v", err)
	}
}

func TestBeginPasskeyRegistration(t *testing.T) {
	user := store.User{
		ID: 7, Uuid: pguuid.MustParse("11111111-2222-4333-8444-555555555555"),
		Email: "user@example.test", Name: "User",
	}
	database := &fakeSessionStore{
		user: user,
		passkeys: []store.PasskeyCredential{{
			Credential: credentialData(t, webauthn.Credential{ID: []byte("existing")}),
		}},
	}
	engine := &fakePasskeyEngine{}
	creation, token, err := passkeyService(database, engine).
		BeginRegistration(context.Background(), user.ID)
	if err != nil || creation == nil || token == "" || len(database.ceremonyCreates) != 1 ||
		database.ceremonyCreates[0].Purpose != purposeRegistration ||
		database.ceremonyCreates[0].UserID == nil {
		t.Fatalf("BeginRegistration = %#v, %q, %v, ceremonies=%#v", creation, token, err, database.ceremonyCreates)
	}
	for _, tc := range []struct {
		db     *fakeSessionStore
		engine *fakePasskeyEngine
	}{
		{&fakeSessionStore{errs: map[string]error{"user": errors.New("x")}}, &fakePasskeyEngine{}},
		{&fakeSessionStore{user: user, errs: map[string]error{"passkeys": errors.New("x")}}, &fakePasskeyEngine{}},
		{&fakeSessionStore{user: user}, &fakePasskeyEngine{beginRegistrationErr: errors.New("x")}},
		{&fakeSessionStore{user: user, errs: map[string]error{"createCeremony": errors.New("x")}}, &fakePasskeyEngine{}},
	} {
		if _, _, err := passkeyService(tc.db, tc.engine).
			BeginRegistration(context.Background(), user.ID); err == nil {
			t.Fatal("begin registration failure hidden")
		}
	}
}

func TestFinishPasskeyRegistration(t *testing.T) {
	userID := int64(7)
	user := store.User{
		ID: userID, Uuid: pguuid.MustParse("11111111-2222-4333-8444-555555555555"),
	}
	base := func() *fakeSessionStore {
		return &fakeSessionStore{
			user: user,
			ceremony: store.PasskeyCeremony{
				UserID: &userID, Data: sessionData(t, "registration"),
			},
		}
	}
	database := base()
	credential, err := passkeyService(database, &fakePasskeyEngine{}).
		FinishRegistration(context.Background(), userID, "token", "Laptop", []byte("response"))
	if err != nil || credential.ID == 0 || len(database.passkeyCreates) != 1 ||
		database.passkeyCreates[0].Name != "Laptop" ||
		string(database.passkeyCreates[0].CredentialID) != "credential" {
		t.Fatalf("FinishRegistration = %#v, %v, creates=%#v", credential, err, database.passkeyCreates)
	}

	wrong := int64(8)
	for _, tc := range []struct {
		name   string
		db     *fakeSessionStore
		engine *fakePasskeyEngine
		token  string
		userID int64
	}{
		{"empty token", base(), &fakePasskeyEngine{}, "", userID},
		{"wrong user", base(), &fakePasskeyEngine{}, "token", wrong},
		{"missing ceremony user", func() *fakeSessionStore {
			db := base()
			db.ceremony.UserID = nil
			return db
		}(), &fakePasskeyEngine{}, "token", userID},
		{"user lookup", func() *fakeSessionStore {
			db := base()
			db.errs = map[string]error{"user": errors.New("x")}
			return db
		}(), &fakePasskeyEngine{}, "token", userID},
		{"adapt", func() *fakeSessionStore {
			db := base()
			db.errs = map[string]error{"passkeys": errors.New("x")}
			return db
		}(), &fakePasskeyEngine{}, "token", userID},
		{"verify", base(), &fakePasskeyEngine{createErr: errors.New("x")}, "token", userID},
		{"insert", func() *fakeSessionStore {
			db := base()
			db.errs = map[string]error{"createPasskey": errors.New("x")}
			return db
		}(), &fakePasskeyEngine{}, "token", userID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := passkeyService(tc.db, tc.engine).FinishRegistration(
				context.Background(), tc.userID, tc.token, "name", []byte("response"),
			); err == nil {
				t.Fatal("finish registration failure hidden")
			}
		})
	}
}

func TestBeginLoginAndStepUp(t *testing.T) {
	for _, purpose := range []string{purposeLogin, purposeStepUp} {
		database := &fakeSessionStore{}
		service := passkeyService(database, &fakePasskeyEngine{})
		var assertion *protocol.CredentialAssertion
		var token string
		var err error
		if purpose == purposeLogin {
			assertion, token, err = service.BeginLogin(context.Background())
		} else {
			assertion, token, err = service.BeginStepUp(context.Background())
		}
		if err != nil || assertion == nil || token == "" ||
			database.ceremonyCreates[0].Purpose != purpose {
			t.Fatalf("%s begin = %#v, %q, %v", purpose, assertion, token, err)
		}
	}
	for _, begin := range []func(*Passkeys) error{
		func(p *Passkeys) error { _, _, err := p.BeginLogin(context.Background()); return err },
		func(p *Passkeys) error { _, _, err := p.BeginStepUp(context.Background()); return err },
	} {
		if err := begin(passkeyService(&fakeSessionStore{}, &fakePasskeyEngine{
			beginLoginErr: errors.New("x"),
		})); err == nil {
			t.Fatal("begin login engine failure hidden")
		}
		database := &fakeSessionStore{errs: map[string]error{"createCeremony": errors.New("x")}}
		if err := begin(passkeyService(database, &fakePasskeyEngine{})); err == nil {
			t.Fatal("begin login store failure hidden")
		}
	}
}

func assertionStore(t *testing.T) *fakeSessionStore {
	t.Helper()
	uuid := pguuid.MustParse("11111111-2222-4333-8444-555555555555")
	credential := webauthn.Credential{ID: []byte("credential")}
	return &fakeSessionStore{
		user: store.User{ID: 7},
		membership: store.GetTeamMembershipForUserRow{
			TeamID: 8,
		},
		passkeyOwner: store.GetPasskeyByCredentialIDRow{
			ID: 5, UserID: 7, UserUuid: uuid, Credential: credentialData(t, credential),
		},
		ceremonyByKey: map[string]store.PasskeyCeremony{
			purposeLogin:  {Data: sessionData(t, "login")},
			purposeStepUp: {Data: sessionData(t, "stepup")},
		},
	}
}

func assertionEngine(database *fakeSessionStore) *fakePasskeyEngine {
	return &fakePasskeyEngine{
		callHandler: true, rawID: []byte("credential"),
		userHandle: database.passkeyOwner.UserUuid.Bytes[:],
	}
}

func TestFinishPasskeyLoginAndStepUp(t *testing.T) {
	database := assertionStore(t)
	session, token, err := passkeyService(database, assertionEngine(database)).
		FinishLogin(context.Background(), loginRequest(), "token", []byte("response"))
	if err != nil || session == nil || token == "" || len(database.passkeyUpdates) != 1 {
		t.Fatalf("FinishLogin = %#v, %q, %v, updates=%#v", session, token, err, database.passkeyUpdates)
	}
	database = assertionStore(t)
	userID, err := passkeyService(database, assertionEngine(database)).
		FinishStepUp(context.Background(), "token", []byte("response"))
	if err != nil || userID != 7 {
		t.Fatalf("FinishStepUp = %d, %v", userID, err)
	}
	database = assertionStore(t)
	database.errs = map[string]error{"user": errors.New("deleted")}
	if _, _, err := passkeyService(database, assertionEngine(database)).
		FinishLogin(context.Background(), loginRequest(), "token", []byte("response")); !errors.Is(err, ErrPasskeyRejected) {
		t.Fatalf("deleted owner login = %v", err)
	}
}

func TestVerifyPasskeyAssertionFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeSessionStore, *fakePasskeyEngine)
		want   error
	}{
		{"owner lookup", func(db *fakeSessionStore, _ *fakePasskeyEngine) {
			db.errs = map[string]error{"passkeyOwner": errors.New("x")}
		}, ErrPasskeyRejected},
		{"user handle mismatch", func(_ *fakeSessionStore, engine *fakePasskeyEngine) {
			engine.userHandle = []byte("wrong")
		}, ErrPasskeyRejected},
		{"stored credential", func(db *fakeSessionStore, _ *fakePasskeyEngine) {
			db.passkeyOwner.Credential = []byte("{")
		}, ErrPasskeyRejected},
		{"engine", func(_ *fakeSessionStore, engine *fakePasskeyEngine) {
			engine.validateErr = errors.New("x")
		}, ErrPasskeyRejected},
		{"clone", func(_ *fakeSessionStore, engine *fakePasskeyEngine) {
			engine.credential = &webauthn.Credential{
				Authenticator: webauthn.Authenticator{CloneWarning: true},
			}
		}, ErrPasskeyClone},
		{"update", func(db *fakeSessionStore, _ *fakePasskeyEngine) {
			db.errs = map[string]error{"updatePasskey": errors.New("x")}
		}, errors.New("want")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := assertionStore(t)
			engine := assertionEngine(database)
			tc.mutate(database, engine)
			_, err := passkeyService(database, engine).verifyAssertion(
				context.Background(), &webauthn.SessionData{}, []byte("response"),
			)
			if err == nil || (tc.want == ErrPasskeyClone && !errors.Is(err, ErrPasskeyClone)) {
				t.Fatalf("verify = %v", err)
			}
		})
	}
}

func TestPasskeyCeremonyPersistence(t *testing.T) {
	database := &fakeSessionStore{}
	service := passkeyService(database, &fakePasskeyEngine{})
	token, err := service.storeCeremony(
		context.Background(), purposeLogin, nil, &webauthn.SessionData{Challenge: "challenge"},
	)
	if err != nil || token == "" || len(database.ceremonyCreates) != 1 ||
		database.ceremonyCreates[0].TokenHash == token ||
		!database.ceremonyCreates[0].ExpiresAt.Valid {
		t.Fatalf("storeCeremony = %q, %v, args=%#v", token, err, database.ceremonyCreates)
	}
	database.ceremony = store.PasskeyCeremony{Data: sessionData(t, "challenge")}
	session, _, err := service.consumeCeremony(context.Background(), purposeLogin, "token")
	if err != nil || session.Challenge != "challenge" {
		t.Fatalf("consumeCeremony = %#v, %v", session, err)
	}
	if _, _, err := service.consumeCeremony(context.Background(), purposeLogin, ""); !errors.Is(err, ErrCeremonyExpired) {
		t.Fatalf("empty consume = %v", err)
	}
	database.errs = map[string]error{"consumeCeremony": errors.New("missing")}
	if _, _, err := service.consumeCeremony(context.Background(), purposeLogin, "token"); !errors.Is(err, ErrCeremonyExpired) {
		t.Fatalf("missing consume = %v", err)
	}
	database.errs = nil
	database.ceremony.Data = []byte("{")
	if _, _, err := service.consumeCeremony(context.Background(), purposeLogin, "token"); err == nil {
		t.Fatal("corrupt ceremony accepted")
	}

	database = &fakeSessionStore{errs: map[string]error{"createCeremony": errors.New("x")}}
	if _, err := passkeyService(database, &fakePasskeyEngine{}).storeCeremony(
		context.Background(), purposeLogin, nil, &webauthn.SessionData{},
	); err == nil {
		t.Fatal("ceremony insert error hidden")
	}
	old := randomReader
	randomReader = failingReader{err: errors.New("entropy")}
	defer func() { randomReader = old }()
	if _, err := passkeyService(&fakeSessionStore{}, &fakePasskeyEngine{}).storeCeremony(
		context.Background(), purposeLogin, nil, &webauthn.SessionData{},
	); err == nil {
		t.Fatal("ceremony entropy error hidden")
	}
}

func TestRealPasskeyEngineRejectsMalformedResponses(t *testing.T) {
	passkeys, err := NewPasskeys(nil, nil, "dock.example.test", "AkerDock", []string{"https://dock.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	user := &passkeyUser{
		id: []byte("1234567890123456"), name: "user@example.test", displayName: "User",
	}
	if _, _, err := passkeys.WA.BeginRegistration(user); err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	if _, _, err := passkeys.WA.BeginDiscoverableLogin(); err != nil {
		t.Fatalf("BeginDiscoverableLogin: %v", err)
	}
	if _, err := passkeys.WA.CreateCredential(user, webauthn.SessionData{}, []byte("{")); err == nil {
		t.Fatal("malformed registration response accepted")
	}
	if _, _, err := passkeys.WA.ValidatePasskeyLogin(
		func([]byte, []byte) (webauthn.User, error) { return user, nil },
		webauthn.SessionData{}, []byte("{"),
	); err == nil {
		t.Fatal("malformed assertion accepted")
	}
}

func TestPasskeySecurityConstants(t *testing.T) {
	if CeremonyLifetime <= 0 || purposeRegistration == purposeLogin || purposeLogin == purposeStepUp {
		t.Fatal("passkey ceremony purposes or lifetime are unsafe")
	}
	_ = http.MethodPost
}
