package jobs

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/go-units"

	"github.com/deepteams/akerdock/internal/adoption"
	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/githubapp"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/waker"
)

// TypeDeploymentRun executes one deployment attempt (deployment-engine §4).
const TypeDeploymentRun = "deployment.run"

// DeploymentRunPayload references the deployment row (never secrets).
type DeploymentRunPayload struct {
	DeploymentID int64 `json:"deployment_id"`
}

// DeploymentRun drives the §4 state machine for the P0 image build pack:
// preparing → (cloning/building as pull) → starting → healthchecking →
// switching (routing applied atomically, old container stopped, candidate
// promoted) → finishing.
type DeploymentRun struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Audit   *audit.Recorder
	Logger  *slog.Logger
	// Docker resolves a server's runtime over its agent channel (ADR-052).
	// The deploy pipeline migrates to it slice by slice — today the hooks'
	// exec; builds, git and file deposits stay on SSH.
	Docker dockerruntime.Source
	// HostOps resolves the ADR-054 file primitives on the same channel —
	// routing files and the waker table ride it.
	HostOps hostops.Source
	// ControlPlanePort is the published port of this instance (AKERDOCK_INSTANCE_PORT):
	// on the server that HOSTS the instance, the preview forward-auth talks to
	// the control plane straight through the Docker host gateway — never the
	// public hairpin, whose latency taxes every preview request (ADR-030).
	ControlPlanePort int
	// WakerImage is this AkerDock release's own image (AKERDOCK_IMAGE): the
	// scale-to-zero waker is deployed as a helper container from it (ADR-036).
	// Empty leaves scale_to_zero inert with a clear error at deploy time.
	WakerImage string
}

// ImageRef and TagRef bound what can reach a remote shell (INV-012); they
// are also enforced at application creation.
var (
	ImageRef = regexp.MustCompile(`^[a-z0-9]+((\.|_{1,2}|-+|/|:[0-9]+/)[a-z0-9]+)*$`)
	TagRef   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	// A queued deployment waits without touching the server while an atomic
	// cleanup owns it. Variable for fast unit tests.
	deploymentCleanupPollInterval = time.Second
	// deploymentStablePeriod is the §4 no-healthcheck wait: running and
	// stable this long counts as up. deploymentHealthPoll is the health
	// inspect cadence. Variables for fast unit tests.
	deploymentStablePeriod = 10 * time.Second
	deploymentHealthPoll   = 2 * time.Second
)

// Execute runs one attempt. A terminal deployment is never re-run: after a
// crash the lease expires and the re-run starts by inspecting the state
// (§2.5). Pipeline failures mark the deployment failed and complete the
// job — the queue never blindly replays a failed deployment (§21.1).
// Deployment progress is tracked in deployment_steps, not in the job's own
// step recorder, hence the unused parameter.
func (h *DeploymentRun) Execute(ctx context.Context, job store.Job, _ *queue.StepRecorder) (any, error) {
	var payload DeploymentRunPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	d, err := h.Store.GetDeploymentByID(ctx, payload.DeploymentID)
	if err != nil {
		return nil, fmt.Errorf("deployment not found: %w", err)
	}
	switch d.Status {
	case store.DeploymentStatusSucceeded, store.DeploymentStatusFailed, store.DeploymentStatusCancelled, store.DeploymentStatusSuperseded:
		return map[string]any{"deployment_status": string(d.Status)}, nil // nothing to redo
	}

	run, err := h.newRun(ctx, d, job.ID)
	if err != nil {
		return nil, err
	}
	// The SSH connection outlives execute(): the compensation below removes the
	// candidate container, and it cannot do that over a closed connection.
	defer run.close()

	if err := run.execute(ctx); err != nil {
		var stateChanged *deploymentStateChangedError
		if errors.As(err, &stateChanged) {
			return map[string]any{"deployment_status": string(stateChanged.status)}, nil
		}
		if errors.Is(err, errCancelled) {
			run.markCancelled(ctx)
			return map[string]any{"deployment_status": "cancelled"}, nil
		}
		// Terminal for this attempt: compensation done, deployment failed.
		run.markFailed(ctx, err)
		return map[string]any{"deployment_status": "failed", "error": err.Error()}, nil
	}
	return map[string]any{"deployment_status": "succeeded", "image_digest": run.digest}, nil
}

// bc is the client the BUILD runs on: the build server when there is one, the
// target server otherwise. Every step that compiles, clones or writes sources
// goes through it; every step that runs, routes or switches goes through
// r.client. Confusing the two is how you build on the right machine and deploy
// on the wrong one.
func (r *deploymentRun) bc() *sshexec.Client {
	if r.builder != nil {
		return r.builder
	}
	return r.client
}

// bcrt is bc()'s typed twin: the Docker runtime of the machine that BUILT the
// image — the build server's channel when one is dialled, the deployment
// server's otherwise (ADR-055 phase 1: registry and inspect operations ride
// the channel; the build invocation itself is still the host CLI).
func (r *deploymentRun) bcrt() dockerruntime.Runtime {
	if r.brt != nil {
		return r.brt
	}
	return r.rt
}

// bhostops is bcrt()'s host-ops twin: the file primitives and the build of
// the machine that builds.
func (r *deploymentRun) bhostops() hostops.Ops {
	if r.bhops != nil {
		return r.bhops
	}
	return r.hops
}

// buildLabels assembles the typed label set of a built image: the managed
// labels every AkerDock object carries, plus the build-specific ones.
func (r *deploymentRun) buildLabels(extra map[string]string) map[string]string {
	labels := make(map[string]string, len(r.labelsMap)+len(extra))
	for k, v := range r.labelsMap {
		labels[k] = v
	}
	for k, v := range extra {
		labels[k] = v
	}
	return labels
}

