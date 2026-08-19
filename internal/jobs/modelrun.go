package jobs

// Model lifecycle (ADR-080): the databases job family, GPU-aware (ADR-079).
// The container carries the device request, host IPC (or an explicit shm
// size), the engines' documented ulimits, and the server-scoped HF cache
// volume; the serve command comes from THE renderer (internal/inference) —
// the same one the dashboard exports and the paste-import round-trips.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/go-connections/nat"
	"github.com/docker/go-units"

	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/inference"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// The model job types — one handler, the databases pattern.
const (
	TypeModelProvision = "model.provision"
	TypeModelStart     = "model.start"
	TypeModelStop      = "model.stop"
	TypeModelRestart   = "model.restart"
	TypeModelDelete    = "model.delete"
)

// Default images, pinned (never latest, instance-config §4.1) and per
// architecture — the ADR-080 case in the flesh: vLLM publishes a distinct
// aarch64 variant, and a GB10 server typically overrides with an sm_121a
// community build anyway.
const (
	DefaultVLLMImage        = "vllm/vllm-openai:v0.27.1"
	DefaultVLLMImageARM64   = "vllm/vllm-openai:v0.27.1-aarch64"
	DefaultSGLangImage      = "lmsysorg/sglang:v0.5.17"
	DefaultSGLangImageARM64 = "lmsysorg/sglang:v0.5.17"
)

// HFCacheVolume is the ONE weights cache per server (ADR-080 §4): mounted in
// every model container, never removed with a model — weights are tens of
// gigabytes and shared.
const HFCacheVolume = "akerdock-hf-cache"

// modelReadyBudget bounds the wait for the engine to answer — weight loading
// is minutes, not seconds, and a loading model is not a dead one (ADR-080
// §4). A var so tests do not wait.
var modelReadyBudget = 15 * time.Minute

// modelMaxRestarts is where "still loading" stops being a defensible reading
// of a container that keeps dying. The container carries `unless-stopped`, so
// an engine that exits — bad flag, weights that do not fit the GPU — is
// relaunched by Docker within a second and the poll below sees `running`
// again, never the `exited` that used to be the only failure signal. Waiting
// out the full readiness budget on a model that has already died three times
// serves nobody: the budget exists for slow loading, not for a loop.
const modelMaxRestarts = 2

// modelProbeTimeout bounds ONE observation of the container. The commands ride
// the agent channel, and a channel whose peer vanished without closing its
// socket answers nothing at all — without a deadline the poll below would
// block on a single inspect for as long as the kernel retransmits, which is a
// quarter of an hour of a job that looks alive and cannot be stopped.
const modelProbeTimeout = 30 * time.Second

// errModelCancelled ends a model job at a cooperative checkpoint (§2.6) — the
// operator asked, the container is put back to a state they can act on, and
// the job is a cancellation, not a failure.
var errModelCancelled = errors.New("cancelled at the operator's request")

// ModelPayload references the model and, for a swap start (ADR-080 §5), the
// running model the operator confirmed stopping first — one job, so the
// order is a program, not a race between two queue entries.
type ModelPayload struct {
	ResourceID int64  `json:"resource_id"`
	Action     string `json:"action"`
	// StopResourceID, on a start, is the model to stop FIRST (the one-click
	// swap). Zero means none.
	StopResourceID int64 `json:"stop_resource_id,omitempty"`
}

// ModelRun executes the model lifecycle on the agent channel.
type ModelRun struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Docker  dockerruntime.Source
	HostOps hostops.Source
	Logger  *slog.Logger
	// HFToken, when set (AKERDOCK_HF_TOKEN), reaches the engine as the
	// HF_TOKEN env for gated models — never argv (INV-003).
	HFToken string
}

