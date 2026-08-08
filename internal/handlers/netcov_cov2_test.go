// WebSocket-path coverage for the network-session handler slice: the terminal
// redeem (ADR-024), the tunnel redeem (ADR-032/045), the agent relay
// (ADR-052 §8) and the ingress tunnel mint (ADR-060). Real loopback WebSocket
// clients drive httptest servers; SSH targets are a loopback SSH server with
// direct-tcpip support; Docker targets are a scripted agent channel.
package handlers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/ssh"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/api"
	aksshkey "github.com/deepteams/akerdock/internal/sshkey"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// ---------------------------------------------------------------------------
// Loopback SSH server (pty/shell echo + direct-tcpip echo)
// ---------------------------------------------------------------------------

type netcovSSHServer struct {
	listener    net.Listener
	config      *ssh.ServerConfig
	rejectStart bool

	mu    sync.Mutex
	conns []net.Conn
}

func netcovNewSSHServer(t *testing.T, rejectStart bool) *netcovSSHServer {
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
	s := &netcovSSHServer{listener: listener, config: config, rejectStart: rejectStart}
	go s.accept()
	t.Cleanup(s.close)
	return s
}

func (s *netcovSSHServer) accept() {
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

func (s *netcovSSHServer) serve(raw net.Conn) {
	conn, channels, requests, err := ssh.NewServerConn(raw, s.config)
	if err != nil {
		_ = raw.Close()
		return
	}
	go ssh.DiscardRequests(requests)
	go func() {
		for ch := range channels {
			go s.handleChannel(ch)
		}
		_ = conn.Close()
	}()
}

func (s *netcovSSHServer) handleChannel(in ssh.NewChannel) {
	switch in.ChannelType() {
	case "session":
		channel, requests, err := in.Accept()
		if err != nil {
			return
		}
		for request := range requests {
			switch request.Type {
			case "pty-req", "window-change":
				_ = request.Reply(true, nil)
			case "shell", "exec":
				if s.rejectStart {
					_ = request.Reply(false, nil)
					_ = channel.Close()
					return
				}
				_ = request.Reply(true, nil)
				_, _ = channel.Write([]byte("shell ready\n"))
				go func() { _, _ = io.Copy(channel, channel) }()
			default:
				_ = request.Reply(false, nil)
			}
		}
	case "direct-tcpip":
		channel, requests, err := in.Accept()
		if err != nil {
			return
		}
		go ssh.DiscardRequests(requests)
		go func() {
			_, _ = io.Copy(channel, channel)
			_ = channel.Close()
		}()
	default:
		_ = in.Reject(ssh.UnknownChannelType, "unsupported")
	}
}

func (s *netcovSSHServer) address(t *testing.T) (string, int) {
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

func (s *netcovSSHServer) close() {
	_ = s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
}

// netcovProvisionSSH points GetServerByID at the loopback SSH server and makes
// the provisioned key decryptable by the test keyring.
func netcovProvisionSSH(t *testing.T, a *API, db *netcovDB, s *netcovSSHServer) {
	t.Helper()
	host, port := s.address(t)
	material, err := aksshkey.GenerateEd25519("netcov")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := a.Keyring.Encrypt("private_keys", "private_key_enc", fixtureUUID, []byte(material.PrivatePEM))
	if err != nil {
		t.Fatal(err)
	}
	var u pgtype.UUID
	_ = u.Scan(fixtureUUID)
	db.rule(netcovRule{match: "-- name: GetPrivateKeyByID ", set: netcovRowOf(store.PrivateKey{
		ID: 1, Uuid: u, Name: "k", FingerprintSha256: "fp", PublicKey: "pk", PrivateKeyEnc: enc,
	})})
	db.rule(netcovRule{match: "-- name: GetServerByID ", set: netcovRowOf(store.Server{
		ID: 1, Uuid: u, TeamID: 1, Name: "srv", Host: host, Port: int32(port),
		SshUser: "tester", SshTimeoutSeconds: 2, PrivateKeyID: 1,
		Status: store.ServerStatusReady, ProxyType: store.ProxyTypeTraefik,
	})})
}

// ---------------------------------------------------------------------------
// Scripted agent channel
// ---------------------------------------------------------------------------

// netcovScriptAgent plays the agent behind a dialPair channel: commands are
// answered by script (nil result = never answer), and any command whose
// script sets attach=true afterwards echoes input chunks back as output.
func netcovScriptAgent(agent *websocket.Conn, script func(cmd agentwire.Command) (res *agentwire.Result, attach bool)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		echo := map[int64]bool{}
		for {
			_, data, err := agent.Read(ctx)
			if err != nil {
				return
			}
			var f agentwire.Frame
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			switch f.Type {
			case agentwire.FrameCommand:
				if f.Cmd == nil {
					continue
				}
				res, attach := script(*f.Cmd)
				if res != nil {
					res.ID = f.Cmd.ID
					_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameResult, Res: res})
				}
				if attach {
					echo[f.Cmd.ID] = true
				}
			case agentwire.FrameStream:
				if f.Chunk == nil || !echo[f.Chunk.ID] {
					continue
				}
				if len(f.Chunk.Data) > 0 {
					_ = agentWrite(agent, agentwire.Frame{
						Type:  agentwire.FrameStream,
						Chunk: &agentwire.StreamChunk{ID: f.Chunk.ID, Data: f.Chunk.Data},
					})
				}
				if f.Chunk.EOF {
					_ = agentWrite(agent, agentwire.Frame{
						Type:  agentwire.FrameStream,
						Chunk: &agentwire.StreamChunk{ID: f.Chunk.ID, EOF: true},
					})
				}
			}
		}
	}()
}

// netcovAgent registers a scripted agent for server 1 on a fresh registry.
func netcovAgent(t *testing.T, a *API, script func(cmd agentwire.Command) (*agentwire.Result, bool)) {
	t.Helper()
	ac, agent := dialPair(t)
	a.AgentRPC = &AgentConns{}
	a.AgentRPC.register(1, ac)
	netcovScriptAgent(agent, script)
}

func netcovDialWS(t *testing.T, url string, subprotocols ...string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, strings.Replace(url, "http", "ws", 1), &websocket.DialOptions{
		Subprotocols: subprotocols,
	})
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	conn.SetReadLimit(1 << 20)
	return conn
}

func netcovReadBinary(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ == websocket.MessageBinary {
			return data
		}
	}
}

