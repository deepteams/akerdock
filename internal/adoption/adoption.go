// Package adoption maps unmanaged Docker resources onto the AkerDock model
// (PRD §20.7, ADR-013). It is the inbound migration path of the product
// (ADR-023): it only understands standard Docker objects — containers,
// compose stacks, volumes, networks — never the internal schema of whatever
// platform created them.
//
// Everything here is pure: the callers (the scan and adopt jobs) do the SSH
// and database work, this package decides. That is what makes the mapping
// unit-testable without a server.
package adoption

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
)

// Label namespaces. A container carrying any akerdock.* label belongs (or
// belonged) to AkerDock — INV-015 draws the managed/unmanaged boundary there,
// refined by the database: only a LIVE resource row makes it truly managed.
const (
	labelPrefix       = "akerdock."
	labelResourceUUID = "akerdock.resource_uuid"
	labelType         = "akerdock.type"

	composeProjectLabel     = "com.docker.compose.project"
	composeServiceLabel     = "com.docker.compose.service"
	composeWorkingDirLabel  = "com.docker.compose.project.working_dir"
	composeConfigFilesLabel = "com.docker.compose.project.config_files"
)

// Pointer is the resources.adoption JSONB: while an adopted resource has not
// been normalized by its first deployment, it points at the real remote
// objects instead of the uuid-derived names (§20.7 step 4).
type Pointer struct {
	ScanUUID       string `json:"scan_uuid,omitempty"`
	ContainerID    string `json:"container_id,omitempty"`
	ContainerName  string `json:"container_name,omitempty"`
	ComposeProject string `json:"compose_project,omitempty"`
}

// ParsePointer decodes resources.adoption; nil when the resource is not
// awaiting normalization (column NULL or empty).
func ParsePointer(raw []byte) *Pointer {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var p Pointer
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	if p.ContainerName == "" && p.ComposeProject == "" {
		return nil
	}
	return &p
}

// ContainerName is the Docker name lifecycle/logs/terminal must target:
// the adopted container until normalization, the uuid-derived name after.
func ContainerName(adoption []byte, fallback string) string {
	if p := ParsePointer(adoption); p != nil && p.ContainerName != "" {
		return p.ContainerName
	}
	return fallback
}

// Inspect is the subset of a container inspection this package reads. The
// JSON tags mirror the Engine API's inspect shape (what `docker inspect`
// prints); production code builds it from the SDK response with FromInspect,
// the fixtures and older scans decode it with ParseInspects.
type Inspect struct {
	ID              string                 `json:"Id"`
	Name            string                 `json:"Name"`
	State           InspectState           `json:"State"`
	Config          InspectConfig          `json:"Config"`
	HostConfig      InspectHostConfig      `json:"HostConfig"`
	Mounts          []InspectMount         `json:"Mounts"`
	NetworkSettings InspectNetworkSettings `json:"NetworkSettings"`
}

// InspectState is the container's runtime state slice.
type InspectState struct {
	Status string `json:"Status"`
}

