package scheduler

import (
	"context"

	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
)

// reapPreviews enforces the preview lifecycle costs (§20.4.3): previews idle
// past their application's TTL are destroyed, and queued previews (capacity
// or fork approval) are promoted when room frees up.
func (s *Scheduler) reapPreviews(ctx context.Context) {
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
		if promoted, _, err := jobs.TryPromotePreview(ctx, s.Store, s.Logger, app, preview); err == nil && promoted {
			s.Logger.Info("queued preview promoted", "preview", pguuid.String(preview.Uuid), "pr", preview.PrID)
		}
	}
}
