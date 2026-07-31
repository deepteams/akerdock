package scheduler

import (
	"context"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/hostops"
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
		// A manual-first reservation (preview_deploy_on_open=false, no human
		// deploy order yet) shares the 'queued' status with capacity-queued
		// deploys — promoting it here would auto-deploy a preview the setting
		// promised to leave alone.
		if jobs.ManualFirstReserved(app, preview) {
			continue
		}
		if promoted, _, err := jobs.TryPromotePreview(ctx, s.Store, s.Logger, app, preview, false); err == nil && promoted {
			s.Logger.Info("queued preview promoted", "preview", pguuid.String(preview.Uuid), "pr", preview.PrID)
		}
	}
}

// emitPreviewEvent publishes a preview lifecycle event to the outbox — the SSE
// feed the UI listens to (ADR-024): the previews tab reloads on it, so a
// scheduler-driven sleep or wake shows up without a manual page refresh.
func (s *Scheduler) emitPreviewEvent(ctx context.Context, eventType string, applicationID int64, previewUUID pgtype.UUID, prID int32) {
	app, err := s.Store.GetApplicationByID(ctx, applicationID)
	if err != nil {
		return
	}
	var teamUUID pgtype.UUID
	if team, err := s.Store.GetTeamByID(ctx, app.Resource.TeamID); err == nil {
		teamUUID = team.Uuid
	}
	s.Audit.Outbox(ctx, s.Store, eventType, teamUUID, app.Resource.Uuid,
		"preview:"+pguuid.String(previewUUID), map[string]any{
			"preview_uuid": pguuid.String(previewUUID),
			"pr_id":        prID,
		})
}

// emitApplicationEvent publishes an application lifecycle event (sleep/wake,
// ADR-037/040) to the outbox — the application pages reload on it, so a
// scale-to-zero transition shows up without a manual refresh.
func (s *Scheduler) emitApplicationEvent(ctx context.Context, eventType string, resourceID int64, resourceUUID pgtype.UUID) {
	app, err := s.Store.GetApplicationByID(ctx, resourceID)
	if err != nil {
		return
	}
	var teamUUID pgtype.UUID
	if team, err := s.Store.GetTeamByID(ctx, app.Resource.TeamID); err == nil {
		teamUUID = team.Uuid
	}
	s.Audit.Outbox(ctx, s.Store, eventType, teamUUID, resourceUUID,
		"application:"+pguuid.String(resourceUUID), map[string]any{"name": app.Resource.Name})
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

	scan := s.newWakerScan(ctx)
	defer scan.close()

	now := time.Now()

	for _, p := range active {
		server, network, ok := s.previewPlacement(ctx, p.ApplicationID)
		if !ok {
			continue
		}
		// The reconcile is opportunistic (SSH, bootstrap family); the
		// DECISIONS require the agent channel — an unreachable channel skips
		// them rather than reading absence as idleness.
		if client := scan.client(server); client != nil {
			scan.reconcile(server, network, client)
		}
		ops := scan.hostOps(server)
		if ops == nil {
			continue
		}
		uuid := pguuid.String(p.Uuid)
		last, err := readWakerActivity(ctx, ops, uuid)
		if err != nil {
			s.Logger.Warn("scale-to-zero activity unreadable — decision skipped", "preview", uuid, "error", err)
			continue
		}
		// A redeploy IS activity: the waker file only moves on proxied
		// requests, so a preview relaunched after it slept would otherwise
		// read as idle since its stale file and be re-slept — and shown
		// sleeping — right after deploying. Take the latest of every signal.
		if dbLast := latestOf(p.LastActivityAt, p.LastDeployedAt); dbLast.After(last) {
			last = dbLast
		}
		if last.IsZero() || !idlePastWindow(last, p.ScaleToZeroAfterMinutes, now) {
			continue
		}
		rt, err := s.Docker.Runtime(ctx, server.ID)
		if err != nil {
			s.Logger.Warn("scale-to-zero stop failed", "preview", uuid, "error", err)
			continue
		}
		if err := stopPreviewContainers(ctx, rt, uuid); err != nil {
			s.Logger.Warn("scale-to-zero stop failed", "preview", uuid, "error", err)
			continue
		}
		if err := s.Store.SetPreviewSleeping(ctx, p.ID); err != nil {
			s.Logger.Warn("scale-to-zero status update failed", "preview", uuid, "error", err)
			continue
		}
		s.emitPreviewEvent(ctx, "application.preview.slept.v1", p.ApplicationID, p.Uuid, p.PrID)
		s.Logger.Info("preview scaled to zero (asleep)", "preview", uuid, "pr", p.PrID, "idle_since", last)
	}

	for _, p := range sleeping {
		server, network, ok := s.previewPlacement(ctx, p.ApplicationID)
		if !ok {
			continue
		}
		if client := scan.client(server); client != nil {
			scan.reconcile(server, network, client)
		}
		ops := scan.hostOps(server)
		if ops == nil {
			continue
		}
		uuid := pguuid.String(p.Uuid)
		last, err := readWakerActivity(ctx, ops, uuid)
		if err != nil {
			s.Logger.Warn("scale-to-zero activity unreadable — decision skipped", "preview", uuid, "error", err)
			continue
		}
		// The waker records activity when it serves a request, which it only does
		// after starting the container: a timestamp newer than when we slept the
		// preview means it is awake again.
		if !last.IsZero() && p.UpdatedAt.Valid && last.After(p.UpdatedAt.Time) {
			if err := s.Store.SetPreviewAwake(ctx, p.ID); err != nil {
				s.Logger.Warn("scale-to-zero wake update failed", "preview", uuid, "error", err)
				continue
			}
			s.emitPreviewEvent(ctx, "application.preview.woken.v1", p.ApplicationID, p.Uuid, p.PrID)
			s.Logger.Info("preview woken by waker", "preview", uuid, "pr", p.PrID)
		} else {
			// The one trace that tells a stuck-sleeping investigation apart:
			// no activity file (the waker never served/recorded) vs a stale
			// one (older than the sleep — the wake never completed).
			s.Logger.Debug("scale-to-zero: sleeping preview not woken",
				"preview", uuid, "activity", last, "slept_at", p.UpdatedAt.Time)
		}
	}
}

