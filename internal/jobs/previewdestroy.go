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

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// PreviewDestroy is the worker handler of TypePreviewDestroy.
type PreviewDestroy struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
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

	cleanupFailed := func(cause error) (any, error) {
		msg := cause.Error()
		_ = h.Store.SetPreviewStatus(ctx, store.SetPreviewStatusParams{
			ID: preview.ID, Status: store.PreviewStatusCleanupFailed, CleanupError: &msg,
		})
		rec.Fail(ctx, msg)
		return nil, cause
	}

	rec.Start(ctx, "teardown")
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
	if err != nil {
		return cleanupFailed(fmt.Errorf("ssh connect: %w", err))
	}
	defer func() { _ = client.Close() }()

	previewUUID := pguuid.String(preview.Uuid)
	// Routing detaches before the workload disappears (§20.6 order).
	if server.ProxyType == store.ProxyTypeTraefik {
		applier := &ProxyApplier{Store: h.Store, Client: client, Server: server, Network: dest.Network}
		if err := applier.Apply(ctx, previewUUID, "", ""); err != nil {
			return cleanupFailed(fmt.Errorf("routing removal: %w", err))
		}
	}
	cmd := fmt.Sprintf(
		"docker rm -f %s %s-next >/dev/null 2>&1; "+
			"docker ps -aq --filter label=akerdock.preview_uuid=%s | xargs -r docker rm -f >/dev/null 2>&1; "+
			"docker volume ls -q --filter label=akerdock.resource_uuid=%s | xargs -r docker volume rm -f >/dev/null 2>&1; "+
			"rm -rf /data/akerdock/previews/%s",
		previewUUID, previewUUID, previewUUID, previewUUID, previewUUID)
	if res, err := client.Run(ctx, cmd); err != nil || res.ExitCode != 0 {
		if err == nil {
			err = fmt.Errorf("remote cleanup exited with code %d", res.ExitCode)
		}
		return cleanupFailed(err)
	}

	if err := h.Store.SetPreviewStatus(ctx, store.SetPreviewStatusParams{ID: preview.ID, Status: store.PreviewStatusDestroyed}); err != nil {
		return nil, err
	}
	rec.Succeed(ctx, "preview removed")
	(&PreviewFeedback{Store: h.Store, Keyring: h.Keyring, Logger: h.Logger}).Notify(ctx, app, preview, "destroyed")
	h.Logger.Info("preview destroyed", "preview", previewUUID, "pr", preview.PrID)
	return map[string]any{"destroyed": previewUUID}, nil
}
