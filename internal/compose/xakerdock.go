package compose

import (
	"encoding/json"
	"fmt"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/deepteams/akerdock/internal/accessroute"
)

// maxFileContentBytes bounds x-akerdock.content (§23.3 PRD): a "config file"
// extension is not a data smuggling channel.
const maxFileContentBytes = 5 << 20

// ServiceExtensions are the x-akerdock keys of a service (compose-spec §5.1).
type ServiceExtensions struct {
	ExcludeFromHC bool
	// AccessPublicRoutes are narrow unauthenticated paths owned by this
	// routed service (ADR-049).
	AccessPublicRoutes []accessroute.Route
	// ZeroDowntime is nil when unset (default: eligible), false when the
	// stack cannot tolerate two simultaneous instances (ADR-015).
	ZeroDowntime *bool
	// Pre/PostDeploymentCommand are the stack-level hooks (deployment-engine
	// §10, applied per service): pre runs in the EXISTING container of this
	// service before any build or mutation; post runs in its CANDIDATE once
	// healthy, before its switch — a failing post never switches this service.
	PreDeploymentCommand  string
	PostDeploymentCommand string
}

// VolumeExtensions are the x-akerdock keys of one volumes[] entry (§5.1):
// managed file/directory creation on the host before the mount.
type VolumeExtensions struct {
	IsDirectory bool
	// Content creates the host file with variable interpolation; editable in
	// the UI afterwards.
	Content  string
	FileMode string
	OwnerUID *int64
	GroupGID *int64
}

// serviceExtensions extracts and validates the x-akerdock block of a service.
func serviceExtensions(name, p string, svc types.ServiceConfig, fs *findings) ServiceExtensions {
	out := ServiceExtensions{}
	raw := extensionMap(svc.Extensions)
	if raw == nil {
		return out
	}
	if v, ok := raw["exclude_from_hc"].(bool); ok {
		out.ExcludeFromHC = v
	}
	if v, ok := raw["zero_downtime"].(bool); ok {
		out.ZeroDowntime = &v
	}
	if v, ok := raw["pre_deployment_command"].(string); ok {
		out.PreDeploymentCommand = v
	}
	if v, ok := raw["post_deployment_command"].(string); ok {
		out.PostDeploymentCommand = v
	}
	if value, ok := raw["access_public_routes"]; ok {
		encoded, err := json.Marshal(value)
		if err != nil {
			fs.errf(CodeAccessPublicRouteInvalid, name, p+".access_public_routes",
				"access_public_routes must be a list of route objects")
		} else {
			var routes []accessroute.Route
			if err := json.Unmarshal(encoded, &routes); err != nil {
				fs.errf(CodeAccessPublicRouteInvalid, name, p+".access_public_routes",
					"access_public_routes must be a list of {path, match, methods, parameters} objects")
			} else {
				for i, route := range routes {
					normalized, err := accessroute.Validate(route)
					if err != nil {
						fs.errf(CodeAccessPublicRouteInvalid, name,
							fmt.Sprintf("%s.access_public_routes[%d]", p, i), "%s", err)
						continue
					}
					out.AccessPublicRoutes = append(out.AccessPublicRoutes, normalized)
				}
			}
		}
	}

	// The §10 guarantees need somewhere to stand. A post hook on a one-shot
	// has no candidate to run in — refused. A post hook on a service with no
	// compose-declared healthcheck may still work (the image can carry one),
	// but the deployment will refuse it before mutating anything if none
	// resolves — warned here, where the author can still fix the file.
	if out.PostDeploymentCommand != "" {
		if svc.Restart == types.RestartPolicyNo {
			fs.errf(CodeHookOnOneShot, name, p+".post_deployment_command",
				"a post-deployment command needs a candidate container: a one-shot service (restart: no) never has one (§10)")
		} else if svc.HealthCheck == nil || svc.HealthCheck.Disable {
			fs.warnf(CodeHookWithoutHealthcheck, name, p+".post_deployment_command",
				"a post-deployment command requires a resolvable health check (§10, INV-005): none is declared here — the deployment will fail before mutating anything if the image provides none")
		}
	}
	return out
}

// volumeExtensions extracts and validates the x-akerdock block of a volume
// entry. content and is_directory are mutually exclusive: a path cannot be
// both a managed file and a managed directory.
func volumeExtensions(service, p string, vol types.ServiceVolumeConfig, fs *findings) *VolumeExtensions {
	raw := extensionMap(vol.Extensions)
	if raw == nil {
		return nil
	}
	out := &VolumeExtensions{}
	if v, ok := raw["is_directory"].(bool); ok {
		out.IsDirectory = v
	}
	if v, ok := raw["content"].(string); ok {
		out.Content = v
	}
	if v, ok := raw["file_mode"].(string); ok {
		out.FileMode = v
	}
	if v, ok := raw["owner_uid"].(int); ok {
		uid := int64(v)
		out.OwnerUID = &uid
	}
	if v, ok := raw["group_gid"].(int); ok {
		gid := int64(v)
		out.GroupGID = &gid
	}

	if out.IsDirectory && out.Content != "" {
		fs.errf(CodeStorageExtensionConflict, service, p, "content and is_directory are mutually exclusive on a volume entry")
	}
	if len(out.Content) > maxFileContentBytes {
		fs.errf(CodeFileContentTooLarge, service, p, "x-akerdock.content exceeds %d MiB", maxFileContentBytes>>20)
	}
	if out.Content == "" && !out.IsDirectory && out.FileMode == "" && out.OwnerUID == nil && out.GroupGID == nil {
		return nil
	}
	return out
}

// validateExtensions walks every service and volume entry once so extension
// errors surface even when the plan is not built.
func validateExtensions(name, p string, svc types.ServiceConfig, fs *findings) {
	serviceExtensions(name, p+".x-akerdock", svc, fs)
	for i, vol := range svc.Volumes {
		volumeExtensions(name, fmt.Sprintf("%s.volumes[%d].x-akerdock", p, i), vol, fs)
	}
}

// extensionMap returns the x-akerdock block of an Extensions map, or nil.
func extensionMap(ext types.Extensions) map[string]any {
	raw, ok := ext["x-akerdock"]
	if !ok {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	return m
}
