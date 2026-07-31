// Executor (ADR-052): the agent side of the typed command channel. Each
// command names one dockerruntime.Runtime method and carries its SDK-typed
// params; the executor unmarshals, calls the local daemon, and answers with a
// result or stream chunks. It decides nothing — policy, ordering and retries
// stay on the control plane (ADR-001) — and it executes nothing outside the
// enumerated vocabulary.
package waker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/hostops"
)

// Executor runs typed commands against the local runtime.
type Executor struct {
	rt     dockerruntime.Runtime
	logger *slog.Logger
	// host executes the ADR-054 file primitives on the bind-mounted tree; nil
	// when this helper predates the mount (spec < 7), in which case host-ops
	// answer unavailable until the control plane recreates the container.
	host *hostops.Local

	mu       sync.Mutex
	inflight map[int64]context.CancelFunc
	// attaches are the hijacked exec streams in flight, keyed by command id —
	// where the control plane's input chunks land (DeliverInput).
	attaches map[int64]*types.HijackedResponse
}

// NewExecutor builds an executor over the local runtime and host tree.
func NewExecutor(rt dockerruntime.Runtime, host *hostops.Local, logger *slog.Logger) *Executor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Executor{
		rt: rt, logger: logger, host: host,
		inflight: map[int64]context.CancelFunc{},
		attaches: map[int64]*types.HijackedResponse{},
	}
}

// DeliverInput routes one input chunk to its attach session: data goes to the
// exec's stdin, EOF closes it (output keeps flowing). Writes land on the
// local daemon socket, so the channel's read loop is never meaningfully
// stalled here.
func (e *Executor) DeliverInput(chunk *agentwire.StreamChunk) {
	if chunk == nil {
		return
	}
	e.mu.Lock()
	hijack := e.attaches[chunk.ID]
	e.mu.Unlock()
	if hijack == nil {
		return
	}
	if len(chunk.Data) > 0 {
		if _, err := hijack.Conn.Write(chunk.Data); err != nil {
			e.logger.Warn("agent: exec input write failed", "error", err)
		}
	}
	if chunk.EOF {
		_ = hijack.CloseWrite()
	}
}

// Cancel aborts the command with this id, if still in flight — the control
// plane's ctx ended, or a stream consumer closed.
func (e *Executor) Cancel(id int64) {
	e.mu.Lock()
	cancel := e.inflight[id]
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Execute runs one command to completion and delivers its result — and, for
// streaming methods, its chunks — through send. The channel runs each command
// on its own goroutine and serializes send; a send failure means the channel
// died, and the command's ctx dies with it.
func (e *Executor) Execute(ctx context.Context, cmd agentwire.Command, send func(agentwire.Frame) error) {
	ctx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.inflight[cmd.ID] = cancel
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		delete(e.inflight, cmd.ID)
		e.mu.Unlock()
	}()

	if streamed := e.executeStream(ctx, cmd, send); streamed {
		return
	}

	body, err := e.executeUnary(ctx, cmd)
	res := agentwire.Result{ID: cmd.ID}
	switch {
	case err != nil:
		res.Err = agentwire.WireError(err)
	case body != nil:
		data, mErr := json.Marshal(body)
		if mErr != nil {
			res.Err = agentwire.WireError(mErr)
		} else {
			res.Body = data
		}
	}
	if err := send(agentwire.Frame{Type: agentwire.FrameResult, Res: &res}); err != nil {
		e.logger.Warn("agent: command result lost, channel gone", "method", cmd.Method, "error", err)
	}
}

