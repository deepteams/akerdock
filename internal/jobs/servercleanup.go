package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/go-units"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/serverdial"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeServerCleanup is the automated Docker cleanup of one server (§3.7).
const TypeServerCleanup = "server.cleanup"

// ServerCleanupPayload names the server and why the cleanup fired.
type ServerCleanupPayload struct {
	ServerID int64 `json:"server_id"`
	// Reason is cron | threshold | manual. A threshold run first measures the
	// disk and exits without pruning when usage is below the server's limit.
	Reason string `json:"reason"`
}

// ServerCleanup prunes what is safe and MANAGED on a server (§3.7, INV-015):
// build cache, dangling images, dead deployment candidates, the akerdock tmp
// directory, plus the server's opt-in prunes (anonymous volumes, managed
// networks). It NEVER touches an unmanaged or persistent object: named
// volumes — adopted ones included (§20.7) — tagged images (the rollback
// artifacts, ADR-006) and foreign containers are out of reach by
// construction, not by filter discipline. Docker objects go through the agent
// channel (ADR-052); the build cache, the tmp purge and the df measurement
// are host-side and stay on SSH.
type ServerCleanup struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Audit   *audit.Recorder
	Docker  dockerruntime.Source
	Logger  *slog.Logger
	dial    cleanupDialFunc
}

type cleanupRemote interface {
	Run(context.Context, string) (*sshexec.Result, error)
	Close() error
}

type cleanupDialFunc func(context.Context, string, int, string, string, time.Duration, string) (cleanupRemote, error)

// cleanupStep is one prune: run returns the operator-facing summary line.
type cleanupStep struct {
	name string
	run  func(ctx context.Context) (string, error)
}

// serverCleanupDeferDelay is short enough for a manual cleanup to remain
// useful after a deployment, while avoiding a tight durable-job loop.
var serverCleanupDeferDelay = time.Minute

