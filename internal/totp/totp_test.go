package totp

import (
	"strings"
	"testing"
	"time"
)

// rfcSecret is the shared secret of the RFC 4226 (appendix D) and RFC 6238
// (appendix B) test vectors: the ASCII bytes "12345678901234567890".
var rfcSecret = []byte("12345678901234567890")

// TestHOTPVectors pins the implementation to RFC 4226 appendix D. If any of
// these fail, every authenticator app on earth disagrees with us.
func TestHOTPVectors(t *testing.T) {
	want := []string{
		"755224", "287082", "359152", "969429", "338314",
		"254676", "287922", "162583", "399871", "520489",
	}
	for counter, expected := range want {
		if got := hotp(rfcSecret, uint64(counter)); got != expected {
			t.Errorf("hotp(counter=%d) = %s, want %s", counter, got, expected)
		}
	}
}

// TestTOTPVectors pins the time-based layer to RFC 6238 appendix B (SHA-1
// rows, truncated to 6 digits — the appendix prints 8).
func TestTOTPVectors(t *testing.T) {
	secret := codec.EncodeToString(rfcSecret)
	cases := []struct {
		unix int64
		code string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, c := range cases {
		if _, ok := Validate(secret, c.code, time.Unix(c.unix, 0)); !ok {
			t.Errorf("Validate at t=%d rejected the RFC 6238 code %s", c.unix, c.code)
		}
	}
}

// A code from the previous or next step must pass (clock drift, human delay),
// two steps away must not — each extra accepted step is a free multiplier on
// an attacker's guessing budget.
func TestValidateSkewWindow(t *testing.T) {
	secret := codec.EncodeToString(rfcSecret)
	now := time.Unix(1111111111, 0)

	for _, offset := range []int64{-1, 0, 1} {
		code := hotp(rfcSecret, uint64(Step(now)+offset))
		matched, ok := Validate(secret, code, now)
		if !ok {
			t.Errorf("code for step offset %+d was rejected: the ±%d window must absorb it", offset, Skew)
		}
		if ok && matched != Step(now)+offset {
			t.Errorf("code for offset %+d reported step %d, want %d — anti-replay would pin the wrong step",
				offset, matched, Step(now)+offset)
		}
	}
	for _, offset := range []int64{-2, 2} {
		code := hotp(rfcSecret, uint64(Step(now)+offset))
		if _, ok := Validate(secret, code, now); ok {
			t.Errorf("code for step offset %+d was accepted: outside the ±%d window", offset, Skew)
		}
	}
}

func TestValidateRejectsMalformedInput(t *testing.T) {
	secret := codec.EncodeToString(rfcSecret)
	now := time.Unix(1111111111, 0)
	good := hotp(rfcSecret, uint64(Step(now)))

	for name, code := range map[string]string{
		"empty":       "",
		"too short":   good[:5],
		"too long":    good + "0",
		"non-numeric": "abcdef",
	} {
		if _, ok := Validate(secret, code, now); ok {
			t.Errorf("%s code was accepted", name)
		}
	}
	if _, ok := Validate("not!base32", good, now); ok {
		t.Error("an undecodable secret validated a code")
	}
}

// Validate must tolerate the forms humans and apps produce: surrounding
// whitespace and a lower-case secret (manual entry).
func TestValidateNormalizesInput(t *testing.T) {
	secret := codec.EncodeToString(rfcSecret)
	now := time.Unix(1111111111, 0)
	good := hotp(rfcSecret, uint64(Step(now)))

	if _, ok := Validate(strings.ToLower(secret), " "+good+" ", now); !ok {
		t.Error("lower-case secret + padded code was rejected")
	}
}

func TestGenerateSecret(t *testing.T) {
	a, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two generated secrets are identical")
	}
	raw, err := codec.DecodeString(a)
	if err != nil {
		t.Fatalf("generated secret is not valid base32: %v", err)
	}
	if len(raw) != SecretSize {
		t.Fatalf("secret is %d bytes, want %d", len(raw), SecretSize)
	}
}

// The provisioning URI is consumed by authenticator apps, not by our code:
// what matters is that the label, issuer and secret survive URL escaping.
func TestURI(t *testing.T) {
	uri := URI("AkerDock", "jean-luc@example.com", "SECRETBASE32")
	for _, want := range []string{
		"otpauth://totp/AkerDock:jean-luc@example.com?",
		"secret=SECRETBASE32",
		"issuer=AkerDock",
		"digits=6",
		"period=30",
		"algorithm=SHA1",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("URI %q lacks %q", uri, want)
		}
	}
}
