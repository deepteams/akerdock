package jobs

import (
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

	"github.com/deepteams/akerdock/internal/adoption"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/githubapp"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
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
	// ControlPlanePort is the published port of this instance (AKERDOCK_INSTANCE_PORT):
	// on the server that HOSTS the instance, the preview forward-auth talks to
	// the control plane straight through the Docker host gateway — never the
	// public hairpin, whose latency taxes every preview request (ADR-030).
	ControlPlanePort int
}

// ImageRef and TagRef bound what can reach a remote shell (INV-012); they
// are also enforced at application creation.
var (
	ImageRef = regexp.MustCompile(`^[a-z0-9]+((\.|_{1,2}|-+|/|:[0-9]+/)[a-z0-9]+)*$`)
	TagRef   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
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
		flush := time.Since(lastFlush) >= time.Second
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
	if err := r.h.Store.SetDeploymentStatus(ctx, store.SetDeploymentStatusParams{ID: r.d.ID, Status: s}); err != nil {
		return err
	}
	// Every transition publishes a versioned outbox event (deployment-engine §12.1).
	if r.h.Audit != nil {
		var teamUUID pgtype.UUID
		if team, err := r.h.Store.GetTeamByID(ctx, r.app.Resource.TeamID); err == nil {
			teamUUID = team.Uuid
		}
		r.h.Audit.Outbox(ctx, r.h.Store, "deployment."+string(s)+".v1", teamUUID, r.app.Resource.Uuid,
			"deployment:"+pguuid.String(r.d.Uuid), map[string]any{
				"deployment_uuid": pguuid.String(r.d.Uuid),
				"status":          string(s),
			})

		// A preview's own lifecycle event, once its deployment succeeds: the
		// first successful deploy CREATED it, any later one UPDATED it (a new
		// commit). Destruction is emitted by the teardown job.
		if r.preview != nil && s == store.DeploymentStatusSucceeded {
			evt := "application.preview.created.v1"
			if r.preview.LastDeployedAt.Valid {
				evt = "application.preview.updated.v1"
			}
			fqdn := ""
			if r.preview.Fqdn != nil {
				fqdn = *r.preview.Fqdn
			}
			r.h.Audit.Outbox(ctx, r.h.Store, evt, teamUUID, r.app.Resource.Uuid,
				"preview:"+pguuid.String(r.preview.Uuid), map[string]any{
					"preview_uuid": pguuid.String(r.preview.Uuid),
					"pr_id":        r.preview.PrID,
					"fqdn":         fqdn,
				})
		}
	}
	return nil
}