// Execute runs one model action.
func (h *ModelRun) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload ModelPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	row, err := h.Store.GetModelByID(ctx, payload.ResourceID)
	if err != nil {
		if payload.Action == "delete" {
			return map[string]any{"status": "already deleted"}, nil
		}
		return nil, fmt.Errorf("model not found: %w", err)
	}
	modelUUID := pguuid.String(row.Resource.Uuid)

	// The first checkpoint is before any work at all: a job cancelled while it
	// waited in the queue can still be picked up — the lease is taken before
	// the worker reads the flag — and starting an engine the operator has
	// already called off is the one outcome nobody wants.
	if h.cancelRequested(ctx, job.ID) {
		rec.Start(ctx, payload.Action)
		rec.Fail(ctx, "cancelled at the operator's request")
		h.Logger.Info("model job cancelled before it started", "action", payload.Action, "model_uuid", modelUUID)
		return map[string]any{"action": payload.Action, "model_uuid": modelUUID, "status": "cancelled"}, nil
	}

	rec.Start(ctx, payload.Action)
	rt, err := h.Docker.Runtime(ctx, row.Model.ServerID)
	if err != nil {
		rec.Fail(ctx, "the server's agent is not connected")
		return nil, err
	}

	// The swap half of a start (ADR-080 §5): stop the confirmed neighbour
	// BEFORE anything claims the GPU memory — inside the same job, so the
	// order cannot race.
	if payload.Action == "start" && payload.StopResourceID != 0 {
		if other, err := h.Store.GetModelByID(ctx, payload.StopResourceID); err == nil {
			if err := h.lifecycle(ctx, rt, "stop", pguuid.String(other.Resource.Uuid), other.Resource.ID); err != nil {
				rec.Fail(ctx, "stopping the running model failed: "+firstLine(err.Error()))
				return nil, err
			}
		}
	}

	switch payload.Action {
	case "provision", "start":
		// A start converges: the container is recreated from the current
		// configuration — serve flags are read once, at process start
		// (ADR-080 §5), so "start after an update" and "provision" are the
		// same act.
		err = h.provision(ctx, rt, row, modelUUID, job.ID)
	case "stop", "restart":
		err = h.lifecycle(ctx, rt, payload.Action, modelUUID, row.Resource.ID)
	case "delete":
		err = h.delete(ctx, rt, row, modelUUID)
	default:
		err = fmt.Errorf("unknown model action %q", payload.Action)
	}
	// A cancellation is an outcome, not a defect: the job SUCCEEDS at having
	// stopped, so the queue does not retry what the operator just interrupted.
	if errors.Is(err, errModelCancelled) {
		rec.Fail(ctx, "cancelled at the operator's request")
		h.Logger.Info("model job cancelled", "action", payload.Action, "model_uuid", modelUUID)
		return map[string]any{"action": payload.Action, "model_uuid": modelUUID, "status": "cancelled"}, nil
	}
	if err != nil {
		rec.Fail(ctx, firstLine(err.Error()))
		return nil, err
	}
	rec.Succeed(ctx, payload.Action+" completed")
	h.Logger.Info("model job done", "action", payload.Action, "model_uuid", modelUUID)
	return map[string]any{"action": payload.Action, "model_uuid": modelUUID}, nil
}

// ModelImage resolves the image: the override when set, the per-engine,
// per-architecture default otherwise (ADR-080 §1).
func ModelImage(m store.Model, architecture string) string {
	if m.Image != nil && *m.Image != "" {
		image := *m.Image
		if m.ImageTag != nil && *m.ImageTag != "" && !strings.Contains(image, ":") {
			image += ":" + *m.ImageTag
		}
		return image
	}
	arm := architecture == "arm64"
	if m.Engine == store.InferenceEngineSglang {
		if arm {
			return DefaultSGLangImageARM64
		}
		return DefaultSGLangImage
	}
	if arm {
		return DefaultVLLMImageARM64
	}
	return DefaultVLLMImage
}