// ---------------------------------------------------------------------------
// terminal.go — the WebSocket redeem
// ---------------------------------------------------------------------------

func netcovTerminalClaim(db *netcovDB, row store.TerminalSession) {
	_ = row.Uuid.Scan(fixtureUUID)
	db.rule(netcovRule{match: "-- name: ClaimTerminalSession ", set: netcovRowOf(row)})
}

func TestNetcovTerminalWebSocketRefusals(t *testing.T) {
	t.Run("missing token", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.TerminalWebSocket(rec, httptest.NewRequest(http.MethodGet, "/terminal/ws", nil))
		netcovStatus(t, rec, http.StatusUnauthorized)
	})
	t.Run("unclaimable token", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ClaimTerminalSession ", noRows: true})
		rec := httptest.NewRecorder()
		a.TerminalWebSocket(rec, httptest.NewRequest(http.MethodGet, "/terminal/ws?token=x", nil))
		netcovStatus(t, rec, http.StatusUnauthorized)
	})
	t.Run("target without a server", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovTerminalClaim(db, store.TerminalSession{ID: 1, TeamID: 1, TargetKind: store.TerminalTargetServer})
		rec := httptest.NewRecorder()
		a.TerminalWebSocket(rec, httptest.NewRequest(http.MethodGet, "/terminal/ws?token=x", nil))
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("server row gone", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovTerminalClaim(db, store.TerminalSession{
			ID: 1, TeamID: 1,
			TargetKind: store.TerminalTargetServer, ServerID: ptr(int64(1)),
		})
		db.rule(netcovRule{match: "-- name: GetServerByID ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.TerminalWebSocket(rec, httptest.NewRequest(http.MethodGet, "/terminal/ws?token=x", nil))
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("ssh unreachable", func(t *testing.T) {
		a, db := netcovAPI(t)
		// The default private-key row does not decrypt: the dial fails fast.
		netcovTerminalClaim(db, store.TerminalSession{
			ID: 1, TeamID: 1,
			TargetKind: store.TerminalTargetServer, ServerID: ptr(int64(1)),
		})
		rec := httptest.NewRecorder()
		a.TerminalWebSocket(rec, httptest.NewRequest(http.MethodGet, "/terminal/ws?token=x", nil))
		netcovStatus(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "not reachable") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("pty refused by the server", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovProvisionSSH(t, a, db, netcovNewSSHServer(t, true))
		netcovTerminalClaim(db, store.TerminalSession{
			ID: 1, TeamID: 1,
			TargetKind: store.TerminalTargetServer, ServerID: ptr(int64(1)),
		})
		rec := httptest.NewRecorder()
		a.TerminalWebSocket(rec, httptest.NewRequest(http.MethodGet, "/terminal/ws?token=x", nil))
		netcovStatus(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "could not start") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("upgrade failure after a resolvable target", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovProvisionSSH(t, a, db, netcovNewSSHServer(t, false))
		netcovTerminalClaim(db, store.TerminalSession{
			ID: 1, TeamID: 1,
			TargetKind: store.TerminalTargetServer, ServerID: ptr(int64(1)),
		})
		rec := httptest.NewRecorder()
		// A plain GET is not a WebSocket handshake: Accept fails after the PTY
		// opened, and the session is finalized.
		a.TerminalWebSocket(rec, httptest.NewRequest(http.MethodGet, "/terminal/ws?token=x", nil))
		if rec.Code < http.StatusBadRequest {
			t.Fatalf("status = %d, want a handshake failure", rec.Code)
		}
	})
}

func TestNetcovTerminalWebSocketServerShell(t *testing.T) {
	a, db := netcovAPI(t)
	netcovProvisionSSH(t, a, db, netcovNewSSHServer(t, false))
	netcovTerminalClaim(db, store.TerminalSession{
		ID: 1, TeamID: 1,
		TargetKind: store.TerminalTargetServer, ServerID: ptr(int64(1)),
	})

	srv := httptest.NewServer(http.HandlerFunc(a.TerminalWebSocket))
	defer srv.Close()
	conn := netcovDialWS(t, srv.URL+"?token=x&cols=100&rows=30")

	if got := netcovReadBinary(t, conn); !strings.Contains(string(got), "shell ready") {
		t.Fatalf("greeting = %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":90,"rows":25}`)); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if got := netcovReadBinary(t, conn); string(got) != "hello\n" {
		t.Fatalf("echo = %q", got)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestNetcovTerminalWebSocketContainerExec(t *testing.T) {
	a, db := netcovAPI(t)
	netcovTerminalClaim(db, store.TerminalSession{
		ID: 1, TeamID: 1,
		TargetKind: store.TerminalTargetContainer, ServerID: ptr(int64(1)), ResourceID: ptr(int64(1)),
	})
	netcovAgent(t, a, func(cmd agentwire.Command) (*agentwire.Result, bool) {
		switch cmd.Method {
		case agentwire.MethodContainerExecCreate:
			return &agentwire.Result{Body: json.RawMessage(`{"Id":"e1"}`)}, false
		case agentwire.MethodContainerExecAttach:
			return &agentwire.Result{}, true
		case agentwire.MethodContainerExecResize:
			return &agentwire.Result{}, false
		}
		return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected"}}, false
	})

	srv := httptest.NewServer(http.HandlerFunc(a.TerminalWebSocket))
	defer srv.Close()
	conn := netcovDialWS(t, srv.URL+"?token=x")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if got := netcovReadBinary(t, conn); string(got) != "ping" {
		t.Fatalf("echo = %q", got)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":132,"rows":43}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestNetcovExecTerminalErrors(t *testing.T) {
	claim := store.TerminalSession{
		ID: 1, TeamID: 1,
		TargetKind: store.TerminalTargetContainer, ServerID: ptr(int64(1)), ResourceID: ptr(int64(1)),
	}

	redeem := func(t *testing.T, a *API, db *netcovDB) *httptest.ResponseRecorder {
		t.Helper()
		netcovTerminalClaim(db, claim)
		rec := httptest.NewRecorder()
		a.TerminalWebSocket(rec, httptest.NewRequest(http.MethodGet, "/terminal/ws?token=x", nil))
		return rec
	}

	t.Run("agent not connected", func(t *testing.T) {
		a, db := netcovAPI(t)
		rec := redeem(t, a, db)
		netcovStatus(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "agent is not connected") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("container missing", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovAgent(t, a, func(agentwire.Command) (*agentwire.Result, bool) {
			return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeNotFound, Message: "no such container"}}, false
		})
		rec := redeem(t, a, db)
		netcovStatus(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "does not exist") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("container stopped", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovAgent(t, a, func(agentwire.Command) (*agentwire.Result, bool) {
			return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeConflict, Message: "not running"}}, false
		})
		rec := redeem(t, a, db)
		netcovStatus(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "not running") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("exec create fails", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovAgent(t, a, func(agentwire.Command) (*agentwire.Result, bool) {
			return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInternal, Message: "boom"}}, false
		})
		rec := redeem(t, a, db)
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("attach fails after create", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovAgent(t, a, func(cmd agentwire.Command) (*agentwire.Result, bool) {
			if cmd.Method == agentwire.MethodContainerExecCreate {
				return &agentwire.Result{Body: json.RawMessage(`{"Id":"e1"}`)}, false
			}
			return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInternal, Message: "no attach"}}, false
		})
		rec := redeem(t, a, db)
		netcovStatus(t, rec, http.StatusConflict)
	})
}

func TestNetcovTerminalContainerNaming(t *testing.T) {
	ctx := context.Background()

	t.Run("no resource", func(t *testing.T) {
		a, _ := netcovAPI(t)
		if _, msg := a.terminalContainer(ctx, store.TerminalSession{}); msg == "" {
			t.Fatal("a session without a resource must not resolve")
		}
	})
	t.Run("resource gone", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetResourceByID ", err: errors.New("down")})
		if _, msg := a.terminalContainer(ctx, store.TerminalSession{ResourceID: ptr(int64(1))}); msg == "" {
			t.Fatal("a vanished resource must not resolve")
		}
	})
	t.Run("resource uuid names the container", func(t *testing.T) {
		a, _ := netcovAPI(t)
		name, msg := a.terminalContainer(ctx, store.TerminalSession{ResourceID: ptr(int64(1))})
		if msg != "" || name != fixtureUUID {
			t.Fatalf("container = %q, %q", name, msg)
		}
	})
	t.Run("preview uuid wins", func(t *testing.T) {
		a, _ := netcovAPI(t)
		name, msg := a.terminalContainer(ctx, store.TerminalSession{
			ResourceID: ptr(int64(1)), PreviewID: ptr(int64(2)),
		})
		if msg != "" || name != fixtureUUID {
			t.Fatalf("container = %q, %q", name, msg)
		}
	})
	t.Run("destroyed preview refuses", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetPreviewByID ", typed: []any{store.PreviewStatusDestroyed}})
		if _, msg := a.terminalContainer(ctx, store.TerminalSession{
			ResourceID: ptr(int64(1)), PreviewID: ptr(int64(2)),
		}); !strings.Contains(msg, "destroyed") {
			t.Fatalf("msg = %q", msg)
		}
	})
	t.Run("component suffixes the base", func(t *testing.T) {
		a, _ := netcovAPI(t)
		name, msg := a.terminalContainer(ctx, store.TerminalSession{
			ResourceID: ptr(int64(1)), TargetComponent: ptr("web"),
		})
		if msg != "" || name != fixtureUUID+"-web" {
			t.Fatalf("container = %q, %q", name, msg)
		}
	})
}

// ---------------------------------------------------------------------------
// portforward.go — the WebSocket redeem
// ---------------------------------------------------------------------------

func netcovTunnelClaim(db *netcovDB, row store.PortForwardSession) {
	_ = row.Uuid.Scan(fixtureUUID)
	db.rule(netcovRule{match: "-- name: ClaimPortForwardSession ", set: netcovRowOf(row)})
}

func TestNetcovTunnelWebSocketRefusals(t *testing.T) {
	t.Run("missing token", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.TunnelWebSocket(rec, httptest.NewRequest(http.MethodGet, "/tunnel/ws", nil))
		netcovStatus(t, rec, http.StatusUnauthorized)
	})
	t.Run("unclaimable token", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: ClaimPortForwardSession ", noRows: true})
		rec := httptest.NewRecorder()
		a.TunnelWebSocket(rec, httptest.NewRequest(http.MethodGet, "/tunnel/ws?token=x", nil))
		netcovStatus(t, rec, http.StatusUnauthorized)
	})
	t.Run("unreachable target answers 409", func(t *testing.T) {
		a, db := netcovAPI(t)
		// Endpoint target whose SSH key does not decrypt: the dial fails.
		netcovTunnelClaim(db, store.PortForwardSession{
			ID: 1, TeamID: 1,
			ServerID: ptr(int64(1)), ExternalEndpointID: ptr(int64(1)), TargetPort: 5432,
		})
		rec := httptest.NewRecorder()
		a.TunnelWebSocket(rec, httptest.NewRequest(http.MethodGet, "/tunnel/ws?token=x", nil))
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("upgrade failure after a resolvable target", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovProvisionSSH(t, a, db, netcovNewSSHServer(t, false))
		netcovTunnelClaim(db, store.PortForwardSession{
			ID: 1, TeamID: 1,
			ServerID: ptr(int64(1)), ExternalEndpointID: ptr(int64(1)), TargetPort: 5432,
		})
		rec := httptest.NewRecorder()
		a.TunnelWebSocket(rec, httptest.NewRequest(http.MethodGet, "/tunnel/ws?token=x", nil))
		if rec.Code < http.StatusBadRequest {
			t.Fatalf("status = %d, want a handshake failure", rec.Code)
		}
	})
}

func TestNetcovTunnelWebSocketBridgesTCP(t *testing.T) {
	a, db := netcovAPI(t)
	netcovProvisionSSH(t, a, db, netcovNewSSHServer(t, false))
	netcovTunnelClaim(db, store.PortForwardSession{
		ID: 1, TeamID: 1,
		ServerID: ptr(int64(1)), ExternalEndpointID: ptr(int64(1)), TargetPort: 5432,
	})

	srv := httptest.NewServer(http.HandlerFunc(a.TunnelWebSocket))
	defer srv.Close()
	conn := netcovDialWS(t, srv.URL+"?token=x", "akerdock-tunnel-v1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"t":"open","id":1}`)); err != nil {
		t.Fatal(err)
	}
	// The first text frame back must be open_ok.
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		if !strings.Contains(string(data), "open_ok") {
			t.Fatalf("control = %s", data)
		}
		break
	}
	frame := append([]byte{0, 0, 0, 1}, []byte("ping")...)
	if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatal(err)
	}
	echo := netcovReadBinary(t, conn)
	if len(echo) < 4 || binary.BigEndian.Uint32(echo[:4]) != 1 || string(echo[4:]) != "ping" {
		t.Fatalf("echo = %q", echo)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestNetcovTunnelWebSocketGrantExpiry(t *testing.T) {
	a, db := netcovAPI(t)
	netcovProvisionSSH(t, a, db, netcovNewSSHServer(t, false))
	// The grant lapsed between mint and attach: the bridge gets a millisecond
	// budget and the close reason must read grant_expired, not max_duration.
	netcovTunnelClaim(db, store.PortForwardSession{
		ID: 1, TeamID: 1,
		ServerID: ptr(int64(1)), ExternalEndpointID: ptr(int64(1)), GrantID: ptr(int64(9)),
		TargetPort:      5432,
		AuthorizedUntil: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
	})

	srv := httptest.NewServer(http.HandlerFunc(a.TunnelWebSocket))
	defer srv.Close()
	conn := netcovDialWS(t, srv.URL+"?token=x", "akerdock-tunnel-v1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var err error
	for err == nil {
		_, _, err = conn.Read(ctx)
	}
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Reason != string(endReasonGrantExpired) {
		t.Fatalf("close = %v, want reason %q", err, endReasonGrantExpired)
	}
}

func TestNetcovTunnelTargetResolution(t *testing.T) {
	ctx := context.Background()

	expectMsg := func(t *testing.T, a *API, row store.PortForwardSession, fragment string) {
		t.Helper()
		client, _, msg := a.tunnelTarget(ctx, row)
		if client != nil {
			_ = client.Close()
		}
		if !strings.Contains(msg, fragment) {
			t.Fatalf("msg = %q, want %q", msg, fragment)
		}
	}

	t.Run("no server", func(t *testing.T) {
		a, _ := netcovAPI(t)
		expectMsg(t, a, store.PortForwardSession{}, "no longer exists")
	})
	t.Run("server gone", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetServerByID ", err: errors.New("down")})
		expectMsg(t, a, store.PortForwardSession{ServerID: ptr(int64(1))}, "server no longer exists")
	})
	t.Run("endpoint gone", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetExternalEndpointByID ", err: errors.New("down")})
		expectMsg(t, a, store.PortForwardSession{ServerID: ptr(int64(1)), ExternalEndpointID: ptr(int64(1))},
			"endpoint no longer exists")
	})
	t.Run("no resource", func(t *testing.T) {
		a, _ := netcovAPI(t)
		expectMsg(t, a, store.PortForwardSession{ServerID: ptr(int64(1))}, "no longer exists")
	})
	t.Run("resource gone", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetResourceByID ", err: errors.New("down")})
		expectMsg(t, a, store.PortForwardSession{ServerID: ptr(int64(1)), ResourceID: ptr(int64(1))},
			"resource no longer exists")
	})
	t.Run("destroyed preview", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetPreviewByID ", typed: []any{store.PreviewStatusDestroyed}})
		expectMsg(t, a, store.PortForwardSession{
			ServerID: ptr(int64(1)), ResourceID: ptr(int64(1)),
			PreviewID: ptr(int64(2)),
		}, "destroyed")
	})
	t.Run("agent disconnected", func(t *testing.T) {
		a, _ := netcovAPI(t)
		expectMsg(t, a, store.PortForwardSession{ServerID: ptr(int64(1)), ResourceID: ptr(int64(1))},
			"agent is not connected")
	})
	t.Run("container not running", func(t *testing.T) {
		a, _ := netcovAPI(t)
		netcovAgent(t, a, func(agentwire.Command) (*agentwire.Result, bool) {
			return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeNotFound, Message: "gone"}}, false
		})
		expectMsg(t, a, store.PortForwardSession{ServerID: ptr(int64(1)), ResourceID: ptr(int64(1))},
			"not running")
	})
	t.Run("container resolves but ssh fails", func(t *testing.T) {
		a, _ := netcovAPI(t)
		netcovAgent(t, a, func(agentwire.Command) (*agentwire.Result, bool) {
			return &agentwire.Result{Body: json.RawMessage(
				`{"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}}`)}, false
		})
		expectMsg(t, a, store.PortForwardSession{
			ServerID: ptr(int64(1)), ResourceID: ptr(int64(1)),
			PreviewID: ptr(int64(2)), TargetComponent: ptr("web"), TargetPort: 3000,
		}, "not reachable")
	})
	t.Run("container target fully resolves", func(t *testing.T) {
		a, db := netcovAPI(t)
		netcovProvisionSSH(t, a, db, netcovNewSSHServer(t, false))
		netcovAgent(t, a, func(agentwire.Command) (*agentwire.Result, bool) {
			return &agentwire.Result{Body: json.RawMessage(
				`{"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}}`)}, false
		})
		client, addr, msg := a.tunnelTarget(ctx, store.PortForwardSession{
			ServerID: ptr(int64(1)), ResourceID: ptr(int64(1)), TargetPort: 3000,
		})
		if msg != "" || client == nil {
			t.Fatalf("target = %v, %q", client, msg)
		}
		defer func() { _ = client.Close() }()
		if addr != "172.17.0.2:3000" {
			t.Fatalf("addr = %q", addr)
		}
	})
	t.Run("no IP on any network", func(t *testing.T) {
		a, _ := netcovAPI(t)
		netcovAgent(t, a, func(agentwire.Command) (*agentwire.Result, bool) {
			return &agentwire.Result{Body: json.RawMessage(`{"NetworkSettings":{"Networks":{}}}`)}, false
		})
		expectMsg(t, a, store.PortForwardSession{ServerID: ptr(int64(1)), ResourceID: ptr(int64(1))},
			"not running")
	})
}

