package compose

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

// Plan is the deterministic execution plan of a stack (compose-spec §2):
// what the deployment engine creates on the server, in which order, under
// which names. Same file + same input = same plan (INV-011, INV-014).
type Plan struct {
	StackUUID string
	// NetworkName is the isolated bridge network of the stack (§2.1).
	NetworkName string
	// ExtraNetworks are the additional file-declared networks, prefixed
	// (§2.1): docker name -> declared name.
	ExtraNetworks map[string]string
	// Volumes maps declared named volumes to their docker names (§2.4).
	Volumes map[string]string
	// SeedVolumes maps the docker name of a volume declaring
	// `x-akerdock: preview_seed: clone` to its DECLARED name (ADR-029): a
	// preview deployment seeds it, still empty, from the production volume
	// `<app-uuid>_<declared>` before the mounting service first starts.
	SeedVolumes map[string]string
	// ExternalVolumes maps `external: true` volumes to their real docker
	// names: mounted verbatim, never created, never prefixed. This is how an
	// adopted stack keeps its data across the normalizing redeployment
	// (§20.7, INV-008).
	ExternalVolumes map[string]string
	// Services in topological start order (§2.6).
	Services []ServicePlan
	// Canonical is the transformed compose, traced in deployment logs (§2).
	Canonical string
}

// Dependency is one edge of the ordering plan (§2.6).
type Dependency struct {
	Service   string
	Condition string
}

// MountPlan is one container mount after rewriting (§2.4).
type MountPlan struct {
	Type     string // volume | bind | tmpfs
	Source   string // docker volume name, or host path for binds
	Target   string
	ReadOnly bool
	// Ext carries the managed file/directory extensions (§5.1), nil if none.
	Ext *VolumeExtensions
}

// HealthFlags maps compose healthcheck to docker create flags (§7.1),
// defaults filled.
type HealthFlags struct {
	Test          []string
	Interval      time.Duration
	Timeout       time.Duration
	StartPeriod   time.Duration
	StartInterval time.Duration
	Retries       uint64
	Disable       bool
}

// LimitFlags are the normalized resource limits (§8.5) — deploy.resources
// and legacy keys reduced to one set of docker create flags.
type LimitFlags struct {
	Memory            int64
	MemoryReservation int64
	MemorySwap        int64
	CPUs              float64
	CPUShares         int64
	CPUSet            string
	Pids              int64
}

// ServicePlan is one compose service, transformed. The full canonical config
// stays available in Service for the flags this plan does not precompute
// (user, working_dir, dns…): the engine reads them from there.
type ServicePlan struct {
	Name          string
	ContainerName string
	// CandidateName is the zero-downtime candidate (§2.2).
	CandidateName string
	// Aliases on the stack network (§2.1): short service name + prefixed.
	Aliases []string
	// ExtraNetworks this service attaches to (docker names).
	ExtraNetworks []string
	Image         string
	// Build is true when the service builds from the clone (§1.3).
	Build bool
	// BuildImage is the local image name for built services (§2.2), tag
	// applied at deploy time with the commit sha.
	BuildImage string
	Mounts     []MountPlan
	DependsOn  []Dependency
	Restart    string
	// OneShot marks restart:no jobs (§7.3) — run at their topological
	// position, success required.
	OneShot bool
	// Pre/PostCommand are the per-service hooks (x-akerdock, §10 semantics):
	// pre in the existing container before any mutation, post in the healthy
	// candidate before its switch.
	PreCommand         string
	PostCommand        string
	ExcludeFromHC      bool
	ZeroDowntimeOptOut bool
	// HasHostPorts makes the service ineligible to zero-downtime (§8.4):
	// two instances cannot bind the same host port.
	HasHostPorts bool
	// DefaultRoutePort is the first exposed port (§6) — the routing default.
	DefaultRoutePort int
	Health           *HealthFlags
	Limits           LimitFlags
	// IsDatabase/DatabaseEngine come from image detection (§10).
	IsDatabase     bool
	DatabaseEngine string
	Service        types.ServiceConfig
}

