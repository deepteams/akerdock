package serverdial

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

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

// loopbackSSH is the smallest server that completes a handshake and records
// the exec commands it receives — enough to prove which POLICY the dialed
// client carries, which is DialWithKey's whole contract.
type loopbackSSH struct {
	listener net.Listener
	mu       sync.Mutex
	cmds     []string
}

func startLoopbackSSH(t *testing.T) *loopbackSSH {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil },
	}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &loopbackSSH{listener: listener}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			go s.serve(raw, config)
		}
	}()
	return s
}

func (s *loopbackSSH) serve(raw net.Conn, config *ssh.ServerConfig) {
	_, channels, requests, err := ssh.NewServerConn(raw, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		channel, chanRequests, err := incoming.Accept()
		if err != nil {
			continue
		}
		go func() {
			for request := range chanRequests {
				if request.Type != "exec" {
					_ = request.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				_ = ssh.Unmarshal(request.Payload, &payload)
				s.mu.Lock()
				s.cmds = append(s.cmds, payload.Command)
				s.mu.Unlock()
				_ = request.Reply(true, nil)
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = channel.Close()
				return
			}
		}()
	}
}

func (s *loopbackSSH) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

// DialWithKey must hand back a client carrying the server's sudo policy: the
// same command goes out bare for a bare server and wrapped for a use_sudo one.
func TestDialWithKeyAppliesTheSudoPolicy(t *testing.T) {
	srv := startLoopbackSSH(t)
	host, portRaw, err := net.SplitHostPort(srv.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil {
		t.Fatal(err)
	}
	material, err := sshkey.GenerateEd25519("serverdial-policy")
	if err != nil {
		t.Fatal(err)
	}
	server := store.Server{Host: host, Port: int32(port), SshUser: "deploy", SshTimeoutSeconds: 2}

	ctx := context.Background()
	for _, useSudo := range []bool{false, true} {
		server.UseSudo = useSudo
		client, err := DialWithKey(ctx, server, material.PrivatePEM)
		if err != nil {
			t.Fatalf("DialWithKey(use_sudo=%v) = %v", useSudo, err)
		}
		if _, err := client.Run(ctx, "true"); err != nil {
			t.Fatal(err)
		}
		_ = client.Close()
	}

	cmds := srv.commands()
	if len(cmds) != 2 || cmds[0] != "true" || !strings.HasPrefix(cmds[1], "LC_ALL=C sudo -n -- sh -c ") {
		t.Fatalf("commands = %q, want a bare `true` then a sudo-wrapped one", cmds)
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
