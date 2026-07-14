package compose

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// Policy is the per-server policy for privilege-raising keys (compose-spec
// §1.4). The zero value is the default policy: everything denied.
type Policy struct {
	AllowPrivileged      bool
	AllowDevices         bool
	AllowSecurityOpt     bool
	AllowExternalObjects bool
	// ExtraCapAdd extends the default allowlist (NET_BIND_SERVICE, CHOWN,
	// SETUID, SETGID).
	ExtraCapAdd []string
	// AllowedBindRoots are the absolute host directories under which bind
	// mounts are allowed. Empty = every absolute bind is denied.
	AllowedBindRoots []string
}

// Input is everything the pipeline needs. Variables are the RESOLVED stack
// variables (shared scopes and magic variables already merged by the caller,
// compose-spec §3.2): this package never talks to the database.
type Input struct {
	Content   string
	StackUUID string
	Variables map[string]string
	Policy    Policy
	// Raw enables the raw compose mode (§9): transformations that rename or
	// rewrite are skipped, security boundaries stay.
	Raw bool
}

// Result carries the loaded project, the execution plan and every finding.
// Plan is nil when findings contain at least one error.
type Result struct {
	Project  *types.Project
	Plan     *Plan
	Findings []Finding
}

// HasErrors reports whether the compose file is deployable.
func (r *Result) HasErrors() bool { return HasErrors(r.Findings) }

// knownTopLevel are the top-level keys the subset understands (§1.2). x-* is
// handled separately.
var knownTopLevel = map[string]bool{
	"services": true, "networks": true, "volumes": true,
	"configs": true, "secrets": true,
	"name": true, "version": true, "include": true,
}

// interpolationRef matches ${VAR}, ${VAR:-def}, ${VAR:?err}… and bare $VAR.
// Used only to WARN about undefined variables without defaults — the real
// interpolation is compose-go's, conforming to the Compose Specification.
var interpolationRef = regexp.MustCompile(`\$(?:\{([A-Za-z_][A-Za-z0-9_]*)(:?[-?+][^}]*)?\}|([A-Za-z_][A-Za-z0-9_]*))`)

// Load runs the full control-plane pipeline of compose-spec.md sections 1–5:
// raw pass (keys the schema would reject must be ignored-with-warning, not
// fatal), Compose Specification parse + interpolation, policy validation and
// deterministic transformation into a Plan.
func Load(ctx context.Context, in Input) (*Result, error) {
	res := &Result{}
	fs := findings{}

	sanitized, ok := rawPass(in.Content, &fs)
	if !ok {
		res.Findings = fs
		return res, nil
	}

	warnUndefinedVariables(in.Content, in.Variables, &fs)

	buf, err := yaml.Marshal(sanitized)
	if err != nil {
		return nil, fmt.Errorf("compose: remarshal: %w", err)
	}

	project, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		ConfigFiles: []types.ConfigFile{{Filename: "docker-compose.yml", Content: buf}},
		Environment: in.Variables,
	}, func(o *loader.Options) {
		o.SetProjectName(in.StackUUID, true)
		o.SkipInclude = true            // rejected in rawPass — never followed
		o.SkipResolveEnvironment = true // env_file is materialized on the server, not here
		o.ResolvePaths = false
		o.SkipConsistencyCheck = true // our checks below are stricter and coded
	})
	if err != nil {
		// A ${VAR:?msg} miss surfaces as a loader error: it is a validation
		// outcome (deployment blocked before enqueue), not an internal one.
		if strings.Contains(err.Error(), "required variable") {
			fs.errf(CodeRequiredVariableMissing, "", "", "%s", err.Error())
		} else {
			fs.errf(CodeParseError, "", "", "%s", err.Error())
		}
		res.Findings = fs
		return res, nil
	}
	res.Project = project

	validate(project, in, &fs)

	if !HasErrors(fs) {
		plan, perr := buildPlan(project, in, &fs)
		if perr != nil {
			return nil, perr
		}
		if !HasErrors(fs) {
			res.Plan = plan
		}
	}
	res.Findings = fs
	return res, nil
}