// InspectConfig is the container-config slice.
type InspectConfig struct {
	Image        string              `json:"Image"`
	Env          []string            `json:"Env"`
	Labels       map[string]string   `json:"Labels"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
}

// InspectHostConfig is the host-config slice — what containerBlockers reads.
type InspectHostConfig struct {
	NetworkMode  string                          `json:"NetworkMode"`
	Privileged   bool                            `json:"Privileged"`
	CapAdd       []string                        `json:"CapAdd"`
	Devices      []InspectDevice                 `json:"Devices"`
	PortBindings map[string][]InspectPortBinding `json:"PortBindings"`
}

// InspectDevice is one mounted host device.
type InspectDevice struct {
	PathOnHost string `json:"PathOnHost"`
}

// InspectPortBinding is one host binding of an exposed port.
type InspectPortBinding struct {
	HostPort string `json:"HostPort"`
}

// InspectMount is one mount point.
type InspectMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

// InspectNetworkSettings is the networks slice.
type InspectNetworkSettings struct {
	Networks map[string]InspectNetwork `json:"Networks"`
}

// InspectNetwork is one network endpoint.
type InspectNetwork struct {
	IPAddress string `json:"IPAddress"`
}

// ParseInspects decodes the JSON array printed by `docker inspect`.
func ParseInspects(raw string) ([]Inspect, error) {
	var out []Inspect
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("adoption: docker inspect output: %w", err)
	}
	return out, nil
}

// FromInspect maps the SDK's inspect response onto the subset this package
// reads — the typed twin of ParseInspects, fed by the agent channel
// (ADR-051) instead of CLI output.
func FromInspect(resp container.InspectResponse) Inspect {
	var out Inspect
	if base := resp.ContainerJSONBase; base != nil {
		out.ID = base.ID
		out.Name = base.Name
		if base.State != nil {
			out.State.Status = base.State.Status
		}
		if hc := base.HostConfig; hc != nil {
			out.HostConfig.NetworkMode = string(hc.NetworkMode)
			out.HostConfig.Privileged = hc.Privileged
			out.HostConfig.CapAdd = []string(hc.CapAdd)
			for _, d := range hc.Devices {
				out.HostConfig.Devices = append(out.HostConfig.Devices, InspectDevice{PathOnHost: d.PathOnHost})
			}
			if len(hc.PortBindings) > 0 {
				out.HostConfig.PortBindings = map[string][]InspectPortBinding{}
				for spec, bindings := range hc.PortBindings {
					for _, b := range bindings {
						out.HostConfig.PortBindings[string(spec)] = append(out.HostConfig.PortBindings[string(spec)], InspectPortBinding{HostPort: b.HostPort})
					}
				}
			}
		}
	}
	if cfg := resp.Config; cfg != nil {
		out.Config.Image = cfg.Image
		out.Config.Env = cfg.Env
		out.Config.Labels = cfg.Labels
		if len(cfg.ExposedPorts) > 0 {
			out.Config.ExposedPorts = map[string]struct{}{}
			for p := range cfg.ExposedPorts {
				out.Config.ExposedPorts[string(p)] = struct{}{}
			}
		}
	}
	for _, m := range resp.Mounts {
		out.Mounts = append(out.Mounts, InspectMount{
			Type: string(m.Type), Name: m.Name, Source: m.Source, Destination: m.Destination,
		})
	}
	if resp.NetworkSettings != nil && len(resp.NetworkSettings.Networks) > 0 {
		out.NetworkSettings.Networks = map[string]InspectNetwork{}
		for name, ep := range resp.NetworkSettings.Networks {
			var ip string
			if ep != nil {
				ip = ep.IPAddress
			}
			out.NetworkSettings.Networks[name] = InspectNetwork{IPAddress: ip}
		}
	}
	return out
}

// Candidate is one adoptable (or not) unit: a standalone container, or a
// whole compose stack. JSON tags match the API schema — the scan stores the
// candidates verbatim and the GET serves them back. compose_content and
// compose_working_dir are scan-internal (used by the adopt job) and are NOT
// part of the API schema: the handler drops them on the way out.
type Candidate struct {
	ID                   string      `json:"id"`
	Kind                 string      `json:"kind"` // container | compose_stack
	ProposedName         string      `json:"proposed_name"`
	ProposedResourceType string      `json:"proposed_resource_type,omitempty"` // application | service
	ComposeProject       string      `json:"compose_project,omitempty"`
	Adoptable            bool        `json:"adoptable"`
	Reasons              []string    `json:"reasons,omitempty"`
	Modifications        []string    `json:"modifications,omitempty"`
	Containers           []Container `json:"containers"`

	ComposeContent    string `json:"compose_content,omitempty"`
	ComposeWorkingDir string `json:"compose_working_dir,omitempty"`
}

// Container is one inspected container inside a candidate. Env carries the
// variable NAMES only — never the values (INV-003): values are captured and
// envelope-encrypted at adoption time, not stored in the scan.
type Container struct {
	ContainerID    string `json:"container_id"`
	ContainerName  string `json:"container_name"`
	Image          string `json:"image"`
	State          string `json:"state"`
	ComposeService string `json:"compose_service,omitempty"`
	// Labels (akerdock.* excluded) let a migration tool recognize the
	// workloads of a given platform (coolify.*, …) without knowing anything
	// about its internal schema (ADR-023).
	Labels   map[string]string `json:"labels,omitempty"`
	EnvKeys  []string          `json:"env_keys,omitempty"`
	Ports    []Port            `json:"ports,omitempty"`
	Mounts   []Mount           `json:"mounts,omitempty"`
	Networks []string          `json:"networks,omitempty"`
	Domains  []string          `json:"domains,omitempty"`
}

// Port is an exposed container port and its optional host binding.
type Port struct {
	ContainerPort int    `json:"container_port"`
	HostPort      *int   `json:"host_port,omitempty"`
	Protocol      string `json:"protocol"`
}

// Mount is a named volume or bind mount of an adopted container.
type Mount struct {
	Kind        string `json:"kind"` // volume | bind
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// ComposeFile is what the scan could read for one compose project.
type ComposeFile struct {
	Content string
	Err     string // non-empty when the file could not be read
}

// ScanInput is everything BuildCandidates needs.
type ScanInput struct {
	Containers []Inspect
	// ImageEnv maps an image reference to its default environment: only the
	// DIFF is adopted, so image defaults are not frozen into explicit
	// variables. A missing entry keeps every variable, with a note.
	ImageEnv map[string][]string
	// ComposeFiles maps a compose project name to its file, read from the
	// server. A stack without a readable file is not adoptable (§20.7: never
	// silently partial).
	ComposeFiles map[string]ComposeFile
	// LiveResourceUUIDs are the akerdock.resource_uuid values that still have
	// a live row: their containers are managed (INV-015). A labelled
	// container WITHOUT a live row was disowned — adoptable again.
	LiveResourceUUIDs map[string]bool
}

// BuildCandidates turns a raw inventory into the proposed mapping (§20.7
// steps 1–3). Deterministic: candidates sorted by id.
func BuildCandidates(in ScanInput) []Candidate {
	var out []Candidate
	stacks := map[string][]Inspect{}

	for _, c := range in.Containers {
		if managed(c, in.LiveResourceUUIDs) {
			continue
		}
		if project := c.Config.Labels[composeProjectLabel]; project != "" {
			stacks[project] = append(stacks[project], c)
			continue
		}
		out = append(out, containerCandidate(c, in.ImageEnv))
	}

	projects := make([]string, 0, len(stacks))
	for p := range stacks {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	for _, p := range projects {
		out = append(out, stackCandidate(p, stacks[p], in.ComposeFiles[p]))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// managed applies INV-015: an akerdock proxy, or a container whose
// akerdock.resource_uuid still has a live row, is off limits. A labelled
// container without a live row was disowned and is a candidate again.
func managed(c Inspect, live map[string]bool) bool {
	labels := c.Config.Labels
	if labels[labelType] == "proxy" {
		return true
	}
	if u := labels[labelResourceUUID]; u != "" {
		return live[u]
	}
	for k := range labels {
		if strings.HasPrefix(k, labelPrefix) {
			// akerdock-labelled but unidentifiable: never touch it.
			return true
		}
	}
	return false
}

func containerCandidate(c Inspect, imageEnv map[string][]string) Candidate {
	cand := Candidate{
		ID:                   shortID(c.ID),
		Kind:                 "container",
		ProposedName:         SanitizeName(strings.TrimPrefix(c.Name, "/")),
		ProposedResourceType: "application",
		Adoptable:            true,
	}
	cc, notes := describeContainer(c, imageEnv)
	cand.Containers = []Container{cc}
	cand.Modifications = append(cand.Modifications, notes...)

	for _, r := range containerBlockers(c) {
		cand.Adoptable = false
		cand.Reasons = append(cand.Reasons, r)
	}
	if cand.Adoptable {
		cand.Modifications = append(cand.Modifications,
			"adoption: aucun changement sur le container — enregistrement côté AkerDock seulement",
			"première normalisation (redéploiement) : labels akerdock.*, nom et réseau AkerDock ; volumes repris sous leur nom d'origine")
		if strings.HasPrefix(c.Config.Image, "sha256:") {
			cand.Modifications = append(cand.Modifications,
				"image locale (absente d'un registry) : le redéploiement normalisateur échouera tant qu'elle n'est pas poussée dans un registry accessible")
		}
	}
	return cand
}

func stackCandidate(project string, members []Inspect, file ComposeFile) Candidate {
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	cand := Candidate{
		ID:                   "compose:" + project,
		Kind:                 "compose_stack",
		ProposedName:         SanitizeName(project),
		ProposedResourceType: "service",
		ComposeProject:       project,
		Adoptable:            true,
		ComposeWorkingDir:    members[0].Config.Labels[composeWorkingDirLabel],
	}
	for _, m := range members {
		cc, _ := describeContainer(m, nil)
		cc.ComposeService = m.Config.Labels[composeServiceLabel]
		cand.Containers = append(cand.Containers, cc)
		for _, r := range containerBlockers(m) {
			cand.Adoptable = false
			cand.Reasons = append(cand.Reasons, cc.ContainerName+": "+r)
		}
	}

	switch {
	case file.Err != "":
		cand.Adoptable = false
		cand.Reasons = append(cand.Reasons, "fichier compose illisible: "+file.Err)
	case strings.TrimSpace(file.Content) == "":
		cand.Adoptable = false
		cand.Reasons = append(cand.Reasons,
			"fichier compose introuvable sur le serveur (label "+composeConfigFilesLabel+" absent ou fichier disparu) — un stack sans sa définition n'est pas représentable")
	default:
		rewritten, err := RewriteComposeExternalVolumes(file.Content, project)
		if err != nil {
			cand.Adoptable = false
			cand.Reasons = append(cand.Reasons, "fichier compose invalide: "+err.Error())
		} else {
			cand.ComposeContent = rewritten
			cand.Modifications = append(cand.Modifications,
				"volumes nommés déclarés `external` sous leur nom actuel — les données survivent au redéploiement (INV-008)")
		}
	}
	if cand.Adoptable {
		cand.Modifications = append(cand.Modifications,
			"adoption: aucun changement sur les containers — enregistrement côté AkerDock seulement",
			"première normalisation (redéploiement) : les containers du projet compose d'origine sont arrêtés puis remplacés par les containers AkerDock (brève interruption), volumes conservés",
			"variables du stack: reprises du fichier compose, pas des containers")
	}
	return cand
}

// containerBlockers lists what the AkerDock model cannot represent (§20.7:
// signalled with a reason, never silently dropped at normalization time).
func containerBlockers(c Inspect) []string {
	var reasons []string
	if c.HostConfig.Privileged {
		reasons = append(reasons, "container privilégié — non représentable dans le modèle, la normalisation le retirerait silencieusement")
	}
	if len(c.HostConfig.CapAdd) > 0 {
		reasons = append(reasons, "capabilities ajoutées ("+strings.Join(c.HostConfig.CapAdd, ", ")+") — non représentables dans le modèle")
	}
	if len(c.HostConfig.Devices) > 0 {
		reasons = append(reasons, "devices hôte montés — non représentables dans le modèle")
	}
	if strings.HasPrefix(c.HostConfig.NetworkMode, "container:") {
		reasons = append(reasons, "network_mode container:<autre> — non représentable dans le modèle")
	}
	return reasons
}

// describeContainer extracts the API-facing description. Env values never
// leave this function: only names, and only those not already defaulted by
// the image (adopting image defaults would freeze them forever).
func describeContainer(c Inspect, imageEnv map[string][]string) (Container, []string) {
	var notes []string
	cc := Container{
		ContainerID:   shortID(c.ID),
		ContainerName: strings.TrimPrefix(c.Name, "/"),
		Image:         c.Config.Image,
		State:         c.State.Status,
	}
	for k, v := range c.Config.Labels {
		if strings.HasPrefix(k, labelPrefix) {
			continue // stale akerdock labels on a disowned container: noise
		}
		if cc.Labels == nil {
			cc.Labels = map[string]string{}
		}
		cc.Labels[k] = v
	}

	base, haveBase := map[string]bool{}, false
	if imageEnv != nil {
		if defaults, ok := imageEnv[c.Config.Image]; ok {
			haveBase = true
			for _, kv := range defaults {
				base[envKey(kv)] = true
			}
		} else {
			notes = append(notes, cc.ContainerName+": environnement de l'image indisponible — toutes les variables du container seront adoptées, défauts d'image compris")
		}
	}
	for _, kv := range c.Config.Env {
		k := envKey(kv)
		if haveBase && base[k] && !changedFromImage(kv, imageEnv[c.Config.Image]) {
			continue
		}
		cc.EnvKeys = append(cc.EnvKeys, k)
	}
	sort.Strings(cc.EnvKeys)

	for spec := range c.Config.ExposedPorts {
		p := parsePortSpec(spec)
		if p == nil {
			continue
		}
		if bindings := c.HostConfig.PortBindings[spec]; len(bindings) > 0 {
			if hp, err := strconv.Atoi(bindings[0].HostPort); err == nil && hp > 0 {
				p.HostPort = &hp
			}
		}
		cc.Ports = append(cc.Ports, *p)
	}
	sort.Slice(cc.Ports, func(i, j int) bool { return cc.Ports[i].ContainerPort < cc.Ports[j].ContainerPort })

	for _, m := range c.Mounts {
		switch m.Type {
		case "volume":
			cc.Mounts = append(cc.Mounts, Mount{Kind: "volume", Source: m.Name, Destination: m.Destination})
		case "bind":
			cc.Mounts = append(cc.Mounts, Mount{Kind: "bind", Source: m.Source, Destination: m.Destination})
		}
	}
	sort.Slice(cc.Mounts, func(i, j int) bool { return cc.Mounts[i].Destination < cc.Mounts[j].Destination })

	for name := range c.NetworkSettings.Networks {
		cc.Networks = append(cc.Networks, name)
	}
	sort.Strings(cc.Networks)

	cc.Domains = DomainsFromLabels(c.Config.Labels)
	return cc, notes
}

// changedFromImage reports whether KEY=VALUE differs from the image default
// for the same key — a changed default is user intent and must be adopted.
func changedFromImage(kv string, defaults []string) bool {
	k := envKey(kv)
	for _, d := range defaults {
		if envKey(d) == k {
			return d != kv
		}
	}
	return true
}

func envKey(kv string) string {
	k, _, _ := strings.Cut(kv, "=")
	return k
}

// AdoptedEnv returns the KEY→VALUE map to adopt at adoption time: the
// container's environment minus the image defaults it did not change. With
// no image defaults available, everything is adopted (the scan warned).
func AdoptedEnv(env, imageDefaults []string) map[string]string {
	base := map[string]string{}
	for _, kv := range imageDefaults {
		k, v, _ := strings.Cut(kv, "=")
		base[k] = v
	}
	out := map[string]string{}
	for _, kv := range env {
		k, v, found := strings.Cut(kv, "=")
		if k == "" || !found {
			continue
		}
		if def, ok := base[k]; ok && def == v {
			continue
		}
		out[k] = v
	}
	return out
}

// SplitImageRef splits a Docker image reference into build_config's
// (image_name, image_tag). A digest reference stays whole in the name — the
// engine deploys it verbatim, which is exactly what a digest is for.
func SplitImageRef(ref string) (name, tag string) {
	if strings.Contains(ref, "@") {
		return ref, ""
	}
	slash := strings.LastIndexByte(ref, '/')
	if colon := strings.LastIndexByte(ref, ':'); colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}

func parsePortSpec(spec string) *Port {
	parts := strings.SplitN(spec, "/", 2)
	n, err := strconv.Atoi(parts[0])
	if err != nil || n <= 0 {
		return nil
	}
	proto := "tcp"
	if len(parts) == 2 && parts[1] != "" {
		proto = parts[1]
	}
	return &Port{ContainerPort: n, Protocol: proto}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// hostRuleRe extracts the backticked arguments of Traefik Host(`…`) rules.
var hostRuleRe = regexp.MustCompile(`Host\(([^)]*)\)`)

// backtickArgRe matches one backticked argument inside a rule.
var backtickArgRe = regexp.MustCompile("`([^`]+)`")

// DomainsFromLabels detects the FQDNs an existing reverse proxy already
// routes to the container: Traefik router rules, and the bare `caddy` label.
// Detection only — the adoption records them, the first normalizing deploy
// routes them (§20.7 step 2).
func DomainsFromLabels(labels map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(fqdn string) {
		fqdn = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(fqdn, "https://"), "http://"))
		fqdn = strings.SplitN(fqdn, "/", 2)[0]
		if fqdn == "" || strings.ContainsAny(fqdn, " \t") || seen[fqdn] {
			return
		}
		seen[fqdn] = true
		out = append(out, fqdn)
	}

	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := labels[k]
		switch {
		case strings.HasPrefix(k, "traefik.http.routers.") && strings.HasSuffix(k, ".rule"):
			for _, rule := range hostRuleRe.FindAllStringSubmatch(v, -1) {
				for _, arg := range backtickArgRe.FindAllStringSubmatch(rule[1], -1) {
					add(arg[1])
				}
			}
		case k == "caddy":
			for _, part := range strings.Fields(v) {
				if strings.Contains(part, ".") {
					add(part)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

var nameSanitizeRe = regexp.MustCompile(`[^a-z0-9-]+`)

// SanitizeName maps an arbitrary Docker name onto the resource-name grammar
// (INV-012): lowercase, alphanumerics and dashes.
func SanitizeName(name string) string {
	s := nameSanitizeRe.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "adopted"
	}
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return s
}
