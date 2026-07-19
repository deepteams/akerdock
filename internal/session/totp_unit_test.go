package session

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
	itotp "github.com/deepteams/akerdock/internal/totp"
)

func sessionKeyring(t *testing.T) *envelope.Keyring {
	t.Helper()
	line := "1:" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

func totpCode(secret string, at time.Time) string {
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(itotp.Step(at)))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1_000_000)
}

func encryptedFactor(t *testing.T, keyring *envelope.Keyring, secret string, confirmed bool) store.MfaFactor {
	t.Helper()
	uuid := pguuid.MustParse("11111111-2222-4333-8444-555555555555")
	ciphertext, err := keyring.Encrypt("mfa_factors", "secret_enc", pguuid.String(uuid), []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	factor := store.MfaFactor{ID: 4, Uuid: uuid, UserID: 7, SecretEnc: ciphertext}
	if confirmed {
		factor.ConfirmedAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}
	}
	return factor
}

func TestTOTPSetup(t *testing.T) {
	keyring := sessionKeyring(t)
	user := store.User{ID: 7, Email: "user@example.test"}
	database := &fakeSessionStore{user: user}
	factor := &TOTP{Store: database, Keyring: keyring}
	secret, uri, err := factor.Setup(context.Background(), user.ID)
	if err != nil || secret == "" || !strings.Contains(uri, "otpauth://totp/") ||
		!strings.Contains(uri, "user@example.test") || len(database.factorUpserts) != 1 {
		t.Fatalf("Setup = %q, %q, %v, upserts=%#v", secret, uri, err, database.factorUpserts)
	}
	arg := database.factorUpserts[0]
	plain, err := keyring.Decrypt("mfa_factors", "secret_enc", pguuid.String(arg.Uuid), arg.SecretEnc)
	if err != nil || string(plain) != secret {
		t.Fatalf("stored secret = %q, %v", plain, err)
	}

	for _, tc := range []struct {
		name string
		db   *fakeSessionStore
		want error
	}{
		{"user", &fakeSessionStore{errs: map[string]error{"user": errors.New("x")}}, nil},
		{"already enabled", &fakeSessionStore{user: user, errs: map[string]error{"upsertFactor": pgx.ErrNoRows}}, ErrMFAAlreadyEnabled},
		{"insert", &fakeSessionStore{user: user, errs: map[string]error{"upsertFactor": errors.New("x")}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := (&TOTP{Store: tc.db, Keyring: keyring}).Setup(context.Background(), user.ID)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("Setup error = %v", err)
			}
		})
	}
}

