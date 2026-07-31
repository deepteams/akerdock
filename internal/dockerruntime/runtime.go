// Package dockerruntime is the single Docker runtime adapter (PRD §18.1,
// ADR-004, ADR-051): every Docker Engine API call AkerDock makes — from the
// control plane's jobs and handlers or from the server agent — goes through
// the Runtime interface. Business logic never talks to a transport, a shell
// or the SDK client directly, so where an operation executes (the agent's
// local socket today, the typed command channel of ADR-052 tomorrow) is
// decided entirely by which implementation a caller is handed (ADR-001).
//
// The method set is the strict subset of the Engine API the codebase uses.
// Deliberately absent: ImageBuild — build contexts live on the target server
// (git clone on the host), so builds are driven server-side (BuildKit via the
// agent, ADR-051 §scope) and never through this interface.
package dockerruntime

import (
	"context"
	"io"

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
)

// Runtime is the Engine API surface AkerDock relies on. Signatures mirror the
// official SDK client verbatim so the local implementation is the SDK itself
// and a typed command frame (ADR-052) maps 1:1 onto a method — the SDK types
// ARE the Engine API wire types.
//
// Error discipline: implementations surface the SDK's typed errors; callers
// branch with IsNotFound/IsConflict/IsNotModified from this package, never by
// matching message text. Streaming returns (io.ReadCloser, hijacked
// connections, channels) are bounded by the caller's ctx — implementations
// must not impose a global timeout, which would kill a follow stream
// mid-flight.
type Runtime interface {
	// Containers.
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, container string, options container.StartOptions) error
	ContainerStop(ctx context.Context, container string, options container.StopOptions) error
	ContainerRestart(ctx context.Context, container string, options container.StopOptions) error
	ContainerRename(ctx context.Context, container, newContainerName string) error
	ContainerRemove(ctx context.Context, container string, options container.RemoveOptions) error
	ContainerInspect(ctx context.Context, container string) (container.InspectResponse, error)
	ContainerWait(ctx context.Context, container string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerLogs(ctx context.Context, container string, options container.LogsOptions) (io.ReadCloser, error)
	// ContainerStats with stream=false is the metrics snapshot: unlike the
	// one-shot variant, it fills precpu_stats, without which CPU% cannot be
	// computed (ADR-034). stream=true is not carried by the agent channel.
	ContainerStats(ctx context.Context, container string, stream bool) (container.StatsResponseReader, error)
	ContainersPrune(ctx context.Context, pruneFilters filters.Args) (container.PruneReport, error)

	// Exec — one-shot (ContainerExecStart) and attached/interactive
	// (ContainerExecAttach returns the hijacked bidirectional stream; resize
	// serves the container terminal's PTY).
	ContainerExecCreate(ctx context.Context, container string, options container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecStart(ctx context.Context, execID string, options container.ExecStartOptions) error
	ContainerExecAttach(ctx context.Context, execID string, options container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
	ContainerExecResize(ctx context.Context, execID string, options container.ResizeOptions) error

	// Images. Pull/push authenticate per request (options.RegistryAuth) —
	// nothing is persisted in the host's docker config for API-path
	// operations, unlike the CLI login/logout dance.
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	ImagePush(ctx context.Context, ref string, options image.PushOptions) (io.ReadCloser, error)
	ImageTag(ctx context.Context, image, ref string) error
	ImageInspect(ctx context.Context, image string, options ...client.ImageInspectOption) (image.InspectResponse, error)
	ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error)
	ImageRemove(ctx context.Context, image string, options image.RemoveOptions) ([]image.DeleteResponse, error)
	ImagesPrune(ctx context.Context, pruneFilter filters.Args) (image.PruneReport, error)

	// Volumes.
	VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error)
	VolumeInspect(ctx context.Context, volumeID string) (volume.Volume, error)
	VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
	VolumesPrune(ctx context.Context, pruneFilter filters.Args) (volume.PruneReport, error)

	// Networks.
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkConnect(ctx context.Context, network, container string, config *network.EndpointSettings) error
	NetworkDisconnect(ctx context.Context, network, container string, force bool) error
	NetworkInspect(ctx context.Context, network string, options network.InspectOptions) (network.Inspect, error)
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
	NetworkRemove(ctx context.Context, network string) error
	NetworksPrune(ctx context.Context, pruneFilter filters.Args) (network.PruneReport, error)

	// System.
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
	Info(ctx context.Context) (system.Info, error)
	ServerVersion(ctx context.Context) (types.Version, error)
	DiskUsage(ctx context.Context, options types.DiskUsageOptions) (types.DiskUsage, error)
	RegistryLogin(ctx context.Context, auth registry.AuthConfig) (registry.AuthenticateOKBody, error)
	Ping(ctx context.Context) (types.Ping, error)

	Close() error
}
