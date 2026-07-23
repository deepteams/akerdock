package sshexec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	aksshkey "github.com/deepteams/akerdock/internal/sshkey"
)

type testSSHServer struct {
	listener    net.Listener
	config      *ssh.ServerConfig
	rejectPTY   bool
	rejectStart bool

	mu    sync.Mutex
	conns []net.Conn
}

func newTestSSHServer(t *testing.T, rejectPTY, rejectStart bool) *testSSHServer {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &testSSHServer{
		listener: listener, config: config, rejectPTY: rejectPTY, rejectStart: rejectStart,
	}
	go server.accept()
	t.Cleanup(server.close)
	return server
}

func (s *testSSHServer) accept() {
	for {
		raw, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, raw)
		s.mu.Unlock()
		go s.serve(raw)
	}
}

func (s *testSSHServer) serve(raw net.Conn) {
	connection, channels, requests, err := ssh.NewServerConn(raw, s.config)
	if err != nil {
		_ = raw.Close()
		return
	}
	go ssh.DiscardRequests(requests)
	go func() {
		for channel := range channels {
			go s.handleChannel(channel)
		}
		_ = connection.Close()
	}()
}

func (s *testSSHServer) handleChannel(in ssh.NewChannel) {
	if in.ChannelType() != "session" {
		_ = in.Reject(ssh.UnknownChannelType, "session only")
		return
	}
	channel, requests, err := in.Accept()
	if err != nil {
		return
	}
	for request := range requests {
		switch request.Type {
		case "pty-req":
			request.Reply(!s.rejectPTY, nil)
			if s.rejectPTY {
				_ = channel.Close()
				return
			}
		case "window-change":
			request.Reply(true, nil)
		case "shell":
			if s.rejectStart {
				request.Reply(false, nil)
				_ = channel.Close()
				return
			}
			request.Reply(true, nil)
			_, _ = channel.Write([]byte("shell ready\n"))
			go func() { _, _ = io.Copy(channel, channel) }()
		case "exec":
			if s.rejectStart {
				request.Reply(false, nil)
				_ = channel.Close()
				return
			}
			var payload struct{ Command string }
			_ = ssh.Unmarshal(request.Payload, &payload)
			request.Reply(true, nil)
			switch payload.Command {
			case "success", "input":
				if payload.Command == "input" {
					raw, _ := io.ReadAll(channel)
					_, _ = channel.Write([]byte("stdin=" + string(raw)))
				} else {
					_, _ = channel.Write([]byte(" stdout \n"))
					_, _ = channel.Stderr().Write([]byte(" stderr \n"))
				}
				sendExit(channel, 0)
				return
			case "exit7":
				_, _ = channel.Stderr().Write([]byte(" failed "))
				sendExit(channel, 7)
				return
			case "abrupt":
				_ = channel.Close()
				return
			case "block":
				// Keep processing requests below until SIGKILL arrives.
			case "interactive":
				_, _ = channel.Write([]byte("pty ready\n"))
				go func() { _, _ = io.Copy(channel, channel) }()
			default:
				sendExit(channel, 0)
				return
			}
		case "signal":
			request.Reply(true, nil)
			sendExit(channel, 137)
			return
		default:
			request.Reply(false, nil)
		}
	}
}

func sendExit(channel ssh.Channel, status uint32) {
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	_ = channel.Close()
}

