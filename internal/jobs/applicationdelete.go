package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeApplicationDelete removes an application: routing first, then
// workloads, then the logical object (§20.6).
const TypeApplicationDelete = "application.delete"

// ApplicationDeletePayload references the resource to delete.
type ApplicationDeletePayload struct {
	ResourceID    int64 `json:"resource_id"`
	DeleteVolumes bool  `json:"delete_volumes"`
}

// ApplicationDelete implements the idempotent deletion job. Persistent
// volumes are kept unless explicitly requested otherwise (INV-008).
type ApplicationDelete struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
}

// Execute performs one deletion attempt; every action tolerates already
// deleted remote objects (idempotent replays, §20.6).
func (h *ApplicationDelete) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload ApplicationDeletePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	app, err := h.Store.GetApplicationByID(ctx, payload.ResourceID)
	if err != nil {
		// Compose stacks (resource_type = service) share the deletion flow:
		// their objects are found by the management labels (§2.3).
		resource, rerr := h.Store.GetResourceByID(ctx, payload.ResourceID)
		if rerr != nil || resource.ResourceType != store.ResourceTypeService {
			return map[string]any{"status": "already deleted"}, nil
		}
		app = store.GetApplicationByIDRow{Resource: resource}
	}
	appUUID := pguuid.String(app.Resource.Uuid)

	server, dest, key, pem, err := h.loadTarget(ctx, app)
	if err != nil {
		return nil, err
	}
	_ = key

	rec.Start(ctx, "remove_workload")
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem), time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
	if err != nil {
		rec.Fail(ctx, "SSH connection failed — the server must be reachable to clean up; retry once it is back")
		return nil, err
	}
	defer func() { _ = client.Close() }()

	// Routing is removed first and its removal verified through the proxy
	// API (§6.5, §20.6): the domains detach before the workload disappears.
	// The PREVIEWS' routing too — their instances die with the application
	// (label-driven removal below), and a routing file pointing at a dead
	// container would otherwise survive as a permanent 502.
	previews, _ := h.Store.ListPreviewsForApplication(ctx, app.Resource.ID)
	if server.ProxyType == store.ProxyTypeTraefik {
		applier := &ProxyApplier{Store: h.Store, Client: client, Server: server, Network: dest.Network}
		if err := applier.Apply(ctx, appUUID, "", ""); err != nil {
			rec.Fail(ctx, "could not remove the routing — the workload is left untouched, retry once the proxy is healthy")
			return nil, err
		}
		for _, p := range previews {
			if p.Status == store.PreviewStatusDestroyed {
				continue
			}
			if err := applier.Apply(ctx, pguuid.String(p.Uuid), "", ""); err != nil {
				rec.Fail(ctx, "could not remove a preview's routing — retry once the proxy is healthy")
				return nil, err
			}
		}
	}
	// Removal is label-driven (§2.3): a compose stack has one container per
	// service plus candidates, and its own networks — a name-based rm would
	// leave all of it behind. The name-based rm stays for belt and braces.
	byLabel := "--filter label=akerdock.managed=true --filter label=akerdock.resource_uuid=" + appUUID
	cmd := fmt.Sprintf(
		"docker ps -aq %s | xargs -r docker rm -f >/dev/null 2>&1; "+
			"docker rm -f %s %s-next >/dev/null 2>&1; "+
			"docker network ls -q %s | xargs -r docker network rm >/dev/null 2>&1; "+
			"rm -rf /var/lib/akerdock/applications/%s /var/lib/akerdock/services/%s",
		byLabel, appUUID, appUUID, byLabel, appUUID, appUUID)
	for _, p := range previews {
		cmd += " /var/lib/akerdock/previews/" + pguuid.String(p.Uuid)
	}
	if payload.DeleteVolumes {
		cmd += fmt.Sprintf("; docker volume ls -q --filter label=akerdock.resource_uuid=%s | xargs -r docker volume rm -f", appUUID)
	}
	if res, err := client.Run(ctx, cmd); err != nil || res.ExitCode != 0 {
		// Record what is actually still there (§20.6.4). Without this the
		// operator is told "remnants recorded" and finds nothing — and orphan
		// containers and volumes keep consuming the server silently.
		h.recordRemnants(ctx, client, app.Resource.ID, appUUID)
		rec.Fail(ctx, "remote cleanup failed — remnants recorded, retry or forget with acknowledgement (§20.6.4)")
		if err == nil {
			err = fmt.Errorf("remote cleanup exited with code %d", res.ExitCode)
		}
		return nil, err
	}
	rec.Succeed(ctx, "containers and files removed from "+dest.Network)

	rec.Start(ctx, "tombstone")
	if _, err := h.Store.SoftDeleteResource(ctx, app.Resource.ID); err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	// The resource is a tombstone, but the domain rows must GO: their
	// (fqdn, path) uniqueness is global and hard (INV-002) — left behind,
	// they lock the URL against any future application, forever. Both kinds:
	// the application's own domains and its compose components' (§6).
	if err := h.Store.DeleteDomainsForApplication(ctx, &app.Resource.ID); err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	if err := h.Store.DeleteComponentDomainsForResource(ctx, app.Resource.ID); err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	// Preview tombstones: their workloads and routing died above with the
	// application — the rows must say so, and drop their FQDNs.
	for _, p := range previews {
		if p.Status == store.PreviewStatusDestroyed {
			continue
		}
		_ = h.Store.SetPreviewStatus(ctx, store.SetPreviewStatusParams{ID: p.ID, Status: store.PreviewStatusDestroyed})
		_ = h.Store.SetPreviewFqdn(ctx, store.SetPreviewFqdnParams{ID: p.ID, Fqdn: nil})
	}
	rec.Succeed(ctx, "resource tombstoned")
	h.Logger.Info("application deleted", "app_uuid", appUUID, "volumes_deleted", payload.DeleteVolumes)
	return map[string]any{"deleted": appUUID, "volumes_deleted": payload.DeleteVolumes}, nil
}

