package dockerruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/deepteams/akerdock/internal/agentwire"
)

// CommandSender is the control-plane handle on one server's agent channel
// (ADR-052): it carries a typed command and returns its result, or opens the
// command's output stream. internal/handlers implements it on the live
// WebSocket; this package never sees the transport.
type CommandSender interface {
	Command(ctx context.Context, method string, params any) (json.RawMessage, error)
	Stream(ctx context.Context, method string, params any) (io.ReadCloser, error)
}

// Source resolves the Runtime executing on a given server — the seam job and
// handler code depends on. The api process serves it from its live channel
// registry; a worker or scheduler serves it through the api relay
// (ADR-052 §8). An unreachable agent answers an IsUnavailable error.
type Source interface {
	Runtime(ctx context.Context, serverID int64) (Runtime, error)
}

// NewAgentRuntime returns the Runtime that executes every call as a typed
// command on the server's agent channel. The agent is mandatory (ADR-051):
// there is no fallback below this — a dead channel surfaces as an
// IsUnavailable error and the caller's remedy is repairing the agent.
func NewAgentRuntime(s CommandSender) Runtime {
	return &agentRuntime{s: s}
}

type agentRuntime struct {
	s CommandSender
}

var _ Runtime = (*agentRuntime)(nil)

// call sends a unary command and unmarshals its result body.
func call[T any](ctx context.Context, s CommandSender, method string, params any) (T, error) {
	var out T
	raw, err := s.Command(ctx, method, params)
	if err != nil {
		return out, err
	}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("agent %s result: %w", method, err)
	}
	return out, nil
}

// do sends a unary command whose only result is success or failure.
func do(ctx context.Context, s CommandSender, method string, params any) error {
	_, err := s.Command(ctx, method, params)
	return err
}

func (r *agentRuntime) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error) {
	return call[container.CreateResponse](ctx, r.s, agentwire.MethodContainerCreate, agentwire.ContainerCreateParams{
		Config: config, HostConfig: hostConfig, NetworkingConfig: networkingConfig, Platform: platform, Name: containerName,
	})
}

func (r *agentRuntime) ContainerStart(ctx context.Context, name string, options container.StartOptions) error {
	return do(ctx, r.s, agentwire.MethodContainerStart, agentwire.ContainerStartParams{Name: name, Options: options})
}

func (r *agentRuntime) ContainerStop(ctx context.Context, name string, options container.StopOptions) error {
	return do(ctx, r.s, agentwire.MethodContainerStop, agentwire.ContainerStopParams{Name: name, Options: options})
}

func (r *agentRuntime) ContainerRestart(ctx context.Context, name string, options container.StopOptions) error {
	return do(ctx, r.s, agentwire.MethodContainerRestart, agentwire.ContainerStopParams{Name: name, Options: options})
}

func (r *agentRuntime) ContainerRename(ctx context.Context, name, newName string) error {
	return do(ctx, r.s, agentwire.MethodContainerRename, agentwire.ContainerRenameParams{Name: name, NewName: newName})
}

func (r *agentRuntime) ContainerRemove(ctx context.Context, name string, options container.RemoveOptions) error {
	return do(ctx, r.s, agentwire.MethodContainerRemove, agentwire.ContainerRemoveParams{Name: name, Options: options})
}

func (r *agentRuntime) ContainerInspect(ctx context.Context, name string) (container.InspectResponse, error) {
	return call[container.InspectResponse](ctx, r.s, agentwire.MethodContainerInspect, agentwire.NameParams{Name: name})
}

// ContainerWait sends the wait as one long-lived command: the agent answers
// when the condition is met, and the channels mirror the SDK's contract.
func (r *agentRuntime) ContainerWait(ctx context.Context, name string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	waitCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := call[container.WaitResponse](ctx, r.s, agentwire.MethodContainerWait,
			agentwire.ContainerWaitParams{Name: name, Condition: condition})
		if err != nil {
			errCh <- err
			return
		}
		waitCh <- resp
	}()
	return waitCh, errCh
}

func (r *agentRuntime) ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error) {
	return call[[]container.Summary](ctx, r.s, agentwire.MethodContainerList, agentwire.ContainerListParams{Options: options})
}

func (r *agentRuntime) ContainerLogs(ctx context.Context, name string, options container.LogsOptions) (io.ReadCloser, error) {
	return r.s.Stream(ctx, agentwire.MethodContainerLogs, agentwire.ContainerLogsParams{Name: name, Options: options})
}

