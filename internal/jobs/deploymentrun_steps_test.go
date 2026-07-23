package jobs

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/compose"
)

// A failed step must keep BOTH the command output and the error: the error
// often carries more than the output — candidateFailure packs the dying
// container's logs into it, and dropping it leaves the operator staring at a
// bare "restarting" with nothing to debug.
func TestAppendFailure(t *testing.T) {
	err := errors.New("container is \"restarting\", expected running\npanic: missing DATABASE_URL")

	if got := appendFailure(nil, err); got == nil || !strings.Contains(*got, "missing DATABASE_URL") {
		t.Fatalf("empty log must become the error, got %v", got)
	}

	inspect := "restarting"
	got := appendFailure(&inspect, err)
	if !strings.Contains(*got, "restarting") || !strings.Contains(*got, "missing DATABASE_URL") {
		t.Fatalf("log must keep output AND error, got %q", *got)
	}

	// An error already embedded in the log (an exit-code error built FROM the
	// output) must not be duplicated.
	full := "some output\n" + err.Error()
	if got := appendFailure(&full, err); *got != full {
		t.Fatalf("embedded error must not duplicate, got %q", *got)
	}
}

// The chown script must only touch EMPTY volumes, resolve named users through
// the image's own /etc/passwd, and never fail the step (trailing true). The
// sh -n pass guards the quoting: this string runs as root on every server.
func TestChownEmptyVolumesScript(t *testing.T) {
	script := chownEmptyVolumesScript("akerdock/app:sha", []string{"vol_a", "vol_b"})

	for _, want := range []string{
		"docker image inspect --format '{{.Config.User}}' akerdock/app:sha",
		"/etc/passwd",
		"for v in vol_a vol_b; do",
		`ls -A`,
		"chown",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n%s", want, script)
		}
	}
	if !strings.HasSuffix(script, "true") {
		t.Fatal("the script must be best-effort: it must end with true")
	}

	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated script does not parse: %v\n%s\n%s", err, out, script)
	}
}

// Only NAMED volumes get the ownership fix: binds belong to the operator and
// tmpfs to the kernel.
func TestComposeVolumeSources(t *testing.T) {
	sp := compose.ServicePlan{Mounts: []compose.MountPlan{
		{Type: "volume", Source: "stack_data"},
		{Type: "bind", Source: "/srv/files"},
		{Type: "tmpfs", Source: ""},
		{Type: "volume", Source: "stack_cache"},
	}}
	got := composeVolumeSources(sp)
	if len(got) != 2 || got[0] != "stack_data" || got[1] != "stack_cache" {
		t.Fatalf("composeVolumeSources = %v", got)
	}
}
