package waker

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
)

// TestExecutorAttachBridgesBothDirections pins the bidirectional command:
// input chunks land on the exec's stdin, its output pumps out as chunks, and
// the daemon closing the stream ends with a clean EOF.
func TestExecutorAttachBridgesBothDirections(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	rt := &fake.Runtime{}
	rt.ContainerExecAttachFn = func(_ context.Context, execID string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
		if execID != "e1" {
			t.Errorf("attach exec = %q", execID)
		}
		return types.HijackedResponse{Conn: clientSide, Reader: bufio.NewReader(clientSide)}, nil
	}
	e := NewExecutor(rt, nil, nil)
	frames := make(chan agentwire.Frame, 16)
	send := func(f agentwire.Frame) error { frames <- f; return nil }

	done := make(chan struct{})
	go func() {
		e.Execute(context.Background(), agentwire.Command{
			ID: 9, Method: agentwire.MethodContainerExecAttach,
			Params: mustParams(t, agentwire.ContainerExecAttachParams{ExecID: "e1"}),
		}, send)
		close(done)
	}()

	next := func() agentwire.Frame {
		select {
		case f := <-frames:
			return f
		case <-time.After(2 * time.Second):
			t.Fatal("no frame")
			return agentwire.Frame{}
		}
	}
	if f := next(); f.Type != agentwire.FrameResult || f.Res.ID != 9 || f.Res.Err != nil {
		t.Fatalf("attach ack = %+v", f)
	}

	// Input chunk → the exec's stdin (the pipe's server side reads it).
	stdin := make(chan string, 1)
	go func() {
		buf := make([]byte, 16)
		n, _ := serverSide.Read(buf)
		stdin <- string(buf[:n])
	}()
	e.DeliverInput(&agentwire.StreamChunk{ID: 9, Data: []byte("stdin!")})
	select {
	case got := <-stdin:
		if got != "stdin!" {
			t.Fatalf("stdin = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("input never reached the exec")
	}

	// Output → chunk frames; daemon close → clean EOF and Execute returns.
	if _, err := serverSide.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if f := next(); f.Type != agentwire.FrameStream || string(f.Chunk.Data) != "hello" {
		t.Fatalf("output frame = %+v", f)
	}
	_ = serverSide.Close()
	if f := next(); f.Type != agentwire.FrameStream || !f.Chunk.EOF || f.Chunk.Err != nil {
		t.Fatalf("end frame = %+v", f)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Execute never returned after the stream ended")
	}
	// The session is gone: late input is dropped, not misrouted.
	e.DeliverInput(&agentwire.StreamChunk{ID: 9, Data: []byte("late")})
}
