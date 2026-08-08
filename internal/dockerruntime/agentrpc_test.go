package dockerruntime

// The agent runtime is a pure translation layer: each Runtime method becomes
// one typed command frame (ADR-052). These tests pin that mapping — method
// name, params struct, result decoding — against a recording CommandSender,
// with no transport underneath.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/deepteams/akerdock/internal/agentwire"
)

// fakeCall is one recorded command: the method name and the params value the
// runtime handed the sender.
type fakeCall struct {
	method string
	params any
}

// fakeSender records every command and answers with canned results. The mutex
// makes it safe for the methods that call from a goroutine (ContainerWait,
// Events) under -race.
type fakeSender struct {
	mu         sync.Mutex
	calls      []fakeCall
	result     json.RawMessage
	err        error
	streamBody string
	streamErr  error
	attach     AttachStream
	attachErr  error
}

func (f *fakeSender) record(method string, params any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{method: method, params: params})
}

func (f *fakeSender) Command(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.record(method, params)
	return f.result, f.err
}

func (f *fakeSender) Stream(_ context.Context, method string, params any) (io.ReadCloser, error) {
	f.record(method, params)
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return io.NopCloser(strings.NewReader(f.streamBody)), nil
}

func (f *fakeSender) Attach(_ context.Context, method string, params any) (AttachStream, error) {
	f.record(method, params)
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	return f.attach, nil
}

// onlyCall asserts exactly one command was sent and returns it.
func (f *fakeSender) onlyCall(t *testing.T) fakeCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 {
		t.Fatalf("recorded %d commands, want exactly 1", len(f.calls))
	}
	return f.calls[0]
}

