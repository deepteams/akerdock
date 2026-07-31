// Package fake is the test double for dockerruntime.Runtime: a typed recorder.
// Every call lands in Calls with its arguments, so tests assert on structs
// (image, env, labels, stop timeout) instead of substring-matching shell
// strings — the pattern replacing the sshexec-era command-string assertions.
//
// Behavior is scripted per method through the *Fn fields. A nil Fn on a
// method that only returns an error succeeds silently (the common case for
// mutations); a nil Fn on a method that returns a value panics, so a test
// exercising a read path must say what the daemon would answer.
package fake

import (
	"context"
	"io"
	"sync"

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

	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// Call is one recorded invocation: the method name and its arguments in
// declaration order (ctx excluded).
type Call struct {
	Method string
	Args   []any
}

// Runtime records calls and plays scripted responses.
type Runtime struct {
	mu    sync.Mutex
	calls []Call

	ContainerCreateFn      func(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStartFn       func(ctx context.Context, name string, options container.StartOptions) error
	ContainerStopFn        func(ctx context.Context, name string, options container.StopOptions) error
	ContainerRestartFn     func(ctx context.Context, name string, options container.StopOptions) error
	ContainerRenameFn      func(ctx context.Context, name, newName string) error
	ContainerRemoveFn      func(ctx context.Context, name string, options container.RemoveOptions) error
	ContainerInspectFn     func(ctx context.Context, name string) (container.InspectResponse, error)
	ContainerWaitFn        func(ctx context.Context, name string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerListFn        func(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerLogsFn        func(ctx context.Context, name string, options container.LogsOptions) (io.ReadCloser, error)
	ContainerStatsFn       func(ctx context.Context, name string) (container.StatsResponseReader, error)
	ContainersPruneFn      func(ctx context.Context, pruneFilters filters.Args) (container.PruneReport, error)
	ContainerExecCreateFn  func(ctx context.Context, name string, options container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecStartFn   func(ctx context.Context, execID string, options container.ExecStartOptions) error
	ContainerExecAttachFn  func(ctx context.Context, execID string, options container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspectFn func(ctx context.Context, execID string) (container.ExecInspect, error)
	ContainerExecResizeFn  func(ctx context.Context, execID string, options container.ResizeOptions) error
	ImagePullFn            func(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	ImagePushFn            func(ctx context.Context, ref string, options image.PushOptions) (io.ReadCloser, error)
	ImageTagFn             func(ctx context.Context, img, ref string) error
	ImageInspectFn         func(ctx context.Context, img string, options ...client.ImageInspectOption) (image.InspectResponse, error)
	ImageListFn            func(ctx context.Context, options image.ListOptions) ([]image.Summary, error)
	ImageRemoveFn          func(ctx context.Context, img string, options image.RemoveOptions) ([]image.DeleteResponse, error)
	ImagesPruneFn          func(ctx context.Context, pruneFilter filters.Args) (image.PruneReport, error)
	VolumeCreateFn         func(ctx context.Context, options volume.CreateOptions) (volume.Volume, error)
	VolumeInspectFn        func(ctx context.Context, volumeID string) (volume.Volume, error)
	VolumeListFn           func(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error)
	VolumeRemoveFn         func(ctx context.Context, volumeID string, force bool) error
	VolumesPruneFn         func(ctx context.Context, pruneFilter filters.Args) (volume.PruneReport, error)
	NetworkCreateFn        func(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkConnectFn       func(ctx context.Context, networkID, name string, config *network.EndpointSettings) error
	NetworkDisconnectFn    func(ctx context.Context, networkID, name string, force bool) error
	NetworkInspectFn       func(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error)
	NetworkListFn          func(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
	NetworkRemoveFn        func(ctx context.Context, networkID string) error
	NetworksPruneFn        func(ctx context.Context, pruneFilter filters.Args) (network.PruneReport, error)
	EventsFn               func(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
	InfoFn                 func(ctx context.Context) (system.Info, error)
	ServerVersionFn        func(ctx context.Context) (types.Version, error)
	DiskUsageFn            func(ctx context.Context, options types.DiskUsageOptions) (types.DiskUsage, error)
	RegistryLoginFn        func(ctx context.Context, auth registry.AuthConfig) (registry.AuthenticateOKBody, error)
	PingFn                 func(ctx context.Context) (types.Ping, error)
	CloseFn                func() error
}

var _ dockerruntime.Runtime = (*Runtime)(nil)

// Calls returns the recorded invocations in order.
func (f *Runtime) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// CallNames returns just the method names, in order — the quick shape
// assertion ("Remove then Create then Start").
func (f *Runtime) CallNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, len(f.calls))
	for i, c := range f.calls {
		names[i] = c.Method
	}
	return names
}

func (f *Runtime) record(method string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: method, Args: args})
}

func (f *Runtime) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error) {
	f.record("ContainerCreate", config, hostConfig, networkingConfig, platform, containerName)
	if f.ContainerCreateFn != nil {
		return f.ContainerCreateFn(ctx, config, hostConfig, networkingConfig, platform, containerName)
	}
	return container.CreateResponse{}, nil
}

func (f *Runtime) ContainerStart(ctx context.Context, name string, options container.StartOptions) error {
	f.record("ContainerStart", name, options)
	if f.ContainerStartFn != nil {
		return f.ContainerStartFn(ctx, name, options)
	}
	return nil
}

func (f *Runtime) ContainerStop(ctx context.Context, name string, options container.StopOptions) error {
	f.record("ContainerStop", name, options)
	if f.ContainerStopFn != nil {
		return f.ContainerStopFn(ctx, name, options)
	}
	return nil
}

func (f *Runtime) ContainerRestart(ctx context.Context, name string, options container.StopOptions) error {
	f.record("ContainerRestart", name, options)
	if f.ContainerRestartFn != nil {
		return f.ContainerRestartFn(ctx, name, options)
	}
	return nil
}

func (f *Runtime) ContainerRename(ctx context.Context, name, newName string) error {
	f.record("ContainerRename", name, newName)
	if f.ContainerRenameFn != nil {
		return f.ContainerRenameFn(ctx, name, newName)
	}
	return nil
}

func (f *Runtime) ContainerRemove(ctx context.Context, name string, options container.RemoveOptions) error {
	f.record("ContainerRemove", name, options)
	if f.ContainerRemoveFn != nil {
		return f.ContainerRemoveFn(ctx, name, options)
	}
	return nil
}

func (f *Runtime) ContainerInspect(ctx context.Context, name string) (container.InspectResponse, error) {
	f.record("ContainerInspect", name)
	if f.ContainerInspectFn == nil {
		panic("fake: ContainerInspectFn not set")
	}
	return f.ContainerInspectFn(ctx, name)
}

func (f *Runtime) ContainerWait(ctx context.Context, name string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	f.record("ContainerWait", name, condition)
	if f.ContainerWaitFn == nil {
		panic("fake: ContainerWaitFn not set")
	}
	return f.ContainerWaitFn(ctx, name, condition)
}

func (f *Runtime) ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error) {
	f.record("ContainerList", options)
	if f.ContainerListFn == nil {
		panic("fake: ContainerListFn not set")
	}
	return f.ContainerListFn(ctx, options)
}

func (f *Runtime) ContainerLogs(ctx context.Context, name string, options container.LogsOptions) (io.ReadCloser, error) {
	f.record("ContainerLogs", name, options)
	if f.ContainerLogsFn == nil {
		panic("fake: ContainerLogsFn not set")
	}
	return f.ContainerLogsFn(ctx, name, options)
}

func (f *Runtime) ContainerStatsOneShot(ctx context.Context, name string) (container.StatsResponseReader, error) {
	f.record("ContainerStatsOneShot", name)
	if f.ContainerStatsFn == nil {
		panic("fake: ContainerStatsFn not set")
	}
	return f.ContainerStatsFn(ctx, name)
}

func (f *Runtime) ContainersPrune(ctx context.Context, pruneFilters filters.Args) (container.PruneReport, error) {
	f.record("ContainersPrune", pruneFilters)
	if f.ContainersPruneFn != nil {
		return f.ContainersPruneFn(ctx, pruneFilters)
	}
	return container.PruneReport{}, nil
}

func (f *Runtime) ContainerExecCreate(ctx context.Context, name string, options container.ExecOptions) (container.ExecCreateResponse, error) {
	f.record("ContainerExecCreate", name, options)
	if f.ContainerExecCreateFn != nil {
		return f.ContainerExecCreateFn(ctx, name, options)
	}
	return container.ExecCreateResponse{}, nil
}

func (f *Runtime) ContainerExecStart(ctx context.Context, execID string, options container.ExecStartOptions) error {
	f.record("ContainerExecStart", execID, options)
	if f.ContainerExecStartFn != nil {
		return f.ContainerExecStartFn(ctx, execID, options)
	}
	return nil
}

func (f *Runtime) ContainerExecAttach(ctx context.Context, execID string, options container.ExecAttachOptions) (types.HijackedResponse, error) {
	f.record("ContainerExecAttach", execID, options)
	if f.ContainerExecAttachFn == nil {
		panic("fake: ContainerExecAttachFn not set")
	}
	return f.ContainerExecAttachFn(ctx, execID, options)
}

func (f *Runtime) ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error) {
	f.record("ContainerExecInspect", execID)
	if f.ContainerExecInspectFn == nil {
		panic("fake: ContainerExecInspectFn not set")
	}
	return f.ContainerExecInspectFn(ctx, execID)
}

func (f *Runtime) ContainerExecResize(ctx context.Context, execID string, options container.ResizeOptions) error {
	f.record("ContainerExecResize", execID, options)
	if f.ContainerExecResizeFn != nil {
		return f.ContainerExecResizeFn(ctx, execID, options)
	}
	return nil
}

func (f *Runtime) ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error) {
	f.record("ImagePull", ref, options)
	if f.ImagePullFn == nil {
		panic("fake: ImagePullFn not set")
	}
	return f.ImagePullFn(ctx, ref, options)
}

func (f *Runtime) ImagePush(ctx context.Context, ref string, options image.PushOptions) (io.ReadCloser, error) {
	f.record("ImagePush", ref, options)
	if f.ImagePushFn == nil {
		panic("fake: ImagePushFn not set")
	}
	return f.ImagePushFn(ctx, ref, options)
}

func (f *Runtime) ImageTag(ctx context.Context, img, ref string) error {
	f.record("ImageTag", img, ref)
	if f.ImageTagFn != nil {
		return f.ImageTagFn(ctx, img, ref)
	}
	return nil
}

func (f *Runtime) ImageInspect(ctx context.Context, img string, options ...client.ImageInspectOption) (image.InspectResponse, error) {
	f.record("ImageInspect", img)
	if f.ImageInspectFn == nil {
		panic("fake: ImageInspectFn not set")
	}
	return f.ImageInspectFn(ctx, img, options...)
}

func (f *Runtime) ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error) {
	f.record("ImageList", options)
	if f.ImageListFn == nil {
		panic("fake: ImageListFn not set")
	}
	return f.ImageListFn(ctx, options)
}

func (f *Runtime) ImageRemove(ctx context.Context, img string, options image.RemoveOptions) ([]image.DeleteResponse, error) {
	f.record("ImageRemove", img, options)
	if f.ImageRemoveFn != nil {
		return f.ImageRemoveFn(ctx, img, options)
	}
	return nil, nil
}

func (f *Runtime) ImagesPrune(ctx context.Context, pruneFilter filters.Args) (image.PruneReport, error) {
	f.record("ImagesPrune", pruneFilter)
	if f.ImagesPruneFn != nil {
		return f.ImagesPruneFn(ctx, pruneFilter)
	}
	return image.PruneReport{}, nil
}

func (f *Runtime) VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error) {
	f.record("VolumeCreate", options)
	if f.VolumeCreateFn != nil {
		return f.VolumeCreateFn(ctx, options)
	}
	return volume.Volume{}, nil
}

