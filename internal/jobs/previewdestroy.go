// Preview teardown (§20.4): routing first, then every Docker object named by
// the preview uuid, then the logical state. A failed cleanup is recorded as
// cleanup_failed and retried — never silently forgotten.

package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// PreviewDestroy is the worker handler of TypePreviewDestroy.
type PreviewDestroy struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Docker  dockerruntime.Source
	HostOps hostops.Source
	Logger  *slog.Logger
	// Audit publishes the preview.deleted outbox event; nil disables it.
	Audit *audit.Recorder
}

// Execute removes one preview instance.
func (h *PreviewDestroy) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload PreviewDestroyPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	preview, err := h.Store.GetPreviewByID(ctx, payload.PreviewID)
	if err != nil {
		return map[string]any{"status": "already gone"}, nil
	}
	if preview.Status == store.PreviewStatusDestroyed {
		return map[string]any{"status": "already destroyed"}, nil
	}
	app, err := h.Store.GetApplicationByID(ctx, preview.ApplicationID)
	if err != nil {
		// The application is gone: its deletion already removed the workloads
		// by resource label; the row just needs its tombstone.
		_ = h.Store.SetPreviewStatus(ctx, store.SetPreviewStatusParams{ID: preview.ID, Status: store.PreviewStatusDestroyed})
		return map[string]any{"status": "destroyed (application gone)"}, nil
	}
	dest, err := h.Store.GetDestinationByID(ctx, app.Resource.DestinationID)
	if err != nil {
		return nil, err
	}
	server, err := h.Store.GetServerByID(ctx, dest.ServerID)
	if err != nil {
		return nil, err
	}
	key, err := h.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return nil, err
	}
	pem, err := h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return nil, err
	}

	cleanupFailed := func(cause error) error {
		msg := cause.Error()
		_ = h.Store.SetPreviewStatus(ctx, store.SetPreviewStatusParams{
			ID: preview.ID, Status: store.PreviewStatusCleanupFailed, CleanupError: &msg,
		})
		rec.Fail(ctx, msg)
		return cause
	}

	rec.Start(ctx, "teardown")
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
	if err != nil {
		return nil, cleanupFailed(fmt.Errorf("ssh connect: %w", err))
	}
	defer func() { _ = client.Close() }()

	previewUUID := pguuid.String(preview.Uuid)
	// Routing detaches before the workload disappears (§20.6 order).
	if server.ProxyType == store.ProxyTypeTraefik {
		applier := &ProxyApplier{Store: h.Store, Client: client, Server: server, Network: dest.Network}
		if err := applier.Apply(ctx, previewUUID, "", ""); err != nil {
			return nil, cleanupFailed(fmt.Errorf("routing removal: %w", err))
		}
	}
	// Containers first, then the volumes and networks a compose preview
	// created under its own labels (§20.4.1 — "détruit intégralement"), then
	// the directory. Every object of the instance derives from the preview
	// uuid (INV-011), so nothing of production matches these filters. Docker
	// objects and the instance directory go through the agent channel
	// (ADR-052/054). A failed removal reports — the file header's promise
	// ("never silently forgotten") holds now that the shell's
	// `>/dev/null 2>&1` is gone.
	rt, err := h.Docker.Runtime(ctx, server.ID)
	if err != nil {
		return nil, cleanupFailed(fmt.Errorf("agent channel: %w", err))
	}
	ops, err := h.HostOps.HostOps(ctx, server.ID)
	if err != nil {
		return nil, cleanupFailed(fmt.Errorf("agent channel: %w", err))
	}
	previewLabel := filters.NewArgs(filters.Arg("label", "akerdock.preview_uuid="+previewUUID))
	if err := removeNamedContainers(ctx, rt, false, previewUUID, previewUUID+"-next"); err != nil {
		return nil, cleanupFailed(fmt.Errorf("container removal: %w", err))
	}
	if err := sweepContainers(ctx, rt, previewLabel, false); err != nil {
		return nil, cleanupFailed(fmt.Errorf("container sweep: %w", err))
	}
	if err := sweepVolumes(ctx, rt, filters.NewArgs(filters.Arg("label", "akerdock.resource_uuid="+previewUUID))); err != nil {
		return nil, cleanupFailed(fmt.Errorf("volume sweep: %w", err))
	}
	if err := sweepVolumes(ctx, rt, previewLabel); err != nil {
		return nil, cleanupFailed(fmt.Errorf("volume sweep: %w", err))
	}
	if err := sweepNetworks(ctx, rt, previewLabel); err != nil {
		return nil, cleanupFailed(fmt.Errorf("network sweep: %w", err))
	}
	// The PR is closed/merged: unlike the per-deployment retention, NONE of
	// this preview's rollback images survive (ADR-006). They live under the
	// preview-uuid namespace, so no production image matches (INV-011).
	if err := sweepImagesByReference(ctx, rt, "akerdock/"+previewUUID); err != nil {
		return nil, cleanupFailed(fmt.Errorf("image sweep: %w", err))
	}
	if err := ops.Remove(ctx, agentwire.FileRemoveParams{
		Path: "/var/lib/akerdock/previews/" + previewUUID, Recursive: true,
	}); err != nil {
		return nil, cleanupFailed(fmt.Errorf("instance directory removal: %w", err))
	}

	// Drop the preview from the waker's shared routing table (ADR-036). Best
	// effort: the container is already gone, and the waker ignores stale routes.
	if app.Application.PreviewScaleToZero {
		_ = removeWakerRoutes(ctx, client, previewUUID)
	}

	if err := h.Store.SetPreviewStatus(ctx, store.SetPreviewStatusParams{ID: preview.ID, Status: store.PreviewStatusDestroyed}); err != nil {
		return nil, err
	}
	// The FQDN dies with the instance: a revival (PR reopen, /deploy) derives
	// a fresh one from the CURRENT url template — keeping the old name would
	// pin every existing PR to the template of its first deployment forever.
	_ = h.Store.SetPreviewFqdn(ctx, store.SetPreviewFqdnParams{ID: preview.ID, Fqdn: nil})
	rec.Succeed(ctx, "preview removed")
	(&PreviewFeedback{Store: h.Store, Keyring: h.Keyring, Logger: h.Logger}).Notify(ctx, app, preview, "destroyed")
	if h.Audit != nil {
		var teamUUID pgtype.UUID
		if team, err := h.Store.GetTeamByID(ctx, app.Resource.TeamID); err == nil {
			teamUUID = team.Uuid
		}
		fqdn := ""
		if preview.Fqdn != nil {
			fqdn = *preview.Fqdn
		}
		h.Audit.Outbox(ctx, h.Store, "application.preview.deleted.v1", teamUUID, app.Resource.Uuid,
			"preview:"+previewUUID, map[string]any{
				"preview_uuid": previewUUID,
				"pr_id":        preview.PrID,
				"fqdn":         fqdn,
			})
	}
	h.Logger.Info("preview destroyed", "preview", previewUUID, "pr", preview.PrID)
	return map[string]any{"destroyed": previewUUID}, nil
}