// agentBuild runs the typed BuildKit build on the machine that builds
// (ADR-055 phase 2) and pumps its plain-text progress into the step log. The
// stream's terminal error IS the build failure, cause included.
func (r *deploymentRun) agentBuild(ctx context.Context, onOutput func(string), p agentwire.ImageBuildParams) error {
	rc, err := r.bhostops().BuildImage(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		onOutput(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("build failed: %s", firstLine(err.Error()))
	}
	return nil
}

// previewAuthHash renders the htpasswd line protecting this preview
// (§20.4.4): the generated AKERDOCK_PREVIEW_BASIC_AUTH secret ("user:pass"),
// bcrypt-hashed for the proxy — the clear text never enters a routing file.
// Computed once per run.
func (r *deploymentRun) previewAuthHash(ctx context.Context) string {
	if r.previewAuth != "" {
		return r.previewAuth
	}
	vars, err := r.h.Store.ListPreviewEnvVars(ctx, store.ListPreviewEnvVarsParams{ResourceID: r.app.Resource.ID, PreviewID: &r.preview.ID})
	if err != nil {
		return ""
	}
	for _, v := range vars {
		if v.Key != "AKERDOCK_PREVIEW_BASIC_AUTH" {
			continue
		}
		plaintext, err := r.h.Keyring.Decrypt("environment_variables", "value_enc", pguuid.String(v.Uuid), v.ValueEnc)
		if err != nil {
			return ""
		}
		user, pass, ok := strings.Cut(string(plaintext), ":")
		if !ok {
			return ""
		}
		hashed, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
		if err != nil {
			return ""
		}
		r.previewAuth = user + ":" + string(hashed)
		return r.previewAuth
	}
	return ""
}

// previewSSOAuthURL is the forwardAuth address of the sso protection
// (ADR-030) — empty when the application uses another mode, an error naming
// the missing instance FQDN when sso is asked without one.
func (r *deploymentRun) previewSSOAuthURL(ctx context.Context) (string, error) {
	if r.app.Application.PreviewProtection != store.PreviewProtectionSso {
		return "", nil
	}
	settings, err := r.h.Store.GetInstanceSettings(ctx)
	if err != nil {
		return "", err
	}
	if settings.Fqdn == nil || *settings.Fqdn == "" {
		return "", fmt.Errorf("preview_protection sso requires the instance FQDN (ADR-030) — set it in the instance settings")
	}
	// The host of the instance reaches its control plane directly through
	// the Docker host gateway: no DNS, no TLS, no public hairpin — the
	// forward-auth runs on EVERY preview request, its latency is the
	// preview's latency. Remote servers keep the public URL.
	if r.server.IsLocalhost && r.h.ControlPlanePort > 0 {
		return fmt.Sprintf("http://host.docker.internal:%d/webhooks/previews/forward-auth", r.h.ControlPlanePort), nil
	}
	return "https://" + *settings.Fqdn + "/webhooks/previews/forward-auth", nil
}

// namingIdentity is the base of the Docker names of this run: the preview
// uuid for a PR instance, the resource uuid otherwise (INV-011).
func (r *deploymentRun) namingIdentity() string {
	if r.preview != nil {
		return pguuid.String(r.preview.Uuid)
	}
	return pguuid.String(r.app.Resource.Uuid)
}

// close releases the SSH connections, once the compensation has had its chance.
func (r *deploymentRun) close() {
	if r.builder != nil {
		_ = r.builder.Close()
	}
	if r.client != nil {
		_ = r.client.Close()
	}
}

type deploymentRun struct {
	h     *DeploymentRun
	jobID int64
	d     store.Deployment
	app   store.GetApplicationByIDRow
	// service is set when the resource is an inline compose stack
	// (resource_type = service): app then carries only the Resource part.
	service *store.Service
	// preview is set for a PR preview deployment (§20.4): the preview uuid
	// becomes the Docker naming identity, the env set is the dedicated
	// is_preview one (INV-010), and routing goes to the preview fqdn.
	preview *store.Preview
	// previewAuth caches the bcrypt htpasswd line of the preview protection.
	previewAuth string
	// previewComposeRouted caches the served-services map of a compose
	// preview (§20.4.1) — the magic pass, the network wiring and the final
	// routing must all see the same one.
	previewComposeRouted map[string]previewComposeRoute
	server               store.Server
	dest                 store.Destination
	teamUUID             string
	client               *sshexec.Client
	// rt is the target server's Docker runtime over its agent channel
	// (ADR-052): every container/image/volume operation on the TARGET goes
	// through it. Builds, git, file deposits — and everything on the build
	// server — stay on the SSH clients.
	rt dockerruntime.Runtime
	// hops is the same channel's ADR-054 file primitives on the target.
	hops hostops.Ops
	// brt is the build server's runtime, resolved when one is dialled — nil
	// when the build runs on the deployment server (bcrt() falls back to rt).
	brt dockerruntime.Runtime
	// bhops is the build server's host-ops; bhostops() falls back to hops.
	bhops hostops.Ops
	// labelsMap is the run's management-label set as the typed creates need
	// it; the CLI flag string keeps feeding the builds.
	labelsMap map[string]string
	// composeStackEnv are the stack-wide KEY=VALUE entries of a compose run;
	// composePullAuth is its per-request registry credential.
	composeStackEnv []string
	composePullAuth string
	// builder is the machine the image is BUILT on. It is the target server
	// unless the application asked for a build server (§3.4) — in which case
	// what ships is not the image on that machine, but the one it pushed to a
	// registry the target can pull from.
	builder      *sshexec.Client
	buildServer  *store.Server
	seq          int32
	digest       string
	rolling      bool
	target       string
	healthBudget int
	// stzWakeSet is the scale-to-zero wake set in stack start order with its
	// depends_on edges, set by the compose engine as soon as the plan exists —
	// so every waker provisioning of this run ships the FULL stack and its
	// start graph (ADR-037 §5), not just the routed services. Nil for a plain
	// single-container app.
	stzWakeSet []waker.WakeContainer
}

func (h *DeploymentRun) newRun(ctx context.Context, d store.Deployment, jobID int64) (*deploymentRun, error) {
	var service *store.Service
	app, err := h.Store.GetApplicationByID(ctx, d.ResourceID)
	if err != nil {
		// Not an application: an inline compose stack deploys through the
		// same machinery, with the compose file coming from the services row.
		resource, rerr := h.Store.GetResourceByID(ctx, d.ResourceID)
		if rerr != nil || resource.ResourceType != store.ResourceTypeService {
			return nil, fmt.Errorf("application vanished: %w", err)
		}
		svc, serr := h.Store.GetServiceByID(ctx, d.ResourceID)
		if serr != nil {
			return nil, fmt.Errorf("service vanished: %w", serr)
		}
		service = &svc
		app = store.GetApplicationByIDRow{Resource: resource}
		app.BuildConfig.BuildPack = store.BuildPackCompose
		// Same routing default as an application's runtime_configs row: HTTP
		// redirects to HTTPS unless the operator opts out.
		app.RuntimeConfig.ForceHttps = true
	}
	server, err := h.Store.GetServerByID(ctx, d.ServerID)
	if err != nil {
		return nil, fmt.Errorf("server vanished: %w", err)
	}
	dest, err := h.Store.GetDestinationByID(ctx, app.Resource.DestinationID)
	if err != nil {
		return nil, fmt.Errorf("destination vanished: %w", err)
	}
	team, err := h.Store.GetTeamByID(ctx, app.Resource.TeamID)
	if err != nil {
		return nil, fmt.Errorf("team vanished: %w", err)
	}
	var preview *store.Preview
	if d.PreviewID != nil {
		p, err := h.Store.GetPreviewByID(ctx, *d.PreviewID)
		if err != nil {
			return nil, fmt.Errorf("preview vanished: %w", err)
		}
		preview = &p
	}
	run := &deploymentRun{h: h, jobID: jobID, d: d, app: app, service: service, preview: preview, server: server, dest: dest, teamUUID: pguuid.String(team.Uuid)}
	// On a resume, continue the step numbering of the attempt that crashed:
	// (deployment_id, seq) is unique, and restarting at 1 would both collide and
	// overwrite the record of what the dead worker had already done.
	if seq, err := h.Store.MaxDeploymentStepSeq(ctx, d.ID); err == nil {
		run.seq = seq
	}
	return run, nil
}

// step opens a deployment_steps row, runs fn, and records log/exit code.
func (r *deploymentRun) step(ctx context.Context, name string, fn func() (*sshexec.Result, error)) error {
	r.seq++
	stepID, err := r.h.Store.CreateDeploymentStep(ctx, store.CreateDeploymentStepParams{
		DeploymentID: r.d.ID, Seq: r.seq, Name: name,
	})
	if err != nil {
		return err
	}
	res, err := fn()
	status := store.DeploymentStepStatusSucceeded
	var exit *int32
	var logText *string
	if res != nil {
		code := int32(res.ExitCode)
		exit = &code
		combined := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
		if combined != "" {
			logText = &combined
		}
		if res.ExitCode != 0 && err == nil {
			err = fmt.Errorf("%s: exit code %d: %s", name, res.ExitCode, firstLine(res.Stderr))
		}
	}
	if err != nil {
		status = store.DeploymentStepStatusFailed
		logText = appendFailure(logText, err)
	}
	_ = r.h.Store.FinishDeploymentStep(ctx, store.FinishDeploymentStepParams{
		ID: stepID, Status: status, ExitCode: exit, Log: logText,
	})
	return err
}

// appendFailure merges the failure into the step log instead of dropping it:
// the error often carries MORE than the command output — candidateFailure
// packs the dying container's logs into it, and losing that leaves the
// operator staring at a bare "restarting" with nothing to debug.
func appendFailure(logText *string, err error) *string {
	msg := err.Error()
	if logText == nil || *logText == "" {
		return &msg
	}
	if strings.Contains(*logText, msg) {
		return logText
	}
	combined := *logText + "\n" + msg
	return &combined
}

// streamStep is step() with a live console: fn receives an output sink that
// refreshes the step's log as the command runs — the SSE stream polls the
// steps every second, so the browser sees the build output while it happens
// instead of one silent blob at the end. Writes are throttled to one per
// second; the closing FinishDeploymentStep records the authoritative
// interleaved transcript.
func (r *deploymentRun) streamStep(ctx context.Context, name string, fn func(onOutput func(string)) (*sshexec.Result, error)) error {
	r.seq++
	stepID, err := r.h.Store.CreateDeploymentStep(ctx, store.CreateDeploymentStepParams{
		DeploymentID: r.d.ID, Seq: r.seq, Name: name,
	})
	if err != nil {
		return err
	}

	var mu sync.Mutex
	var buf strings.Builder
	var lastFlush time.Time
	onOutput := func(chunk string) {
		mu.Lock()
		buf.WriteString(chunk)
		// Flush new complete lines several times a second so the stream reads
		// line-by-line rather than in one-second blocks — still coalesced enough
		// to spare the database a write per line on a chatty build.
		flush := time.Since(lastFlush) >= 200*time.Millisecond
		var text string
		if flush {
			lastFlush = time.Now()
			// Only up to the last COMPLETE line: the SSE stream assigns each
			// line a definitive sequence — a line cut mid-flush would be
			// emitted short and never completed on screen.
			text = buf.String()
			if i := strings.LastIndexByte(text, '\n'); i >= 0 {
				text = strings.TrimSpace(text[:i])
			} else {
				text = ""
			}
		}
		mu.Unlock()
		if flush && text != "" {
			_ = r.h.Store.SetDeploymentStepLog(ctx, store.SetDeploymentStepLogParams{ID: stepID, Log: &text})
		}
	}

	res, err := fn(onOutput)
	status := store.DeploymentStepStatusSucceeded
	var exit *int32
	var logText *string
	// The streamed transcript preserves stdout/stderr interleaving — better
	// than the Result's two separated halves when both are non-empty.
	if combined := strings.TrimSpace(buf.String()); combined != "" {
		logText = &combined
	}
	if res != nil {
		code := int32(res.ExitCode)
		exit = &code
		if logText == nil {
			if combined := strings.TrimSpace(res.Stdout + "\n" + res.Stderr); combined != "" {
				logText = &combined
			}
		}
		if res.ExitCode != 0 && err == nil {
			err = fmt.Errorf("%s: exit code %d: %s", name, res.ExitCode, firstLine(res.Stderr))
		}
	}
	if err != nil {
		status = store.DeploymentStepStatusFailed
		logText = appendFailure(logText, err)
	}
	_ = r.h.Store.FinishDeploymentStep(ctx, store.FinishDeploymentStepParams{
		ID: stepID, Status: status, ExitCode: exit, Log: logText,
	})
	return err
}

func (r *deploymentRun) skipStep(ctx context.Context, name, reason string) {
	r.seq++
	if stepID, err := r.h.Store.CreateDeploymentStep(ctx, store.CreateDeploymentStepParams{
		DeploymentID: r.d.ID, Seq: r.seq, Name: name,
	}); err == nil {
		_ = r.h.Store.FinishDeploymentStep(ctx, store.FinishDeploymentStepParams{
			ID: stepID, Status: store.DeploymentStepStatusSkipped, Log: &reason,
		})
	}
}

func (r *deploymentRun) setStatus(ctx context.Context, s store.DeploymentStatus) error {
	// Write-ahead: the state in the database says what may have started.
	if s == store.DeploymentStatusPreparing && r.d.Status == store.DeploymentStatusQueued {
		waitLogged := false
		for {
			rows, err := r.h.Store.StartDeploymentUnlessCleanupRunning(ctx, r.d.ID)
			if err != nil {
				return err
			}
			if rows > 0 {
				break
			}
			status, err := r.h.Store.GetDeploymentStatus(ctx, r.d.ID)
			if err != nil {
				return err
			}
			if status != store.DeploymentStatusQueued {
				return &deploymentStateChangedError{status: status}
			}
			if err := r.checkpoint(ctx); err != nil {
				return err
			}
			if !waitLogged {
				r.h.Logger.Info("deployment waiting for server cleanup",
					"deployment_uuid", pguuid.String(r.d.Uuid), "server_id", r.server.ID)
				waitLogged = true
			}
			timer := time.NewTimer(deploymentCleanupPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	} else if err := r.h.Store.SetDeploymentStatus(ctx, store.SetDeploymentStatusParams{ID: r.d.ID, Status: s}); err != nil {
		return err
	}
	// Every transition publishes a versioned outbox event (deployment-engine §12.1).
	if r.h.Audit != nil {
		var teamUUID pgtype.UUID
		if team, err := r.h.Store.GetTeamByID(ctx, r.app.Resource.TeamID); err == nil {
			teamUUID = team.Uuid
		}
		// Enough context for a notification to be actionable on its own: the
		// resource NAME (not a bare uuid), what triggered it, the commit, and —
		// on failure — the error. Chat channels render these as fields.
		payload := map[string]any{
			"deployment_uuid": pguuid.String(r.d.Uuid),
			"status":          string(s),
			"name":            r.app.Resource.Name,
			"trigger":         string(r.d.Trigger),
		}
		if r.d.CommitSha != nil && *r.d.CommitSha != "" {
			payload["commit_sha"] = *r.d.CommitSha
		}
		if r.d.CommitAuthor != nil && *r.d.CommitAuthor != "" {
			payload["commit_author"] = *r.d.CommitAuthor
		}
		if r.d.GitBranch != nil && *r.d.GitBranch != "" {
			payload["branch"] = *r.d.GitBranch
		}
		if s == store.DeploymentStatusFailed && r.d.ErrorMessage != nil && *r.d.ErrorMessage != "" {
			payload["error"] = *r.d.ErrorMessage
		}
		if r.preview != nil {
			payload["pr_id"] = r.preview.PrID
			if r.preview.Fqdn != nil && *r.preview.Fqdn != "" {
				payload["url"] = "https://" + *r.preview.Fqdn
			}
		} else if s == store.DeploymentStatusSucceeded {
			// The app's own URL, only on success (one domain lookup, not per step).
			if dep := r.deploymentRefs(ctx); dep["deployment.url"] != "" {
				payload["url"] = dep["deployment.url"]
			}
		}
		r.h.Audit.Outbox(ctx, r.h.Store, "deployment."+string(s)+".v1", teamUUID, r.app.Resource.Uuid,
			"deployment:"+pguuid.String(r.d.Uuid), payload)

		// A preview's own lifecycle event, once its deployment succeeds: the
		// first successful deploy CREATED it, any later one UPDATED it (a new
		// commit). Destruction is emitted by the teardown job.
		if r.preview != nil && s == store.DeploymentStatusSucceeded {
			evt := "application.preview.created.v1"
			if r.preview.LastDeployedAt.Valid {
				evt = "application.preview.updated.v1"
			}
			r.h.Audit.Outbox(ctx, r.h.Store, evt, teamUUID, r.app.Resource.Uuid,
				"preview:"+pguuid.String(r.preview.Uuid),
				previewLifecyclePayload(r.app.Resource.Name, *r.preview, r.d))
		}
	}
	return nil
}

func previewLifecyclePayload(name string, preview store.Preview, deployment store.Deployment) map[string]any {
	payload := map[string]any{
		"preview_uuid": pguuid.String(preview.Uuid),
		"pr_id":        preview.PrID,
		"name":         name,
	}
	if preview.Fqdn != nil && *preview.Fqdn != "" {
		payload["fqdn"] = *preview.Fqdn
	}
	if deployment.CommitAuthor != nil && *deployment.CommitAuthor != "" {
		payload["commit_author"] = *deployment.CommitAuthor
	}
	branch := preview.SourceBranch
	if branch == nil || *branch == "" {
		branch = deployment.GitBranch
	}
	if branch != nil && *branch != "" {
		payload["branch"] = *branch
	}
	return payload
}

// errCancelled aborts the pipeline at a cooperative checkpoint (§2.6).
var errCancelled = errors.New("deployment cancelled")

// deploymentStateChangedError is a benign stop: while a queued deployment was
// waiting for cleanup, it was cancelled or superseded. The atomic start query
// refuses to resurrect it, and Execute completes the job without overwriting
// the deployment's newer state with `failed`.
type deploymentStateChangedError struct {
	status store.DeploymentStatus
}

func (e *deploymentStateChangedError) Error() string {
	return "deployment is no longer queued: " + string(e.status)
}

// checkpoint honours a pending cancellation request between steps — never
// past the switching barrier (§21.1).
func (r *deploymentRun) checkpoint(ctx context.Context) error {
	cancelled, err := r.h.Store.IsJobCancelRequested(ctx, r.jobID)
	if err == nil && cancelled {
		return errCancelled
	}
	return nil
}

// markCancelled applies the same compensation as a failure — remove the
// candidate, never the healthy serving container (INV-006).
func (r *deploymentRun) markCancelled(ctx context.Context) {
	if r.rt != nil {
		candidate := r.namingIdentity() + "-next"
		_ = removeNamedContainers(ctx, r.rt, false, candidate)
	}
	_ = r.setStatus(ctx, store.DeploymentStatusCancelled)
	r.h.Logger.Info("deployment cancelled", "deployment_uuid", pguuid.String(r.d.Uuid))
}

func (r *deploymentRun) execute(ctx context.Context) error {
	resourceUUID := pguuid.String(r.app.Resource.Uuid)
	// appUUID is the NAMING identity: the resource for a normal deployment,
	// the preview uuid for a PR instance (INV-011) — containers, directories
	// and routing files all derive from it, which is what lets a preview live
	// NEXT TO production instead of replacing it.
	appUUID := resourceUUID
	appDir := "/var/lib/akerdock/applications/" + appUUID
	resourceType := "application"
	if r.service != nil {
		appDir = "/var/lib/akerdock/services/" + appUUID
		resourceType = "service"
	}
	previewLabel := ""
	if r.preview != nil {
		if r.preview.IsFork && !r.preview.ForkApprovedAt.Valid {
			return fmt.Errorf("fork preview without maintainer approval (INV-010)")
		}
		appUUID = pguuid.String(r.preview.Uuid)
		appDir = "/var/lib/akerdock/previews/" + appUUID
		previewLabel = " --label akerdock.preview_uuid=" + appUUID
	}
	candidate := appUUID + "-next"
	// The container currently serving: the uuid-derived name, except for an
	// adopted resource awaiting normalization (§20.7) — there it is whatever
	// the original platform named it. This deployment IS the normalization.
	oldName := appUUID
	if r.preview == nil {
		oldName = adoption.ContainerName(r.app.Resource.Adoption, appUUID)
	}
	labels := fmt.Sprintf("--label akerdock.managed=true --label akerdock.resource_uuid=%s --label akerdock.type=%s --label akerdock.team_uuid=%s --label akerdock.deployment_uuid=%s",
		resourceUUID, resourceType, r.teamUUID, pguuid.String(r.d.Uuid)) + previewLabel
	// The same identity as a map for the typed create — the CLI flag string
	// above still feeds the builds, which stay on the CLI path (ADR-051).
	labelsMap := map[string]string{
		"akerdock.managed":         "true",
		"akerdock.resource_uuid":   resourceUUID,
		"akerdock.type":            resourceType,
		"akerdock.team_uuid":       r.teamUUID,
		"akerdock.deployment_uuid": pguuid.String(r.d.Uuid),
	}
	if r.preview != nil {
		labelsMap["akerdock.preview_uuid"] = appUUID
	}
	r.labelsMap = labelsMap

	// --- preparing -------------------------------------------------------
	if err := r.checkpoint(ctx); err != nil {
		return err
	}
	if err := r.setStatus(ctx, store.DeploymentStatusPreparing); err != nil {
		return err
	}
	key, err := r.h.Store.GetPrivateKeyByID(ctx, r.server.PrivateKeyID)
	if err != nil {
		return fmt.Errorf("private key vanished: %w", err)
	}
	pem, err := r.h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return err
	}
	timeout := time.Duration(r.server.SshTimeoutSeconds) * time.Second
	r.client, err = sshexec.Dial(ctx, r.server.Host, int(r.server.Port), r.server.SshUser, string(pem), timeout, pinnedHostKey(r.server))
	if err != nil {
		return fmt.Errorf("ssh connect: %w", err)
	}
	// The target's Docker runtime (ADR-052): mandatory — the compensation
	// (markFailed/markCancelled) also removes the candidate through it, so it
	// is resolved once and kept on the run.
	if r.rt, err = r.h.Docker.Runtime(ctx, r.server.ID); err != nil {
		return fmt.Errorf("the server's agent is not connected: %w", err)
	}
	if r.hops, err = r.h.HostOps.HostOps(ctx, r.server.ID); err != nil {
		return fmt.Errorf("the server's agent is not connected: %w", err)
	}
	// The build server (§3.4): a separate machine, dialled only when the
	// application asked for one and only for a build pack that builds something.
	if r.app.BuildConfig.UseBuildServer && r.app.BuildConfig.BuildPack != store.BuildPackImage && !r.reusesArtifact() {
		if err := r.dialBuildServer(ctx); err != nil {
			return err
		}
	}

	// A compose stack has its own multi-service pipeline (compose-spec §8.2,
	// deployment-engine §5.7): everything from here diverges, including the
	// resume semantics.
	if r.app.BuildConfig.BuildPack == store.BuildPackCompose {
		return r.executeCompose(ctx, appUUID, appDir, labels)
	}

	// NOT closed here: the compensation (markFailed / markCancelled) runs after
	// execute returns and needs this connection to remove the candidate. The
	// caller closes it — see Execute.

	// A deployment that is already past `queued` when its job is re-run is a
	// RESUME: the previous worker died mid-flight (its lease expired). It must
	// never be replayed blindly — §2.5.
	if resumed, err := r.resume(ctx, appUUID, oldName, candidate); err != nil || resumed {
		return err
	}

	// The runtime variables ride the typed create body over the channel
	// (INV-003 as ADR-051 clarified): no argv, and no runtime.sh on the host
	// anymore. The app directory is still prepared — builds and build.env
	// live there.
	envVars, err := r.renderRuntimeEnv(ctx)
	if err != nil {
		return err
	}
	if err := r.step(ctx, "prepare", func() (*sshexec.Result, error) {
		if _, err := r.rt.Ping(ctx); err != nil {
			return nil, fmt.Errorf("the Docker daemon is not answering: %s", firstLine(err.Error()))
		}
		if err := ensureNetwork(ctx, r.rt, r.dest.Network); err != nil {
			return nil, err
		}
		return r.client.Run(ctx, fmt.Sprintf(
			"mkdir -p %s/env && chmod 700 %s %s/env", appDir, appDir, appDir))
	}); err != nil {
		return err
	}

	// The pre-deployment command runs in the EXISTING container, at the end of
	// preparing — before anything is built (§10). A failure here fails the
	// deployment while nothing has been mutated yet: the running application is
	// untouched.
	if err := r.runHook(ctx, "pre_deployment", r.app.RuntimeConfig.PreDeploymentCommand, oldName); err != nil {
		return err
	}

	// --- cloning / building: per build pack (§5.3/§5.4) -------------------
	if err := r.checkpoint(ctx); err != nil {
		return err
	}
	if err := r.setStatus(ctx, store.DeploymentStatusBuilding); err != nil {
		return err
	}
	var runRef string
	switch {
	// The artifact is already built and verified — no rebuild. Either an
	// earlier image (rollback, ADR-006) or the one already running
	// (skip_build, ADR-048). Both were resolved at trigger time.
	case r.reusesArtifact():
		reason, missing := "rollback (no source)", "the rollback image %s is no longer present on the server"
		if r.d.SkipBuild {
			reason = "no build requested (the running artifact is redeployed)"
			missing = "the image %s this application runs is no longer present on the server — deploy it again to rebuild it"
		}
		r.skipStep(ctx, "clone", reason)
		runRef = *r.d.ImageName
		if r.d.ImageDigest != nil && strings.Contains(*r.d.ImageDigest, "@") {
			runRef = *r.d.ImageDigest // registry digest, reproducible
		} else if r.d.ImageTag != nil {
			runRef += ":" + *r.d.ImageTag
		}
		ref := runRef
		if err := r.step(ctx, "verify_artifact", func() (*sshexec.Result, error) {
			if _, err := r.rt.ImageInspect(ctx, ref); err != nil {
				if dockerruntime.IsNotFound(err) {
					return nil, fmt.Errorf(missing, ref)
				}
				return nil, err
			}
			return nil, nil
		}); err != nil {
			return err
		}
	case r.app.BuildConfig.BuildPack == store.BuildPackImage:
		r.skipStep(ctx, "clone", "no git source (image build pack)")
		imageRef := *r.d.ImageName + ":" + *r.d.ImageTag
		// A private registry authenticates PER REQUEST (ADR-051): the
		// credential rides the pull call and nothing is ever persisted in the
		// host's ~/.docker/config.json — the login/logout dance survives only
		// on the CLI build path.
		auth, err := r.registryAuth(ctx, r.app.BuildConfig.RegistryCredentialID)
		if err != nil {
			return err
		}
		// Streamed: pulling a large image is otherwise a long, silent wait.
		if err := r.streamStep(ctx, "pull", func(onOutput func(string)) (*sshexec.Result, error) {
			rc, err := r.rt.ImagePull(ctx, imageRef, image.PullOptions{RegistryAuth: auth})
			if err != nil {
				return nil, err
			}
			defer func() { _ = rc.Close() }()
			return nil, streamPullProgress(rc, onOutput)
		}); err != nil {
			return err
		}
		if err := r.step(ctx, "resolve_digest", func() (*sshexec.Result, error) {
			inspect, err := r.rt.ImageInspect(ctx, imageRef)
			if err == nil && len(inspect.RepoDigests) > 0 {
				r.digest = inspect.RepoDigests[0]
				_ = r.h.Store.SetDeploymentImageDigest(ctx, store.SetDeploymentImageDigestParams{ID: r.d.ID, ImageDigest: &r.digest})
			}
			return nil, err
		}); err != nil {
			return err
		}
		runRef = r.digest
		if runRef == "" {
			runRef = imageRef
		}
	case r.app.BuildConfig.BuildPack == store.BuildPackDockerfile:
		var err error
		if r.app.Application.GitRepositoryUrl != nil {
			runRef, err = r.buildFromGit(ctx, appUUID, appDir, labels)
		} else {
			runRef, err = r.buildFromDockerfile(ctx, appUUID, appDir)
		}
		if err != nil {
			return err
		}
	case r.app.BuildConfig.BuildPack == store.BuildPackStatic,
		r.app.BuildConfig.BuildPack == store.BuildPackNixpacks:
		// Same pipeline as a git+dockerfile build (§5.5/§5.6): only the way the
		// image is produced differs — a generated Dockerfile for static, the
		// nixpacks builder for nixpacks.
		var err error
		if runRef, err = r.buildFromGit(ctx, appUUID, appDir, labels); err != nil {
			return err
		}
	default:
		return fmt.Errorf("build pack %q is not implemented yet", r.app.BuildConfig.BuildPack)
	}
	// The image was built somewhere the target server cannot reach. Push it to
	// the registry, then have the target pull it BY DIGEST: the digest is what
	// makes "the image the target runs" and "the image the builder produced" the
	// same thing, provably, and it is what a rollback replays (ADR-006).
	if r.builder != nil {
		pushed, err := r.pushBuiltImage(ctx)
		if err != nil {
			return err
		}
		runRef = pushed
	} else {
		r.skipStep(ctx, "push", "built on the deployment server (no registry push needed)")
	}

	// The image is built: reclaim the source checkouts beyond the current +
	// previous (deployment-engine §5.1). Nothing else removes them, so a busy
	// app would otherwise fill the disk with hundreds of full clones.
	if !r.reusesArtifact() {
		r.pruneOldSources(ctx, appDir)
	}

	// --- starting ---------------------------------------------------------
	if err := r.checkpoint(ctx); err != nil {
		return err
	}
	if err := r.setStatus(ctx, store.DeploymentStatusStarting); err != nil {
		return err
	}
	var memoryLimit int64
	if r.app.RuntimeConfig.MemoryLimit != nil {
		if memoryLimit, err = units.RAMInBytes(*r.app.RuntimeConfig.MemoryLimit); err != nil {
			return fmt.Errorf("invalid memory limit %q: %w", *r.app.RuntimeConfig.MemoryLimit, err)
		}
	}
	// Declared volumes are created idempotently before the candidate, and
	// mounted into it (§5.3.4). Bind mount host directories are created too.
	binds, err := r.prepareStorages(ctx, appUUID, runRef)
	if err != nil {
		return err
	}
	healthConfig, hasHealthCheck := r.healthConfig(ctx)
	// Rolling eligibility (§7.3): a working health check is required. Without
	// one, the deployment falls back to stop-then-start (§7.4).
	r.rolling = hasHealthCheck
	// Non-rolling fallback: the old container is stopped first and the new
	// one is created directly under the final name (§7.4).
	target := candidate
	if !r.rolling {
		target = appUUID
	}
	r.target = target
	if err := r.step(ctx, "start_candidate", func() (*sshexec.Result, error) {
		grace := int(r.app.RuntimeConfig.StopGracePeriodSeconds)
		if !r.rolling {
			if err := containerLifecycle(ctx, r.rt, "stop", oldName, grace); err != nil && !dockerruntime.IsNotFound(err) {
				return nil, err
			}
			if err := removeNamedContainers(ctx, r.rt, false, oldName); err != nil {
				return nil, err
			}
		}
		if err := removeNamedContainers(ctx, r.rt, false, target); err != nil {
			return nil, err
		}
		// The variables ride the typed body over the encrypted channel: no
		// shell, no argv, no host file (INV-003, ADR-051).
		config := &container.Config{
			Image: runRef, Env: envVars, Labels: labelsMap,
			Healthcheck: healthConfig, StopTimeout: &grace,
		}
		host := &container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			NetworkMode:   container.NetworkMode(r.dest.Network),
			Binds:         binds,
		}
		host.Memory = memoryLimit
		if _, err := r.rt.ContainerCreate(ctx, config, host, nil, nil, target); err != nil {
			return nil, err
		}
		return nil, r.rt.ContainerStart(ctx, target, container.StartOptions{})
	}); err != nil {
		return err
	}

	// --- healthchecking ----------------------------------------------------
	// No container healthcheck yet: non-rolling mode waits for a stable
	// running state (§4 healthchecking, default 10 s).
	if err := r.checkpoint(ctx); err != nil {
		return err
	}
	if err := r.setStatus(ctx, store.DeploymentStatusHealthchecking); err != nil {
		return err
	}
	// The container's own stdout/stderr is followed LIVE during the wait, so an
	// operator sees the app boot (and why it fails) in the deployment stream
	// instead of a silent "starting". The follow stops the moment the verdict
	// is reached, so it never delays the switch.
	if err := r.streamStep(ctx, "healthcheck", func(onOutput func(string)) (*sshexec.Result, error) {
		verdict, err := r.awaitCandidate(ctx, target, hasHealthCheck, onOutput)
		if err != nil {
			return nil, err
		}
		if !verdict {
			return nil, r.candidateFailure(ctx, target, "the new container did not become healthy in time")
		}
		return nil, nil
	}); err != nil {
		return err
	}

	// The post-deployment command runs in the CANDIDATE, once healthy and
	// BEFORE the switch (§10). A failure fails the deployment: the candidate is
	// removed by the compensation and the old container keeps serving
	// (INV-005). There is no automatic rollback — the old one never stopped.
	if err := r.runHook(ctx, "post_deployment", r.app.RuntimeConfig.PostDeploymentCommand, r.target); err != nil {
		return err
	}

	// --- switching: cancellation barrier — past this point the deployment
	// always runs to a terminal state (§21.1).
	if err := r.checkpoint(ctx); err != nil {
		return err
	}
	if err := r.setStatus(ctx, store.DeploymentStatusSwitching); err != nil {
		return err
	}
	grace := r.app.RuntimeConfig.StopGracePeriodSeconds
	if r.rolling {
		// Zero-downtime (§7.2): route to the candidate's IP first — the old
		// container keeps serving until the new endpoint is live (INV-005) —
		// then stop the old one and promote the candidate.
		var candidateIP string
		if err := r.step(ctx, "resolve_endpoint", func() (*sshexec.Result, error) {
			resp, err := r.rt.ContainerInspect(ctx, candidate)
			if err != nil {
				return nil, err
			}
			if resp.NetworkSettings != nil {
				if ep := resp.NetworkSettings.Networks[r.dest.Network]; ep != nil {
					candidateIP = ep.IPAddress
				}
			}
			if candidateIP == "" {
				return nil, fmt.Errorf("could not resolve the candidate IP on network %s", r.dest.Network)
			}
			return nil, nil
		}); err != nil {
			return err
		}
		if err := r.applyRoutingTo(ctx, appUUID, candidateIP); err != nil {
			return err
		}
		if err := r.step(ctx, "switch", func() (*sshexec.Result, error) {
			return nil, r.promoteCandidate(ctx, oldName, candidate, appUUID, int(grace))
		}); err != nil {
			return err
		}
	} else if err := r.step(ctx, "switch", func() (*sshexec.Result, error) {
		// Non-rolling: the old container is already gone (§7.4); the inspect
		// only proves the final name exists.
		_, err := r.rt.ContainerInspect(ctx, appUUID)
		return nil, err
	}); err != nil {
		return err
	}

	if err := r.applyRouting(ctx, appUUID); err != nil {
		return err
	}

	// --- finishing: the same idempotent tail a resume would run (§4).
	return r.finish(ctx, appUUID)
}

// buildFromDockerfile implements the inline-Dockerfile P0 flow (§5.3): the
// content is uploaded via stdin into the per-deployment source directory,
// then built locally with the system labels. There is no git source, so
// the image tag derives from the deployment UUID instead of a commit SHA.
func (r *deploymentRun) buildFromDockerfile(ctx context.Context, appUUID, appDir string) (string, error) {
	r.skipStep(ctx, "clone", "inline Dockerfile (no git source)")

	dockerfile := r.app.BuildConfig.DockerfileContent
	if dockerfile == nil || *dockerfile == "" {
		return "", fmt.Errorf("the application has no Dockerfile content")
	}
	depUUID := pguuid.String(r.d.Uuid)
	tag := strings.ReplaceAll(depUUID, "-", "")[:12]
	imageRef := "akerdock/" + appUUID + ":" + tag
	srcDir := fmt.Sprintf("%s/source/%s", appDir, depUUID)

	// Build-time variables reach an inline Dockerfile exactly as they reach a
	// git one: plain ones as ARGs, secret ones as BuildKit secrets (§5.2) —
	// both in the typed build command now (ADR-055): no build.env lands on
	// the host for a dockerfile build.
	_, buildArgs, err := r.renderBuildEnv(ctx)
	if err != nil {
		return "", err
	}
	if err := r.bhostops().WriteFile(ctx, agentwire.FileWriteParams{
		Path: srcDir + "/Dockerfile", Content: []byte(*dockerfile),
		Mode: 0o600, MakeDirs: true, DirMode: 0o700,
	}); err != nil {
		return "", fmt.Errorf("writing the Dockerfile failed: %s", firstLine(err.Error()))
	}
	if err := r.streamStep(ctx, "build", func(onOutput func(string)) (*sshexec.Result, error) {
		return nil, r.agentBuild(ctx, onOutput, agentwire.ImageBuildParams{
			ContextDir: srcDir, Dockerfile: "Dockerfile", Tags: []string{imageRef},
			BuildArgs: buildArgs.argValues, Secrets: buildArgs.secretValues,
			Labels: r.buildLabels(map[string]string{"akerdock.commit_sha": ""}), NoCache: r.d.ForceRebuild,
		})
	}); err != nil {
		return "", err
	}
	r.d.ImageName, r.d.ImageTag = ptrStr("akerdock/"+appUUID), &tag
	_ = r.h.Store.SetDeploymentImage(ctx, store.SetDeploymentImageParams{ID: r.d.ID, ImageName: r.d.ImageName, ImageTag: r.d.ImageTag})

	// Local builds have no registry digest: the image ID is recorded so
	// the rollback retention can pin it (ADR-006, local mode).
	if err := r.resolveLocalDigest(ctx, imageRef); err != nil {
		return "", err
	}
	return imageRef, nil
}

// resolveLocalDigest records the built image's ID as the deployment digest —
// a typed inspect on the machine that built it (ADR-055 phase 1).
func (r *deploymentRun) resolveLocalDigest(ctx context.Context, imageRef string) error {
	return r.step(ctx, "resolve_digest", func() (*sshexec.Result, error) {
		resp, err := r.bcrt().ImageInspect(ctx, imageRef)
		if err != nil {
			return nil, err
		}
		if resp.ID != "" {
			r.digest = resp.ID
			_ = r.h.Store.SetDeploymentImageDigest(ctx, store.SetDeploymentImageDigestParams{ID: r.d.ID, ImageDigest: &r.digest})
		}
		return nil, nil
	})
}

func ptrStr(s string) *string { return &s }

// installDeployKey uploads the application's deploy key to the build server
// and returns the environment prefix that makes git use it (§5.1).
//
// The key is written through stdin, never through argv (INV-003), under a
// per-deployment path with mode 0600, and removed by the returned cleanup —
// which runs even when the clone fails. GIT_SSH_COMMAND pins the identity
// (`IdentitiesOnly`) so the agent or the server's own keys cannot be used by
// accident, and `accept-new` records the host key on first contact while
// still refusing a key that later changes.
//
// Applications without a deploy key (public repositories) get an empty prefix
// and no file on disk.
func (r *deploymentRun) installDeployKey(ctx context.Context, appDir string) (string, func(), error) {
	noop := func() {}
	if r.app.Application.GitSourceID == nil {
		return "", noop, nil
	}
	source, err := r.h.Store.GetGitSourceByID(ctx, *r.app.Application.GitSourceID)
	if err != nil {
		return "", noop, fmt.Errorf("git source: %w", err)
	}
	if source.GithubAppID != nil {
		// GitHub App source: the clone authenticates with a one-hour
		// installation token instead of a deploy key (protocols §2.2.4).
		return r.installGithubToken(ctx, appDir, *source.GithubAppID)
	}
	if source.PrivateKeyID == nil {
		return "", noop, nil
	}
	key, err := r.h.Store.GetPrivateKeyByID(ctx, *source.PrivateKeyID)
	if err != nil {
		return "", noop, fmt.Errorf("deploy key vanished: %w", err)
	}
	pem, err := r.h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return "", noop, err
	}

	depUUID := pguuid.String(r.d.Uuid)
	keyPath := fmt.Sprintf("%s/keys/%s", appDir, depUUID)
	knownHosts := fmt.Sprintf("%s/keys/%s.known_hosts", appDir, depUUID)
	cleanup := func() {
		// context.WithoutCancel: a cancelled deployment must still take its
		// key off the server.
		_, _ = r.bc().Run(context.WithoutCancel(ctx), fmt.Sprintf("rm -f %s %s", keyPath, knownHosts))
	}
	res, err := r.bc().RunInput(ctx, fmt.Sprintf(
		"mkdir -p %s/keys && chmod 700 %s/keys && umask 077 && cat > %s", appDir, appDir, keyPath), string(pem))
	if err != nil || res.ExitCode != 0 {
		cleanup()
		return "", noop, fmt.Errorf("installing the deploy key failed")
	}
	gitEnv := fmt.Sprintf(
		"GIT_TERMINAL_PROMPT=0 GIT_SSH_COMMAND='ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s' ",
		keyPath, knownHosts)
	return gitEnv, cleanup, nil
}