// buildPlan turns a validated project into the execution plan (§2).
func buildPlan(project *types.Project, in Input, fs *findings) (*Plan, error) {
	plan := &Plan{
		StackUUID:       in.StackUUID,
		NetworkName:     in.StackUUID,
		ExtraNetworks:   map[string]string{},
		Volumes:         map[string]string{},
		ExternalVolumes: map[string]string{},
		SeedVolumes:     map[string]string{},
	}

	for name, network := range project.Networks {
		if name == "default" || bool(network.External) {
			continue // default = the stack network; external = unmanaged
		}
		dockerName := in.StackUUID + "_" + name
		if in.Raw {
			dockerName = name // raw mode: compose project-name semantics (§9)
		}
		plan.ExtraNetworks[dockerName] = name
	}
	for name, volume := range project.Volumes {
		seed, seedDeclared := "", false
		if raw := extensionMap(volume.Extensions); raw != nil {
			if v, ok := raw["preview_seed"]; ok {
				seedDeclared = true
				seed, _ = v.(string)
			}
		}
		if bool(volume.External) {
			// External = pre-existing: mounted under its real name (the
			// explicit `name:` or the key), never created or prefixed.
			if seedDeclared {
				// An external volume IS production (adoption §20.7): there is
				// no per-preview copy of it to seed.
				fs.errf(CodePreviewSeedInvalid, "", "volumes."+name+".x-akerdock.preview_seed",
					"preview_seed does not apply to an external volume — it is the production object itself (ADR-029)")
			}
			dockerName := volume.Name
			if dockerName == "" {
				dockerName = name
			}
			plan.ExternalVolumes[name] = dockerName
			continue
		}
		dockerName := in.StackUUID + "_" + name
		if in.Raw {
			dockerName = name
		}
		plan.Volumes[name] = dockerName
		switch {
		case !seedDeclared:
		case seed != "clone":
			fs.errf(CodePreviewSeedInvalid, "", "volumes."+name+".x-akerdock.preview_seed",
				"preview_seed only accepts \"clone\" (ADR-029)")
		case in.Raw:
			// Raw mode keeps compose project-name semantics: the preview and
			// the production stack would designate the SAME volume — there is
			// nothing to clone from, only production to corrupt.
			fs.errf(CodePreviewSeedInvalid, "", "volumes."+name+".x-akerdock.preview_seed",
				"preview_seed is not available in raw compose mode — volume names are not prefixed there (ADR-029, §9)")
		default:
			plan.SeedVolumes[dockerName] = name
		}
	}

	order, ok := topologicalOrder(project)
	if !ok {
		// findCycle reported the precise cycle during validation already.
		return nil, fmt.Errorf("compose: dependency cycle survived validation")
	}

	for _, name := range order {
		svc := project.Services[name]
		plan.Services = append(plan.Services, buildServicePlan(name, svc, project, in, plan, fs))
	}

	canonical, err := project.MarshalYAML()
	if err != nil {
		return nil, fmt.Errorf("compose: canonical form: %w", err)
	}
	plan.Canonical = string(canonical)
	return plan, nil
}