// errCancelled aborts the pipeline at a cooperative checkpoint (§2.6).
var errCancelled = errors.New("deployment cancelled")

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
	if r.client != nil {
		candidate := r.namingIdentity() + "-next"
		_, _ = r.client.Run(ctx, "docker rm -f "+candidate+" >/dev/null 2>&1 || true")
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
	// The build server (§3.4): a separate machine, dialled only when the
	// application asked for one and only for a build pack that builds something.
	if r.app.BuildConfig.UseBuildServer && r.app.BuildConfig.BuildPack != store.BuildPackImage && !r.d.IsRollback {
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

	// runtime.env carries the decrypted variables — uploaded via stdin so
	// values never appear in argv (INV-003), file mode 0600 (§5.1).
	envFile, envKeys, err := r.renderRuntimeEnv(ctx)
	if err != nil {
		return err
	}
	if err := r.step(ctx, "prepare", func() (*sshexec.Result, error) {
		return r.client.RunInput(ctx, fmt.Sprintf(
			"docker info --format ok >/dev/null && mkdir -p %s/env && chmod 700 %s %s/env && umask 077 && cat > %s/env/runtime.sh && (docker network inspect %s >/dev/null 2>&1 || docker network create --label akerdock.managed=true %s)",
			appDir, appDir, appDir, appDir, r.dest.Network, r.dest.Network), envFile)
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
	// Rollback: the artifact is already built and verified — no rebuild
	// (ADR-006). The image reference was resolved at trigger time.
	case r.d.IsRollback:
		r.skipStep(ctx, "clone", "rollback (no source)")
		runRef = *r.d.ImageName
		if r.d.ImageDigest != nil && strings.Contains(*r.d.ImageDigest, "@") {
			runRef = *r.d.ImageDigest // registry digest, reproducible
		} else if r.d.ImageTag != nil {
			runRef += ":" + *r.d.ImageTag
		}
		ref := runRef
		if err := r.step(ctx, "verify_artifact", func() (*sshexec.Result, error) {
			res, err := r.client.Run(ctx, "docker image inspect --format '{{.Id}}' "+ref)
			if err == nil && res.ExitCode != 0 {
				return res, fmt.Errorf("the rollback image %s is no longer present on the server", ref)
			}
			return res, err
		}); err != nil {
			return err
		}
	case r.app.BuildConfig.BuildPack == store.BuildPackImage:
		r.skipStep(ctx, "clone", "no git source (image build pack)")
		imageRef := *r.d.ImageName + ":" + *r.d.ImageTag
		// A private image needs a `docker login` on the server first. The
		// credential is logged out again as soon as the pull is done, whatever
		// its outcome: leaving the server authenticated would leave the token in
		// ~/.docker/config.json for anything else on that host to use.
		logout, err := r.registryLogin(ctx)
		if err != nil {
			return err
		}
		defer logout()
		if err := r.step(ctx, "pull", func() (*sshexec.Result, error) {
			return r.client.Run(ctx, "docker pull "+imageRef)
		}); err != nil {
			return err
		}
		if err := r.step(ctx, "resolve_digest", func() (*sshexec.Result, error) {
			res, err := r.client.Run(ctx, fmt.Sprintf("docker image inspect --format '{{index .RepoDigests 0}}' %s", imageRef))
			if err == nil && res.ExitCode == 0 && res.Stdout != "" {
				r.digest = firstLine(res.Stdout)
				_ = r.h.Store.SetDeploymentImageDigest(ctx, store.SetDeploymentImageDigestParams{ID: r.d.ID, ImageDigest: &r.digest})
			}
			return res, err
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
			runRef, err = r.buildFromDockerfile(ctx, appUUID, appDir, labels)
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

	// --- starting ---------------------------------------------------------
	if err := r.checkpoint(ctx); err != nil {
		return err
	}
	if err := r.setStatus(ctx, store.DeploymentStatusStarting); err != nil {
		return err
	}
	limits := ""
	if r.app.RuntimeConfig.MemoryLimit != nil {
		limits += " --memory " + *r.app.RuntimeConfig.MemoryLimit
	}
	// Declared volumes are created idempotently before the candidate, and
	// mounted into it (§5.3.4). Bind mount host directories are created too.
	mounts, prepare, err := r.renderStorages(ctx, appUUID, runRef)
	if err != nil {
		return err
	}
	healthFlags, hasHealthCheck := r.renderHealthCheck(ctx)
	// Rolling eligibility (§7.3): a working health check is required. Without
	// one, the deployment falls back to stop-then-start (§7.4).
	r.rolling = hasHealthCheck
	if prepare != "" {
		if err := r.step(ctx, "prepare_storages", func() (*sshexec.Result, error) {
			return r.client.Run(ctx, prepare)
		}); err != nil {
			return err
		}
	}
	// Non-rolling fallback: the old container is stopped first and the new
	// one is created directly under the final name (§7.4).
	target := candidate
	stopOld := ""
	if !r.rolling {
		target = appUUID
		stopOld = fmt.Sprintf("docker stop -t %d %s >/dev/null 2>&1 && docker rm %s >/dev/null 2>&1; ",
			r.app.RuntimeConfig.StopGracePeriodSeconds, oldName, oldName)
	}
	r.target = target
	if err := r.step(ctx, "start_candidate", func() (*sshexec.Result, error) {
		// The values are sourced into the shell and referenced by NAME only:
		// `-e KEY` makes Docker read KEY from the environment, so a multiline
		// value (a PEM key, a JSON blob) works and never lands in argv (INV-003).
		return r.client.Run(ctx, fmt.Sprintf(
			". %s/env/runtime.sh; %sdocker rm -f %s >/dev/null 2>&1 || true; docker create --name %s --restart unless-stopped --stop-timeout %d --network %s%s %s%s%s%s %s >/dev/null && docker start %s",
			appDir, stopOld, target, target, r.app.RuntimeConfig.StopGracePeriodSeconds, r.dest.Network, envFlags(envKeys),
			labels, limits, mounts, healthFlags, runRef, target))
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
	if err := r.step(ctx, "healthcheck", func() (*sshexec.Result, error) {
		if !hasHealthCheck {
			// No health check: wait for a stable running state (§4).
			res, err := r.client.Run(ctx, fmt.Sprintf("sleep 10; docker inspect --format '{{.State.Status}}' %s", target))
			if err == nil && res.ExitCode == 0 && firstLine(res.Stdout) != "running" {
				return res, r.candidateFailure(ctx, target, fmt.Sprintf("container is %q, expected running", firstLine(res.Stdout)))
			}
			return res, err
		}
		// Poll the container health until healthy, bounded by the configured
		// budget: start_period + (interval + timeout) × retries + 30 s (§4).
		res, err := r.client.Run(ctx, fmt.Sprintf(
			"deadline=$(( $(date +%%s) + %d )); while [ $(date +%%s) -lt $deadline ]; do "+
				"st=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' %s 2>/dev/null); "+
				"case \"$st\" in healthy) echo healthy; exit 0;; unhealthy) echo unhealthy; exit 1;; "+
				"none) echo no-healthcheck; exit 0;; esac; sleep 2; done; echo timeout; exit 1",
			r.healthBudget, target))
		if err == nil && res.ExitCode != 0 {
			return res, r.candidateFailure(ctx, target, "the health check did not turn healthy ("+firstLine(res.Stdout)+")")
		}
		return res, err
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
			res, err := r.client.Run(ctx, fmt.Sprintf(
				"docker inspect --format '{{(index .NetworkSettings.Networks \"%s\").IPAddress}}' %s", r.dest.Network, candidate))
			if err == nil && res.ExitCode == 0 {
				candidateIP = firstLine(res.Stdout)
				if candidateIP == "" {
					return res, fmt.Errorf("could not resolve the candidate IP on network %s", r.dest.Network)
				}
			}
			return res, err
		}); err != nil {
			return err
		}
		if err := r.applyRoutingTo(ctx, appUUID, candidateIP); err != nil {
			return err
		}
		if err := r.step(ctx, "switch", func() (*sshexec.Result, error) {
			return r.client.Run(ctx, fmt.Sprintf(
				"(docker stop -t %d %s >/dev/null 2>&1 && docker rm %s >/dev/null 2>&1) || true; docker rename %s %s",
				grace, oldName, oldName, candidate, appUUID))
		}); err != nil {
			return err
		}
	} else if err := r.step(ctx, "switch", func() (*sshexec.Result, error) {
		// Non-rolling: the old container is already gone (§7.4).
		return r.client.Run(ctx, "docker inspect --format '{{.State.Status}}' "+appUUID)
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
func (r *deploymentRun) buildFromDockerfile(ctx context.Context, appUUID, appDir, labels string) (string, error) {
	r.skipStep(ctx, "clone", "inline Dockerfile (no git source)")

	dockerfile := r.app.BuildConfig.DockerfileContent
	if dockerfile == nil || *dockerfile == "" {
		return "", fmt.Errorf("the application has no Dockerfile content")
	}
	depUUID := pguuid.String(r.d.Uuid)
	tag := strings.ReplaceAll(depUUID, "-", "")[:12]
	imageRef := "akerdock/" + appUUID + ":" + tag
	srcDir := fmt.Sprintf("%s/source/%s", appDir, depUUID)

	noCache := ""
	if r.d.ForceRebuild {
		noCache = " --no-cache"
	}
	// Build-time variables reach an inline Dockerfile exactly as they reach a
	// git one: plain ones as ARGs, secret ones as BuildKit secrets (§5.2).
	buildEnv, buildArgs, err := r.renderBuildEnv(ctx)
	if err != nil {
		return "", err
	}
	if res, err := r.bc().RunInput(ctx, fmt.Sprintf(
		"mkdir -p %s/env && umask 077 && cat > %s/env/build.env", appDir, appDir), buildEnv); err != nil || res.ExitCode != 0 {
		return "", fmt.Errorf("uploading build.env failed")
	}
	if err := r.streamStep(ctx, "build", func(onOutput func(string)) (*sshexec.Result, error) {
		return r.bc().RunInputStream(ctx, fmt.Sprintf(
			"mkdir -p %s && cat > %s/Dockerfile && set -a && . %s/env/build.env && set +a && "+
				"DOCKER_BUILDKIT=1 docker build --progress plain%s%s -t %s %s --label akerdock.commit_sha= %s",
			srcDir, srcDir, appDir, noCache, buildArgs.Flags(), imageRef, labels, srcDir), *dockerfile, onOutput)
	}); err != nil {
		return "", err
	}
	r.d.ImageName, r.d.ImageTag = ptrStr("akerdock/"+appUUID), &tag
	_ = r.h.Store.SetDeploymentImage(ctx, store.SetDeploymentImageParams{ID: r.d.ID, ImageName: r.d.ImageName, ImageTag: r.d.ImageTag})

	// Local builds have no registry digest: the image ID is recorded so
	// the rollback retention can pin it (ADR-006, local mode).
	if err := r.step(ctx, "resolve_digest", func() (*sshexec.Result, error) {
		res, err := r.bc().Run(ctx, "docker image inspect --format '{{.Id}}' "+imageRef)
		if err == nil && res.ExitCode == 0 && res.Stdout != "" {
			r.digest = firstLine(res.Stdout)
			_ = r.h.Store.SetDeploymentImageDigest(ctx, store.SetDeploymentImageDigestParams{ID: r.d.ID, ImageDigest: &r.digest})
		}
		return res, err
	}); err != nil {
		return "", err
	}
	return imageRef, nil
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

	// --- building ------------------------------------------------------------
	if err := r.setStatus(ctx, store.DeploymentStatusBuilding); err != nil {
		return "", err
	}
	buildEnv, buildArgs, err := r.renderBuildEnv(ctx)
	if err != nil {
		return "", err
	}
	if res, err := r.bc().RunInput(ctx, fmt.Sprintf("umask 077 && cat > %s/env/build.env", appDir), buildEnv); err != nil || res.ExitCode != 0 {
		return "", fmt.Errorf("uploading build.env failed")
	}

	sha12 := sha[:12]
	imageRef := "akerdock/" + appUUID + ":" + sha12
	baseDir := strings.TrimPrefix(r.app.Application.BaseDirectory, "/")
	dockerfile := "./Dockerfile"
	if p := r.app.BuildConfig.DockerfilePath; p != nil && *p != "" {
		dockerfile = "./" + strings.TrimPrefix(*p, "/")
	}

	// The static build pack has no Dockerfile in the repository: one is
	// synthesized next to the sources (§5.2). It is written under a name of
	// our own so it can never collide with — or overwrite — a file the
	// repository already ships.
	if r.app.BuildConfig.BuildPack == store.BuildPackStatic {
		dockerfile = "./" + staticDockerfileName
		if err := r.writeStaticDockerfile(ctx, srcDir, baseDir); err != nil {
			return "", err
		}
	}
	noCache := ""
	if r.d.ForceRebuild {
		noCache = " --no-cache"
	}
	if r.app.BuildConfig.BuildPack == store.BuildPackNixpacks {
		if err := r.buildWithNixpacks(ctx, srcDir, baseDir, appDir, imageRef, labels, sha, noCache); err != nil {
			return "", err
		}
	} else if err := r.streamStep(ctx, "build", func(onOutput func(string)) (*sshexec.Result, error) {
		return r.bc().RunStream(ctx, fmt.Sprintf(
			"cd %s/%s && set -a && . %s/env/build.env && set +a && DOCKER_BUILDKIT=1 docker build --file %s --progress plain%s%s --tag %s %s --label akerdock.commit_sha=%s .",
			srcDir, baseDir, appDir, dockerfile, noCache, buildArgs.Flags(), imageRef, labels, sha), onOutput)
	}); err != nil {
		return "", err
	}
	r.d.ImageName, r.d.ImageTag = ptrStr("akerdock/"+appUUID), &sha12
	_ = r.h.Store.SetDeploymentImage(ctx, store.SetDeploymentImageParams{ID: r.d.ID, ImageName: r.d.ImageName, ImageTag: r.d.ImageTag})

	if err := r.step(ctx, "resolve_digest", func() (*sshexec.Result, error) {
		res, err := r.bc().Run(ctx, "docker image inspect --format '{{.Id}}' "+imageRef)
		if err == nil && res.ExitCode == 0 && res.Stdout != "" {
			r.digest = firstLine(res.Stdout)
			_ = r.h.Store.SetDeploymentImageDigest(ctx, store.SetDeploymentImageDigestParams{ID: r.d.ID, ImageDigest: &r.digest})
		}
		return res, err
	}); err != nil {
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
			inputs.secrets = append(inputs.secrets, v.Key)
		} else {
			inputs.args = append(inputs.args, v.Key)
		}
	}
	return b.String(), inputs, nil
}

// buildInputs splits the build-time variables into what may end up in the image
// and what must never (§5.2).
type buildInputs struct {
	args    []string // plain values: available as ARG, and visible in `docker history`
	secrets []string // BuildKit secrets: mounted at RUN time, never a layer
}

// Flags renders the docker build flags.
//
// The distinction is the whole point. `--build-arg KEY` (no value: BuildKit
// reads it from the environment) makes the value an ARG — and an ARG is written
// into the image metadata, where `docker history` shows it to anyone who can
// pull the image. So a variable marked secret NEVER becomes a build arg: it is
// passed as `--secret id=KEY,env=KEY`, which BuildKit mounts under
// /run/secrets/KEY for the lifetime of a single RUN and leaves out of every
// layer (INV-003).
//
// Neither form puts a value in argv: both name the variable and let BuildKit
// read it from the environment the shell already sourced.
func (in buildInputs) Flags() string {
	var b strings.Builder
	for _, key := range in.args {
		b.WriteString(" --build-arg ")
		b.WriteString(key)
	}
	for _, key := range in.secrets {
		fmt.Fprintf(&b, " --secret id=%s,env=%s", key, key)
	}
	return b.String()
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
	if r.preview != nil {
		ssoURL, ssoErr := r.previewSSOAuthURL(ctx)
		if ssoErr != nil {
			return ssoErr
		}
		content, err = RenderPreviewRoutingFile(r.app, *r.preview, r.d.ID, endpoint, r.previewAuthHash(ctx), ssoURL)
	} else {
		content, err = RenderRoutingFileTo(ctx, r.h.Store, r.app, r.d.ID, endpoint)
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
	applier := &ProxyApplier{Store: r.h.Store, Client: r.client, Server: r.server, Network: r.dest.Network}
	return r.step(ctx, name, func() (*sshexec.Result, error) {
		return nil, applier.Apply(ctx, appUUID, content, expect)
	})
}

// renderHealthCheck maps the configured health check to docker create
// flags (§5.3.4). A Dockerfile HEALTHCHECK stays authoritative: these flags
// only add one when the image has none.
func (r *deploymentRun) renderHealthCheck(ctx context.Context) (flags string, enabled bool) {
	hc, err := r.h.Store.GetHealthCheck(ctx, r.app.Resource.ID)
	if err != nil || !hc.Enabled {
		return "", false // no row means no health check
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
	flags = fmt.Sprintf(" --health-cmd %q --health-interval %ds --health-timeout %ds --health-retries %d --health-start-period %ds",
		cmd, hc.IntervalSeconds, hc.TimeoutSeconds, hc.Retries, hc.StartPeriodSeconds)
	r.healthBudget = int(hc.StartPeriodSeconds) + int(hc.IntervalSeconds+hc.TimeoutSeconds)*int(hc.Retries) + 30
	return flags, true
}

// candidateFailure captures the candidate logs before it is removed by the
// compensation, so the failure is diagnosable (§4 healthchecking).
func (r *deploymentRun) candidateFailure(ctx context.Context, target, reason string) error {
	logs, _ := r.client.Run(ctx, "docker logs --tail 200 "+target+" 2>&1")
	detail := ""
	if logs != nil {
		detail = "\n" + logs.Stdout
	}
	return fmt.Errorf("%s%s", reason, detail)
}

// DockerVolumeName is the deterministic Docker name of a declared volume:
// <resource_uuid>_<name> — the UUID prefix prevents collisions (§8, INV-011).
func DockerVolumeName(resourceUUID, name string) string {
	return resourceUUID + "_" + name
}

// renderStorages returns the docker create mount flags and the idempotent
// preparation command for the application's declared storages (§8). imageRef
// is the image the candidate will run: still-empty volumes are handed to its
// runtime user, so a non-root image can write its own storage without a
// custom Dockerfile.
func (r *deploymentRun) renderStorages(ctx context.Context, appUUID, imageRef string) (mounts, prepare string, err error) {
	storages, err := r.h.Store.ListStoragesForResource(ctx, r.app.Resource.ID)
	if err != nil {
		return "", "", err
	}
	var prep []string
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
			prep = append(prep, fmt.Sprintf(
				"docker volume inspect %s >/dev/null 2>&1 || docker volume create --label akerdock.managed=true --label akerdock.resource_uuid=%s --label akerdock.team_uuid=%s %s >/dev/null",
				vol, appUUID, r.teamUUID, vol))
			mounts += fmt.Sprintf(" -v %s:%s", vol, s.MountPath)
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
			prep = append(prep, "mkdir -p "+*s.HostPath)
			mounts += fmt.Sprintf(" -v %s:%s", *s.HostPath, s.MountPath)
		}
	}
	if len(volumes) > 0 && imageRef != "" {
		prep = append(prep, chownEmptyVolumesScript(imageRef, volumes))
	}
	return mounts, strings.Join(prep, " && "), nil
}

// chownEmptyVolumesScript hands STILL-EMPTY volumes to the image's runtime
// user. A fresh named volume mounted on a path absent from the image belongs
// to root — a USER'd image then crash-loops on its first write, a failure
// every non-root image would hit on every platform that didn't do this.
//
// The fix runs INSIDE a throwaway container of the image itself (--user 0),
// never against /var/lib/docker on the host: a non-root SSH user cannot touch
// the host path, but anyone who can talk to the daemon can do this — and a
// named USER resolves against the image's own /etc/passwd for free, since
// chown executes where that file lives. Only empty volumes are touched: data
// that exists already has an owner, and it is not this function's to change.
// Best-effort by design, hence `|| true` and the trailing `true`: an image
// without /bin/sh (distroless) falls back to today's behavior instead of
// failing the deployment.
func chownEmptyVolumesScript(imageRef string, volumes []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "u=$(docker image inspect --format '{{.Config.User}}' %s 2>/dev/null); ", imageRef)
	b.WriteString("case \"$u\" in ''|root|0|0:*) :;; *) ")
	fmt.Fprintf(&b, "for v in %s; do ", strings.Join(volumes, " "))
	fmt.Fprintf(&b, "docker run --rm --user 0 --entrypoint /bin/sh -v \"$v\":/akerdock-volume %s "+
		"-c \"[ -n \\\"\\$(ls -A /akerdock-volume)\\\" ] || chown -- '$u' /akerdock-volume\" >/dev/null 2>&1 || true; ", imageRef)
	b.WriteString("done;; esac; true")
	return b.String()
}

// renderRuntimeEnv decrypts the application's variables into the
// the shell file sourced before `docker create` (§5.2). Values are
// shell-quoted, so multiline secrets survive intact.
func (r *deploymentRun) renderRuntimeEnv(ctx context.Context) (string, []string, error) {
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
		return "", nil, err
	}
	// Shared variables (§5.4, §3.1): {{scope.KEY}} references resolve inside
	// values (literal variables excepted — that is what literal means), and
	// the server-scoped variables are injected unless the resource overrides
	// the key. Previews keep their strictly dedicated set (INV-010).
	shared := sharedEnv{}
	if r.preview == nil {
		if shared, err = resolveSharedEnv(ctx, r.h.Store, r.h.Keyring, r.app.Resource.ID); err != nil {
			return "", nil, err
		}
	}
	var b strings.Builder
	keys := make([]string, 0, len(vars))
	seen := map[string]bool{}
	for _, v := range vars {
		plaintext, err := r.h.Keyring.Decrypt("environment_variables", "value_enc", pguuid.String(v.Uuid), v.ValueEnc)
		if err != nil {
			return "", nil, fmt.Errorf("decrypt variable %s: %w", v.Key, err)
		}
		value := string(plaintext)
		if !v.IsLiteral {
			value = shared.interpolate(value)
		}
		fmt.Fprintf(&b, "export %s=%s\n", v.Key, shellQuote(value))
		keys = append(keys, v.Key)
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
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(shared.server[k]))
		keys = append(keys, k)
	}
	if r.preview != nil {
		fmt.Fprintf(&b, "export AKERDOCK_PR_ID=%d\n", r.preview.PrID)
		keys = append(keys, "AKERDOCK_PR_ID")
		if r.preview.Fqdn != nil && *r.preview.Fqdn != "" {
			fmt.Fprintf(&b, "export AKERDOCK_FQDN=%s\n", shellQuote(*r.preview.Fqdn))
			fmt.Fprintf(&b, "export AKERDOCK_URL=%s\n", shellQuote("https://"+*r.preview.Fqdn))
			keys = append(keys, "AKERDOCK_FQDN", "AKERDOCK_URL")
		}
	}
	return b.String(), keys, nil
}

// shellQuote renders a value as a single-quoted shell literal. Single quotes
// are literal in POSIX shells — a value can contain newlines, $, backticks,
// backslashes, anything — except a single quote itself, which is closed,
// escaped and reopened. This is what lets a PEM key or a JSON blob survive as
// an environment variable (INV-012).
// registryLogin authenticates the target server against the private registry of
// this application, if it has one, and returns the logout to defer.
//
// The password reaches the server through STDIN, never through argv: a command
// line is world-readable in `ps` on the target host, and it would land in the
// shell history and in any command-level audit (INV-003). `--password-stdin` is
// the only form docker offers that keeps it out.
func (r *deploymentRun) registryLogin(ctx context.Context) (func(), error) {
	credID := r.app.BuildConfig.RegistryCredentialID
	if credID == nil {
		return func() {}, nil
	}
	cred, err := r.h.Store.GetRegistryCredentialByID(ctx, *credID)
	if err != nil {
		return nil, fmt.Errorf("the registry credential of this application is gone: %w", err)
	}
	password, err := r.h.Keyring.Decrypt("registry_credentials", "password_enc",
		pguuid.String(cred.Uuid), cred.PasswordEnc)
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt the registry credential: %w", err)
	}

	if err := r.step(ctx, "registry_login", func() (*sshexec.Result, error) {
		res, err := r.client.RunInput(ctx, fmt.Sprintf("docker login %s -u %s --password-stdin",
			shellQuote(cred.RegistryUrl), shellQuote(cred.Username)), string(password))
		if err == nil && res.ExitCode != 0 {
			// firstLine, never the whole stderr: docker echoes the command it
			// ran on some failures.
			return res, fmt.Errorf("docker login to %s failed: %s", cred.RegistryUrl, firstLine(res.Stderr))
		}
		return res, err
	}); err != nil {
		return nil, err
	}

	return func() {
		// Best effort, and on a context that outlives a cancellation: a logout
		// skipped because the deployment was cancelled would leave the token on
		// the server, which is the one outcome this must not have.
		if _, err := r.client.Run(context.WithoutCancel(ctx), "docker logout "+shellQuote(cred.RegistryUrl)); err != nil {
			r.h.Logger.Warn("docker logout failed — the registry token stays on the server", "error", err)
		}
	}, nil
}

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
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

// recordArtifact registers the deployed image as a rollback candidate,
// protected from the automated cleanup (ADR-006, INV-015). Rollback
// deployments do not create a new artifact: they redeploy an existing one.
func (r *deploymentRun) recordArtifact(ctx context.Context) {
	if r.d.IsRollback || r.d.ImageName == nil {
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

// markFailed applies compensation C2 (remove the candidate, never touch the
// serving container) and records the terminal failed state (§9).
func (r *deploymentRun) markFailed(ctx context.Context, cause error) {
	if r.preview != nil {
		_ = r.h.Store.SetPreviewStatus(ctx, store.SetPreviewStatusParams{ID: r.preview.ID, Status: store.PreviewStatusFailed})
		(&PreviewFeedback{Store: r.h.Store, Keyring: r.h.Keyring, Logger: r.h.Logger}).Notify(ctx, r.app, *r.preview, "failure")
	}
	if r.client != nil {
		candidate := r.namingIdentity() + "-next"
		_, _ = r.client.Run(ctx, "docker rm -f "+candidate+" >/dev/null 2>&1 || true")
	}
	msg := cause.Error()
	_ = r.h.Store.SetDeploymentError(ctx, store.SetDeploymentErrorParams{ID: r.d.ID, ErrorMessage: &msg})
	_ = r.h.Store.SetDeploymentStatus(ctx, store.SetDeploymentStatusParams{ID: r.d.ID, Status: store.DeploymentStatusFailed})
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
	if res, err := r.bc().RunInput(ctx, "cat > "+confPath, nginxConf); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("writing the nginx config failed")
	}

	dockerfile := fmt.Sprintf(`FROM nginx:alpine
COPY .akerdock-nginx.conf /etc/nginx/conf.d/default.conf
COPY %s /usr/share/nginx/html
EXPOSE 80
`, publish)
	path := srcDir + "/" + baseDir + "/" + staticDockerfileName
	res, err := r.bc().RunInput(ctx, "cat > "+path, dockerfile)
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("writing the static Dockerfile failed")
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

	return r.step(ctx, "package_static", func() (*sshexec.Result, error) {
		conf := defaultNginxConfig
		if c := r.app.BuildConfig.CustomNginxConfig; c != nil && *c != "" {
			conf = *c
		}
		confPath := srcDir + "/" + baseDir + "/.akerdock-nginx.conf"
		if res, err := r.bc().RunInput(ctx, "cat > "+confPath, conf); err != nil || res.ExitCode != 0 {
			return res, fmt.Errorf("writing the nginx config failed")
		}
		// Nixpacks builds into /app. The publish directory is validated at the
		// API edge (INV-012) before it is interpolated into a Dockerfile that a
		// remote shell then builds.
		dockerfile := fmt.Sprintf(`FROM %s AS build
FROM nginx:alpine
COPY .akerdock-nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/%s /usr/share/nginx/html
EXPOSE 80
`, buildRef, publish)
		path := srcDir + "/" + baseDir + "/" + staticDockerfileName
		if res, err := r.bc().RunInput(ctx, "cat > "+path, dockerfile); err != nil || res.ExitCode != 0 {
			return res, fmt.Errorf("writing the static Dockerfile failed")
		}
		res, err := r.bc().Run(ctx, fmt.Sprintf(
			"cd %s/%s && DOCKER_BUILDKIT=1 docker build --file %s --progress plain --tag %s %s --label akerdock.commit_sha=%s . && docker rmi %s >/dev/null 2>&1 || true",
			srcDir, baseDir, staticDockerfileName, imageRef, labels, sha, buildRef))
		if err == nil && res.ExitCode != 0 {
			return res, fmt.Errorf("packaging the built assets into nginx failed: %s", firstLine(res.Stderr))
		}
		return res, err
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
	// Prefix, not equality: the compose hooks are per service
	// ("pre_deployment_<svc>") and share the same §10 rule — no existing
	// container means nothing to run in, not a failure.
	if strings.HasPrefix(name, "pre_deployment") {
		res, err := r.client.Run(ctx, "docker inspect --format '{{.State.Running}}' "+container+" 2>/dev/null || true")
		if err != nil {
			return err
		}
		if !strings.Contains(res.Stdout, "true") {
			r.skipStep(ctx, name, "skipped: no running container")
			return nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	return r.step(ctx, name, func() (*sshexec.Result, error) {
		return r.client.Run(ctx, fmt.Sprintf("docker exec %s sh -c %s", container, shellQuote(*command)))
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
	res, err := r.client.Run(ctx, fmt.Sprintf(
		"echo old=$(docker inspect --format '{{.State.Status}}' %s 2>/dev/null || echo absent); "+
			"echo next=$(docker inspect --format '{{.State.Status}}' %s 2>/dev/null || echo absent)",
		oldName, candidate))
	if err != nil {
		return false, err
	}
	oldState := inspectField(res.Stdout, "old=")
	nextState := inspectField(res.Stdout, "next=")

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
			return r.client.Run(ctx, fmt.Sprintf("docker rename %s %s", candidate, appUUID))
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
			return r.client.Run(ctx, fmt.Sprintf(
				"(docker stop -t %d %s >/dev/null 2>&1 && docker rm %s >/dev/null 2>&1) || true; docker rename %s %s",
				grace, oldName, oldName, candidate, appUUID))
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

// inspectField reads `key=value` out of the inspection output.
func inspectField(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, key); ok {
			return strings.TrimSpace(after)
		}
	}
	return "absent"
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

	// The working directory exists on the deployment server because `prepare`
	// created it there. The build machine has never seen this application, and
	// the very first thing the build does is write build.env into it.
	appDir := "/var/lib/akerdock/applications/" + pguuid.String(r.app.Resource.Uuid)
	if res, err := client.Run(ctx, fmt.Sprintf("mkdir -p %s/env && chmod 700 %s %s/env", appDir, appDir, appDir)); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("cannot prepare the working directory on the build server")
	}
	_ = r.h.Store.SetDeploymentBuildServer(ctx, store.SetDeploymentBuildServerParams{ID: r.d.ID, BuildServerID: &pick.ID})
	r.h.Logger.Info("building on a build server", "server", pick.Name, "deployment", pguuid.String(r.d.Uuid))
	return nil
}

// pushBuiltImage tags the image for the push registry, pushes it from the build
// server, and pulls it back BY DIGEST on the deployment server. It returns the
// reference the container will actually run.
//
// Both machines log in and log out around their own operation: leaving either
// authenticated would leave a registry token in ~/.docker/config.json for
// anything else on that host to use (INV-003).
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

	local := *r.d.ImageName + ":" + *r.d.ImageTag
	remote := cred.RegistryUrl + "/" + *r.d.ImageName + ":" + *r.d.ImageTag

	if err := r.step(ctx, "push", func() (*sshexec.Result, error) {
		if err := dockerLogin(ctx, r.bc(), cred.RegistryUrl, cred.Username, string(password)); err != nil {
			return nil, err
		}
		defer dockerLogout(ctx, r.bc(), cred.RegistryUrl, r.h.Logger)

		res, err := r.bc().Run(ctx, fmt.Sprintf("docker tag %s %s && docker push %s",
			shellQuote(local), shellQuote(remote), shellQuote(remote)))
		if err != nil {
			return res, err
		}
		if res.ExitCode != 0 {
			return res, fmt.Errorf("pushing %s failed: %s", remote, firstLine(res.Stderr))
		}
		return res, nil
	}); err != nil {
		return "", err
	}

	// The digest is read on the build server, right after the push: it is the
	// identity of what was pushed, and it is what the target is told to pull.
	// Pulling by tag would let a racing push swap the image under us.
	digest := ""
	if err := r.step(ctx, "resolve_pushed_digest", func() (*sshexec.Result, error) {
		res, err := r.bc().Run(ctx, "docker image inspect --format '{{index .RepoDigests 0}}' "+shellQuote(remote))
		if err == nil && res.ExitCode == 0 {
			digest = firstLine(res.Stdout)
		}
		if digest == "" {
			return res, fmt.Errorf("the registry returned no digest for the pushed image")
		}
		return res, err
	}); err != nil {
		return "", err
	}

	if err := r.step(ctx, "pull_from_registry", func() (*sshexec.Result, error) {
		if err := dockerLogin(ctx, r.client, cred.RegistryUrl, cred.Username, string(password)); err != nil {
			return nil, err
		}
		defer dockerLogout(ctx, r.client, cred.RegistryUrl, r.h.Logger)

		res, err := r.client.Run(ctx, "docker pull "+shellQuote(digest))
		if err != nil {
			return res, err
		}
		if res.ExitCode != 0 {
			return res, fmt.Errorf("the deployment server cannot pull %s: %s", digest, firstLine(res.Stderr))
		}
		return res, nil
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

// dockerLogin authenticates a machine against a registry. The password goes in
// through STDIN: a command line is readable by any `ps` on that host (INV-003).
func dockerLogin(ctx context.Context, c *sshexec.Client, registry, user, password string) error {
	res, err := c.RunInput(ctx, fmt.Sprintf("docker login %s -u %s --password-stdin",
		shellQuote(registry), shellQuote(user)), password)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker login to %s failed: %s", registry, firstLine(res.Stderr))
	}
	return nil
}

// dockerLogout runs on a context that outlives a cancellation: a logout skipped
// because the deployment was cancelled would leave the token on the machine.
func dockerLogout(ctx context.Context, c *sshexec.Client, registry string, logger *slog.Logger) {
	if _, err := c.Run(context.WithoutCancel(ctx), "docker logout "+shellQuote(registry)); err != nil {
		logger.Warn("docker logout failed — the registry token stays on the server", "error", err)
	}
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