// ---------------------------------------------------------------------------
// WebSocket adapters (wsConn / tunnelConn)
// ---------------------------------------------------------------------------

// netcovEchoWS runs a WebSocket echo server and returns a connected client;
// a text frame "bye" makes the server close normally.
func netcovEchoWS(t *testing.T) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText && string(data) == "bye" {
				_ = conn.Close(websocket.StatusNormalClosure, "bye")
				return
			}
			if err := conn.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return netcovDialWS(t, srv.URL)
}

// coder/websocket processes control frames while a reader is active. Exercise
// Ping on its own connection so cancelling that reader cannot affect the
// adapter's subsequent data-frame assertions.
func netcovAssertPing(t *testing.T, ping func(context.Context) error, conn *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	readCtx, stopRead := context.WithCancel(ctx)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_, _, _ = conn.Read(readCtx)
	}()
	if err := ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	stopRead()
	<-readDone
	conn.CloseNow()
}

func TestNetcovWSConnAdapter(t *testing.T) {
	pingConn := netcovEchoWS(t)
	netcovAssertPing(t, wsConn{pingConn}.Ping, pingConn)

	conn := netcovEchoWS(t)
	w := wsConn{conn}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := w.Write(ctx, 0, []byte("bin")); err != nil { // terminal.MessageBinary
		t.Fatal(err)
	}
	if typ, data, err := w.Read(ctx); err != nil || typ != 0 || string(data) != "bin" {
		t.Fatalf("binary echo = %v %q %v", typ, data, err)
	}
	if err := w.Write(ctx, 1, []byte("txt")); err != nil { // terminal.MessageText
		t.Fatal(err)
	}
	if typ, data, err := w.Read(ctx); err != nil || typ != 1 || string(data) != "txt" {
		t.Fatalf("text echo = %v %q %v", typ, data, err)
	}
	_ = w.Write(ctx, 1, []byte("bye"))
	if _, _, err := w.Read(ctx); err == nil {
		t.Fatal("a closed peer must surface an error")
	}
}

