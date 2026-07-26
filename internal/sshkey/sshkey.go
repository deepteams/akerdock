// Package sshkey parses and generates SSH key material (PRD §3.1: keys
// without passphrase, public part and SHA256 fingerprint derived from the
// private material).
package sshkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

var randomReader = rand.Reader

// Material is the derived view of a private key.
type Material struct {
	PrivatePEM  string // as provided or generated, PEM/OpenSSH
	PublicKey   string // authorized_keys line
	Fingerprint string // "SHA256:..." of the public key
}

// Parse validates a PEM/OpenSSH private key and derives its public part.
// Passphrase-protected keys are rejected explicitly (§3.1).
func Parse(privatePEM string) (*Material, error) {
	signer, err := ssh.ParsePrivateKey([]byte(privatePEM))
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, fmt.Errorf("sshkey: passphrase-protected keys are not supported (PRD §3.1)")
		}
		return nil, fmt.Errorf("sshkey: invalid private key: %w", err)
	}
	return fromSigner(privatePEM, signer), nil
}

// GenerateEd25519 creates a fresh ed25519 key without passphrase, used for
// the instance key (instance-config §6.2).
func GenerateEd25519(comment string) (*Material, error) {
	_, priv, err := ed25519.GenerateKey(randomReader)
	if err != nil {
		return nil, fmt.Errorf("sshkey: generate: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, fmt.Errorf("sshkey: marshal: %w", err)
	}
	privatePEM := string(pem.EncodeToMemory(block))
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("sshkey: signer: %w", err)
	}
	return fromSigner(privatePEM, signer), nil
}

func fromSigner(privatePEM string, signer ssh.Signer) *Material {
	pub := signer.PublicKey()
	return &Material{
		PrivatePEM:  privatePEM,
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
		Fingerprint: ssh.FingerprintSHA256(pub),
	}
}
