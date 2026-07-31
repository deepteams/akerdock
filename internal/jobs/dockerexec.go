// One-shot container exec through the agent channel (ADR-052): the typed
// `docker exec <c> sh -c '…' 2>&1` — output merged in arrival order, exit
// code from the exec inspect once the stream ends.
package jobs

import (
	"context"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"

	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// execCapture runs cmd inside the container and returns its merged output
// and exit code. The caller bounds the run with ctx — a canceled attach
// surfaces as an error, never as a fabricated exit code.
func execCapture(ctx context.Context, rt dockerruntime.Runtime, containerName string, cmd []string) (string, int, error) {
	created, err := rt.ContainerExecCreate(ctx, containerName, container.ExecOptions{
		Cmd: cmd, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return "", 0, err
	}
	att, err := rt.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", 0, err
	}
	defer att.Close()
	var sb strings.Builder
	if err := dockerruntime.Demux(att.Reader, false, func(chunk string) { sb.WriteString(chunk) }); err != nil {
		return sb.String(), 0, err
	}
	inspect, err := rt.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return sb.String(), 0, err
	}
	return sb.String(), inspect.ExitCode, nil
}

// containerLogsTail reads a container's last lines through the agent channel
// — stdout and stderr merged, the diagnostic attached when a wait times out.
func containerLogsTail(ctx context.Context, rt dockerruntime.Runtime, containerName string, lines int) (string, error) {
	inspect, err := rt.ContainerInspect(ctx, containerName)
	if err != nil {
		return "", err
	}
	tty := inspect.Config != nil && inspect.Config.Tty
	rc, err := rt.ContainerLogs(ctx, containerName, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: strconv.Itoa(lines),
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	var sb strings.Builder
	if err := dockerruntime.Demux(rc, tty, func(chunk string) { sb.WriteString(chunk) }); err != nil {
		return "", err
	}
	return sb.String(), nil
}
