package compose

import (
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// serviceNamePattern is the accepted compose service name (§2.2): the name
// ends up in container and DNS names, so it is validated, not sanitized.
var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// defaultCapAllowlist (§1.4): capabilities a container may add without a
// server policy.
var defaultCapAllowlist = []string{"NET_BIND_SERVICE", "CHOWN", "SETUID", "SETGID"}

// validate applies the key policy of compose-spec §1.3–§1.5 on the loaded
// project. Every refusal carries its stable code; nothing is fixed silently.
func validate(project *types.Project, in Input, fs *findings) {
	for name, svc := range project.Services {
		p := "services." + name
		if !serviceNamePattern.MatchString(name) {
			fs.errf(CodeInvalidServiceName, name, p, "service name must match [a-z0-9][a-z0-9_.-]*")
		}
		validateRejected(name, p, svc, fs)
		validatePolicy(name, p, svc, in.Policy, fs)
		validateLimits(name, p, svc, fs)
		validateLabels(name, p, svc, fs)
		validateMounts(name, p, svc, in, fs)
		validateExtensions(name, p, svc, fs)
	}
	validateDependencies(project, fs)
	validateTopLevelObjects(project, in.Policy, fs)
}

// validateRejected refuses the keys of §1.5 — Swarm semantics, host
// namespaces and Windows-only keys.
func validateRejected(name, p string, svc types.ServiceConfig, fs *findings) {
	if svc.Deploy != nil {
		d := svc.Deploy
		switch {
		case d.Replicas != nil:
			fs.errf(CodeSwarmKeyRejected, name, p+".deploy.replicas", "deploy.replicas is Swarm semantics — one instance per service")
		case d.Mode != "":
			fs.errf(CodeSwarmKeyRejected, name, p+".deploy.mode", "deploy.mode is Swarm semantics")
		case d.UpdateConfig != nil || d.RollbackConfig != nil:
			fs.errf(CodeSwarmKeyRejected, name, p+".deploy.update_config", "deploy update/rollback configs are Swarm semantics — zero-downtime is built in (§8)")
		case len(d.Placement.Constraints) > 0 || len(d.Placement.Preferences) > 0:
			fs.errf(CodeSwarmKeyRejected, name, p+".deploy.placement", "deploy.placement is Swarm semantics — the server is chosen by the resource")
		case d.EndpointMode != "":
			fs.errf(CodeSwarmKeyRejected, name, p+".deploy.endpoint_mode", "deploy.endpoint_mode is Swarm semantics")
		case len(d.Labels) > 0:
			fs.errf(CodeSwarmKeyRejected, name, p+".deploy.labels", "deploy.labels is Swarm semantics — use service labels")
		}
	}
	if svc.Scale != nil && *svc.Scale != 1 {
		fs.errf(CodeSwarmKeyRejected, name, p+".scale", "scale is rejected — one instance per service (multi-instance is P3)")
	}
	if len(svc.ExternalLinks) > 0 {
		fs.errf(CodeSwarmKeyRejected, name, p+".external_links", "external_links is legacy and bypasses destinations")
	}

	switch {
	case svc.NetworkMode == "host":
		fs.errf(CodeNetworkModeHostRejected, name, p+".network_mode", "network_mode: host breaks per-stack isolation and proxy routing")
	case strings.HasPrefix(svc.NetworkMode, "service:") || strings.HasPrefix(svc.NetworkMode, "container:"):
		fs.errf(CodeNetworkModeRejected, name, p+".network_mode", "network_mode %q is incompatible with container replacement", svc.NetworkMode)
	}

	if svc.Pid == "host" || svc.Ipc == "host" || svc.UserNSMode == "host" || svc.Cgroup == "host" || svc.CgroupParent != "" {
		fs.errf(CodeHostNamespaceRejected, name, p, "host namespaces (pid/ipc/userns/cgroup) are isolation escapes")
	}
	if svc.CredentialSpec != nil || svc.Isolation != "" {
		fs.errf(CodePlatformUnsupported, name, p, "credential_spec and isolation are Windows-only")
	}
}

// validatePolicy gates the privilege-raising keys of §1.4 on the server
// policy — denied by default, never silently dropped.
func validatePolicy(name, p string, svc types.ServiceConfig, policy Policy, fs *findings) {
	if svc.Privileged && !policy.AllowPrivileged {
		fs.errf(CodePrivilegedDenied, name, p+".privileged", "privileged: true requires an explicit server policy")
	}
	for _, cap := range svc.CapAdd {
		allowed := slices.Contains(defaultCapAllowlist, cap) || slices.Contains(policy.ExtraCapAdd, cap)
		if !allowed {
			fs.errf(CodePrivilegedDenied, name, p+".cap_add", "capability %q is outside the allowlist — requires a server policy", cap)
		}
	}
	if len(svc.Devices) > 0 && !policy.AllowDevices {
		fs.errf(CodePrivilegedDenied, name, p+".devices", "devices requires an explicit server policy")
	}
	for _, opt := range svc.SecurityOpt {
		if opt == "no-new-privileges:true" || opt == "no-new-privileges" {
			continue // always allowed: it reduces privileges
		}
		if !policy.AllowSecurityOpt {
			fs.errf(CodePrivilegedDenied, name, p+".security_opt", "security_opt %q requires a server policy", opt)
		}
	}
	for key := range svc.Sysctls {
		// Unprivileged net.* sysctls are commonly needed (e.g. somaxconn) and
		// scoped to the container's own namespace.
		if !strings.HasPrefix(key, "net.") {
			fs.errf(CodePrivilegedDenied, name, p+".sysctls", "sysctl %q is outside the net.* allowlist", key)
		}
	}
}

// validateLimits refuses a service mixing deploy.resources with the legacy
// limit keys when they contradict each other (§1.3): guessing which of two
// contradictory limits the operator meant would apply the wrong one silently.
func validateLimits(name, p string, svc types.ServiceConfig, fs *findings) {
	if svc.Deploy == nil {
		return
	}
	limits := svc.Deploy.Resources.Limits
	if limits == nil {
		return
	}
	if svc.MemLimit != 0 && limits.MemoryBytes != 0 && types.UnitBytes(limits.MemoryBytes) != svc.MemLimit {
		fs.errf(CodeConflictingLimits, name, p, "mem_limit and deploy.resources.limits.memory contradict each other")
	}
	if svc.CPUS != 0 && limits.NanoCPUs != 0 && float32(limits.NanoCPUs) != svc.CPUS {
		fs.errf(CodeConflictingLimits, name, p, "cpus and deploy.resources.limits.cpus contradict each other")
	}
}

// validateLabels reserves the akerdock.* label namespace (§2.3): a user label
// there could spoof the managed/unmanaged boundary (INV-015).
func validateLabels(name, p string, svc types.ServiceConfig, fs *findings) {
	for key := range svc.Labels {
		if strings.HasPrefix(strings.ToLower(key), "akerdock.") {
			fs.errf(CodeReservedLabel, name, p+".labels", "the akerdock.* label prefix is reserved for the platform")
		}
	}
}

// validateMounts applies the bind-mount policy (§1.4) and path traversal
// rules (§2.4): relative paths must stay inside the clone / stack directory,
// absolute paths must be under an allowed root.
func validateMounts(name, p string, svc types.ServiceConfig, in Input, fs *findings) {
	for _, vol := range svc.Volumes {
		if vol.Type != types.VolumeTypeBind {
			continue
		}
		source := vol.Source
		if source == "" {
			continue
		}
		if strings.HasPrefix(source, "/") {
			if !underAllowedRoot(source, in.Policy.AllowedBindRoots) {
				fs.errf(CodeBindMountDenied, name, p+".volumes", "absolute bind mount %q is outside the allowed roots", source)
			}
			continue
		}
		if escapesBase(source) {
			fs.errf(CodePathTraversal, name, p+".volumes", "relative bind mount %q escapes the stack directory", source)
		}
	}
	for _, envFile := range svc.EnvFiles {
		if strings.HasPrefix(envFile.Path, "/") {
			fs.errf(CodePathTraversal, name, p+".env_file", "env_file %q is an absolute path — only files inside the source are allowed", envFile.Path)
		} else if escapesBase(envFile.Path) {
			fs.errf(CodePathTraversal, name, p+".env_file", "env_file %q escapes the source directory", envFile.Path)
		}
	}
	if svc.Build != nil {
		if strings.HasPrefix(svc.Build.Context, "/") || escapesBase(svc.Build.Context) {
			fs.errf(CodePathTraversal, name, p+".build.context", "build context %q must stay inside the source", svc.Build.Context)
		}
	}
	if svc.Extends != nil && svc.Extends.File != "" {
		if strings.HasPrefix(svc.Extends.File, "/") || escapesBase(svc.Extends.File) {
			fs.errf(CodePathTraversal, name, p+".extends.file", "extends.file %q must stay inside the source", svc.Extends.File)
		}
	}
}

// escapesBase reports whether a relative path climbs out of its base once
// cleaned — `./ok/../up/../..` style sequences included.
func escapesBase(rel string) bool {
	cleaned := path.Clean(rel)
	return cleaned == ".." || strings.HasPrefix(cleaned, "../")
}

func underAllowedRoot(abs string, roots []string) bool {
	cleaned := path.Clean(abs)
	for _, root := range roots {
		root = path.Clean(root)
		if cleaned == root || strings.HasPrefix(cleaned, root+"/") {
			return true
		}
	}
	return false
}

// validateDependencies checks the depends_on graph (§2.6): conditions that
// reference missing healthchecks or non-oneshot services, and cycles.
func validateDependencies(project *types.Project, fs *findings) {
	for name, svc := range project.Services {
		p := "services." + name + ".depends_on"
		for dep, cfg := range svc.DependsOn {
			target, ok := project.Services[dep]
			if !ok {
				// compose-go's consistency check is skipped: report it here.
				fs.errf(CodeParseError, name, p, "depends_on references unknown service %q", dep)
				continue
			}
			switch cfg.Condition {
			case types.ServiceConditionHealthy:
				if target.HealthCheck == nil || target.HealthCheck.Disable {
					fs.errf(CodeDependencyNeedsHealthcheck, name, p, "service_healthy requires a healthcheck on %q", dep)
				}
			case types.ServiceConditionCompletedSuccessfully:
				if target.Restart != "" && target.Restart != types.RestartPolicyNo {
					fs.errf(CodeParseError, name, p, "service_completed_successfully requires %q to be a one-shot job (restart: no)", dep)
				}
			}
		}
	}
	if cycle := findCycle(project); len(cycle) > 0 {
		fs.errf(CodeDependencyCycle, "", "services", "depends_on cycle: %s", strings.Join(cycle, " -> "))
	}
}

// validateTopLevelObjects gates external networks/volumes and refuses
// external configs/secrets (§1.2): an external object is unmanaged (INV-015),
// which the operator must opt into per server.
func validateTopLevelObjects(project *types.Project, policy Policy, fs *findings) {
	for name, network := range project.Networks {
		if bool(network.External) && !policy.AllowExternalObjects {
			fs.errf(CodeExternalObjectRejected, "", "networks."+name, "external network %q requires a server policy", name)
		}
	}
	for name, volume := range project.Volumes {
		if bool(volume.External) && !policy.AllowExternalObjects {
			fs.errf(CodeExternalObjectRejected, "", "volumes."+name, "external volume %q requires a server policy", name)
		}
	}
	for name, config := range project.Configs {
		if bool(config.External) {
			fs.errf(CodeExternalObjectRejected, "", "configs."+name, "external configs are rejected")
		}
	}
	for name, secret := range project.Secrets {
		if bool(secret.External) {
			fs.errf(CodeSwarmKeyRejected, "", "secrets."+name, "external secrets are Swarm semantics")
		}
	}
}
