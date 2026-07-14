package sshkey

import (
	"strings"
	"testing"
)

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
