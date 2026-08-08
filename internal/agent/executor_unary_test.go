package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/hostops"
)

// TestExecutorUnaryDispatchCoversTheVocabulary drives every non-streaming
// Docker method once with well-formed params and asserts the daemon call
// lands and the answer is clean — the enumerated vocabulary is the contract
// (ADR-052), so each arm of the dispatch is pinned.
func TestExecutorUnaryDispatchCoversTheVocabulary(t *testing.T) {
	flt := filters.NewArgs(filters.Arg("label", managedLabel))
	cases := []struct {
		method string
		params any
		setup  func(rt *fake.Runtime)
		check  func(t *testing.T, res *agentwire.Result)
	}{
		{method: agentwire.MethodContainerCreate, params: agentwire.ContainerCreateParams{Name: "c"}},
		{method: agentwire.MethodContainerStop, params: agentwire.ContainerStopParams{Name: "c"}},
		{method: agentwire.MethodContainerRestart, params: agentwire.ContainerStopParams{Name: "c"}},
		{method: agentwire.MethodContainerRename, params: agentwire.ContainerRenameParams{Name: "c", NewName: "d"}},
		{method: agentwire.MethodContainerRemove, params: agentwire.ContainerRemoveParams{Name: "c"}},
		{
			method: agentwire.MethodContainerWait,
			params: agentwire.ContainerWaitParams{Name: "c", Condition: container.WaitConditionNotRunning},
			setup: func(rt *fake.Runtime) {
				rt.ContainerWaitFn = func(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
					waitCh := make(chan container.WaitResponse, 1)
					waitCh <- container.WaitResponse{StatusCode: 7}
					return waitCh, make(chan error, 1)
				}
			},
			check: func(t *testing.T, res *agentwire.Result) {
				var resp container.WaitResponse
				if err := json.Unmarshal(res.Body, &resp); err != nil || resp.StatusCode != 7 {
					t.Fatalf("wait body = %s (%v), want status 7", res.Body, err)
				}
			},
		},
		{
			method: agentwire.MethodContainerStats,
			params: agentwire.NameParams{Name: "c"},
			setup: func(rt *fake.Runtime) {
				rt.ContainerStatsFn = func(_ context.Context, _ string, stream bool) (container.StatsResponseReader, error) {
					if stream {
						t.Error("stats must be the one-shot snapshot, never a stream")
					}
					return container.StatsResponseReader{
						OSType: "linux",
						Body:   io.NopCloser(strings.NewReader(`{"read":"now"}`)),
					}, nil
				}
			},
			check: func(t *testing.T, res *agentwire.Result) {
				var stats agentwire.StatsResult
				if err := json.Unmarshal(res.Body, &stats); err != nil {
					t.Fatal(err)
				}
				if stats.OSType != "linux" || string(stats.Body) != `{"read":"now"}` {
					t.Fatalf("stats result = %+v", stats)
				}
			},
		},
		{
			method: agentwire.MethodContainersPrune,
			params: agentwire.PruneParams{Filters: agentwire.EncodeFilters(flt)},
		},
		{method: agentwire.MethodContainerExecCreate, params: agentwire.ContainerExecCreateParams{Name: "c"}},
		{method: agentwire.MethodContainerExecStart, params: agentwire.ContainerExecStartParams{ExecID: "e1"}},
		{
			method: agentwire.MethodContainerExecInspect,
			params: agentwire.NameParams{Name: "e1"},
			setup: func(rt *fake.Runtime) {
				rt.ContainerExecInspectFn = func(context.Context, string) (container.ExecInspect, error) {
					return container.ExecInspect{ExitCode: 3}, nil
				}
			},
		},
		{method: agentwire.MethodContainerExecResize, params: agentwire.ContainerExecResizeParams{ExecID: "e1"}},
		{method: agentwire.MethodImageTag, params: agentwire.ImageTagParams{Image: "a", Ref: "b"}},
		{
			method: agentwire.MethodImageInspect,
			params: agentwire.NameParams{Name: "img"},
			setup: func(rt *fake.Runtime) {
				rt.ImageInspectFn = func(context.Context, string, ...client.ImageInspectOption) (image.InspectResponse, error) {
					return image.InspectResponse{}, nil
				}
			},
		},
		{
			method: agentwire.MethodImageList,
			params: agentwire.ImageListParams{Filters: agentwire.EncodeFilters(flt)},
			setup: func(rt *fake.Runtime) {
				rt.ImageListFn = func(_ context.Context, opts image.ListOptions) ([]image.Summary, error) {
					if got := opts.Filters.Get("label"); len(got) != 1 || got[0] != managedLabel {
						t.Errorf("image list filter = %v, want the raw filter decoded", got)
					}
					return nil, nil
				}
			},
		},
		{method: agentwire.MethodImageRemove, params: agentwire.ImageRemoveParams{Image: "img"}},
		{method: agentwire.MethodImagesPrune, params: agentwire.PruneParams{Filters: agentwire.EncodeFilters(flt)}},
		{method: agentwire.MethodVolumeCreate, params: agentwire.VolumeCreateParams{}},
		{
			method: agentwire.MethodVolumeInspect,
			params: agentwire.NameParams{Name: "v"},
			setup: func(rt *fake.Runtime) {
				rt.VolumeInspectFn = func(context.Context, string) (volume.Volume, error) {
					return volume.Volume{Name: "v"}, nil
				}
			},
		},
		{
			method: agentwire.MethodVolumeList,
			params: agentwire.VolumeListParams{Filters: agentwire.EncodeFilters(flt)},
			setup: func(rt *fake.Runtime) {
				rt.VolumeListFn = func(context.Context, volume.ListOptions) (volume.ListResponse, error) {
					return volume.ListResponse{}, nil
				}
			},
		},
		{method: agentwire.MethodVolumeRemove, params: agentwire.VolumeRemoveParams{Name: "v", Force: true}},
		{method: agentwire.MethodVolumesPrune, params: agentwire.PruneParams{Filters: agentwire.EncodeFilters(flt)}},
		{method: agentwire.MethodNetworkCreate, params: agentwire.NetworkCreateParams{Name: "n"}},
		{method: agentwire.MethodNetworkConnect, params: agentwire.NetworkConnectParams{Network: "n", Container: "c"}},
		{method: agentwire.MethodNetworkDisconnect, params: agentwire.NetworkDisconnectParams{Network: "n", Container: "c"}},
		{
			method: agentwire.MethodNetworkInspect,
			params: agentwire.NetworkInspectParams{Network: "n"},
			setup: func(rt *fake.Runtime) {
				rt.NetworkInspectFn = func(context.Context, string, network.InspectOptions) (network.Inspect, error) {
					return network.Inspect{}, nil
				}
			},
		},
		{
			method: agentwire.MethodNetworkList,
			params: agentwire.NetworkListParams{Filters: agentwire.EncodeFilters(flt)},
			setup: func(rt *fake.Runtime) {
				rt.NetworkListFn = func(context.Context, network.ListOptions) ([]network.Summary, error) {
					return nil, nil
				}
			},
		},
		{method: agentwire.MethodNetworkRemove, params: agentwire.NameParams{Name: "n"}},
		{method: agentwire.MethodNetworksPrune, params: agentwire.PruneParams{Filters: agentwire.EncodeFilters(flt)}},
		{
			method: agentwire.MethodInfo,
			setup: func(rt *fake.Runtime) {
				rt.InfoFn = func(context.Context) (system.Info, error) { return system.Info{}, nil }
			},
		},
		{
			method: agentwire.MethodServerVersion,
			setup: func(rt *fake.Runtime) {
				rt.ServerVersionFn = func(context.Context) (types.Version, error) { return types.Version{}, nil }
			},
		},
		{
			method: agentwire.MethodDiskUsage,
			params: agentwire.DiskUsageParams{},
			setup: func(rt *fake.Runtime) {
				rt.DiskUsageFn = func(context.Context, types.DiskUsageOptions) (types.DiskUsage, error) {
					return types.DiskUsage{}, nil
				}
			},
		},
		{method: agentwire.MethodRegistryLogin, params: agentwire.RegistryLoginParams{}},
		{method: agentwire.MethodPing},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			rt := &fake.Runtime{}
			if tc.setup != nil {
				tc.setup(rt)
			}
			e := NewExecutor(rt, nil, nil)
			sink := &frameSink{}
			var params json.RawMessage
			if tc.params != nil {
				params = mustParams(t, tc.params)
			}
			e.Execute(context.Background(), agentwire.Command{ID: 1, Method: tc.method, Params: params}, sink.send)

			frames := sink.all()
			if len(frames) != 1 || frames[0].Res == nil {
				t.Fatalf("frames = %+v, want one result", frames)
			}
			if frames[0].Res.Err != nil {
				t.Fatalf("result error = %+v", frames[0].Res.Err)
			}
			found := false
			for _, name := range rt.CallNames() {
				found = found || name == tc.method
			}
			if !found {
				t.Fatalf("daemon calls = %v, want %s", rt.CallNames(), tc.method)
			}
			if tc.check != nil {
				tc.check(t, frames[0].Res)
			}
		})
	}
}

