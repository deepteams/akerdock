package waker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/hostops"
)

// frameSink collects everything the executor sends back on the channel.
type frameSink struct {
	mu     sync.Mutex
	frames []agentwire.Frame
}

func (s *frameSink) send(f agentwire.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, f)
	return nil
}

func (s *frameSink) all() []agentwire.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentwire.Frame(nil), s.frames...)
}

func mustParams(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestExecutorUnaryCommandRunsAndAnswers(t *testing.T) {
	rt := &fake.Runtime{}
	e := NewExecutor(rt, nil, nil)
	sink := &frameSink{}

	e.Execute(context.Background(), agentwire.Command{
		ID: 1, Method: agentwire.MethodContainerStart,
		Params: mustParams(t, agentwire.ContainerStartParams{Name: "akerdock-app"}),
	}, sink.send)

	frames := sink.all()
	if len(frames) != 1 || frames[0].Type != agentwire.FrameResult || frames[0].Res.ID != 1 || frames[0].Res.Err != nil {
		t.Fatalf("frames = %+v", frames)
	}
	if calls := rt.CallNames(); len(calls) != 1 || calls[0] != "ContainerStart" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestExecutorUnaryResultCarriesTheBody(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "abc123"}}, nil
	}
	e := NewExecutor(rt, nil, nil)
	sink := &frameSink{}

	e.Execute(context.Background(), agentwire.Command{
		ID: 2, Method: agentwire.MethodContainerInspect,
		Params: mustParams(t, agentwire.NameParams{Name: "akerdock-app"}),
	}, sink.send)

	frames := sink.all()
	if len(frames) != 1 || frames[0].Res == nil {
		t.Fatalf("frames = %+v", frames)
	}
	var resp container.InspectResponse
	if err := json.Unmarshal(frames[0].Res.Body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != "abc123" {
		t.Fatalf("inspect body = %+v", resp)
	}
}

func TestExecutorMapsTypedErrors(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerStartFn = func(context.Context, string, container.StartOptions) error {
		return fmt.Errorf("no such container: %w", cerrdefs.ErrNotFound)
	}
	e := NewExecutor(rt, nil, nil)
	sink := &frameSink{}

	e.Execute(context.Background(), agentwire.Command{
		ID: 3, Method: agentwire.MethodContainerStart,
		Params: mustParams(t, agentwire.ContainerStartParams{Name: "gone"}),
	}, sink.send)

	res := sink.all()[0].Res
	if res.Err == nil || res.Err.Code != agentwire.CodeNotFound {
		t.Fatalf("error = %+v, want not_found", res.Err)
	}
}

func TestExecutorRefusesUnknownMethodsAndBadParams(t *testing.T) {
	e := NewExecutor(&fake.Runtime{}, nil, nil)
	sink := &frameSink{}

	e.Execute(context.Background(), agentwire.Command{ID: 4, Method: "DeleteEverything"}, sink.send)
	e.Execute(context.Background(), agentwire.Command{
		ID: 5, Method: agentwire.MethodContainerStart, Params: json.RawMessage(`{broken`),
	}, sink.send)

	frames := sink.all()
	if frames[0].Res.Err == nil || frames[0].Res.Err.Code != agentwire.CodeUnimplemented {
		t.Fatalf("unknown method = %+v, want unimplemented", frames[0].Res.Err)
	}
	if frames[1].Res.Err == nil || frames[1].Res.Err.Code != agentwire.CodeInvalid {
		t.Fatalf("bad params = %+v, want invalid", frames[1].Res.Err)
	}
}

func TestExecutorStreamsLogsAsAckThenChunksThenEOF(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerLogsFn = func(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("line one\nline two\n")), nil
	}
	e := NewExecutor(rt, nil, nil)
	sink := &frameSink{}

	e.Execute(context.Background(), agentwire.Command{
		ID: 6, Method: agentwire.MethodContainerLogs,
		Params: mustParams(t, agentwire.ContainerLogsParams{Name: "akerdock-app"}),
	}, sink.send)

	frames := sink.all()
	if frames[0].Type != agentwire.FrameResult || frames[0].Res.Err != nil {
		t.Fatalf("stream open = %+v", frames[0])
	}
	var payload strings.Builder
	last := frames[len(frames)-1]
	for _, f := range frames[1:] {
		if f.Type != agentwire.FrameStream {
			t.Fatalf("unexpected frame %+v", f)
		}
		payload.Write(f.Chunk.Data)
	}
	if payload.String() != "line one\nline two\n" {
		t.Fatalf("stream payload = %q", payload.String())
	}
	if !last.Chunk.EOF || last.Chunk.Err != nil {
		t.Fatalf("stream end = %+v, want clean EOF", last.Chunk)
	}
}

