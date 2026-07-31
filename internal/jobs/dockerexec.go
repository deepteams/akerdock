// One-shot container exec through the agent channel (ADR-052): the typed
// `docker exec <c> sh -c '…' 2>&1` — output merged in arrival order, exit
// code from the exec inspect once the stream ends.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

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

// ensureNetwork creates the destination network when absent — the typed
// `docker network inspect || docker network create` with the managed label.
func ensureNetwork(ctx context.Context, rt dockerruntime.Runtime, name string) error {
	if _, err := rt.NetworkInspect(ctx, name, network.InspectOptions{}); err == nil {
		return nil
	} else if !dockerruntime.IsNotFound(err) {
		return err
	}
	_, err := rt.NetworkCreate(ctx, name, network.CreateOptions{
		Labels: map[string]string{"akerdock.managed": "true"},
	})
	if err != nil && dockerruntime.IsConflict(err) {
		return nil // created concurrently
	}
	return err
}

// ensureVolume creates a labelled volume when absent — the typed
// `docker volume inspect || docker volume create`.
func ensureVolume(ctx context.Context, rt dockerruntime.Runtime, name string, labels map[string]string) error {
	if _, err := rt.VolumeInspect(ctx, name); err == nil {
		return nil
	} else if !dockerruntime.IsNotFound(err) {
		return err
	}
	_, err := rt.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels})
	return err
}

// runOneShot runs a throwaway container to completion — the typed
// `docker run --rm`: create, start, wait, remove. A non-zero exit reports as
// an error with the container's output attached.
func runOneShot(ctx context.Context, rt dockerruntime.Runtime, cfg *container.Config, host *container.HostConfig) error {
	created, err := rt.ContainerCreate(ctx, cfg, host, nil, nil, "")
	if err != nil {
		return err
	}
	defer func() {
		_ = rt.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
	}()
	if err := rt.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return err
	}
	waitCh, errCh := rt.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	case st := <-waitCh:
		if st.StatusCode != 0 {
			detail := ""
			if out, lerr := containerLogsTail(ctx, rt, created.ID, 50); lerr == nil && out != "" {
				detail = ": " + firstLine(out)
			}
			return fmt.Errorf("one-shot container exited with code %d%s", st.StatusCode, detail)
		}
		return nil
	}
}

// streamPullProgress renders a pull's JSON progress stream as step-log lines
// — one line per layer status transition, and the stream's own error
// surfaced (the daemon reports pull failures IN the stream, not on the
// call).
func streamPullProgress(rc io.Reader, onOutput func(string)) error {
	dec := json.NewDecoder(rc)
	last := ""
	for {
		var m struct {
			Status string `json:"status"`
			ID     string `json:"id"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if m.Error != "" {
			return fmt.Errorf("%s", m.Error)
		}
		line := m.Status
		if m.ID != "" {
			line = m.ID + ": " + line
		}
		if line != "" && line != last {
			onOutput(line + "\n")
			last = line
		}
	}
}