// TestExecutorRejectsMalformedParamsAcrossTheVocabulary sweeps every method
// that carries params with a broken payload: each must answer a typed
// invalid-argument, never reach the daemon or the host tree.
func TestExecutorRejectsMalformedParamsAcrossTheVocabulary(t *testing.T) {
	methods := []string{
		agentwire.MethodContainerCreate, agentwire.MethodContainerStart, agentwire.MethodContainerStop,
		agentwire.MethodContainerRestart, agentwire.MethodContainerRename, agentwire.MethodContainerRemove,
		agentwire.MethodContainerInspect, agentwire.MethodContainerWait, agentwire.MethodContainerList,
		agentwire.MethodContainerStats, agentwire.MethodContainersPrune, agentwire.MethodContainerExecCreate,
		agentwire.MethodContainerExecStart, agentwire.MethodContainerExecInspect, agentwire.MethodContainerExecResize,
		agentwire.MethodImageTag, agentwire.MethodImageInspect, agentwire.MethodImageList,
		agentwire.MethodImageRemove, agentwire.MethodImagesPrune,
		agentwire.MethodVolumeCreate, agentwire.MethodVolumeInspect, agentwire.MethodVolumeList,
		agentwire.MethodVolumeRemove, agentwire.MethodVolumesPrune,
		agentwire.MethodNetworkCreate, agentwire.MethodNetworkConnect, agentwire.MethodNetworkDisconnect,
		agentwire.MethodNetworkInspect, agentwire.MethodNetworkList, agentwire.MethodNetworkRemove,
		agentwire.MethodNetworksPrune, agentwire.MethodDiskUsage, agentwire.MethodRegistryLogin,
		agentwire.MethodFileWrite, agentwire.MethodFileRead, agentwire.MethodFileRemove,
		agentwire.MethodFileStat, agentwire.MethodFileChown, agentwire.MethodFileCopy,
		agentwire.MethodDirEnsure, agentwire.MethodExecToFile, agentwire.MethodFileToExec,
		agentwire.MethodFileToURL, agentwire.MethodURLToFile, agentwire.MethodFileHash,
		agentwire.MethodContainerLogs, agentwire.MethodImagePull, agentwire.MethodImagePush,
		agentwire.MethodImageBuild, agentwire.MethodEvents, agentwire.MethodContainerExecAttach,
		agentwire.MethodIngressExpect, agentwire.MethodIngressCut,
	}
	rt := &fake.Runtime{}
	e := NewExecutor(rt, &hostops.Local{Root: t.TempDir()}, nil)
	e.Ingress = NewIngress(nil)
	for i, m := range methods {
		sink := &frameSink{}
		e.Execute(context.Background(), agentwire.Command{
			ID: int64(i + 1), Method: m, Params: json.RawMessage(`{broken`),
		}, sink.send)
		frames := sink.all()
		if len(frames) != 1 || frames[0].Res == nil || frames[0].Res.Err == nil {
			t.Fatalf("%s: frames = %+v, want one error result", m, frames)
		}
		if frames[0].Res.Err.Code != agentwire.CodeInvalid {
			t.Fatalf("%s: error = %+v, want invalid", m, frames[0].Res.Err)
		}
	}
	if calls := rt.CallNames(); len(calls) != 0 {
		t.Fatalf("malformed params reached the daemon: %v", calls)
	}
}

