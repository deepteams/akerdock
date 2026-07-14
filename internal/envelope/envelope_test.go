package envelope

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func keyLineFor(version int) string {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return fmt.Sprintf("%d:%s", version, base64.StdEncoding.EncodeToString(key))
}

func TestParseValidFileActiveIsHighest(t *testing.T) {
	data := "# comment\n\n" + keyLineFor(1) + "\n" + keyLineFor(7) + "\n" + keyLineFor(3) + "\n"
	kr, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kr.ActiveVersion() != 7 {
		t.Fatalf("active version = %d, want 7", kr.ActiveVersion())
	}
}

func TestParseErrors(t *testing.T) {
	valid := keyLineFor(1)
	cases := map[string]string{
		"empty file":        "# only a comment\n",
		"duplicate version": valid + "\n" + keyLineFor(1) + "\n",
		"leading zero":      "01:" + strings.SplitN(valid, ":", 2)[1] + "\n",
		"bad base64":        "1:not-base64!!\n",
		"wrong key length":  "1:" + base64.StdEncoding.EncodeToString([]byte("too-short")) + "\n",
		"spaces":            "1 : " + strings.SplitN(valid, ":", 2)[1] + "\n",
		"version overflow":  "4294967296:" + strings.SplitN(valid, ":", 2)[1] + "\n",
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if strings.Contains(err.Error(), strings.SplitN(valid, ":", 2)[1]) {
			t.Errorf("%s: error must not reproduce key material: %v", name, err)
		}
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	kr, err := Parse([]byte(keyLineFor(1) + "\n" + keyLineFor(2) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("ssh-ed25519 private key material")
	ct, err := kr.Encrypt("private_keys", "private_key_enc", "11111111-2222-3333-4444-555555555555", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if got := ct[3]; got != 2 {
		t.Fatalf("ciphertext must embed active key version 2, got prefix byte %d", got)
	}
	pt, err := kr.Decrypt("private_keys", "private_key_enc", "11111111-2222-3333-4444-555555555555", ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatal("round-trip mismatch")
	}
}

func TestDecryptRejectsWrongRowOrColumn(t *testing.T) {
	kr, _ := Parse([]byte(keyLineFor(1) + "\n"))
	ct, _ := kr.Encrypt("private_keys", "private_key_enc", "row-a", []byte("secret"))
	if _, err := kr.Decrypt("private_keys", "private_key_enc", "row-b", ct); err == nil {
		t.Fatal("replaying a ciphertext on another row must fail (AAD)")
	}
	if _, err := kr.Decrypt("github_apps", "private_key_enc", "row-a", ct); err == nil {
		t.Fatal("replaying a ciphertext on another table must fail (AAD)")
	}
}

func TestDecryptMissingVersionIsExplicit(t *testing.T) {
	old, _ := Parse([]byte(keyLineFor(1) + "\n" + keyLineFor(2) + "\n"))
	ct, _ := old.Encrypt("servers", "ca_key_enc", "row", []byte("x"))
	current, _ := Parse([]byte(keyLineFor(3) + "\n"))
	_, err := current.Decrypt("servers", "ca_key_enc", "row", ct)
	if err == nil || !strings.Contains(err.Error(), "key version 2") {
		t.Fatalf("expected explicit missing-version error, got %v", err)
	}
}

func TestSelfTest(t *testing.T) {
	kr, _ := Parse([]byte(keyLineFor(5) + "\n"))
	if err := kr.SelfTest(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, []byte(keyLineFor(1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, warnings, err := LoadFile(path); err != nil || len(warnings) != 0 {
		t.Fatalf("0600 file must load cleanly, err=%v warnings=%v", err, warnings)
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, warnings, err := LoadFile(path); err != nil || len(warnings) != 1 {
		t.Fatalf("0640 must load with one warning, err=%v warnings=%v", err, warnings)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "other") {
		t.Fatalf("world-readable file must be fatal, got %v", err)
	}
}