func TestNetcovTunnelConnAdapter(t *testing.T) {
	pingConn := netcovEchoWS(t)
	netcovAssertPing(t, tunnelConn{pingConn}.Ping, pingConn)

	conn := netcovEchoWS(t)
	c := tunnelConn{conn}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Write(ctx, tunnel.MessageBinary, []byte("bin")); err != nil {
		t.Fatal(err)
	}
	if typ, data, err := c.Read(ctx); err != nil || typ != tunnel.MessageBinary || string(data) != "bin" {
		t.Fatalf("binary echo = %v %q %v", typ, data, err)
	}
	if err := c.Write(ctx, tunnel.MessageText, []byte("txt")); err != nil {
		t.Fatal(err)
	}
	if typ, data, err := c.Read(ctx); err != nil || typ != tunnel.MessageText || string(data) != "txt" {
		t.Fatalf("text echo = %v %q %v", typ, data, err)
	}
	_ = c.Write(ctx, tunnel.MessageText, []byte("bye"))
	if _, _, err := c.Read(ctx); !errors.Is(err, tunnel.ErrClientClosed) {
		t.Fatalf("clean close must map to ErrClientClosed, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// relay.go — GET /agent/v1/relay
// ---------------------------------------------------------------------------

func TestNetcovAgentRelayAuth(t *testing.T) {
	t.Run("missing token", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.AgentRelay(rec, httptest.NewRequest(http.MethodGet, "/agent/v1/relay", nil))
		netcovStatus(t, rec, http.StatusUnauthorized)
	})
	t.Run("unknown token", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetAgentTokenByHash ", noRows: true})
		r := httptest.NewRequest(http.MethodGet, "/agent/v1/relay", nil)
		r.Header.Set("Authorization", "Bearer akda_unit")
		rec := httptest.NewRecorder()
		a.AgentRelay(rec, r)
		netcovStatus(t, rec, http.StatusUnauthorized)
	})
	t.Run("handshake failure after auth", func(t *testing.T) {
		a, _ := netcovAPI(t)
		r := httptest.NewRequest(http.MethodGet, "/agent/v1/relay", nil)
		r.Header.Set("Authorization", "Bearer akda_unit")
		rec := httptest.NewRecorder()
		a.AgentRelay(rec, r)
		if rec.Code < http.StatusBadRequest {
			t.Fatalf("status = %d, want a handshake failure", rec.Code)
		}
	})
}

// netcovDialRelay connects a relay client to a live AgentRelay handler.
func netcovDialRelay(t *testing.T, a *API) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(a.AgentRelay))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, strings.Replace(srv.URL, "http", "ws", 1), &websocket.DialOptions{
		Subprotocols: []string{agentwire.SubprotocolRelay},
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer akda_unit"}},
	})
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	conn.SetReadLimit(1 << 20)
	return conn
}