func TestAgentRuntimeUnaryCommandsCarryTheirTypedParams(t *testing.T) {
	sweepFilters := filters.NewArgs(filters.Arg("label", "akerdock.managed=true"))
	stopTimeout := 10
	cfg := &container.Config{Image: "nginx:1"}
	hostCfg := &container.HostConfig{AutoRemove: true}
	netCfg := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{"akerdock": {}}}
	platform := &ocispec.Platform{OS: "linux", Architecture: "arm64"}

	tests := []struct {
		name       string
		invoke     func(ctx context.Context, rt Runtime) (any, error)
		wantMethod string
		wantParams any
		wantOut    any // nil for methods whose only result is success
	}{
		{
			name: "ContainerCreate",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.ContainerCreate(ctx, cfg, hostCfg, netCfg, platform, "web")
			},
			wantMethod: agentwire.MethodContainerCreate,
			wantParams: agentwire.ContainerCreateParams{Config: cfg, HostConfig: hostCfg, NetworkingConfig: netCfg, Platform: platform, Name: "web"},
			wantOut:    container.CreateResponse{ID: "c1", Warnings: []string{"platform mismatch"}},
		},
		{
			name: "ContainerStart",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return nil, rt.ContainerStart(ctx, "web", container.StartOptions{})
			},
			wantMethod: agentwire.MethodContainerStart,
			wantParams: agentwire.ContainerStartParams{Name: "web", Options: container.StartOptions{}},
		},
		{
			name: "ContainerStop",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return nil, rt.ContainerStop(ctx, "web", container.StopOptions{Timeout: &stopTimeout})
			},
			wantMethod: agentwire.MethodContainerStop,
			wantParams: agentwire.ContainerStopParams{Name: "web", Options: container.StopOptions{Timeout: &stopTimeout}},
		},
		{
			name: "ContainerRestart",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return nil, rt.ContainerRestart(ctx, "web", container.StopOptions{Signal: "SIGHUP"})
			},
			wantMethod: agentwire.MethodContainerRestart,
			wantParams: agentwire.ContainerStopParams{Name: "web", Options: container.StopOptions{Signal: "SIGHUP"}},
		},
		{
			name: "ContainerRename",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return nil, rt.ContainerRename(ctx, "web", "web-old")
			},
			wantMethod: agentwire.MethodContainerRename,
			wantParams: agentwire.ContainerRenameParams{Name: "web", NewName: "web-old"},
		},
		{
			name: "ContainerRemove",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return nil, rt.ContainerRemove(ctx, "web", container.RemoveOptions{Force: true})
			},
			wantMethod: agentwire.MethodContainerRemove,
			wantParams: agentwire.ContainerRemoveParams{Name: "web", Options: container.RemoveOptions{Force: true}},
		},
		{
			name: "ContainerInspect",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.ContainerInspect(ctx, "web")
			},
			wantMethod: agentwire.MethodContainerInspect,
			wantParams: agentwire.NameParams{Name: "web"},
			wantOut:    container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "c1", Name: "/web"}},
		},
		{
			name: "ContainerList",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.ContainerList(ctx, container.ListOptions{All: true, Filters: sweepFilters})
			},
			wantMethod: agentwire.MethodContainerList,
			wantParams: agentwire.ContainerListParams{Options: container.ListOptions{All: true, Filters: sweepFilters}, Filters: agentwire.EncodeFilters(sweepFilters)},
			wantOut:    []container.Summary{{ID: "c1", Image: "nginx:1"}},
		},
		{
			name: "ContainersPrune",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.ContainersPrune(ctx, sweepFilters)
			},
			wantMethod: agentwire.MethodContainersPrune,
			wantParams: agentwire.PruneParams{Filters: agentwire.EncodeFilters(sweepFilters)},
			wantOut:    container.PruneReport{ContainersDeleted: []string{"c1"}, SpaceReclaimed: 512},
		},
		{
			name: "ContainerExecCreate",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.ContainerExecCreate(ctx, "web", container.ExecOptions{Cmd: []string{"sh"}})
			},
			wantMethod: agentwire.MethodContainerExecCreate,
			wantParams: agentwire.ContainerExecCreateParams{Name: "web", Options: container.ExecOptions{Cmd: []string{"sh"}}},
			wantOut:    container.ExecCreateResponse{ID: "e1"},
		},
		{
			name: "ContainerExecStart",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return nil, rt.ContainerExecStart(ctx, "e1", container.ExecStartOptions{Detach: true})
			},
			wantMethod: agentwire.MethodContainerExecStart,
			wantParams: agentwire.ContainerExecStartParams{ExecID: "e1", Options: container.ExecStartOptions{Detach: true}},
		},
		{
			name: "ContainerExecInspect",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.ContainerExecInspect(ctx, "e1")
			},
			wantMethod: agentwire.MethodContainerExecInspect,
			wantParams: agentwire.NameParams{Name: "e1"},
			wantOut:    container.ExecInspect{ExecID: "e1", ContainerID: "c1", ExitCode: 137},
		},
		{
			name: "ContainerExecResize",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return nil, rt.ContainerExecResize(ctx, "e1", container.ResizeOptions{Height: 24, Width: 80})
			},
			wantMethod: agentwire.MethodContainerExecResize,
			wantParams: agentwire.ContainerExecResizeParams{ExecID: "e1", Options: container.ResizeOptions{Height: 24, Width: 80}},
		},
		{
			name: "ImageTag",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return nil, rt.ImageTag(ctx, "nginx:1", "registry.local/nginx:1")
			},
			wantMethod: agentwire.MethodImageTag,
			wantParams: agentwire.ImageTagParams{Image: "nginx:1", Ref: "registry.local/nginx:1"},
		},
		{
			name: "ImageInspect",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.ImageInspect(ctx, "nginx:1")
			},
			wantMethod: agentwire.MethodImageInspect,
			wantParams: agentwire.NameParams{Name: "nginx:1"},
			wantOut:    image.InspectResponse{ID: "sha256:abc"},
		},
		{
			name: "ImageList",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.ImageList(ctx, image.ListOptions{All: true, Filters: sweepFilters})
			},
			wantMethod: agentwire.MethodImageList,
			wantParams: agentwire.ImageListParams{Options: image.ListOptions{All: true, Filters: sweepFilters}, Filters: agentwire.EncodeFilters(sweepFilters)},
			wantOut:    []image.Summary{{ID: "sha256:abc"}},
		},
		{
			name: "ImageRemove",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.ImageRemove(ctx, "nginx:1", image.RemoveOptions{Force: true})
			},
			wantMethod: agentwire.MethodImageRemove,
			wantParams: agentwire.ImageRemoveParams{Image: "nginx:1", Options: image.RemoveOptions{Force: true}},
			wantOut:    []image.DeleteResponse{{Deleted: "sha256:abc"}},
		},
		{
			name: "ImagesPrune",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.ImagesPrune(ctx, sweepFilters)
			},
			wantMethod: agentwire.MethodImagesPrune,
			wantParams: agentwire.PruneParams{Filters: agentwire.EncodeFilters(sweepFilters)},
			wantOut:    image.PruneReport{SpaceReclaimed: 1024},
		},
		{
			name: "VolumeCreate",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.VolumeCreate(ctx, volume.CreateOptions{Name: "data"})
			},
			wantMethod: agentwire.MethodVolumeCreate,
			wantParams: agentwire.VolumeCreateParams{Options: volume.CreateOptions{Name: "data"}},
			wantOut:    volume.Volume{Name: "data", Driver: "local"},
		},
		{
			name: "VolumeInspect",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.VolumeInspect(ctx, "data")
			},
			wantMethod: agentwire.MethodVolumeInspect,
			wantParams: agentwire.NameParams{Name: "data"},
			wantOut:    volume.Volume{Name: "data"},
		},
		{
			name: "VolumeList",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.VolumeList(ctx, volume.ListOptions{Filters: sweepFilters})
			},
			wantMethod: agentwire.MethodVolumeList,
			wantParams: agentwire.VolumeListParams{Options: volume.ListOptions{Filters: sweepFilters}, Filters: agentwire.EncodeFilters(sweepFilters)},
			wantOut:    volume.ListResponse{Volumes: []*volume.Volume{{Name: "data"}}},
		},
		{
			name: "VolumeRemove",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return nil, rt.VolumeRemove(ctx, "data", true)
			},
			wantMethod: agentwire.MethodVolumeRemove,
			wantParams: agentwire.VolumeRemoveParams{Name: "data", Force: true},
		},
		{
			name: "VolumesPrune",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.VolumesPrune(ctx, sweepFilters)
			},
			wantMethod: agentwire.MethodVolumesPrune,
			wantParams: agentwire.PruneParams{Filters: agentwire.EncodeFilters(sweepFilters)},
			wantOut:    volume.PruneReport{VolumesDeleted: []string{"data"}},
		},
		{
			name: "NetworkCreate",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.NetworkCreate(ctx, "akerdock", network.CreateOptions{Driver: "bridge"})
			},
			wantMethod: agentwire.MethodNetworkCreate,
			wantParams: agentwire.NetworkCreateParams{Name: "akerdock", Options: network.CreateOptions{Driver: "bridge"}},
			wantOut:    network.CreateResponse{ID: "n1"},
		},
		{
			name: "NetworkConnect",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return nil, rt.NetworkConnect(ctx, "n1", "web", &network.EndpointSettings{Aliases: []string{"web"}})
			},
			wantMethod: agentwire.MethodNetworkConnect,
			wantParams: agentwire.NetworkConnectParams{Network: "n1", Container: "web", Config: &network.EndpointSettings{Aliases: []string{"web"}}},
		},
		{
			name: "NetworkDisconnect",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return nil, rt.NetworkDisconnect(ctx, "n1", "web", true)
			},
			wantMethod: agentwire.MethodNetworkDisconnect,
			wantParams: agentwire.NetworkDisconnectParams{Network: "n1", Container: "web", Force: true},
		},
		{
			name: "NetworkInspect",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.NetworkInspect(ctx, "n1", network.InspectOptions{Verbose: true})
			},
			wantMethod: agentwire.MethodNetworkInspect,
			wantParams: agentwire.NetworkInspectParams{Network: "n1", Options: network.InspectOptions{Verbose: true}},
			wantOut:    network.Inspect{ID: "n1", Name: "akerdock"},
		},
		{
			name: "NetworkList",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.NetworkList(ctx, network.ListOptions{Filters: sweepFilters})
			},
			wantMethod: agentwire.MethodNetworkList,
			wantParams: agentwire.NetworkListParams{Options: network.ListOptions{Filters: sweepFilters}, Filters: agentwire.EncodeFilters(sweepFilters)},
			wantOut:    []network.Summary{{ID: "n1"}},
		},
		{
			name: "NetworkRemove",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return nil, rt.NetworkRemove(ctx, "n1")
			},
			wantMethod: agentwire.MethodNetworkRemove,
			wantParams: agentwire.NameParams{Name: "n1"},
		},
		{
			name: "NetworksPrune",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.NetworksPrune(ctx, sweepFilters)
			},
			wantMethod: agentwire.MethodNetworksPrune,
			wantParams: agentwire.PruneParams{Filters: agentwire.EncodeFilters(sweepFilters)},
			wantOut:    network.PruneReport{NetworksDeleted: []string{"n1"}},
		},
		{
			name: "Info",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.Info(ctx)
			},
			wantMethod: agentwire.MethodInfo,
			wantParams: nil,
			wantOut:    system.Info{ID: "daemon-1", Name: "srv-1"},
		},
		{
			name: "ServerVersion",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.ServerVersion(ctx)
			},
			wantMethod: agentwire.MethodServerVersion,
			wantParams: nil,
			wantOut:    types.Version{Version: "27.0.1", APIVersion: "1.46"},
		},
		{
			name: "DiskUsage",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.DiskUsage(ctx, types.DiskUsageOptions{})
			},
			wantMethod: agentwire.MethodDiskUsage,
			wantParams: agentwire.DiskUsageParams{Options: types.DiskUsageOptions{}},
			wantOut:    types.DiskUsage{LayersSize: 42},
		},
		{
			name: "RegistryLogin",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.RegistryLogin(ctx, registry.AuthConfig{Username: "bob", ServerAddress: "ghcr.io"})
			},
			wantMethod: agentwire.MethodRegistryLogin,
			wantParams: agentwire.RegistryLoginParams{Auth: registry.AuthConfig{Username: "bob", ServerAddress: "ghcr.io"}},
			wantOut:    registry.AuthenticateOKBody{Status: "Login Succeeded"},
		},
		{
			name: "Ping",
			invoke: func(ctx context.Context, rt Runtime) (any, error) {
				return rt.Ping(ctx)
			},
			wantMethod: agentwire.MethodPing,
			wantParams: nil,
			wantOut:    types.Ping{APIVersion: "1.46", OSType: "linux"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeSender{}
			if tc.wantOut != nil {
				raw, err := json.Marshal(tc.wantOut)
				if err != nil {
					t.Fatalf("marshal canned result: %v", err)
				}
				s.result = raw
			}
			rt := NewAgentRuntime(s)
			got, err := tc.invoke(context.Background(), rt)
			if err != nil {
				t.Fatalf("%s: %v", tc.wantMethod, err)
			}
			sent := s.onlyCall(t)
			if sent.method != tc.wantMethod {
				t.Fatalf("method = %q, want %q", sent.method, tc.wantMethod)
			}
			if !reflect.DeepEqual(sent.params, tc.wantParams) {
				t.Fatalf("params = %#v, want %#v", sent.params, tc.wantParams)
			}
			if tc.wantOut != nil && !reflect.DeepEqual(got, tc.wantOut) {
				t.Fatalf("result = %#v, want %#v", got, tc.wantOut)
			}
		})
	}
}

