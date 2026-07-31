package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	cerrdefs "github.com/containerd/errdefs"

	"github.com/deepteams/akerdock/internal/agentwire"
)

// dialPair builds a live CP↔agent v2 channel: the returned agentConn is the
// control-plane side (its route loop mirrors AgentChannel's v2 branch), the
// returned conn plays the agent.
func dialPair(t *testing.T) (*agentConn, *websocket.Conn) {
	t.Helper()
	acCh := make(chan *agentConn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentwire.SubprotocolV2}})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		ac := newAgentConn(ctx, conn)
		acCh <- ac
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var f agentwire.Frame
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			switch f.Type {
			case agentwire.FrameResult:
				ac.DeliverResult(f.Res)
			case agentwire.FrameStream:
				ac.DeliverChunk(f.Chunk)
			}
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	agent, _, err := websocket.Dial(ctx, strings.Replace(srv.URL, "http", "ws", 1), &websocket.DialOptions{
		Subprotocols: []string{agentwire.SubprotocolV2},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(websocket.StatusNormalClosure, "") })
	agent.SetReadLimit(1 << 20)
	return <-acCh, agent
}

// readCommand reads the next command frame the agent receives. It runs in
// scripted-agent goroutines, so failures return instead of t.Fatal — the test
// side surfaces them as assertion failures or timeouts.
func readCommand(agent *websocket.Conn) (agentwire.Command, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, data, err := agent.Read(ctx)
		if err != nil {
			return agentwire.Command{}, err
		}
		var f agentwire.Frame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		if f.Type == agentwire.FrameCommand && f.Cmd != nil {
			return *f.Cmd, nil
		}
	}
}

func agentWrite(agent *websocket.Conn, f agentwire.Frame) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return agent.Write(ctx, websocket.MessageText, data)
}

func TestAgentConnCommandRoundTrip(t *testing.T) {
	ac, agent := dialPair(t)

	go func() {
		cmd, err := readCommand(agent)
		if err != nil {
			return
		}
		res := &agentwire.Result{ID: cmd.ID, Body: json.RawMessage(`{"APIVersion":"1.45"}`)}
		if cmd.Method != agentwire.MethodPing {
			res = &agentwire.Result{ID: cmd.ID, Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "wrong method"}}
		}
		_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameResult, Res: res})
	}()

	body, err := ac.Command(context.Background(), agentwire.MethodPing, nil)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if !strings.Contains(string(body), "1.45") {
		t.Fatalf("body = %s", body)
	}
}

func TestAgentConnCommandKeepsTypedErrors(t *testing.T) {
	ac, agent := dialPair(t)

	go func() {
		cmd, err := readCommand(agent)
		if err != nil {
			return
		}
		_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{
			ID: cmd.ID, Err: &agentwire.Error{Code: agentwire.CodeNotFound, Message: "no such container"},
		}})
	}()

	_, err := ac.Command(context.Background(), agentwire.MethodContainerInspect, agentwire.NameParams{Name: "gone"})
	if !cerrdefs.IsNotFound(err) {
		t.Fatalf("error = %v, want IsNotFound across the channel", err)
	}
}

func TestAgentConnStreamDeliversChunksThenEOF(t *testing.T) {
	ac, agent := dialPair(t)

	go func() {
		cmd, err := readCommand(agent)
		if err != nil {
			return
		}
		_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID}})
		_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: cmd.ID, Data: []byte("hello ")}})
		_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: cmd.ID, Data: []byte("world")}})
		_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: cmd.ID, EOF: true}})
	}()

	rc, err := ac.Stream(context.Background(), agentwire.MethodContainerLogs, agentwire.ContainerLogsParams{Name: "c"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = rc.Close() }()
	all, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(all) != "hello world" {
		t.Fatalf("stream = %q", all)
	}
}

func TestAgentConnStreamOpenErrorIsTyped(t *testing.T) {
	ac, agent := dialPair(t)

	go func() {
		cmd, err := readCommand(agent)
		if err != nil {
			return
		}
		_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{
			ID: cmd.ID, Err: &agentwire.Error{Code: agentwire.CodeNotFound, Message: "no such container"},
		}})
	}()

	_, err := ac.Stream(context.Background(), agentwire.MethodContainerLogs, agentwire.ContainerLogsParams{Name: "gone"})
	if !cerrdefs.IsNotFound(err) {
		t.Fatalf("open error = %v, want IsNotFound", err)
	}
}