func netcovRelayWrite(t *testing.T, conn *websocket.Conn, f agentwire.Frame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}

func netcovRelayRead(t *testing.T, conn *websocket.Conn) agentwire.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("relay read: %v", err)
		}
		var f agentwire.Frame
		if json.Unmarshal(data, &f) == nil && f.Type != "" {
			return f
		}
	}
}

func TestNetcovAgentRelayWithoutChannelAnswersUnavailable(t *testing.T) {
	a, _ := netcovAPI(t)
	conn := netcovDialRelay(t, a)

	// Garbage and empty frames are skipped without killing the loop.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	netcovRelayWrite(t, conn, agentwire.Frame{Type: agentwire.FrameCommand})                                                          // nil Cmd
	netcovRelayWrite(t, conn, agentwire.Frame{Type: agentwire.FrameStream})                                                           // nil Chunk
	netcovRelayWrite(t, conn, agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: 99, Data: []byte("x")}}) // unknown attach
	netcovRelayWrite(t, conn, agentwire.Frame{Type: agentwire.FrameCancel, Cancel: 12345})                                            // unknown id

	netcovRelayWrite(t, conn, agentwire.Frame{
		Type: agentwire.FrameCommand,
		Cmd:  &agentwire.Command{ID: 1, Method: agentwire.MethodPing},
	})
	res := netcovRelayRead(t, conn)
	if res.Res == nil || res.Res.ID != 1 || res.Res.Err == nil {
		t.Fatalf("frame = %+v, want an unavailable error result", res)
	}
}

