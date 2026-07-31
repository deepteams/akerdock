package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepteams/akerdock/internal/adoption"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// Adoption of existing Docker resources (§20.7, ADR-013/ADR-023): scan the
// unmanaged inventory of a server, adopt selected candidates WITHOUT
// restarting them, and disown — the reverse — without destroying anything.
const (
	TypeAdoptionScan   = "adoption.scan"
	TypeAdoptionAdopt  = "adoption.adopt"
	TypeResourceDisown = "resource.disown"
)

// AdoptionScanPayload references the scan row to fill.
type AdoptionScanPayload struct {
	ScanID int64 `json:"scan_id"`
}

// AdoptPayload selects candidates of a completed scan.
type AdoptPayload struct {
	ScanID        int64       `json:"scan_id"`
	EnvironmentID int64       `json:"environment_id"`
	Items         []AdoptItem `json:"items"`
}

// AdoptItem is one selected candidate, optionally renamed.
type AdoptItem struct {
	CandidateID string `json:"candidate_id"`
	Name        string `json:"name,omitempty"`
}

// DisownPayload references the resource to release.
type DisownPayload struct {
	ResourceID int64 `json:"resource_id"`
}

// Adoption executes the three job types.
type Adoption struct {
	Store   *store.Queries
	Pool    *pgxpool.Pool
	Keyring *envelope.Keyring
	Docker  dockerruntime.Source
	Logger  *slog.Logger
}

// ExecuteScan inventories the unmanaged containers and compose stacks of a
// server and stores the proposed mapping (§20.7 steps 1–3). Idempotent: a
// replay overwrites the same scan row.
func (h *Adoption) ExecuteScan(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload AdoptionScanPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	scan, err := h.Store.GetAdoptionScanByID(ctx, payload.ScanID)
	if err != nil {
		return nil, fmt.Errorf("adoption scan vanished: %w", err)
	}
	_ = h.Store.SetAdoptionScanRunning(ctx, scan.ID)

	candidates, err := h.runScan(ctx, scan, rec)
	if err != nil {
		_ = h.Store.FailAdoptionScan(ctx, store.FailAdoptionScanParams{ID: scan.ID, Error: ptrOf(err.Error())})
		return nil, err
	}
	raw, err := json.Marshal(candidates)
	if err != nil {
		_ = h.Store.FailAdoptionScan(ctx, store.FailAdoptionScanParams{ID: scan.ID, Error: ptrOf(err.Error())})
		return nil, err
	}
	if err := h.Store.CompleteAdoptionScan(ctx, store.CompleteAdoptionScanParams{ID: scan.ID, Candidates: raw}); err != nil {
		return nil, err
	}
	adoptable := 0
	for _, c := range candidates {
		if c.Adoptable {
			adoptable++
		}
	}
	return map[string]any{"scan_uuid": pguuid.String(scan.Uuid), "candidates": len(candidates), "adoptable": adoptable}, nil
}

func (h *Adoption) runScan(ctx context.Context, scan store.AdoptionScan, rec *queue.StepRecorder) ([]adoption.Candidate, error) {
	server, err := h.Store.GetServerByID(ctx, scan.ServerID)
	if err != nil {
		return nil, err
	}

	rec.Start(ctx, "inventory")
	rt, err := h.Docker.Runtime(ctx, server.ID)
	if err != nil {
		rec.Fail(ctx, "the server's agent is not connected")
		return nil, err
	}
	// The WHOLE inventory, managed or not — the scan's very purpose is the
	// unmanaged remainder.
	list, err := rt.ContainerList(ctx, containertypes.ListOptions{All: true})
	if err != nil {
		rec.Fail(ctx, firstLine(err.Error()))
		return nil, err
	}
	containers := make([]adoption.Inspect, 0, len(list))
	for _, c := range list {
		resp, err := rt.ContainerInspect(ctx, c.ID)
		if err != nil {
			if dockerruntime.IsNotFound(err) {
				continue // removed between the list and the inspect
			}
			rec.Fail(ctx, firstLine(err.Error()))
			return nil, err
		}
		containers = append(containers, adoption.FromInspect(resp))
	}
	rec.Succeed(ctx, fmt.Sprintf("%d containers inspected", len(containers)))

	rec.Start(ctx, "mapping")
	live, err := h.liveResourceUUIDs(ctx, containers)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	input := adoption.ScanInput{
		Containers:        containers,
		ImageEnv:          h.imageEnvs(ctx, rt, containers, live),
		ComposeFiles:      h.composeFiles(ctx, server, containers, live),
		LiveResourceUUIDs: live,
	}
	candidates := adoption.BuildCandidates(input)
	rec.Succeed(ctx, fmt.Sprintf("%d candidates", len(candidates)))
	return candidates, nil
}

