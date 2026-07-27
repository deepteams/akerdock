package session

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/deepteams/akerdock/internal/store"
)

type fakeSessionStore struct {
	errs map[string]error
	ints map[string]int64

	user       store.User
	userByMail store.User
	membership store.GetTeamMembershipForUserRow
	session    store.Session
	sessionRow store.GetSessionByTokenHashRow
	settings   store.InstanceSetting
	factor     store.MfaFactor
	challenge  store.MfaChallenge

	clearedPending []int64

	passkeys      []store.PasskeyCredential
	passkey       store.PasskeyCredential
	passkeyOwner  store.GetPasskeyByCredentialIDRow
	ceremony      store.PasskeyCeremony
	ceremonyByKey map[string]store.PasskeyCeremony

	oauthConfigs []store.ListEnabledOauthProviderConfigsRow
	oauthConfig  store.OauthProviderConfig
	oauthStates  map[string]store.OauthLoginState
	identity     store.Identity
	team         store.Team

	pendingInvites  []store.ListPendingInvitationsByEmailRow
	acceptedInvites []int64

	failedLogins       []store.RecordFailedLoginParams
	clearedLogins      []int64
	sessionCreates     []store.CreateSessionParams
	touchedSessions    []int64
	revokedSessions    []int64
	factorUpserts      []store.UpsertUnconfirmedMfaFactorParams
	factorConfirms     []store.ConfirmMfaFactorParams
	recoveryConsumes   []store.ConsumeMfaRecoveryCodeParams
	factorTouches      []store.TouchMfaFactorUsedParams
	challengeCreates   []store.CreateMfaChallengeParams
	passkeyCreates     []store.CreatePasskeyCredentialParams
	passkeyUpdates     []store.UpdatePasskeyCredentialParams
	ceremonyCreates    []store.CreatePasskeyCeremonyParams
	oauthStateCreates  []store.CreateOauthLoginStateParams
	identityCreates    []store.CreateIdentityParams
	userCreates        []store.CreateUserParams
	membershipCreates  []store.AddTeamMemberParams
	identityDeletes    []store.DeleteIdentityForUserParams
	replacedRecoveries []store.ReplaceMfaRecoveryCodesParams
}

func (f *fakeSessionStore) err(name string) error {
	if f.errs == nil {
		return nil
	}
	return f.errs[name]
}

func (f *fakeSessionStore) number(name string) int64 {
	if f.ints == nil {
		return 0
	}
	return f.ints[name]
}

func (f *fakeSessionStore) GetUserByEmail(context.Context, string) (store.User, error) {
	if f.userByMail.ID != 0 || f.userByMail.Email != "" {
		return f.userByMail, f.err("userByEmail")
	}
	return f.user, f.err("userByEmail")
}

func (f *fakeSessionStore) GetUserByID(context.Context, int64) (store.User, error) {
	return f.user, f.err("user")
}

func (f *fakeSessionStore) GetUserByEmailIncludingDeleted(context.Context, string) (store.User, error) {
	return f.userByMail, f.err("userIncludingDeleted")
}

func (f *fakeSessionStore) RecordFailedLogin(_ context.Context, arg store.RecordFailedLoginParams) (store.RecordFailedLoginRow, error) {
	f.failedLogins = append(f.failedLogins, arg)
	return store.RecordFailedLoginRow{}, f.err("recordFailed")
}

func (f *fakeSessionStore) ClearFailedLogins(_ context.Context, id int64) error {
	f.clearedLogins = append(f.clearedLogins, id)
	return f.err("clearFailed")
}

func (f *fakeSessionStore) GetTeamMembershipForUser(context.Context, int64) (store.GetTeamMembershipForUserRow, error) {
	return f.membership, f.err("membership")
}

func (f *fakeSessionStore) CreateSession(_ context.Context, arg store.CreateSessionParams) (store.Session, error) {
	f.sessionCreates = append(f.sessionCreates, arg)
	if f.session.ID == 0 {
		f.session.ID = 70
	}
	return f.session, f.err("createSession")
}

func (f *fakeSessionStore) GetSessionByTokenHash(context.Context, string) (store.GetSessionByTokenHashRow, error) {
	return f.sessionRow, f.err("getSession")
}