// rawPass inspects the file BEFORE the schema parser: the subset's contract
// is "unknown key = ignored with warning" (§1.1), which a strict schema
// validation would turn into a fatal error. It returns the document with the
// offending keys removed, ready for compose-go.
func rawPass(content string, fs *findings) (map[string]any, bool) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		fs.errf(CodeParseError, "", "", "invalid YAML: %v", err)
		return nil, false
	}
	if doc == nil {
		fs.errf(CodeParseError, "", "", "empty compose file")
		return nil, false
	}

	for key := range doc {
		switch {
		case key == "version":
			fs.warnf(CodeVersionIgnored, "", "version", "the version key is obsolete in the Compose Specification and is ignored")
			delete(doc, key)
		case key == "name":
			fs.warnf(CodeKeyIgnored, "", "name", "the project name is imposed by the stack UUID (INV-011) — name is ignored")
			delete(doc, key)
		case key == "include":
			fs.errf(CodeIncludeRejected, "", "include", "include is rejected: one stack = one file (path traversal surface)")
			delete(doc, key)
		case strings.HasPrefix(key, "x-"):
			// Legal extensions (kept: compose-go exposes them via Extensions).
		case !knownTopLevel[key]:
			fs.warnf(CodeKeyIgnored, "", key, "unsupported top-level key %q removed", key)
			delete(doc, key)
		}
	}

	services, _ := doc["services"].(map[string]any)
	if len(services) == 0 {
		fs.errf(CodeParseError, "", "services", "the file declares no service")
		return nil, false
	}
	for name, raw := range services {
		svc, _ := raw.(map[string]any)
		if svc == nil {
			continue // schema validation will name the problem precisely
		}
		for key := range svc {
			switch {
			case key == "container_name":
				fs.warnf(CodeContainerNameIgnored, name, "services."+name+".container_name", "container naming is imposed (INV-011) — container_name is ignored")
				delete(svc, key)
			case key == "links":
				fs.warnf(CodeKeyIgnored, name, "services."+name+".links", "links is legacy — the stack's isolated network DNS covers it")
				delete(svc, key)
			case strings.HasPrefix(key, "x-"):
			case !knownServiceKeys[key]:
				fs.warnf(CodeKeyIgnored, name, "services."+name+"."+key, "unsupported service key %q removed", key)
				delete(svc, key)
			}
		}
	}
	return doc, true
}

// knownServiceKeys is the union of the supported, transformed and rejected
// service keys (§1.3–§1.5): rejected keys must SURVIVE the raw pass so the
// typed validation can refuse them with their own stable code.
var knownServiceKeys = map[string]bool{
	"image": true, "build": true, "command": true, "entrypoint": true,
	"environment": true, "env_file": true, "ports": true, "expose": true,
	"volumes": true, "labels": true, "hostname": true, "networks": true,
	"depends_on": true, "healthcheck": true, "restart": true, "deploy": true,
	"mem_limit": true, "mem_reservation": true, "memswap_limit": true,
	"cpus": true, "cpu_shares": true, "cpuset": true,
	"stop_grace_period": true, "stop_signal": true,
	"user": true, "working_dir": true, "init": true, "tty": true, "stdin_open": true,
	"read_only": true, "tmpfs": true, "shm_size": true,
	"ulimits": true, "group_add": true,
	"dns": true, "dns_search": true, "extra_hosts": true,
	"platform": true, "pull_policy": true, "profiles": true,
	"logging": true, "extends": true, "configs": true, "secrets": true,
	// Policy-gated and rejected keys — kept for the typed checks (§1.4–1.5).
	"privileged": true, "cap_add": true, "cap_drop": true, "devices": true,
	"security_opt": true, "sysctls": true, "network_mode": true,
	"pid": true, "ipc": true, "userns_mode": true, "cgroup_parent": true, "cgroup": true,
	"external_links": true, "scale": true, "credential_spec": true, "isolation": true,
}

// warnUndefinedVariables emits compose_variable_undefined for ${VAR}/$VAR
// references without a default and without a definition (§3.1) — the value
// interpolates to empty, which is legal but rarely intended.
func warnUndefinedVariables(content string, vars map[string]string, fs *findings) {
	seen := map[string]bool{}
	for _, m := range interpolationRef.FindAllStringSubmatch(content, -1) {
		name, operator := m[1], m[2]
		if name == "" {
			name = m[3]
		}
		if operator != "" || seen[name] {
			continue // has a default/requirement, or already reported
		}
		if _, ok := vars[name]; !ok {
			seen[name] = true
			fs.warnf(CodeVariableUndefined, "", "", "variable %q is undefined and interpolates to an empty string", name)
		}
	}
}
