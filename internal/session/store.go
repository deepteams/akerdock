package session

import (
	"context"

	"github.com/deepteams/akerdock/internal/store"
)

// Store is the persistence boundary for browser authentication. The generated
// SQL implementation satisfies it; unit tests can exercise lockout, replay,
// CSRF, OAuth, passkey and TOTP decisions without PostgreSQL.
type Store interface {
	GetUserByEmail(context.Context, string) (store.User, error)
	GetUserByID(context.Context, int64) (store.User, error)
	GetUserByEmailIncludingDeleted(context.Context, string) (store.User, error)
	RecordFailedLogin(context.Context, store.RecordFailedLoginParams) (store.RecordFailedLoginRow, error)
	ClearFailedLogins(context.Context, int64) error
	GetTeamMembershipForUser(context.Context, store.GetTeamMembershipForUserParams) (store.GetTeamMembershipForUserRow, error)
	// The team switcher (PRD §37): the teams a user may act in, moving a live
	// session into one of them, and remembering it for the next login.
	ListTeamMembershipsForUser(context.Context, int64) ([]store.ListTeamMembershipsForUserRow, error)
	SetSessionCurrentTeam(context.Context, store.SetSessionCurrentTeamParams) (int64, error)
	SetUserLastTeam(context.Context, store.SetUserLastTeamParams) error
	CreateSession(context.Context, store.CreateSessionParams) (store.Session, error)
	GetSessionByTokenHash(context.Context, string) (store.GetSessionByTokenHashRow, error)
	TouchSession(context.Context, int64) error
	RevokeSession(context.Context, int64) error
	GetInstanceSettings(context.Context) (store.InstanceSetting, error)
	ClearMfaPendingForUser(context.Context, int64) error

	GetMfaFactorForUser(context.Context, int64) (store.MfaFactor, error)
	// CountPasskeysForUser lets forced MFA enrolment treat a passkey (user
	// verification required) as a satisfied factor, not only a TOTP secret.
	CountPasskeysForUser(context.Context, int64) (int64, error)
	UpsertUnconfirmedMfaFactor(context.Context, store.UpsertUnconfirmedMfaFactorParams) (store.MfaFactor, error)
	ConfirmMfaFactor(context.Context, store.ConfirmMfaFactorParams) (store.MfaFactor, error)
	DeleteMfaFactorForUser(context.Context, int64) (int64, error)
	ReplaceMfaRecoveryCodes(context.Context, store.ReplaceMfaRecoveryCodesParams) (int64, error)
	ConsumeMfaRecoveryCode(context.Context, store.ConsumeMfaRecoveryCodeParams) (int64, error)
	TouchMfaFactorUsed(context.Context, store.TouchMfaFactorUsedParams) (int64, error)
	// SetSessionTotpVerified stamps the TOTP step-up marker (ADR-045 §5) —
	// a different column from the passkey one, so a TOTP can never satisfy
	// the passkey-only ritual the root terminal requires.
	SetSessionTotpVerified(context.Context, int64) error
	PurgeExpiredMfaChallenges(context.Context) (int64, error)
	CreateMfaChallenge(context.Context, store.CreateMfaChallengeParams) error
	GetMfaChallenge(context.Context, string) (store.MfaChallenge, error)
	ConsumeMfaChallenge(context.Context, string) (store.MfaChallenge, error)

	ListPasskeysForUser(context.Context, int64) ([]store.PasskeyCredential, error)
	CreatePasskeyCredential(context.Context, store.CreatePasskeyCredentialParams) (store.PasskeyCredential, error)
	GetPasskeyByCredentialID(context.Context, []byte) (store.GetPasskeyByCredentialIDRow, error)
	UpdatePasskeyCredential(context.Context, store.UpdatePasskeyCredentialParams) error
	PurgeExpiredPasskeyCeremonies(context.Context) (int64, error)
	CreatePasskeyCeremony(context.Context, store.CreatePasskeyCeremonyParams) error
	ConsumePasskeyCeremony(context.Context, store.ConsumePasskeyCeremonyParams) (store.PasskeyCeremony, error)

	ListEnabledOauthProviderConfigs(context.Context) ([]store.ListEnabledOauthProviderConfigsRow, error)
	GetOauthProviderConfig(context.Context, store.OauthProvider) (store.OauthProviderConfig, error)
	PurgeExpiredOauthLoginStates(context.Context) (int64, error)
	CreateOauthLoginState(context.Context, store.CreateOauthLoginStateParams) error
	ConsumeOauthLoginState(context.Context, store.ConsumeOauthLoginStateParams) (store.OauthLoginState, error)
	GetIdentity(context.Context, store.GetIdentityParams) (store.Identity, error)
	CreateIdentity(context.Context, store.CreateIdentityParams) (store.Identity, error)
	CountCredentialsForUser(context.Context, int64) (int32, error)
	DeleteIdentityForUser(context.Context, store.DeleteIdentityForUserParams) (int64, error)
	CreateUser(context.Context, store.CreateUserParams) (store.User, error)
	CreatePersonalTeam(context.Context, string) (store.Team, error)
	AddTeamMember(context.Context, store.AddTeamMemberParams) error
	// Invitation-driven SSO signup (§10.2): a pending invitation authorizes
	// account creation on first OAuth login even when open registration is off.
	ListPendingInvitationsByEmail(context.Context, string) ([]store.ListPendingInvitationsByEmailRow, error)
	AcceptInvitationByID(context.Context, int64) (store.AcceptInvitationByIDRow, error)
}