func (f *fakeSessionStore) TouchSession(_ context.Context, id int64) error {
	f.touchedSessions = append(f.touchedSessions, id)
	return f.err("touchSession")
}

func (f *fakeSessionStore) RevokeSession(_ context.Context, id int64) error {
	f.revokedSessions = append(f.revokedSessions, id)
	return f.err("revokeSession")
}

func (f *fakeSessionStore) GetInstanceSettings(context.Context) (store.InstanceSetting, error) {
	return f.settings, f.err("settings")
}

func (f *fakeSessionStore) ClearMfaPendingForUser(_ context.Context, userID int64) error {
	f.clearedPending = append(f.clearedPending, userID)
	return f.err("clearPending")
}

func (f *fakeSessionStore) GetMfaFactorForUser(context.Context, int64) (store.MfaFactor, error) {
	return f.factor, f.err("factor")
}

func (f *fakeSessionStore) UpsertUnconfirmedMfaFactor(_ context.Context, arg store.UpsertUnconfirmedMfaFactorParams) (store.MfaFactor, error) {
	f.factorUpserts = append(f.factorUpserts, arg)
	return f.factor, f.err("upsertFactor")
}

func (f *fakeSessionStore) ConfirmMfaFactor(_ context.Context, arg store.ConfirmMfaFactorParams) (store.MfaFactor, error) {
	f.factorConfirms = append(f.factorConfirms, arg)
	return f.factor, f.err("confirmFactor")
}

func (f *fakeSessionStore) DeleteMfaFactorForUser(context.Context, int64) (int64, error) {
	return f.number("deleteFactor"), f.err("deleteFactor")
}

func (f *fakeSessionStore) ReplaceMfaRecoveryCodes(_ context.Context, arg store.ReplaceMfaRecoveryCodesParams) (int64, error) {
	f.replacedRecoveries = append(f.replacedRecoveries, arg)
	return f.number("replaceRecovery"), f.err("replaceRecovery")
}

func (f *fakeSessionStore) ConsumeMfaRecoveryCode(_ context.Context, arg store.ConsumeMfaRecoveryCodeParams) (int64, error) {
	f.recoveryConsumes = append(f.recoveryConsumes, arg)
	return f.number("consumeRecovery"), f.err("consumeRecovery")
}

func (f *fakeSessionStore) TouchMfaFactorUsed(_ context.Context, arg store.TouchMfaFactorUsedParams) (int64, error) {
	f.factorTouches = append(f.factorTouches, arg)
	return f.number("touchFactor"), f.err("touchFactor")
}

func (f *fakeSessionStore) PurgeExpiredMfaChallenges(context.Context) (int64, error) {
	return 0, f.err("purgeChallenges")
}

func (f *fakeSessionStore) CreateMfaChallenge(_ context.Context, arg store.CreateMfaChallengeParams) error {
	f.challengeCreates = append(f.challengeCreates, arg)
	return f.err("createChallenge")
}

func (f *fakeSessionStore) GetMfaChallenge(context.Context, string) (store.MfaChallenge, error) {
	return f.challenge, f.err("challenge")
}

func (f *fakeSessionStore) ConsumeMfaChallenge(context.Context, string) (store.MfaChallenge, error) {
	return f.challenge, f.err("consumeChallenge")
}

func (f *fakeSessionStore) ListPasskeysForUser(context.Context, int64) ([]store.PasskeyCredential, error) {
	return f.passkeys, f.err("passkeys")
}

func (f *fakeSessionStore) CreatePasskeyCredential(_ context.Context, arg store.CreatePasskeyCredentialParams) (store.PasskeyCredential, error) {
	f.passkeyCreates = append(f.passkeyCreates, arg)
	if f.passkey.ID == 0 {
		f.passkey.ID = 5
	}
	return f.passkey, f.err("createPasskey")
}

func (f *fakeSessionStore) GetPasskeyByCredentialID(context.Context, []byte) (store.GetPasskeyByCredentialIDRow, error) {
	return f.passkeyOwner, f.err("passkeyOwner")
}

func (f *fakeSessionStore) UpdatePasskeyCredential(_ context.Context, arg store.UpdatePasskeyCredentialParams) error {
	f.passkeyUpdates = append(f.passkeyUpdates, arg)
	return f.err("updatePasskey")
}