func (f *Runtime) VolumeInspect(ctx context.Context, volumeID string) (volume.Volume, error) {
	f.record("VolumeInspect", volumeID)
	if f.VolumeInspectFn == nil {
		panic("fake: VolumeInspectFn not set")
	}
	return f.VolumeInspectFn(ctx, volumeID)
}

func (f *Runtime) VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error) {
	f.record("VolumeList", options)
	if f.VolumeListFn == nil {
		panic("fake: VolumeListFn not set")
	}
	return f.VolumeListFn(ctx, options)
}

func (f *Runtime) VolumeRemove(ctx context.Context, volumeID string, force bool) error {
	f.record("VolumeRemove", volumeID, force)
	if f.VolumeRemoveFn != nil {
		return f.VolumeRemoveFn(ctx, volumeID, force)
	}
	return nil
}

func (f *Runtime) VolumesPrune(ctx context.Context, pruneFilter filters.Args) (volume.PruneReport, error) {
	f.record("VolumesPrune", pruneFilter)
	if f.VolumesPruneFn != nil {
		return f.VolumesPruneFn(ctx, pruneFilter)
	}
	return volume.PruneReport{}, nil
}

func (f *Runtime) NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error) {
	f.record("NetworkCreate", name, options)
	if f.NetworkCreateFn != nil {
		return f.NetworkCreateFn(ctx, name, options)
	}
	return network.CreateResponse{}, nil
}