// liveResourceUUIDs resolves which akerdock.resource_uuid labels still have a
// live row — the refinement of INV-015 that makes disown reversible: a
// labelled container whose resource was disowned is adoptable again.
func (h *Adoption) liveResourceUUIDs(ctx context.Context, containers []adoption.Inspect) (map[string]bool, error) {
	var uuids []pgtype.UUID
	seen := map[string]bool{}
	for _, c := range containers {
		if u := c.Config.Labels["akerdock.resource_uuid"]; u != "" && !seen[u] {
			seen[u] = true
			if p := pguuid.MustParse(u); p.Valid {
				uuids = append(uuids, p)
			}
		}
	}
	live := map[string]bool{}
	if len(uuids) == 0 {
		return live, nil
	}
	rows, err := h.Store.ListLiveResourceUUIDs(ctx, uuids)
	if err != nil {
		return nil, err
	}
	for _, u := range rows {
		live[pguuid.String(u)] = true
	}
	return live, nil
}

// imageEnvs fetches the default environment of each image used by an
// unmanaged standalone container, so only the DIFF is adopted. Best-effort:
// a missing image just means every variable is kept (the scan says so).
func (h *Adoption) imageEnvs(ctx context.Context, rt dockerruntime.Runtime, containers []adoption.Inspect, live map[string]bool) map[string][]string {
	images := map[string]bool{}
	for _, c := range containers {
		if c.Config.Labels["com.docker.compose.project"] != "" {
			continue
		}
		if live[c.Config.Labels["akerdock.resource_uuid"]] {
			continue
		}
		images[c.Config.Image] = true
	}
	out := map[string][]string{}
	for img := range images {
		resp, err := rt.ImageInspect(ctx, img)
		if err != nil || resp.Config == nil {
			continue
		}
		out[img] = resp.Config.Env
	}
	return out
}

// composeFiles reads the compose definition of each unmanaged stack from the
// server, via the standard com.docker.compose.project.config_files label.
// The definitions are HOST files: this is the one scan step that still needs
// SSH — dialed only when a stack actually exists.
func (h *Adoption) composeFiles(ctx context.Context, server store.Server, containers []adoption.Inspect, live map[string]bool) map[string]adoption.ComposeFile {
	paths := map[string]string{}
	for _, c := range containers {
		project := c.Config.Labels["com.docker.compose.project"]
		if project == "" || live[c.Config.Labels["akerdock.resource_uuid"]] {
			continue
		}
		if _, done := paths[project]; done {
			continue
		}
		// Several config files (overrides) are possible; the first one is the
		// base definition.
		file, _, _ := strings.Cut(c.Config.Labels["com.docker.compose.project.config_files"], ",")
		paths[project] = strings.TrimSpace(file)
	}
	out := map[string]adoption.ComposeFile{}
	if len(paths) == 0 {
		return out
	}
	client, err := h.dial(ctx, server)
	if err != nil {
		for project := range paths {
			out[project] = adoption.ComposeFile{Err: "server unreachable over SSH: " + firstLine(err.Error())}
		}
		return out
	}
	defer func() { _ = client.Close() }()
	for project, path := range paths {
		if path == "" {
			out[project] = adoption.ComposeFile{}
			continue
		}
		// The path comes from a container label — quoted, never interpolated.
		res, err := client.Run(ctx, "head -c 1048576 "+shellQuote(path))
		switch {
		case err != nil:
			out[project] = adoption.ComposeFile{Err: err.Error()}
		case res.ExitCode != 0:
			out[project] = adoption.ComposeFile{Err: firstLine(res.Stderr)}
		default:
			out[project] = adoption.ComposeFile{Content: res.Stdout}
		}
	}
	return out
}

