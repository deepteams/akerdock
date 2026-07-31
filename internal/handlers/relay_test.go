package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/docker/docker/api/types/container"

	"github.com/deepteams/akerdock/internal/agentrelay"
	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// scriptedSender plays the server's live agent channel behind the relay.
type scriptedSender struct {
	CommandFn func(ctx context.Context, method string, params any) (json.RawMessage, error)
	StreamFn  func(ctx context.Context, method string, params any) (io.ReadCloser, error)
	AttachFn  func(ctx context.Context, method string, params any) (dockerruntime.AttachStream, error)
}

func (s *scriptedSender) Command(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return s.CommandFn(ctx, method, params)
}

func (s *scriptedSender) Stream(ctx context.Context, method string, params any) (io.ReadCloser, error) {
	return s.StreamFn(ctx, method, params)
}

func (s *scriptedSender) Attach(ctx context.Context, method string, params any) (dockerruntime.AttachStream, error) {
	if s.AttachFn == nil {
		return nil, errors.New("no attach scripted")
	}
	return s.AttachFn(ctx, method, params)
}

// relayTestServer runs the api side of the relay over a scripted sender —
// the auth path is exercised by the agent-token tests; here the bridge is.
func relayTestServer(t *testing.T, senderFor func() (dockerruntime.CommandSender, bool)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentwire.SubprotocolRelay}})
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
		conn.SetReadLimit(1 << 20)
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		relayLoop(ctx, conn, agentwire.NewConn(ctx, conn), senderFor)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func relaySource(t *testing.T, srv *httptest.Server) *agentrelay.Source {
	t.Helper()
	src := &agentrelay.Source{
		BaseURL: func(context.Context) (string, error) { return srv.URL, nil },
		Token:   func(context.Context, int64) (string, error) { return "akda_test", nil },
	}
	t.Cleanup(src.Close)
	return src
}

// TestRelayBridgesUnaryCommandsAndStreams is the ADR-052 §8 end-to-end: a
// worker-side Runtime, through the relay client, the api's bridge and the
// (scripted) live channel — unary results, typed errors and streams intact
// across both hops.
func TestRelayBridgesUnaryCommandsAndStreams(t *testing.T) {
	sender := &scriptedSender{
		CommandFn: func(_ context.Context, method string, _ any) (json.RawMessage, error) {
			switch method {
			case agentwire.MethodPing:
				return json.RawMessage(`{"APIVersion":"1.45"}`), nil
			default:
				return nil, fmt.Errorf("no such container: %w", cerrdefs.ErrNotFound)
			}
		},
		StreamFn: func(_ context.Context, method string, _ any) (io.ReadCloser, error) {
			if method != agentwire.MethodContainerLogs {
				t.Errorf("unexpected stream method %q", method)
			}
			return io.NopCloser(strings.NewReader("bridged log line\n")), nil
		},
	}
	srv := relayTestServer(t, func() (dockerruntime.CommandSender, bool) { return sender, true })
	src := relaySource(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := src.Runtime(ctx, 7)
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if _, err := rt.Ping(ctx); err != nil {
		t.Fatalf("Ping across the relay: %v", err)
	}
	if _, err := rt.ContainerInspect(ctx, "gone"); !dockerruntime.IsNotFound(err) {
		t.Fatalf("inspect = %v, want IsNotFound preserved across two hops", err)
	}
	logs, err := rt.ContainerLogs(ctx, "abc", containertypes.LogsOptions{ShowStdout: true})
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	defer func() { _ = logs.Close() }()
	all, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(all) != "bridged log line\n" {
		t.Fatalf("stream = %q", all)
	}
}

func TestRelayAnswersUnavailableWithoutALiveChannel(t *testing.T) {
	srv := relayTestServer(t, func() (dockerruntime.CommandSender, bool) { return nil, false })
	src := relaySource(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := src.Runtime(ctx, 7)
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if _, err := rt.Ping(ctx); !dockerruntime.IsUnavailable(err) {
		t.Fatalf("Ping = %v, want IsUnavailable (agent absent behind the relay)", err)
	}
}

func TestRelaySourceRedialsAfterABrokenConnection(t *testing.T) {
	sender := &scriptedSender{
		CommandFn: func(context.Context, string, any) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	}
	srv := relayTestServer(t, func() (dockerruntime.CommandSender, bool) { return sender, true })
	src := relaySource(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := src.Runtime(ctx, 7)
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if _, err := rt.Ping(ctx); err != nil {
		t.Fatalf("first Ping: %v", err)
	}

	srv.CloseClientConnections()
	// The next resolution notices the dead connection and dials a fresh one —
	// the worker survives an api restart without its own restart.
	deadline := time.Now().Add(3 * time.Second)
	for {
		rt, err = src.Runtime(ctx, 7)
		if err == nil {
			if _, err = rt.Ping(ctx); err == nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay never recovered: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRelayBridgesAttach proves the bidirectional stream survives the second
// hop: a worker-side exec attach echoes through the relay, the bridge and
// the (scripted) live channel.
func TestRelayBridgesAttach(t *testing.T) {
	sender := &scriptedSender{
		CommandFn: func(_ context.Context, method string, _ any) (json.RawMessage, error) {
			if method == agentwire.MethodContainerExecCreate {
				return json.RawMessage(`{"Id":"e1"}`), nil
			}
			return json.RawMessage(`{}`), nil
		},
		AttachFn: func(context.Context, string, any) (dockerruntime.AttachStream, error) {
			pr, pw := io.Pipe()
			return &loopbackAttach{pr: pr, pw: pw}, nil
		},
	}
	srv := relayTestServer(t, func() (dockerruntime.CommandSender, bool) { return sender, true })
	src := relaySource(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := src.Runtime(ctx, 7)
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	hijack, err := rt.ContainerExecAttach(ctx, "e1", containertypes.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("ContainerExecAttach: %v", err)
	}
	defer hijack.Close()
	if _, err := hijack.Conn.Write([]byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(hijack.Reader, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo across the relay = %q, %v", buf, err)
	}
	if err := hijack.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if rest, err := io.ReadAll(hijack.Reader); err != nil || len(rest) != 0 {
		t.Fatalf("after CloseWrite: %q, %v — want a clean EOF", rest, err)
	}
}

// loopbackAttach echoes its input back as output; CloseWrite ends it.
type loopbackAttach struct {
	pr *io.PipeReader
	pw *io.PipeWriter
}

func (l *loopbackAttach) Read(p []byte) (int, error)  { return l.pr.Read(p) }
func (l *loopbackAttach) Write(p []byte) (int, error) { return l.pw.Write(p) }
func (l *loopbackAttach) CloseWrite() error           { return l.pw.Close() }
func (l *loopbackAttach) Close() error                { _ = l.pw.Close(); return l.pr.Close() }