// buildFromGit implements the git + dockerfile P0 flow (§5.3): the branch
// is resolved to an immutable SHA (§20.2), then a shallow fetch of that
// exact SHA — never a moving branch head (§5.3.1) — and a remote BuildKit
// build with the system labels (§5.3.2).
// installGithubToken mints an installation access token and materializes a
// GIT_ASKPASS helper for it on the build machine: the token reaches git
// through a 0700 file uploaded via stdin and an environment variable — never
// argv, never a log line (INV-003, INV-012). The helper is removed by the
// returned cleanup, and the token dies on its own within the hour.
func (r *deploymentRun) installGithubToken(ctx context.Context, appDir string, githubAppID int64) (string, func(), error) {
	noop := func() {}
	app, err := r.h.Store.GetGithubAppByID(ctx, githubAppID)
	if err != nil {
		return "", noop, fmt.Errorf("github app vanished: %w", err)
	}
	if app.AppID == nil || app.InstallationID == nil || app.AppPrivateKeyEnc == nil {
		return "", noop, fmt.Errorf("the GitHub App of this source is not installed")
	}
	pem, err := r.h.Keyring.Decrypt("github_apps", "app_private_key_enc", pguuid.String(app.Uuid), app.AppPrivateKeyEnc)
	if err != nil {
		return "", noop, err
	}
	client := &githubapp.Client{APIURL: app.ApiUrl}
	jwt, err := githubapp.AppJWT(*app.AppID, pem, time.Now())
	if err != nil {
		return "", noop, err
	}
	// Restricted to the one repository being cloned when known (§2.2.2).
	var repos []string
	if r.app.Application.RepositoryID != nil {
		if repo, err := r.h.Store.GetRepositoryByID(ctx, *r.app.Application.RepositoryID); err == nil {
			if _, name, ok := strings.Cut(repo.FullName, "/"); ok {
				repos = []string{name}
			}
		}
	}
	token, err := client.InstallationToken(ctx, jwt, *app.InstallationID, repos)
	if err != nil {
		return "", noop, fmt.Errorf("github installation token: %w", err)
	}

	askpass := appDir + "/keys/git_askpass"
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in Username*) echo x-access-token;; *) echo %s;; esac\n", shellQuote(token.Token))
	if res, err := r.bc().RunInput(ctx, fmt.Sprintf("umask 077 && mkdir -p %s/keys && cat > %s && chmod 700 %s", appDir, askpass, askpass), script); err != nil || res.ExitCode != 0 {
		return "", noop, fmt.Errorf("uploading the git credential helper failed")
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_, _ = r.bc().Run(cleanupCtx, "rm -f "+askpass)
	}
	return fmt.Sprintf("GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=%s ", askpass), cleanup, nil
}