// ExecuteAdopt takes control of the selected candidates WITHOUT restarting
// them (§20.7 step 4): AkerDock objects are created pointing at the existing
// containers; the first redeployment normalizes. Idempotent: a replayed item
// whose resource already exists is skipped.
func (h *Adoption) ExecuteAdopt(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload AdoptPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	scan, err := h.Store.GetAdoptionScanByID(ctx, payload.ScanID)
	if err != nil {
		return nil, fmt.Errorf("adoption scan vanished: %w", err)
	}
	if scan.Status != store.AdoptionScanStatusCompleted {
		return nil, fmt.Errorf("scan is %s, not completed", scan.Status)
	}
	var candidates []adoption.Candidate
	if err := json.Unmarshal(scan.Candidates, &candidates); err != nil {
		return nil, fmt.Errorf("scan candidates: %w", err)
	}
	byID := map[string]adoption.Candidate{}
	for _, c := range candidates {
		byID[c.ID] = c
	}

	server, err := h.Store.GetServerByID(ctx, scan.ServerID)
	if err != nil {
		return nil, err
	}
	dest, err := h.Store.GetDefaultDestination(ctx, server.ID)
	if err != nil {
		return nil, fmt.Errorf("server has no default destination: %w", err)
	}
	rt, err := h.Docker.Runtime(ctx, server.ID)
	if err != nil {
		return nil, err
	}

	var adopted []string
	var warnings []string
	for _, item := range payload.Items {
		cand, ok := byID[item.CandidateID]
		if !ok {
			return nil, fmt.Errorf("candidate %q is not in the scan", item.CandidateID)
		}
		if !cand.Adoptable {
			return nil, fmt.Errorf("candidate %q is not adoptable: %s", item.CandidateID, strings.Join(cand.Reasons, "; "))
		}
		name := item.Name
		if name == "" {
			name = cand.ProposedName
		}
		rec.Start(ctx, "adopt "+name)
		uuid, warns, err := h.adoptOne(ctx, rt, scan, server, dest, payload.EnvironmentID, cand, name)
		if err != nil {
			rec.Fail(ctx, err.Error())
			return nil, fmt.Errorf("candidate %q: %w", item.CandidateID, err)
		}
		warnings = append(warnings, warns...)
		if uuid == "" {
			rec.Succeed(ctx, name+" already adopted (replay)")
			continue
		}
		adopted = append(adopted, uuid)
		rec.Succeed(ctx, name+" adopted without a restart")
	}
	return map[string]any{"adopted": adopted, "warnings": warnings}, nil
}

// adoptOne creates the AkerDock objects for one candidate, transactionally.
// Returns "" when the resource already exists (idempotent replay).
func (h *Adoption) adoptOne(ctx context.Context, rt dockerruntime.Runtime, scan store.AdoptionScan, _ store.Server, dest store.Destination, envID int64, cand adoption.Candidate, name string) (string, []string, error) {
	// Re-inspect: the scan is a snapshot, the workload must still be there.
	lead := cand.Containers[0]
	probe := lead.ContainerID
	if cand.Kind == "compose_stack" {
		probe = lead.ContainerName
	}
	resp, err := rt.ContainerInspect(ctx, probe)
	if err != nil {
		if dockerruntime.IsNotFound(err) {
			return "", nil, fmt.Errorf("the container %s no longer exists — re-scan before adopting", lead.ContainerName)
		}
		return "", nil, err
	}
	current := adoption.FromInspect(resp)

	// Environment values are captured NOW, encrypted at rest, never stored in
	// the scan (INV-003).
	env := map[string]string{}
	if cand.Kind == "container" {
		var imageDefaults []string
		if ires, err := rt.ImageInspect(ctx, current.Config.Image); err == nil && ires.Config != nil {
			imageDefaults = ires.Config.Env
		}
		env = adoption.AdoptedEnv(current.Config.Env, imageDefaults)
	}

	resourceType := store.ResourceTypeApplication
	if cand.Kind == "compose_stack" {
		resourceType = store.ResourceTypeService
	}

	u, err := pguuid.New()
	if err != nil {
		return "", nil, err
	}
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := h.Store.WithTx(tx)

	resource, err := qtx.CreateResource(ctx, store.CreateResourceParams{
		Uuid: u, TeamID: scan.TeamID, EnvironmentID: envID, DestinationID: dest.ID,
		ResourceType: resourceType, Name: name,
		Description: ptrOf("adopted from " + lead.ContainerName + " (§20.7)"),
	})
	if err != nil {
		if isUniqueViolationErr(err) {
			return "", nil, nil // already adopted by a previous attempt
		}
		return "", nil, err
	}
	resourceUUID := pguuid.String(resource.Uuid)

	var warnings []string
	if cand.Kind == "container" {
		warnings, err = h.adoptContainerRows(ctx, qtx, resource, cand, current, env)
	} else {
		warnings, err = h.adoptStackRows(ctx, qtx, resource, cand)
	}
	if err != nil {
		return "", nil, err
	}

	pointer, _ := json.Marshal(adoption.Pointer{
		ScanUUID:       pguuid.String(scan.Uuid),
		ContainerID:    lead.ContainerID,
		ContainerName:  lead.ContainerName,
		ComposeProject: cand.ComposeProject,
	})
	if err := qtx.SetResourceAdoption(ctx, store.SetResourceAdoptionParams{ID: resource.ID, Adoption: pointer}); err != nil {
		return "", nil, err
	}

	desired, observed := store.ResourceDesiredStatusStopped, store.ResourceObservedStatusExited
	if current.State.Status == "running" {
		desired, observed = store.ResourceDesiredStatusRunning, store.ResourceObservedStatusHealthy
	}
	if err := qtx.SetResourceDesiredStatus(ctx, store.SetResourceDesiredStatusParams{ID: resource.ID, DesiredStatus: desired}); err != nil {
		return "", nil, err
	}
	if err := qtx.SetResourceObservedStatus(ctx, store.SetResourceObservedStatusParams{ID: resource.ID, ObservedStatus: observed}); err != nil {
		return "", nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", nil, err
	}
	h.Logger.Info("resource adopted without a restart", "resource_uuid", resourceUUID, "kind", cand.Kind, "name", name)
	return resourceUUID, warnings, nil
}