func TestAgentConnCanceledCallTellsTheAgent(t *testing.T) {
	ac, agent := dialPair(t)

	gotCancel := make(chan int64, 1)
	go func() {
		cmd, err := readCommand(agent)
		if err != nil {
			return
		}
		// Never answer; wait for the cancel frame instead.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for {
			_, data, err := agent.Read(ctx)
			if err != nil {
				return
			}
			var f agentwire.Frame
			if json.Unmarshal(data, &f) == nil && f.Type == agentwire.FrameCancel && f.Cancel == cmd.ID {
				gotCancel <- f.Cancel
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := ac.Command(ctx, agentwire.MethodPing, nil)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("Command = %v, want ctx expiry", err)
	}
	select {
	case <-gotCancel:
	case <-time.After(2 * time.Second):
		t.Fatal("the agent never received the cancel frame")
	}
}

func TestAgentConnClosedChannelIsUnavailable(t *testing.T) {
	ac, agent := dialPair(t)
	_ = agent.Close(websocket.StatusNormalClosure, "agent restarting")

	// The handler ctx dies with the connection; wait for it to propagate.
	select {
	case <-ac.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("connection close never reached the handler ctx")
	}

	_, err := ac.Command(context.Background(), agentwire.MethodPing, nil)
	if !cerrdefs.IsUnavailable(err) {
		t.Fatalf("error = %v, want IsUnavailable (the mandatory-agent failure mode)", err)
	}
}

func TestAgentConnsLatestConnectionWins(t *testing.T) {
	var r AgentConns
	c1 := newAgentConn(context.Background(), nil)
	c2 := newAgentConn(context.Background(), nil)
	r.register(7, c1)
	r.register(7, c2)
	r.unregister(7, c1) // the replaced handler exiting must not evict the newcomer
	if s, ok := r.Sender(7); !ok || s.(*agentConn) != c2 {
		t.Fatalf("sender = %v, %v", s, ok)
	}
	r.unregister(7, c2)
	if _, ok := r.Sender(7); ok {
		t.Fatal("sender must be gone after its own unregister")
	}
	if _, err := r.Runtime(context.Background(), 7); !cerrdefs.IsUnavailable(err) {
		t.Fatalf("Runtime without a channel = %v, want IsUnavailable", err)
	}
}

// TestAgentConnAttachEchoesBothWays pins the bidirectional stream on the CP
// side: writes travel as input chunks, output chunks come back through Read,
// CloseWrite marks stdin closed without ending the reads.
func TestAgentConnAttachEchoesBothWays(t *testing.T) {
	ac, agent := dialPair(t)

	go func() {
		cmd, err := readCommand(agent)
		if err != nil || cmd.Method != agentwire.MethodContainerExecAttach {
			return
		}
		_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID}})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for {
			_, data, err := agent.Read(ctx)
			if err != nil {
				return
			}
			var f agentwire.Frame
			if json.Unmarshal(data, &f) != nil || f.Type != agentwire.FrameStream || f.Chunk == nil || f.Chunk.ID != cmd.ID {
				continue
			}
			if len(f.Chunk.Data) > 0 { // echo the input back as output
				_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: cmd.ID, Data: f.Chunk.Data}})
			}
			if f.Chunk.EOF { // stdin closed: the exec ends, output EOFs
				_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: cmd.ID, EOF: true}})
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	att, err := ac.Attach(ctx, agentwire.MethodContainerExecAttach, agentwire.ContainerExecAttachParams{ExecID: "e1"})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer func() { _ = att.Close() }()
	if _, err := att.Write([]byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(att, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	if err := att.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if rest, err := io.ReadAll(att); err != nil || len(rest) != 0 {
		t.Fatalf("after CloseWrite: %q, %v — want a clean EOF", rest, err)
	}
}

// TestRuntimeSurvivesAgentReconnect pins the dynamic resolution: a runtime
// resolved from the registry keeps working across an agent reconnect — the
// call after the reconnect rides the FRESH channel, never the corpse the
// caller started with (the failure mode: a deployment holding one runtime
// for minutes dies on "write failed" after a harmless blip).
func TestRuntimeSurvivesAgentReconnect(t *testing.T) {
	var r AgentConns
	ac1, agent1 := dialPair(t)
	r.register(9, ac1)

	rt, err := r.Runtime(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	answerPing := func(agent *websocket.Conn, version string) {
		go func() {
			cmd, err := readCommand(agent)
			if err != nil || cmd.Method != agentwire.MethodPing {
				return
			}
			_ = agentWrite(agent, agentwire.Frame{
				Type: agentwire.FrameResult,
				Res:  &agentwire.Result{ID: cmd.ID, Body: json.RawMessage(`{"APIVersion":"` + version + `"}`)},
			})
		}()
	}
	answerPing(agent1, "1.45")
	if p, err := rt.Ping(context.Background()); err != nil || p.APIVersion != "1.45" {
		t.Fatalf("first ping = %+v, %v", p, err)
	}

	// The agent reconnects: a new channel replaces the old, whose handler
	// unregisters on its way out.
	ac2, agent2 := dialPair(t)
	r.register(9, ac2)
	r.unregister(9, ac1)
	_ = agent1.Close(websocket.StatusNormalClosure, "")

	answerPing(agent2, "1.46")
	if p, err := rt.Ping(context.Background()); err != nil || p.APIVersion != "1.46" {
		t.Fatalf("ping after reconnect = %+v, %v — the SAME runtime must ride the fresh channel", p, err)
	}

	// Fully gone: the same runtime answers unavailable, not a hang.
	r.unregister(9, ac2)
	if _, err := rt.Ping(context.Background()); !cerrdefs.IsUnavailable(err) {
		t.Fatalf("ping without any channel = %v, want IsUnavailable", err)
	}
}