// executeStream handles the four streaming methods: acknowledge the open with
// an empty result, then pump chunks until EOF, error or cancel. Reports
// whether the command was one of them.
func (e *Executor) executeStream(ctx context.Context, cmd agentwire.Command, send func(agentwire.Frame) error) bool {
	switch cmd.Method {
	case agentwire.MethodContainerLogs:
		var p agentwire.ContainerLogsParams
		e.openAndPump(ctx, cmd, send, func() (io.ReadCloser, error) {
			if err := json.Unmarshal(cmd.Params, &p); err != nil {
				return nil, invalidParams(err)
			}
			return e.rt.ContainerLogs(ctx, p.Name, p.Options)
		})
	case agentwire.MethodImagePull:
		var p agentwire.ImagePullParams
		e.openAndPump(ctx, cmd, send, func() (io.ReadCloser, error) {
			if err := json.Unmarshal(cmd.Params, &p); err != nil {
				return nil, invalidParams(err)
			}
			return e.rt.ImagePull(ctx, p.Ref, p.Options)
		})
	case agentwire.MethodImagePush:
		var p agentwire.ImagePushParams
		e.openAndPump(ctx, cmd, send, func() (io.ReadCloser, error) {
			if err := json.Unmarshal(cmd.Params, &p); err != nil {
				return nil, invalidParams(err)
			}
			return e.rt.ImagePush(ctx, p.Ref, p.Options)
		})
	case agentwire.MethodEvents:
		e.pumpEvents(ctx, cmd, send)
	case agentwire.MethodContainerExecAttach:
		e.attachExec(ctx, cmd, send)
	case agentwire.MethodImageBuild:
		// The ADR-055 build: solved agent-side, only progress crosses the wire.
		var p agentwire.ImageBuildParams
		e.openAndPump(ctx, cmd, send, func() (io.ReadCloser, error) {
			if err := json.Unmarshal(cmd.Params, &p); err != nil {
				return nil, invalidParams(err)
			}
			if e.host == nil {
				return nil, agentwire.Unavailable("this helper has no host mount — it recreates with the next spec")
			}
			return e.host.BuildImage(ctx, p)
		})
	default:
		return false
	}
	return true
}

// attachExec runs the one bidirectional command: the hijacked exec stream's
// output pumps out as chunks while DeliverInput feeds its stdin, until the
// exec ends (daemon closes the stream) or the command is canceled.
func (e *Executor) attachExec(ctx context.Context, cmd agentwire.Command, send func(agentwire.Frame) error) {
	var p agentwire.ContainerExecAttachParams
	if err := json.Unmarshal(cmd.Params, &p); err != nil {
		_ = send(agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID, Err: agentwire.WireError(invalidParams(err))}})
		return
	}
	hijack, err := e.rt.ContainerExecAttach(ctx, p.ExecID, p.Options)
	if err != nil {
		_ = send(agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID, Err: agentwire.WireError(err)}})
		return
	}
	e.mu.Lock()
	e.attaches[cmd.ID] = &hijack
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.attaches, cmd.ID)
		e.mu.Unlock()
		hijack.Close()
	}()
	if err := send(agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID}}); err != nil {
		return
	}
	// A canceled command must unblock the reader: the hijacked stream has no
	// ctx of its own.
	go func() {
		<-ctx.Done()
		hijack.Close()
	}()
	agentwire.PumpReader(ctx, cmd.ID, hijack.Reader, send)
}

// openAndPump opens the stream, acks the command, then forwards chunks. The
// open error travels in the Result; a mid-stream error travels in the final
// chunk.
func (e *Executor) openAndPump(ctx context.Context, cmd agentwire.Command, send func(agentwire.Frame) error, open func() (io.ReadCloser, error)) {
	rc, err := open()
	if err != nil {
		_ = send(agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID, Err: agentwire.WireError(err)}})
		return
	}
	defer func() { _ = rc.Close() }()
	if err := send(agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID}}); err != nil {
		return
	}
	agentwire.PumpReader(ctx, cmd.ID, rc, send)
}

// pumpEvents forwards the daemon's event stream: each chunk carries one
// events.Message as JSON, decoded back sequentially on the other side.
func (e *Executor) pumpEvents(ctx context.Context, cmd agentwire.Command, send func(agentwire.Frame) error) {
	var p agentwire.EventsParams
	if err := json.Unmarshal(cmd.Params, &p); err != nil {
		_ = send(agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID, Err: agentwire.WireError(invalidParams(err))}})
		return
	}
	msgs, errs := e.rt.Events(ctx, p.Options)
	if err := send(agentwire.Frame{Type: agentwire.FrameResult, Res: &agentwire.Result{ID: cmd.ID}}); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			_ = send(agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: cmd.ID, Err: agentwire.WireError(ctx.Err())}})
			return
		case err, ok := <-errs:
			chunk := agentwire.StreamChunk{ID: cmd.ID, EOF: true}
			if ok && err != nil && err != io.EOF {
				chunk = agentwire.StreamChunk{ID: cmd.ID, Err: agentwire.WireError(err)}
			}
			_ = send(agentwire.Frame{Type: agentwire.FrameStream, Chunk: &chunk})
			return
		case m := <-msgs:
			data, err := json.Marshal(m)
			if err != nil {
				continue
			}
			if send(agentwire.Frame{Type: agentwire.FrameStream, Chunk: &agentwire.StreamChunk{ID: cmd.ID, Data: data}}) != nil {
				return
			}
		}
	}
}

