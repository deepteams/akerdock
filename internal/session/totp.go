package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/totp"
)

// TOTP implements the 2FA of PRD §10.2: an authenticator-app second factor
// on the password login, with single-use recovery codes (§23.3).
//
// It deliberately does NOT touch sessions.mfa_verified_at: that column marks
// a recent PASSKEY re-authentication, the only step-up rbac-matrix §5 accepts
// for a root terminal. A TOTP typed at login proves the login; it must not
// leak into the stronger ceremony.
//
// Passkey logins skip TOTP entirely: a passkey already is a second factor —
// possession plus the verification the authenticator enforced — and it is
// phishing-resistant, which TOTP is not. Demanding a TOTP after a passkey
// would add a WEAKER factor on top of a stronger one.
type TOTP struct {
	Store    *store.Queries
	Sessions *Manager
	Keyring  *envelope.Keyring
}

const (
	// ChallengeLifetime bounds the password→code window of a two-step login.
	// Same figure as the passkey ceremonies, for the same reason: generous
	// for a human reading a code, stale before it is worth stealing.
	ChallengeLifetime = 5 * time.Minute

	// RecoveryCodeCount is how many single-use codes enabling 2FA hands out.
	RecoveryCodeCount = 10

	// recoveryCodeBytes sizes one recovery code: 8 bytes is 64 bits, far past
	// what the account lockout lets anyone enumerate.
	recoveryCodeBytes = 8

	// Issuer is the label under which authenticator apps file the entry.
	// The product name, never the instance FQDN: ADR-022 forbids surfacing
	// the instance under another brand, and the account part of the URI
	// already carries the user.
	Issuer = "AkerDock"
)

var (
	// ErrMFARequired is how Login says "the password was right, now the
	// code": deliberately an error, so no caller can forget the second step
	// by only checking err == nil.
	ErrMFARequired = errors.New("a TOTP code is required to finish this login")

	// ErrMFAChallengeExpired covers an unknown, expired and consumed
	// challenge alike — indistinguishable on purpose.
	ErrMFAChallengeExpired = errors.New("unknown or expired login challenge — sign in again")

	// ErrMFACodeInvalid is the one answer for every wrong code: wrong digits,
	// replayed step, spent recovery code. Anything more specific is an oracle.
	ErrMFACodeInvalid = errors.New("invalid code")

	// ErrMFAAlreadyEnabled refuses a setup over a confirmed factor: replacing
	// an active second factor must require proving the old one (Disable),
	// not merely asking.
	ErrMFAAlreadyEnabled = errors.New("two-factor authentication is already enabled")

	// ErrMFANotConfigured is returned when there is no factor to act on.
	ErrMFANotConfigured = errors.New("two-factor authentication is not enabled")
)

// Setup starts enrolment: it mints a fresh secret, stores it envelope-
// encrypted and UNCONFIRMED, and returns the secret with its provisioning
// URI for the authenticator app. Nothing changes for the login until Confirm
// proves the app actually has the secret — a 2FA enabled on a mistyped scan
// locks the user out, permanently and politely.
func (t *TOTP) Setup(ctx context.Context, userID int64) (secret, uri string, err error) {
	user, err := t.Store.GetUserByID(ctx, userID)
	if err != nil {
		return "", "", err
	}
	secret, err = totp.GenerateSecret()
	if err != nil {
		return "", "", err
	}

	// The row uuid participates in the envelope AAD, so it is generated here,
	// before the insert (data-dictionary §2.7).
	u, err := pguuid.New()
	if err != nil {
		return "", "", err
	}
	enc, err := t.Keyring.Encrypt("mfa_factors", "secret_enc", pguuid.String(u), []byte(secret))
	if err != nil {
		return "", "", err
	}

	if _, err := t.Store.UpsertUnconfirmedMfaFactor(ctx, store.UpsertUnconfirmedMfaFactorParams{
		Uuid: u, UserID: userID, SecretEnc: enc,
	}); err != nil {
		// The upsert only fires over an UNCONFIRMED row; a confirmed factor
		// makes it return nothing, which is the polite SQL way to say no.
		if isNoRows(err) {
			return "", "", ErrMFAAlreadyEnabled
		}
		return "", "", err
	}
	return secret, totp.URI(Issuer, user.Email, secret), nil
}