func TestTOTPConfirm(t *testing.T) {
	keyring := sessionKeyring(t)
	secret := "JBSWY3DPEHPK3PXP"
	factor := encryptedFactor(t, keyring, secret, false)
	code := totpCode(secret, time.Now())

	t.Run("success", func(t *testing.T) {
		database := &fakeSessionStore{factor: factor}
		codes, err := (&TOTP{Store: database, Keyring: keyring}).Confirm(context.Background(), factor.UserID, code)
		if err != nil || len(codes) != RecoveryCodeCount || len(database.factorConfirms) != 1 ||
			len(database.factorConfirms[0].RecoveryCodeHashes) != RecoveryCodeCount ||
			!database.factorConfirms[0].LastUsedAt.Valid {
			t.Fatalf("Confirm codes=%d err=%v args=%#v", len(codes), err, database.factorConfirms)
		}
	})
	t.Run("lookup and state", func(t *testing.T) {
		cases := []struct {
			db   *fakeSessionStore
			want error
		}{
			{&fakeSessionStore{errs: map[string]error{"factor": pgx.ErrNoRows}}, ErrMFANotConfigured},
			{&fakeSessionStore{errs: map[string]error{"factor": errors.New("x")}}, nil},
			{&fakeSessionStore{factor: encryptedFactor(t, keyring, secret, true)}, ErrMFAAlreadyEnabled},
		}
		for _, tc := range cases {
			_, err := (&TOTP{Store: tc.db, Keyring: keyring}).Confirm(context.Background(), 7, code)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Errorf("Confirm = %v", err)
			}
		}
	})
	t.Run("invalid secret and code", func(t *testing.T) {
		broken := factor
		broken.SecretEnc = []byte("bad")
		for _, tc := range []struct {
			factor store.MfaFactor
			code   string
		}{
			{broken, code},
			{factor, "000000"},
		} {
			_, err := (&TOTP{Store: &fakeSessionStore{factor: tc.factor}, Keyring: keyring}).
				Confirm(context.Background(), 7, tc.code)
			if err == nil {
				t.Fatal("invalid confirmation accepted")
			}
		}
	})
	t.Run("confirm write", func(t *testing.T) {
		for _, writeErr := range []error{pgx.ErrNoRows, errors.New("x")} {
			database := &fakeSessionStore{factor: factor, errs: map[string]error{"confirmFactor": writeErr}}
			_, err := (&TOTP{Store: database, Keyring: keyring}).Confirm(context.Background(), 7, code)
			if writeErr == pgx.ErrNoRows && !errors.Is(err, ErrMFAAlreadyEnabled) {
				t.Fatalf("race error = %v", err)
			}
			if writeErr != pgx.ErrNoRows && err == nil {
				t.Fatal("write error hidden")
			}
		}
	})
	t.Run("recovery entropy", func(t *testing.T) {
		old := randomReader
		randomReader = failingReader{err: errors.New("entropy")}
		defer func() { randomReader = old }()
		if _, err := (&TOTP{Store: &fakeSessionStore{factor: factor}, Keyring: keyring}).
			Confirm(context.Background(), 7, code); err == nil {
			t.Fatal("recovery entropy failure hidden")
		}
	})
}

func TestTOTPEnabled(t *testing.T) {
	confirmed := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	for _, tc := range []struct {
		db      *fakeSessionStore
		enabled bool
		err     bool
	}{
		{&fakeSessionStore{errs: map[string]error{"factor": pgx.ErrNoRows}}, false, false},
		{&fakeSessionStore{errs: map[string]error{"factor": errors.New("x")}}, false, true},
		{&fakeSessionStore{factor: store.MfaFactor{}}, false, false},
		{&fakeSessionStore{factor: store.MfaFactor{ConfirmedAt: confirmed, RecoveryCodeHashes: []string{"a", "b"}}}, true, false},
	} {
		enabled, at, count, err := (&TOTP{Store: tc.db}).Enabled(context.Background(), 1)
		if enabled != tc.enabled || (err != nil) != tc.err {
			t.Errorf("Enabled = %v, %v, %d, %v", enabled, at, count, err)
		}
		if enabled && (at.IsZero() || count != 2) {
			t.Errorf("enabled metadata = %v, %d", at, count)
		}
	}
}

func TestCreateMFAChallenge(t *testing.T) {
	database := &fakeSessionStore{}
	token, err := (&Manager{Store: database}).CreateChallenge(context.Background(), 7)
	if err != nil || token == "" || len(database.challengeCreates) != 1 ||
		database.challengeCreates[0].TokenHash == token ||
		!database.challengeCreates[0].ExpiresAt.Valid {
		t.Fatalf("CreateChallenge = %q, %v, args=%#v", token, err, database.challengeCreates)
	}
	database.errs = map[string]error{"createChallenge": errors.New("x")}
	if _, err := (&Manager{Store: database}).CreateChallenge(context.Background(), 7); err == nil {
		t.Fatal("challenge insert error hidden")
	}
}

