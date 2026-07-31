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

// Host-ops vocabulary (ADR-054): file primitives the agent executes in pure
// Go against the bind-mounted /var/lib/akerdock tree — the helper image is
// distroless, so there is no shell to fall back on, and every path is
// validated against that root before it is touched.
const (
	MethodFileWrite  = "FileWrite"
	MethodFileRead   = "FileRead"
	MethodFileRemove = "FileRemove"
	MethodFileStat   = "FileStat"
	MethodFileChown  = "FileChown"
	MethodFileCopy   = "FileCopy"
	MethodDirEnsure  = "DirEnsure"
)

// Pipe vocabulary (ADR-054 tranche C): bulk transfers the agent executes
// LOCALLY — container exec ↔ host file with compression, host file ↔
// presigned URL — so a multi-gigabyte dump never crosses the control plane.
// Each is a single long-running unary command; only the typed verdict (exit
// code, size, digest, output tail) travels back.
const (
	MethodExecToFile = "ExecToFile"
	MethodFileToExec = "FileToExec"
	MethodFileToURL  = "FileToURL"
	MethodURLToFile  = "URLToFile"
	MethodFileHash   = "FileHash"
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

// Host-ops params and results (ADR-054). Modes travel as the numeric
// os.FileMode value; content as JSON base64 — these files are key material,
// configs and manifests, kilobytes not gigabytes (bulk payloads are the pipe
// primitives' job, a later tranche).

type FileWriteParams struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
	// Mode is applied explicitly after the write — the agent's umask never
	// decides what a key file ends up world-readable as.
	Mode uint32 `json:"mode"`
	// MakeDirs creates the missing parents with DirMode first.
	MakeDirs bool   `json:"make_dirs,omitempty"`
	DirMode  uint32 `json:"dir_mode,omitempty"`
	// Atomic stages the content next to Path and renames it into place, so a
	// concurrent reader (the proxy, the waker) never sees a partial file.
	Atomic bool `json:"atomic,omitempty"`
}

type FileReadParams struct {
	Path string `json:"path"`
	// MaxBytes bounds what travels back; 0 means the executor's default cap.
	MaxBytes int64 `json:"max_bytes,omitempty"`
}

type FileReadResult struct {
	Content []byte `json:"content,omitempty"`
	// Found is false for a missing file — absence is data here (an activity
	// file not yet written, an ACME store not yet initialized), not an error.
	Found     bool `json:"found"`
	Truncated bool `json:"truncated,omitempty"`
}

type FileRemoveParams struct {
	Path string `json:"path"`
	// Recursive removes a whole tree; either way an absent path is a no-op.
	Recursive bool `json:"recursive,omitempty"`
}

type FileStatParams struct {
	Path string `json:"path"`
}

type FileStatResult struct {
	Found bool  `json:"found"`
	IsDir bool  `json:"is_dir,omitempty"`
	Size  int64 `json:"size,omitempty"`
}

type FileChownParams struct {
	Path string `json:"path"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
}

type FileCopyParams struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

type DirEnsureParams struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}

// ExecToFileParams runs Cmd in Container and streams its stdout to Path —
// gzipped when Gzip is set. The digest and size describe the file as
// written (compressed), so a later FileHash comparison is byte-exact.
type ExecToFileParams struct {
	Container string   `json:"container"`
	Cmd       []string `json:"cmd"`
	Path      string   `json:"path"`
	Mode      uint32   `json:"mode"`
	MakeDirs  bool     `json:"make_dirs,omitempty"`
	DirMode   uint32   `json:"dir_mode,omitempty"`
	Gzip      bool     `json:"gzip,omitempty"`
}

type ExecToFileResult struct {
	ExitCode  int    `json:"exit_code"`
	Stderr    string `json:"stderr,omitempty"` // tail — the diagnostic, not the payload
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// FileToExecParams streams Path — gunzipped when Gunzip is set — into the
// stdin of Cmd run in Container.
type FileToExecParams struct {
	Path      string   `json:"path"`
	Gunzip    bool     `json:"gunzip,omitempty"`
	Container string   `json:"container"`
	Cmd       []string `json:"cmd"`
}

type FileToExecResult struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"` // merged tail
}

// FileToURLParams uploads Path to URL with a plain PUT. The URL is presigned
// by the control plane and travels in this body over the encrypted channel —
// never argv, never a process list (INV-003).
type FileToURLParams struct {
	Path    string            `json:"path"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// URLToFileParams downloads URL into Path.
type URLToFileParams struct {
	URL      string `json:"url"`
	Path     string `json:"path"`
	Mode     uint32 `json:"mode"`
	MakeDirs bool   `json:"make_dirs,omitempty"`
	DirMode  uint32 `json:"dir_mode,omitempty"`
}

type FileHashParams struct {
	Path string `json:"path"`
}

type FileHashResult struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}