// Confirm turns the factor on: the code proves the authenticator holds the
// secret, and the recovery codes are minted and returned — ONCE. Only their
// hashes survive this call.
func (t *TOTP) Confirm(ctx context.Context, userID int64, code string) ([]string, error) {
	factor, err := t.Store.GetMfaFactorForUser(ctx, userID)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrMFANotConfigured
		}
		return nil, err
	}
	if factor.ConfirmedAt.Valid {
		return nil, ErrMFAAlreadyEnabled
	}

	secret, err := t.secretOf(factor)
	if err != nil {
		return nil, err
	}
	matched, ok := totp.Validate(secret, code, time.Now())
	if !ok {
		return nil, ErrMFACodeInvalid
	}

	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	if _, err := t.Store.ConfirmMfaFactor(ctx, store.ConfirmMfaFactorParams{
		UserID:             userID,
		RecoveryCodeHashes: hashes,
		LastUsedAt:         stepTime(matched),
	}); err != nil {
		if isNoRows(err) {
			return nil, ErrMFAAlreadyEnabled
		}
		return nil, err
	}
	return codes, nil
}

// Enabled reports whether the user has a CONFIRMED factor, with the metadata
// the settings page shows. An unconfirmed leftover from an abandoned setup
// counts as disabled — it never guarded anything.
func (t *TOTP) Enabled(ctx context.Context, userID int64) (enabled bool, confirmedAt time.Time, recoveryLeft int, err error) {
	factor, err := t.Store.GetMfaFactorForUser(ctx, userID)
	if err != nil {
		if isNoRows(err) {
			return false, time.Time{}, 0, nil
		}
		return false, time.Time{}, 0, err
	}
	if !factor.ConfirmedAt.Valid {
		return false, time.Time{}, 0, nil
	}
	return true, factor.ConfirmedAt.Time, len(factor.RecoveryCodeHashes), nil
}

// CreateChallenge opens the password→code window of a two-step login and
// returns the clear token the client must echo with its code. Called by
// Login once the password verified; the token is the ONLY thing the client
// holds between the two steps — it names no user and unlocks nothing alone.
func (m *Manager) CreateChallenge(ctx context.Context, userID int64) (string, error) {
	// Same opportunistic purge as the passkey ceremonies: creation is the only
	// moment this table grows, so it is the right moment to shrink it.
	_, _ = m.Store.PurgeExpiredMfaChallenges(ctx)

	token, err := randomToken()
	if err != nil {
		return "", err
	}
	if err := m.Store.CreateMfaChallenge(ctx, store.CreateMfaChallengeParams{
		TokenHash: hashToken(token),
		UserID:    userID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(ChallengeLifetime), Valid: true},
	}); err != nil {
		return "", err
	}
	return token, nil
}

// VerifyLogin finishes a two-step login: challenge + TOTP code (or recovery
// code) in, session out.
//
// A wrong code does NOT consume the challenge — a typo must not send the
// user back to the password form — but it DOES count toward the account
// lockout: the challenge holder gets the same five attempts an attacker at
// the password prompt would (§23.3), not an unlimited oracle.
func (t *TOTP) VerifyLogin(ctx context.Context, r *http.Request, challengeToken, code, recoveryCode string) (*Session, string, error) {
	if challengeToken == "" {
		return nil, "", ErrMFAChallengeExpired
	}
	chal, err := t.Store.GetMfaChallenge(ctx, hashToken(challengeToken))
	if err != nil {
		return nil, "", ErrMFAChallengeExpired
	}
	user, err := t.Store.GetUserByID(ctx, chal.UserID)
	if err != nil {
		return nil, "", ErrMFAChallengeExpired
	}
	if user.LockedUntil.Valid && user.LockedUntil.Time.After(time.Now()) {
		return nil, "", ErrAccountLocked
	}

	if err := t.redeemCode(ctx, user, code, recoveryCode); err != nil {
		if errors.Is(err, ErrMFANotConfigured) {
			// The factor vanished between the two steps (disabled elsewhere,
			// account rebuilt): to this login it is just a dead challenge.
			return nil, "", ErrMFAChallengeExpired
		}
		return nil, "", err
	}

	// Consume-then-open: DELETE ... RETURNING makes the challenge single-use
	// even against two concurrent verifications of the same code.
	if _, err := t.Store.ConsumeMfaChallenge(ctx, hashToken(challengeToken)); err != nil {
		return nil, "", ErrMFAChallengeExpired
	}
	if err := t.Store.ClearFailedLogins(ctx, user.ID); err != nil {
		return nil, "", err
	}
	return t.Sessions.Open(ctx, r, user)
}

// Disable turns 2FA off. It demands a currently-valid code (or a recovery
// code): a hijacked session must not be able to strip the account of its
// second factor with one click. The deactivation itself is audited by the
// handler (§23.4, data-dictionary §4.3).
func (t *TOTP) Disable(ctx context.Context, userID int64, code, recoveryCode string) error {
	user, err := t.Store.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if user.LockedUntil.Valid && user.LockedUntil.Time.After(time.Now()) {
		return ErrAccountLocked
	}
	if err := t.redeemCode(ctx, user, code, recoveryCode); err != nil {
		return err
	}
	if _, err := t.Store.DeleteMfaFactorForUser(ctx, userID); err != nil {
		return err
	}
	return nil
}