func TestRedeemRecoveryAndTOTP(t *testing.T) {
	keyring := sessionKeyring(t)
	secret := "JBSWY3DPEHPK3PXP"
	user := store.User{ID: 7}
	factor := encryptedFactor(t, keyring, secret, true)
	service := func(db *fakeSessionStore) *TOTP { return &TOTP{Store: db, Keyring: keyring} }

	t.Run("recovery succeeds", func(t *testing.T) {
		database := &fakeSessionStore{factor: factor, ints: map[string]int64{"consumeRecovery": 1}}
		if err := service(database).redeemCode(context.Background(), user, "", "ABCD-EF01"); err != nil {
			t.Fatal(err)
		}
		if len(database.recoveryConsumes) != 1 ||
			database.recoveryConsumes[0].CodeHash != hashRecoveryCode("ABCD-EF01") {
			t.Fatalf("consume = %#v", database.recoveryConsumes)
		}
	})
	t.Run("totp succeeds and burns step", func(t *testing.T) {
		database := &fakeSessionStore{factor: factor, ints: map[string]int64{"touchFactor": 1}}
		if err := service(database).redeemCode(
			context.Background(), user, totpCode(secret, time.Now()), "",
		); err != nil {
			t.Fatal(err)
		}
		if len(database.factorTouches) != 1 || !database.factorTouches[0].UsedAt.Valid {
			t.Fatalf("touch = %#v", database.factorTouches)
		}
	})
	t.Run("wrong and replay record a failure", func(t *testing.T) {
		for _, code := range []string{"", "000000"} {
			database := &fakeSessionStore{factor: factor}
			err := service(database).redeemCode(context.Background(), user, code, "")
			if !errors.Is(err, ErrMFACodeInvalid) || len(database.failedLogins) != 1 {
				t.Fatalf("redeem = %v, failures=%#v", err, database.failedLogins)
			}
		}
	})
	t.Run("unconfigured", func(t *testing.T) {
		for _, database := range []*fakeSessionStore{
			{errs: map[string]error{"factor": errors.New("missing")}},
			{factor: store.MfaFactor{}},
		} {
			if err := service(database).redeemCode(context.Background(), user, "", "x"); !errors.Is(err, ErrMFANotConfigured) {
				t.Fatalf("redeem = %v", err)
			}
		}
	})
	t.Run("store errors", func(t *testing.T) {
		cases := []struct {
			key      string
			code     string
			recovery string
		}{
			{"consumeRecovery", "", "x"},
			{"touchFactor", totpCode(secret, time.Now()), ""},
			{"recordFailed", "000000", ""},
		}
		for _, tc := range cases {
			database := &fakeSessionStore{
				factor: factor, errs: map[string]error{tc.key: errors.New(tc.key)},
			}
			if err := service(database).redeemCode(context.Background(), user, tc.code, tc.recovery); err == nil {
				t.Errorf("%s error hidden", tc.key)
			}
		}
	})
}

func TestVerifyTOTPLogin(t *testing.T) {
	keyring := sessionKeyring(t)
	secret := "JBSWY3DPEHPK3PXP"
	user := store.User{ID: 7, Email: "user@example.test"}
	base := func() *fakeSessionStore {
		return &fakeSessionStore{
			user: user, challenge: store.MfaChallenge{UserID: user.ID},
			factor:     encryptedFactor(t, keyring, secret, true),
			ints:       map[string]int64{"consumeRecovery": 1},
			membership: store.GetTeamMembershipForUserRow{TeamID: 2},
		}
	}
	service := func(db *fakeSessionStore) *TOTP {
		return &TOTP{Store: db, Sessions: &Manager{Store: db}, Keyring: keyring}
	}
	if _, _, err := service(base()).VerifyLogin(
		context.Background(), loginRequest(), "", "", "",
	); !errors.Is(err, ErrMFAChallengeExpired) {
		t.Fatalf("empty challenge = %v", err)
	}
	for _, key := range []string{"challenge", "user", "consumeChallenge"} {
		database := base()
		database.errs = map[string]error{key: errors.New(key)}
		_, _, err := service(database).VerifyLogin(
			context.Background(), loginRequest(), "challenge", "", "recovery",
		)
		if !errors.Is(err, ErrMFAChallengeExpired) {
			t.Errorf("%s = %v", key, err)
		}
	}
	database := base()
	locked := user
	locked.LockedUntil = pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	database.user = locked
	if _, _, err := service(database).VerifyLogin(
		context.Background(), loginRequest(), "challenge", "", "recovery",
	); !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("locked = %v", err)
	}
	database = base()
	session, token, err := service(database).VerifyLogin(
		context.Background(), loginRequest(), "challenge", "", "recovery",
	)
	if err != nil || session == nil || token == "" || len(database.clearedLogins) != 1 {
		t.Fatalf("VerifyLogin = %#v, %q, %v", session, token, err)
	}
	database = base()
	database.errs = map[string]error{"clearFailed": errors.New("x")}
	if _, _, err := service(database).VerifyLogin(
		context.Background(), loginRequest(), "challenge", "", "recovery",
	); err == nil {
		t.Fatal("clear failed error hidden")
	}
}