func TestAgentRuntimeStreamCommandsOpenTheChunkStream(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		invoke     func(ctx context.Context, rt Runtime) (io.ReadCloser, error)
		wantMethod string
		wantParams any
	}{
		{
			name: "ContainerLogs",
			body: "log line\n",
			invoke: func(ctx context.Context, rt Runtime) (io.ReadCloser, error) {
				return rt.ContainerLogs(ctx, "web", container.LogsOptions{ShowStdout: true, Follow: true})
			},
			wantMethod: agentwire.MethodContainerLogs,
			wantParams: agentwire.ContainerLogsParams{Name: "web", Options: container.LogsOptions{ShowStdout: true, Follow: true}},
		},
		{
			name: "ImagePull",
			body: `{"status":"Pulling"}`,
			invoke: func(ctx context.Context, rt Runtime) (io.ReadCloser, error) {
				return rt.ImagePull(ctx, "nginx:1", image.PullOptions{RegistryAuth: "tok", Platform: "linux/amd64"})
			},
			wantMethod: agentwire.MethodImagePull,
			wantParams: agentwire.ImagePullParams{Ref: "nginx:1", RegistryAuth: "tok", Platform: "linux/amd64"},
		},
		{
			name: "ImagePush",
			body: `{"status":"Pushing"}`,
			invoke: func(ctx context.Context, rt Runtime) (io.ReadCloser, error) {
				return rt.ImagePush(ctx, "registry.local/nginx:1", image.PushOptions{All: true, RegistryAuth: "tok"})
			},
			wantMethod: agentwire.MethodImagePush,
			wantParams: agentwire.ImagePushParams{Ref: "registry.local/nginx:1", All: true, RegistryAuth: "tok"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeSender{streamBody: tc.body}
			rt := NewAgentRuntime(s)
			rc, err := tc.invoke(context.Background(), rt)
			if err != nil {
				t.Fatalf("%s: %v", tc.wantMethod, err)
			}
			defer func() { _ = rc.Close() }()
			sent := s.onlyCall(t)
			if sent.method != tc.wantMethod {
				t.Fatalf("method = %q, want %q", sent.method, tc.wantMethod)
			}
			if !reflect.DeepEqual(sent.params, tc.wantParams) {
				t.Fatalf("params = %#v, want %#v", sent.params, tc.wantParams)
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read stream: %v", err)
			}
			if string(got) != tc.body {
				t.Fatalf("stream body = %q, want %q", got, tc.body)
			}
		})
	}
}