// TestExecutorRejectsUndecodableFilters pins the RawFilters escape hatch's
// error side: a filter string that cannot decode is a typed invalid-argument,
// never an empty (server-wide!) filter reaching the daemon.
func TestExecutorRejectsUndecodableFilters(t *testing.T) {
	bad := agentwire.RawFilters(`{"label":`)
	cases := map[string]any{
		agentwire.MethodContainerList:   agentwire.ContainerListParams{Filters: bad},
		agentwire.MethodContainersPrune: agentwire.PruneParams{Filters: bad},
		agentwire.MethodImageList:       agentwire.ImageListParams{Filters: bad},
		agentwire.MethodImagesPrune:     agentwire.PruneParams{Filters: bad},
		agentwire.MethodVolumeList:      agentwire.VolumeListParams{Filters: bad},
		agentwire.MethodVolumesPrune:    agentwire.PruneParams{Filters: bad},
		agentwire.MethodNetworkList:     agentwire.NetworkListParams{Filters: bad},
		agentwire.MethodNetworksPrune:   agentwire.PruneParams{Filters: bad},
		agentwire.MethodEvents:          agentwire.EventsParams{Filters: bad},
	}
	rt := &fake.Runtime{}
	e := NewExecutor(rt, nil, nil)
	id := int64(0)
	for m, p := range cases {
		id++
		sink := &frameSink{}
		e.Execute(context.Background(), agentwire.Command{ID: id, Method: m, Params: mustParams(t, p)}, sink.send)
		frames := sink.all()
		if len(frames) != 1 || frames[0].Res == nil || frames[0].Res.Err == nil || frames[0].Res.Err.Code != agentwire.CodeInvalid {
			t.Fatalf("%s: frames = %+v, want invalid filters", m, frames)
		}
	}
	if calls := rt.CallNames(); len(calls) != 0 {
		t.Fatalf("undecodable filters reached the daemon: %v", calls)
	}
}

