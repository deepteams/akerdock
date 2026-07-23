// Package sshexec runs commands on target servers over SSH (§3.1). Every
// call has a timeout and returns a classified error (§22.1).
package sshexec

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Client is one authenticated SSH connection to a server.
type Client struct {
	conn *ssh.Client
	// HostKeyFingerprint is the SHA256 fingerprint of the key the server
	// actually presented — pinned on later connections (§20.1).
	HostKeyFingerprint string
}

// ErrHostKeyChanged is returned when a server presents a different host key
// than the pinned one. It is deliberately its own error: the caller must be
// able to tell "the server is unreachable" from "the server is not the server
// we onboarded", which is either a rebuild or an attack — and never something
// to paper over with a retry.
var ErrHostKeyChanged = errors.New("sshexec: host key changed")

// Dial opens an SSH connection with the given private key material.
//
// expectedFingerprint pins the host key (trust-on-first-use, §20.1): pass the
// fingerprint recorded at validation, and the handshake fails with
// ErrHostKeyChanged if the server presents anything else. Pass "" ONLY on the
// very first contact — the validation job — where there is nothing to compare
// against yet; the fingerprint it observes is what gets pinned.
//
// The parameter is mandatory rather than optional on purpose: an SSH client
// that silently accepts any host key is a man-in-the-middle away from handing
// over the deploy key and every secret uploaded to the server, and an
// easy-to-forget option is an option that gets forgotten.
func Dial(ctx context.Context, host string, port int, user, privateKeyPEM string, timeout time.Duration, expectedFingerprint string) (*Client, error) {
	signer, err := ssh.ParsePrivateKey([]byte(privateKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("sshexec: private key: %w", err)
	}

	var fingerprint string
	cfg := &ssh.ClientConfig{
		User:    user,
		Auth:    []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout: timeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			if expectedFingerprint == "" {
				return nil // first contact: this is what we pin
			}
			// Constant-time: the fingerprint is public, but comparing secrets
			// and non-secrets differently is how one of them ends up compared
			// the wrong way.
			if subtle.ConstantTimeCompare([]byte(fingerprint), []byte(expectedFingerprint)) != 1 {
				return fmt.Errorf("%w: expected %s, got %s", ErrHostKeyChanged, expectedFingerprint, fingerprint)
			}
			return nil
		},
	}

	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	dialer := net.Dialer{Timeout: timeout}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sshexec: dial %s: %w", addr, err)
	}
	conn, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("sshexec: handshake with %s: %w", addr, err)
	}
	return &Client{conn: ssh.NewClient(conn, chans, reqs), HostKeyFingerprint: fingerprint}, nil
}

// Result carries the outcome of a remote command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes a command in its own session, bounded by ctx.
func (c *Client) Run(ctx context.Context, command string) (*Result, error) {
	return c.run(ctx, command, nil, nil)
}

// RunInput executes a command feeding input on stdin — used to upload
// sensitive file contents without exposing them in argv (INV-003).
func (c *Client) RunInput(ctx context.Context, command, input string) (*Result, error) {
	return c.run(ctx, command, strings.NewReader(input), nil)
}

// RunStream is Run with live output: every chunk the command writes — stdout
// and stderr interleaved, in arrival order — is also handed to onOutput as it
// arrives. For long-running commands whose console matters while they run
// (a docker build, typically), not just once they exit.
func (c *Client) RunStream(ctx context.Context, command string, onOutput func(string)) (*Result, error) {
	return c.run(ctx, command, nil, onOutput)
}

// RunInputStream is RunInput with the live output of RunStream.
func (c *Client) RunInputStream(ctx context.Context, command, input string, onOutput func(string)) (*Result, error) {
	return c.run(ctx, command, strings.NewReader(input), onOutput)
}

// callbackWriter serializes chunks into the onOutput callback: the ssh
// library pumps stdout and stderr from two goroutines, the callback must not
// see them concurrently.
type callbackWriter struct {
	mu sync.Mutex
	fn func(string)
}

func (w *callbackWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.fn(string(p))
	w.mu.Unlock()
	return len(p), nil
}

func (c *Client) run(ctx context.Context, command string, stdin io.Reader, onOutput func(string)) (*Result, error) {
	session, err := c.conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("sshexec: session: %w", err)
	}
	defer func() { _ = session.Close() }()

	var stdout, stderr bytes.Buffer
	session.Stdout, session.Stderr = &stdout, &stderr
	if onOutput != nil {
		tee := &callbackWriter{fn: onOutput}
		session.Stdout = io.MultiWriter(&stdout, tee)
		session.Stderr = io.MultiWriter(&stderr, tee)
	}
	session.Stdin = stdin

	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return nil, ctx.Err()
	case err := <-done:
		res := &Result{Stdout: strings.TrimSpace(stdout.String()), Stderr: strings.TrimSpace(stderr.String())}
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitStatus()
			return res, nil
		}
		if err != nil {
			return nil, fmt.Errorf("sshexec: run: %w", err)
		}
		return res, nil
	}
}

// Close terminates the connection.
func (c *Client) Close() error { return c.conn.Close() }