// scaleZeroApplications runs the scale-to-zero lifecycle for production
// applications (ADR-037): the exact mirror of the preview scan, keyed on the
// application's own resource uuid. A manually stopped app is excluded by the
// query (desired_status = running only).
func (s *Scheduler) scaleZeroApplications(ctx context.Context) {
	toSleep, err := s.Store.ListApplicationsToSleep(ctx)
	if err != nil {
		s.Logger.Warn("app scale-to-zero scan failed", "error", err)
		return
	}
	sleeping, err := s.Store.ListSleepingApplications(ctx)
	if err != nil {
		s.Logger.Warn("app scale-to-zero sleeping scan failed", "error", err)
		return
	}
	if len(toSleep) == 0 && len(sleeping) == 0 {
		return
	}

	scan := s.newWakerScan(ctx)
	defer scan.close()

	now := time.Now()

	for _, a := range toSleep {
		server, network, ok := s.previewPlacement(ctx, a.ID)
		if !ok {
			continue
		}
		if client := scan.client(server); client != nil {
			scan.reconcile(server, network, client)
		}
		ops := scan.hostOps(server)
		if ops == nil {
			continue
		}
		uuid := pguuid.String(a.Uuid)
		last, err := readWakerActivity(ctx, ops, uuid)
		if err != nil {
			s.Logger.Warn("scale-to-zero activity unreadable — decision skipped", "application", uuid, "error", err)
			continue
		}
		// Same rule as previews: a fresh deploy/update counts as activity, or
		// a stale waker file would put a just-redeployed app straight back to
		// sleep.
		if a.UpdatedAt.Valid && a.UpdatedAt.Time.After(last) {
			last = a.UpdatedAt.Time
		}
		if last.IsZero() || !idlePastWindow(last, a.ScaleToZeroAfterMinutes, now) {
			continue
		}
		rt, err := s.Docker.Runtime(ctx, server.ID)
		if err != nil {
			s.Logger.Warn("app scale-to-zero stop failed", "application", uuid, "error", err)
			continue
		}
		if err := stopResourceContainers(ctx, rt, uuid); err != nil {
			s.Logger.Warn("app scale-to-zero stop failed", "application", uuid, "error", err)
			continue
		}
		if err := s.Store.SetApplicationSlept(ctx, a.ID); err != nil {
			s.Logger.Warn("app scale-to-zero status update failed", "application", uuid, "error", err)
			continue
		}
		s.emitApplicationEvent(ctx, "application.slept.v1", a.ID, a.Uuid)
		s.Logger.Info("application scaled to zero (asleep)", "application", uuid, "idle_since", last)
	}

	for _, a := range sleeping {
		server, network, ok := s.previewPlacement(ctx, a.ID)
		if !ok {
			continue
		}
		if client := scan.client(server); client != nil {
			scan.reconcile(server, network, client)
		}
		ops := scan.hostOps(server)
		if ops == nil {
			continue
		}
		uuid := pguuid.String(a.Uuid)
		last, err := readWakerActivity(ctx, ops, uuid)
		if err != nil {
			s.Logger.Warn("scale-to-zero activity unreadable — decision skipped", "application", uuid, "error", err)
			continue
		}
		// Woken by the waker if the activity file is newer than when we slept it.
		if !last.IsZero() && a.ScaleSleptAt.Valid && last.After(a.ScaleSleptAt.Time) {
			if err := s.Store.SetApplicationAwake(ctx, a.ID); err != nil {
				s.Logger.Warn("app scale-to-zero wake update failed", "application", uuid, "error", err)
				continue
			}
			s.emitApplicationEvent(ctx, "application.woken.v1", a.ID, a.Uuid)
			s.Logger.Info("application woken by waker", "application", uuid)
		} else {
			s.Logger.Debug("scale-to-zero: sleeping application not woken",
				"application", uuid, "activity", last, "slept_at", a.ScaleSleptAt.Time)
		}
	}
}