// ModelInferenceConfig assembles the renderer's Config from the row — one
// place, so the deployment, the export and the import cannot disagree
// (ADR-080 §3bis).
func ModelInferenceConfig(m store.Model) (inference.Config, error) {
	var flags []inference.Flag
	if len(m.EngineFlags) > 0 {
		if err := json.Unmarshal(m.EngineFlags, &flags); err != nil {
			return inference.Config{}, fmt.Errorf("engine_flags: %w", err)
		}
	}
	cfg := inference.Config{
		Engine:         inference.Engine(m.Engine),
		ModelID:        m.ModelID,
		TensorParallel: int(m.TensorParallelSize),
		Flags:          flags,
	}
	if m.ServedModelName != nil {
		cfg.ServedModelName = *m.ServedModelName
	}
	if m.Quantization != nil {
		cfg.Quantization = *m.Quantization
	}
	if m.MaxModelLen != nil {
		cfg.MaxModelLen = int(*m.MaxModelLen)
	}
	if m.MemoryFraction != nil {
		cfg.MemoryFraction = float64(*m.MemoryFraction)
	}
	return cfg, nil
}

// provision recreates and starts the model container: GPU device request,
// host IPC or explicit shm, ulimits, shared HF cache, published LAN port,
// the API key on the engine's own flag — and a readiness budget in minutes.
func (h *ModelRun) provision(ctx context.Context, rt dockerruntime.Runtime, row store.GetModelByIDRow, modelUUID string, jobID int64) error {
	apiKey, err := h.Keyring.Decrypt("models", "api_key_enc", modelUUID, row.Model.ApiKeyEnc)
	if err != nil {
		return err
	}
	cfg, err := ModelInferenceConfig(row.Model)
	if err != nil {
		return err
	}
	server, err := h.Store.GetServerByID(ctx, row.Model.ServerID)
	if err != nil {
		return err
	}
	dest, err := h.Store.GetDestinationByID(ctx, row.Resource.DestinationID)
	if err != nil {
		return err
	}
	arch := ""
	if server.Architecture != nil {
		arch = *server.Architecture
	}
	image := ModelImage(row.Model, arch)

	team := ""
	if t, err := h.Store.GetTeamByID(ctx, row.Resource.TeamID); err == nil {
		team = pguuid.String(t.Uuid)
	}
	labels := map[string]string{
		"akerdock.managed":       "true",
		"akerdock.resource_uuid": modelUUID,
		"akerdock.type":          "model",
		"akerdock.team_uuid":     team,
	}

	// The server-scoped weights cache (ADR-080 §4): created once, mounted in
	// every model container, never tied to one model's lifetime.
	if _, err := rt.VolumeInspect(ctx, HFCacheVolume); err != nil {
		if !dockerruntime.IsNotFound(err) {
			return err
		}
		if _, err := rt.VolumeCreate(ctx, volume.CreateOptions{
			Name: HFCacheVolume, Labels: map[string]string{"akerdock.managed": "true"},
		}); err != nil {
			return err
		}
	}

	env := []string{"HF_HOME=/root/.cache/huggingface"}
	// The server's own token wins over the instance fallback (ADR-081).
	hfToken, err := ModelHFToken(h.Keyring, server, h.HFToken)
	if err != nil {
		return err
	}
	if hfToken != "" {
		env = append(env, "HF_TOKEN="+hfToken)
	}
	// The resource's own variables, LAST: the operator's explicit variable
	// wins over anything managed — Docker keeps the final occurrence of a
	// duplicated key. Same machinery as every resource (ADR-080 §1): shared
	// {{scope.KEY}} references resolve, server-scoped variables inject
	// unless the model overrides the key.
	userEnv, err := h.renderModelEnv(ctx, row.Resource.ID)
	if err != nil {
		return err
	}
	env = append(env, userEnv...)

	config := &container.Config{
		Image: image, Env: env, Labels: labels,
		Cmd: inference.ContainerCommand(cfg, string(apiKey)),
		// The engine's own health signal: /health answers while loading too
		// slowly for TCP checks to mean anything, so the start period is the
		// generous one and readiness is judged below, not here.
		Healthcheck: &container.HealthConfig{
			Test: []string{"CMD-SHELL", "python3 -c \"import urllib.request; urllib.request.urlopen('http://127.0.0.1:" +
				strconv.Itoa(inference.ContainerPort) + "/health', timeout=5)\""},
			Interval: 10 * time.Second, Retries: 3, StartPeriod: 2 * time.Minute,
		},
		ExposedPorts: nat.PortSet{modelContainerPort(): struct{}{}},
	}
	host := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		NetworkMode:   container.NetworkMode(dest.Network),
		Binds:         []string{HFCacheVolume + ":/root/.cache/huggingface"},
		PortBindings:  nat.PortMap{modelContainerPort(): {{HostPort: strconv.Itoa(int(row.Model.PublishedPort))}}},
		// ADR-079 §2: all GPUs — one accelerator per server is the fleet this
		// serves — plus the ulimits the engines' own examples mandate.
		Resources: container.Resources{
			DeviceRequests: []container.DeviceRequest{{Count: -1, Capabilities: [][]string{{"gpu"}}}},
			Ulimits: []*units.Ulimit{
				{Name: "memlock", Soft: -1, Hard: -1},
				{Name: "stack", Soft: 67108864, Hard: 67108864},
			},
		},
	}
	// Host IPC is what both engines document first; an explicit shm size is
	// the alternative for operators who refuse host IPC — the two are
	// mutually exclusive by Docker's own semantics.
	if row.Model.ShmSizeMb != nil {
		host.ShmSize = int64(*row.Model.ShmSizeMb) * 1024 * 1024
	} else {
		host.IpcMode = container.IPCModeHost
	}

	if err := removeNamedContainers(ctx, rt, false, modelUUID); err != nil {
		return err
	}
	if _, err := rt.ContainerCreate(ctx, config, host, nil, nil, modelUUID); err != nil {
		return fmt.Errorf("creating the model container failed: %s", firstLine(err.Error()))
	}
	if err := rt.ContainerStart(ctx, modelUUID, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting the model container failed: %s", firstLine(err.Error()))
	}
	if err := h.waitModelReady(ctx, rt, modelUUID, jobID); err != nil {
		// The container carries `unless-stopped`, which Docker honours against
		// everything EXCEPT an explicit stop — so a start that failed must
		// stop what it created, or the engine keeps crash-looping on the GPU
		// long after the job is over, holding memory nobody can account for.
		// Stopped, not removed: the logs are the operator's evidence.
		h.stopAfterFailedStart(ctx, rt, modelUUID)
		observed := store.ResourceObservedStatusUnhealthy
		if errors.Is(err, errModelCancelled) {
			observed = store.ResourceObservedStatusExited
		}
		_ = h.Store.SetResourceDesiredStatus(ctx, store.SetResourceDesiredStatusParams{
			ID: row.Resource.ID, DesiredStatus: store.ResourceDesiredStatusStopped,
		})
		_ = h.Store.SetResourceObservedStatus(ctx, store.SetResourceObservedStatusParams{
			ID: row.Resource.ID, ObservedStatus: observed,
		})
		return err
	}

	_ = h.Store.SetResourceDesiredStatus(ctx, store.SetResourceDesiredStatusParams{
		ID: row.Resource.ID, DesiredStatus: store.ResourceDesiredStatusRunning,
	})
	_ = h.Store.SetResourceObservedStatus(ctx, store.SetResourceObservedStatusParams{
		ID: row.Resource.ID, ObservedStatus: store.ResourceObservedStatusHealthy,
	})
	return nil
}

