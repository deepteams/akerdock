package password

import (
	"strings"
	"testing"
)

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
	for _, phc := range []string{"", "$argon2i$v=19$m=1,t=1,p=1$AA$AA", "plainhash"} {
		if ok, err := Verify("x", phc); ok || err == nil {
			t.Errorf("garbage %q must be rejected", phc)
		}
	}
}