func (r *agentRuntime) ContainerStats(ctx context.Context, name string, stream bool) (container.StatsResponseReader, error) {
	if stream {
		return container.StatsResponseReader{}, fmt.Errorf("streamed stats over the agent channel: %w", cerrdefs.ErrNotImplemented)
	}
	res, err := call[agentwire.StatsResult](ctx, r.s, agentwire.MethodContainerStats, agentwire.NameParams{Name: name})
	if err != nil {
		return container.StatsResponseReader{}, err
	}
	return container.StatsResponseReader{
		Body:   io.NopCloser(bytes.NewReader(res.Body)),
		OSType: res.OSType,
	}, nil
}

func (r *agentRuntime) ContainersPrune(ctx context.Context, pruneFilters filters.Args) (container.PruneReport, error) {
	return call[container.PruneReport](ctx, r.s, agentwire.MethodContainersPrune, agentwire.PruneParams{Filters: pruneFilters})
}

func (r *agentRuntime) ContainerExecCreate(ctx context.Context, name string, options container.ExecOptions) (container.ExecCreateResponse, error) {
	return call[container.ExecCreateResponse](ctx, r.s, agentwire.MethodContainerExecCreate, agentwire.ContainerExecCreateParams{Name: name, Options: options})
}

func (r *agentRuntime) ContainerExecStart(ctx context.Context, execID string, options container.ExecStartOptions) error {
	return do(ctx, r.s, agentwire.MethodContainerExecStart, agentwire.ContainerExecStartParams{ExecID: execID, Options: options})
}

// ContainerExecAttach is the hijacked bidirectional terminal stream — not yet
// carried by the channel; the terminal slice will design it (ADR-052 scope).
func (r *agentRuntime) ContainerExecAttach(context.Context, string, container.ExecAttachOptions) (types.HijackedResponse, error) {
	return types.HijackedResponse{}, fmt.Errorf("exec attach over the agent channel: %w", cerrdefs.ErrNotImplemented)
}

func (r *agentRuntime) ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error) {
	return call[container.ExecInspect](ctx, r.s, agentwire.MethodContainerExecInspect, agentwire.NameParams{Name: execID})
}

func (r *agentRuntime) ContainerExecResize(ctx context.Context, execID string, options container.ResizeOptions) error {
	return do(ctx, r.s, agentwire.MethodContainerExecResize, agentwire.ContainerExecResizeParams{ExecID: execID, Options: options})
}

func (r *agentRuntime) ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error) {
	return r.s.Stream(ctx, agentwire.MethodImagePull, agentwire.ImagePullParams{Ref: ref, Options: options})
}

func (r *agentRuntime) ImagePush(ctx context.Context, ref string, options image.PushOptions) (io.ReadCloser, error) {
	return r.s.Stream(ctx, agentwire.MethodImagePush, agentwire.ImagePushParams{Ref: ref, Options: options})
}

func (r *agentRuntime) ImageTag(ctx context.Context, img, ref string) error {
	return do(ctx, r.s, agentwire.MethodImageTag, agentwire.ImageTagParams{Image: img, Ref: ref})
}

func (r *agentRuntime) ImageInspect(ctx context.Context, img string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
	return call[image.InspectResponse](ctx, r.s, agentwire.MethodImageInspect, agentwire.NameParams{Name: img})
}

func (r *agentRuntime) ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error) {
	return call[[]image.Summary](ctx, r.s, agentwire.MethodImageList, agentwire.ImageListParams{Options: options})
}

func (r *agentRuntime) ImageRemove(ctx context.Context, img string, options image.RemoveOptions) ([]image.DeleteResponse, error) {
	return call[[]image.DeleteResponse](ctx, r.s, agentwire.MethodImageRemove, agentwire.ImageRemoveParams{Image: img, Options: options})
}

func (r *agentRuntime) ImagesPrune(ctx context.Context, pruneFilter filters.Args) (image.PruneReport, error) {
	return call[image.PruneReport](ctx, r.s, agentwire.MethodImagesPrune, agentwire.PruneParams{Filters: pruneFilter})
}

func (r *agentRuntime) VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error) {
	return call[volume.Volume](ctx, r.s, agentwire.MethodVolumeCreate, agentwire.VolumeCreateParams{Options: options})
}

func (r *agentRuntime) VolumeInspect(ctx context.Context, volumeID string) (volume.Volume, error) {
	return call[volume.Volume](ctx, r.s, agentwire.MethodVolumeInspect, agentwire.NameParams{Name: volumeID})
}

