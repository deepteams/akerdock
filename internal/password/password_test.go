package password

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	// Accepted: long and not obviously weak.
	for _, ok := range []string{"correct horse battery", "a-perfectly-fine-passphrase", "Tr0ub4dour&3xtra"} {
		if err := Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
	// Rejected: too short, common, or a single repeated rune.
	for _, bad := range []string{"", "short", "elevenchars", "password1234", "PASSWORD1234", "aaaaaaaaaaaa"} {
		var weak WeakPasswordError
		if err := Validate(bad); err == nil {
			t.Errorf("Validate(%q) = nil, want a policy error", bad)
		} else if !errors.As(err, &weak) {
			t.Errorf("Validate(%q) error = %T, want WeakPasswordError", bad, err)
		}
	}
	// Exactly the minimum length is accepted (boundary).
	if err := Validate(strings.Repeat("ab", MinLength/2)); err != nil {
		t.Errorf("a %d-char varied password should pass: %v", MinLength, err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestHashAndVerify(t *testing.T) {
	phc, err := Hash("a-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$") {
		t.Fatalf("unexpected PHC format: %s", phc)
	}
	ok, err := Verify("a-long-enough-password", phc)
	if err != nil || !ok {
		t.Fatalf("valid password must verify, ok=%v err=%v", ok, err)
	}
	ok, err = Verify("wrong-password!!", phc)
	if err != nil || ok {
		t.Fatalf("wrong password must not verify, ok=%v err=%v", ok, err)
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, _ := Hash("same-password-here")
	b, _ := Hash("same-password-here")
	if a == b {
		t.Fatal("two hashes of the same password must differ (random salt)")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	for _, phc := range []string{
		"",
		"$argon2i$v=19$m=1,t=1,p=1$AA$AA",
		"plainhash",
		"$argon2id$v=nope$m=65536,t=3,p=2$AAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$broken$AAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=4294967295,t=3,p=2$AAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=65536,t=3,p=2$%%%$AAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAA$%%%",
		"$argon2id$v=19$m=65536,t=3,p=2$AA$AAAAAAAAAAAAAAAAAAAAAA",
	} {
		if ok, err := Verify("x", phc); ok || err == nil {
			t.Errorf("garbage %q must be rejected", phc)
		}
	}
}

func TestHashReportsEntropyFailure(t *testing.T) {
	old := randomReader
	randomReader = failingReader{}
	t.Cleanup(func() { randomReader = old })

	if _, err := Hash("a-long-enough-password"); err == nil || !strings.Contains(err.Error(), "salt generation") {
		t.Fatalf("Hash should report the entropy failure, got %v", err)
	}
}