func (r *deploymentRun) buildFromGit(ctx context.Context, appUUID, appDir, labels string) (string, error) {
	repoURL := *r.app.Application.GitRepositoryUrl
	branch := "main"
	if r.app.Application.GitBranch != nil && *r.app.Application.GitBranch != "" {
		branch = *r.app.Application.GitBranch
	}

	// --- cloning -----------------------------------------------------------
	if err := r.setStatus(ctx, store.DeploymentStatusCloning); err != nil {
		return "", err
	}
	// A private repository is cloned with a deploy key (§5.1): the key is
	// installed on the build server for the time of the clone and removed
	// right after, whatever happens.
	gitEnv, cleanup, err := r.installDeployKey(ctx, appDir)
	if err != nil {
		return "", err
	}
	defer cleanup()

	var sha string
	if r.preview != nil && r.preview.HeadSha != nil && *r.preview.HeadSha != "" {
		// A preview deploys the PR head, pinned at delivery time (§20.4).
		sha = *r.preview.HeadSha
		r.skipStep(ctx, "resolve_sha", "preview head "+sha[:min(12, len(sha))])
		_ = r.h.Store.SetDeploymentCommit(ctx, store.SetDeploymentCommitParams{ID: r.d.ID, CommitSha: &sha, GitBranch: r.preview.SourceBranch})
	} else if err := r.step(ctx, "resolve_sha", func() (*sshexec.Result, error) {
		res, err := r.bc().Run(ctx, fmt.Sprintf("%sgit ls-remote %s refs/heads/%s",
			gitEnv, shellQuote(repoURL), shellQuote(branch)))
		if err == nil && res.ExitCode == 0 {
			sha, _, _ = strings.Cut(firstLine(res.Stdout), "\t")
			sha = strings.TrimSpace(sha)
			if len(sha) != 40 {
				return res, fmt.Errorf("branch %q not found on %s", branch, repoURL)
			}
			_ = r.h.Store.SetDeploymentCommit(ctx, store.SetDeploymentCommitParams{ID: r.d.ID, CommitSha: &sha, GitBranch: &branch})
		}
		return res, err
	}); err != nil {
		return "", err
	}

	depUUID := pguuid.String(r.d.Uuid)
	srcDir := fmt.Sprintf("%s/source/%s", appDir, depUUID)
	// Crash recovery: a partial clone directory is destroyed and redone (§4).
	fetchRef := sha
	if ref := r.previewFetchRef(); ref != "" {
		// A fork's commit does not exist in the BASE repository — fetching its
		// SHA there fails. GitHub publishes every PR head under
		// refs/pull/<n>/head of the base repo, fork included: that ref is the
		// only thing reachable with the installation token, and it is what a
		// preview builds (§20.4.8).
		fetchRef = ref
	}
	if err := r.step(ctx, "clone", func() (*sshexec.Result, error) {
		return r.bc().Run(ctx, fmt.Sprintf(
			"rm -rf %s && mkdir -p %s && cd %s && git init -q && git remote add origin %s && %sgit fetch -q --depth 1 origin %s && git checkout -q --detach FETCH_HEAD",
			srcDir, srcDir, srcDir, shellQuote(repoURL), gitEnv, shellQuote(fetchRef)))
	}); err != nil {
		return "", err
	}
	if fetchRef != sha {
		// The ref must carry the SHA the delivery announced: a PR ref that
		// moved between the webhook and the clone would silently build a
		// different commit than the one reported on the PR.
		if err := r.step(ctx, "verify_head", func() (*sshexec.Result, error) {
			res, err := r.bc().Run(ctx, "cd "+srcDir+" && git rev-parse HEAD")
			if err == nil && res.ExitCode == 0 {
				got := firstLine(res.Stdout)
				if got != sha {
					return res, fmt.Errorf("%s resolved to %s, but the delivery announced %s — the pull request moved", fetchRef, got, sha)
				}
			}
			return res, err
		}); err != nil {
			return "", err
		}
	}

	// Author and subject of the checked-out commit — best effort: it answers
	// "who last pushed" in the deployment view, never a reason to fail a build.
	// The unit separator (\x1f) can't appear in a commit line, so it is a safe
	// field delimiter.
	if res, err := r.bc().Run(ctx, "cd "+srcDir+" && git log -1 --format='%an%x1f%s'"); err == nil && res.ExitCode == 0 {
		author, message, _ := strings.Cut(strings.TrimRight(res.Stdout, "\n"), "\x1f")
		var authorPtr, messagePtr *string
		if author = strings.TrimSpace(author); author != "" {
			authorPtr = &author
			r.d.CommitAuthor = &author
		}
		if message = strings.TrimSpace(message); message != "" {
			messagePtr = &message
			r.d.CommitMessage = &message
		}
		if authorPtr != nil || messagePtr != nil {
			_ = r.h.Store.SetDeploymentCommitMeta(ctx, store.SetDeploymentCommitMetaParams{
				ID: r.d.ID, CommitAuthor: authorPtr, CommitMessage: messagePtr,
			})
		}
	}

	// --- building ------------------------------------------------------------
	if err := r.setStatus(ctx, store.DeploymentStatusBuilding); err != nil {
		return "", err
	}
	buildEnv, buildArgs, err := r.renderBuildEnv(ctx)
	if err != nil {
		return "", err
	}

	sha12 := sha[:12]
	imageRef := "akerdock/" + appUUID + ":" + sha12
	baseDir := strings.TrimPrefix(r.app.Application.BaseDirectory, "/")
	dockerfile := "Dockerfile"
	if p := r.app.BuildConfig.DockerfilePath; p != nil && *p != "" {
		dockerfile = strings.TrimPrefix(*p, "/")
	}

	// The static build pack has no Dockerfile in the repository: one is
	// synthesized next to the sources (§5.2). It is written under a name of
	// our own so it can never collide with — or overwrite — a file the
	// repository already ships.
	if r.app.BuildConfig.BuildPack == store.BuildPackStatic {
		dockerfile = staticDockerfileName
		if err := r.writeStaticDockerfile(ctx, srcDir, baseDir); err != nil {
			return "", err
		}
	}
	if r.app.BuildConfig.BuildPack == store.BuildPackNixpacks {
		// nixpacks drives the host CLI itself and sources build.env — the one
		// build path a host env file still serves (ADR-055 phase 2 note).
		noCache := ""
		if r.d.ForceRebuild {
			noCache = " --no-cache"
		}
		if res, err := r.bc().RunInput(ctx, fmt.Sprintf("umask 077 && cat > %s/env/build.env", appDir), buildEnv); err != nil || res.ExitCode != 0 {
			return "", fmt.Errorf("uploading build.env failed")
		}
		if err := r.buildWithNixpacks(ctx, srcDir, baseDir, appDir, imageRef, labels, sha, noCache); err != nil {
			return "", err
		}
	} else if err := r.streamStep(ctx, "build", func(onOutput func(string)) (*sshexec.Result, error) {
		// The typed BuildKit build (ADR-055): args and secrets travel in the
		// command body — build.env never lands on the host for this path.
		return nil, r.agentBuild(ctx, onOutput, agentwire.ImageBuildParams{
			ContextDir: strings.TrimRight(srcDir+"/"+baseDir, "/"), Dockerfile: dockerfile,
			Tags: []string{imageRef}, BuildArgs: buildArgs.argValues, Secrets: buildArgs.secretValues,
			Labels: r.buildLabels(map[string]string{"akerdock.commit_sha": sha}), NoCache: r.d.ForceRebuild,
		})
	}); err != nil {
		return "", err
	}
	r.d.ImageName, r.d.ImageTag = ptrStr("akerdock/"+appUUID), &sha12
	_ = r.h.Store.SetDeploymentImage(ctx, store.SetDeploymentImageParams{ID: r.d.ID, ImageName: r.d.ImageName, ImageTag: r.d.ImageTag})

	if err := r.resolveLocalDigest(ctx, imageRef); err != nil {
		return "", err
	}
	return imageRef, nil
}