func (f *fakeSessionStore) PurgeExpiredPasskeyCeremonies(context.Context) (int64, error) {
	return 0, f.err("purgeCeremonies")
}

func (f *fakeSessionStore) CreatePasskeyCeremony(_ context.Context, arg store.CreatePasskeyCeremonyParams) error {
	f.ceremonyCreates = append(f.ceremonyCreates, arg)
	return f.err("createCeremony")
}

func (f *fakeSessionStore) ConsumePasskeyCeremony(_ context.Context, arg store.ConsumePasskeyCeremonyParams) (store.PasskeyCeremony, error) {
	if f.ceremonyByKey != nil {
		if row, ok := f.ceremonyByKey[arg.Purpose]; ok {
			return row, f.err("consumeCeremony")
		}
	}
	return f.ceremony, f.err("consumeCeremony")
}

func (f *fakeSessionStore) ListEnabledOauthProviderConfigs(context.Context) ([]store.ListEnabledOauthProviderConfigsRow, error) {
	return f.oauthConfigs, f.err("oauthConfigs")
}

func (f *fakeSessionStore) GetOauthProviderConfig(context.Context, store.OauthProvider) (store.OauthProviderConfig, error) {
	return f.oauthConfig, f.err("oauthConfig")
}

func (f *fakeSessionStore) PurgeExpiredOauthLoginStates(context.Context) (int64, error) {
	return 0, f.err("purgeOauth")
}

func (f *fakeSessionStore) CreateOauthLoginState(_ context.Context, arg store.CreateOauthLoginStateParams) error {
	f.oauthStateCreates = append(f.oauthStateCreates, arg)
	return f.err("createOauthState")
}

func (f *fakeSessionStore) ConsumeOauthLoginState(_ context.Context, arg store.ConsumeOauthLoginStateParams) (store.OauthLoginState, error) {
	if f.oauthStates != nil {
		if state, ok := f.oauthStates[arg.Purpose]; ok {
			return state, nil
		}
	}
	return store.OauthLoginState{}, f.err("consumeOauth")
}

func (f *fakeSessionStore) GetIdentity(context.Context, store.GetIdentityParams) (store.Identity, error) {
	return f.identity, f.err("identity")
}

func (f *fakeSessionStore) CreateIdentity(_ context.Context, arg store.CreateIdentityParams) (store.Identity, error) {
	f.identityCreates = append(f.identityCreates, arg)
	return f.identity, f.err("createIdentity")
}

func (f *fakeSessionStore) CountCredentialsForUser(context.Context, int64) (int32, error) {
	return int32(f.number("credentials")), f.err("credentials")
}

func (f *fakeSessionStore) DeleteIdentityForUser(_ context.Context, arg store.DeleteIdentityForUserParams) (int64, error) {
	f.identityDeletes = append(f.identityDeletes, arg)
	return f.number("deleteIdentity"), f.err("deleteIdentity")
}

func (f *fakeSessionStore) CreateUser(_ context.Context, arg store.CreateUserParams) (store.User, error) {
	f.userCreates = append(f.userCreates, arg)
	return f.user, f.err("createUser")
}

func (f *fakeSessionStore) CreatePersonalTeam(context.Context, string) (store.Team, error) {
	return f.team, f.err("createTeam")
}

func (f *fakeSessionStore) AddTeamMember(_ context.Context, arg store.AddTeamMemberParams) error {
	f.membershipCreates = append(f.membershipCreates, arg)
	return f.err("addMember")
}

func (f *fakeSessionStore) ListPendingInvitationsByEmail(context.Context, string) ([]store.ListPendingInvitationsByEmailRow, error) {
	return f.pendingInvites, f.err("listPendingInvites")
}

func (f *fakeSessionStore) AcceptInvitationByID(_ context.Context, id int64) (store.AcceptInvitationByIDRow, error) {
	f.acceptedInvites = append(f.acceptedInvites, id)
	for _, inv := range f.pendingInvites {
		if inv.ID == id {
			return store.AcceptInvitationByIDRow{TeamID: inv.TeamID, Role: inv.Role, CustomRoleID: inv.CustomRoleID}, nil
		}
	}
	return store.AcceptInvitationByIDRow{}, pgx.ErrNoRows
}