func (f *Runtime) NetworkConnect(ctx context.Context, networkID, name string, config *network.EndpointSettings) error {
	f.record("NetworkConnect", networkID, name, config)
	if f.NetworkConnectFn != nil {
		return f.NetworkConnectFn(ctx, networkID, name, config)
	}
	return nil
}

func (f *Runtime) NetworkDisconnect(ctx context.Context, networkID, name string, force bool) error {
	f.record("NetworkDisconnect", networkID, name, force)
	if f.NetworkDisconnectFn != nil {
		return f.NetworkDisconnectFn(ctx, networkID, name, force)
	}
	return nil
}

func (f *Runtime) NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error) {
	f.record("NetworkInspect", networkID, options)
	if f.NetworkInspectFn == nil {
		panic("fake: NetworkInspectFn not set")
	}
	return f.NetworkInspectFn(ctx, networkID, options)
}

func (f *Runtime) NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error) {
	f.record("NetworkList", options)
	if f.NetworkListFn == nil {
		panic("fake: NetworkListFn not set")
	}
	return f.NetworkListFn(ctx, options)
}

func (f *Runtime) NetworkRemove(ctx context.Context, networkID string) error {
	f.record("NetworkRemove", networkID)
	if f.NetworkRemoveFn != nil {
		return f.NetworkRemoveFn(ctx, networkID)
	}
	return nil
}