// TestExecutorContainerWaitSurfacesTheErrorChannel pins the errCh arm of the
// wait select.
func TestExecutorContainerWaitSurfacesTheErrorChannel(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerWaitFn = func(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		errCh := make(chan error, 1)
		errCh <- errors.New("wait broke")
		return make(chan container.WaitResponse), errCh
	}
	e := NewExecutor(rt, nil, nil)
	sink := &frameSink{}
	e.Execute(context.Background(), agentwire.Command{
		ID: 1, Method: agentwire.MethodContainerWait,
		Params: mustParams(t, agentwire.ContainerWaitParams{Name: "c", Condition: container.WaitConditionNotRunning}),
	}, sink.send)
	res := sink.all()[0].Res
	if res.Err == nil || !strings.Contains(res.Err.Message, "wait broke") {
		t.Fatalf("result = %+v, want the wait error surfaced", res)
	}
}

// TestExecutorContainerStatsSurfacesTheDaemonError pins the stats error arm.
func TestExecutorContainerStatsSurfacesTheDaemonError(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerStatsFn = func(context.Context, string, bool) (container.StatsResponseReader, error) {
		return container.StatsResponseReader{}, errors.New("stats broke")
	}
	e := NewExecutor(rt, nil, nil)
	sink := &frameSink{}
	e.Execute(context.Background(), agentwire.Command{
		ID: 1, Method: agentwire.MethodContainerStats,
		Params: mustParams(t, agentwire.NameParams{Name: "c"}),
	}, sink.send)
	if res := sink.all()[0].Res; res.Err == nil {
		t.Fatalf("result = %+v, want the stats error surfaced", res)
	}
}