// RegenerateRecoveryCodes replaces the whole set — the standard answer to
// "I printed them and lost the page". Requires a valid TOTP code, for the
// same reason Disable does.
func (t *TOTP) RegenerateRecoveryCodes(ctx context.Context, userID int64, code string) ([]string, error) {
	user, err := t.Store.GetUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if user.LockedUntil.Valid && user.LockedUntil.Time.After(time.Now()) {
		return nil, ErrAccountLocked
	}
	if err := t.redeemCode(ctx, user, code, ""); err != nil {
		return nil, err
	}
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	n, err := t.Store.ReplaceMfaRecoveryCodes(ctx, store.ReplaceMfaRecoveryCodesParams{
		UserID: userID, RecoveryCodeHashes: hashes,
	})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrMFANotConfigured
	}
	return codes, nil
}

// redeemCode accepts either a TOTP code or a recovery code against the
// user's CONFIRMED factor, and burns what must be burnt: the TOTP time step
// (anti-replay) or the recovery code (single-use). Every failure records a
// failed login and comes out as ErrMFACodeInvalid.
func (t *TOTP) redeemCode(ctx context.Context, user store.User, code, recoveryCode string) error {
	factor, err := t.Store.GetMfaFactorForUser(ctx, user.ID)
	if err != nil || !factor.ConfirmedAt.Valid {
		// No confirmed factor: nothing can validate. For a login challenge
		// this is a half-deleted state, not a guessing attempt.
		return ErrMFANotConfigured
	}

	ok := false
	switch {
	case recoveryCode != "":
		// The hash lookup consumes the code in the same statement: a spent
		// code and a wrong one are the same non-event.
		n, err := t.Store.ConsumeMfaRecoveryCode(ctx, store.ConsumeMfaRecoveryCodeParams{
			ID: factor.ID, CodeHash: hashRecoveryCode(recoveryCode),
		})
		if err != nil {
			return err
		}
		ok = n > 0
	case code != "":
		secret, err := t.secretOf(factor)
		if err != nil {
			return err
		}
		if matched, valid := totp.Validate(secret, code, time.Now()); valid {
			// The step is burnt in SQL: rows=0 means this step (or a later
			// one) was already used — a replay, refused like any wrong code.
			n, err := t.Store.TouchMfaFactorUsed(ctx, store.TouchMfaFactorUsedParams{
				ID: factor.ID, UsedAt: stepTime(matched),
			})
			if err != nil {
				return err
			}
			ok = n > 0
		}
	}

	if !ok {
		if _, err := t.Store.RecordFailedLogin(ctx, store.RecordFailedLoginParams{
			ID: user.ID, MaxAttempts: MaxFailedLogins, LockMinutes: LockoutMinutes,
		}); err != nil {
			return err
		}
		return ErrMFACodeInvalid
	}
	return nil
}

// secretOf decrypts the factor's TOTP secret.
func (t *TOTP) secretOf(factor store.MfaFactor) (string, error) {
	plain, err := t.Keyring.Decrypt("mfa_factors", "secret_enc", pguuid.String(factor.Uuid), factor.SecretEnc)
	if err != nil {
		return "", fmt.Errorf("mfa factor %d: %w", factor.ID, err)
	}
	return string(plain), nil
}

// --- recovery codes -----------------------------------------------------

// newRecoveryCodes mints the set: the clear forms for the user's eyes, the
// hashes for the database. SHA-256, not Argon2: the input is 64 random bits,
// not a human password — there is nothing for a GPU to shortcut.
func newRecoveryCodes() (codes, hashes []string, err error) {
	codes = make([]string, RecoveryCodeCount)
	hashes = make([]string, RecoveryCodeCount)
	for i := range codes {
		raw := make([]byte, recoveryCodeBytes)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		h := hex.EncodeToString(raw)
		// Displayed with a hyphen for human transcription; the hyphen is
		// cosmetic and normalization strips it back out.
		codes[i] = h[:8] + "-" + h[8:]
		hashes[i] = hashRecoveryCode(codes[i])
	}
	return codes, hashes, nil
}

// hashRecoveryCode normalizes (hyphens, spaces, case are transcription
// noise, not entropy) then hashes. The stored form is comparable whatever
// way the user typed the code back.
func hashRecoveryCode(code string) string {
	normalized := strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(code))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// stepTime is the instant a TOTP step begins — the canonical value stored in
// last_used_at so "same step" compares equal across replicas.
func stepTime(step int64) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Unix(step*int64(totp.Period.Seconds()), 0).UTC(), Valid: true}
}