func TestAgentRuntimeSurfacesTheChannelFailure(t *testing.T) {
	down := fmt.Errorf("server 7: %w", cerrdefs.ErrUnavailable)

	t.Run("unary result", func(t *testing.T) {
		rt := NewAgentRuntime(&fakeSender{err: down})
		if _, err := rt.ContainerInspect(context.Background(), "web"); !IsUnavailable(err) {
			t.Fatalf("ContainerInspect err = %v, want the unavailable chain", err)
		}
	})
	t.Run("unary ack", func(t *testing.T) {
		rt := NewAgentRuntime(&fakeSender{err: down})
		if err := rt.ContainerStart(context.Background(), "web", container.StartOptions{}); !IsUnavailable(err) {
			t.Fatalf("ContainerStart err = %v, want the unavailable chain", err)
		}
	})
	t.Run("stream", func(t *testing.T) {
		rt := NewAgentRuntime(&fakeSender{streamErr: down})
		if _, err := rt.ContainerLogs(context.Background(), "web", container.LogsOptions{}); !IsUnavailable(err) {
			t.Fatalf("ContainerLogs err = %v, want the unavailable chain", err)
		}
	})
}

func TestAgentRuntimeCallDecodesEdgeResults(t *testing.T) {
	t.Run("empty result decodes to the zero value", func(t *testing.T) {
		rt := NewAgentRuntime(&fakeSender{})
		info, err := rt.Info(context.Background())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if !reflect.DeepEqual(info, system.Info{}) {
			t.Fatalf("Info from empty result = %#v, want the zero value", info)
		}
	})
	t.Run("malformed result names the method", func(t *testing.T) {
		rt := NewAgentRuntime(&fakeSender{result: json.RawMessage(`{"ID":`)})
		_, err := rt.Info(context.Background())
		if err == nil || !strings.Contains(err.Error(), "agent Info result") {
			t.Fatalf("Info with malformed result = %v, want a decode error naming the method", err)
		}
	})
}