// Execute runs one cleanup attempt. Idempotent: every prune tolerates
// nothing-to-do.
func (h *ServerCleanup) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload ServerCleanupPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	switch payload.Reason {
	case "cron", "threshold", "manual":
	default:
		return nil, fmt.Errorf("invalid payload: unknown cleanup reason %q", payload.Reason)
	}
	server, err := h.Store.GetServerByID(ctx, payload.ServerID)
	if err != nil {
		return nil, fmt.Errorf("server vanished: %w", err)
	}

	// Never during a deployment (§3.7). This check locks the server row and is
	// atomic with the queued deployment's first transition: one side wins,
	// while the other waits or creates a durable deferred cleanup.
	canStart, err := h.Store.CanStartServerCleanup(ctx, server.ID)
	if err != nil {
		return nil, err
	}
	if !canStart {
		rec.Start(ctx, "guard")
		next, err := h.deferCleanup(ctx, job, server, payload)
		if err != nil {
			rec.Fail(ctx, "could not schedule the deferred cleanup: "+firstLine(err.Error()))
			return nil, err
		}
		rec.Succeed(ctx, "deferred: a deployment is running; retry scheduled for "+next.RunAt.Time.UTC().Format(time.RFC3339))
		return map[string]any{
			"status": "deferred", "reason": "deployment_in_progress",
			"retry_job_uuid": pguuid.String(next.Uuid),
		}, nil
	}

	rt, err := h.Docker.Runtime(ctx, server.ID)
	if err != nil {
		rec.Start(ctx, "agent_channel")
		rec.Fail(ctx, "the server's agent is not connected")
		h.publish(ctx, server, "server.cleanup.failed.v1", map[string]any{"reason": "agent not connected"})
		return nil, err
	}

	pem, err := serverdial.Key(ctx, h.Store, h.Keyring, server)
	if err != nil {
		return nil, err
	}
	dial := h.dial
	if dial == nil {
		dial = func(ctx context.Context, host string, port int, user, privateKey string, timeout time.Duration, hostKey string) (cleanupRemote, error) {
			c, err := sshexec.Dial(ctx, host, port, user, privateKey, timeout, hostKey)
			if err != nil {
				return nil, err
			}
			// The seam signature carries scalars only; the sudo policy
			// (ADR-076) rides the captured server row.
			if server.UseSudo {
				c.EnableSudo()
			}
			return c, nil
		}
	}
	client, err := dial(ctx, server.Host, int(server.Port), server.SshUser, pem,
		time.Duration(server.SshTimeoutSeconds)*time.Second, serverdial.HostKey(server))
	if err != nil {
		rec.Start(ctx, "ssh_connect")
		rec.Fail(ctx, "SSH connection failed")
		h.publish(ctx, server, "server.cleanup.failed.v1", map[string]any{"reason": "ssh connection failed"})
		return nil, err
	}
	defer func() { _ = client.Close() }()

	rec.Start(ctx, "measure_before")
	before, err := h.diskUsagePct(ctx, rt, client)
	if err != nil {
		rec.Fail(ctx, firstLine(err.Error()))
		h.publish(ctx, server, "server.cleanup.failed.v1", map[string]any{
			"reason": firstLine(err.Error()), "step": "measure_before",
		})
		return nil, err
	}
	if payload.Reason == "threshold" {
		// The threshold pass measures first: below the limit there is nothing
		// to reclaim and the pruning is skipped entirely.
		threshold := 0
		if server.CleanupDiskThresholdPct != nil {
			threshold = int(*server.CleanupDiskThresholdPct)
		}
		if threshold == 0 || before < threshold {
			rec.Succeed(ctx, fmt.Sprintf("Docker disk at %d%%, threshold %d%% — nothing to do", before, threshold))
			return map[string]any{"status": "skipped", "disk_pct": before, "threshold_pct": threshold}, nil
		}
		rec.Succeed(ctx, fmt.Sprintf("Docker disk at %d%% reached threshold %d%%", before, threshold))
	} else {
		rec.Succeed(ctx, fmt.Sprintf("Docker disk at %d%%", before))
	}

	for _, step := range h.cleanupSteps(server, rt, client) {
		if err := func() error {
			rec.Start(ctx, step.name)
			summary, err := step.run(ctx)
			if err != nil {
				rec.Fail(ctx, firstLine(err.Error()))
				return fmt.Errorf("%s failed: %s", step.name, firstLine(err.Error()))
			}
			rec.Succeed(ctx, summary)
			return nil
		}(); err != nil {
			h.publish(ctx, server, "server.cleanup.failed.v1", map[string]any{
				"reason": firstLine(err.Error()), "step": step.name,
			})
			return nil, err
		}
	}

	rec.Start(ctx, "measure_after")
	after, err := h.diskUsagePct(ctx, rt, client)
	if err != nil {
		rec.Fail(ctx, firstLine(err.Error()))
		h.publish(ctx, server, "server.cleanup.failed.v1", map[string]any{
			"reason": firstLine(err.Error()), "step": "measure_after",
		})
		return nil, err
	}
	rec.Succeed(ctx, fmt.Sprintf("Docker disk at %d%%", after))

	if err := h.Store.SetServerCleanupSchedule(ctx, store.SetServerCleanupScheduleParams{
		ID: server.ID, LastRunAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		h.publish(ctx, server, "server.cleanup.failed.v1", map[string]any{
			"reason": firstLine(err.Error()), "step": "record_completion",
		})
		return nil, fmt.Errorf("record cleanup completion: %w", err)
	}
	h.publish(ctx, server, "server.cleanup.completed.v1", map[string]any{
		"disk_pct_before": before, "disk_pct_after": after, "reason": payload.Reason,
	})
	h.Logger.Info("server cleanup completed", "server", pguuid.String(server.Uuid),
		"before_pct", before, "after_pct", after, "reason", payload.Reason)
	return map[string]any{
		"status": "completed", "disk_pct_before": before, "disk_pct_after": after,
	}, nil
}

