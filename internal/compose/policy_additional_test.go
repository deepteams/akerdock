package compose

import (
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func ptr[T any](value T) *T { return &value }

func TestRejectedComposeBranches(t *testing.T) {
	t.Parallel()

	cases := []types.ServiceConfig{
		{Deploy: &types.DeployConfig{Mode: "replicated"}},
		{Deploy: &types.DeployConfig{UpdateConfig: &types.UpdateConfig{}}},
		{Deploy: &types.DeployConfig{Placement: types.Placement{Constraints: []string{"node.role==manager"}}}},
		{Deploy: &types.DeployConfig{EndpointMode: "dnsrr"}},
		{Deploy: &types.DeployConfig{Labels: types.Labels{"managed": "true"}}},
		{Scale: ptr(2)},
		{ExternalLinks: []string{"legacy"}},
		{NetworkMode: "container:peer"},
		{CgroupParent: "host.slice"},
		{CredentialSpec: &types.CredentialSpecConfig{File: "credential.json"}},
	}
	for i, service := range cases {
		var got findings
		validateRejected("app", "services.app", service, &got)
		if len(got) == 0 {
			t.Errorf("case %d should be rejected: %+v", i, service)
		}
	}
}

func TestPrivilegePolicyBranches(t *testing.T) {
	t.Parallel()

	service := types.ServiceConfig{
		Devices:     []types.DeviceMapping{{Source: "/dev/kvm"}},
		SecurityOpt: []string{"seccomp=unconfined", "no-new-privileges:true"},
		Sysctls: types.Mapping{
			"kernel.shmmax":      "1024",
			"net.core.somaxconn": "1024",
		},
	}
	var denied findings
	validatePolicy("app", "services.app", service, Policy{}, &denied)
	if len(denied) != 3 {
		t.Fatalf("devices, custom security_opt and non-net sysctl should be denied: %v", denied)
	}

	var allowed findings
	validatePolicy("app", "services.app", service, Policy{
		AllowDevices:     true,
		AllowSecurityOpt: true,
	}, &allowed)
	if len(allowed) != 1 || allowed[0].Path != "services.app.sysctls" {
		t.Fatalf("only the non-net sysctl should remain denied: %v", allowed)
	}
}

func TestPathPolicyBranches(t *testing.T) {
	t.Parallel()

	cases := []types.ServiceConfig{
		{Volumes: []types.ServiceVolumeConfig{{Type: types.VolumeTypeBind}}},
		{EnvFiles: []types.EnvFile{{Path: "/etc/app.env"}}},
		{EnvFiles: []types.EnvFile{{Path: "../app.env"}}},
		{Build: &types.BuildConfig{Context: "/srv/source"}},
		{Build: &types.BuildConfig{Context: "../../source"}},
		{Extends: &types.ExtendsConfig{File: "/srv/base.yml"}},
		{Extends: &types.ExtendsConfig{File: "../base.yml"}},
	}
	for i, service := range cases {
		var got findings
		validateMounts("app", "services.app", service, Input{}, &got)
		if i == 0 {
			if len(got) != 0 {
				t.Fatalf("an empty bind source should be ignored: %v", got)
			}
			continue
		}
		if len(got) != 1 || got[0].Code != CodePathTraversal {
			t.Errorf("case %d should report exactly one traversal: %v", i, got)
		}
	}
}

func TestDependencyAndTopLevelObjectBranches(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Services: types.Services{
			"app": {
				DependsOn: types.DependsOnConfig{
					"missing": {Condition: types.ServiceConditionStarted},
					"task":    {Condition: types.ServiceConditionCompletedSuccessfully},
				},
			},
			"task": {Restart: types.RestartPolicyUnlessStopped},
		},
		Volumes: types.Volumes{
			"shared": {External: true},
		},
		Configs: types.Configs{
			"settings": types.ConfigObjConfig(types.FileObjectConfig{External: true}),
		},
		Secrets: types.Secrets{
			"token": types.SecretConfig(types.FileObjectConfig{External: true}),
		},
	}
	var got findings
	validateDependencies(project, &got)
	validateTopLevelObjects(project, Policy{}, &got)

	for _, code := range []string{CodeParseError, CodeExternalObjectRejected, CodeSwarmKeyRejected} {
		found := false
		for _, finding := range got {
			found = found || finding.Code == code
		}
		if !found {
			t.Errorf("missing %s in %v", code, got)
		}
	}
}

func TestFindingString(t *testing.T) {
	t.Parallel()

	withService := Finding{Severity: Error, Code: CodeParseError, Service: "api", Message: "invalid"}
	if got := withService.String(); !strings.Contains(got, "(api)") {
		t.Fatalf("service missing from finding string: %q", got)
	}
	withoutService := Finding{Severity: Warning, Code: CodeVersionIgnored, Message: "ignored"}
	if got := withoutService.String(); strings.Contains(got, "()") || !strings.Contains(got, "ignored") {
		t.Fatalf("unexpected global finding string: %q", got)
	}
}
