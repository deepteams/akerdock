package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/oidc"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// OAuth implements the federated dashboard login of PRD §10.2 on top of
// internal/oidc: start a round-trip, finish it into a session, and manage
// the explicit account links of §23.3.
//
// The one rule everything below serves: the provider's SUBJECT is the key,
// the email is a claim. A login matches an account through the identities
// table alone; an email collision never links silently — it stops the login
// and tells the user to sign in and link explicitly, because "same email at
// some provider" is exactly the takeover primitive §23.3 forbids.
//
// Like passkeys, a federated login skips the local TOTP: the IdP owns that
// account's MFA story, and demanding our TOTP after theirs would not add a
// factor — only a second enrolment of the same kind.
type OAuth struct {
	Store    Store
	Sessions *Manager
	Keyring  *envelope.Keyring
	Settings *instance.Cache
	Client   OAuthClient
	// BaseURL is where the provider sends the browser back:
	// {BaseURL}/auth/oauth/{provider}/callback. Derived from the instance
	// FQDN — pinned, like the passkey relying party, never from Host.
	BaseURL string
}

// OAuthClient is the OIDC provider client used by the login flow (§10.2).
type OAuthClient interface {
	Discover(context.Context, string) (*oidc.Endpoints, error)
	Exchange(context.Context, *oidc.Endpoints, string, string, string, string, string) (*oidc.TokenResponse, error)
	VerifyIDToken(context.Context, *oidc.Endpoints, string, string, string, time.Time) (*oidc.Identity, error)
	FetchOAuth2Identity(context.Context, string, *oidc.Endpoints, string) (*oidc.Identity, error)
}

// StateLifetime bounds the authorize→callback window. Ten minutes, not
// five: this round-trip may include the IdP's own login and MFA prompts.
const StateLifetime = 10 * time.Minute

const (
	oauthPurposeLogin = "login"
	oauthPurposeLink  = "link"
)

var (
	// ErrOAuthProviderUnavailable covers a provider that is absent, disabled
	// or unknown — one answer, the sign-in page only shows enabled ones.
	ErrOAuthProviderUnavailable = errors.New("this sign-in method is not available")

	// ErrOAuthStateInvalid covers an unknown, expired and replayed state
	// alike — indistinguishable on purpose.
	ErrOAuthStateInvalid = errors.New("expired or replayed sign-in attempt — start again")

	// ErrOAuthExchangeFailed is the catch-all for a round-trip the provider
	// refused (bad code, bad client credentials, unreachable IdP).
	ErrOAuthExchangeFailed = errors.New("the identity provider refused the sign-in")

	// ErrOAuthAccountCollision is the §23.3 stop: the provider vouched for
	// an email that already names a local account NOT linked to this
	// identity. Linking is explicit — sign in with the existing credential
	// and link from the security page.
	ErrOAuthAccountCollision = errors.New("an account with this email already exists — sign in with it, then link this provider from the Security page")

	// ErrOAuthRegistrationDisabled is returned when there is no matching identity,
	// no collision, and the instance does not take new accounts (§10.2, closed by default).
	ErrOAuthRegistrationDisabled = errors.New("registration is disabled on this instance")

	// ErrOAuthEmailMissing is returned when the provider did not vouch for any
	// email, so there is nothing safe to create an account from.
	ErrOAuthEmailMissing = errors.New("the identity provider reported no verified email for this account")

	// ErrOAuthIdentityTaken (link): this provider account is already linked
	// to ANOTHER user.
	ErrOAuthIdentityTaken = errors.New("this provider account is already linked to another user")

	// ErrLastCredential (unlink): removing this identity would leave the
	// account with no way to sign in at all.
	ErrLastCredential = errors.New("this is the account's last sign-in method — set a password or add another one first")
)

// EnabledProvider is what the sign-in page needs to draw one button.
type EnabledProvider struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// EnabledProviders lists the configured, enabled providers with their button
// labels. Public by design: the sign-in page is anonymous.
func (o *OAuth) EnabledProviders(ctx context.Context) ([]EnabledProvider, error) {
	rows, err := o.Store.ListEnabledOauthProviderConfigs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]EnabledProvider, 0, len(rows))
	for _, row := range rows {
		profile, ok := oidc.Profiles[string(row.Provider)]
		if !ok {
			continue
		}
		name := profile.DefaultName
		if row.DisplayName != nil && *row.DisplayName != "" {
			name = *row.DisplayName
		}
		out = append(out, EnabledProvider{Provider: string(row.Provider), Name: name})
	}
	return out, nil
}