// previewFetchRef is the ref a preview clone must fetch: the pull request's
// head ref of the BASE repository, which exists for forks too — the only ref
// reachable with the base repo's credential (§20.4.8). GitHub and Gitea
// publish refs/pull/<n>/head; GitLab refs/merge-requests/<iid>/head. Empty
// for a non-preview run.
func (r *deploymentRun) previewFetchRef() string {
	if r.preview == nil {
		return ""
	}
	switch r.preview.Provider {
	case store.GitProviderGithub, store.GitProviderGitea:
		return fmt.Sprintf("refs/pull/%d/head", r.preview.PrID)
	case store.GitProviderGitlab:
		return fmt.Sprintf("refs/merge-requests/%d/head", r.preview.PrID)
	default:
		return ""
	}
}

// renderBuildEnv renders build.env with the build-time variables (§5.2).
func (r *deploymentRun) renderBuildEnv(ctx context.Context) (string, buildInputs, error) {
	vars, err := r.h.Store.ListEnvVarsForDeploy(ctx, r.app.Resource.ID)
	if err != nil {
		return "", buildInputs{}, err
	}
	// Shared {{scope.KEY}} references resolve in build values too (§5.4) —
	// never for previews (their set is strictly dedicated, INV-010).
	shared := sharedEnv{}
	if r.preview == nil {
		if shared, err = resolveSharedEnv(ctx, r.h.Store, r.h.Keyring, r.app.Resource.ID); err != nil {
			return "", buildInputs{}, err
		}
	}
	// {{deployment.*}} resolves in build-time values too, so a frontend can bake
	// its own URL / CORS origin at build (interpolation into a value the operator
	// wrote — not a new auto --build-arg, so no Dockerfile ARG warnings).
	r.mergeDeploymentRefs(&shared, r.deploymentRefs(ctx))
	var b strings.Builder
	var inputs buildInputs
	for _, v := range vars {
		if !v.IsBuildTime {
			continue
		}
		plaintext, err := r.h.Keyring.Decrypt("environment_variables", "value_enc", pguuid.String(v.Uuid), v.ValueEnc)
		if err != nil {
			return "", buildInputs{}, fmt.Errorf("decrypt variable %s: %w", v.Key, err)
		}
		value := string(plaintext)
		if !v.IsLiteral {
			value = shared.interpolate(value)
		}
		// Shell-quoted: build.env is sourced with `set -a`, so a quoted
		// multiline value is exported intact.
		fmt.Fprintf(&b, "%s=%s\n", v.Key, shellQuote(value))
		if v.IsSecret {
			if inputs.secretValues == nil {
				inputs.secretValues = map[string][]byte{}
			}
			inputs.secretValues[v.Key] = []byte(value)
		} else {
			if inputs.argValues == nil {
				inputs.argValues = map[string]string{}
			}
			inputs.argValues[v.Key] = value
		}
	}
	return b.String(), inputs, nil
}

// buildInputs splits the build-time variables into what may end up in the image
// and what must never (§5.2). The key lists feed the shell renderer (build.env
// + flags, nixpacks); the value maps feed the typed build command (ADR-055),
// where secrets become BuildKit session secrets without ever touching a host
// file.
// The distinction is the whole point (§5.2): a plain value becomes a
// build-arg — written into the image metadata, where `docker history` shows
// it to anyone who can pull the image. A variable marked secret NEVER
// becomes one: it rides the typed build command as a BuildKit session
// secret, mounted under /run/secrets/KEY for the lifetime of a single RUN
// and left out of every layer (INV-003).
type buildInputs struct {
	argValues    map[string]string
	secretValues map[string][]byte
}

// applyRouting materializes the stable routing (by container name, robust
// to container restarts — §7.2 step 7).
func (r *deploymentRun) applyRouting(ctx context.Context, appUUID string) error {
	return r.applyRoutingTo(ctx, appUUID, "")
}

// applyRoutingTo points the routing at endpoint (an IP during the rolling
// switch, the container name once stable), applied atomically (§6.2).
func (r *deploymentRun) applyRoutingTo(ctx context.Context, appUUID, endpoint string) error {
	if r.server.ProxyType != store.ProxyTypeTraefik {
		return nil
	}
	var content string
	var err error
	var access *proxy.AccessPolicy
	if r.preview == nil {
		access, err = resourceAccessPolicy(ctx, r.h.Store, r.h.Keyring, r.app, r.service,
			r.server, r.h.ControlPlanePort)
		if err != nil {
			return err
		}
	}
	switch {
	case r.preview != nil:
		ssoURL, ssoErr := r.previewSSOAuthURL(ctx)
		if ssoErr != nil {
			return ssoErr
		}
		// Scale-to-zero (ADR-036): point the preview's single route at the waker,
		// which forwards to the container and wakes it on demand. Provision the
		// waker (and its routing table) before the routing flips to it.
		if r.app.Application.PreviewScaleToZero {
			// The waker forwards to the stable container name and wakes it by
			// `docker start` — never the candidate IP of a rolling switch.
			if rg, ok, rgErr := previewSingleRouteGroup(r.app, *r.preview, ""); rgErr != nil {
				return rgErr
			} else if ok {
				previewUUID := pguuid.String(r.preview.Uuid)
				if err = ensureWaker(ctx, r.client, r.hops, r.dest.Network, r.h.WakerImage, previewUUID,
					wakerConfigFromRouteGroup(previewUUID, rg, r.stzWakeSet),
					AgentEnvForServer(ctx, r.h.Store, r.h.Keyring, r.h.Logger, r.server, r.h.ControlPlanePort)); err != nil {
					return err
				}
				content = renderPreviewContent(pointRouteGroupAtWaker(rg), previewUUID, r.d.ID,
					r.app.Application.PreviewProtection, r.previewAuthHash(ctx), ssoURL, []string{*r.preview.Fqdn})
			}
		} else {
			content, err = RenderPreviewRoutingFile(r.app, *r.preview, r.d.ID, endpoint, r.previewAuthHash(ctx), ssoURL)
		}
	case r.app.Application.ScaleToZero:
		// Scale-to-zero application (ADR-037): route through the waker, which
		// forwards to the app's container(s) by their stable names and wakes them
		// on demand. The rolling candidate-IP step is skipped — cold-start is
		// accepted, so zero-downtime is not the goal; only the stable step routes.
		if endpoint != "" {
			return nil
		}
		rg, ok, rgErr := applicationRouteGroup(ctx, r.h.Store, r.app, "", nil)
		if rgErr != nil {
			return rgErr
		}
		if ok && len(rg.Routes) > 0 {
			rg.Access = access
			if err = ensureWaker(ctx, r.client, r.hops, r.dest.Network, r.h.WakerImage, appUUID,
				wakerConfigFromRouteGroup(appUUID, rg, r.stzWakeSet),
				AgentEnvForServer(ctx, r.h.Store, r.h.Keyring, r.h.Logger, r.server, r.h.ControlPlanePort)); err != nil {
				return err
			}
			content = proxy.GenerateDynamic(pointRouteGroupAtWaker(rg), r.d.ID)
		}
	default:
		content, err = RenderRoutingFileTo(ctx, r.h.Store, r.app, r.d.ID, endpoint, access)
	}
	if err != nil {
		return err
	}
	name := "apply_routing"
	if endpoint != "" {
		name = "switch_routing"
	}
	// The expected service URL: the proxy must really expose it before the
	// apply counts as successful (§6.3); otherwise the file is rolled back
	// to the last applied revision and the old container keeps serving.
	expect := ""
	if content != "" && endpoint != "" {
		expect = "http://" + endpoint + ":"
	}
	applier := &ProxyApplier{Store: r.h.Store, Docker: r.rt, Host: r.hops, Server: r.server, Network: r.dest.Network}
	return r.step(ctx, name, func() (*sshexec.Result, error) {
		return nil, applier.Apply(ctx, appUUID, content, expect)
	})
}

// healthConfig maps the configured health check to the typed create body
// (§5.3.4). A Dockerfile HEALTHCHECK stays authoritative: this only adds one
// when the image has none.
func (r *deploymentRun) healthConfig(ctx context.Context) (*container.HealthConfig, bool) {
	hc, err := r.h.Store.GetHealthCheck(ctx, r.app.Resource.ID)
	if err != nil || !hc.Enabled {
		return nil, false // no row means no health check
	}
	port := 80
	if hc.Port != nil {
		port = int(*hc.Port)
	} else if p := r.app.RuntimeConfig.PortsExposes; p != nil {
		first, _, _ := strings.Cut(*p, ",")
		if n, convErr := strconv.Atoi(strings.TrimSpace(first)); convErr == nil {
			port = n
		}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, hc.Path)
	cmd := fmt.Sprintf("curl -fsS -X %s -o /dev/null --max-time %d %s || wget -q -O /dev/null %s",
		hc.Method, hc.TimeoutSeconds, url, url)
	r.healthBudget = int(hc.StartPeriodSeconds) + int(hc.IntervalSeconds+hc.TimeoutSeconds)*int(hc.Retries) + 30
	return &container.HealthConfig{
		Test:        []string{"CMD-SHELL", cmd},
		Interval:    time.Duration(hc.IntervalSeconds) * time.Second,
		Timeout:     time.Duration(hc.TimeoutSeconds) * time.Second,
		Retries:     int(hc.Retries),
		StartPeriod: time.Duration(hc.StartPeriodSeconds) * time.Second,
	}, true
}

// candidateFailure captures the candidate logs before it is removed by the
// compensation, so the failure is diagnosable (§4 healthchecking).
func (r *deploymentRun) candidateFailure(ctx context.Context, target, reason string) error {
	detail := ""
	if out, err := containerLogsTail(ctx, r.rt, target, 200); err == nil && out != "" {
		detail = "\n" + out
	}
	return fmt.Errorf("%s%s", reason, detail)
}

// DockerVolumeName is the deterministic Docker name of a declared volume:
// <resource_uuid>_<name> — the UUID prefix prevents collisions (§8, INV-011).
func DockerVolumeName(resourceUUID, name string) string {
	return resourceUUID + "_" + name
}

// prepareStorages converges the application's declared storages (§8) and
// returns the candidate's bind list. Volumes are ensured through the channel
// with the management labels; bind-mount host directories are created over
// SSH (host paths); still-empty volumes are handed to the image's runtime
// user so a non-root image can write its own storage without a custom
// Dockerfile.
func (r *deploymentRun) prepareStorages(ctx context.Context, appUUID, imageRef string) ([]string, error) {
	storages, err := r.h.Store.ListStoragesForResource(ctx, r.app.Resource.ID)
	if err != nil {
		return nil, err
	}
	var binds []string
	var hostDirs []string
	var volumes []string
	for _, s := range storages {
		switch s.Kind {
		case store.StorageKindVolume:
			if s.Name == nil {
				continue
			}
			vol := DockerVolumeName(appUUID, *s.Name)
			if s.ExternalName != nil && *s.ExternalName != "" {
				if r.preview != nil {
					// An adopted volume is production data: a preview must
					// never mount it (INV-010) — same rule as bind mounts.
					r.h.Logger.Warn("preview skips adopted volume", "volume", *s.ExternalName)
					continue
				}
				// Adopted volume (§20.7): mounted under its ORIGINAL name —
				// prefixing it would remount an empty volume (INV-008).
				vol = *s.ExternalName
			}
			if err := ensureVolume(ctx, r.rt, vol, map[string]string{
				"akerdock.managed": "true", "akerdock.resource_uuid": appUUID, "akerdock.team_uuid": r.teamUUID,
			}); err != nil {
				return nil, err
			}
			binds = append(binds, vol+":"+s.MountPath)
			volumes = append(volumes, vol)
		case store.StorageKindBind:
			if s.HostPath == nil {
				continue
			}
			if r.preview != nil {
				// A bind mount is production data on the host: sharing it with
				// a preview would leak state across instances (INV-010).
				r.h.Logger.Warn("preview skips bind mount", "host_path", *s.HostPath)
				continue
			}
			hostDirs = append(hostDirs, *s.HostPath)
			binds = append(binds, *s.HostPath+":"+s.MountPath)
		}
	}
	if len(hostDirs) > 0 {
		if res, err := r.client.Run(ctx, "mkdir -p "+strings.Join(hostDirs, " ")); err != nil || res.ExitCode != 0 {
			return nil, fmt.Errorf("creating the bind-mount directories failed")
		}
	}
	if len(volumes) > 0 && imageRef != "" {
		r.chownEmptyVolumes(ctx, imageRef, volumes)
	}
	return binds, nil
}