// ensureAgents provisions the helper on EVERY ready server (ADR-040/052):
// Docker operations flow through the agent's command channel, so a server
// without an agent has no Docker path at all — build servers and
// database-only hosts included, proxy or not. Runs at the maintenance
// cadence; the ensure command is a no-op when the image and spec already
// match, so a steady state costs one inspect per pass.
func (s *Scheduler) ensureAgents(ctx context.Context) {
	if s.WakerImage == "" {
		return
	}
	servers, err := s.Store.ListReadyServers(ctx)
	if err != nil {
		s.Logger.Warn("agent ensure: cannot list servers", "error", err)
		return
	}
	if len(servers) == 0 {
		return
	}
	scan := s.newWakerScan(ctx)
	defer scan.close()
	for _, server := range servers {
		client := scan.client(server)
		if client == nil {
			continue
		}
		// The helper joins the server's default destination network when one
		// exists (so it can also serve wakes there); the plain bridge is
		// enough for an observation-only server.
		network := "bridge"
		if dest, err := s.Store.GetDefaultDestination(ctx, server.ID); err == nil && dest.Network != "" {
			network = dest.Network
		}
		scan.reconcile(server, network, client)
	}
}

// wakerScan shares one SSH connection per server across a scale-to-zero pass
// (for the waker reconcile — bootstrap family, ADR-054) and one host-ops
// handle (for the activity reads), reconciling each server's waker image once.
type wakerScan struct {
	s          *Scheduler
	ctx        context.Context
	clients    map[int64]remoteClient
	opsCache   map[int64]hostops.Ops
	reconciled map[int64]bool
}

func (s *Scheduler) newWakerScan(ctx context.Context) *wakerScan {
	return &wakerScan{
		s: s, ctx: ctx,
		clients: map[int64]remoteClient{}, opsCache: map[int64]hostops.Ops{},
		reconciled: map[int64]bool{},
	}
}

// hostOps resolves (and caches) the server's agent file primitives; nil skips
// the server's scale-to-zero decisions this pass, with the cause logged once
// — the same stance as an unreachable SSH server.
func (w *wakerScan) hostOps(server store.Server) hostops.Ops {
	if ops, ok := w.opsCache[server.ID]; ok {
		return ops
	}
	ops, err := w.s.HostOps.HostOps(w.ctx, server.ID)
	if err != nil {
		w.opsCache[server.ID] = nil
		w.s.Logger.Warn("scale-to-zero scan: agent channel unavailable, resources skipped",
			"server_id", server.ID, "error", err)
		return nil
	}
	w.opsCache[server.ID] = ops
	return ops
}

func (w *wakerScan) client(server store.Server) remoteClient {
	if c, ok := w.clients[server.ID]; ok {
		return c
	}
	// A nil client silently skips every scale-to-zero decision for the
	// server's resources — sleeping previews then LOOK stuck (never flipped
	// back to active) with no trace. Log the cause; cache the nil so one
	// broken server costs one dial and one log line per pass, not one per
	// resource.
	fail := func(stage string, err error) remoteClient {
		w.clients[server.ID] = nil
		w.s.Logger.Warn("scale-to-zero scan: server unreachable, resources skipped",
			"server_id", server.ID, "host", server.Host, "stage", stage, "error", err)
		return nil
	}
	key, err := w.s.Store.GetPrivateKeyByID(w.ctx, server.PrivateKeyID)
	if err != nil {
		return fail("private key fetch", err)
	}
	pem, err := w.s.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return fail("private key decrypt", err)
	}
	var c remoteClient
	if w.s.dialSSH != nil {
		c, err = w.s.dialSSH(w.ctx, server, string(pem))
	} else {
		c, err = sshexec.Dial(w.ctx, server.Host, int(server.Port), server.SshUser, string(pem),
			time.Duration(server.SshTimeoutSeconds)*time.Second, hostKeyOf(server))
	}
	if err != nil {
		return fail("ssh dial", err)
	}
	w.clients[server.ID] = c
	return c
}