// Start opens a round-trip: state + PKCE verifier + nonce parked in the
// database, authorize URL out. userID is nil for a login and the signed-in
// user for a link — the purpose is part of the state row, so one kind of
// round-trip can never be redeemed as the other.
func (o *OAuth) Start(ctx context.Context, provider, purpose string, userID *int64) (string, error) {
	cfg, profile, err := o.providerConfig(ctx, provider)
	if err != nil {
		return "", err
	}
	ep, err := o.endpoints(ctx, cfg, profile)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrOAuthProviderUnavailable, err)
	}

	state, err := randomToken()
	if err != nil {
		return "", err
	}
	nonce, err := randomToken()
	if err != nil {
		return "", err
	}
	verifier, err := oidc.NewVerifier()
	if err != nil {
		return "", err
	}

	// Same opportunistic purge as every other short-lived table: creation is
	// the only moment it grows.
	_, _ = o.Store.PurgeExpiredOauthLoginStates(ctx)
	if err := o.Store.CreateOauthLoginState(ctx, store.CreateOauthLoginStateParams{
		StateHash:    hashToken(state),
		Provider:     store.OauthProvider(provider),
		Purpose:      purpose,
		UserID:       userID,
		PkceVerifier: verifier,
		Nonce:        nonce,
		ExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(StateLifetime), Valid: true},
	}); err != nil {
		return "", err
	}

	return oidc.AuthorizeURL(ep, cfg.ClientID, o.redirectURI(provider), state, nonce, verifier, profile.Scopes), nil
}

// CallbackResult is what a finished round-trip produced: a session for a
// login, a linked identity for a link.
type CallbackResult struct {
	Purpose      string
	Session      *Session
	SessionToken string
}

// Callback finishes the round-trip the provider sent back: state consumed,
// code exchanged, identity verified, and then the one decision that
// matters — which local account this is, if any.
func (o *OAuth) Callback(ctx context.Context, r *http.Request, provider, state, code string) (*CallbackResult, error) {
	if state == "" || code == "" {
		return nil, ErrOAuthStateInvalid
	}
	row, err := o.Store.ConsumeOauthLoginState(ctx, store.ConsumeOauthLoginStateParams{
		StateHash: hashToken(state), Provider: store.OauthProvider(provider), Purpose: oauthPurposeLogin,
	})
	purpose := oauthPurposeLogin
	if err != nil {
		row, err = o.Store.ConsumeOauthLoginState(ctx, store.ConsumeOauthLoginStateParams{
			StateHash: hashToken(state), Provider: store.OauthProvider(provider), Purpose: oauthPurposeLink,
		})
		if err != nil {
			return nil, ErrOAuthStateInvalid
		}
		purpose = oauthPurposeLink
	}

	cfg, profile, err := o.providerConfig(ctx, provider)
	if err != nil {
		return nil, err
	}
	ep, err := o.endpoints(ctx, cfg, profile)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOAuthProviderUnavailable, err)
	}
	secret, err := o.Keyring.Decrypt("oauth_provider_configs", "client_secret_enc", pguuid.String(cfg.Uuid), cfg.ClientSecretEnc)
	if err != nil {
		return nil, err
	}

	tok, err := o.Client.Exchange(ctx, ep, cfg.ClientID, string(secret), o.redirectURI(provider), code, row.PkceVerifier)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOAuthExchangeFailed, err)
	}

	var who *oidc.Identity
	if profile.Kind == oidc.KindOIDC {
		// The nonce ties the signed ID token to THIS round-trip (§23.3): a
		// token captured elsewhere fails here even with a valid signature.
		who, err = o.Client.VerifyIDToken(ctx, ep, cfg.ClientID, row.Nonce, tok.IDToken, time.Now())
	} else {
		who, err = o.Client.FetchOAuth2Identity(ctx, provider, ep, tok.AccessToken)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOAuthExchangeFailed, err)
	}

	if purpose == oauthPurposeLink {
		if row.UserID == nil {
			return nil, ErrOAuthStateInvalid
		}
		if err := o.link(ctx, *row.UserID, provider, who); err != nil {
			return nil, err
		}
		return &CallbackResult{Purpose: purpose}, nil
	}

	user, err := o.resolveLoginUser(ctx, provider, who)
	if err != nil {
		return nil, err
	}
	sess, token, err := o.Sessions.Open(ctx, r, user)
	if err != nil {
		return nil, err
	}
	// A completed federated login is a completed login: the lockout counter
	// resets exactly like after a password+TOTP success.
	_ = o.Store.ClearFailedLogins(ctx, user.ID)
	return &CallbackResult{Purpose: purpose, Session: sess, SessionToken: token}, nil
}

