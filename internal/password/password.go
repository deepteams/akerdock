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

var randomReader io.Reader = rand.Reader

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
