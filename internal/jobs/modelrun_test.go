package jobs

// The model lifecycle (ADR-080) at its load-bearing joints: the container
// contract (device request, IPC/shm, ulimits, shared cache, per-engine
// command — the ADR-079/080 verification list), the image defaults per
// architecture, the swap ordering, and the readiness budget's fast failure.

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/inference"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

func TestModelImage(t *testing.T) {
	base := store.Model{Engine: store.InferenceEngineVllm}
	if got := ModelImage(base, "amd64"); got != DefaultVLLMImage {
		t.Fatalf("vllm amd64 default = %q", got)
	}
	// The ADR-080 case in the flesh: vLLM publishes a distinct aarch64 tag.
	if got := ModelImage(base, "arm64"); got != DefaultVLLMImageARM64 {
		t.Fatalf("vllm arm64 default = %q", got)
	}
	base.Engine = store.InferenceEngineSglang
	if got := ModelImage(base, "amd64"); got != DefaultSGLangImage {
		t.Fatalf("sglang default = %q", got)
	}
	custom, tag := "ghcr.io/x/vllm-gb10", "cu13"
	base.Image, base.ImageTag = &custom, &tag
	if got := ModelImage(base, "arm64"); got != "ghcr.io/x/vllm-gb10:cu13" {
		t.Fatalf("override = %q", got)
	}
}

func TestModelInferenceConfig(t *testing.T) {
	served, quant := "llama", "awq"
	maxLen, frac := int32(8192), 0.85
	m := store.Model{
		Engine: store.InferenceEngineSglang, ModelID: "org/m", TensorParallelSize: 2,
		ServedModelName: &served, Quantization: &quant, MaxModelLen: &maxLen, MemoryFraction: &frac,
		EngineFlags: []byte(`[{"flag":"--enable-torch-compile"},{"flag":"--kv-cache-dtype","value":"fp8"}]`),
	}
	cfg, err := ModelInferenceConfig(m)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != inference.EngineSGLang || cfg.ModelID != "org/m" || cfg.TensorParallel != 2 ||
		cfg.ServedModelName != "llama" || cfg.MaxModelLen != 8192 || len(cfg.Flags) != 2 {
		t.Fatalf("cfg = %+v", cfg)
	}
	m.EngineFlags = []byte("not json")
	if _, err := ModelInferenceConfig(m); err == nil {
		t.Fatal("corrupt engine_flags must refuse, not render a wrong command")
	}
}

// modelrunFixture hand-builds the row (the fake DB cannot hold a ciphertext
// and a JSON list in the same []byte fill) and wires the fakes.
func modelrunFixture(t *testing.T, engine store.InferenceEngine, shmMB *int32) (*ModelRun, store.GetModelByIDRow, *fake.Runtime) {
	t.Helper()
	q, keyring, logger, db := prevjobsDeps(t)
	db.strs["GetServerByID"] = "192.168.10.20"
	uuid := pguuid.MustParse(jobFixtureUUID)
	enc := prevjobsEncrypt(t, keyring, "models", "api_key_enc", []byte("akm_unit_key"))
	row := store.GetModelByIDRow{
		Resource: store.Resource{ID: 7, Uuid: uuid, TeamID: 1, DestinationID: 1},
		Model: store.Model{
			ID: 7, Engine: engine, ModelID: "org/m", TensorParallelSize: 1,
			EngineFlags: []byte(`[{"flag":"--enable-prefix-caching"}]`),
			ApiKeyEnc:   enc, PublishedPort: 18001, ServerID: 1, ShmSizeMb: shmMB,
		},
	}
	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{
			State: &container.State{Status: "running", Health: &container.Health{Status: "healthy"}},
		}}, nil
	}
	rt.VolumeInspectFn = func(context.Context, string) (volume.Volume, error) { return volume.Volume{}, nil }
	rt.ContainerRemoveFn = func(context.Context, string, container.RemoveOptions) error { return nil }
	rt.ContainerStartFn = func(context.Context, string, container.StartOptions) error { return nil }
	h := &ModelRun{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger, HFToken: "hf_unit"}
	return h, row, rt
}