// resolveLoginUser turns a verified provider identity into a local user —
// or refuses, one explicit reason per §23.3 rule.
func (o *OAuth) resolveLoginUser(ctx context.Context, provider string, who *oidc.Identity) (store.User, error) {
	var zero store.User
	if identity, err := o.Store.GetIdentity(ctx, store.GetIdentityParams{
		Provider: store.OauthProvider(provider), ProviderSubject: who.Subject,
	}); err == nil {
		// The subject is known: this IS the linked account. GetUserByID
		// filters soft-deleted users — a deleted account's identities open
		// nothing.
		return o.Store.GetUserByID(ctx, identity.UserID)
	}

	// Unknown subject. Everything from here depends on the email, so it must
	// be one the PROVIDER vouched for — an unverified email is attacker
	// input with a nice domain.
	if who.Email == "" || !who.EmailVerified {
		return zero, ErrOAuthEmailMissing
	}
	if _, err := o.Store.GetUserByEmailIncludingDeleted(ctx, who.Email); err == nil {
		// The email names an existing (or tombstoned) account with no link to
		// this identity: STOP. Auto-linking here is the account-takeover
		// primitive — whoever controls this email at the provider would
		// inherit the local account.
		return zero, ErrOAuthAccountCollision
	}

	settings, err := o.Settings.Get(ctx)
	if err != nil {
		return zero, err
	}
	if !settings.RegistrationEnabled {
		return zero, ErrOAuthRegistrationDisabled
	}

	// First sign-in of a new account: user (no password), personal team,
	// owner membership, identity — the bootstrap sequence, because an
	// account with no team can authenticate and then do nothing.
	name := who.Name
	if name == "" {
		name = who.Email
	}
	user, err := o.Store.CreateUser(ctx, store.CreateUserParams{Email: who.Email, Name: name})
	if err != nil {
		return zero, err
	}
	team, err := o.Store.CreatePersonalTeam(ctx, name)
	if err != nil {
		return zero, err
	}
	// The team creator is `admin`, the top team role (ADR-038 — `owner` merged in).
	if err := o.Store.AddTeamMember(ctx, store.AddTeamMemberParams{
		TeamID: team.ID, UserID: user.ID, Role: store.TeamRoleAdmin,
	}); err != nil {
		return zero, err
	}
	if _, err := o.Store.CreateIdentity(ctx, store.CreateIdentityParams{
		UserID: user.ID, Provider: store.OauthProvider(provider),
		ProviderSubject: who.Subject, Email: emailPtr(who.Email),
	}); err != nil {
		return zero, err
	}
	return user, nil
}

// link attaches the verified identity to the signed-in user of the state row.
func (o *OAuth) link(ctx context.Context, userID int64, provider string, who *oidc.Identity) error {
	if existing, err := o.Store.GetIdentity(ctx, store.GetIdentityParams{
		Provider: store.OauthProvider(provider), ProviderSubject: who.Subject,
	}); err == nil {
		if existing.UserID == userID {
			return nil // already linked to this very account — idempotent
		}
		return ErrOAuthIdentityTaken
	}
	_, err := o.Store.CreateIdentity(ctx, store.CreateIdentityParams{
		UserID: userID, Provider: store.OauthProvider(provider),
		ProviderSubject: who.Subject, Email: emailPtr(who.Email),
	})
	return err
}

// Unlink removes one of the user's identities — unless it is the last way
// into the account.
func (o *OAuth) Unlink(ctx context.Context, userID int64, identityUUID pgtype.UUID) error {
	credentials, err := o.Store.CountCredentialsForUser(ctx, userID)
	if err != nil {
		return err
	}
	if credentials <= 1 {
		return ErrLastCredential
	}
	n, err := o.Store.DeleteIdentityForUser(ctx, store.DeleteIdentityForUserParams{
		Uuid: identityUUID, UserID: userID,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("identity not found")
	}
	return nil
}

// providerConfig loads and gates one provider's configuration.
func (o *OAuth) providerConfig(ctx context.Context, provider string) (store.OauthProviderConfig, oidc.Profile, error) {
	var zero store.OauthProviderConfig
	profile, ok := oidc.Profiles[provider]
	if !ok {
		return zero, profile, ErrOAuthProviderUnavailable
	}
	cfg, err := o.Store.GetOauthProviderConfig(ctx, store.OauthProvider(provider))
	if err != nil || !cfg.Enabled {
		return zero, profile, ErrOAuthProviderUnavailable
	}
	return cfg, profile, nil
}

// endpoints resolves where to send the browser and the back-channel calls:
// discovery for the OIDC family, pinned constants for the OAuth2 one.
func (o *OAuth) endpoints(ctx context.Context, cfg store.OauthProviderConfig, profile oidc.Profile) (*oidc.Endpoints, error) {
	if profile.Kind != oidc.KindOIDC {
		ep := profile.Endpoints
		return &ep, nil
	}
	issuer := profile.Issuer
	if profile.NeedsIssuer {
		if cfg.IssuerUrl == nil || *cfg.IssuerUrl == "" {
			return nil, errors.New("no issuer_url configured")
		}
		issuer = *cfg.IssuerUrl
	}
	return o.Client.Discover(ctx, issuer)
}

func (o *OAuth) redirectURI(provider string) string {
	return o.BaseURL + "/auth/oauth/" + provider + "/callback"
}

func emailPtr(email string) *string {
	if email == "" {
		return nil
	}
	return &email
}