func (s *testSSHServer) address(t *testing.T) (string, int) {
	t.Helper()
	host, rawPort, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func (s *testSSHServer) close() {
	_ = s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, connection := range s.conns {
		_ = connection.Close()
	}
}

func clientPrivateKey(t *testing.T) string {
	t.Helper()
	material, err := aksshkey.GenerateEd25519("unit-test")
	if err != nil {
		t.Fatal(err)
	}
	return material.PrivatePEM
}

func dialTestServer(t *testing.T, server *testSSHServer, expected string) *Client {
	t.Helper()
	host, port := server.address(t)
	client, err := Dial(context.Background(), host, port, "tester", clientPrivateKey(t), 2*time.Second, expected)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestDialPinsHostKey(t *testing.T) {
	server := newTestSSHServer(t, false, false)
	first := dialTestServer(t, server, "")
	if first.HostKeyFingerprint == "" {
		t.Fatal("first contact did not expose the host fingerprint")
	}
	second := dialTestServer(t, server, first.HostKeyFingerprint)
	if second.HostKeyFingerprint != first.HostKeyFingerprint {
		t.Fatalf("fingerprints differ: %q / %q", first.HostKeyFingerprint, second.HostKeyFingerprint)
	}

	host, port := server.address(t)
	_, err := Dial(context.Background(), host, port, "tester", clientPrivateKey(t),
		2*time.Second, "SHA256:not-the-key")
	if err == nil || !errors.Is(err, ErrHostKeyChanged) ||
		!strings.Contains(err.Error(), "handshake") {
		t.Fatalf("changed host key error = %v", err)
	}
}

func TestDialErrors(t *testing.T) {
	if _, err := Dial(context.Background(), "127.0.0.1", 22, "user", "bad key",
		time.Millisecond, ""); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("private key error = %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, rawPort, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(rawPort)
	_ = listener.Close()
	if _, err := Dial(context.Background(), host, port, "user", clientPrivateKey(t),
		100*time.Millisecond, ""); err == nil || !strings.Contains(err.Error(), "dial") {
		t.Fatalf("dial error = %v", err)
	}

	junk, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer junk.Close()
	go func() {
		connection, acceptErr := junk.Accept()
		if acceptErr == nil {
			_, _ = connection.Write([]byte("not ssh\r\n"))
			_ = connection.Close()
		}
	}()
	host, rawPort, _ = net.SplitHostPort(junk.Addr().String())
	port, _ = strconv.Atoi(rawPort)
	if _, err := Dial(context.Background(), host, port, "user", clientPrivateKey(t),
		time.Second, ""); err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("handshake error = %v", err)
	}
}

func TestRunSuccessInputAndExitStatus(t *testing.T) {
	client := dialTestServer(t, newTestSSHServer(t, false, false), "")
	result, err := client.Run(context.Background(), "success")
	if err != nil || result.Stdout != "stdout" || result.Stderr != "stderr" || result.ExitCode != 0 {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	result, err = client.RunInput(context.Background(), "input", "sensitive input")
	if err != nil || result.Stdout != "stdin=sensitive input" {
		t.Fatalf("RunInput = %#v, %v", result, err)
	}
	result, err = client.Run(context.Background(), "exit7")
	if err != nil || result.ExitCode != 7 || result.Stderr != "failed" {
		t.Fatalf("exit result = %#v, %v", result, err)
	}
}

// RunStream must hand the output to the callback AND still return it in the
// Result: the stream is a live view, not a replacement for the transcript.
func TestRunStreamDeliversLiveOutput(t *testing.T) {
	client := dialTestServer(t, newTestSSHServer(t, false, false), "")
	var mu sync.Mutex
	var streamed strings.Builder
	result, err := client.RunStream(context.Background(), "success", func(chunk string) {
		mu.Lock()
		streamed.WriteString(chunk)
		mu.Unlock()
	})
	if err != nil || result.Stdout != "stdout" || result.Stderr != "stderr" {
		t.Fatalf("RunStream = %#v, %v", result, err)
	}
	got := streamed.String()
	if !strings.Contains(got, "stdout") || !strings.Contains(got, "stderr") {
		t.Fatalf("callback missed output, streamed %q", got)
	}
}

func TestRunCancellationAndProtocolErrors(t *testing.T) {
	client := dialTestServer(t, newTestSSHServer(t, false, false), "")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Run(ctx, "block"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled Run = %v", err)
	}
	if _, err := client.Run(context.Background(), "abrupt"); err == nil ||
		!strings.Contains(err.Error(), "sshexec: run") {
		t.Fatalf("abrupt Run = %v", err)
	}

	closed := dialTestServer(t, newTestSSHServer(t, false, false), "")
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := closed.Run(context.Background(), "success"); err == nil ||
		!strings.Contains(err.Error(), "session") {
		t.Fatalf("closed client Run = %v", err)
	}
}

func TestStartPTYCommandShellResizeAndIO(t *testing.T) {
	client := dialTestServer(t, newTestSSHServer(t, false, false), "")
	for _, command := range []string{"interactive", ""} {
		pty, err := client.StartPTY(command, 100, 30)
		if err != nil {
			t.Fatalf("StartPTY(%q): %v", command, err)
		}
		buffer := make([]byte, 64)
		n, err := pty.Read(buffer)
		if err != nil || !strings.Contains(string(buffer[:n]), "ready") {
			t.Fatalf("initial PTY output = %q, %v", buffer[:n], err)
		}
		if err := pty.Resize(120, 40); err != nil {
			t.Fatalf("Resize: %v", err)
		}
		if _, err := pty.Write([]byte("hello\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		n, err = pty.Read(buffer)
		if err != nil || string(buffer[:n]) != "hello\n" {
			t.Fatalf("echo = %q, %v", buffer[:n], err)
		}
		if err := pty.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

func TestStartPTYFailures(t *testing.T) {
	rejected := dialTestServer(t, newTestSSHServer(t, true, false), "")
	if _, err := rejected.StartPTY("interactive", 80, 24); err == nil ||
		!strings.Contains(err.Error(), "request pty") {
		t.Fatalf("PTY rejection = %v", err)
	}

	startRejected := dialTestServer(t, newTestSSHServer(t, false, true), "")
	if _, err := startRejected.StartPTY("interactive", 80, 24); err == nil ||
		!strings.Contains(err.Error(), "start") {
		t.Fatalf("start rejection = %v", err)
	}

	closed := dialTestServer(t, newTestSSHServer(t, false, false), "")
	_ = closed.Close()
	if _, err := closed.StartPTY("", 80, 24); err == nil ||
		!strings.Contains(err.Error(), "session") {
		t.Fatalf("closed client PTY = %v", err)
	}
}
