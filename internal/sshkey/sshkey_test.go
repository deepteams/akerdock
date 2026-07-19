package sshkey

import (
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestGenerateAndParseRoundTrip(t *testing.T) {
	generated, err := GenerateEd25519("test-comment")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(generated.PublicKey, "ssh-ed25519 ") {
		t.Fatalf("unexpected public key: %s", generated.PublicKey)
	}
	if !strings.HasPrefix(generated.Fingerprint, "SHA256:") {
		t.Fatalf("unexpected fingerprint: %s", generated.Fingerprint)
	}

	parsed, err := Parse(generated.PrivatePEM)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Fingerprint != generated.Fingerprint || parsed.PublicKey != generated.PublicKey {
		t.Fatal("parse must derive the same public material")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse("not a key"); err == nil {
		t.Fatal("garbage must be rejected")
	}
}

func TestParseRejectsPassphraseProtectedKey(t *testing.T) {
	generated, err := GenerateEd25519("encrypted")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := ssh.ParseRawPrivateKey([]byte(generated.PrivatePEM))
	encrypted, err := ssh.MarshalPrivateKeyWithPassphrase(block, "encrypted", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(string(pem.EncodeToMemory(encrypted))); err == nil || !strings.Contains(err.Error(), "passphrase-protected") {
		t.Fatalf("protected key should be rejected explicitly, got %v", err)
	}
}

func TestGenerateReportsEntropyFailure(t *testing.T) {
	old := randomReader
	randomReader = errorReader{}
	t.Cleanup(func() { randomReader = old })
	if _, err := GenerateEd25519("test"); err == nil || !strings.Contains(err.Error(), "generate") {
		t.Fatalf("GenerateEd25519 should report the entropy failure, got %v", err)
	}
}
