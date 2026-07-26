// Package password hashes user passwords with Argon2id (PRD §23.2) using
// the PHC string format, so parameters can evolve without a schema change.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

// OWASP-recommended Argon2id parameters (64 MiB, 3 iterations).
const (
	memoryKiB   = 64 * 1024
	iterations  = 3
	parallelism = 2
	saltLen     = 16
	keyLen      = 32
	// Older hashes (including the constant-time dummy hash) used four lanes.
	// Accept a small bounded range for backwards compatibility.
	maxVerifyParallelism = 16
)

var randomReader = rand.Reader

// MinLength is the minimum password length (PRD §10.2). Length is the single
// strongest lever against guessing; complexity rules mostly push users toward
// predictable substitutions, so the policy is "long, and not a known-weak one".
const MinLength = 12

// commonPasswords are weak choices refused regardless of length — the handful
// that survive a 12-char minimum (repetition, keyboard walks, obvious phrases).
// Matched case-insensitively. Deliberately small: this is a floor, not a
// breach-corpus check (that is a separate, network-dependent enhancement).
var commonPasswords = map[string]bool{
	"password":         true,
	"passwordpassword": true,
	"password1234":     true,
	"password12345":    true,
	"passw0rd12345":    true,
	"123456789012":     true,
	"1234567890123":    true,
	"12345678901234":   true,
	"qwertyuiopas":     true,
	"qwertyuiop1234":   true,
	"administrator":    true,
	"administrator1":   true,
	"letmeinletmein":   true,
	"welcome1234567":   true,
	"changemechangeme": true,
	"aaaaaaaaaaaa":     true,
}

// WeakPasswordError is returned by Validate for a password that does not meet the
// policy. Its message is safe to show the user setting the password.
type WeakPasswordError struct{ Reason string }

func (e WeakPasswordError) Error() string { return e.Reason }

// Validate enforces the password policy (PRD §10.2, ISO A.8.5): a minimum length
// and a refusal of known-weak passwords. It is the single source of truth for
// "is this password acceptable", called wherever a password is set.
func Validate(password string) error {
	if len(password) < MinLength {
		return WeakPasswordError{fmt.Sprintf("must be at least %d characters", MinLength)}
	}
	normalized := strings.ToLower(strings.TrimSpace(password))
	if commonPasswords[normalized] {
		return WeakPasswordError{"this password is too common — choose a less predictable one"}
	}
	if isSingleRepeatedRune(password) {
		return WeakPasswordError{"this password is a single repeated character"}
	}
	return nil
}

// isSingleRepeatedRune reports whether every rune of s is identical (e.g.
// "aaaaaaaaaaaa"): long, but trivially guessed.
func isSingleRepeatedRune(s string) bool {
	runes := []rune(s)
	if len(runes) == 0 {
		return false
	}
	for _, r := range runes[1:] {
		if r != runes[0] {
			return false
		}
	}
	return true
}

// Hash derives an Argon2id hash and encodes it as a PHC string:
// $argon2id$v=19$m=...,t=...,p=...$<salt b64>$<hash b64>
func Hash(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(randomReader, salt); err != nil {
		return "", fmt.Errorf("password: salt generation: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, iterations, memoryKiB, parallelism, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memoryKiB, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether password matches the PHC-encoded hash, using the
// parameters stored in the hash itself.
func Verify(password, phc string) (bool, error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, fmt.Errorf("password: unsupported hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, fmt.Errorf("password: unsupported argon2 version")
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, fmt.Errorf("password: malformed parameters")
	}
	// The PHC string is stored data, not trusted input. Bound its cost before
	// handing it to Argon2: otherwise one corrupted row could request
	// gigabytes of memory (or panic with zero lanes) during login.
	if m < 8 || m > memoryKiB || t < 1 || t > iterations || p < 1 || p > maxVerifyParallelism {
		return false, fmt.Errorf("password: unsafe argon2 parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("password: malformed salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("password: malformed hash")
	}
	if len(salt) < 8 || len(want) < 16 || len(want) > 64 {
		return false, fmt.Errorf("password: unsafe salt or hash length")
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
