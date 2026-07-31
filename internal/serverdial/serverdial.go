// Package serverdial opens the SSH connection to a managed server: the
// provisioned private key comes from the store, is decrypted by the keyring,
// and the server's host key is pinned (§20.1). Dials go through Open, or —
// for the few sites with a test seam or per-step operator messages — through
// Key + HostKey, so the pin has a single source either way.
package serverdial

import (
	"context"
	"fmt"
	"time"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// KeyStore is the single query this package needs.
type KeyStore interface {
	GetPrivateKeyByID(context.Context, int64) (store.PrivateKey, error)
}

// HostKey is the pinned SSH host key of a server, or "" on first contact
// (§20.1). Empty happens exactly once, during the first validation of a
// server (trust-on-first-use); every dial afterwards runs against a pinned
// key, so a server that suddenly presents a different one fails loudly
// instead of being handed our deploy key.
func HostKey(server store.Server) string {
	if server.HostKeyFingerprint == nil {
		return ""
	}
	return *server.HostKeyFingerprint
}

// Key fetches and decrypts the private key PEM of a server's SSH identity.
// Exposed for the rare caller that dials through a test seam; everyone else
// uses Open.
func Key(ctx context.Context, q KeyStore, keyring *envelope.Keyring, server store.Server) (string, error) {
	key, err := q.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return "", fmt.Errorf("private key fetch: %w", err)
	}
	pem, err := keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return "", fmt.Errorf("private key decrypt: %w", err)
	}
	return string(pem), nil
}

// Open dials a server with its provisioned key and pinned host key.
func Open(ctx context.Context, q KeyStore, keyring *envelope.Keyring, server store.Server) (*sshexec.Client, error) {
	pem, err := Key(ctx, q, keyring, server)
	if err != nil {
		return nil, err
	}
	return sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, pem,
		time.Duration(server.SshTimeoutSeconds)*time.Second, HostKey(server))
}
