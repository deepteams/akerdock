package serverdial

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/sshkey"
	"github.com/deepteams/akerdock/internal/store"
)

type fakeKeyStore struct {
	key store.PrivateKey
	err error
}

func (f *fakeKeyStore) GetPrivateKeyByID(context.Context, int64) (store.PrivateKey, error) {
	return f.key, f.err
}

func testKeyring(t *testing.T) *envelope.Keyring {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte("1:" + key + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

func testServer() store.Server {
	fingerprint := "SHA256:pinned"
	return store.Server{
		Host: "127.0.0.1", Port: 1, SshUser: "deploy",
		SshTimeoutSeconds: 1, HostKeyFingerprint: &fingerprint,
	}
}

func TestHostKey(t *testing.T) {
	if got := HostKey(store.Server{}); got != "" {
		t.Fatalf("unpinned server host key = %q, want empty (trust-on-first-use)", got)
	}
	if got := HostKey(testServer()); got != "SHA256:pinned" {
		t.Fatalf("host key = %q, want the pinned fingerprint", got)
	}
}

func TestKeyDecryptsTheStoredPEM(t *testing.T) {
	keyring := testKeyring(t)
	u := pguuid.MustParse("6f7e9a34-0000-4000-8000-000000000001")
	enc, err := keyring.Encrypt("private_keys", "private_key_enc", pguuid.String(u), []byte("PEM-BYTES"))
	if err != nil {
		t.Fatal(err)
	}
	q := &fakeKeyStore{key: store.PrivateKey{Uuid: u, PrivateKeyEnc: enc}}

	pem, err := Key(context.Background(), q, keyring, testServer())
	if err != nil || pem != "PEM-BYTES" {
		t.Fatalf("Key() = %q, %v", pem, err)
	}
}

func TestKeySurfacesFetchAndDecryptFailures(t *testing.T) {
	keyring := testKeyring(t)

	q := &fakeKeyStore{err: errors.New("no rows")}
	if _, err := Key(context.Background(), q, keyring, testServer()); err == nil ||
		!strings.Contains(err.Error(), "private key fetch") {
		t.Fatalf("fetch failure = %v, want a wrapped fetch error", err)
	}

	// A ciphertext sealed for another row must not open: the AAD binds it.
	u := pguuid.MustParse("6f7e9a34-0000-4000-8000-000000000001")
	other := pguuid.MustParse("6f7e9a34-0000-4000-8000-000000000002")
	enc, err := keyring.Encrypt("private_keys", "private_key_enc", pguuid.String(other), []byte("PEM"))
	if err != nil {
		t.Fatal(err)
	}
	q = &fakeKeyStore{key: store.PrivateKey{Uuid: u, PrivateKeyEnc: enc}}
	if _, err := Key(context.Background(), q, keyring, testServer()); err == nil ||
		!strings.Contains(err.Error(), "private key decrypt") {
		t.Fatalf("decrypt failure = %v, want a wrapped decrypt error", err)
	}
}

func TestOpenPropagatesKeyErrors(t *testing.T) {
	q := &fakeKeyStore{err: errors.New("db down")}
	if _, err := Open(context.Background(), q, testKeyring(t), testServer()); err == nil ||
		!strings.Contains(err.Error(), "private key fetch") {
		t.Fatalf("Open() = %v, want the key error", err)
	}
}

func TestOpenDialsWithTheDecryptedKey(t *testing.T) {
	// No SSH server listens on port 1: the dial must fail, but only AFTER the
	// key pipeline succeeded — which is exactly what this asserts (a fetch or
	// decrypt failure would carry the "private key" wrap instead). The PEM is
	// real because Dial parses it before connecting.
	material, err := sshkey.GenerateEd25519("serverdial-test")
	if err != nil {
		t.Fatal(err)
	}
	keyring := testKeyring(t)
	u := pguuid.MustParse("6f7e9a34-0000-4000-8000-000000000001")
	enc, err := keyring.Encrypt("private_keys", "private_key_enc", pguuid.String(u), []byte(material.PrivatePEM))
	if err != nil {
		t.Fatal(err)
	}
	q := &fakeKeyStore{key: store.PrivateKey{Uuid: u, PrivateKeyEnc: enc}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = Open(ctx, q, keyring, testServer())
	if err == nil {
		t.Fatal("Open() succeeded against a closed port")
	}
	if strings.Contains(err.Error(), "private key") {
		t.Fatalf("Open() failed in the key pipeline, not the dial: %v", err)
	}
}
