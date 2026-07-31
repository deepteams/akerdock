package agentwire

import (
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The command vocabulary: one name per dockerruntime.Runtime method carried
// over the channel. The executor refuses anything outside this list, and each
// name is what audit and telemetry record.
const (
	// MethodContainerExecAttach is the one BIDIRECTIONAL stream: after the
	// acknowledging result, output flows as chunks one way and input chunks
	// travel the other way under the same command id — an input chunk with
	// EOF closes the exec's stdin without ending the output.
	MethodContainerExecAttach = "ContainerExecAttach"
)

const (
	MethodContainerCreate      = "ContainerCreate"
	MethodContainerStart       = "ContainerStart"
	MethodContainerStop        = "ContainerStop"
	MethodContainerRestart     = "ContainerRestart"
	MethodContainerRename      = "ContainerRename"
	MethodContainerRemove      = "ContainerRemove"
	MethodContainerInspect     = "ContainerInspect"
	MethodContainerWait        = "ContainerWait"
	MethodContainerList        = "ContainerList"
	MethodContainerLogs        = "ContainerLogs" // stream
	MethodContainerStats       = "ContainerStats"
	MethodContainersPrune      = "ContainersPrune"
	MethodContainerExecCreate  = "ContainerExecCreate"
	MethodContainerExecStart   = "ContainerExecStart"
	MethodContainerExecInspect = "ContainerExecInspect"
	MethodContainerExecResize  = "ContainerExecResize"
	MethodImagePull            = "ImagePull" // stream
	MethodImagePush            = "ImagePush" // stream
	MethodImageTag             = "ImageTag"
	MethodImageInspect         = "ImageInspect"
	MethodImageList            = "ImageList"
	MethodImageRemove          = "ImageRemove"
	MethodImagesPrune          = "ImagesPrune"
	MethodVolumeCreate         = "VolumeCreate"
	MethodVolumeInspect        = "VolumeInspect"
	MethodVolumeList           = "VolumeList"
	MethodVolumeRemove         = "VolumeRemove"
	MethodVolumesPrune         = "VolumesPrune"
	MethodNetworkCreate        = "NetworkCreate"
	MethodNetworkConnect       = "NetworkConnect"
	MethodNetworkDisconnect    = "NetworkDisconnect"
	MethodNetworkInspect       = "NetworkInspect"
	MethodNetworkList          = "NetworkList"
	MethodNetworkRemove        = "NetworkRemove"
	MethodNetworksPrune        = "NetworksPrune"
	MethodEvents               = "Events" // stream
	MethodInfo                 = "Info"
	MethodServerVersion        = "ServerVersion"
	MethodDiskUsage            = "DiskUsage"
	MethodRegistryLogin        = "RegistryLogin"
	MethodPing                 = "Ping"
)

// Params structs, one per method. Fields are the SDK types — the Engine API's
// own wire representations (filters.Args carries its custom JSON both ways).
// Methods without params (Info, Ping, …) send none.

type ContainerCreateParams struct {
	Config           *container.Config         `json:"config"`
	HostConfig       *container.HostConfig     `json:"host_config,omitempty"`
	NetworkingConfig *network.NetworkingConfig `json:"networking_config,omitempty"`
	Platform         *ocispec.Platform         `json:"platform,omitempty"`
	Name             string                    `json:"name"`
}

type ContainerStartParams struct {
	Name    string                 `json:"name"`
	Options container.StartOptions `json:"options"`
}

type ContainerStopParams struct {
	Name    string                `json:"name"`
	Options container.StopOptions `json:"options"`
}

type ContainerRenameParams struct {
	Name    string `json:"name"`
	NewName string `json:"new_name"`
}

type ContainerRemoveParams struct {
	Name    string                  `json:"name"`
	Options container.RemoveOptions `json:"options"`
}

// NameParams serves every method whose only parameter is the object's name or
// id: ContainerInspect, ContainerStatsOneShot, VolumeInspect, ImageInspect,
// NetworkRemove, …
type NameParams struct {
	Name string `json:"name"`
}

type ContainerWaitParams struct {
	Name      string                  `json:"name"`
	Condition container.WaitCondition `json:"condition"`
}

type ContainerListParams struct {
	Options container.ListOptions `json:"options"`
}

type ContainerLogsParams struct {
	Name    string                `json:"name"`
	Options container.LogsOptions `json:"options"`
}

// StatsResult carries the one-shot stats snapshot: the daemon's OS type and
// the raw JSON body, re-wrapped into a StatsResponseReader on the caller side.
type StatsResult struct {
	OSType string `json:"os_type"`
	Body   []byte `json:"body"`
}

// PruneParams serves every *sPrune method.
type PruneParams struct {
	Filters filters.Args `json:"filters"`
}

type ContainerExecCreateParams struct {
	Name    string                `json:"name"`
	Options container.ExecOptions `json:"options"`
}

type ContainerExecStartParams struct {
	ExecID  string                     `json:"exec_id"`
	Options container.ExecStartOptions `json:"options"`
}

type ContainerExecAttachParams struct {
	ExecID  string                      `json:"exec_id"`
	Options container.ExecAttachOptions `json:"options"`
}

type ContainerExecResizeParams struct {
	ExecID  string                  `json:"exec_id"`
	Options container.ResizeOptions `json:"options"`
}

type ImagePullParams struct {
	Ref     string            `json:"ref"`
	Options image.PullOptions `json:"options"`
}

type ImagePushParams struct {
	Ref     string            `json:"ref"`
	Options image.PushOptions `json:"options"`
}

type ImageTagParams struct {
	Image string `json:"image"`
	Ref   string `json:"ref"`
}

type ImageListParams struct {
	Options image.ListOptions `json:"options"`
}

type ImageRemoveParams struct {
	Image   string              `json:"image"`
	Options image.RemoveOptions `json:"options"`
}

type VolumeCreateParams struct {
	Options volume.CreateOptions `json:"options"`
}

type VolumeListParams struct {
	Options volume.ListOptions `json:"options"`
}

type VolumeRemoveParams struct {
	Name  string `json:"name"`
	Force bool   `json:"force"`
}

type NetworkCreateParams struct {
	Name    string                `json:"name"`
	Options network.CreateOptions `json:"options"`
}

type NetworkConnectParams struct {
	Network   string                    `json:"network"`
	Container string                    `json:"container"`
	Config    *network.EndpointSettings `json:"config,omitempty"`
}

type NetworkDisconnectParams struct {
	Network   string `json:"network"`
	Container string `json:"container"`
	Force     bool   `json:"force"`
}

type NetworkInspectParams struct {
	Network string                 `json:"network"`
	Options network.InspectOptions `json:"options"`
}

type NetworkListParams struct {
	Options network.ListOptions `json:"options"`
}

type EventsParams struct {
	Options events.ListOptions `json:"options"`
}

type DiskUsageParams struct {
	Options types.DiskUsageOptions `json:"options"`
}

type RegistryLoginParams struct {
	Auth registry.AuthConfig `json:"auth"`
}