func (r *agentRuntime) VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error) {
	return call[volume.ListResponse](ctx, r.s, agentwire.MethodVolumeList, agentwire.VolumeListParams{Options: options})
}

func (r *agentRuntime) VolumeRemove(ctx context.Context, volumeID string, force bool) error {
	return do(ctx, r.s, agentwire.MethodVolumeRemove, agentwire.VolumeRemoveParams{Name: volumeID, Force: force})
}

func (r *agentRuntime) VolumesPrune(ctx context.Context, pruneFilter filters.Args) (volume.PruneReport, error) {
	return call[volume.PruneReport](ctx, r.s, agentwire.MethodVolumesPrune, agentwire.PruneParams{Filters: pruneFilter})
}

func (r *agentRuntime) NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error) {
	return call[network.CreateResponse](ctx, r.s, agentwire.MethodNetworkCreate, agentwire.NetworkCreateParams{Name: name, Options: options})
}

func (r *agentRuntime) NetworkConnect(ctx context.Context, networkID, containerName string, config *network.EndpointSettings) error {
	return do(ctx, r.s, agentwire.MethodNetworkConnect, agentwire.NetworkConnectParams{Network: networkID, Container: containerName, Config: config})
}

func (r *agentRuntime) NetworkDisconnect(ctx context.Context, networkID, containerName string, force bool) error {
	return do(ctx, r.s, agentwire.MethodNetworkDisconnect, agentwire.NetworkDisconnectParams{Network: networkID, Container: containerName, Force: force})
}

func (r *agentRuntime) NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error) {
	return call[network.Inspect](ctx, r.s, agentwire.MethodNetworkInspect, agentwire.NetworkInspectParams{Network: networkID, Options: options})
}

func (r *agentRuntime) NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error) {
	return call[[]network.Summary](ctx, r.s, agentwire.MethodNetworkList, agentwire.NetworkListParams{Options: options})
}

func (r *agentRuntime) NetworkRemove(ctx context.Context, networkID string) error {
	return do(ctx, r.s, agentwire.MethodNetworkRemove, agentwire.NameParams{Name: networkID})
}

func (r *agentRuntime) NetworksPrune(ctx context.Context, pruneFilter filters.Args) (network.PruneReport, error) {
	return call[network.PruneReport](ctx, r.s, agentwire.MethodNetworksPrune, agentwire.PruneParams{Filters: pruneFilter})
}

// Events opens the daemon's event stream through the channel: each chunk is
// one events.Message, decoded back into the SDK's channel contract.
func (r *agentRuntime) Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error) {
	msgCh := make(chan events.Message)
	errCh := make(chan error, 1)
	go func() {
		rc, err := r.s.Stream(ctx, agentwire.MethodEvents, agentwire.EventsParams{Options: options})
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = rc.Close() }()
		dec := json.NewDecoder(rc)
		for {
			var m events.Message
			if err := dec.Decode(&m); err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF // the SDK never ends this stream cleanly
				}
				errCh <- err
				return
			}
			select {
			case msgCh <- m:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
	}()
	return msgCh, errCh
}

func (r *agentRuntime) Info(ctx context.Context) (system.Info, error) {
	return call[system.Info](ctx, r.s, agentwire.MethodInfo, nil)
}

func (r *agentRuntime) ServerVersion(ctx context.Context) (types.Version, error) {
	return call[types.Version](ctx, r.s, agentwire.MethodServerVersion, nil)
}

func (r *agentRuntime) DiskUsage(ctx context.Context, options types.DiskUsageOptions) (types.DiskUsage, error) {
	return call[types.DiskUsage](ctx, r.s, agentwire.MethodDiskUsage, agentwire.DiskUsageParams{Options: options})
}

func (r *agentRuntime) RegistryLogin(ctx context.Context, auth registry.AuthConfig) (registry.AuthenticateOKBody, error) {
	return call[registry.AuthenticateOKBody](ctx, r.s, agentwire.MethodRegistryLogin, agentwire.RegistryLoginParams{Auth: auth})
}

func (r *agentRuntime) Ping(ctx context.Context) (types.Ping, error) {
	return call[types.Ping](ctx, r.s, agentwire.MethodPing, nil)
}

// Close releases nothing: the channel belongs to the connection handler, not
// to any single runtime borrowed from it.
func (r *agentRuntime) Close() error { return nil }