func (f *Runtime) NetworksPrune(ctx context.Context, pruneFilter filters.Args) (network.PruneReport, error) {
	f.record("NetworksPrune", pruneFilter)
	if f.NetworksPruneFn != nil {
		return f.NetworksPruneFn(ctx, pruneFilter)
	}
	return network.PruneReport{}, nil
}

func (f *Runtime) Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error) {
	f.record("Events", options)
	if f.EventsFn == nil {
		panic("fake: EventsFn not set")
	}
	return f.EventsFn(ctx, options)
}

func (f *Runtime) Info(ctx context.Context) (system.Info, error) {
	f.record("Info")
	if f.InfoFn == nil {
		panic("fake: InfoFn not set")
	}
	return f.InfoFn(ctx)
}

func (f *Runtime) ServerVersion(ctx context.Context) (types.Version, error) {
	f.record("ServerVersion")
	if f.ServerVersionFn == nil {
		panic("fake: ServerVersionFn not set")
	}
	return f.ServerVersionFn(ctx)
}

func (f *Runtime) DiskUsage(ctx context.Context, options types.DiskUsageOptions) (types.DiskUsage, error) {
	f.record("DiskUsage", options)
	if f.DiskUsageFn == nil {
		panic("fake: DiskUsageFn not set")
	}
	return f.DiskUsageFn(ctx, options)
}

func (f *Runtime) RegistryLogin(ctx context.Context, auth registry.AuthConfig) (registry.AuthenticateOKBody, error) {
	f.record("RegistryLogin", auth)
	if f.RegistryLoginFn != nil {
		return f.RegistryLoginFn(ctx, auth)
	}
	return registry.AuthenticateOKBody{}, nil
}

func (f *Runtime) Ping(ctx context.Context) (types.Ping, error) {
	f.record("Ping")
	if f.PingFn != nil {
		return f.PingFn(ctx)
	}
	return types.Ping{}, nil
}

func (f *Runtime) Close() error {
	f.record("Close")
	if f.CloseFn != nil {
		return f.CloseFn()
	}
	return nil
}