// adoptContainerRows maps a standalone container onto application +
// build/runtime config + encrypted variables + storages + domains.
func (h *Adoption) adoptContainerRows(ctx context.Context, qtx *store.Queries, resource store.Resource, cand adoption.Candidate, current adoption.Inspect, env map[string]string) ([]string, error) {
	if err := qtx.CreateApplicationRow(ctx, store.CreateApplicationRowParams{ID: resource.ID, BaseDirectory: "/"}); err != nil {
		return nil, err
	}
	imageName, imageTag := adoption.SplitImageRef(current.Config.Image)
	if err := qtx.CreateBuildConfig(ctx, store.CreateBuildConfigParams{
		ApplicationID: resource.ID, BuildPack: store.BuildPackImage,
		ImageName: &imageName, ImageTag: nilIfEmpty(imageTag),
	}); err != nil {
		return nil, err
	}
	lead := cand.Containers[0]
	var ports []string
	for _, p := range lead.Ports {
		if p.Protocol == "tcp" {
			ports = append(ports, strconv.Itoa(p.ContainerPort))
		}
	}
	var portsExposes *string
	if len(ports) > 0 {
		portsExposes = ptrOf(strings.Join(ports, ","))
	}
	if err := qtx.CreateRuntimeConfig(ctx, store.CreateRuntimeConfigParams{
		ApplicationID: resource.ID, PortsExposes: portsExposes,
	}); err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vu, err := pguuid.New()
		if err != nil {
			return nil, err
		}
		enc, err := h.Keyring.Encrypt("environment_variables", "value_enc", pguuid.String(vu), []byte(env[k]))
		if err != nil {
			return nil, err
		}
		if _, err := qtx.CreateEnvVar(ctx, store.CreateEnvVarParams{
			Uuid: vu, ResourceID: resource.ID, Key: k, ValueEnc: enc,
			IsLiteral: true, IsMultiline: strings.Contains(env[k], "\n"),
		}); err != nil {
			return nil, err
		}
	}

	var warnings []string
	for _, m := range lead.Mounts {
		su, err := pguuid.New()
		if err != nil {
			return nil, err
		}
		params := store.CreateAdoptedStorageParams{
			Uuid: su, ResourceID: resource.ID, MountPath: m.Destination,
		}
		switch m.Kind {
		case "volume":
			params.Kind = store.StorageKindVolume
			params.Name = ptrOf(adoption.SanitizeName(m.Source))
			// The original Docker name: renaming it would remount an empty
			// volume at normalization time (INV-008).
			params.ExternalName = ptrOf(m.Source)
		case "bind":
			params.Kind = store.StorageKindBind
			params.HostPath = ptrOf(m.Source)
		default:
			continue
		}
		if _, err := qtx.CreateAdoptedStorage(ctx, params); err != nil {
			return nil, err
		}
	}

	for _, fqdn := range lead.Domains {
		du, err := pguuid.New()
		if err != nil {
			return nil, err
		}
		var targetPort *int32
		if len(lead.Ports) > 0 {
			targetPort = ptrOf(int32(lead.Ports[0].ContainerPort))
		}
		if _, err := qtx.CreateDomain(ctx, store.CreateDomainParams{
			Uuid: du, ApplicationID: &resource.ID, Fqdn: fqdn, Path: "/",
			TargetPort: targetPort,
		}); err != nil {
			if isUniqueViolationErr(err) {
				warnings = append(warnings, "domain "+fqdn+" is already routed by this instance — not adopted")
				continue
			}
			return nil, err
		}
	}
	return warnings, nil
}

