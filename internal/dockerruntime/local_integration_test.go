//go:build dockerintegration

package dockerruntime

// Integration tier (`make test-docker`): exercises the SDK implementation
// against the machine's real daemon — status-code mapping, stream demux, the
// behaviors a fake cannot vouch for. Jobs and handlers stay on the fake; only
// this package earns the real socket.

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
)

const integrationImage = "alpine:3"

func integrationRuntime(t *testing.T) *Local {
	t.Helper()
	rt, err := NewLocal("", "")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rt.Ping(ctx); err != nil {
		t.Skipf("no local Docker daemon: %v", err)
	}
	return rt
}

func TestLocalContainerRoundTrip(t *testing.T) {
	rt := integrationRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pull, err := rt.ImagePull(ctx, integrationImage, image.PullOptions{})
	if err != nil {
		t.Fatalf("ImagePull: %v", err)
	}
	_, _ = io.Copy(io.Discard, pull) // the pull completes when the progress stream ends
	_ = pull.Close()

	const name = "akerdock-dockerruntime-it"
	_ = rt.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}) // leftover from a broken run

	created, err := rt.ContainerCreate(ctx, &container.Config{
		Image: integrationImage,
		Cmd:   []string{"sh", "-c", "echo out-line; echo err-line >&2"},
	}, nil, nil, nil, name)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	defer func() { _ = rt.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true}) }()

	if err := rt.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	waitCh, errCh := rt.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		t.Fatalf("ContainerWait: %v", err)
	case st := <-waitCh:
		if st.StatusCode != 0 {
			t.Fatalf("exit code = %d", st.StatusCode)
		}
	}

	logs, err := rt.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	defer func() { _ = logs.Close() }()
	var merged strings.Builder
	if err := Demux(logs, false, func(chunk string) { merged.WriteString(chunk) }); err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if !strings.Contains(merged.String(), "out-line") || !strings.Contains(merged.String(), "err-line") {
		t.Fatalf("logs = %q, want both streams demuxed", merged.String())
	}

	if err := rt.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}
	if _, err := rt.ContainerInspect(ctx, created.ID); !IsNotFound(err) {
		t.Fatalf("inspect after remove = %v, want IsNotFound", err)
	}
}

func TestLocalTypedErrors(t *testing.T) {
	rt := integrationRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := rt.ContainerInspect(ctx, "akerdock-definitely-absent")
	if !IsNotFound(err) {
		t.Fatalf("inspect absent = %v, want IsNotFound", err)
	}
	if err := rt.ContainerRemove(ctx, "akerdock-definitely-absent", container.RemoveOptions{Force: true}); err != nil && !IsNotFound(err) {
		t.Fatalf("remove absent = %v, want nil or IsNotFound (the `|| true` contract)", err)
	}
}
