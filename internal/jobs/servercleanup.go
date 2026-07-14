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
}

// Execute runs one cleanup attempt. Idempotent: every prune tolerates
// nothing-to-do.
func (h *ServerCleanup) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload ServerCleanupPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	server, err := h.Store.GetServerByID(ctx, payload.ServerID)
	if err != nil {
		return nil, fmt.Errorf("server vanished: %w", err)
	}

	// Never during a deployment (§3.7): a prune racing a build could remove
	// the layers the build is about to use. Deferred, not failed — the next
	// cron or threshold pass retries.
	if active, err := h.Store.CountActiveDeploymentsOnServer(ctx, server.ID); err != nil {
		return nil, err
	} else if active > 0 {
		rec.Start(ctx, "guard")
		rec.Succeed(ctx, "deferred: a deployment is running on this server")
		return map[string]any{"status": "deferred", "reason": "deployment_in_progress"}, nil
	}

	key, err := h.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return nil, err
	}
	pem, err := h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return nil, err
	}
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
	if err != nil {
		rec.Fail(ctx, "SSH connection failed")
		h.publish(ctx, server, "server.cleanup.failed.v1", map[string]any{"reason": "ssh connection failed"})
		return nil, err
	}
	defer func() { _ = client.Close() }()

	before := h.diskUsagePct(ctx, client)
	if payload.Reason == "threshold" {
		// The threshold pass measures first: below the limit there is nothing
		// to reclaim and the pruning is skipped entirely.
		threshold := 0
		if server.CleanupDiskThresholdPct != nil {
			threshold = int(*server.CleanupDiskThresholdPct)
		}
		if threshold == 0 || before < threshold {
			rec.Start(ctx, "measure")
			rec.Succeed(ctx, fmt.Sprintf("disk at %d%%, threshold %d%% — nothing to do", before, threshold))
			return map[string]any{"status": "skipped", "disk_pct": before, "threshold_pct": threshold}, nil
		}
	}

	// The safe prunes (§3.7, runbook stuck-cleanup): each is scoped to what
	// AkerDock owns. NO `docker system prune`, NO `-a` on images — a tagged
	// image is a potential rollback artifact (ADR-006, INV-015).
	prunes := []struct{ name, cmd string }{
		{"prune_build_cache", "docker builder prune -f --keep-storage 2GB"},
		{"prune_dangling_images", "docker image prune -f"},
		// Dead candidates: exited `-next` containers are leftovers of crashed
		// deployments — the live ones are protected by their running state.
		{"prune_dead_candidates",
			`ids=$(docker ps -aq --filter status=exited --filter label=akerdock.managed=true | while read -r id; do docker inspect --format '{{.Name}}' "$id" | grep -q -- '-next$' && echo "$id"; done); [ -n "$ids" ] && docker rm $ids || echo none`},
		{"purge_tmp", "rm -rf /data/akerdock/tmp/* 2>/dev/null; echo done"},
	}
	if server.CleanupPruneVolumes {
		// Anonymous volumes ONLY: `docker volume prune` without --all never
		// touches a named volume — data volumes and adopted volumes (§20.7,
		// external_name) are structurally out of reach (INV-015).
		prunes = append(prunes, struct{ name, cmd string }{"prune_anonymous_volumes", "docker volume prune -f"})
	}
	if server.CleanupPruneNetworks {
		prunes = append(prunes, struct{ name, cmd string }{"prune_managed_networks",
			"docker network prune -f --filter label=akerdock.managed=true"})
	}

	for _, p := range prunes {
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

	after := h.diskUsagePct(ctx, client)
	_ = h.Store.SetServerCleanupSchedule(ctx, store.SetServerCleanupScheduleParams{
		ID: server.ID, LastRunAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	h.publish(ctx, server, "server.cleanup.completed.v1", map[string]any{
		"disk_pct_before": before, "disk_pct_after": after, "reason": payload.Reason,
	})
	h.Logger.Info("server cleanup completed", "server", pguuid.String(server.Uuid),
		"before_pct", before, "after_pct", after, "reason", payload.Reason)
	return map[string]any{
		"status": "completed", "disk_pct_before": before, "disk_pct_after": after,
	}, nil
}

// diskUsagePct measures the usage of the filesystem holding /data/akerdock —
// the sub-tree every AkerDock byte lives under. Best-effort: -1 when the
// server cannot answer (the prunes still run; only the report degrades).
func (h *ServerCleanup) diskUsagePct(ctx context.Context, client *sshexec.Client) int {
	res, err := client.Run(ctx, "df -P /data/akerdock 2>/dev/null | awk 'NR==2 {gsub(/%/,\"\",$5); print $5}'")
	if err != nil || res.ExitCode != 0 {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(firstLine(res.Stdout)))
	if err != nil {
		return -1
	}
	return n
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