// chownEmptyVolumes hands STILL-EMPTY volumes to the image's runtime user. A
// fresh named volume mounted on a path absent from the image belongs to root
// — a USER'd image then crash-loops on its first write. The fix runs INSIDE
// a throwaway container of the image itself (user 0), never against
// /var/lib/docker on the host, so a named USER resolves against the image's
// own /etc/passwd for free. Only empty volumes are touched: data that exists
// already has an owner, and it is not this function's to change.
// Best-effort by design: an image without /bin/sh (distroless) falls back to
// today's behavior instead of failing the deployment.
func (r *deploymentRun) chownEmptyVolumes(ctx context.Context, imageRef string, volumes []string) {
	inspect, err := r.rt.ImageInspect(ctx, imageRef)
	if err != nil || inspect.Config == nil {
		return
	}
	user := inspect.Config.User
	if user == "" || user == "root" || user == "0" || strings.HasPrefix(user, "0:") {
		return
	}
	for _, vol := range volumes {
		err := runOneShot(ctx, r.rt, &container.Config{
			Image:      imageRef,
			User:       "0",
			Entrypoint: []string{"/bin/sh"},
			Cmd:        []string{"-c", fmt.Sprintf("[ -n \"$(ls -A /akerdock-volume)\" ] || chown -- '%s' /akerdock-volume", user)},
		}, &container.HostConfig{Binds: []string{vol + ":/akerdock-volume"}})
		if err != nil {
			r.h.Logger.Warn("volume ownership fix skipped", "volume", vol, "error", err)
		}
	}
}

// deploymentRefs are the {{deployment.*}} interpolation values and the source
// of the AKERDOCK_FQDN/URL/PR_ID predefined variables: the deployment's OWN
// public identity, resolved identically in production (the application's primary
// domain) and previews (the generated preview FQDN, which changes per PR). Empty
// fqdn/url when the deployment has no domain yet.
func (r *deploymentRun) deploymentRefs(ctx context.Context) map[string]string {
	refs := map[string]string{}
	fqdn := ""
	if r.preview != nil {
		refs["deployment.pr_id"] = strconv.Itoa(int(r.preview.PrID))
		if r.preview.Fqdn != nil {
			fqdn = *r.preview.Fqdn
		}
	} else if domains, err := r.h.Store.ListDomainsForApplication(ctx, &r.app.Resource.ID); err == nil && len(domains) > 0 {
		fqdn = domains[0].Fqdn
	}
	if fqdn != "" {
		refs["deployment.fqdn"] = fqdn
		refs["deployment.url"] = "https://" + fqdn
	}
	return refs
}

// mergeDeploymentRefs adds the {{deployment.*}} pseudo-scope to a resolved
// sharedEnv so interpolation resolves it — for previews too, where the plain
// shared set is skipped (INV-010): a deployment's own FQDN is not a secret.
func (r *deploymentRun) mergeDeploymentRefs(shared *sharedEnv, dep map[string]string) {
	if len(dep) == 0 {
		return
	}
	if shared.refs == nil {
		shared.refs = map[string]string{}
	}
	for k, v := range dep {
		shared.refs[k] = v
	}
}

// renderRuntimeEnv decrypts the application's variables into the KEY=VALUE
// entries of the typed create body (§5.2, ADR-051): no shell, no quoting —
// multiline secrets survive verbatim.
func (r *deploymentRun) renderRuntimeEnv(ctx context.Context) ([]string, error) {
	var vars []store.EnvironmentVariable
	var err error
	if r.preview != nil {
		// The DEDICATED preview set (INV-010): production secrets are never
		// copied implicitly into a PR instance.
		vars, err = r.h.Store.ListPreviewEnvVars(ctx, store.ListPreviewEnvVarsParams{ResourceID: r.app.Resource.ID, PreviewID: &r.preview.ID})
	} else {
		vars, err = r.h.Store.ListEnvVarsForDeploy(ctx, r.app.Resource.ID)
	}
	if err != nil {
		return nil, err
	}
	// Shared variables (§5.4, §3.1): {{scope.KEY}} references resolve inside
	// values (literal variables excepted — that is what literal means), and
	// the server-scoped variables are injected unless the resource overrides
	// the key. Previews keep their strictly dedicated set (INV-010).
	shared := sharedEnv{}
	if r.preview == nil {
		if shared, err = resolveSharedEnv(ctx, r.h.Store, r.h.Keyring, r.app.Resource.ID); err != nil {
			return nil, err
		}
	}
	dep := r.deploymentRefs(ctx)
	r.mergeDeploymentRefs(&shared, dep)
	entries := make([]string, 0, len(vars))
	seen := map[string]bool{}
	for _, v := range vars {
		plaintext, err := r.h.Keyring.Decrypt("environment_variables", "value_enc", pguuid.String(v.Uuid), v.ValueEnc)
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
			continue // the resource's own variable wins
		}
		entries = append(entries, k+"="+shared.server[k])
	}
	// The same deployment identity as the {{deployment.*}} tokens, also exposed
	// as standalone predefined variables (§5.2) for apps that read them directly:
	// AKERDOCK_FQDN/URL (own public address) and AKERDOCK_PR_ID (previews).
	if v := dep["deployment.pr_id"]; v != "" {
		entries = append(entries, "AKERDOCK_PR_ID="+v)
	}
	if v := dep["deployment.fqdn"]; v != "" {
		entries = append(entries, "AKERDOCK_FQDN="+v, "AKERDOCK_URL="+dep["deployment.url"])
	}
	return entries, nil
}

// shellQuote renders a value as a single-quoted shell literal. Single quotes
// are literal in POSIX shells — a value can contain newlines, $, backticks,
// backslashes, anything — except a single quote itself, which is closed,
// escaped and reopened. This is what lets a PEM key or a JSON blob survive as
// an environment variable (INV-012).
// registryAuth builds the per-request registry credential of a typed pull
// (ADR-051): the password rides that one call over the encrypted channel and
// is never persisted in any ~/.docker/config.json. Empty when the
// application has no private registry.
func (r *deploymentRun) registryAuth(ctx context.Context, credID *int64) (string, error) {
	if credID == nil {
		return "", nil
	}
	cred, err := r.h.Store.GetRegistryCredentialByID(ctx, *credID)
	if err != nil {
		return "", fmt.Errorf("the registry credential of this application is gone: %w", err)
	}
	password, err := r.h.Keyring.Decrypt("registry_credentials", "password_enc",
		pguuid.String(cred.Uuid), cred.PasswordEnc)
	if err != nil {
		return "", fmt.Errorf("cannot decrypt the registry credential: %w", err)
	}
	return registry.EncodeAuthConfig(registry.AuthConfig{
		Username: cred.Username, Password: string(password), ServerAddress: cred.RegistryUrl,
	})
}

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// awaitCandidate is the §4 healthchecking verdict: without a health check it
// waits for a stable running state (10 s); with one it polls the container
// health until healthy/unhealthy/none, bounded by the configured budget. The
// container's own output streams live into the deployment log while the wait
// runs, and stops with the verdict.
func (r *deploymentRun) awaitCandidate(ctx context.Context, target string, hasHealthCheck bool, onOutput func(string)) (bool, error) {
	logsCtx, stopLogs := context.WithCancel(ctx)
	defer stopLogs()
	go func() {
		rc, err := r.rt.ContainerLogs(logsCtx, target, container.LogsOptions{
			ShowStdout: true, ShowStderr: true, Follow: true, Tail: "50",
		})
		if err != nil {
			return
		}
		defer func() { _ = rc.Close() }()
		_ = dockerruntime.Demux(rc, false, onOutput)
	}()

	if !hasHealthCheck {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(deploymentStablePeriod):
		}
		st, err := r.containerState(ctx, target)
		if err != nil {
			return false, err
		}
		onOutput(fmt.Sprintf("--- container status: %s ---\n", st))
		return st == "running", nil
	}

	deadline := time.Now().Add(time.Duration(r.healthBudget) * time.Second)
	verdict := "timeout"
	for time.Now().Before(deadline) {
		resp, err := r.rt.ContainerInspect(ctx, target)
		if err == nil {
			if resp.State == nil || resp.State.Health == nil {
				verdict = "none"
				break
			}
			if st := resp.State.Health.Status; st == "healthy" || st == "unhealthy" {
				verdict = st
				break
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(deploymentHealthPoll):
		}
	}
	onOutput(fmt.Sprintf("--- health: %s ---\n", verdict))
	return verdict == "healthy" || verdict == "none", nil
}

// promoteCandidate is the §7.2 switch: stop and remove the old container
// (tolerating its absence — the replay case), then rename the candidate into
// the final name. The rename failing fails the step: a switch that did not
// happen must never read as done.
func (r *deploymentRun) promoteCandidate(ctx context.Context, oldName, candidate, appUUID string, grace int) error {
	if err := containerLifecycle(ctx, r.rt, "stop", oldName, grace); err != nil && !dockerruntime.IsNotFound(err) {
		r.h.Logger.Warn("stopping the old container failed — promoting anyway, like the CLI switch did",
			"container", oldName, "error", err)
	}
	if err := removeNamedContainers(ctx, r.rt, false, oldName); err != nil {
		r.h.Logger.Warn("removing the old container failed — promoting anyway",
			"container", oldName, "error", err)
	}
	return r.rt.ContainerRename(ctx, candidate, appUUID)
}

// containerState reports a container's status, "absent" when it does not
// exist — the resume inspection's vocabulary (§2.5).
func (r *deploymentRun) containerState(ctx context.Context, name string) (string, error) {
	resp, err := r.rt.ContainerInspect(ctx, name)
	if err != nil {
		if dockerruntime.IsNotFound(err) {
			return "absent", nil
		}
		return "", err
	}
	if resp.State == nil {
		return "absent", nil
	}
	return resp.State.Status, nil
}

// envFlags renders `-e KEY` for each variable — with no value. Docker then
// reads the value from the environment of the shell that runs it, so a
// multiline secret never appears in argv, in `ps`, or in a shell history
// (INV-003).
func envFlags(keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(" -e ")
		b.WriteString(k)
	}
	return b.String()
}

// reusesArtifact reports whether this deployment runs an image that already
// exists on the server: an earlier one (rollback, ADR-006) or the one already
// running (skip_build, ADR-048). Neither clones, builds, produces a new
// artifact, nor reclaims anything — there is no new image to account for.
func (r *deploymentRun) reusesArtifact() bool {
	return r.d.IsRollback || r.d.SkipBuild
}

// recordArtifact registers the deployed image as a rollback candidate,
// protected from the automated cleanup (ADR-006, INV-015). A deployment that
// reuses an existing artifact creates none: it redeploys one already recorded.
func (r *deploymentRun) recordArtifact(ctx context.Context) {
	if r.reusesArtifact() || r.d.ImageName == nil {
		return
	}
	kind := store.ArtifactKindLocalImage
	if r.digest != "" && strings.Contains(r.digest, "@") {
		kind = store.ArtifactKindRegistryImage
	}
	var digest *string
	if r.digest != "" {
		digest = &r.digest
	}
	if err := r.h.Store.CreateDeploymentArtifact(ctx, store.CreateDeploymentArtifactParams{
		DeploymentID: r.d.ID, Kind: kind,
		ImageName: *r.d.ImageName, ImageTag: r.d.ImageTag, ImageDigest: digest,
		ServerID: &r.server.ID,
	}); err != nil {
		r.h.Logger.Warn("could not record rollback artifact", "error", err)
	}
}

// pruneOldSources keeps the current and previous source checkout of this app on
// the build machine and reclaims older ones (deployment-engine §5.1): every
// build clones into source/<deployment-uuid> and nothing else removes them, so
// a busy application would otherwise fill the disk with hundreds of full
// checkouts. Best-effort — a leftover clone never fails a deployment. `ls -1dt`
// lists newest-first; the two most recent (this build + the last) are kept.
func (r *deploymentRun) pruneOldSources(ctx context.Context, appDir string) {
	bc := r.bc()
	if bc == nil {
		return
	}
	_, _ = bc.Run(ctx, fmt.Sprintf(
		"ls -1dt %s/source/*/ 2>/dev/null | tail -n +3 | xargs -r rm -rf", appDir))
}

// prunableImage is one rollback artifact considered for reclamation.
type prunableImage struct {
	id  int64
	ref string
}

// pruneOldImages enforces the rollback retention (ADR-006, §29.4): after a
// successful deployment it keeps the N most recent images of this application —
// or, for a preview, of that single PR — on the server, and reclaims the older
// ones (`docker rmi`). N is the instance setting image_retention_count (min 1,
// so the live image, which is the newest artifact, is always kept). Best-effort:
// a rebuild reproduces any pruned image, so a failure here never fails the
// deployment. A deployment that reuses an existing artifact (rollback,
// skip_build) reclaims nothing — it added no image.
func (r *deploymentRun) pruneOldImages(ctx context.Context) {
	if r.reusesArtifact() || r.rt == nil {
		return
	}
	keep := 5
	if st, err := r.h.Store.GetInstanceSettings(ctx); err == nil && st.ImageRetentionCount > 0 {
		keep = int(st.ImageRetentionCount)
	}

	// Collect the artifacts newest-first, scoped to this app or this preview.
	var arts []prunableImage
	collect := func(id int64, name string, tag *string) {
		if tag == nil {
			return
		}
		arts = append(arts, prunableImage{id: id, ref: name + ":" + *tag})
	}
	if r.preview != nil {
		rows, err := r.h.Store.ListPreviewArtifactsOnServer(ctx, store.ListPreviewArtifactsOnServerParams{
			PreviewID: &r.preview.ID, ServerID: &r.server.ID,
		})
		if err != nil {
			r.h.Logger.Warn("image retention: list preview artifacts failed", "error", err)
			return
		}
		for _, a := range rows {
			collect(a.ID, a.ImageName, a.ImageTag)
		}
	} else {
		rows, err := r.h.Store.ListAppArtifactsOnServer(ctx, store.ListAppArtifactsOnServerParams{
			ResourceID: r.app.Resource.ID, ServerID: &r.server.ID,
		})
		if err != nil {
			r.h.Logger.Warn("image retention: list app artifacts failed", "error", err)
			return
		}
		for _, a := range rows {
			collect(a.ID, a.ImageName, a.ImageTag)
		}
	}
	for _, a := range imagesToReclaim(arts, keep) {
		if a.ref != "" {
			// Best-effort, like the CLI `|| true`: an image still in use (or
			// already gone) is not this pass's problem.
			_, _ = r.rt.ImageRemove(ctx, a.ref, image.RemoveOptions{})
		}
		// Drop the pointer whether or not the rmi reclaimed anything: beyond the
		// retention window it is no longer a valid rollback target.
		if err := r.h.Store.DeleteDeploymentArtifact(ctx, a.id); err != nil {
			r.h.Logger.Warn("image retention: drop artifact pointer failed", "artifact_id", a.id, "error", err)
		}
	}
}

