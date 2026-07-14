// Package envelope implements the multi-version master key file
// (instance-config §3) and the envelope encryption format of every *_enc
// column (data-dictionary §2.7, ADR-003).
package envelope

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
)

const keySize = 32 // AES-256-GCM

// Keyring holds the master key versions. The active version — the one that
// encrypts every new write — is the highest version present (§3.1).
type Keyring struct {
	keys   map[uint32][]byte
	active uint32
}

// keyLine is "<version>:<standard base64, 32 decoded bytes>", no leading
// zeros, no spaces around the separator (§3.1).
var keyLine = regexp.MustCompile(`^([1-9][0-9]*):([A-Za-z0-9+/]+={0,2})$`)

// Parse reads a master key file. Any malformed line, duplicate version,
// invalid base64 or wrong key length makes the whole file invalid; errors
// name the offending line number but never reproduce its content (§3.1).
func Parse(data []byte) (*Keyring, error) {
	kr := &Keyring{keys: map[uint32][]byte{}}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		m := keyLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("master key file: line %d: expected \"<version>:<base64 key>\" (version without leading zeros, standard base64 with padding)", lineNo)
		}
		version64, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("master key file: line %d: version out of range 1–4294967295", lineNo)
		}
		version := uint32(version64)
		if _, dup := kr.keys[version]; dup {
			return nil, fmt.Errorf("master key file: line %d: duplicate version %d", lineNo, version)
		}
		key, err := base64.StdEncoding.DecodeString(m[2])
		if err != nil {
			return nil, fmt.Errorf("master key file: line %d: invalid base64", lineNo)
		}
		if len(key) != keySize {
			return nil, fmt.Errorf("master key file: line %d: key must decode to exactly %d bytes, got %d", lineNo, keySize, len(key))
		}
		kr.keys[version] = key
		if version > kr.active {
			kr.active = version
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("master key file: %w", err)
	}
	if len(kr.keys) == 0 {
		return nil, fmt.Errorf("master key file: no key found (at least one \"<version>:<base64 key>\" line is required)")
	}
	return kr, nil
}

// LoadFile parses the key file at path after checking its permissions:
// readable or writable by "other" is a fatal error; any other deviation from
// 0600 is reported as a warning (§3.3).
func LoadFile(path string) (*Keyring, []string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("master key file %q: %w (expected a 0600 file, see runbook install.md step 2)", path, err)
	}
	var warnings []string
	perm := info.Mode().Perm()
	if perm&0o006 != 0 {
		return nil, nil, fmt.Errorf("master key file %q: permissions %04o allow access by \"other\" (expected 0600)", path, perm)
	}
	if perm != 0o600 {
		warnings = append(warnings, fmt.Sprintf("master key file %q: permissions %04o differ from the expected 0600", path, perm))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if _, ok := err.(*fs.PathError); ok {
			return nil, nil, fmt.Errorf("master key file %q: unreadable: %w", path, err)
		}
		return nil, nil, err
	}
	kr, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	return kr, warnings, nil
}

// ActiveVersion returns the version that encrypts new writes.
func (k *Keyring) ActiveVersion() uint32 { return k.active }

// SelfTest performs an encrypt/decrypt round-trip with the active version,
// as required by the startup sequence (instance-config §6.1 step 4).
func (k *Keyring) SelfTest() error {
	plaintext := []byte("akerdock master key self-test")
	ct, err := k.Encrypt("selftest", "selftest", "00000000-0000-0000-0000-000000000000", plaintext)
	if err != nil {
		return fmt.Errorf("master key self-test (version %d): %w", k.active, err)
	}
	pt, err := k.Decrypt("selftest", "selftest", "00000000-0000-0000-0000-000000000000", ct)
	if err != nil || !bytes.Equal(pt, plaintext) {
		return fmt.Errorf("master key self-test (version %d): round-trip failed", k.active)
	}
	return nil
}
