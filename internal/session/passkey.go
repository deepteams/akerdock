package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/store"
)

// Passkeys implements WebAuthn enrolment and login for the dashboard.
//
// Why passkeys at all: a password can be phished, reused, and brute-forced —
// the lockout and the constant-time verify in this package only BOUND those
// attacks. A passkey removes them: the private key never leaves the
// authenticator, and the signature binds the origin, so a look-alike domain
// gets nothing to replay.
//
// Two decisions are deliberately strict:
//
//   - The relying-party ID is PINNED to the instance FQDN, never derived from
//     the Host header: a derived RP ID would let anyone who can make the
//     server answer under another name mint credentials for it.
//
//   - User verification is REQUIRED, not preferred: a passkey here replaces
//     the password entirely, so it must prove the user (PIN, biometric), not
//     merely that the authenticator was present.
type Passkeys struct {
	Store    Store
	Sessions *Manager
	WA       passkeyEngine
}

type passkeyEngine interface {
	BeginRegistration(webauthn.User, ...webauthn.RegistrationOption) (*protocol.CredentialCreation, *webauthn.SessionData, error)
	CreateCredential(webauthn.User, webauthn.SessionData, []byte) (*webauthn.Credential, error)
	BeginDiscoverableLogin(...webauthn.LoginOption) (*protocol.CredentialAssertion, *webauthn.SessionData, error)
	ValidatePasskeyLogin(webauthn.DiscoverableUserHandler, webauthn.SessionData, []byte) (webauthn.User, *webauthn.Credential, error)
}

type realPasskeyEngine struct{ engine *webauthn.WebAuthn }

func (e realPasskeyEngine) BeginRegistration(user webauthn.User, opts ...webauthn.RegistrationOption) (*protocol.CredentialCreation, *webauthn.SessionData, error) {
	return e.engine.BeginRegistration(user, opts...)
}
func (e realPasskeyEngine) CreateCredential(user webauthn.User, session webauthn.SessionData, response []byte) (*webauthn.Credential, error) {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return nil, err
	}
	return e.engine.CreateCredential(user, session, parsed)
}
func (e realPasskeyEngine) BeginDiscoverableLogin(opts ...webauthn.LoginOption) (*protocol.CredentialAssertion, *webauthn.SessionData, error) {
	return e.engine.BeginDiscoverableLogin(opts...)
}
func (e realPasskeyEngine) ValidatePasskeyLogin(handler webauthn.DiscoverableUserHandler, session webauthn.SessionData, response []byte) (webauthn.User, *webauthn.Credential, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return nil, nil, err
	}
	return e.engine.ValidatePasskeyLogin(handler, session, parsed)
}

// CeremonyLifetime bounds the begin→finish window. Five minutes is generous
// for a fingerprint prompt and short enough that a leaked challenge is stale
// before it is useful.
const CeremonyLifetime = 5 * time.Minute

const (
	purposeRegistration = "registration"
	purposeLogin        = "login"
	// purposeStepUp is the re-authentication ceremony of rbac-matrix §5: a
	// separate purpose so a login ceremony can never be redeemed as a step-up.
	purposeStepUp = "stepup"
)

var (
	// ErrCeremonyExpired covers unknown, expired and replayed ceremonies alike:
	// the caller cannot distinguish them, and must not be able to.
	ErrCeremonyExpired = errors.New("unknown or expired passkey ceremony — start again")

	// ErrPasskeyClone is returned when the authenticator's signature counter
	// went backwards: two devices are signing with the same credential, which
	// means the key was extracted. The login is refused, loudly.
	ErrPasskeyClone = errors.New("passkey signature counter went backwards — the credential may be cloned")

	// ErrPasskeyRejected is the generic verification failure of a login or
	// registration response. One message for every cause: a verification
	// endpoint that explains its refusals is an oracle.
	ErrPasskeyRejected = errors.New("passkey verification failed")
)

// RelyingParty derives the WebAuthn relying party from the instance FQDN.
//
// With an FQDN, the RP ID is its host (a registrable domain, never host:port)
// and the only accepted origin is the https one — the FQDN is how the operator
// said the instance is reached, and anything else answering under another name
// must not be able to mint credentials. Without an FQDN the fallback is
// localhost on the control-plane port: the one origin browsers treat as secure
// over plain HTTP, so a dev instance keeps working and nothing else does.
func RelyingParty(fqdn string, port int) (rpID string, origins []string) {
	if fqdn == "" {
		return "localhost", []string{fmt.Sprintf("http://localhost:%d", port)}
	}
	host := fqdn
	if h, _, ok := strings.Cut(fqdn, ":"); ok {
		host = h
	}
	return host, []string{"https://" + fqdn}
}

// NewPasskeys builds the WebAuthn engine pinned to the given relying party.
func NewPasskeys(st Store, sessions *Manager, rpID, displayName string, origins []string) (*Passkeys, error) {
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: displayName,
		RPOrigins:     origins,
		// No attestation: we are not curating authenticator models, and asking
		// for attestation only adds a consent prompt and a tracking surface.
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			// Resident (discoverable) keys make the usernameless login flow
			// possible: the authenticator knows who it is signing for.
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn config: %w", err)
	}
	return &Passkeys{Store: st, Sessions: sessions, WA: realPasskeyEngine{engine: wa}}, nil
}