// adoptStackRows maps a compose stack onto service + components + component
// domains. The stored compose content already pins the volumes to their
// current names (external) — that is what the scan produced.
func (h *Adoption) adoptStackRows(ctx context.Context, qtx *store.Queries, resource store.Resource, cand adoption.Candidate) ([]string, error) {
	if err := qtx.CreateApplicationRow(ctx, store.CreateApplicationRowParams{ID: resource.ID, BaseDirectory: "/"}); err != nil {
		return nil, err
	}
	if err := qtx.CreateBuildConfig(ctx, store.CreateBuildConfigParams{
		ApplicationID: resource.ID, BuildPack: store.BuildPackCompose,
	}); err != nil {
		return nil, err
	}
	if err := qtx.CreateRuntimeConfig(ctx, store.CreateRuntimeConfigParams{ApplicationID: resource.ID}); err != nil {
		return nil, err
	}
	if err := qtx.CreateServiceRow(ctx, store.CreateServiceRowParams{
		ID: resource.ID, ComposeContent: cand.ComposeContent,
	}); err != nil {
		return nil, err
	}

	var warnings []string
	for _, cc := range cand.Containers {
		if cc.ComposeService == "" {
			continue
		}
		image := cc.Image
		component, err := qtx.UpsertServiceComponent(ctx, store.UpsertServiceComponentParams{
			ResourceID: resource.ID, Name: cc.ComposeService, Image: &image,
			AccessPublicRoutes: []byte("[]"),
		})
		if err != nil {
			return nil, err
		}
		for _, fqdn := range cc.Domains {
			du, err := pguuid.New()
			if err != nil {
				return nil, err
			}
			var targetPort *int32
			if len(cc.Ports) > 0 {
				targetPort = ptrOf(int32(cc.Ports[0].ContainerPort))
			}
			if _, err := qtx.CreateComponentDomain(ctx, store.CreateComponentDomainParams{
				Uuid: du, ServiceComponentID: &component.ID, Fqdn: fqdn,
				TargetPort: targetPort,
			}); err != nil {
				if isUniqueViolationErr(err) {
					warnings = append(warnings, "domain "+fqdn+" is already routed by this instance — not adopted")
					continue
				}
				return nil, err
			}
		}
	}
	return warnings, nil
}

// ExecuteDisown releases a resource (§20.7 step 5): routing removed, row
// tombstoned — containers, volumes, networks and files stay EXACTLY as they
// are. The reverse of adoption, available to any resource.
func (h *Adoption) ExecuteDisown(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload DisownPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	resource, err := h.Store.GetResourceByID(ctx, payload.ResourceID)
	if err != nil {
		return map[string]any{"status": "already gone"}, nil
	}
	resourceUUID := pguuid.String(resource.Uuid)

	dest, err := h.Store.GetDestinationByID(ctx, resource.DestinationID)
	if err != nil {
		return nil, err
	}
	server, err := h.Store.GetServerByID(ctx, dest.ServerID)
	if err != nil {
		return nil, err
	}

	// Routing is detached first (§20.6 order), and ONLY routing: a disowned
	// workload keeps serving whoever routes to it next.
	if server.ProxyType == store.ProxyTypeTraefik {
		rec.Start(ctx, "detach_routing")
		client, err := h.dial(ctx, server)
		if err != nil {
			rec.Fail(ctx, "SSH connection failed — the server must be reachable to detach the routing; retry once it is back")
			return nil, err
		}
		defer func() { _ = client.Close() }()
		applier := &ProxyApplier{Store: h.Store, Client: client, Server: server, Network: dest.Network}
		if err := applier.Apply(ctx, resourceUUID, "", ""); err != nil {
			rec.Fail(ctx, "could not detach the routing — nothing was released, retry once the proxy is healthy")
			return nil, err
		}
		rec.Succeed(ctx, "routing detached")
	}

	rec.Start(ctx, "release")
	if _, err := h.Store.SoftDeleteResource(ctx, resource.ID); err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	rec.Succeed(ctx, "resource released — remote objects untouched")
	h.Logger.Info("resource disowned", "resource_uuid", resourceUUID)
	return map[string]any{"disowned": resourceUUID}, nil
}

func (h *Adoption) dial(ctx context.Context, server store.Server) (*sshexec.Client, error) {
	key, err := h.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return nil, err
	}
	pem, err := h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return nil, err
	}
	return sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
}

// isUniqueViolationErr reports a PostgreSQL 23505: an adopted resource that
// already exists means a previous attempt committed — the replay skips it.
func isUniqueViolationErr(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