// TestExecutorHostOpsDispatchAllMethods walks the ADR-054 vocabulary against
// a rooted Local: the file primitives succeed on the mounted tree; the pipe
// and URL primitives — which need a runtime or a remote — answer a typed
// error rather than dispatch elsewhere.
func TestExecutorHostOpsDispatchAllMethods(t *testing.T) {
	root := t.TempDir()
	e := NewExecutor(&fake.Runtime{}, &hostops.Local{Root: root}, nil)

	steps := []struct {
		method  string
		params  any
		wantErr bool
	}{
		{agentwire.MethodDirEnsure, agentwire.DirEnsureParams{Path: root + "/data", Mode: 0o755}, false},
		{agentwire.MethodFileWrite, agentwire.FileWriteParams{Path: root + "/data/a.txt", Content: []byte("payload"), Mode: 0o600}, false},
		{agentwire.MethodFileCopy, agentwire.FileCopyParams{Src: root + "/data/a.txt", Dst: root + "/data/b.txt"}, false},
		{agentwire.MethodFileChown, agentwire.FileChownParams{Path: root + "/data/a.txt", UID: os.Getuid(), GID: os.Getgid()}, false},
		{agentwire.MethodFileStat, agentwire.FileStatParams{Path: root + "/data/a.txt"}, false},
		{agentwire.MethodFileHash, agentwire.FileHashParams{Path: root + "/data/a.txt"}, false},
		{agentwire.MethodFileRemove, agentwire.FileRemoveParams{Path: root + "/data/b.txt"}, false},
		// No runtime wired: the exec pipes must answer an error, not hang.
		{agentwire.MethodExecToFile, agentwire.ExecToFileParams{Container: "c", Cmd: []string{"cat"}, Path: root + "/data/dump", Mode: 0o600}, true},
		{agentwire.MethodFileToExec, agentwire.FileToExecParams{Path: root + "/data/a.txt", Container: "c", Cmd: []string{"cat"}}, true},
		// A source outside the guard: refused before any byte moves.
		{agentwire.MethodFileToURL, agentwire.FileToURLParams{Path: "/etc/passwd", URL: "http://127.0.0.1:1/up"}, true},
		{agentwire.MethodURLToFile, agentwire.URLToFileParams{URL: "http://127.0.0.1:1/down", Path: "/etc/pwned", Mode: 0o600}, true},
	}
	for i, s := range steps {
		sink := &frameSink{}
		e.Execute(context.Background(), agentwire.Command{
			ID: int64(i + 1), Method: s.method, Params: mustParams(t, s.params),
		}, sink.send)
		frames := sink.all()
		if len(frames) != 1 || frames[0].Res == nil {
			t.Fatalf("%s: frames = %+v", s.method, frames)
		}
		if gotErr := frames[0].Res.Err != nil; gotErr != s.wantErr {
			t.Fatalf("%s: error = %+v, wantErr = %v", s.method, frames[0].Res.Err, s.wantErr)
		}
	}

	// The stat and hash answers describe the file actually written.
	sink := &frameSink{}
	e.Execute(context.Background(), agentwire.Command{
		ID: 100, Method: agentwire.MethodFileStat,
		Params: mustParams(t, agentwire.FileStatParams{Path: root + "/data/a.txt"}),
	}, sink.send)
	var st agentwire.FileStatResult
	if err := json.Unmarshal(sink.all()[0].Res.Body, &st); err != nil {
		t.Fatal(err)
	}
	if !st.Found || st.IsDir || st.Size != int64(len("payload")) {
		t.Fatalf("stat = %+v", st)
	}
}

// TestExecutorIngressCommands pins the ADR-060 session-control dispatch: an
// un-enrolled executor answers unavailable; an armed one applies the
// expectation and the cut.
func TestExecutorIngressCommands(t *testing.T) {
	expectParams := mustParams(t, agentwire.IngressExpectParams{
		SessionUUID: "sess1", EndpointUUID: "ep1",
		TokenSHA256: tokenHash("tok"), ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	})
	cutParams := mustParams(t, agentwire.IngressCutParams{SessionUUID: "sess1", Reason: "revoked"})

	// Without the module: both commands answer unavailable.
	bare := NewExecutor(&fake.Runtime{}, nil, nil)
	for i, p := range []json.RawMessage{expectParams, cutParams} {
		method := agentwire.MethodIngressExpect
		if i == 1 {
			method = agentwire.MethodIngressCut
		}
		sink := &frameSink{}
		bare.Execute(context.Background(), agentwire.Command{ID: int64(i + 1), Method: method, Params: p}, sink.send)
		res := sink.all()[0].Res
		if res.Err == nil || res.Err.Code != agentwire.CodeUnavailable {
			t.Fatalf("%s without ingress = %+v, want unavailable", method, res.Err)
		}
	}

	// With the module: the expectation arms, then the cut disarms it.
	e := NewExecutor(&fake.Runtime{}, nil, nil)
	e.Ingress = NewIngress(nil)
	sink := &frameSink{}
	e.Execute(context.Background(), agentwire.Command{ID: 1, Method: agentwire.MethodIngressExpect, Params: expectParams}, sink.send)
	e.Execute(context.Background(), agentwire.Command{ID: 2, Method: agentwire.MethodIngressCut, Params: cutParams}, sink.send)
	for _, f := range sink.all() {
		if f.Res == nil || f.Res.Err != nil {
			t.Fatalf("ingress command answered %+v, want clean results", f)
		}
	}
	if e.Ingress.Cut("sess1", "revoked") {
		t.Fatal("the cut command left the expectation armed")
	}
}