func TestNetcovAgentRelayBridgesCommandsStreamsAndAttach(t *testing.T) {
	a, _ := netcovAPI(t)
	netcovAgent(t, a, func(cmd agentwire.Command) (*agentwire.Result, bool) {
		switch cmd.Method {
		case agentwire.MethodPing:
			return &agentwire.Result{Body: json.RawMessage(`{"APIVersion":"1.45"}`)}, false
		case agentwire.MethodContainerLogs:
			return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeNotFound, Message: "gone"}}, false
		case agentwire.MethodContainerExecAttach:
			return &agentwire.Result{}, true
		}
		return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected"}}, false
	})
	conn := netcovDialRelay(t, a)

	// Plain command round-trips its body.
	netcovRelayWrite(t, conn, agentwire.Frame{
		Type: agentwire.FrameCommand,
		Cmd:  &agentwire.Command{ID: 1, Method: agentwire.MethodPing},
	})
	res := netcovRelayRead(t, conn)
	if res.Res == nil || res.Res.Err != nil || !strings.Contains(string(res.Res.Body), "1.45") {
		t.Fatalf("command result = %+v", res.Res)
	}

	// Stream open error is re-flattened intact.
	netcovRelayWrite(t, conn, agentwire.Frame{
		Type: agentwire.FrameCommand,
		Cmd:  &agentwire.Command{ID: 2, Method: agentwire.MethodContainerLogs},
	})
	res = netcovRelayRead(t, conn)
	if res.Res == nil || res.Res.ID != 2 || res.Res.Err == nil || res.Res.Err.Code != agentwire.CodeNotFound {
		t.Fatalf("stream failure = %+v", res.Res)
	}

	// Attach: ok result, then input chunks route to the attach and come back.
	netcovRelayWrite(t, conn, agentwire.Frame{
		Type: agentwire.FrameCommand,
		Cmd:  &agentwire.Command{ID: 3, Method: agentwire.MethodContainerExecAttach},
	})
	res = netcovRelayRead(t, conn)
	if res.Res == nil || res.Res.ID != 3 || res.Res.Err != nil {
		t.Fatalf("attach open = %+v", res.Res)
	}
	netcovRelayWrite(t, conn, agentwire.Frame{
		Type:  agentwire.FrameStream,
		Chunk: &agentwire.StreamChunk{ID: 3, Data: []byte("stdin")},
	})
	echo := netcovRelayRead(t, conn)
	if echo.Chunk == nil || echo.Chunk.ID != 3 || string(echo.Chunk.Data) != "stdin" {
		t.Fatalf("attach echo = %+v", echo)
	}
	netcovRelayWrite(t, conn, agentwire.Frame{
		Type:  agentwire.FrameStream,
		Chunk: &agentwire.StreamChunk{ID: 3, EOF: true},
	})
	eof := netcovRelayRead(t, conn)
	if eof.Chunk == nil || !eof.Chunk.EOF {
		t.Fatalf("attach eof = %+v", eof)
	}
}

func TestNetcovAgentRelayCancelReachesInflightCommands(t *testing.T) {
	a, _ := netcovAPI(t)
	netcovAgent(t, a, func(cmd agentwire.Command) (*agentwire.Result, bool) {
		if cmd.Method == agentwire.MethodPing {
			return nil, false // never answer: stays in flight until cancelled
		}
		return &agentwire.Result{}, false
	})
	conn := netcovDialRelay(t, a)

	netcovRelayWrite(t, conn, agentwire.Frame{
		Type: agentwire.FrameCommand,
		Cmd:  &agentwire.Command{ID: 7, Method: agentwire.MethodPing},
	})
	netcovRelayWrite(t, conn, agentwire.Frame{Type: agentwire.FrameCancel, Cancel: 7})
	res := netcovRelayRead(t, conn)
	if res.Res == nil || res.Res.ID != 7 || res.Res.Err == nil {
		t.Fatalf("cancelled command = %+v, want an error result", res.Res)
	}
}

// ---------------------------------------------------------------------------
// ingressendpoints.go
// ---------------------------------------------------------------------------

