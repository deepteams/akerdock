// Package compose parses, validates and transforms the Docker Compose subset
// of compose-spec.md. Everything here is pure: same file + same variables +
// same policy produce exactly the same plan and findings (INV-011, INV-014) —
// which is what makes the whole pipeline unit-testable without a server.
package compose

import "fmt"

// Severity of a finding (compose-spec §11): an error blocks the deployment
// or the save; a warning is accepted, traced and displayed.
type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Stable finding codes (compose-spec §11) — consumed by the API in details[].
const (
	CodeParseError                 = "compose_parse_error"
	CodeVersionIgnored             = "compose_version_ignored"
	CodeKeyIgnored                 = "compose_key_ignored"
	CodeContainerNameIgnored       = "compose_container_name_ignored"
	CodeSwarmKeyRejected           = "compose_swarm_key_rejected"
	CodeNetworkModeHostRejected    = "compose_network_mode_host_rejected"
	CodeNetworkModeRejected        = "compose_network_mode_rejected"
	CodeHostNamespaceRejected      = "compose_host_namespace_rejected"
	CodePrivilegedDenied           = "compose_privileged_denied"
	CodeBindMountDenied            = "compose_bind_mount_denied"
	CodeExternalObjectRejected     = "compose_external_object_rejected"
	CodeIncludeRejected            = "compose_include_rejected"
	CodePlatformUnsupported        = "compose_platform_unsupported"
	CodeInvalidServiceName         = "compose_invalid_service_name"
	CodeReservedLabel              = "compose_reserved_label"
	CodePathTraversal              = "compose_path_traversal"
	CodeConflictingLimits          = "compose_conflicting_limits"
	CodeDependencyCycle            = "compose_dependency_cycle"
	CodeDependencyNeedsHealthcheck = "compose_dependency_needs_healthcheck"
	CodeRequiredVariableMissing    = "compose_required_variable_missing"
	CodeVariableUndefined          = "compose_variable_undefined"
	CodeSharedVariableMissing      = "compose_shared_variable_missing"
	CodeMagicVariableInvalidType   = "compose_magic_variable_invalid_type"
	CodeMagicVariableUnknownComp   = "compose_magic_variable_unknown_component"
	CodeStorageExtensionConflict   = "compose_storage_extension_conflict"
	CodeRoutablePortUnresolved     = "compose_routable_port_unresolved"
	CodeDomainConflict             = "compose_domain_conflict"
	CodeOneshotWithoutExclude      = "compose_oneshot_without_exclude"
	CodeZeroDowntimeIneligible     = "compose_zero_downtime_ineligible"
	CodeFileContentTooLarge        = "compose_file_content_too_large"
	CodeHookOnOneShot              = "compose_hook_on_one_shot"
	CodeHookWithoutHealthcheck     = "compose_hook_without_healthcheck"
)

// Finding is one validation outcome. Message is generic and never contains a
// secret (INV-003); Path is the YAML path (e.g. services.app.deploy.replicas).
type Finding struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Service  string   `json:"service,omitempty"`
	Path     string   `json:"path,omitempty"`
	Message  string   `json:"message"`
}

func (f Finding) String() string {
	if f.Service != "" {
		return fmt.Sprintf("[%s] %s (%s): %s", f.Severity, f.Code, f.Service, f.Message)
	}
	return fmt.Sprintf("[%s] %s: %s", f.Severity, f.Code, f.Message)
}

// findings collects results during a pass.
type findings []Finding

func (fs *findings) errf(code, service, path, format string, args ...any) {
	*fs = append(*fs, Finding{Code: code, Severity: Error, Service: service, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (fs *findings) warnf(code, service, path, format string, args ...any) {
	*fs = append(*fs, Finding{Code: code, Severity: Warning, Service: service, Path: path, Message: fmt.Sprintf(format, args...)})
}

// HasErrors reports whether at least one finding blocks the operation.
func HasErrors(fs []Finding) bool {
	for _, f := range fs {
		if f.Severity == Error {
			return true
		}
	}
	return false
}