// passkeyUser adapts a user row and its stored credentials to webauthn.User.
// The user handle is the user's public uuid: stable, opaque, and revealing
// nothing (a discoverable credential stores it inside the authenticator).
type passkeyUser struct {
	id          []byte
	name        string
	displayName string
	credentials []webauthn.Credential
}

func (u *passkeyUser) WebAuthnID() []byte                         { return u.id }
func (u *passkeyUser) WebAuthnName() string                       { return u.name }
func (u *passkeyUser) WebAuthnDisplayName() string                { return u.displayName }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// adaptUser loads the user's enrolled credentials and wraps everything as the
// library's User.
func (p *Passkeys) adaptUser(ctx context.Context, user store.User) (*passkeyUser, error) {
	rows, err := p.Store.ListPasskeysForUser(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	creds := make([]webauthn.Credential, 0, len(rows))
	for _, row := range rows {
		var c webauthn.Credential
		if err := json.Unmarshal(row.Credential, &c); err != nil {
			return nil, fmt.Errorf("stored passkey %d is unreadable: %w", row.ID, err)
		}
		creds = append(creds, c)
	}
	return &passkeyUser{
		id:          user.Uuid.Bytes[:],
		name:        user.Email,
		displayName: user.Name,
		credentials: creds,
	}, nil
}

// BeginRegistration opens an enrolment ceremony for the given (session-
// authenticated) user. It returns the creation options for the browser and
// the ceremony token the client must echo on finish.
func (p *Passkeys) BeginRegistration(ctx context.Context, userID int64) (*protocol.CredentialCreation, string, error) {
	user, err := p.Store.GetUserByID(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	pu, err := p.adaptUser(ctx, user)
	if err != nil {
		return nil, "", err
	}

	// Excluding the already-enrolled credentials makes the browser refuse to
	// re-register the same authenticator, instead of silently creating a
	// duplicate the user cannot tell apart in the revocation list.
	exclusions := make([]protocol.CredentialDescriptor, 0, len(pu.credentials))
	for _, c := range pu.credentials {
		exclusions = append(exclusions, c.Descriptor())
	}

	creation, sess, err := p.WA.BeginRegistration(pu, webauthn.WithExclusions(exclusions))
	if err != nil {
		return nil, "", err
	}

	token, err := p.storeCeremony(ctx, purposeRegistration, &userID, sess)
	if err != nil {
		return nil, "", err
	}
	return creation, token, nil
}

// FinishRegistration verifies the browser's response and persists the new
// credential under the given name.
func (p *Passkeys) FinishRegistration(ctx context.Context, userID int64, ceremonyToken, name string, response []byte) (store.PasskeyCredential, error) {
	sess, ceremonyUser, err := p.consumeCeremony(ctx, purposeRegistration, ceremonyToken)
	if err != nil {
		return store.PasskeyCredential{}, err
	}
	// The ceremony must belong to the session that finishes it: a registration
	// challenge issued to one user must not enrol a key for another.
	if ceremonyUser == nil || *ceremonyUser != userID {
		return store.PasskeyCredential{}, ErrCeremonyExpired
	}

	user, err := p.Store.GetUserByID(ctx, userID)
	if err != nil {
		return store.PasskeyCredential{}, err
	}
	pu, err := p.adaptUser(ctx, user)
	if err != nil {
		return store.PasskeyCredential{}, err
	}

	cred, err := p.WA.CreateCredential(pu, *sess, response)
	if err != nil {
		return store.PasskeyCredential{}, ErrPasskeyRejected
	}

	raw, err := json.Marshal(cred)
	if err != nil {
		return store.PasskeyCredential{}, err
	}
	return p.Store.CreatePasskeyCredential(ctx, store.CreatePasskeyCredentialParams{
		UserID:       userID,
		Name:         name,
		CredentialID: cred.ID,
		Credential:   raw,
	})
}

// BeginLogin opens a usernameless (discoverable) login ceremony.
func (p *Passkeys) BeginLogin(ctx context.Context) (*protocol.CredentialAssertion, string, error) {
	assertion, sess, err := p.WA.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		return nil, "", err
	}
	token, err := p.storeCeremony(ctx, purposeLogin, nil, sess)
	if err != nil {
		return nil, "", err
	}
	return assertion, token, nil
}

// FinishLogin verifies the assertion and opens a browser session for the
// credential's owner. Every verification failure surfaces as
// ErrPasskeyRejected — except a clone detection, which deserves its own loud
// error, because it means a credential was extracted, not merely mistyped.
func (p *Passkeys) FinishLogin(ctx context.Context, r *http.Request, ceremonyToken string, response []byte) (*Session, string, error) {
	sess, _, err := p.consumeCeremony(ctx, purposeLogin, ceremonyToken)
	if err != nil {
		return nil, "", err
	}
	owner, err := p.verifyAssertion(ctx, sess, response)
	if err != nil {
		return nil, "", err
	}
	user, err := p.Store.GetUserByID(ctx, owner.UserID)
	if err != nil {
		return nil, "", ErrPasskeyRejected
	}
	return p.Sessions.Open(ctx, r, user)
}

// BeginStepUp starts a re-authentication ceremony (rbac-matrix §5) for an
// already-authenticated user: same discoverable assertion as a login, stored
// under its own purpose.
func (p *Passkeys) BeginStepUp(ctx context.Context) (*protocol.CredentialAssertion, string, error) {
	assertion, sess, err := p.WA.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		return nil, "", err
	}
	token, err := p.storeCeremony(ctx, purposeStepUp, nil, sess)
	if err != nil {
		return nil, "", err
	}
	return assertion, token, nil
}