// stopAfterFailedStart silences the restart policy on the way out. Best
// effort and bounded: the reason the start failed may well be the reason this
// cannot be delivered either, and a job that hangs on its own cleanup is the
// defect it is trying to compensate. context.WithoutCancel because a
// cancelled job must still clean up after itself.
func (h *ModelRun) stopAfterFailedStart(ctx context.Context, rt dockerruntime.Runtime, modelUUID string) {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelProbeTimeout)
	defer cancel()
	grace := 10
	if err := rt.ContainerStop(stopCtx, modelUUID, container.StopOptions{Timeout: &grace}); err != nil && !dockerruntime.IsNotFound(err) {
		h.Logger.Warn("could not stop the model container after a failed start",
			"model_uuid", modelUUID, "error", err)
	}
}

func modelContainerPort() nat.Port {
	return nat.Port(strconv.Itoa(inference.ContainerPort) + "/tcp")
}

// renderModelEnv decrypts the model's variables into KEY=VALUE, resolving
// shared references and injecting server-scoped variables the model does not
// override — the resource machinery, unchanged (ADR-080 §1).
func (h *ModelRun) renderModelEnv(ctx context.Context, resourceID int64) ([]string, error) {
	vars, err := h.Store.ListEnvVarsForDeploy(ctx, resourceID)
	if err != nil {
		return nil, err
	}
	shared, err := resolveSharedEnv(ctx, h.Store, h.Keyring, resourceID)
	if err != nil {
		return nil, err
	}
	entries := make([]string, 0, len(vars))
	seen := map[string]bool{}
	for _, v := range vars {
		plaintext, err := h.Keyring.Decrypt("environment_variables", "value_enc", pguuid.String(v.Uuid), v.ValueEnc)
		if err != nil {
			return nil, fmt.Errorf("decrypt variable %s: %w", v.Key, err)
		}
		value := string(plaintext)
		if !v.IsLiteral {
			value = shared.interpolate(value)
		}
		entries = append(entries, v.Key+"="+value)
		seen[v.Key] = true
	}
	serverKeys := make([]string, 0, len(shared.server))
	for k := range shared.server {
		serverKeys = append(serverKeys, k)
	}
	sort.Strings(serverKeys)
	for _, k := range serverKeys {
		if seen[k] {
			continue // the model's own variable wins
		}
		entries = append(entries, k+"="+shared.server[k])
	}
	return entries, nil
}

