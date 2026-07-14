package session

import (
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/password"
)

// The constant-time defence only works if dummyHash is a REAL Argon2id hash: a
// malformed one makes Verify fail instantly, and an unknown email would then
// answer measurably faster than a wrong password — which is exactly the account
// enumeration oracle the dummy exists to prevent.
func TestDummyHashIsVerifiable(t *testing.T) {
	ok, err := password.Verify("anything", dummyHash)
	if err != nil {
		t.Fatalf("dummyHash is not a valid Argon2id hash: %v — an unknown email would answer faster than a wrong password", err)
	}
	if ok {
		t.Fatal("dummyHash must not verify against a guessable password")
	}
}

// ...and it must cost a real Argon2 computation. If verifying the dummy is
// orders of magnitude faster than verifying a real hash, the timing difference
// leaks which emails exist — the very thing the dummy is for.
func TestDummyHashCostsRealWork(t *testing.T) {
	genuineHash, err := password.Hash("a-real-password")
	if err != nil {
		t.Fatal(err)
	}

	measure := func(hash string) time.Duration {
		start := time.Now()
		for range 3 {
			_, _ = password.Verify("guess", hash)
		}
		return time.Since(start) / 3
	}

	dummy := measure(dummyHash)
	genuine := measure(genuineHash)

	// Generous bound: this catches "the dummy is malformed and returns in
	// nanoseconds", not micro-variations.
	if dummy < genuine/4 {
		t.Errorf("verifying dummyHash took %s vs %s for a real hash — an unknown email answers "+
			"measurably faster, which enumerates accounts", dummy, genuine)
	}
}