func TestAgentRuntimeContainerWaitMirrorsTheSDKChannels(t *testing.T) {
	t.Run("answer on the wait channel", func(t *testing.T) {
		s := &fakeSender{result: json.RawMessage(`{"StatusCode":137}`)}
		rt := NewAgentRuntime(s)
		waitCh, errCh := rt.ContainerWait(context.Background(), "web", container.WaitConditionNotRunning)
		select {
		case resp := <-waitCh:
			if resp.StatusCode != 137 {
				t.Fatalf("StatusCode = %d, want 137", resp.StatusCode)
			}
		case err := <-errCh:
			t.Fatalf("unexpected wait error: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("ContainerWait never answered")
		}
		sent := s.onlyCall(t)
		if sent.method != agentwire.MethodContainerWait {
			t.Fatalf("method = %q", sent.method)
		}
		want := agentwire.ContainerWaitParams{Name: "web", Condition: container.WaitConditionNotRunning}
		if !reflect.DeepEqual(sent.params, want) {
			t.Fatalf("params = %#v, want %#v", sent.params, want)
		}
	})
	t.Run("failure on the error channel", func(t *testing.T) {
		down := fmt.Errorf("channel: %w", cerrdefs.ErrUnavailable)
		rt := NewAgentRuntime(&fakeSender{err: down})
		waitCh, errCh := rt.ContainerWait(context.Background(), "web", container.WaitConditionNextExit)
		select {
		case resp := <-waitCh:
			t.Fatalf("unexpected wait answer: %#v", resp)
		case err := <-errCh:
			if !IsUnavailable(err) {
				t.Fatalf("wait err = %v, want the unavailable chain", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ContainerWait never failed")
		}
	})
}

func TestAgentRuntimeContainerStatsRewrapsTheSnapshot(t *testing.T) {
	snapshot := []byte(`{"pids_stats":{"current":3}}`)
	raw, err := json.Marshal(agentwire.StatsResult{OSType: "linux", Body: snapshot})
	if err != nil {
		t.Fatalf("marshal canned result: %v", err)
	}
	s := &fakeSender{result: raw}
	rt := NewAgentRuntime(s)

	reader, err := rt.ContainerStats(context.Background(), "web", false)
	if err != nil {
		t.Fatalf("ContainerStats: %v", err)
	}
	defer func() { _ = reader.Body.Close() }()
	if reader.OSType != "linux" {
		t.Fatalf("OSType = %q, want linux", reader.OSType)
	}
	body, err := io.ReadAll(reader.Body)
	if err != nil {
		t.Fatalf("read stats body: %v", err)
	}
	if !bytes.Equal(body, snapshot) {
		t.Fatalf("stats body = %s, want %s", body, snapshot)
	}
	sent := s.onlyCall(t)
	if sent.method != agentwire.MethodContainerStats || !reflect.DeepEqual(sent.params, agentwire.NameParams{Name: "web"}) {
		t.Fatalf("sent %q %#v", sent.method, sent.params)
	}
}

func TestAgentRuntimeContainerStatsRefusesStreaming(t *testing.T) {
	s := &fakeSender{}
	rt := NewAgentRuntime(s)
	_, err := rt.ContainerStats(context.Background(), "web", true)
	if !errors.Is(err, cerrdefs.ErrNotImplemented) {
		t.Fatalf("stream=true err = %v, want ErrNotImplemented", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 0 {
		t.Fatalf("stream=true still sent %d commands", len(s.calls))
	}
}

func TestAgentRuntimeContainerStatsSurfacesTheCommandFailure(t *testing.T) {
	down := fmt.Errorf("channel: %w", cerrdefs.ErrUnavailable)
	rt := NewAgentRuntime(&fakeSender{err: down})
	if _, err := rt.ContainerStats(context.Background(), "web", false); !IsUnavailable(err) {
		t.Fatalf("ContainerStats err = %v, want the unavailable chain", err)
	}
}

// fakeAttach is a bidirectional exec stream double: reads serve the canned
// output, writes accumulate as the stdin the caller sent.
type fakeAttach struct {
	out         *strings.Reader
	stdin       bytes.Buffer
	closed      bool
	writeClosed bool
}

func (f *fakeAttach) Read(p []byte) (int, error)  { return f.out.Read(p) }
func (f *fakeAttach) Write(p []byte) (int, error) { return f.stdin.Write(p) }
func (f *fakeAttach) Close() error                { f.closed = true; return nil }
func (f *fakeAttach) CloseWrite() error           { f.writeClosed = true; return nil }

func TestAgentRuntimeExecAttachDressesTheStreamAsAHijackedConn(t *testing.T) {
	att := &fakeAttach{out: strings.NewReader("exec output")}
	s := &fakeSender{attach: att}
	rt := NewAgentRuntime(s)

	hr, err := rt.ContainerExecAttach(context.Background(), "e1", container.ExecAttachOptions{Tty: true})
	if err != nil {
		t.Fatalf("ContainerExecAttach: %v", err)
	}
	sent := s.onlyCall(t)
	want := agentwire.ContainerExecAttachParams{ExecID: "e1", Options: container.ExecAttachOptions{Tty: true}}
	if sent.method != agentwire.MethodContainerExecAttach || !reflect.DeepEqual(sent.params, want) {
		t.Fatalf("sent %q %#v", sent.method, sent.params)
	}

	// Output flows through both the raw Conn and the buffered Reader.
	head := make([]byte, 5)
	if n, err := hr.Conn.Read(head); err != nil || string(head[:n]) != "exec " {
		t.Fatalf("Conn.Read = %q, %v", head[:n], err)
	}
	rest, err := io.ReadAll(hr.Reader)
	if err != nil || string(rest) != "output" {
		t.Fatalf("Reader = %q, %v", rest, err)
	}

	// Stdin flows the other way; CloseWrite ends it without ending the output.
	if _, err := hr.Conn.Write([]byte("exit\n")); err != nil {
		t.Fatalf("Conn.Write: %v", err)
	}
	if att.stdin.String() != "exit\n" {
		t.Fatalf("stdin = %q", att.stdin.String())
	}
	if err := hr.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if !att.writeClosed {
		t.Fatal("CloseWrite never reached the attach stream")
	}

	// The conn placates net.Conn: named pseudo-addresses, no-op deadlines.
	if hr.Conn.LocalAddr().Network() != "akerdock-agent" || hr.Conn.LocalAddr().String() != "agent-channel" {
		t.Fatalf("LocalAddr = %v/%v", hr.Conn.LocalAddr().Network(), hr.Conn.LocalAddr().String())
	}
	if hr.Conn.RemoteAddr().Network() != "akerdock-agent" {
		t.Fatalf("RemoteAddr = %v", hr.Conn.RemoteAddr())
	}
	now := time.Now()
	if err := hr.Conn.SetDeadline(now); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if err := hr.Conn.SetReadDeadline(now); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if err := hr.Conn.SetWriteDeadline(now); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}

	hr.Close()
	if !att.closed {
		t.Fatal("Close never reached the attach stream")
	}
}

func TestAgentRuntimeExecAttachSurfacesTheAttachFailure(t *testing.T) {
	down := fmt.Errorf("channel: %w", cerrdefs.ErrUnavailable)
	rt := NewAgentRuntime(&fakeSender{attachErr: down})
	if _, err := rt.ContainerExecAttach(context.Background(), "e1", container.ExecAttachOptions{}); !IsUnavailable(err) {
		t.Fatalf("ContainerExecAttach err = %v, want the unavailable chain", err)
	}
}

func TestAgentRuntimeEventsDecodesTheChunkStream(t *testing.T) {
	evFilters := filters.NewArgs(filters.Arg("type", "container"))
	msgs := []events.Message{
		{Type: events.ContainerEventType, Action: events.ActionStart},
		{Type: events.ContainerEventType, Action: events.ActionDie},
	}
	var body strings.Builder
	for _, m := range msgs {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		body.Write(raw)
	}

	s := &fakeSender{streamBody: body.String()}
	rt := NewAgentRuntime(s)
	msgCh, errCh := rt.Events(context.Background(), events.ListOptions{Filters: evFilters})

	for i, want := range msgs {
		select {
		case got := <-msgCh:
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("event %d = %#v, want %#v", i, got, want)
			}
		case err := <-errCh:
			t.Fatalf("event %d: unexpected error %v", i, err)
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d never arrived", i)
		}
	}
	// The SDK contract never ends this stream cleanly: EOF is abnormal.
	select {
	case err := <-errCh:
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("end-of-stream err = %v, want ErrUnexpectedEOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("end of stream never surfaced")
	}

	sent := s.onlyCall(t)
	want := agentwire.EventsParams{Options: events.ListOptions{Filters: evFilters}, Filters: agentwire.EncodeFilters(evFilters)}
	if sent.method != agentwire.MethodEvents || !reflect.DeepEqual(sent.params, want) {
		t.Fatalf("sent %q %#v", sent.method, sent.params)
	}
}

func TestAgentRuntimeEventsSurfacesTheStreamFailure(t *testing.T) {
	down := fmt.Errorf("channel: %w", cerrdefs.ErrUnavailable)
	rt := NewAgentRuntime(&fakeSender{streamErr: down})
	_, errCh := rt.Events(context.Background(), events.ListOptions{})
	select {
	case err := <-errCh:
		if !IsUnavailable(err) {
			t.Fatalf("Events err = %v, want the unavailable chain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Events never failed")
	}
}

func TestAgentRuntimeEventsStopsOnContextCancel(t *testing.T) {
	raw, err := json.Marshal(events.Message{Type: events.ContainerEventType, Action: events.ActionStart})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt := NewAgentRuntime(&fakeSender{streamBody: string(raw)})
	// Nobody drains msgCh: the decoded event blocks on delivery until the
	// cancellation releases the goroutine.
	_, errCh := rt.Events(ctx, events.ListOptions{})
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Events err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Events never noticed the cancellation")
	}
}

func TestAgentRuntimeCloseReleasesNothing(t *testing.T) {
	s := &fakeSender{}
	rt := NewAgentRuntime(s)
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 0 {
		t.Fatalf("Close sent %d commands, want none — the channel belongs to the handler", len(s.calls))
	}
}