func (h *ServerCleanup) deferCleanup(ctx context.Context, job store.Job, server store.Server, payload ServerCleanupPayload) (store.Job, error) {
	lockKey := job.LockKey
	if lockKey == nil || *lockKey == "" {
		key := "server:cleanup:" + pguuid.String(server.Uuid)
		lockKey = &key
	}
	teamID := job.TeamID
	if teamID == nil {
		teamID = &server.TeamID
	}
	idempotencyKey := fmt.Sprintf("server-cleanup-deferred:%d", job.ID)
	retryOfID := job.ID
	return queue.Enqueue(ctx, h.Store, queue.EnqueueOptions{
		Queue: "cleanup", Type: TypeServerCleanup, Payload: payload,
		RunAt: time.Now().Add(serverCleanupDeferDelay), MaxAttempts: job.MaxAttempts,
		LockKey: lockKey, TeamID: teamID, RetryOfID: &retryOfID,
		IdempotencyKey: &idempotencyKey,
	})
}

// managedLabelFilter scopes a prune to AkerDock's own objects — the positive
// filtering that keeps foreign resources out of reach.
var managedLabelFilter = filters.NewArgs(filters.Arg("label", "akerdock.managed=true"))

// cleanupSteps is kept separate from Execute so the destructive boundary is
// unit-testable as a complete inventory. The build cache and the tmp purge
// are host-side (the CLI build path, ADR-051) and run over SSH; everything
// else is a typed call on the agent channel.
func (h *ServerCleanup) cleanupSteps(server store.Server, rt dockerruntime.Runtime, client cleanupRemote) []cleanupStep {
	sshStep := func(cmd string) func(context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			res, err := client.Run(ctx, cmd)
			if err != nil {
				return "", err
			}
			if res.ExitCode != 0 {
				return "", fmt.Errorf("%s", firstLine(res.Stderr))
			}
			return firstLine(reclaimedLine(res.Stdout)), nil
		}
	}
	steps := []cleanupStep{
		// Build cache is reconstructible. -a includes every unused cache record;
		// --keep-storage leaves a useful warm 2 GiB floor. The cache belongs to
		// the CLI build path, so its prune stays a CLI command.
		{"prune_build_cache", sshStep("docker builder prune -af --keep-storage 2GB")},
		// Locally built AkerDock images carry the managed label. Positive
		// filtering is what keeps foreign dangling images out of reach; the
		// dangling filter is what `docker image prune` (without -a) implies.
		{"prune_dangling_images", func(ctx context.Context) (string, error) {
			report, err := rt.ImagesPrune(ctx, filters.NewArgs(
				filters.Arg("dangling", "true"),
				filters.Arg("label", "akerdock.managed=true"),
			))
			if err != nil {
				return "", err
			}
			return reclaimedSummary(report.SpaceReclaimed), nil
		}},
		// With no active deployment, every managed exact `-next` name is an
		// orphan regardless of its state. RemoveVolumes reclaims only the
		// candidate's attached anonymous volumes; named volumes stay intact.
		{"prune_dead_candidates", func(ctx context.Context) (string, error) {
			return pruneDeadCandidates(ctx, rt)
		}},
		// Images of DESTROYED previews are tagged, so the dangling prune never
		// reaches them — a destroy that failed partway leaks them forever.
		// They are artifacts of a closed PR (ADR-006: none survive), so this
		// runs unconditionally; anything a container still uses is kept.
		{"prune_destroyed_preview_images", func(ctx context.Context) (string, error) {
			return pruneDestroyedPreviewImages(ctx, rt, h.Store)
		}},
		// find includes dotfiles. Its exit status is preserved, so permission
		// failures fail the job instead of being hidden behind `echo done`.
		{"purge_tmp", sshStep(cleanupTmpCommand("/var/lib/akerdock/tmp"))},
	}
	if server.CleanupPruneVolumes {
		// Positive ownership filtering preserves foreign anonymous volumes.
		// Named managed volumes are not selected without the `all` filter.
		steps = append(steps, cleanupStep{"prune_anonymous_volumes", func(ctx context.Context) (string, error) {
			report, err := rt.VolumesPrune(ctx, managedLabelFilter)
			if err != nil {
				return "", err
			}
			return reclaimedSummary(report.SpaceReclaimed), nil
		}})
		// NAMED volumes of DESTROYED previews: preview data is disposable by
		// definition (§20.4.1) — INV-008 protects production volumes, which
		// never carry a preview label. Same opt-in as the volume prune: it is
		// still a data-deletion switch.
		steps = append(steps, cleanupStep{"prune_destroyed_preview_volumes", func(ctx context.Context) (string, error) {
			return pruneDestroyedPreviewVolumes(ctx, rt, h.Store)
		}})
	}
	if server.CleanupPruneNetworks {
		steps = append(steps, cleanupStep{"prune_managed_networks", func(ctx context.Context) (string, error) {
			return pruneOrphanManagedNetworks(ctx, rt, h.Store)
		}})
	}
	return steps
}