// waitModelReady waits on the container's health with a minutes-scale budget
// (weight loading IS the workload's cost) — and gives up early on the three
// things that are not slow loading: a container that exited, one that keeps
// being restarted by its own restart policy, and an operator who asked to
// stop. The five-second poll is the job family's only checkpoint, so it
// carries the cancellation check too (§2.6).
func (h *ModelRun) waitModelReady(ctx context.Context, rt dockerruntime.Runtime, modelUUID string, jobID int64) error {
	deadline := time.Now().Add(modelReadyBudget)
	for {
		if h.cancelRequested(ctx, jobID) {
			return errModelCancelled
		}
		inspect, err := h.inspectBounded(ctx, rt, modelUUID)
		if err != nil {
			return err
		}
		if inspect.State != nil {
			if inspect.State.Health != nil && inspect.State.Health.Status == "healthy" {
				return nil
			}
			if inspect.State.Status == "exited" || inspect.State.Status == "dead" {
				tail, _ := containerLogsTail(ctx, rt, modelUUID, 20)
				return fmt.Errorf("the model container exited during startup — %s", firstLine(strings.TrimSpace(tail)))
			}
		}
		// The restart counter is what makes a crash loop visible: the exit
		// itself lasts a fraction of a second and a five-second poll almost
		// never lands on it, so a dying engine would otherwise present as a
		// container that is simply "running" until the budget runs out.
		if inspect.RestartCount > modelMaxRestarts {
			tail, _ := containerLogsTail(ctx, rt, modelUUID, 20)
			return fmt.Errorf("the model container has restarted %d times without ever becoming ready — "+
				"it is crash-looping, not loading: %s", inspect.RestartCount, firstLine(strings.TrimSpace(tail)))
		}
		if time.Now().After(deadline) {
			tail, _ := containerLogsTail(ctx, rt, modelUUID, 20)
			return fmt.Errorf("the model did not become ready within %s — weight loading can be long, "+
				"but past this budget something is wrong: %s", modelReadyBudget, firstLine(strings.TrimSpace(tail)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// inspectBounded observes the container under its own deadline, so a wedged
// agent channel costs one probe instead of the job.
func (h *ModelRun) inspectBounded(ctx context.Context, rt dockerruntime.Runtime, modelUUID string) (container.InspectResponse, error) {
	probeCtx, cancel := context.WithTimeout(ctx, modelProbeTimeout)
	defer cancel()
	inspect, err := rt.ContainerInspect(probeCtx, modelUUID)
	if err != nil && probeCtx.Err() != nil && ctx.Err() == nil {
		return inspect, fmt.Errorf("the server stopped answering while the model was starting — "+
			"no reply within %s: %w", modelProbeTimeout, err)
	}
	return inspect, err
}

// cancelRequested reports the operator's request to stop. A store that cannot
// answer is not a cancellation: the job carries on rather than aborting a
// start over a transient query error.
func (h *ModelRun) cancelRequested(ctx context.Context, jobID int64) bool {
	cancelled, err := h.Store.IsJobCancelRequested(ctx, jobID)
	return err == nil && cancelled
}

// lifecycle is the databases' one: the container by name, statuses recorded,
// an explicit stop being a state, never a defect (ADR-080 §5).
func (h *ModelRun) lifecycle(ctx context.Context, rt dockerruntime.Runtime, action, modelUUID string, resourceID int64) error {
	if err := containerLifecycle(ctx, rt, action, modelUUID, 60); err != nil {
		if dockerruntime.IsNotFound(err) {
			return fmt.Errorf("no container exists for this model — start it first")
		}
		return err
	}
	desired, observed := store.ResourceDesiredStatusRunning, store.ResourceObservedStatusHealthy
	if action == "stop" {
		desired, observed = store.ResourceDesiredStatusStopped, store.ResourceObservedStatusExited
	}
	_ = h.Store.SetResourceDesiredStatus(ctx, store.SetResourceDesiredStatusParams{ID: resourceID, DesiredStatus: desired})
	_ = h.Store.SetResourceObservedStatus(ctx, store.SetResourceObservedStatusParams{ID: resourceID, ObservedStatus: observed})
	return nil
}

// delete removes the container and soft-deletes the resource. The HF cache
// volume is deliberately untouched: weights are shared across models
// (ADR-080 §4) — a model is not the owner of the corpus it read.
func (h *ModelRun) delete(ctx context.Context, rt dockerruntime.Runtime, row store.GetModelByIDRow, modelUUID string) error {
	// Routing first, the applications' order: the FQDNs are freed and the
	// edge relay re-derived before the workload goes.
	if err := h.removeRouting(ctx, rt, row, modelUUID); err != nil {
		return fmt.Errorf("could not remove the routing — the model is left untouched, retry once the proxy is healthy: %w", err)
	}
	if err := removeNamedContainers(ctx, rt, false, modelUUID); err != nil {
		return err
	}
	_, err := h.Store.SoftDeleteResource(ctx, row.Resource.ID)
	return err
}

// removeRouting deletes the model's domains and removes its routing file,
// which also re-derives the edge relay without them (ADR-077). A model that
// never had a domain skips the proxy entirely.
func (h *ModelRun) removeRouting(ctx context.Context, rt dockerruntime.Runtime, row store.GetModelByIDRow, modelUUID string) error {
	resID := row.Resource.ID
	domains, err := h.Store.ListDomainsForModel(ctx, &resID)
	if err != nil || len(domains) == 0 {
		return err
	}
	if err := h.Store.DeleteDomainsForModel(ctx, &resID); err != nil {
		return err
	}
	dest, err := h.Store.GetDestinationByID(ctx, row.Resource.DestinationID)
	if err != nil {
		return err
	}
	server, err := h.Store.GetServerByID(ctx, dest.ServerID)
	if err != nil {
		return err
	}
	if server.ProxyType != store.ProxyTypeTraefik {
		return nil
	}
	ops, err := h.HostOps.HostOps(ctx, server.ID)
	if err != nil {
		return err
	}
	applier := &ProxyApplier{
		Store: h.Store, Docker: rt, Host: ops, Server: server, Network: dest.Network,
		Edge: &EdgeSyncer{Store: h.Store, Docker: h.Docker, Host: h.HostOps, Logger: h.Logger},
	}
	return applier.Apply(ctx, modelUUID, "", "")
}
