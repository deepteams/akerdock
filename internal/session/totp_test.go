package session

import (
	"strings"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/totp"
)

func TestNewRecoveryCodesShape(t *testing.T) {
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != RecoveryCodeCount || len(hashes) != RecoveryCodeCount {
		t.Fatalf("got %d codes / %d hashes, want %d", len(codes), len(hashes), RecoveryCodeCount)
	}

	seen := map[string]bool{}
	for i, code := range codes {
		// 8 hex chars, hyphen, 8 hex chars: transcribable, 64 bits of entropy.
		parts := strings.Split(code, "-")
		if len(parts) != 2 || len(parts[0]) != 8 || len(parts[1]) != 8 {
			t.Errorf("code %q is not of the form xxxxxxxx-xxxxxxxx", code)
		}
		if seen[code] {
			t.Errorf("duplicate recovery code %q", code)
		}
		seen[code] = true
		if hashes[i] != hashRecoveryCode(code) {
			t.Errorf("hash %d does not match its code", i)
		}
	}
}

// A recovery code is transcribed by a human off a printout: hyphens, spaces
// and case are noise, and the stored hash must match whatever form they typed.
func TestHashRecoveryCodeNormalizes(t *testing.T) {
	base := hashRecoveryCode("deadbeef-cafe0123")
	for _, form := range []string{
		"deadbeefcafe0123",
		"DEADBEEF-CAFE0123",
		"dead beef cafe 0123",
		" deadbeef-cafe0123 ",
	} {
		if hashRecoveryCode(form) != base {
			t.Errorf("form %q hashes differently from the canonical form", form)
		}
	}
	if hashRecoveryCode("deadbeef-cafe0124") == base {
		t.Error("a different code hashed to the same value")
	}
}

// last_used_at stores the START of the accepted step: canonical across
// replicas, so "same step" compares equal wherever the code was verified.
func TestStepTimeIsCanonical(t *testing.T) {
	at := time.Date(2026, 7, 14, 10, 30, 29, 0, time.UTC)
	step := totp.Step(at)
	got := stepTime(step)
	if !got.Valid {
		t.Fatal("stepTime returned an invalid timestamp")
	}
	if got.Time.Unix()%int64(totp.Period.Seconds()) != 0 {
		t.Errorf("stepTime %v is not aligned on a period boundary", got.Time)
	}
	if totp.Step(got.Time) != step {
		t.Errorf("stepTime round-trips to step %d, want %d", totp.Step(got.Time), step)
	}
	// The next step must be strictly later — that inequality IS the
	// anti-replay comparison TouchMfaFactorUsed runs in SQL.
	if !stepTime(step + 1).Time.After(got.Time) {
		t.Error("consecutive steps are not strictly ordered")
	}
}