func TestNetcovCreateIngressEndpointBranches(t *testing.T) {
	valid := api.IngressEndpointCreate{Name: "dev", Fqdn: "dev.example.test", ServerUuid: fixtureUUID}

	t.Run("invalid JSON", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.CreateIngressEndpoint(rec, netcovRequest(t, http.MethodPost, "/ie", "{", netcovIdentity()))
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("empty name", func(t *testing.T) {
		a, _ := netcovAPI(t)
		body := valid
		body.Name = " "
		rec := httptest.NewRecorder()
		a.CreateIngressEndpoint(rec, netcovRequest(t, http.MethodPost, "/ie", body, netcovIdentity()))
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("invalid fqdn", func(t *testing.T) {
		a, _ := netcovAPI(t)
		body := valid
		body.Fqdn = "*.example.test"
		rec := httptest.NewRecorder()
		a.CreateIngressEndpoint(rec, netcovRequest(t, http.MethodPost, "/ie", body, netcovIdentity()))
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("basic_auth without password", func(t *testing.T) {
		a, _ := netcovAPI(t)
		body := valid
		body.Access = ptr(api.IngressEndpointCreateAccessBasicAuth)
		rec := httptest.NewRecorder()
		a.CreateIngressEndpoint(rec, netcovRequest(t, http.MethodPost, "/ie", body, netcovIdentity()))
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("basic_auth with a password succeeds", func(t *testing.T) {
		a, _ := netcovAPI(t)
		body := valid
		body.Access = ptr(api.IngressEndpointCreateAccessBasicAuth)
		body.BasicAuthPassword = ptr("s3cret")
		rec := httptest.NewRecorder()
		a.CreateIngressEndpoint(rec, netcovRequest(t, http.MethodPost, "/ie", body, netcovIdentity()))
		netcovStatus(t, rec, http.StatusCreated)
	})
	t.Run("access none is accepted", func(t *testing.T) {
		a, _ := netcovAPI(t)
		body := valid
		body.Access = ptr(api.IngressEndpointCreateAccessNone)
		rec := httptest.NewRecorder()
		a.CreateIngressEndpoint(rec, netcovRequest(t, http.MethodPost, "/ie", body, netcovIdentity()))
		netcovStatus(t, rec, http.StatusCreated)
	})
	t.Run("duplicate endpoint 409s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CreateIngressEndpoint ", err: netcovUniqueViolation()})
		rec := httptest.NewRecorder()
		a.CreateIngressEndpoint(rec, netcovRequest(t, http.MethodPost, "/ie", valid, netcovIdentity()))
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("endpoint create failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CreateIngressEndpoint ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CreateIngressEndpoint(rec, netcovRequest(t, http.MethodPost, "/ie", valid, netcovIdentity()))
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("routed hostname collision 409s and rolls back", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CreateIngressDomain ", err: netcovUniqueViolation()})
		rec := httptest.NewRecorder()
		a.CreateIngressEndpoint(rec, netcovRequest(t, http.MethodPost, "/ie", valid, netcovIdentity()))
		netcovStatus(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "already routed") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("domain registration failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: CreateIngressDomain ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.CreateIngressEndpoint(rec, netcovRequest(t, http.MethodPost, "/ie", valid, netcovIdentity()))
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
}

func TestNetcovUpdateIngressEndpointBranches(t *testing.T) {
	valid := api.IngressEndpointUpdate{Name: "dev"}

	t.Run("invalid JSON", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.UpdateIngressEndpoint(rec, netcovRequest(t, http.MethodPut, "/ie/x", "{", netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("empty name", func(t *testing.T) {
		a, _ := netcovAPI(t)
		rec := httptest.NewRecorder()
		a.UpdateIngressEndpoint(rec, netcovRequest(t, http.MethodPut, "/ie/x",
			api.IngressEndpointUpdate{Name: ""}, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("keeping the access keeps the router", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetIngressEndpointByUUID ", typed: []any{store.IngressAccessSso}})
		db.rule(netcovRule{match: "-- name: UpdateIngressEndpoint ", typed: []any{store.IngressAccessSso}})
		rec := httptest.NewRecorder()
		a.UpdateIngressEndpoint(rec, netcovRequest(t, http.MethodPut, "/ie/x", valid, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusOK)
	})
	t.Run("switching to none re-applies the router", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetIngressEndpointByUUID ", typed: []any{store.IngressAccessSso}})
		body := valid
		body.Access = ptr(api.IngressEndpointUpdateAccessNone)
		rec := httptest.NewRecorder()
		a.UpdateIngressEndpoint(rec, netcovRequest(t, http.MethodPut, "/ie/x", body, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusOK)
	})
	t.Run("basic_auth with a fresh password rehashes", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetIngressEndpointByUUID ", typed: []any{store.IngressAccessSso}})
		body := valid
		body.Access = ptr(api.IngressEndpointUpdateAccessBasicAuth)
		body.BasicAuthPassword = ptr("s3cret")
		rec := httptest.NewRecorder()
		a.UpdateIngressEndpoint(rec, netcovRequest(t, http.MethodPut, "/ie/x", body, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusOK)
	})
	t.Run("basic_auth without any password 400s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{
			match: "-- name: GetIngressEndpointByUUID ",
			typed: []any{store.IngressAccessSso}, set: map[int]any{8: nil},
		}) // BasicAuthHash NULL
		body := valid
		body.Access = ptr(api.IngressEndpointUpdateAccessBasicAuth)
		rec := httptest.NewRecorder()
		a.UpdateIngressEndpoint(rec, netcovRequest(t, http.MethodPut, "/ie/x", body, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusBadRequest)
	})
	t.Run("duplicate name 409s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: UpdateIngressEndpoint ", err: netcovUniqueViolation()})
		rec := httptest.NewRecorder()
		a.UpdateIngressEndpoint(rec, netcovRequest(t, http.MethodPut, "/ie/x", valid, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("update failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: UpdateIngressEndpoint ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.UpdateIngressEndpoint(rec, netcovRequest(t, http.MethodPut, "/ie/x", valid, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
}

func TestNetcovIngressUpdateAccessMapping(t *testing.T) {
	if ingressUpdateAccess(api.IngressEndpointUpdateAccessNone) != store.IngressAccessNone {
		t.Fatal("none")
	}
	if ingressUpdateAccess(api.IngressEndpointUpdateAccessBasicAuth) != store.IngressAccessBasicAuth {
		t.Fatal("basic_auth")
	}
	if ingressUpdateAccess(api.IngressEndpointUpdateAccessSso) != store.IngressAccessSso {
		t.Fatal("sso")
	}
}

func TestNetcovIngressAccessOrDefaultBasicAuth(t *testing.T) {
	if ingressAccessOrDefault(ptr(api.IngressEndpointCreateAccessBasicAuth)) != store.IngressAccessBasicAuth {
		t.Fatal("basic_auth must map through")
	}
	if ingressAccessOrDefault(ptr(api.IngressEndpointCreateAccessSso)) != store.IngressAccessSso {
		t.Fatal("sso must map through")
	}
}

func TestNetcovDeleteIngressEndpointBranches(t *testing.T) {
	t.Run("occupied endpoint cuts the session first", func(t *testing.T) {
		a, _ := netcovAPI(t) // default: an open session exists; AgentRPC nil → cut is a no-op
		rec := httptest.NewRecorder()
		a.DeleteIngressEndpoint(rec, netcovRequest(t, http.MethodDelete, "/ie/x", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNoContent)
	})
	t.Run("free endpoint deletes directly", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetOpenIngressSessionForEndpoint ", noRows: true})
		rec := httptest.NewRecorder()
		a.DeleteIngressEndpoint(rec, netcovRequest(t, http.MethodDelete, "/ie/x", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusNoContent)
	})
	t.Run("delete failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: DeleteIngressEndpoint ", err: errors.New("down")})
		rec := httptest.NewRecorder()
		a.DeleteIngressEndpoint(rec, netcovRequest(t, http.MethodDelete, "/ie/x", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
}

func TestNetcovIngressBasicAuthHashDirect(t *testing.T) {
	a, _ := netcovAPI(t)
	r := netcovRequest(t, http.MethodPost, "/ie", nil, nil)

	if hash, ok := a.ingressBasicAuthHash(httptest.NewRecorder(), r, store.IngressAccessSso, nil); !ok || hash != nil {
		t.Fatal("sso needs no hash")
	}
	rec := httptest.NewRecorder()
	if _, ok := a.ingressBasicAuthHash(rec, r, store.IngressAccessBasicAuth, ptr("")); ok {
		t.Fatal("an empty password must be refused")
	}
	netcovStatus(t, rec, http.StatusBadRequest)
	hash, ok := a.ingressBasicAuthHash(httptest.NewRecorder(), r, store.IngressAccessBasicAuth, ptr("pw"))
	if !ok || hash == nil || !strings.HasPrefix(*hash, "akerdock:") {
		t.Fatalf("hash = %v, %v", hash, ok)
	}
}

func TestNetcovCreateIngressTunnelBranches(t *testing.T) {
	free := func(db *netcovDB) {
		db.rule(netcovRule{match: "-- name: GetOpenIngressSessionForEndpoint ", noRows: true})
	}

	t.Run("occupied names the occupant", func(t *testing.T) {
		a, _ := netcovAPI(t) // default open session with UserEmail "unit"
		rec := httptest.NewRecorder()
		a.CreateIngressTunnel(rec, netcovRequest(t, http.MethodPost, "/it", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "in use by unit") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("occupied without an email stays anonymous", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetOpenIngressSessionForEndpoint ", set: map[int]any{14: nil}})
		rec := httptest.NewRecorder()
		a.CreateIngressTunnel(rec, netcovRequest(t, http.MethodPost, "/it", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusConflict)
		if strings.Contains(rec.Body.String(), "in use by") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("team cap 409s", func(t *testing.T) {
		a, db := netcovAPI(t)
		free(db)
		db.rule(netcovRule{match: "-- name: CountOpenIngressSessions ", set: map[int]any{0: int64(ingressTeamCap)}})
		rec := httptest.NewRecorder()
		a.CreateIngressTunnel(rec, netcovRequest(t, http.MethodPost, "/it", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("no agent registry", func(t *testing.T) {
		a, db := netcovAPI(t)
		free(db)
		rec := httptest.NewRecorder()
		a.CreateIngressTunnel(rec, netcovRequest(t, http.MethodPost, "/it", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "server_agent_unavailable") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("agent not connected", func(t *testing.T) {
		a, db := netcovAPI(t)
		free(db)
		a.AgentRPC = &AgentConns{}
		rec := httptest.NewRecorder()
		a.CreateIngressTunnel(rec, netcovRequest(t, http.MethodPost, "/it", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("lost occupancy race 409s", func(t *testing.T) {
		a, db := netcovAPI(t)
		free(db)
		db.rule(netcovRule{match: "-- name: CreateIngressSession ", err: netcovUniqueViolation()})
		netcovAgent(t, a, func(agentwire.Command) (*agentwire.Result, bool) { return &agentwire.Result{}, false })
		rec := httptest.NewRecorder()
		a.CreateIngressTunnel(rec, netcovRequest(t, http.MethodPost, "/it", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusConflict)
	})
	t.Run("session create failure 500s", func(t *testing.T) {
		a, db := netcovAPI(t)
		free(db)
		db.rule(netcovRule{match: "-- name: CreateIngressSession ", err: errors.New("down")})
		netcovAgent(t, a, func(agentwire.Command) (*agentwire.Result, bool) { return &agentwire.Result{}, false })
		rec := httptest.NewRecorder()
		a.CreateIngressTunnel(rec, netcovRequest(t, http.MethodPost, "/it", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusInternalServerError)
	})
	t.Run("armed mint returns the attach url", func(t *testing.T) {
		a, db := netcovAPI(t)
		free(db)
		netcovAgent(t, a, func(cmd agentwire.Command) (*agentwire.Result, bool) {
			if cmd.Method != agentwire.MethodIngressExpect {
				return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected"}}, false
			}
			return &agentwire.Result{}, false
		})
		rec := httptest.NewRecorder()
		a.CreateIngressTunnel(rec, netcovRequest(t, http.MethodPost, "/it", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusCreated)
		var out api.IngressTunnelSession
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out.AttachUrl, "wss://") || !strings.HasPrefix(out.Token, "akdi_") {
			t.Fatalf("mint = %+v", out)
		}
	})
	t.Run("an agent that refuses the expectation fails the mint", func(t *testing.T) {
		a, db := netcovAPI(t)
		free(db)
		netcovAgent(t, a, func(agentwire.Command) (*agentwire.Result, bool) {
			return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeUnavailable, Message: "restarting"}}, false
		})
		rec := httptest.NewRecorder()
		a.CreateIngressTunnel(rec, netcovRequest(t, http.MethodPost, "/it", nil, netcovIdentity()), fixtureUUID)
		netcovStatus(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "did not accept") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
}

func TestNetcovArmIngressExpectWithoutSender(t *testing.T) {
	a, _ := netcovAPI(t)
	a.AgentRPC = &AgentConns{}
	err := a.armIngressExpect(context.Background(), store.IngressEndpoint{ServerID: 1},
		store.IngressTunnelSession{}, "akdi_x", time.Now())
	if err == nil {
		t.Fatal("a missing sender must fail the arm")
	}
}

func TestNetcovCutIngressSessionBranches(t *testing.T) {
	ctx := context.Background()
	var u pgtype.UUID
	_ = u.Scan(fixtureUUID)

	t.Run("no registry", func(t *testing.T) {
		a, _ := netcovAPI(t)
		a.cutIngressSession(ctx, 1, u, "revoked")
	})
	t.Run("no sender", func(t *testing.T) {
		a, _ := netcovAPI(t)
		a.AgentRPC = &AgentConns{}
		a.cutIngressSession(ctx, 1, u, "revoked")
	})
	t.Run("command delivered", func(t *testing.T) {
		a, _ := netcovAPI(t)
		netcovAgent(t, a, func(cmd agentwire.Command) (*agentwire.Result, bool) {
			if cmd.Method != agentwire.MethodIngressCut {
				return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected"}}, false
			}
			return &agentwire.Result{}, false
		})
		a.cutIngressSession(ctx, 1, u, "revoked")
	})
	t.Run("command failure is logged", func(t *testing.T) {
		a, _ := netcovAPI(t)
		netcovAgent(t, a, func(agentwire.Command) (*agentwire.Result, bool) {
			return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeUnavailable, Message: "gone"}}, false
		})
		a.cutIngressSession(ctx, 1, u, "revoked")
	})
}

func TestNetcovEnqueueIngressRoutingBranches(t *testing.T) {
	row := store.IngressEndpoint{ID: 1}
	_ = row.Uuid.Scan(fixtureUUID)

	t.Run("a server that is not ready is skipped", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: GetServerByID ", typed: []any{store.ServerStatusValidating}})
		a.enqueueIngressRouting(netcovRequest(t, http.MethodPost, "/ie", nil, nil), row, 1)
	})
	t.Run("an enqueue failure is logged", func(t *testing.T) {
		a, db := netcovAPI(t)
		db.rule(netcovRule{match: "-- name: EnqueueJob", err: errors.New("down")})
		a.enqueueIngressRouting(netcovRequest(t, http.MethodPost, "/ie", nil, nil), row, 1)
	})
	t.Run("removal payload rides the same path", func(t *testing.T) {
		a, _ := netcovAPI(t)
		a.enqueueIngressRoutingRemoval(netcovRequest(t, http.MethodDelete, "/ie", nil, nil), row.Uuid, 1)
	})
}