func buildServicePlan(name string, svc types.ServiceConfig, _ *types.Project, in Input, plan *Plan, fs *findings) ServicePlan {
	p := "services." + name
	ext := serviceExtensions(name, p+".x-akerdock", svc, &findings{}) // findings already reported by validate

	sp := ServicePlan{
		Name:          name,
		ContainerName: in.StackUUID + "-" + name,
		CandidateName: in.StackUUID + "-" + name + "-next",
		Aliases:       []string{name, in.StackUUID + "-" + name},
		Image:         svc.Image,
		ExcludeFromHC: ext.ExcludeFromHC,
		PreCommand:    ext.PreDeploymentCommand,
		PostCommand:   ext.PostDeploymentCommand,
		Service:       svc,
	}
	if ext.ZeroDowntime != nil && !*ext.ZeroDowntime {
		sp.ZeroDowntimeOptOut = true
	}

	// Restart policy (§2.5): absent means unless-stopped, `no` marks one-shots.
	sp.Restart = svc.Restart
	if sp.Restart == "" && !in.Raw {
		sp.Restart = types.RestartPolicyUnlessStopped
	}
	sp.OneShot = svc.Restart == types.RestartPolicyNo
	if sp.OneShot && !sp.ExcludeFromHC {
		fs.warnf(CodeOneshotWithoutExclude, name, p+".restart", "restart: no without x-akerdock.exclude_from_hc: the one-shot job will drag the stack health down — add the extension")
	}

	// Source: prebuilt image or build from the clone (§1.3).
	if svc.Build != nil {
		sp.Build = true
		sp.BuildImage = "akerdock/" + in.StackUUID + "-" + name
	}

	// Networks (§2.1): extra file-declared networks this service joins.
	for netName := range svc.Networks {
		if netName == "default" {
			continue
		}
		for dockerName, declared := range plan.ExtraNetworks {
			if declared == netName {
				sp.ExtraNetworks = append(sp.ExtraNetworks, dockerName)
			}
		}
	}
	sort.Strings(sp.ExtraNetworks)

	// Mounts (§2.4): named volumes rewritten coherently across services.
	for i, vol := range svc.Volumes {
		mount := MountPlan{Type: vol.Type, Source: vol.Source, Target: vol.Target, ReadOnly: vol.ReadOnly}
		if vol.Type == types.VolumeTypeVolume && vol.Source != "" {
			if dockerName, ok := plan.Volumes[vol.Source]; ok {
				mount.Source = dockerName
			} else if dockerName, ok := plan.ExternalVolumes[vol.Source]; ok {
				mount.Source = dockerName
			}
		}
		mount.Ext = volumeExtensions(name, fmt.Sprintf("%s.volumes[%d]", p, i), vol, &findings{})
		sp.Mounts = append(sp.Mounts, mount)
	}

	// Ordering (§2.6) — deterministic edge order.
	for dep, cfg := range svc.DependsOn {
		sp.DependsOn = append(sp.DependsOn, Dependency{Service: dep, Condition: cfg.Condition})
	}
	sort.Slice(sp.DependsOn, func(i, j int) bool { return sp.DependsOn[i].Service < sp.DependsOn[j].Service })

	// Host port mappings (§8.4).
	sp.HasHostPorts = len(svc.Ports) > 0
	if len(svc.Expose) > 0 {
		_, _ = fmt.Sscanf(svc.Expose[0], "%d", &sp.DefaultRoutePort)
	}

	sp.Health = healthFlags(svc.HealthCheck)
	sp.Limits = limitFlags(svc)
	sp.IsDatabase, sp.DatabaseEngine = detectDatabase(svc.Image)
	return sp
}

// healthFlags maps the compose healthcheck onto docker create flags with the
// Compose Specification defaults (§7.1).
func healthFlags(hc *types.HealthCheckConfig) *HealthFlags {
	if hc == nil {
		return nil
	}
	out := &HealthFlags{
		Test:          hc.Test,
		Interval:      30 * time.Second,
		Timeout:       30 * time.Second,
		StartPeriod:   0,
		StartInterval: 5 * time.Second,
		Retries:       3,
		Disable:       hc.Disable,
	}
	if hc.Interval != nil {
		out.Interval = time.Duration(*hc.Interval)
	}
	if hc.Timeout != nil {
		out.Timeout = time.Duration(*hc.Timeout)
	}
	if hc.StartPeriod != nil {
		out.StartPeriod = time.Duration(*hc.StartPeriod)
	}
	if hc.StartInterval != nil {
		out.StartInterval = time.Duration(*hc.StartInterval)
	}
	if hc.Retries != nil {
		out.Retries = *hc.Retries
	}
	return out
}

