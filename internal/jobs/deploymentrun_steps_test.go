package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/deepteams/akerdock/internal/compose"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
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

// The ownership fix must only run for a non-root image user, INSIDE a
// throwaway container of the image itself (user 0) — never against
// /var/lib/docker on the host — and only guard-then-chown empty volumes. It
// is best-effort: a failing one-shot never fails the deployment.
func TestChownEmptyVolumes(t *testing.T) {
	newRT := func(user string) *fake.Runtime {
		rt := &fake.Runtime{}
		rt.ImageInspectFn = func(context.Context, string, ...client.ImageInspectOption) (imagetypes.InspectResponse, error) {
			return imagetypes.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{
				ImageConfig: ocispecv1.ImageConfig{User: user},
			}}, nil
		}
		rt.ContainerWaitFn = func(context.Context, string, containertypes.WaitCondition) (<-chan containertypes.WaitResponse, <-chan error) {
			waitCh := make(chan containertypes.WaitResponse, 1)
			waitCh <- containertypes.WaitResponse{StatusCode: 0}
			return waitCh, make(chan error, 1)
		}
		return rt
	}

	run := &deploymentRun{h: &DeploymentRun{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}

	rt := newRT("app")
	run.rt = rt
	run.chownEmptyVolumes(context.Background(), "akerdock/app:sha", []string{"vol_a", "vol_b"})
	creates := 0
	for _, c := range rt.Calls() {
		if c.Method != "ContainerCreate" {
			continue
		}
		creates++
		cfg := c.Args[0].(*containertypes.Config)
		host := c.Args[1].(*containertypes.HostConfig)
		if cfg.User != "0" || cfg.Entrypoint[0] != "/bin/sh" {
			t.Fatalf("fix must run as user 0 through /bin/sh: %+v", cfg)
		}
		if !strings.Contains(cfg.Cmd[1], "ls -A /akerdock-volume") || !strings.Contains(cfg.Cmd[1], "chown -- 'app'") {
			t.Fatalf("fix must guard-then-chown to the image user: %v", cfg.Cmd)
		}
		if len(host.Binds) != 1 || !strings.HasSuffix(host.Binds[0], ":/akerdock-volume") {
			t.Fatalf("fix must mount the volume, never the host path: %v", host.Binds)
		}
	}
	if creates != 2 {
		t.Fatalf("one-shot containers = %d, want one per volume", creates)
	}

	// A root image needs no fix: nothing must run.
	rootRT := newRT("")
	run.rt = rootRT
	run.chownEmptyVolumes(context.Background(), "akerdock/app:sha", []string{"vol_a"})
	for _, name := range rootRT.CallNames() {
		if name == "ContainerCreate" {
			t.Fatal("a root image must not trigger the ownership fix")
		}
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

// awaitCandidate is the §4 verdict machine: no-healthcheck waits for a
// stable running state; a healthcheck polls to healthy/unhealthy/none within
// the budget — and an image whose healthcheck vanished (none) still passes,
// like the CLI loop did.
func TestAwaitCandidateVerdicts(t *testing.T) {
	oldStable, oldPoll := deploymentStablePeriod, deploymentHealthPoll
	deploymentStablePeriod, deploymentHealthPoll = 0, 0
	t.Cleanup(func() { deploymentStablePeriod, deploymentHealthPoll = oldStable, oldPoll })

	inspect := func(status string, health string) func(context.Context, string) (containertypes.InspectResponse, error) {
		return func(context.Context, string) (containertypes.InspectResponse, error) {
			st := &containertypes.State{Status: status, Running: status == "running"}
			if health != "" {
				st.Health = &containertypes.Health{Status: health}
			}
			return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{State: st}}, nil
		}
	}
	silentLogs := func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}

	for name, tc := range map[string]struct {
		health  bool
		status  string
		hstatus string
		want    bool
	}{
		"stable running without healthcheck": {false, "running", "", true},
		"restarting without healthcheck":     {false, "restarting", "", false},
		"healthy":                            {true, "running", "healthy", true},
		"unhealthy":                          {true, "running", "unhealthy", false},
		"healthcheck vanished (none)":        {true, "running", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			rt := &fake.Runtime{}
			rt.ContainerInspectFn = inspect(tc.status, tc.hstatus)
			rt.ContainerLogsFn = silentLogs
			run := &deploymentRun{rt: rt, healthBudget: 1}
			var out strings.Builder
			ok, err := run.awaitCandidate(context.Background(), "c", tc.health, func(chunk string) { out.WriteString(chunk) })
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.want {
				t.Fatalf("verdict = %v, want %v (%s)", ok, tc.want, out.String())
			}
		})
	}
}

// TestPushedDigest pins the ADR-055 digest selection: the identity minted
// under the push repository wins over digests inherited from other repos,
// with the shell era's index-0 as the fallback.
func TestPushedDigest(t *testing.T) {
	repo := "registry.example/acme/app"
	digests := []string{
		"docker.io/library/nginx@sha256:base",
		repo + "@sha256:pushed",
	}
	if got := pushedDigest(digests, repo); got != repo+"@sha256:pushed" {
		t.Fatalf("digest = %q", got)
	}
	if got := pushedDigest([]string{"other@sha256:x"}, repo); got != "other@sha256:x" {
		t.Fatalf("fallback = %q", got)
	}
	if got := pushedDigest(nil, repo); got != "" {
		t.Fatalf("absent = %q", got)
	}
}
