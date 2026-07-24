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

// The chown script must only touch EMPTY volumes, and must do it INSIDE a
// container of the image (--user 0): the SSH user may not be root, and
// /var/lib/docker on the host is out of its reach — the daemon is not. It
// must never fail the step (trailing true). The sh -n pass guards the
// quoting: this string runs on every server.
func TestChownEmptyVolumesScript(t *testing.T) {
	script := chownEmptyVolumesScript("akerdock/app:sha", []string{"vol_a", "vol_b"})

	for _, want := range []string{
		"docker image inspect --format '{{.Config.User}}' akerdock/app:sha",
		"docker run --rm --user 0 --entrypoint /bin/sh",
		`-v "$v":/akerdock-volume akerdock/app:sha`,
		"for v in vol_a vol_b; do",
		`ls -A`,
		"chown",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n%s", want, script)
		}
	}
	if strings.Contains(script, "/var/lib/docker") || strings.Contains(script, "Mountpoint") {
		t.Fatal("the script must never touch the host-side volume path — non-root SSH users cannot")
	}
	if !strings.HasSuffix(script, "true") {
		t.Fatal("the script must be best-effort: it must end with true")
	}

	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated script does not parse: %v\n%s\n%s", err, out, script)
	}

	// The inner -c payload must reach the container's shell with the command
	// substitution UNEXPANDED (escaped $) — expanded on the host it would
	// test the wrong directory.
	if !strings.Contains(script, `\$(ls -A /akerdock-volume)`) {
		t.Fatalf("inner substitution must be escaped for the container shell\n%s", script)
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

// The seed script (ADR-029) mounts production READ-ONLY, only fills EMPTY
// preview volumes, skips a missing production volume without creating it,
// and — unlike the best-effort chown — lets a copy failure fail the step:
// the operator declared they want data.
func TestPreviewSeedScript(t *testing.T) {
	script := previewSeedScript("postgres:17", [][2]string{
		{"app_pgdata", "preview_pgdata"},
	})

	for _, want := range []string{
		"if docker volume inspect app_pgdata >/dev/null 2>&1; then",
		"-v app_pgdata:/akerdock-seed-from:ro",
		"-v preview_pgdata:/akerdock-volume postgres:17",
		"cp -a /akerdock-seed-from/. /akerdock-volume/",
		`ls -A /akerdock-volume`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n%s", want, script)
		}
	}
	if strings.Contains(script, "|| true") {
		t.Fatal("a failed seed must fail the step — never best-effort")
	}

	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated script does not parse: %v\n%s\n%s", err, out, script)
	}
}
