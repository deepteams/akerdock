package jobs

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"testing"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
)

// TestExecCaptureMergesOutputAndReadsTheExitCode pins the one-shot exec
// contract: `sh -c` argv, both streams attached and merged in arrival order,
// exit code from the inspect once the stream ends.
func TestExecCaptureMergesOutputAndReadsTheExitCode(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	rt := &fake.Runtime{}
	rt.ContainerExecCreateFn = func(_ context.Context, name string, opts containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
		if name != "abc" || len(opts.Cmd) != 3 || opts.Cmd[0] != "sh" || opts.Cmd[2] != "echo hi" {
			t.Errorf("exec create = %q %v", name, opts.Cmd)
		}
		if !opts.AttachStdout || !opts.AttachStderr {
			t.Error("both output streams must be attached")
		}
		return containertypes.ExecCreateResponse{ID: "e1"}, nil
	}
	rt.ContainerExecAttachFn = func(context.Context, string, containertypes.ExecAttachOptions) (types.HijackedResponse, error) {
		return types.HijackedResponse{Conn: clientSide, Reader: bufio.NewReader(clientSide)}, nil
	}
	rt.ContainerExecInspectFn = func(_ context.Context, execID string) (containertypes.ExecInspect, error) {
		if execID != "e1" {
			t.Errorf("inspect exec = %q", execID)
		}
		return containertypes.ExecInspect{ExitCode: 3}, nil
	}
	go func() {
		var buf bytes.Buffer
		_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stdout).Write([]byte("out "))
		_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stderr).Write([]byte("err"))
		_, _ = serverSide.Write(buf.Bytes())
		_ = serverSide.Close() // exec ended: the daemon closes the stream
	}()

	output, exit, err := execCapture(context.Background(), rt, "abc", []string{"sh", "-c", "echo hi"})
	if err != nil {
		t.Fatalf("execCapture: %v", err)
	}
	if output != "out err" || exit != 3 {
		t.Fatalf("output = %q exit = %d", output, exit)
	}
}