// TestExecutorImagePullAndPushStream pins the two registry streams: the ack,
// the progress chunks, the clean EOF — and the rebuilt SDK options.
func TestExecutorImagePullAndPushStream(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ImagePullFn = func(_ context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error) {
		if ref != "img:1" || !opts.All || opts.RegistryAuth != "auth" || opts.Platform != "linux/amd64" {
			t.Errorf("pull ref=%q opts=%+v, want the params rebuilt", ref, opts)
		}
		return io.NopCloser(strings.NewReader("pulling")), nil
	}
	rt.ImagePushFn = func(_ context.Context, ref string, opts image.PushOptions) (io.ReadCloser, error) {
		if ref != "img:1" || opts.RegistryAuth != "auth" {
			t.Errorf("push ref=%q opts=%+v", ref, opts)
		}
		return io.NopCloser(strings.NewReader("pushing")), nil
	}
	e := NewExecutor(rt, nil, nil)

	for _, tc := range []struct {
		method string
		params any
		want   string
	}{
		{agentwire.MethodImagePull, agentwire.ImagePullParams{Ref: "img:1", All: true, RegistryAuth: "auth", Platform: "linux/amd64"}, "pulling"},
		{agentwire.MethodImagePush, agentwire.ImagePushParams{Ref: "img:1", RegistryAuth: "auth"}, "pushing"},
	} {
		sink := &frameSink{}
		e.Execute(context.Background(), agentwire.Command{ID: 1, Method: tc.method, Params: mustParams(t, tc.params)}, sink.send)
		frames := sink.all()
		if frames[0].Type != agentwire.FrameResult || frames[0].Res.Err != nil {
			t.Fatalf("%s ack = %+v", tc.method, frames[0])
		}
		var payload strings.Builder
		for _, f := range frames[1:] {
			payload.Write(f.Chunk.Data)
		}
		if payload.String() != tc.want {
			t.Fatalf("%s stream = %q, want %q", tc.method, payload.String(), tc.want)
		}
		if last := frames[len(frames)-1]; !last.Chunk.EOF || last.Chunk.Err != nil {
			t.Fatalf("%s end = %+v, want clean EOF", tc.method, last.Chunk)
		}
	}
}

// TestExecutorStreamOpenErrorTravelsInTheResult pins openAndPump's error arm:
// a stream that cannot open answers with the error in the Result, no chunks.
func TestExecutorStreamOpenErrorTravelsInTheResult(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerLogsFn = func(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
		return nil, errors.New("no such container")
	}
	e := NewExecutor(rt, nil, nil)
	sink := &frameSink{}
	e.Execute(context.Background(), agentwire.Command{
		ID: 1, Method: agentwire.MethodContainerLogs,
		Params: mustParams(t, agentwire.ContainerLogsParams{Name: "gone"}),
	}, sink.send)
	frames := sink.all()
	if len(frames) != 1 || frames[0].Res == nil || frames[0].Res.Err == nil {
		t.Fatalf("frames = %+v, want the open error in one result", frames)
	}
}

// brokenSink fails after okFrames successful sends — the channel died
// mid-command.
type brokenSink struct {
	frameSink
	okFrames int
}

func (s *brokenSink) send(f agentwire.Frame) error {
	s.mu.Lock()
	n := len(s.frames)
	s.mu.Unlock()
	if n >= s.okFrames {
		return errors.New("channel gone")
	}
	return s.frameSink.send(f)
}

