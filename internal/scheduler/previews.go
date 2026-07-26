package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/waker"
)

// reapPreviews enforces the preview lifecycle costs (§20.4.3): previews idle
// past their application's TTL are destroyed, and queued previews (capacity
// or fork approval) are promoted when room frees up.
func (s *Scheduler) reapPreviews(ctx context.Context) {
	// A heads-up before destruction (§20.4.3): previews well into their idle TTL
	// emit application.preview.expiring.v1 once, so a developer can /keep them.
	if toWarn, err := s.Store.ListPreviewsToWarn(ctx); err != nil {
		s.Logger.Warn("preview expiry warn scan failed", "error", err)
	} else {
		for _, preview := range toWarn {
			app, err := s.Store.GetApplicationByID(ctx, preview.ApplicationID)
			if err != nil {
				continue
			}
			var teamUUID pgtype.UUID
			if team, err := s.Store.GetTeamByID(ctx, app.Resource.TeamID); err == nil {
				teamUUID = team.Uuid
			}
			fqdn := ""
			if preview.Fqdn != nil {
				fqdn = *preview.Fqdn
			}
			s.Audit.Outbox(ctx, s.Store, "application.preview.expiring.v1", teamUUID, app.Resource.Uuid,
				"preview:"+pguuid.String(preview.Uuid), map[string]any{
					"preview_uuid": pguuid.String(preview.Uuid),
					"pr_id":        preview.PrID,
					"fqdn":         fqdn,
				})
			_ = s.Store.SetPreviewExpiryWarned(ctx, preview.ID)
		}
	}

	expired, err := s.Store.ListExpiredPreviews(ctx)
	if err != nil {
		s.Logger.Warn("preview TTL scan failed", "error", err)
		return
	}
	for _, preview := range expired {
		if err := jobs.EnqueuePreviewDestroy(ctx, s.Store, preview); err != nil {
			s.Logger.Warn("preview TTL destroy enqueue failed", "preview", pguuid.String(preview.Uuid), "error", err)
			continue
		}
		s.Logger.Info("preview expired (TTL), destroy queued", "preview", pguuid.String(preview.Uuid), "pr", preview.PrID)
	}

	queued, err := s.Store.ListQueuedPreviews(ctx)
	if err != nil {
		s.Logger.Warn("preview queue scan failed", "error", err)
		return
	}
	for _, preview := range queued {
		// An unapproved fork stays queued whatever the capacity (INV-010).
		if preview.IsFork && !preview.ForkApprovedAt.Valid {
			continue
		}
		app, err := s.Store.GetApplicationByID(ctx, preview.ApplicationID)
		if err != nil || !app.Application.PreviewsEnabled {
			continue
		}
		if promoted, _, err := jobs.TryPromotePreview(ctx, s.Store, s.Logger, app, preview, false); err == nil && promoted {
			s.Logger.Info("queued preview promoted", "preview", pguuid.String(preview.Uuid), "pr", preview.PrID)
		}
	}
}

// idlePastWindow reports whether a resource last active at `last` has been idle
// for at least `windowMin` minutes as of `now` (ADR-036). A non-positive window
// falls back to the default so a misconfiguration never sleeps instantly.
func idlePastWindow(last time.Time, windowMin int32, now time.Time) bool {
	window := time.Duration(windowMin) * time.Minute
	if windowMin <= 0 {
		window = 30 * time.Minute
	}
	return now.Sub(last) >= window
}