// imagesToReclaim picks, from artifacts ordered newest-first, those beyond the
// retention window (ADR-006). The N most recent are kept; older ones are
// returned for removal — but an older artifact whose image reference is still
// referenced by a kept one (a constant registry tag reused across deployments)
// keeps a blank ref, so its stale pointer is dropped without reclaiming the
// image a live deployment still uses.
func imagesToReclaim(arts []prunableImage, keep int) []prunableImage {
	if keep < 1 {
		keep = 1
	}
	if len(arts) <= keep {
		return nil
	}
	kept := make(map[string]bool, keep)
	for _, a := range arts[:keep] {
		kept[a.ref] = true
	}
	out := make([]prunableImage, 0, len(arts)-keep)
	for _, a := range arts[keep:] {
		if kept[a.ref] {
			out = append(out, prunableImage{id: a.id, ref: ""})
			continue
		}
		out = append(out, a)
	}
	return out
}

// markFailed applies compensation C2 (remove the candidate, never touch the
// serving container) and records the terminal failed state (§9).
func (r *deploymentRun) markFailed(ctx context.Context, cause error) {
	if r.preview != nil {
		_ = r.h.Store.SetPreviewStatus(ctx, store.SetPreviewStatusParams{ID: r.preview.ID, Status: store.PreviewStatusFailed})
		(&PreviewFeedback{Store: r.h.Store, Keyring: r.h.Keyring, Logger: r.h.Logger}).Notify(ctx, r.app, *r.preview, "failure")
	}
	if r.rt != nil {
		candidate := r.namingIdentity() + "-next"
		_ = removeNamedContainers(ctx, r.rt, false, candidate)
	}
	msg := cause.Error()
	_ = r.h.Store.SetDeploymentError(ctx, store.SetDeploymentErrorParams{ID: r.d.ID, ErrorMessage: &msg})
	r.d.ErrorMessage = &msg
	// Through setStatus, so the deployment.failed.v1 outbox event is emitted
	// (enriched with the error): a failure MUST notify, not just flip a row.
	_ = r.setStatus(ctx, store.DeploymentStatusFailed)
	if r.app.Resource.ID != 0 {
		_ = r.h.Store.SetResourceObservedStatus(ctx, store.SetResourceObservedStatusParams{ID: r.app.Resource.ID, ObservedStatus: store.ResourceObservedStatusUnknown})
	}
	r.h.Logger.Warn("deployment failed", "deployment_uuid", pguuid.String(r.d.Uuid), "error", cause)
}

// staticDockerfileName is the generated Dockerfile of the static build pack.
// The dot-prefixed name keeps it out of the way of a repository that already
// has a Dockerfile of its own.
const staticDockerfileName = ".akerdock-static.Dockerfile"

// defaultNginxConfig serves the published directory. `try_files … /index.html`
// makes a single-page application work: a deep link like /users/42 is a client
// route, not a file, and must not 404 (§5.2).
const defaultNginxConfig = `server {
    listen 80;
    server_name _;
    root /usr/share/nginx/html;
    index index.html;
    location / {
        try_files $uri $uri/ /index.html;
    }
}
`

// writeStaticDockerfile synthesizes the image of a static site: the published
// directory copied into nginx. Nothing is executed at build time, so the
// output is exactly what the repository contains — no toolchain, no lockfile,
// no surprise.
//
// The published directory comes from the build config and is validated at the
// API edge (INV-012): it is interpolated into a Dockerfile that a remote shell
// then builds.
func (r *deploymentRun) writeStaticDockerfile(ctx context.Context, srcDir, baseDir string) error {
	publish := "."
	if p := r.app.BuildConfig.PublishDirectory; p != nil && *p != "" {
		publish = "./" + strings.Trim(*p, "/")
	}
	nginxConf := defaultNginxConfig
	if c := r.app.BuildConfig.CustomNginxConfig; c != nil && *c != "" {
		nginxConf = *c
	}

	// The nginx config is written next to the sources and COPYied in: passing
	// it through the Dockerfile would mean escaping a multi-line value into a
	// RUN, which is exactly the kind of thing INV-012 exists to avoid.
	confPath := srcDir + "/" + baseDir + "/.akerdock-nginx.conf"
	if err := r.bhostops().WriteFile(ctx, agentwire.FileWriteParams{
		Path: confPath, Content: []byte(nginxConf), Mode: 0o644,
	}); err != nil {
		return fmt.Errorf("writing the nginx config failed: %s", firstLine(err.Error()))
	}

	dockerfile := fmt.Sprintf(`FROM nginx:alpine
COPY .akerdock-nginx.conf /etc/nginx/conf.d/default.conf
COPY %s /usr/share/nginx/html
EXPOSE 80
`, publish)
	path := srcDir + "/" + baseDir + "/" + staticDockerfileName
	if err := r.bhostops().WriteFile(ctx, agentwire.FileWriteParams{
		Path: path, Content: []byte(dockerfile), Mode: 0o644,
	}); err != nil {
		return fmt.Errorf("writing the static Dockerfile failed: %s", firstLine(err.Error()))
	}
	return nil
}

// buildWithNixpacks produces the image with the nixpacks builder (§5.5): the
// plan is written next to the sources and traced in the build logs — it is the
// only way to answer "why did it pick Node 20" after the fact — then the image
// is built with the same tag and labels as any other build pack, so everything
// downstream (rolling switch, rollback artifacts, cleanup) is unchanged.
//
// Build-time variables are exported into the process environment (nixpacks
// propagates it), never passed in argv (INV-003).
//
// The binary is provisioned at onboarding, but a server validated before this
// build pack existed — or one where the install failed — has none. Rather than
// fail with "not found", the install is retried here, where the user did ask
// for nixpacks.
func (r *deploymentRun) buildWithNixpacks(ctx context.Context, srcDir, baseDir, appDir, imageRef, labels, sha, noCache string) error {
	if err := r.step(ctx, "provision_nixpacks", func() (*sshexec.Result, error) {
		if err := installNixpacks(ctx, r.client); err != nil {
			return nil, fmt.Errorf("nixpacks %s is not available on this server: %w", NixpacksVersion, err)
		}
		return r.bc().Run(ctx, nixpacksBin+" --version")
	}); err != nil {
		return err
	}

	planFile := fmt.Sprintf("%s/.nixpacks-plan.json", srcDir)
	if err := r.step(ctx, "nixpacks_plan", func() (*sshexec.Result, error) {
		return r.bc().Run(ctx, fmt.Sprintf(
			"cd %s/%s && set -a && . %s/env/build.env && set +a && %s plan . --format json | tee %s",
			srcDir, baseDir, appDir, nixpacksBin, planFile))
	}); err != nil {
		return err
	}

	// Static mode (§5.5): the repository is a site generator, not a server. The
	// nixpacks image is only the BUILDER — what ships is nginx serving the
	// directory the build produced. Deploying the builder image instead would
	// ship a whole Node toolchain, and would serve nothing: `npm run build`
	// exits, and a container whose command exits is a container that is down.
	publish := ""
	if p := r.app.BuildConfig.PublishDirectory; p != nil && *p != "" {
		publish = strings.Trim(*p, "/")
	}
	buildRef := imageRef
	staticFlags := ""
	if publish != "" {
		buildRef = imageRef + "-builder"
		// A site generator has no start command, and nixpacks refuses to build
		// without one. It is right to refuse in general — an image that starts
		// nothing is a container that is down — but here the image is a builder
		// whose output we extract, and nothing will ever start it.
		staticFlags = " --no-error-without-start"
	}

	if err := r.streamStep(ctx, "build", func(onOutput func(string)) (*sshexec.Result, error) {
		return r.bc().RunStream(ctx, fmt.Sprintf(
			"cd %s/%s && set -a && . %s/env/build.env && set +a && %s build . --name %s %s --label akerdock.commit_sha=%s%s%s",
			srcDir, baseDir, appDir, nixpacksBin, buildRef, labels, sha, noCache, staticFlags), onOutput)
	}); err != nil {
		return err
	}
	if publish == "" {
		return nil
	}

	return r.streamStep(ctx, "package_static", func(onOutput func(string)) (*sshexec.Result, error) {
		conf := defaultNginxConfig
		if c := r.app.BuildConfig.CustomNginxConfig; c != nil && *c != "" {
			conf = *c
		}
		confPath := srcDir + "/" + baseDir + "/.akerdock-nginx.conf"
		if err := r.bhostops().WriteFile(ctx, agentwire.FileWriteParams{
			Path: confPath, Content: []byte(conf), Mode: 0o644,
		}); err != nil {
			return nil, fmt.Errorf("writing the nginx config failed: %s", firstLine(err.Error()))
		}
		// Nixpacks builds into /app. The publish directory is validated at the
		// API edge (INV-012) before it is interpolated into the Dockerfile.
		dockerfile := fmt.Sprintf(`FROM %s AS build
FROM nginx:alpine
COPY .akerdock-nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/%s /usr/share/nginx/html
EXPOSE 80
`, buildRef, publish)
		path := srcDir + "/" + baseDir + "/" + staticDockerfileName
		if err := r.bhostops().WriteFile(ctx, agentwire.FileWriteParams{
			Path: path, Content: []byte(dockerfile), Mode: 0o644,
		}); err != nil {
			return nil, fmt.Errorf("writing the static Dockerfile failed: %s", firstLine(err.Error()))
		}
		// Typed and streamed like every other build (ADR-055): the packaging
		// can pull a base image and copy a large asset tree, so its progress
		// must show live rather than land in one block at the end.
		if err := r.agentBuild(ctx, onOutput, agentwire.ImageBuildParams{
			ContextDir: strings.TrimRight(srcDir+"/"+baseDir, "/"), Dockerfile: staticDockerfileName,
			Tags:   []string{imageRef},
			Labels: r.buildLabels(map[string]string{"akerdock.commit_sha": sha}),
		}); err != nil {
			return nil, fmt.Errorf("packaging the built assets into nginx failed: %s", firstLine(err.Error()))
		}
		// The builder image was only the intermediate; tolerate its absence.
		if _, err := r.bcrt().ImageRemove(ctx, buildRef, image.RemoveOptions{}); err != nil && !dockerruntime.IsNotFound(err) && !dockerruntime.IsConflict(err) {
			r.h.Logger.Debug("builder image not removed", "image", buildRef, "error", err)
		}
		return nil, nil
	})
}

// hookTimeout bounds a user-supplied command (§10, §11). A hook that hangs must
// not hold a deployment slot forever.
const hookTimeout = 10 * time.Minute