// ownerStore answers "does this object's owner still exist?" — the two
// liveness lookups every orphan prune shares.
type ownerStore interface {
	ListLiveResourceUUIDs(ctx context.Context, uuids []pgtype.UUID) ([]pgtype.UUID, error)
	ListLivePreviewUUIDs(ctx context.Context, uuids []pgtype.UUID) ([]pgtype.UUID, error)
}

// liveOwners resolves which of the labelled owner uuids are still alive — a
// live resource row, or a preview that is not destroyed. Sleeping counts as
// alive everywhere: scale-to-zero is a state, not an abandonment.
func liveOwners(ctx context.Context, q ownerStore, owners map[string]bool) (map[string]bool, error) {
	var uuids []pgtype.UUID
	for owner := range owners {
		if p := pguuid.MustParse(owner); p.Valid {
			uuids = append(uuids, p)
		}
	}
	live := map[string]bool{}
	if len(uuids) == 0 {
		return live, nil
	}
	rows, err := q.ListLiveResourceUUIDs(ctx, uuids)
	if err != nil {
		return nil, err
	}
	for _, u := range rows {
		live[pguuid.String(u)] = true
	}
	rows, err = q.ListLivePreviewUUIDs(ctx, uuids)
	if err != nil {
		return nil, err
	}
	for _, u := range rows {
		live[pguuid.String(u)] = true
	}
	return live, nil
}

// previewLabelFilter selects objects carrying ANY preview ownership label.
var previewLabelFilter = filters.NewArgs(filters.Arg("label", "akerdock.preview_uuid"))

// pruneDestroyedPreviewImages removes images whose owning PREVIEW is
// destroyed. Production images never carry the preview label, so rollback
// retention (ADR-006) is out of reach by construction; a refusal (an image a
// container still references) keeps the image and never fails the cleanup.
func pruneDestroyedPreviewImages(ctx context.Context, rt dockerruntime.Runtime, q ownerStore) (string, error) {
	list, err := rt.ImageList(ctx, image.ListOptions{Filters: previewLabelFilter})
	if err != nil {
		return "", err
	}
	owners := map[string]bool{}
	for _, img := range list {
		if u := img.Labels["akerdock.preview_uuid"]; u != "" {
			owners[u] = true
		}
	}
	if len(owners) == 0 {
		return "0 images removed", nil
	}
	live, err := liveOwners(ctx, q, owners)
	if err != nil {
		return "", err
	}
	removed, kept := 0, 0
	for _, img := range list {
		if live[img.Labels["akerdock.preview_uuid"]] {
			continue
		}
		if _, err := rt.ImageRemove(ctx, img.ID, image.RemoveOptions{Force: true}); err != nil {
			if dockerruntime.IsNotFound(err) {
				continue
			}
			kept++
			continue
		}
		removed++
	}
	if kept > 0 {
		return fmt.Sprintf("%d images removed, %d kept (still in use)", removed, kept), nil
	}
	return fmt.Sprintf("%d images removed", removed), nil
}