// TestExecutorSurvivesADeadChannel drives the send-failure arms: the unary
// result lost, the stream ack lost, the mid-stream chunk lost. None may hang
// or panic — the channel's death is routine.
func TestExecutorSurvivesADeadChannel(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerLogsFn = func(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("some logs")), nil
	}
	e := NewExecutor(rt, nil, nil)

	// Unary result lost.
	dead := &brokenSink{}
	e.Execute(context.Background(), agentwire.Command{
		ID: 1, Method: agentwire.MethodContainerStart,
		Params: mustParams(t, agentwire.ContainerStartParams{Name: "c"}),
	}, dead.send)

	// Stream ack lost: the reader must still be opened and closed, no pump.
	dead = &brokenSink{}
	e.Execute(context.Background(), agentwire.Command{
		ID: 2, Method: agentwire.MethodContainerLogs,
		Params: mustParams(t, agentwire.ContainerLogsParams{Name: "c"}),
	}, dead.send)

	// Mid-stream chunk lost after a successful ack.
	half := &brokenSink{okFrames: 1}
	e.Execute(context.Background(), agentwire.Command{
		ID: 3, Method: agentwire.MethodContainerLogs,
		Params: mustParams(t, agentwire.ContainerLogsParams{Name: "c"}),
	}, half.send)
	if frames := half.all(); len(frames) != 1 || frames[0].Type != agentwire.FrameResult {
		t.Fatalf("frames = %+v, want only the ack before the channel died", frames)
	}
}

// TestExecutorImageBuildRequiresTheHostMount pins ADR-055's degradation: no
// mounted tree answers a typed unavailable; a mounted tree still refuses a
// context escaping the guard.
func TestExecutorImageBuildRequiresTheHostMount(t *testing.T) {
	params := mustParams(t, agentwire.ImageBuildParams{
		ContextDir: "/srv/app", Dockerfile: "Dockerfile", Tags: []string{"app:1"},
	})

	e := NewExecutor(&fake.Runtime{}, nil, nil)
	sink := &frameSink{}
	e.Execute(context.Background(), agentwire.Command{ID: 1, Method: agentwire.MethodImageBuild, Params: params}, sink.send)
	res := sink.all()[0].Res
	if res.Err == nil || res.Err.Code != agentwire.CodeUnavailable {
		t.Fatalf("build without mount = %+v, want unavailable", res.Err)
	}

	e = NewExecutor(&fake.Runtime{}, &hostops.Local{Root: t.TempDir()}, nil)
	sink = &frameSink{}
	e.Execute(context.Background(), agentwire.Command{ID: 2, Method: agentwire.MethodImageBuild, Params: params}, sink.send)
	res = sink.all()[0].Res
	if res.Err == nil || res.Err.Code != agentwire.CodeInvalid {
		t.Fatalf("build outside the guard = %+v, want invalid", res.Err)
	}
}