// limitFlags normalizes deploy.resources and the legacy keys into one set of
// flags (§8.5) — really applied, never ignored. Conflicts were refused in
// validation; here the modern form wins when both agree.
func limitFlags(svc types.ServiceConfig) LimitFlags {
	out := LimitFlags{
		Memory:            int64(svc.MemLimit),
		MemoryReservation: int64(svc.MemReservation),
		MemorySwap:        int64(svc.MemSwapLimit),
		CPUs:              float64(svc.CPUS),
		CPUShares:         svc.CPUShares,
		CPUSet:            svc.CPUSet,
	}
	if svc.Deploy != nil {
		if limits := svc.Deploy.Resources.Limits; limits != nil {
			if limits.MemoryBytes != 0 {
				out.Memory = int64(limits.MemoryBytes)
			}
			if limits.NanoCPUs != 0 {
				out.CPUs = float64(limits.NanoCPUs)
			}
			if limits.Pids != 0 {
				out.Pids = limits.Pids
			}
		}
		if res := svc.Deploy.Resources.Reservations; res != nil && res.MemoryBytes != 0 {
			out.MemoryReservation = int64(res.MemoryBytes)
		}
	}
	return out
}

// databaseImages maps image basenames to backupable engines (§10) — the
// list follows the catalogue.
var databaseImages = map[string]string{
	"postgres": "postgresql", "postgresql": "postgresql", "pgvector": "postgresql",
	"mysql": "mysql", "percona": "mysql", "percona-server": "mysql",
	"mariadb": "mariadb",
	"mongo":   "mongodb", "mongodb": "mongodb", "mongodb-community-server": "mongodb",
}

// detectDatabase classifies a service by image basename — registry,
// namespace, tag and digest ignored (§10).
func detectDatabase(image string) (bool, string) {
	if image == "" {
		return false, ""
	}
	base := image
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.IndexAny(base, ":@"); i >= 0 {
		base = base[:i]
	}
	engine, ok := databaseImages[strings.ToLower(base)]
	return ok, engine
}

// topologicalOrder returns the start order of the services (Kahn, §2.6),
// names sorted at each rank so the order is deterministic.
func topologicalOrder(project *types.Project) ([]string, bool) {
	indegree := map[string]int{}
	dependents := map[string][]string{}
	for name := range project.Services {
		indegree[name] += 0
	}
	for name, svc := range project.Services {
		for dep := range svc.DependsOn {
			if _, ok := project.Services[dep]; !ok {
				continue
			}
			indegree[name]++
			dependents[dep] = append(dependents[dep], name)
		}
	}

	var ready []string
	for name, deg := range indegree {
		if deg == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)

	var order []string
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		next := dependents[name]
		sort.Strings(next)
		for _, dependent := range next {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Strings(ready)
			}
		}
	}
	return order, len(order) == len(project.Services)
}

// findCycle names one dependency cycle for the error message (§2.6).
func findCycle(project *types.Project) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var cycle []string

	var visit func(string) bool
	visit = func(name string) bool {
		color[name] = gray
		stack = append(stack, name)
		svc := project.Services[name]
		deps := make([]string, 0, len(svc.DependsOn))
		for dep := range svc.DependsOn {
			deps = append(deps, dep)
		}
		sort.Strings(deps)
		for _, dep := range deps {
			if _, ok := project.Services[dep]; !ok {
				continue
			}
			switch color[dep] {
			case white:
				if visit(dep) {
					return true
				}
			case gray:
				for i, on := range stack {
					if on == dep {
						cycle = append(append([]string{}, stack[i:]...), dep)
						return true
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[name] = black
		return false
	}

	names := make([]string, 0, len(project.Services))
	for name := range project.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if color[name] == white && visit(name) {
			return cycle
		}
	}
	return nil
}