// pruneDestroyedPreviewVolumes removes NAMED volumes whose owning preview is
// destroyed — the leak a partially-failed destroy leaves behind. Preview
// data is disposable by definition (§20.4.1); production volumes never carry
// the preview label (INV-008 stays intact by construction).
func pruneDestroyedPreviewVolumes(ctx context.Context, rt dockerruntime.Runtime, q ownerStore) (string, error) {
	list, err := rt.VolumeList(ctx, volume.ListOptions{Filters: previewLabelFilter})
	if err != nil {
		return "", err
	}
	owners := map[string]bool{}
	for _, v := range list.Volumes {
		if v == nil {
			continue
		}
		if u := v.Labels["akerdock.preview_uuid"]; u != "" {
			owners[u] = true
		}
	}
	if len(owners) == 0 {
		return "0 volumes removed", nil
	}
	live, err := liveOwners(ctx, q, owners)
	if err != nil {
		return "", err
	}
	removed, kept := 0, 0
	for _, v := range list.Volumes {
		if v == nil || live[v.Labels["akerdock.preview_uuid"]] {
			continue
		}
		if err := rt.VolumeRemove(ctx, v.Name, true); err != nil {
			if dockerruntime.IsNotFound(err) {
				continue
			}
			kept++
			continue
		}
		removed++
	}
	if kept > 0 {
		return fmt.Sprintf("%d volumes removed, %d kept (still in use)", removed, kept), nil
	}
	return fmt.Sprintf("%d volumes removed", removed), nil
}

// pruneOrphanManagedNetworks removes managed networks whose OWNER is gone —
// never networks that merely look unused. Docker's own `network prune`
// considers a network with only STOPPED containers unused (endpoints exist
// only while running), so a blanket prune deleted the stack network of every
// sleeping scale-to-zero resource and the next wake died on "network not
// found". Ownership is the resource/preview uuid label; a managed network
// with NO owner label (a destination network) is never touched here.
func pruneOrphanManagedNetworks(ctx context.Context, rt dockerruntime.Runtime, q ownerStore) (string, error) {
	list, err := rt.NetworkList(ctx, network.ListOptions{Filters: managedLabelFilter})
	if err != nil {
		return "", err
	}
	var candidates []network.Summary
	seen := map[string]bool{}
	ownerOf := func(labels map[string]string) string {
		if u := labels["akerdock.preview_uuid"]; u != "" {
			return u
		}
		return labels["akerdock.resource_uuid"]
	}
	for _, n := range list {
		owner := ownerOf(n.Labels)
		if owner == "" {
			continue // a destination network: owned by a row, not a uuid label
		}
		candidates = append(candidates, n)
		seen[owner] = true
	}
	if len(candidates) == 0 {
		return "0 networks removed", nil
	}
	live, err := liveOwners(ctx, q, seen)
	if err != nil {
		return "", err
	}
	removed, kept := 0, 0
	for _, n := range candidates {
		if live[ownerOf(n.Labels)] {
			continue // sleeping is not orphaned — the wake needs this network
		}
		if err := rt.NetworkRemove(ctx, n.ID); err != nil {
			if dockerruntime.IsNotFound(err) {
				continue
			}
			// Best-effort per object: "has active endpoints" arrives as a
			// Forbidden (a class the wire does not single out), and a network
			// something still runs on is not an orphan whatever the daemon
			// calls the refusal. Kept and counted, never a failed cleanup.
			kept++
			continue
		}
		removed++
	}
	if kept > 0 {
		return fmt.Sprintf("%d networks removed, %d kept (still in use)", removed, kept), nil
	}
	return fmt.Sprintf("%d networks removed", removed), nil
}