func (h *ApplicationDelete) loadTarget(ctx context.Context, app store.GetApplicationByIDRow) (store.Server, store.Destination, store.PrivateKey, []byte, error) {
	dest, err := h.Store.GetDestinationByID(ctx, app.Resource.DestinationID)
	if err != nil {
		return store.Server{}, store.Destination{}, store.PrivateKey{}, nil, err
	}
	server, err := h.Store.GetServerByID(ctx, dest.ServerID)
	if err != nil {
		return store.Server{}, store.Destination{}, store.PrivateKey{}, nil, err
	}
	key, err := h.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return store.Server{}, store.Destination{}, store.PrivateKey{}, nil, err
	}
	pem, err := h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return store.Server{}, store.Destination{}, store.PrivateKey{}, nil, err
	}
	return server, dest, key, pem, nil
}

// recordRemnants inspects what a failed deletion left on the server and stores
// it on the resource (§20.6.4).
//
// It is best-effort by design: it runs on a path that is already failing, so it
// must never fail harder. An empty inventory is still recorded — knowing that
// nothing is left is exactly what lets an operator forget the job with a clear
// conscience.
func (h *ApplicationDelete) recordRemnants(ctx context.Context, client *sshexec.Client, resourceID int64, appUUID string) {
	inventory := map[string]any{
		"observed_at": time.Now().UTC().Format(time.RFC3339),
		"server_uuid": appUUID,
	}

	res, err := client.Run(ctx, fmt.Sprintf(
		"echo '---containers'; docker ps -a --format '{{.Names}}' --filter label=akerdock.resource_uuid=%s; "+
			"echo '---volumes'; docker volume ls -q --filter label=akerdock.resource_uuid=%s; "+
			"echo '---files'; ls -d /var/lib/akerdock/applications/%s 2>/dev/null",
		appUUID, appUUID, appUUID))
	if err != nil || res == nil {
		inventory["error"] = "the server could not be inspected — the remnants are unknown"
	} else {
		containers, volumes, files := parseRemnants(res.Stdout)
		inventory["containers"] = containers
		inventory["volumes"] = volumes
		inventory["files"] = files
	}

	raw, err := json.Marshal(inventory)
	if err != nil {
		return
	}
	if err := h.Store.SetResourceRemnants(ctx, store.SetResourceRemnantsParams{
		ID: resourceID, Remnants: raw,
	}); err != nil {
		h.Logger.Warn("could not record the deletion remnants", "resource_id", resourceID, "error", err)
		return
	}
	h.Logger.Warn("deletion left remnants on the server", "app_uuid", appUUID, "remnants", string(raw))
}

// parseRemnants splits the inspection output into its three sections.
func parseRemnants(out string) (containers, volumes, files []string) {
	section := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "---"):
			section = strings.TrimPrefix(line, "---")
			continue
		}
		switch section {
		case "containers":
			containers = append(containers, line)
		case "volumes":
			volumes = append(volumes, line)
		case "files":
			files = append(files, line)
		}
	}
	return containers, volumes, files
}
