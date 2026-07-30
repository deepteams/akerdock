package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
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
// construction, not by filter discipline.
type ServerCleanup struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Audit   *audit.Recorder
	Logger  *slog.Logger
	dial    cleanupDialFunc
}

type cleanupRemote interface {
	Run(context.Context, string) (*sshexec.Result, error)
	Close() error
}

type cleanupDialFunc func(context.Context, string, int, string, string, time.Duration, string) (cleanupRemote, error)

type cleanupPrune struct {
	name string
	cmd  string
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

	key, err := h.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return nil, err
	}
	pem, err := h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return nil, err
	}
	dial := h.dial
	if dial == nil {
		dial = func(ctx context.Context, host string, port int, user, privateKey string, timeout time.Duration, hostKey string) (cleanupRemote, error) {
			return sshexec.Dial(ctx, host, port, user, privateKey, timeout, hostKey)
		}
	}
	client, err := dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
	if err != nil {
		rec.Start(ctx, "ssh_connect")
		rec.Fail(ctx, "SSH connection failed")
		h.publish(ctx, server, "server.cleanup.failed.v1", map[string]any{"reason": "ssh connection failed"})
		return nil, err
	}
	defer func() { _ = client.Close() }()

	rec.Start(ctx, "measure_before")
	before, err := h.diskUsagePct(ctx, client)
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

	for _, p := range serverCleanupPrunes(server) {
		if err := func() error {
			rec.Start(ctx, p.name)
			res, err := client.Run(ctx, p.cmd)
			if err != nil {
				rec.Fail(ctx, err.Error())
				return err
			}
			if res.ExitCode != 0 {
				rec.Fail(ctx, firstLine(res.Stderr))
				return fmt.Errorf("%s failed: %s", p.name, firstLine(res.Stderr))
			}
			rec.Succeed(ctx, firstLine(reclaimedLine(res.Stdout)))
			return nil
		}(); err != nil {
			h.publish(ctx, server, "server.cleanup.failed.v1", map[string]any{
				"reason": firstLine(err.Error()), "step": p.name,
			})
			return nil, err
		}
	}

	rec.Start(ctx, "measure_after")
	after, err := h.diskUsagePct(ctx, client)
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

// serverCleanupPrunes is kept separate from Execute so the destructive
// boundary is unit-testable as a complete inventory.
func serverCleanupPrunes(server store.Server) []cleanupPrune {
	prunes := []cleanupPrune{
		// Build cache is reconstructible. -a includes every unused cache record;
		// --keep-storage leaves a useful warm 2 GiB floor.
		{"prune_build_cache", "docker builder prune -af --keep-storage 2GB"},
		// Locally built AkerDock images carry this label. Positive filtering is
		// what keeps foreign dangling images out of reach.
		{"prune_dangling_images", "docker image prune -f --filter label=akerdock.managed=true"},
		// With no active deployment, every managed exact `-next` name is an
		// orphan regardless of whether Docker calls it created, running,
		// restarting, exited, paused or dead. -v also reclaims only the
		// candidate's attached anonymous volumes; named volumes stay intact.
		{
			"prune_dead_candidates",
			cleanupCandidatesCommand(),
		},
		// find includes dotfiles. Its exit status is preserved, so permission
		// failures fail the job instead of being hidden behind `echo done`.
		{
			"purge_tmp",
			cleanupTmpCommand("/var/lib/akerdock/tmp"),
		},
	}
	if server.CleanupPruneVolumes {
		// Positive ownership filtering preserves foreign anonymous volumes.
		// Named managed volumes are not selected without --all.
		prunes = append(prunes, cleanupPrune{
			"prune_anonymous_volumes",
			"docker volume prune -f --filter label=akerdock.managed=true",
		})
	}
	if server.CleanupPruneNetworks {
		prunes = append(prunes, cleanupPrune{
			"prune_managed_networks",
			"docker network prune -f --filter label=akerdock.managed=true",
		})
	}
	return prunes
}

func cleanupCandidatesCommand() string {
	return `set -e; ids=$(docker ps -aq --filter label=akerdock.managed=true); for id in $ids; do meta=$(docker inspect --format '{{.Name}}|{{with index .Config.Labels "akerdock.resource_uuid"}}{{.}}{{end}}|{{with index .Config.Labels "akerdock.preview_uuid"}}{{.}}{{end}}|{{with index .Config.Labels "akerdock.component"}}{{.}}{{end}}' "$id"); name=${meta%%|*}; meta=${meta#*|}; resource=${meta%%|*}; meta=${meta#*|}; preview=${meta%%|*}; component=${meta#*|}; base=${preview:-$resource}; candidate="/$base-next"; if [ -n "$component" ]; then candidate="/$base-$component-next"; fi; if [ -n "$base" ] && [ "$name" = "$candidate" ]; then docker rm -fv "$id"; fi; done`
}

func cleanupTmpCommand(dir string) string {
	return `set -e; dir=` + shellQuote(dir) + `; if [ -d "$dir" ]; then find "$dir" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; fi`
}

// diskUsagePct measures Docker's actual data filesystem. Docker Root Dir may
// live on a dedicated mount, so using /var/lib/akerdock can silently watch the
// wrong disk. Measurement is required: a threshold job must never turn an
// unreadable value into a successful "below threshold" result.
func (h *ServerCleanup) diskUsagePct(ctx context.Context, client cleanupRemote) (int, error) {
	res, err := client.Run(ctx, `root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null) && [ -n "$root" ] && df -P "$root" | awk 'NR==2 {gsub(/%/,"",$5); print $5}'`)
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

// reclaimedLine keeps the docker prune summary ("Total reclaimed space: …")
// when present — it is the number the operator wants in the step.
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