// invalidParams marks a malformed params payload — the control plane sent
// something this build cannot read; retrying will not change that.
func invalidParams(err error) error {
	return fmt.Errorf("params: %w: %w", cerrdefs.ErrInvalidArgument, err)
}

// executeUnary dispatches every non-streaming method. A nil body with a nil
// error answers with an empty result.
func (e *Executor) executeUnary(ctx context.Context, cmd agentwire.Command) (any, error) {
	switch cmd.Method {
	case agentwire.MethodContainerCreate:
		var p agentwire.ContainerCreateParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.ContainerCreate(ctx, p.Config, p.HostConfig, p.NetworkingConfig, p.Platform, p.Name)
	case agentwire.MethodContainerStart:
		var p agentwire.ContainerStartParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.rt.ContainerStart(ctx, p.Name, p.Options)
	case agentwire.MethodContainerStop:
		var p agentwire.ContainerStopParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.rt.ContainerStop(ctx, p.Name, p.Options)
	case agentwire.MethodContainerRestart:
		var p agentwire.ContainerStopParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.rt.ContainerRestart(ctx, p.Name, p.Options)
	case agentwire.MethodContainerRename:
		var p agentwire.ContainerRenameParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.rt.ContainerRename(ctx, p.Name, p.NewName)
	case agentwire.MethodContainerRemove:
		var p agentwire.ContainerRemoveParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.rt.ContainerRemove(ctx, p.Name, p.Options)
	case agentwire.MethodContainerInspect:
		var p agentwire.NameParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.ContainerInspect(ctx, p.Name)
	case agentwire.MethodContainerWait:
		var p agentwire.ContainerWaitParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		waitCh, errCh := e.rt.ContainerWait(ctx, p.Name, p.Condition)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-errCh:
			return nil, err
		case resp := <-waitCh:
			return resp, nil
		}
	case agentwire.MethodContainerList:
		var p agentwire.ContainerListParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.ContainerList(ctx, p.Options)
	case agentwire.MethodContainerStats:
		var p agentwire.NameParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		// Always the stream=false snapshot: precpu filled (CPU% computable),
		// exactly one JSON body — a true stream would never end this unary.
		stats, err := e.rt.ContainerStats(ctx, p.Name, false)
		if err != nil {
			return nil, err
		}
		defer func() { _ = stats.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(stats.Body, 1<<20))
		if err != nil {
			return nil, err
		}
		return agentwire.StatsResult{OSType: stats.OSType, Body: body}, nil
	case agentwire.MethodContainersPrune:
		var p agentwire.PruneParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.ContainersPrune(ctx, p.Filters)
	case agentwire.MethodContainerExecCreate:
		var p agentwire.ContainerExecCreateParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.ContainerExecCreate(ctx, p.Name, p.Options)
	case agentwire.MethodContainerExecStart:
		var p agentwire.ContainerExecStartParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.rt.ContainerExecStart(ctx, p.ExecID, p.Options)
	case agentwire.MethodContainerExecInspect:
		var p agentwire.NameParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.ContainerExecInspect(ctx, p.Name)
	case agentwire.MethodContainerExecResize:
		var p agentwire.ContainerExecResizeParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.rt.ContainerExecResize(ctx, p.ExecID, p.Options)
	case agentwire.MethodImageTag:
		var p agentwire.ImageTagParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.rt.ImageTag(ctx, p.Image, p.Ref)
	case agentwire.MethodImageInspect:
		var p agentwire.NameParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.ImageInspect(ctx, p.Name)
	case agentwire.MethodImageList:
		var p agentwire.ImageListParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.ImageList(ctx, p.Options)
	case agentwire.MethodImageRemove:
		var p agentwire.ImageRemoveParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.ImageRemove(ctx, p.Image, p.Options)
	case agentwire.MethodImagesPrune:
		var p agentwire.PruneParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.ImagesPrune(ctx, p.Filters)
	case agentwire.MethodVolumeCreate:
		var p agentwire.VolumeCreateParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.VolumeCreate(ctx, p.Options)
	case agentwire.MethodVolumeInspect:
		var p agentwire.NameParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.VolumeInspect(ctx, p.Name)
	case agentwire.MethodVolumeList:
		var p agentwire.VolumeListParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.VolumeList(ctx, p.Options)
	case agentwire.MethodVolumeRemove:
		var p agentwire.VolumeRemoveParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.rt.VolumeRemove(ctx, p.Name, p.Force)
	case agentwire.MethodVolumesPrune:
		var p agentwire.PruneParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.VolumesPrune(ctx, p.Filters)
	case agentwire.MethodNetworkCreate:
		var p agentwire.NetworkCreateParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.NetworkCreate(ctx, p.Name, p.Options)
	case agentwire.MethodNetworkConnect:
		var p agentwire.NetworkConnectParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.rt.NetworkConnect(ctx, p.Network, p.Container, p.Config)
	case agentwire.MethodNetworkDisconnect:
		var p agentwire.NetworkDisconnectParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.rt.NetworkDisconnect(ctx, p.Network, p.Container, p.Force)
	case agentwire.MethodNetworkInspect:
		var p agentwire.NetworkInspectParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.NetworkInspect(ctx, p.Network, p.Options)
	case agentwire.MethodNetworkList:
		var p agentwire.NetworkListParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.NetworkList(ctx, p.Options)
	case agentwire.MethodNetworkRemove:
		var p agentwire.NameParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.rt.NetworkRemove(ctx, p.Name)
	case agentwire.MethodNetworksPrune:
		var p agentwire.PruneParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.NetworksPrune(ctx, p.Filters)
	case agentwire.MethodInfo:
		return e.rt.Info(ctx)
	case agentwire.MethodServerVersion:
		return e.rt.ServerVersion(ctx)
	case agentwire.MethodDiskUsage:
		var p agentwire.DiskUsageParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.DiskUsage(ctx, p.Options)
	case agentwire.MethodRegistryLogin:
		var p agentwire.RegistryLoginParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.rt.RegistryLogin(ctx, p.Auth)
	case agentwire.MethodPing:
		return e.rt.Ping(ctx)
	case agentwire.MethodFileWrite, agentwire.MethodFileRead, agentwire.MethodFileRemove,
		agentwire.MethodFileStat, agentwire.MethodFileChown, agentwire.MethodFileCopy,
		agentwire.MethodDirEnsure, agentwire.MethodExecToFile, agentwire.MethodFileToExec,
		agentwire.MethodFileToURL, agentwire.MethodURLToFile, agentwire.MethodFileHash:
		return e.executeHostOp(ctx, cmd)
	default:
		return nil, fmt.Errorf("method %q: %w", cmd.Method, cerrdefs.ErrNotImplemented)
	}
}