// scaleZeroPreviews runs the scale-to-zero lifecycle (ADR-036): active previews
// idle past their window are stopped (sleeping), and sleeping previews the waker
// has served again (fresh activity file) are flipped back to active. Activity is
// read from the waker's per-resource file over SSH — the server never contacts
// the control plane (push §18.1).
func (s *Scheduler) scaleZeroPreviews(ctx context.Context) {
	active, err := s.Store.ListPreviewsForScaleToZero(ctx)
	if err != nil {
		s.Logger.Warn("scale-to-zero scan failed", "error", err)
		return
	}
	sleeping, err := s.Store.ListSleepingPreviews(ctx)
	if err != nil {
		s.Logger.Warn("scale-to-zero sleeping scan failed", "error", err)
		return
	}
	if len(active) == 0 && len(sleeping) == 0 {
		return
	}

	// One SSH connection per server, reused across its previews.
	clients := map[int64]remoteClient{}
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	clientFor := func(server store.Server) remoteClient {
		if c, ok := clients[server.ID]; ok {
			return c
		}
		key, err := s.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
		if err != nil {
			return nil
		}
		pem, err := s.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
		if err != nil {
			return nil
		}
		var c remoteClient
		if s.dialSSH != nil {
			c, err = s.dialSSH(ctx, server, string(pem))
		} else {
			c, err = sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
				time.Duration(server.SshTimeoutSeconds)*time.Second, hostKeyOf(server))
		}
		if err != nil {
			return nil
		}
		clients[server.ID] = c
		return c
	}

	now := time.Now()

	for _, p := range active {
		server, ok := s.previewServer(ctx, p.ApplicationID)
		if !ok {
			continue
		}
		client := clientFor(server)
		if client == nil {
			continue
		}
		uuid := pguuid.String(p.Uuid)
		last := readWakerActivity(ctx, client, uuid)
		if last.IsZero() { // no activity yet: fall back to the last known times
			last = latestOf(p.LastActivityAt, p.LastDeployedAt)
		}
		if last.IsZero() || !idlePastWindow(last, p.ScaleToZeroAfterMinutes, now) {
			continue
		}
		if err := stopPreviewContainers(ctx, client, uuid); err != nil {
			s.Logger.Warn("scale-to-zero stop failed", "preview", uuid, "error", err)
			continue
		}
		if err := s.Store.SetPreviewSleeping(ctx, p.ID); err != nil {
			s.Logger.Warn("scale-to-zero status update failed", "preview", uuid, "error", err)
			continue
		}
		s.Logger.Info("preview scaled to zero (asleep)", "preview", uuid, "pr", p.PrID, "idle_since", last)
	}

	for _, p := range sleeping {
		server, ok := s.previewServer(ctx, p.ApplicationID)
		if !ok {
			continue
		}
		client := clientFor(server)
		if client == nil {
			continue
		}
		uuid := pguuid.String(p.Uuid)
		last := readWakerActivity(ctx, client, uuid)
		// The waker records activity when it serves a request, which it only does
		// after starting the container: a timestamp newer than when we slept the
		// preview means it is awake again.
		if !last.IsZero() && p.UpdatedAt.Valid && last.After(p.UpdatedAt.Time) {
			if err := s.Store.SetPreviewAwake(ctx, p.ID); err != nil {
				s.Logger.Warn("scale-to-zero wake update failed", "preview", uuid, "error", err)
				continue
			}
			s.Logger.Info("preview woken by waker", "preview", uuid, "pr", p.PrID)
		}
	}
}

// previewServer resolves a preview's application to its server.
func (s *Scheduler) previewServer(ctx context.Context, applicationID int64) (store.Server, bool) {
	app, err := s.Store.GetApplicationByID(ctx, applicationID)
	if err != nil {
		return store.Server{}, false
	}
	dest, err := s.Store.GetDestinationByID(ctx, app.Resource.DestinationID)
	if err != nil {
		return store.Server{}, false
	}
	server, err := s.Store.GetServerByID(ctx, dest.ServerID)
	if err != nil {
		return store.Server{}, false
	}
	return server, true
}

// readWakerActivity reads a preview's waker activity file over SSH; a zero time
// means absent or unreadable (never an error the scan should stop on).
func readWakerActivity(ctx context.Context, client remoteClient, uuid string) time.Time {
	res, err := client.Run(ctx, "cat "+waker.ActivityPath(waker.DefaultDir, uuid)+" 2>/dev/null || true")
	if err != nil || res == nil || res.Stdout == "" {
		return time.Time{}
	}
	at, err := waker.ParseActivity(res.Stdout)
	if err != nil {
		return time.Time{}
	}
	return at
}

// stopPreviewContainers stops every container of the preview — the single
// container by name and, for a compose stack, every one labelled with the
// preview uuid (INV-011). Stopped, not removed: the waker wakes them with
// `docker start`.
func stopPreviewContainers(ctx context.Context, client remoteClient, uuid string) error {
	cmd := fmt.Sprintf(
		"docker stop -t 10 %s >/dev/null 2>&1; "+
			"docker ps -q --filter label=akerdock.preview_uuid=%s | xargs -r docker stop -t 10 >/dev/null 2>&1; true",
		uuid, uuid)
	res, err := client.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker stop exited %d", res.ExitCode)
	}
	return nil
}

// latestOf returns the most recent of two nullable timestamps (zero if both
// null).
func latestOf(a, b pgtype.Timestamptz) time.Time {
	var t time.Time
	if a.Valid {
		t = a.Time
	}
	if b.Valid && b.Time.After(t) {
		t = b.Time
	}
	return t
}