// runHook executes a pre/post-deployment command inside a container (§10).
//
// The command is user-supplied and deliberately IS a shell command, so it is
// not sanitized — it is *quoted*: shellQuote makes it a single literal argument
// to `sh -c`, which is what stops a command containing a quote from breaking
// out of the docker exec line and becoming several commands (INV-012).
//
// It runs with the container's environment, so the runtime variables are
// already there and no value is ever added to argv (INV-003).
//
// A missing container (first deployment, stopped application) is a skipped
// step, not a failure — there is nothing to run the command in.
func (r *deploymentRun) runHook(ctx context.Context, name string, command *string, container string) error {
	if command == nil || strings.TrimSpace(*command) == "" {
		r.skipStep(ctx, name, "no command configured")
		return nil
	}
	rt, err := r.h.Docker.Runtime(ctx, r.server.ID)
	if err != nil {
		return err
	}
	// Prefix, not equality: the compose hooks are per service
	// ("pre_deployment_<svc>") and share the same §10 rule — no existing
	// container means nothing to run in, not a failure.
	if strings.HasPrefix(name, "pre_deployment") {
		resp, err := rt.ContainerInspect(ctx, container)
		if err != nil {
			if dockerruntime.IsNotFound(err) {
				r.skipStep(ctx, name, "skipped: no running container")
				return nil
			}
			return err
		}
		if resp.State == nil || !resp.State.Running {
			r.skipStep(ctx, name, "skipped: no running container")
			return nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	return r.step(ctx, name, func() (*sshexec.Result, error) {
		out, exit, err := execCapture(ctx, rt, container, []string{"sh", "-c", *command})
		if err != nil {
			return nil, err
		}
		return &sshexec.Result{Stdout: out, ExitCode: exit}, nil
	})
}

// resume applies the crash-recovery rules of §2.5 / §4. It returns true when
// the deployment was carried to a terminal state here and execute() must not
// continue.
//
// The dangerous state is `switching`. Everything before it is idempotent by
// construction — the clone destroys its directory before re-cloning, the build
// is content-addressed, the candidate is force-removed before being recreated —
// so replaying is safe and cheap. But a blind replay of `switching` could
// switch twice: stop a container that is already the live one, or repoint the
// proxy at a candidate that no longer exists. Hence: inspect first, never
// switch without knowing the outcome of the previous attempt (INV-004/005).
func (r *deploymentRun) resume(ctx context.Context, appUUID, oldName, candidate string) (bool, error) {
	switch r.d.Status {
	case store.DeploymentStatusQueued:
		return false, nil // never started: nothing to recover
	case store.DeploymentStatusSwitching, store.DeploymentStatusFinishing:
		// Fall through to the inspection below.
	default:
		// preparing / cloning / building / starting / healthchecking: every
		// remote effect is idempotent or destroyed-then-redone. Replay.
		r.h.Logger.Warn("resuming a crashed deployment by replay",
			"deployment_uuid", pguuid.String(r.d.Uuid), "state", string(r.d.Status))
		return false, nil
	}

	r.h.Logger.Warn("resuming a deployment that crashed during the switch — inspecting before acting",
		"deployment_uuid", pguuid.String(r.d.Uuid))

	// What actually exists on the server decides, not what we believe.
	oldState, err := r.containerState(ctx, oldName)
	if err != nil {
		return false, err
	}
	nextState, err := r.containerState(ctx, candidate)
	if err != nil {
		return false, err
	}

	// Every branch below records what the inspection found: an operator must be
	// able to see WHY a resumed deployment did what it did.
	rec := func(msg string) {
		r.seq++
		if id, err := r.h.Store.CreateDeploymentStep(ctx, store.CreateDeploymentStepParams{
			DeploymentID: r.d.ID, Seq: r.seq, Name: "resume_inspect",
		}); err == nil {
			_ = r.h.Store.FinishDeploymentStep(ctx, store.FinishDeploymentStepParams{
				ID: id, Status: store.DeploymentStepStatusSucceeded, Log: &msg,
			})
		}
	}

	switch {
	case nextState == "absent" && oldState == "running":
		// The rename already happened: the candidate IS the live container now.
		// Nothing left to switch — finish the bookkeeping (§4, finishing is
		// idempotent).
		rec("the switch had completed (candidate promoted); finishing")
		if err := r.applyRouting(ctx, appUUID); err != nil {
			return true, err
		}
		return true, r.finish(ctx, appUUID)

	case nextState == "running" && oldState == "absent":
		// The old container is gone but the rename never happened: resume at the
		// rename (§4, case c).
		rec("old container gone, candidate not yet promoted; resuming at the rename")
		if err := r.step(ctx, "switch", func() (*sshexec.Result, error) {
			return nil, r.rt.ContainerRename(ctx, candidate, appUUID)
		}); err != nil {
			return true, err
		}
		if err := r.applyRouting(ctx, appUUID); err != nil {
			return true, err
		}
		return true, r.finish(ctx, appUUID)

	case nextState == "running" && oldState == "running":
		// Both alive: the switch had not stopped the old one yet. The candidate
		// is healthy and routing may already point at it — replaying the stop +
		// rename is exactly what the previous worker was about to do, and it is
		// idempotent.
		rec("both containers alive; resuming at the stop of the old one")
		grace := r.app.RuntimeConfig.StopGracePeriodSeconds
		if err := r.step(ctx, "switch", func() (*sshexec.Result, error) {
			return nil, r.promoteCandidate(ctx, oldName, candidate, appUUID, int(grace))
		}); err != nil {
			return true, err
		}
		if err := r.applyRouting(ctx, appUUID); err != nil {
			return true, err
		}
		return true, r.finish(ctx, appUUID)

	default:
		// The candidate is gone (or dead) and the old one is not running: the
		// switch left nothing usable. Fail loudly rather than guess — a second
		// blind switch is exactly what INV-004 forbids.
		rec(fmt.Sprintf("unrecoverable: old=%s, candidate=%s", oldState, nextState))
		return true, fmt.Errorf("crashed during the switch and neither container is usable (old=%s, candidate=%s)", oldState, nextState)
	}
}

// finish performs the idempotent tail of a deployment (§4, finishing).
func (r *deploymentRun) finish(ctx context.Context, appUUID string) error {
	if err := r.setStatus(ctx, store.DeploymentStatusFinishing); err != nil {
		return err
	}
	_ = r.h.Store.SetResourceDesiredStatus(ctx, store.SetResourceDesiredStatusParams{
		ID: r.app.Resource.ID, DesiredStatus: store.ResourceDesiredStatusRunning,
	})
	_ = r.h.Store.SetResourceObservedStatus(ctx, store.SetResourceObservedStatusParams{
		ID: r.app.Resource.ID, ObservedStatus: store.ResourceObservedStatusHealthy,
	})
	// An adopted resource is now fully normalized (§20.7 step 4): the remote
	// objects converged onto the uuid-derived names, the pointer is obsolete.
	if r.preview == nil && adoption.ParsePointer(r.app.Resource.Adoption) != nil {
		_ = r.h.Store.ClearResourceAdoption(ctx, r.app.Resource.ID)
	}
	r.recordArtifact(ctx)
	// Reclaim images beyond the rollback retention now that the new one is the
	// newest (ADR-006, §29.4) — keeps the server's disk bounded per deployment.
	r.pruneOldImages(ctx)
	if r.preview != nil {
		_ = r.h.Store.SetPreviewDeployed(ctx, r.preview.ID)
		(&PreviewFeedback{Store: r.h.Store, Keyring: r.h.Keyring, Logger: r.h.Logger}).Notify(ctx, r.app, *r.preview, "success")
	}
	if err := r.setStatus(ctx, store.DeploymentStatusSucceeded); err != nil {
		return err
	}
	r.h.Logger.Info("deployment succeeded", "deployment_uuid", pguuid.String(r.d.Uuid), "app_uuid", appUUID)
	return nil
}

// dialBuildServer picks a build server and connects to it.
//
// The selection is random among the ready build servers of the team (§3.4).
// Random rather than "the first": a fixed order sends every build of every
// application to the same machine, which is the opposite of what a fleet of
// build servers is for.
func (r *deploymentRun) dialBuildServer(ctx context.Context) error {
	builders, err := r.h.Store.ListReadyBuildServers(ctx, r.app.Resource.TeamID)
	if err != nil {
		return err
	}
	if len(builders) == 0 {
		// Loud, not silent: falling back to building on the production server
		// would quietly do the exact thing the operator asked us not to do.
		return fmt.Errorf("this application is configured to build on a build server, and none is ready")
	}
	pick := builders[randIndex(len(builders))]

	// An image built for another architecture starts and immediately dies with
	// "exec format error" — a failure that looks like the application's fault.
	if pick.Architecture != nil && r.server.Architecture != nil && *pick.Architecture != *r.server.Architecture {
		return fmt.Errorf("the build server is %s and the deployment server is %s: the image would not run there",
			*pick.Architecture, *r.server.Architecture)
	}
	if err := r.reserveBuildServer(ctx, pick); err != nil {
		return err
	}

	key, err := r.h.Store.GetPrivateKeyByID(ctx, pick.PrivateKeyID)
	if err != nil {
		return fmt.Errorf("the private key of the build server vanished: %w", err)
	}
	pem, err := r.h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return err
	}
	client, err := sshexec.Dial(ctx, pick.Host, int(pick.Port), pick.SshUser, string(pem),
		time.Duration(pick.SshTimeoutSeconds)*time.Second, pinnedHostKey(pick))
	if err != nil {
		return fmt.Errorf("ssh connect to the build server: %w", err)
	}
	r.builder, r.buildServer = client, &pick
	// The build server's channel (ADR-055): builds, tag, push and inspect run
	// typed there — agents are provisioned on every ready server, build
	// servers included.
	if r.brt, err = r.h.Docker.Runtime(ctx, pick.ID); err != nil {
		return fmt.Errorf("the build server's agent is not connected: %w", err)
	}
	if r.bhops, err = r.h.HostOps.HostOps(ctx, pick.ID); err != nil {
		return fmt.Errorf("the build server's agent is not connected: %w", err)
	}

	// The working directory exists on the deployment server because `prepare`
	// created it there. The build machine has never seen this application, and
	// the very first thing the build does is write build.env into it.
	appDir := "/var/lib/akerdock/applications/" + pguuid.String(r.app.Resource.Uuid)
	if res, err := client.Run(ctx, fmt.Sprintf("mkdir -p %s/env && chmod 700 %s %s/env", appDir, appDir, appDir)); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("cannot prepare the working directory on the build server")
	}
	r.h.Logger.Info("building on a build server", "server", pick.Name, "deployment", pguuid.String(r.d.Uuid))
	return nil
}

func (r *deploymentRun) reserveBuildServer(ctx context.Context, server store.Server) error {
	waitLogged := false
	for {
		rows, err := r.h.Store.AssignDeploymentBuildServerUnlessCleanupRunning(ctx,
			store.AssignDeploymentBuildServerUnlessCleanupRunningParams{
				BuildServerID: server.ID,
				DeploymentID:  r.d.ID,
			})
		if err != nil {
			return err
		}
		if rows > 0 {
			r.d.BuildServerID = &server.ID
			return nil
		}
		if err := r.checkpoint(ctx); err != nil {
			return err
		}
		if !waitLogged {
			r.h.Logger.Info("deployment waiting for build-server cleanup",
				"deployment_uuid", pguuid.String(r.d.Uuid), "server_id", server.ID)
			waitLogged = true
		}
		timer := time.NewTimer(deploymentCleanupPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// pushBuiltImage tags the image for the push registry, pushes it from the build
// server, and pulls it back BY DIGEST on the deployment server. It returns the
// reference the container will actually run.
//
// Both sides authenticate PER REQUEST through the channel (ADR-051/055): no
// `docker login` ever runs, so no registry token ever lands in any host's
// ~/.docker/config.json (INV-003) — and there is no logout to forget.
func (r *deploymentRun) pushBuiltImage(ctx context.Context) (string, error) {
	credID := r.app.BuildConfig.PushRegistryCredentialID
	if credID == nil {
		return "", fmt.Errorf("no push registry is configured, so the image built on the build server could never be pulled here")
	}
	cred, err := r.h.Store.GetRegistryCredentialByID(ctx, *credID)
	if err != nil {
		return "", fmt.Errorf("the push registry credential vanished: %w", err)
	}
	password, err := r.h.Keyring.Decrypt("registry_credentials", "password_enc", pguuid.String(cred.Uuid), cred.PasswordEnc)
	if err != nil {
		return "", err
	}
	auth, err := registry.EncodeAuthConfig(registry.AuthConfig{
		Username: cred.Username, Password: string(password), ServerAddress: cred.RegistryUrl,
	})
	if err != nil {
		return "", err
	}

	local := *r.d.ImageName + ":" + *r.d.ImageTag
	remoteRepo := cred.RegistryUrl + "/" + *r.d.ImageName
	remote := remoteRepo + ":" + *r.d.ImageTag

	if err := r.streamStep(ctx, "push", func(onOutput func(string)) (*sshexec.Result, error) {
		if err := r.bcrt().ImageTag(ctx, local, remote); err != nil {
			return nil, err
		}
		rc, err := r.bcrt().ImagePush(ctx, remote, image.PushOptions{RegistryAuth: auth})
		if err != nil {
			return nil, fmt.Errorf("pushing %s failed: %s", remote, firstLine(err.Error()))
		}
		defer func() { _ = rc.Close() }()
		// The daemon reports push failures IN the stream, like pulls.
		if err := streamPullProgress(rc, onOutput); err != nil {
			return nil, fmt.Errorf("pushing %s failed: %s", remote, firstLine(err.Error()))
		}
		return nil, nil
	}); err != nil {
		return "", err
	}

	// The digest is read on the build server, right after the push: it is the
	// identity of what was pushed, and it is what the target is told to pull.
	// Pulling by tag would let a racing push swap the image under us. The
	// image may also carry digests of OTHER repositories (its base pull): the
	// one under the push repo is the identity that was just minted.
	digest := ""
	if err := r.step(ctx, "resolve_pushed_digest", func() (*sshexec.Result, error) {
		resp, err := r.bcrt().ImageInspect(ctx, remote)
		if err != nil {
			return nil, err
		}
		if digest = pushedDigest(resp.RepoDigests, remoteRepo); digest == "" {
			return nil, fmt.Errorf("the registry returned no digest for the pushed image")
		}
		return nil, nil
	}); err != nil {
		return "", err
	}

	if err := r.streamStep(ctx, "pull_from_registry", func(onOutput func(string)) (*sshexec.Result, error) {
		// The TARGET pulls by digest through the channel, authenticating per
		// request (ADR-051): nothing lands in its ~/.docker/config.json.
		auth, err := registry.EncodeAuthConfig(registry.AuthConfig{
			Username: cred.Username, Password: string(password), ServerAddress: cred.RegistryUrl,
		})
		if err != nil {
			return nil, err
		}
		rc, err := r.rt.ImagePull(ctx, digest, image.PullOptions{RegistryAuth: auth})
		if err != nil {
			return nil, fmt.Errorf("the deployment server cannot pull %s: %s", digest, firstLine(err.Error()))
		}
		defer func() { _ = rc.Close() }()
		if err := streamPullProgress(rc, onOutput); err != nil {
			return nil, fmt.Errorf("the deployment server cannot pull %s: %s", digest, firstLine(err.Error()))
		}
		return nil, nil
	}); err != nil {
		return "", err
	}

	r.digest = digest
	r.d.ImageName = ptrStr(cred.RegistryUrl + "/" + *r.d.ImageName)
	_ = r.h.Store.SetDeploymentImage(ctx, store.SetDeploymentImageParams{
		ID: r.d.ID, ImageName: r.d.ImageName, ImageTag: r.d.ImageTag,
	})
	_ = r.h.Store.SetDeploymentImageDigest(ctx, store.SetDeploymentImageDigestParams{ID: r.d.ID, ImageDigest: &digest})
	return digest, nil
}

// pushedDigest picks the pushed image's identity among an image's repo
// digests: the one minted under the push repository — the image may also
// carry digests of OTHER repositories (its base pull), and the shell era's
// blind "index 0" could hand the target one of those. Falls back to the
// first digest, then to absent.
func pushedDigest(repoDigests []string, remoteRepo string) string {
	for _, d := range repoDigests {
		if strings.HasPrefix(d, remoteRepo+"@") {
			return d
		}
	}
	if len(repoDigests) > 0 {
		return repoDigests[0]
	}
	return ""
}

// randIndex picks one of n. crypto/rand is overkill for load spreading, but it
// is what is already imported and it cannot be seeded wrong.
func randIndex(n int) int {
	if n <= 1 {
		return 0
	}
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return 0
	}
	return int(binary.BigEndian.Uint16(b)) % n
}