// executeHostOp dispatches the ADR-054 file primitives onto the mounted host
// tree. The path guard lives in hostops.Local — authoritative on this side of
// the wire, whatever the control plane sent.
func (e *Executor) executeHostOp(ctx context.Context, cmd agentwire.Command) (any, error) {
	if e.host == nil {
		return nil, agentwire.Unavailable("this helper has no host mount — it recreates with the next spec")
	}
	switch cmd.Method {
	case agentwire.MethodFileWrite:
		var p agentwire.FileWriteParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.host.WriteFile(ctx, p)
	case agentwire.MethodFileRead:
		var p agentwire.FileReadParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.host.ReadFile(ctx, p)
	case agentwire.MethodFileRemove:
		var p agentwire.FileRemoveParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.host.Remove(ctx, p)
	case agentwire.MethodFileStat:
		var p agentwire.FileStatParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.host.Stat(ctx, p.Path)
	case agentwire.MethodFileChown:
		var p agentwire.FileChownParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.host.Chown(ctx, p)
	case agentwire.MethodFileCopy:
		var p agentwire.FileCopyParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.host.CopyFile(ctx, p)
	case agentwire.MethodDirEnsure:
		var p agentwire.DirEnsureParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.host.EnsureDir(ctx, p)
	case agentwire.MethodExecToFile:
		var p agentwire.ExecToFileParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.host.ExecToFile(ctx, p)
	case agentwire.MethodFileToExec:
		var p agentwire.FileToExecParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.host.FileToExec(ctx, p)
	case agentwire.MethodFileToURL:
		var p agentwire.FileToURLParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.host.FileToURL(ctx, p)
	case agentwire.MethodURLToFile:
		var p agentwire.URLToFileParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return nil, e.host.URLToFile(ctx, p)
	default: // MethodFileHash
		var p agentwire.FileHashParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return nil, invalidParams(err)
		}
		return e.host.HashFile(ctx, p.Path)
	}
}