// FinishStepUp verifies the assertion and returns the id of the user who
// owns the credential — it mints nothing. The caller compares that id to the
// session's user: a valid passkey belonging to someone else proves nothing.
func (p *Passkeys) FinishStepUp(ctx context.Context, ceremonyToken string, response []byte) (int64, error) {
	sess, _, err := p.consumeCeremony(ctx, purposeStepUp, ceremonyToken)
	if err != nil {
		return 0, err
	}
	owner, err := p.verifyAssertion(ctx, sess, response)
	if err != nil {
		return 0, err
	}
	return owner.UserID, nil
}

// verifyAssertion validates an authenticator assertion against the stored
// credential and persists the moved signature counter. Shared by login and
// step-up: the crypto is identical, only what happens afterwards differs.
func (p *Passkeys) verifyAssertion(ctx context.Context, sess *webauthn.SessionData, response []byte) (store.GetPasskeyByCredentialIDRow, error) {
	var zero store.GetPasskeyByCredentialIDRow
	// The handler resolves "which user is this credential" for the library.
	// The user handle the authenticator stored at enrolment must match the
	// owner we know for that credential id — a mismatch is an attack, not a
	// lookup miss.
	var owner store.GetPasskeyByCredentialIDRow
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		row, err := p.Store.GetPasskeyByCredentialID(ctx, rawID)
		if err != nil {
			return nil, ErrPasskeyRejected
		}
		if string(userHandle) != string(row.UserUuid.Bytes[:]) {
			return nil, ErrPasskeyRejected
		}
		owner = row
		var c webauthn.Credential
		if err := json.Unmarshal(row.Credential, &c); err != nil {
			return nil, err
		}
		return &passkeyUser{
			id:          row.UserUuid.Bytes[:],
			name:        row.Email,
			displayName: row.UserName,
			credentials: []webauthn.Credential{c},
		}, nil
	}

	_, cred, err := p.WA.ValidatePasskeyLogin(handler, *sess, response)
	if err != nil {
		return zero, ErrPasskeyRejected
	}
	if cred.Authenticator.CloneWarning {
		return zero, ErrPasskeyClone
	}

	// Persist the moved signature counter BEFORE opening the session: the
	// clone detection above is only as good as the last stored counter.
	raw, err := json.Marshal(cred)
	if err != nil {
		return zero, err
	}
	if err := p.Store.UpdatePasskeyCredential(ctx, store.UpdatePasskeyCredentialParams{
		ID: owner.ID, Credential: raw,
	}); err != nil {
		return zero, err
	}
	return owner, nil
}

// --- ceremony persistence -----------------------------------------------

// storeCeremony persists the library's session data keyed by the hash of a
// fresh random token, and returns the clear token for the client to echo.
func (p *Passkeys) storeCeremony(ctx context.Context, purpose string, userID *int64, sess *webauthn.SessionData) (string, error) {
	// Expired ceremonies are purged opportunistically: begin is the only
	// moment this table grows, so it is also the right moment to shrink it.
	_, _ = p.Store.PurgeExpiredPasskeyCeremonies(ctx)

	token, err := randomToken()
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(sess)
	if err != nil {
		return "", err
	}
	if err := p.Store.CreatePasskeyCeremony(ctx, store.CreatePasskeyCeremonyParams{
		TokenHash: hashToken(token),
		Purpose:   purpose,
		UserID:    userID,
		Data:      data,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(CeremonyLifetime), Valid: true},
	}); err != nil {
		return "", err
	}
	return token, nil
}

// consumeCeremony redeems a ceremony token exactly once.
func (p *Passkeys) consumeCeremony(ctx context.Context, purpose, token string) (*webauthn.SessionData, *int64, error) {
	if token == "" {
		return nil, nil, ErrCeremonyExpired
	}
	row, err := p.Store.ConsumePasskeyCeremony(ctx, store.ConsumePasskeyCeremonyParams{
		TokenHash: hashToken(token), Purpose: purpose,
	})
	if err != nil {
		return nil, nil, ErrCeremonyExpired
	}
	var sess webauthn.SessionData
	if err := json.Unmarshal(row.Data, &sess); err != nil {
		return nil, nil, err
	}
	return &sess, row.UserID, nil
}