// The container contract, both engines (ADR-079 §2 + ADR-080 §4): device
// request for all GPUs, host IPC by default / explicit shm when asked,
// the documented ulimits, the ONE shared HF cache, the published port, the
// per-engine command shape, and the HF token as env — never argv.
func TestModelProvisionContainerContract(t *testing.T) {
	shrink := func(t *testing.T) {
		t.Helper()
		previous := modelReadyBudget
		modelReadyBudget = time.Second
		t.Cleanup(func() { modelReadyBudget = previous })
	}

	t.Run("vllm: flags-only command, host IPC", func(t *testing.T) {
		shrink(t)
		h, row, rt := modelrunFixture(t, store.InferenceEngineVllm, nil)
		var config *container.Config
		var host *container.HostConfig
		rt.ContainerCreateFn = func(_ context.Context, c *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			config, host = c, hc
			return container.CreateResponse{}, nil
		}
		if err := h.provision(context.Background(), rt, row, jobFixtureUUID); err != nil {
			t.Fatal(err)
		}
		if config.Cmd[0] != "--model" {
			t.Fatalf("the vLLM container command must be flags alone, got %q first", config.Cmd[0])
		}
		joined := strings.Join(config.Cmd, " ")
		if !strings.Contains(joined, "--api-key akm_unit_key") || !strings.Contains(joined, "--enable-prefix-caching") {
			t.Fatalf("cmd = %s", joined)
		}
		dr := host.DeviceRequests
		if len(dr) != 1 || dr[0].Count != -1 || dr[0].Capabilities[0][0] != "gpu" {
			t.Fatalf("device requests = %+v", dr)
		}
		if host.IpcMode != container.IPCModeHost || host.ShmSize != 0 {
			t.Fatalf("ipc = %q shm = %d — host IPC is the default", host.IpcMode, host.ShmSize)
		}
		if len(host.Ulimits) != 2 {
			t.Fatalf("ulimits = %+v", host.Ulimits)
		}
		if host.Binds[0] != HFCacheVolume+":/root/.cache/huggingface" {
			t.Fatalf("binds = %v — the cache is server-scoped and shared", host.Binds)
		}
		if _, ok := host.PortBindings["8000/tcp"]; !ok {
			t.Fatalf("port bindings = %+v", host.PortBindings)
		}
		var hasToken bool
		for _, e := range config.Env {
			if e == "HF_TOKEN=hf_unit" {
				hasToken = true
			}
		}
		if !hasToken {
			t.Fatalf("HF token must ride env, never argv: %v", config.Env)
		}
	})

	t.Run("sglang: full invocation, explicit shm replaces host IPC", func(t *testing.T) {
		shrink(t)
		shm := int32(4096)
		h, row, rt := modelrunFixture(t, store.InferenceEngineSglang, &shm)
		var config *container.Config
		var host *container.HostConfig
		rt.ContainerCreateFn = func(_ context.Context, c *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			config, host = c, hc
			return container.CreateResponse{}, nil
		}
		if err := h.provision(context.Background(), rt, row, jobFixtureUUID); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(config.Cmd[:3], " "); got != "python3 -m sglang.launch_server" {
			t.Fatalf("the SGLang command must be the full invocation, got %q", got)
		}
		if host.IpcMode == container.IPCModeHost || host.ShmSize != int64(shm)*1024*1024 {
			t.Fatalf("ipc = %q shm = %d — an explicit shm size replaces host IPC", host.IpcMode, host.ShmSize)
		}
	})

	t.Run("a container that exits during startup fails fast, logs attached", func(t *testing.T) {
		shrink(t)
		h, row, rt := modelrunFixture(t, store.InferenceEngineVllm, nil)
		rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
			return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{
				State: &container.State{Status: "exited"},
			}}, nil
		}
		rt.ContainerLogsFn = func(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		}
		err := h.provision(context.Background(), rt, row, jobFixtureUUID)
		if err == nil || !strings.Contains(err.Error(), "exited during startup") {
			t.Fatalf("err = %v", err)
		}
	})
}

// Execute: stop is a state, and the swap stops the neighbour BEFORE anything
// claims the GPU — one job, ordered by construction (ADR-080 §5).
func TestModelExecuteLifecycleAndSwap(t *testing.T) {
	ctx := context.Background()

	t.Run("stop records the stopped state", func(t *testing.T) {
		q, _, logger, _ := prevjobsDeps(t)
		rt := &fake.Runtime{}
		stopped := false
		rt.ContainerStopFn = func(context.Context, string, container.StopOptions) error {
			stopped = true
			return nil
		}
		h := &ModelRun{Store: q, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}
		job := store.Job{ID: 1, JobType: TypeModelStop, Payload: []byte(`{"resource_id":7,"action":"stop"}`)}
		if _, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job)); err != nil {
			t.Fatal(err)
		}
		if !stopped {
			t.Fatal("stop never reached the container")
		}
	})

	t.Run("a swap start stops the neighbour first", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		rt := &fake.Runtime{}
		var order []string
		rt.ContainerStopFn = func(context.Context, string, container.StopOptions) error {
			order = append(order, "stop")
			return nil
		}
		rt.ContainerCreateFn = func(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error) {
			order = append(order, "create")
			return container.CreateResponse{}, context.Canceled
		}
		rt.VolumeInspectFn = func(context.Context, string) (volume.Volume, error) { return volume.Volume{}, nil }
		rt.ContainerRemoveFn = func(context.Context, string, container.RemoveOptions) error { return nil }
		h := &ModelRun{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}
		job := store.Job{ID: 1, JobType: TypeModelStart, Payload: []byte(`{"resource_id":7,"action":"start","stop_resource_id":9}`)}
		// The provision half fails on purpose (the fixture's key does not
		// decrypt): what this test pins is that the neighbour's stop already
		// HAPPENED, first, inside the same job — order by construction.
		_, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job))
		if err == nil {
			t.Fatal("the scripted provision failure must surface")
		}
		if len(order) == 0 || order[0] != "stop" {
			t.Fatalf("order = %v — the neighbour must stop before the GPU is claimed", order)
		}
	})

	t.Run("an invalid payload never reaches a server", func(t *testing.T) {
		q, _, logger, _ := prevjobsDeps(t)
		h := &ModelRun{Store: q, Docker: fixedSource{}, HostOps: fixedHost{}, Logger: logger}
		bad := store.Job{ID: 1, JobType: TypeModelStop, Payload: []byte(`{`)}
		if _, err := h.Execute(ctx, bad, queue.NewStepRecorder(q, bad)); err == nil {
			t.Fatal("want a payload error")
		}
	})
}
