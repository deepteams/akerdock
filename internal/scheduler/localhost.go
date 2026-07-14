package scheduler

import (
	"context"

	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
)

// validateSeededLocalhost drives the pre-registered localhost server to
// `ready` without an operator's click (instance-config §6.2): install.sh
// authorizes the instance key on the host only AFTER the stack is up, so the
// first boot cannot validate it — someone has to retry once the key is in
// place, and that someone is this tick.
//
// Only the seeded, never-validated localhost server is retried (the query
// bounds it to 24h after creation): a validation that already succeeded once
// makes the server an ordinary one, whose lifecycle belongs to the operator.
func (s *Scheduler) validateSeededLocalhost(ctx context.Context) {
	servers, err := s.Store.ListUnvalidatedLocalhostServers(ctx)
	if err != nil {
		s.Logger.Warn("localhost validation: cannot list servers", "error", err)
		return
	}
	for _, server := range servers {
		lockKey := "server:validate:" + pguuid.String(server.Uuid)
		active, err := s.Store.CountActiveJobsByLockKey(ctx, &lockKey)
		if err != nil {
			s.Logger.Warn("localhost validation: lock check failed", "server_id", server.ID, "error", err)
			continue
		}
		if active > 0 {
			continue // one at a time — same guard as POST /servers/{uuid}/validate
		}
		job, err := queue.Enqueue(ctx, s.Store, queue.EnqueueOptions{
			Queue:   "maintenance",
			Type:    jobs.TypeServerValidate,
			Payload: jobs.ServerValidatePayload{ServerID: server.ID},
			LockKey: &lockKey,
			TeamID:  &server.TeamID,
		})
		if err != nil {
			s.Logger.Warn("localhost validation: enqueue failed", "server_id", server.ID, "error", err)
			continue
		}
		s.Logger.Info("localhost server validation enqueued", "server_id", server.ID, "job", pguuid.String(job.Uuid))
	}
}