func TestDisableAndRegenerateRecoveryCodes(t *testing.T) {
	keyring := sessionKeyring(t)
	secret := "JBSWY3DPEHPK3PXP"
	code := totpCode(secret, time.Now())
	user := store.User{ID: 7}
	base := func() *fakeSessionStore {
		return &fakeSessionStore{
			user: user, factor: encryptedFactor(t, keyring, secret, true),
			ints: map[string]int64{"touchFactor": 1, "deleteFactor": 1, "replaceRecovery": 1},
		}
	}
	service := func(db *fakeSessionStore) *TOTP { return &TOTP{Store: db, Keyring: keyring} }

	database := base()
	if err := service(database).Disable(context.Background(), user.ID, code, ""); err != nil {
		t.Fatal(err)
	}
	database = base()
	codes, err := service(database).RegenerateRecoveryCodes(context.Background(), user.ID, code)
	if err != nil || len(codes) != RecoveryCodeCount || len(database.replacedRecoveries) != 1 {
		t.Fatalf("Regenerate = %d, %v", len(codes), err)
	}
	database = base()
	database.ints["replaceRecovery"] = 0
	if _, err := service(database).RegenerateRecoveryCodes(
		context.Background(), user.ID, code,
	); !errors.Is(err, ErrMFANotConfigured) {
		t.Fatalf("missing factor = %v", err)
	}

	for _, key := range []string{"user", "deleteFactor", "replaceRecovery"} {
		database := base()
		database.errs = map[string]error{key: errors.New(key)}
		if key == "replaceRecovery" {
			if _, err := service(database).RegenerateRecoveryCodes(context.Background(), user.ID, code); err == nil {
				t.Errorf("%s error hidden", key)
			}
		} else if err := service(database).Disable(context.Background(), user.ID, code, ""); err == nil {
			t.Errorf("%s error hidden", key)
		}
	}
	for _, invoke := range []func(*TOTP) error{
		func(totp *TOTP) error { return totp.Disable(context.Background(), user.ID, code, "") },
		func(totp *TOTP) error {
			_, err := totp.RegenerateRecoveryCodes(context.Background(), user.ID, code)
			return err
		},
	} {
		database := base()
		database.user.LockedUntil = pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
		if err := invoke(service(database)); !errors.Is(err, ErrAccountLocked) {
			t.Fatalf("locked operation = %v", err)
		}
	}
}

func TestTOTPHelpersAndSentinels(t *testing.T) {
	if !isNoRows(pgx.ErrNoRows) || isNoRows(errors.New("x")) {
		t.Fatal("isNoRows failed")
	}
	if ChallengeLifetime <= 0 || RecoveryCodeCount != 10 || Issuer != "AkerDock" {
		t.Fatal("MFA safety constants changed")
	}
	_ = httptest.NewRequest("POST", "/", nil)
}