func TestExecutorCancelAbortsALongCommand(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerWaitFn = func(ctx context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		waitCh := make(chan container.WaitResponse)
		errCh := make(chan error, 1)
		go func() {
			<-ctx.Done() // never resolves on its own
			errCh <- ctx.Err()
		}()
		return waitCh, errCh
	}
	e := NewExecutor(rt, nil, nil)
	sink := &frameSink{}

	done := make(chan struct{})
	go func() {
		e.Execute(context.Background(), agentwire.Command{
			ID: 7, Method: agentwire.MethodContainerWait,
			Params: mustParams(t, agentwire.ContainerWaitParams{Name: "c", Condition: container.WaitConditionNotRunning}),
		}, sink.send)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for len(sink.all()) == 0 {
		e.Cancel(7)
		select {
		case <-deadline:
			t.Fatal("cancel never unblocked the command")
		case <-time.After(5 * time.Millisecond):
		}
	}
	<-done
	res := sink.all()[0].Res
	if res == nil || res.Err == nil || res.Err.Code != agentwire.CodeCanceled {
		t.Fatalf("result = %+v, want canceled", res)
	}
}

// TestExecutorHostOpsRoundTrip pins the ADR-054 dispatch: a FileWrite lands
// on the mounted tree and a FileRead answers with the typed result — the
// same guard-bearing Local the agent runs in production, rooted for the test.
func TestExecutorHostOpsRoundTrip(t *testing.T) {
	root := t.TempDir()
	e := NewExecutor(&fake.Runtime{}, &hostops.Local{Root: root}, nil)
	sink := &frameSink{}

	e.Execute(context.Background(), agentwire.Command{
		ID: 1, Method: agentwire.MethodFileWrite,
		Params: mustParams(t, agentwire.FileWriteParams{
			Path: root + "/proxy/dynamic/app.yaml", Content: []byte("routing"),
			Mode: 0o600, MakeDirs: true, Atomic: true,
		}),
	}, sink.send)
	e.Execute(context.Background(), agentwire.Command{
		ID: 2, Method: agentwire.MethodFileRead,
		Params: mustParams(t, agentwire.FileReadParams{Path: root + "/proxy/dynamic/app.yaml"}),
	}, sink.send)

	frames := sink.all()
	if len(frames) != 2 || frames[0].Res.Err != nil || frames[1].Res.Err != nil {
		t.Fatalf("frames = %+v", frames)
	}
	var res agentwire.FileReadResult
	if err := json.Unmarshal(frames[1].Res.Body, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Found || string(res.Content) != "routing" {
		t.Fatalf("read = %+v", res)
	}
}

// TestExecutorHostOpsWithoutMountAnswerUnavailable pins the pre-spec-7
// contract: a helper without the host tree answers a typed unavailability the
// control plane can distinguish from a failed operation.
func TestExecutorHostOpsWithoutMountAnswerUnavailable(t *testing.T) {
	e := NewExecutor(&fake.Runtime{}, nil, nil)
	sink := &frameSink{}
	e.Execute(context.Background(), agentwire.Command{
		ID: 1, Method: agentwire.MethodFileStat,
		Params: mustParams(t, agentwire.FileStatParams{Path: "/var/lib/akerdock/x"}),
	}, sink.send)
	frames := sink.all()
	if len(frames) != 1 || frames[0].Res.Err == nil {
		t.Fatalf("frames = %+v", frames)
	}
	if err := frames[0].Res.Err.Err(); !cerrdefs.IsUnavailable(err) {
		t.Fatalf("error = %v, want unavailable", err)
	}
}

// TestExecutorHostOpsGuardTravelsTyped pins that the agent-side path guard
// crosses the wire as invalid-argument.
func TestExecutorHostOpsGuardTravelsTyped(t *testing.T) {
	e := NewExecutor(&fake.Runtime{}, &hostops.Local{Root: t.TempDir()}, nil)
	sink := &frameSink{}
	e.Execute(context.Background(), agentwire.Command{
		ID: 1, Method: agentwire.MethodFileRemove,
		Params: mustParams(t, agentwire.FileRemoveParams{Path: "/etc/passwd", Recursive: true}),
	}, sink.send)
	frames := sink.all()
	if len(frames) != 1 || frames[0].Res.Err == nil {
		t.Fatalf("frames = %+v", frames)
	}
	if err := frames[0].Res.Err.Err(); !cerrdefs.IsInvalidArgument(err) {
		t.Fatalf("error = %v, want the guard's invalid-argument", err)
	}
}