// pruneDeadCandidates removes every managed container whose name is exactly
// its resource's `-next` candidate: with no deployment running (the §3.7
// guard), each one is an orphan of an interrupted deploy, whatever state
// Docker reports for it.
func pruneDeadCandidates(ctx context.Context, rt dockerruntime.Runtime) (string, error) {
	list, err := rt.ContainerList(ctx, container.ListOptions{All: true, Filters: managedLabelFilter})
	if err != nil {
		return "", err
	}
	removed := 0
	for _, c := range list {
		name := containerName(c)
		base := c.Labels["akerdock.preview_uuid"]
		if base == "" {
			base = c.Labels["akerdock.resource_uuid"]
		}
		if base == "" || name == "" {
			continue
		}
		candidate := base + "-next"
		if component := c.Labels["akerdock.component"]; component != "" {
			candidate = base + "-" + component + "-next"
		}
		if name != candidate {
			continue
		}
		if err := removeNamedContainers(ctx, rt, true, c.ID); err != nil {
			return "", err
		}
		removed++
	}
	return fmt.Sprintf("%d dead candidates removed", removed), nil
}

func cleanupTmpCommand(dir string) string {
	return `set -e; dir=` + shellQuote(dir) + `; if [ -d "$dir" ]; then find "$dir" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; fi`
}

// diskUsagePct measures Docker's actual data filesystem: the root comes from
// the daemon (over the channel), the filesystem occupancy from the host's df.
// Docker Root Dir may live on a dedicated mount, so using /var/lib/akerdock
// can silently watch the wrong disk. Measurement is required: a threshold job
// must never turn an unreadable value into a successful "below threshold"
// result.
func (h *ServerCleanup) diskUsagePct(ctx context.Context, rt dockerruntime.Runtime, client cleanupRemote) (int, error) {
	info, err := rt.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("measure Docker disk usage: %w", err)
	}
	if info.DockerRootDir == "" {
		return 0, fmt.Errorf("measure Docker disk usage: the daemon reported no root dir")
	}
	res, err := client.Run(ctx, "df -P "+shellQuote(info.DockerRootDir)+` | awk 'NR==2 {gsub(/%/,"",$5); print $5}'`)
	if err != nil {
		return 0, fmt.Errorf("measure Docker disk usage: %w", err)
	}
	if res == nil || res.ExitCode != 0 {
		detail := ""
		if res != nil {
			detail = firstLine(res.Stderr)
		}
		return 0, fmt.Errorf("measure Docker disk usage failed: %s", detail)
	}
	n, err := strconv.Atoi(strings.TrimSpace(firstLine(res.Stdout)))
	if err != nil || n < 0 || n > 100 {
		return 0, fmt.Errorf("measure Docker disk usage returned %q", firstLine(res.Stdout))
	}
	return n, nil
}

// reclaimedSummary renders a prune report the way the CLI did — it is the
// number the operator wants in the step.
func reclaimedSummary(bytes uint64) string {
	return "Total reclaimed space: " + units.HumanSize(float64(bytes))
}

// reclaimedLine keeps the docker prune summary ("Total reclaimed space: …")
// when present — for the host-side prunes that still run the CLI.
func reclaimedLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Total reclaimed space:") {
			return line
		}
	}
	if strings.TrimSpace(out) == "" {
		return "done"
	}
	return out
}

func (h *ServerCleanup) publish(ctx context.Context, server store.Server, event string, payload map[string]any) {
	if h.Audit == nil {
		return
	}
	var teamUUID pgtype.UUID
	if team, err := h.Store.GetTeamByID(ctx, server.TeamID); err == nil {
		teamUUID = team.Uuid
	}
	payload["server_uuid"] = pguuid.String(server.Uuid)
	h.Audit.Outbox(ctx, h.Store, event, teamUUID, server.Uuid,
		"server:"+pguuid.String(server.Uuid), payload)
}