// reconcile upgrades a server's waker in place once per pass when its running
// image differs from this release's (ADR-036), re-injecting the agent
// enrollment (ADR-040) so a recreate never loses it.
func (w *wakerScan) reconcile(server store.Server, network string, client remoteClient) {
	if w.s.WakerImage == "" || network == "" || w.reconciled[server.ID] {
		return
	}
	w.reconciled[server.ID] = true
	env := jobs.AgentEnvForServer(w.ctx, w.s.Store, w.s.Keyring, w.s.Logger, server, w.s.InstancePort)
	if _, err := client.Run(w.ctx, jobs.WakerEnsureCommand(network, w.s.WakerImage, env)); err != nil {
		w.s.Logger.Warn("waker image reconcile failed", "server_id", server.ID, "error", err)
	}
}

func (w *wakerScan) close() {
	for _, c := range w.clients {
		_ = c.Close()
	}
}

// previewPlacement resolves a preview's application to its server and the proxy
// network the waker shares with the proxy and the app containers.
func (s *Scheduler) previewPlacement(ctx context.Context, applicationID int64) (store.Server, string, bool) {
	app, err := s.Store.GetApplicationByID(ctx, applicationID)
	if err != nil {
		return store.Server{}, "", false
	}
	dest, err := s.Store.GetDestinationByID(ctx, app.Resource.DestinationID)
	if err != nil {
		return store.Server{}, "", false
	}
	server, err := s.Store.GetServerByID(ctx, dest.ServerID)
	if err != nil {
		return store.Server{}, "", false
	}
	return server, dest.Network, true
}

// readWakerActivity reads a resource's waker activity file through the agent
// channel (ADR-054). A zero time with a nil error means absent or unreadable
// — genuine "no activity yet". A non-nil error means the channel itself
// failed: the caller must SKIP the decision, not treat it as idleness.
func readWakerActivity(ctx context.Context, ops hostops.Ops, uuid string) (time.Time, error) {
	res, err := ops.ReadFile(ctx, agentwire.FileReadParams{Path: waker.ActivityPath(waker.DefaultDir, uuid)})
	if err != nil {
		return time.Time{}, err
	}
	if !res.Found {
		return time.Time{}, nil
	}
	at, err := waker.ParseActivity(string(res.Content))
	if err != nil {
		//nolint:nilerr // an unreadable file is "no activity yet", not a channel failure.
		return time.Time{}, nil
	}
	return at, nil
}

// stopPreviewContainers stops every container of the preview — the single
// container by name and, for a compose stack, every one labelled with the
// preview uuid (INV-011). Stopped, not removed: the waker wakes them with
// a plain start.
func stopPreviewContainers(ctx context.Context, rt dockerruntime.Runtime, uuid string) error {
	return stopByLabel(ctx, rt, uuid, "akerdock.preview_uuid")
}

// stopResourceContainers stops every container of an application — the single
// container by name and, for a compose stack, every one labelled with the
// resource uuid (ADR-037). Stopped, not removed.
func stopResourceContainers(ctx context.Context, rt dockerruntime.Runtime, uuid string) error {
	return stopByLabel(ctx, rt, uuid, "akerdock.resource_uuid")
}

// stopByLabel puts a resource to sleep: the container named after the uuid
// (absent for a compose stack — tolerated), then every running container
// carrying the label. A real stop failure reports, so a still-running
// resource is never recorded asleep.
func stopByLabel(ctx context.Context, rt dockerruntime.Runtime, uuid, label string) error {
	t := 10
	if err := rt.ContainerStop(ctx, uuid, container.StopOptions{Timeout: &t}); err != nil && !dockerruntime.IsNotFound(err) {
		return err
	}
	list, err := rt.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", label+"="+uuid)),
	})
	if err != nil {
		return err
	}
	for _, c := range list {
		if len(c.Names) == 0 {
			continue
		}
		if err := rt.ContainerStop(ctx, strings.TrimPrefix(c.Names[0], "/"), container.StopOptions{Timeout: &t}); err != nil && !dockerruntime.IsNotFound(err) {
			return err
		}
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