// TestExecutorPumpEvents drives the Events stream: each daemon message is one
// JSON chunk; the stream ends on the error channel — as a clean EOF when it
// closes, as a carried error when it fails — or on cancellation.
func TestExecutorPumpEvents(t *testing.T) {
	newEventsRuntime := func() (*fake.Runtime, chan events.Message, chan error) {
		msgs := make(chan events.Message)
		errs := make(chan error)
		rt := &fake.Runtime{}
		rt.EventsFn = func(_ context.Context, opts events.ListOptions) (<-chan events.Message, <-chan error) {
			if got := opts.Filters.Get("label"); len(got) != 1 || got[0] != managedLabel {
				t.Errorf("events filter = %v, want the raw filter decoded", got)
			}
			return msgs, errs
		}
		return rt, msgs, errs
	}
	flt := filters.NewArgs(filters.Arg("label", managedLabel))
	params := mustParams(t, agentwire.EventsParams{Filters: agentwire.EncodeFilters(flt)})

	t.Run("message then broken stream", func(t *testing.T) {
		rt, msgs, errs := newEventsRuntime()
		e := NewExecutor(rt, nil, nil)
		sink := &frameSink{}
		go func() {
			msgs <- events.Message{Action: "start", Actor: events.Actor{Attributes: map[string]string{"name": "c1"}}}
			errs <- io.ErrUnexpectedEOF
		}()
		e.Execute(context.Background(), agentwire.Command{ID: 1, Method: agentwire.MethodEvents, Params: params}, sink.send)

		frames := sink.all()
		if len(frames) != 3 || frames[0].Res == nil || frames[0].Res.Err != nil {
			t.Fatalf("frames = %+v, want ack + message + error chunk", frames)
		}
		var m events.Message
		if err := json.Unmarshal(frames[1].Chunk.Data, &m); err != nil || m.Action != "start" {
			t.Fatalf("event chunk = %s (%v)", frames[1].Chunk.Data, err)
		}
		if frames[2].Chunk.Err == nil {
			t.Fatalf("end chunk = %+v, want the stream error carried", frames[2].Chunk)
		}
	})

	t.Run("closed error channel is a clean EOF", func(t *testing.T) {
		rt, _, errs := newEventsRuntime()
		e := NewExecutor(rt, nil, nil)
		sink := &frameSink{}
		go close(errs)
		e.Execute(context.Background(), agentwire.Command{ID: 2, Method: agentwire.MethodEvents, Params: params}, sink.send)

		frames := sink.all()
		last := frames[len(frames)-1]
		if !last.Chunk.EOF || last.Chunk.Err != nil {
			t.Fatalf("end chunk = %+v, want clean EOF", last.Chunk)
		}
	})

	t.Run("cancellation ends the stream with a canceled chunk", func(t *testing.T) {
		rt, _, _ := newEventsRuntime()
		e := NewExecutor(rt, nil, nil)
		sink := &frameSink{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		e.Execute(ctx, agentwire.Command{ID: 3, Method: agentwire.MethodEvents, Params: params}, sink.send)

		frames := sink.all()
		last := frames[len(frames)-1]
		if last.Chunk == nil || last.Chunk.Err == nil || last.Chunk.Err.Code != agentwire.CodeCanceled {
			t.Fatalf("end chunk = %+v, want canceled", last)
		}
	})

	t.Run("dead channel stops the pump", func(t *testing.T) {
		rt, _, _ := newEventsRuntime()
		e := NewExecutor(rt, nil, nil)
		dead := &brokenSink{}
		e.Execute(context.Background(), agentwire.Command{ID: 4, Method: agentwire.MethodEvents, Params: params}, dead.send)
		if frames := dead.all(); len(frames) != 0 {
			t.Fatalf("frames = %+v, want none once the channel died", frames)
		}
	})
}

// TestExecutorAttachErrorsTravelInTheResult pins attachExec's failure arm: a
// daemon refusing the attach answers with the error, no session recorded.
func TestExecutorAttachErrorsTravelInTheResult(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerExecAttachFn = func(context.Context, string, container.ExecAttachOptions) (types.HijackedResponse, error) {
		return types.HijackedResponse{}, errors.New("no such exec")
	}
	e := NewExecutor(rt, nil, nil)
	sink := &frameSink{}
	e.Execute(context.Background(), agentwire.Command{
		ID: 1, Method: agentwire.MethodContainerExecAttach,
		Params: mustParams(t, agentwire.ContainerExecAttachParams{ExecID: "gone"}),
	}, sink.send)
	frames := sink.all()
	if len(frames) != 1 || frames[0].Res.Err == nil {
		t.Fatalf("frames = %+v, want the attach error in one result", frames)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.attaches) != 0 {
		t.Fatal("a failed attach left a session behind")
	}
}

// TestDeliverInputEdges pins the input router's guards: a nil chunk is a
// no-op, and a write onto a dead session is dropped with a warning, never a
// panic — the daemon socket closing under an attach is routine.
func TestDeliverInputEdges(t *testing.T) {
	e := NewExecutor(&fake.Runtime{}, nil, nil)
	e.DeliverInput(nil)

	c1, c2 := net.Pipe()
	_ = c2.Close()
	_ = c1.Close()
	e.mu.Lock()
	e.attaches[7] = &types.HijackedResponse{Conn: c1}
	e.mu.Unlock()
	e.DeliverInput(&agentwire.StreamChunk{ID: 7, Data: []byte("late input")})
	e.DeliverInput(&agentwire.StreamChunk{ID: 7, EOF: true})
}
