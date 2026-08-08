// Coverage tests for the compose deployment pipeline (composedeploy.go):
// executeCompose end-to-end variants, clone, env/magic variables, preview
// routes and the per-service replacement paths. composecov prefix throughout.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	networktypes "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/jackc/pgx/v5"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/compose"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/store"
)

const (
	composecovPreviewUUID = "88888888-8888-4888-8888-888888888888"
	composecovSHA         = "0123456789012345678901234567890123456789"
)

// composecovGitSSH scripts the SSH half of a compose git deployment.
func composecovGitSSH(content string, headSHA string) func(string) (string, uint32) {
	return func(cmd string) (string, uint32) {
		switch {
		case strings.Contains(cmd, "git ls-remote"):
			return composecovSHA + "\trefs/heads/main\n", 0
		case strings.Contains(cmd, "git rev-parse HEAD"):
			return headSHA + "\n", 0
		case strings.Contains(cmd, "git log -1"):
			return "Alice\x1ffix: compose\n", 0
		case strings.Contains(cmd, "cat "):
			return content, 0
		default:
			return "", 0
		}
	}
}

// composecovVerifyOutput answers the proxy-verification exec with everything
// any expectation of this package's appliers could look for.
func composecovVerifyOutput(uuids ...string) func(cmd []string) string {
	return func(cmd []string) string {
		if len(cmd) > 0 && cmd[0] == "wget" {
			return strings.Join(uuids, " ") + ` {"svc":{"serverStatus":{"http://10.0.0.9:3000":"UP"}}}`
		}
		return "hook ok"
	}
}

// composecovStackRuntime scripts container inspection for a whole stack: the
// -next candidates are healthy with a routable IP, everything else runs with
// empty labels (so nothing hash-skips).
func composecovStackRuntime(network string, uuids ...string) *fake.Runtime {
	rt := composecovRuntime()
	rt.ContainerInspectFn = func(_ context.Context, name string) (containertypes.InspectResponse, error) {
		if strings.HasSuffix(name, "-next") {
			return containertypes.InspectResponse{
				ContainerJSONBase: &containertypes.ContainerJSONBase{
					State: &containertypes.State{Running: true, Status: "running", Health: &containertypes.Health{Status: "healthy"}},
				},
				Config: &containertypes.Config{},
				NetworkSettings: &containertypes.NetworkSettings{
					Networks: map[string]*networktypes.EndpointSettings{
						network: {IPAddress: "10.0.0.9"},
					},
				},
			}, nil
		}
		return containertypes.InspectResponse{
			ContainerJSONBase: &containertypes.ContainerJSONBase{
				State: &containertypes.State{Running: true, Status: "running"},
			},
			Config: &containertypes.Config{Labels: map[string]string{}},
		}, nil
	}
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return nil, nil
	}
	composecovScriptExec(rt, composecovVerifyOutput(uuids...))
	return rt
}

func TestComposecovExecuteComposeGuards(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)
	r := composecovRunner(t, q, keyring, logger, composecovRuntime())

	r.d.IsRollback = true
	if err := r.executeCompose(context.Background(), composecovAppUUID, "/tmp/app", ""); err == nil ||
		!strings.Contains(err.Error(), "rollback of a compose stack") {
		t.Fatalf("rollback guard = %v", err)
	}

	r.d.IsRollback = false
	r.app.Application.GitRepositoryUrl = nil
	if err := r.executeCompose(context.Background(), composecovAppUUID, "/tmp/app", ""); err == nil ||
		!strings.Contains(err.Error(), "requires a git source") {
		t.Fatalf("git source guard = %v", err)
	}
}

func TestComposecovExecuteComposeInlineStackHappyPath(t *testing.T) {
	composecovShrinkTimers(t)
	q, keyring, logger, _ := composecovDeps(t)
	rt := composecovStackRuntime("composecov-net", composecovAppUUID)
	r := composecovRunner(t, q, keyring, logger, rt)
	ssh := composecovNewSSH(t, func(string) (string, uint32) { return "", 0 })
	r.client = composecovDial(t, ssh)
	r.service = &store.Service{ID: 1, ComposeContent: `
services:
  web:
    image: nginx:1.27
    expose: ["3000"]
    volumes:
      - data:/srv/data
    networks:
      - default
      - back
  worker:
    image: alpine:3.20
    networks:
      - default
volumes:
  data: {}
networks:
  back: {}
`}
	if err := r.executeCompose(context.Background(), composecovAppUUID,
		"/var/lib/akerdock/services/"+composecovAppUUID, ""); err != nil {
		t.Fatalf("inline stack deploy: %v", err)
	}
	creates := 0
	for _, c := range rt.CallNames() {
		if c == "ContainerCreate" {
			creates++
		}
	}
	if creates != 2 {
		t.Fatalf("creates = %d, want one per service", creates)
	}
}

func TestComposecovExecuteComposeInlineRefusesBuilds(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)
	rt := composecovRuntime()
	r := composecovRunner(t, q, keyring, logger, rt)
	ssh := composecovNewSSH(t, func(string) (string, uint32) { return "", 0 })
	r.client = composecovDial(t, ssh)
	r.service = &store.Service{ID: 1, ComposeContent: `
services:
  app:
    build:
      context: .
`}
	err := r.executeCompose(context.Background(), composecovAppUUID,
		"/var/lib/akerdock/services/"+composecovAppUUID, "")
	if err == nil || !strings.Contains(err.Error(), "inline stacks deploy images only") {
		t.Fatalf("inline build guard = %v", err)
	}
}

// TestComposecovExecuteComposeZeroDowntimeE2E drives the full git-sourced
// pipeline: adopted-project retirement, hooks, per-service replacement with a
// zero-downtime switch for the routed web service, an opted-out worker
// recreated with interruption, a one-shot at its topological position, and
// the routing switch through the proxy applier.
func TestComposecovExecuteComposeZeroDowntimeE2E(t *testing.T) {
	composecovShrinkTimers(t)
	q, keyring, logger, db := composecovDeps(t)
	db.rows["-- name: ListServiceComponentDomains "] = composecovPortRows(1, 3000)
	db.rows["-- name: ListServiceComponents "] = composecovComponentRows(1, 3000)

	content := `
services:
  web:
    image: ghcr.io/acme/web:1.2
    expose: ["3000"]
    healthcheck:
      test: ["CMD-SHELL", "curl -fsS localhost:3000/health"]
      interval: 1s
      timeout: 1s
      retries: 1
    x-akerdock:
      pre_deployment_command: "echo pre"
      post_deployment_command: "echo post"
  worker:
    image: ghcr.io/acme/worker:1.2
    x-akerdock:
      zero_downtime: false
  migrate:
    image: ghcr.io/acme/migrate:1.2
    restart: "no"
`
	rt := composecovStackRuntime("composecov-net", composecovAppUUID)
	r := composecovRunner(t, q, keyring, logger, rt)
	repo := "https://git.example.test/acme/app.git"
	r.app.Application.GitRepositoryUrl = &repo
	r.app.Resource.Adoption = []byte(`{"compose_project":"legacy"}`)
	r.server.ProxyType = store.ProxyTypeTraefik
	ssh := composecovNewSSH(t, composecovGitSSH(content, composecovSHA))
	r.client = composecovDial(t, ssh)

	if err := r.executeCompose(context.Background(), composecovAppUUID,
		"/var/lib/akerdock/applications/"+composecovAppUUID, ""); err != nil {
		t.Fatalf("zero-downtime compose deploy: %v", err)
	}

	renames, waits := 0, 0
	for _, c := range rt.Calls() {
		switch c.Method {
		case "ContainerRename":
			renames++
			if got := c.Args[0].(string); !strings.HasSuffix(got, "-web-next") {
				t.Fatalf("renamed %q, want the web candidate", got)
			}
		case "ContainerWait":
			if strings.HasSuffix(c.Args[0].(string), "-migrate") {
				waits++
			}
		}
	}
	if renames != 1 {
		t.Fatalf("renames = %d — exactly the web service must switch", renames)
	}
	if waits != 1 {
		t.Fatalf("one-shot waits = %d", waits)
	}
}

func TestComposecovExecuteComposeReadComposeFailure(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)
	rt := composecovRuntime()
	r := composecovRunner(t, q, keyring, logger, rt)
	repo := "https://git.example.test/acme/app.git"
	branch := "release"
	path := "/deploy/compose.yaml"
	r.app.Application.GitRepositoryUrl = &repo
	r.app.Application.GitBranch = &branch
	r.app.BuildConfig.ComposeFilePath = &path
	ssh := composecovNewSSH(t, func(cmd string) (string, uint32) {
		switch {
		case strings.Contains(cmd, "git ls-remote"):
			return composecovSHA + "\trefs/heads/release\n", 0
		case strings.Contains(cmd, "cat "):
			if !strings.Contains(cmd, "deploy/compose.yaml") {
				return "", 0
			}
			return "", 1
		default:
			return "", 0
		}
	})
	r.client = composecovDial(t, ssh)
	err := r.executeCompose(context.Background(), composecovAppUUID,
		"/var/lib/akerdock/applications/"+composecovAppUUID, "")
	if err == nil || !strings.Contains(err.Error(), `compose file "deploy/compose.yaml" not found`) {
		t.Fatalf("read_compose failure = %v", err)
	}
}

func TestComposecovExecuteComposeValidationRefusal(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)
	rt := composecovRuntime()
	r := composecovRunner(t, q, keyring, logger, rt)
	r.service = &store.Service{ID: 1, ComposeContent: `
services:
  app:
    image: nginx
    volumes:
      - /etc:/host-etc
`}
	ssh := composecovNewSSH(t, func(string) (string, uint32) { return "", 0 })
	r.client = composecovDial(t, ssh)
	err := r.executeCompose(context.Background(), composecovAppUUID,
		"/var/lib/akerdock/services/"+composecovAppUUID, "")
	if err == nil || !strings.Contains(err.Error(), "compose validation failed") {
		t.Fatalf("validation refusal = %v", err)
	}
}

func TestComposecovExecuteComposePostCommandGuards(t *testing.T) {
	composecovShrinkTimers(t)
	q, keyring, logger, db := composecovDeps(t)

	// Unrouted service with a post command: refused before any mutation.
	rt := composecovRuntime()
	r := composecovRunner(t, q, keyring, logger, rt)
	r.service = &store.Service{ID: 1, ComposeContent: `
services:
  web:
    image: nginx
    expose: ["80"]
    x-akerdock:
      post_deployment_command: "echo done"
`}
	ssh := composecovNewSSH(t, func(string) (string, uint32) { return "", 0 })
	r.client = composecovDial(t, ssh)
	err := r.executeCompose(context.Background(), composecovAppUUID,
		"/var/lib/akerdock/services/"+composecovAppUUID, "")
	if err == nil || !strings.Contains(err.Error(), "requires a routed service") {
		t.Fatalf("unrouted post guard = %v", err)
	}

	// Routed but ineligible (no healthcheck anywhere): refused with the reason.
	db.rows["-- name: ListServiceComponentDomains "] = composecovPortRows(1, 80)
	r2 := composecovRunner(t, q, keyring, logger, composecovRuntime())
	r2.service = r.service
	r2.client = composecovDial(t, composecovNewSSH(t, func(string) (string, uint32) { return "", 0 }))
	err = r2.executeCompose(context.Background(), composecovAppUUID,
		"/var/lib/akerdock/services/"+composecovAppUUID, "")
	if err == nil || !strings.Contains(err.Error(), "ineligible") {
		t.Fatalf("ineligible post guard = %v", err)
	}
}

func TestComposecovExecuteComposeServiceFailureMarksComponent(t *testing.T) {
	composecovShrinkTimers(t)
	q, keyring, logger, _ := composecovDeps(t)
	rt := composecovStackRuntime("composecov-net")
	rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispecPlatformAlias, string) (containertypes.CreateResponse, error) {
		return containertypes.CreateResponse{}, errors.New("no space left on device")
	}
	r := composecovRunner(t, q, keyring, logger, rt)
	r.service = &store.Service{ID: 1, ComposeContent: `
services:
  web:
    image: nginx
`}
	r.client = composecovDial(t, composecovNewSSH(t, func(string) (string, uint32) { return "", 0 }))
	err := r.executeCompose(context.Background(), composecovAppUUID,
		"/var/lib/akerdock/services/"+composecovAppUUID, "")
	if err == nil || !strings.Contains(err.Error(), "service web") {
		t.Fatalf("service failure = %v", err)
	}
}

// TestComposecovExecuteComposeResume drives the §8.2 per-service resume: a
// stale candidate is discarded and redone, an absent candidate falls through
// to the normal path, and a healthy survivor finishes its switch only.
func TestComposecovExecuteComposeResume(t *testing.T) {
	composecovShrinkTimers(t)
	q, keyring, logger, _ := composecovDeps(t)
	rt := composecovRuntime()
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return nil, nil
	}
	rt.ContainerInspectFn = func(_ context.Context, name string) (containertypes.InspectResponse, error) {
		switch {
		case strings.HasSuffix(name, "-alive-next"):
			return containertypes.InspectResponse{
				ContainerJSONBase: &containertypes.ContainerJSONBase{
					State: &containertypes.State{Running: true, Status: "running", Health: &containertypes.Health{Status: "healthy"}},
				},
				Config: &containertypes.Config{},
				NetworkSettings: &containertypes.NetworkSettings{
					Networks: map[string]*networktypes.EndpointSettings{
						"composecov-net": {IPAddress: "10.0.0.7"},
					},
				},
			}, nil
		case strings.HasSuffix(name, "-stale-next"):
			return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
				State: &containertypes.State{Running: false, Status: "exited"},
			}}, nil
		case strings.HasSuffix(name, "-next"):
			return containertypes.InspectResponse{}, composecovNotFound("container")
		default:
			return containertypes.InspectResponse{
				ContainerJSONBase: &containertypes.ContainerJSONBase{
					State: &containertypes.State{Running: true, Status: "running"},
				},
				Config: &containertypes.Config{Labels: map[string]string{}},
			}, nil
		}
	}
	composecovScriptExec(rt, composecovVerifyOutput(composecovAppUUID))

	r := composecovRunner(t, q, keyring, logger, rt)
	r.d.Status = store.DeploymentStatusStarting // a crashed attempt resumes here
	r.service = &store.Service{ID: 1, ComposeContent: `
services:
  stale:
    image: acme/stale:1
  gone:
    image: acme/gone:1
  alive:
    image: acme/alive:1
`}
	r.client = composecovDial(t, composecovNewSSH(t, func(string) (string, uint32) { return "", 0 }))
	if err := r.executeCompose(context.Background(), composecovAppUUID,
		"/var/lib/akerdock/services/"+composecovAppUUID, ""); err != nil {
		t.Fatalf("resumed deploy: %v", err)
	}

	var renamed []string
	creates := 0
	for _, c := range rt.Calls() {
		switch c.Method {
		case "ContainerRename":
			renamed = append(renamed, c.Args[0].(string))
		case "ContainerCreate":
			creates++
		}
	}
	// Only the surviving candidate is promoted; stale and gone are redone.
	if len(renamed) != 1 || !strings.HasSuffix(renamed[0], "-alive-next") {
		t.Fatalf("renamed = %v", renamed)
	}
	if creates != 2 {
		t.Fatalf("creates = %d — stale and gone must be recreated, alive must not", creates)
	}
}

func TestComposecovResumeComposeServiceBranches(t *testing.T) {
	composecovShrinkTimers(t)
	q, keyring, logger, _ := composecovDeps(t)
	plan := loadPlan(t, "services:\n  web:\n    image: nginx\n")
	sp := plan.Services[0]

	// Inspect failure propagates.
	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{}, errors.New("daemon down")
	}
	r := composecovRunner(t, q, keyring, logger, rt)
	if _, err := r.resumeComposeService(context.Background(), plan, sp, composecovAppUUID, nil); err == nil {
		t.Fatal("inspect error must propagate")
	}

	// A candidate that survived but never turns healthy is discarded and the
	// service is redone from scratch (nil error, done=false).
	rt = composecovRuntime()
	calls := 0
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		calls++
		if calls <= 2 {
			return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
				State: &containertypes.State{Running: true, Status: "running"},
			}}, nil
		}
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: false, Status: "exited"},
		}}, nil
	}
	r = composecovRunner(t, q, keyring, logger, rt)
	done, err := r.resumeComposeService(context.Background(), plan, sp, composecovAppUUID, map[string]int64{"web": 1})
	if err != nil || done {
		t.Fatalf("unhealthy survivor = done %v err %v", done, err)
	}

	// A healthy survivor whose promotion fails surfaces the failure as done.
	rt = composecovRuntime()
	rt.ContainerInspectFn = func(_ context.Context, name string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{
			ContainerJSONBase: &containertypes.ContainerJSONBase{
				State: &containertypes.State{Running: true, Status: "running", Health: &containertypes.Health{Status: "healthy"}},
			},
			// No NetworkSettings: the candidate IP cannot resolve.
		}, nil
	}
	r = composecovRunner(t, q, keyring, logger, rt)
	done, err = r.resumeComposeService(context.Background(), plan, sp, composecovAppUUID, nil)
	if !done || err == nil || !strings.Contains(err.Error(), "could not resolve the candidate IP") {
		t.Fatalf("failed promotion = done %v err %v", done, err)
	}
}

func TestComposecovZeroDowntimeReplaceFailuresDiscardCandidate(t *testing.T) {
	composecovShrinkTimers(t)
	q, keyring, logger, db := composecovDeps(t)
	plan := loadPlan(t, `
services:
  web:
    image: nginx
    healthcheck:
      test: ["CMD-SHELL", "true"]
      interval: 1s
      timeout: 1s
      retries: 1
    x-akerdock:
      post_deployment_command: "run-checks"
`)
	sp := plan.Services[0]
	env := []string{"K=v"}

	discarded := func(rt *fake.Runtime) bool {
		for _, c := range rt.Calls() {
			if c.Method == "ContainerRemove" && strings.HasSuffix(c.Args[0].(string), "-next") {
				return true
			}
		}
		return false
	}

	// Create fails: candidate cleaned up, error surfaced.
	rt := composecovRuntime()
	rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispecPlatformAlias, string) (containertypes.CreateResponse, error) {
		return containertypes.CreateResponse{}, errors.New("boom")
	}
	r := composecovRunner(t, q, keyring, logger, rt)
	if err := r.zeroDowntimeReplace(context.Background(), plan, sp, "/app", composecovAppUUID, nil, env, "nginx"); err == nil {
		t.Fatal("create failure must fail the switch")
	}
	if !discarded(rt) {
		t.Fatal("candidate not discarded after create failure")
	}

	// The candidate never turns healthy: discarded.
	rt = composecovRuntime()
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: true, Status: "running", Health: &containertypes.Health{Status: "unhealthy"}},
		}}, nil
	}
	r = composecovRunner(t, q, keyring, logger, rt)
	if err := r.zeroDowntimeReplace(context.Background(), plan, sp, "/app", composecovAppUUID, nil, env, "nginx"); err == nil {
		t.Fatal("unhealthy candidate must fail the switch")
	}
	if !discarded(rt) {
		t.Fatal("candidate not discarded after failed health")
	}

	// The post hook fails in the healthy candidate: discarded, old serving.
	rt = composecovStackRuntime("composecov-net")
	rt.ContainerExecInspectFn = func(context.Context, string) (containertypes.ExecInspect, error) {
		return containertypes.ExecInspect{ExitCode: 3}, nil
	}
	r = composecovRunner(t, q, keyring, logger, rt)
	if err := r.zeroDowntimeReplace(context.Background(), plan, sp, "/app", composecovAppUUID, nil, env, "nginx"); err == nil {
		t.Fatal("post hook failure must fail the switch")
	}
	if !discarded(rt) {
		t.Fatal("candidate not discarded after post hook failure")
	}

	// A cancellation at the barrier discards the candidate too.
	db.rowFns["-- name: IsJobCancelRequested "] = func() pgx.Row {
		return composecovRow{fill: func(dest []any) error {
			*(dest[0].(*bool)) = true
			return nil
		}}
	}
	rt = composecovStackRuntime("composecov-net")
	r = composecovRunner(t, q, keyring, logger, rt)
	if err := r.zeroDowntimeReplace(context.Background(), plan, sp, "/app", composecovAppUUID, nil, env, "nginx"); !errors.Is(err, errCancelled) {
		t.Fatalf("cancel at the barrier = %v", err)
	}
	if !discarded(rt) {
		t.Fatal("candidate not discarded after cancellation")
	}
}

func TestComposecovReplaceComposeServiceSkipsUnchanged(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)
	plan := loadPlan(t, "services:\n  web:\n    image: nginx\n    environment:\n      A: \"1\"\n")
	sp := plan.Services[0]
	appDir := "/apps/x"

	serviceEnv, envKeys, envContent := composeServiceEnv(sp)
	stackKeys := envEntryKeys(nil)
	allKeys := append(append([]string{}, stackKeys...), envKeys...)
	rBase := composecovRunner(t, q, keyring, logger, nil)
	envPath := fmt.Sprintf("%s/env/%s.sh", appDir, sp.Name)
	hashV1 := composeConfigHash(
		rBase.composeCreateCommand(plan, sp, appDir, "", envPath, allKeys, "nginx", composeCreateOpts{Name: sp.ContainerName, Aliases: sp.Aliases}),
		envContent)
	hashV2 := composeConfigHashV2(buildComposeCreateSpec(plan, sp, appDir, nil, serviceEnv, "nginx",
		composeCreateOpts{Name: sp.ContainerName, Aliases: sp.Aliases}))

	inspectWith := func(labels map[string]string) *fake.Runtime {
		rt := composecovRuntime()
		rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return containertypes.InspectResponse{
				ContainerJSONBase: &containertypes.ContainerJSONBase{State: &containertypes.State{Status: "running"}},
				Config:            &containertypes.Config{Labels: labels},
			}, nil
		}
		return rt
	}

	// v2 label matches: untouched, only the network convergence runs.
	rt := inspectWith(map[string]string{"akerdock.config_hash_v2": hashV2})
	r := composecovRunner(t, q, keyring, logger, rt)
	if err := r.replaceComposeService(context.Background(), plan, sp, appDir, composecovAppUUID, "", stackKeys, false, composeImage{Ref: "nginx"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range rt.CallNames() {
		if c == "ContainerCreate" || c == "ContainerRemove" {
			t.Fatalf("unchanged service was mutated: %v", rt.CallNames())
		}
	}

	// Pre-rollout container: the frozen v1 hash decides the skip.
	rt = inspectWith(map[string]string{"akerdock.config_hash": hashV1})
	r = composecovRunner(t, q, keyring, logger, rt)
	if err := r.replaceComposeService(context.Background(), plan, sp, appDir, composecovAppUUID, "", stackKeys, true, composeImage{Ref: "nginx"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range rt.CallNames() {
		if c == "ContainerCreate" {
			t.Fatal("v1-matched service was recreated")
		}
	}

	// force_rebuild ignores the matching hash and recreates.
	rt = inspectWith(map[string]string{"akerdock.config_hash_v2": hashV2})
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: true, Status: "running"},
		}}, nil
	}
	composecovShrinkTimers(t)
	r = composecovRunner(t, q, keyring, logger, rt)
	r.d.ForceRebuild = true
	if err := r.replaceComposeService(context.Background(), plan, sp, appDir, composecovAppUUID, "", stackKeys, false, composeImage{Ref: "nginx"}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range rt.CallNames() {
		if c == "ContainerCreate" {
			found = true
		}
	}
	if !found {
		t.Fatal("force_rebuild must recreate")
	}

	// A routed preview service recreates instead of switching.
	rt = composecovStackRuntime("composecov-net")
	r = composecovRunner(t, q, keyring, logger, rt)
	fqdn := "pr-3.example.test"
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, composecovPreviewUUID), PrID: 3, Fqdn: &fqdn}
	if err := r.replaceComposeService(context.Background(), plan, sp, appDir, composecovPreviewUUID, "", stackKeys, true, composeImage{Ref: "nginx"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range rt.CallNames() {
		if c == "ContainerRename" {
			t.Fatal("preview must never zero-downtime switch")
		}
	}
}

func TestComposecovRecreateOneShotVerdicts(t *testing.T) {
	composecovShrinkTimers(t)
	q, keyring, logger, _ := composecovDeps(t)
	plan := loadPlan(t, "services:\n  migrate:\n    image: acme/migrate:1\n    restart: \"no\"\n")
	sp := plan.Services[0]

	// Non-zero exit fails deterministically.
	rt := composecovRuntime()
	rt.ContainerWaitFn = func(context.Context, string, containertypes.WaitCondition) (<-chan containertypes.WaitResponse, <-chan error) {
		waitCh := make(chan containertypes.WaitResponse, 1)
		waitCh <- containertypes.WaitResponse{StatusCode: 9}
		return waitCh, make(chan error, 1)
	}
	r := composecovRunner(t, q, keyring, logger, rt)
	err := r.recreateComposeService(context.Background(), plan, sp, "/app", nil, nil, "acme/migrate:1", false)
	if err == nil || !strings.Contains(err.Error(), "exited non-zero (exit=9)") {
		t.Fatalf("one-shot failure = %v", err)
	}

	// A wait transport error surfaces as-is.
	rt = composecovRuntime()
	rt.ContainerWaitFn = func(context.Context, string, containertypes.WaitCondition) (<-chan containertypes.WaitResponse, <-chan error) {
		errCh := make(chan error, 1)
		errCh <- errors.New("wait transport broken")
		return make(chan containertypes.WaitResponse, 1), errCh
	}
	r = composecovRunner(t, q, keyring, logger, rt)
	if err := r.recreateComposeService(context.Background(), plan, sp, "/app", nil, nil, "acme/migrate:1", false); err == nil {
		t.Fatal("wait error must propagate")
	}

	// exclude_from_hc skips the health wait of a long-running service.
	plan2 := loadPlan(t, "services:\n  side:\n    image: nginx\n    x-akerdock:\n      exclude_from_hc: true\n")
	rt = composecovRuntime()
	r = composecovRunner(t, q, keyring, logger, rt)
	if err := r.recreateComposeService(context.Background(), plan2, plan2.Services[0], "/app", nil, nil, "nginx", false); err != nil {
		t.Fatal(err)
	}
}

func TestComposecovSwitchComponentRoutingModes(t *testing.T) {
	composecovShrinkTimers(t)
	q, keyring, logger, _ := composecovDeps(t)

	// Non-Traefik server: nothing to switch.
	r := composecovRunner(t, q, keyring, logger, &fake.Runtime{})
	if err := r.switchComponentRouting(context.Background(), composecovAppUUID, "web", "10.0.0.9"); err != nil {
		t.Fatal(err)
	}

	// Scale-to-zero: candidate steps are skipped entirely…
	rt := composecovRuntime()
	r = composecovRunner(t, q, keyring, logger, rt)
	r.server.ProxyType = store.ProxyTypeTraefik
	r.app.Application.ScaleToZero = true
	if err := r.switchComponentRouting(context.Background(), composecovAppUUID, "web", "10.0.0.9"); err != nil {
		t.Fatal(err)
	}
	if len(rt.CallNames()) != 0 {
		t.Fatalf("candidate switch of a scale-to-zero app must be inert, got %v", rt.CallNames())
	}
	// …and the stable step re-applies the waker routing (here: removal, the
	// app having no routable domain).
	composecovScriptExec(rt, func(cmd []string) string { return "{}" })
	if err := r.switchComponentRouting(context.Background(), composecovAppUUID, "web", ""); err != nil {
		t.Fatal(err)
	}
}

func TestComposecovCloneForCompose(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)
	repo := "https://git.example.test/acme/app.git"
	appDir := "/var/lib/akerdock/applications/" + composecovAppUUID

	newRun := func(handler func(string) (string, uint32)) *deploymentRun {
		r := composecovRunner(t, q, keyring, logger, composecovRuntime())
		r.app.Application.GitRepositoryUrl = &repo
		r.client = composecovDial(t, composecovNewSSH(t, handler))
		return r
	}

	// Preview head, fork-style fetch ref, verified and annotated.
	r := newRun(composecovGitSSH("", composecovSHA))
	branch := "feature/x"
	sha := composecovSHA
	fqdn := "pr-7.example.test"
	r.preview = &store.Preview{
		ID: 1, Uuid: mustUUID(t, composecovPreviewUUID), PrID: 7,
		Provider: store.GitProviderGithub, HeadSha: &sha, SourceBranch: &branch, Fqdn: &fqdn,
	}
	srcDir, gotSHA, err := r.cloneForCompose(context.Background(), composecovAppUUID, appDir)
	if err != nil || gotSHA != composecovSHA || !strings.HasPrefix(srcDir, appDir+"/source/") {
		t.Fatalf("preview clone = %q %q %v", srcDir, gotSHA, err)
	}
	if r.d.CommitAuthor == nil || *r.d.CommitAuthor != "Alice" {
		t.Fatalf("commit author = %v", r.d.CommitAuthor)
	}
	if r.d.CommitMessage == nil || *r.d.CommitMessage != "fix: compose" {
		t.Fatalf("commit message = %v", r.d.CommitMessage)
	}

	// The PR moved between delivery and clone: refused.
	moved := strings.ReplaceAll(composecovSHA, "0", "f")
	r = newRun(composecovGitSSH("", moved))
	r.preview = &store.Preview{
		ID: 1, Uuid: mustUUID(t, composecovPreviewUUID), PrID: 7,
		Provider: store.GitProviderGithub, HeadSha: &sha, SourceBranch: &branch,
	}
	if _, _, err := r.cloneForCompose(context.Background(), composecovAppUUID, appDir); err == nil ||
		!strings.Contains(err.Error(), "the pull request moved") {
		t.Fatalf("moved PR = %v", err)
	}

	// skip_build pins the deployed commit without resolving the branch.
	lsRemoteCalled := false
	r = newRun(func(cmd string) (string, uint32) {
		if strings.Contains(cmd, "git ls-remote") {
			lsRemoteCalled = true
		}
		return "", 0
	})
	r.d.SkipBuild = true
	deployed := composecovSHA
	r.d.CommitSha = &deployed
	if _, gotSHA, err := r.cloneForCompose(context.Background(), composecovAppUUID, appDir); err != nil || gotSHA != deployed {
		t.Fatalf("skip_build clone = %q %v", gotSHA, err)
	}
	if lsRemoteCalled {
		t.Fatal("skip_build must not resolve the branch")
	}

	// An unknown branch is a named failure.
	r = newRun(func(cmd string) (string, uint32) { return "", 0 })
	if _, _, err := r.cloneForCompose(context.Background(), composecovAppUUID, appDir); err == nil ||
		!strings.Contains(err.Error(), `branch "main" not found`) {
		t.Fatalf("unknown branch = %v", err)
	}
}

func TestComposecovPlainEnvVars(t *testing.T) {
	q, keyring, logger, db := composecovDeps(t)
	envBlob, err := keyring.Encrypt("environment_variables", "value_enc", jobFixtureUUID, []byte("cors={{deployment.fqdn}}"))
	if err != nil {
		t.Fatal(err)
	}

	// Production: the stack variable decrypts, interpolates its deployment
	// reference, and the predefined FQDN/URL variables appear.
	db.rows["-- name: ListEnvVarsForDeploy "] = func() pgx.Rows {
		return &composecovRows{remaining: 1, blob: envBlob}
	}
	db.rows["-- name: ListDomainsForApplication "] = func() pgx.Rows {
		return &composecovRows{remaining: 1}
	}
	r := composecovRunner(t, q, keyring, logger, nil)
	vars, err := r.plainEnvVars(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if vars["unit"] != "cors=unit" {
		t.Fatalf("interpolated value = %q", vars["unit"])
	}
	if vars["AKERDOCK_FQDN"] != "unit" || vars["AKERDOCK_URL"] != "https://unit" {
		t.Fatalf("predefined vars = %v", vars)
	}

	// A corrupt ciphertext is a named decrypt failure.
	db.rows["-- name: ListEnvVarsForDeploy "] = func() pgx.Rows {
		return &composecovRows{remaining: 1, blob: []byte("garbage")}
	}
	if _, err := r.plainEnvVars(context.Background()); err == nil || !strings.Contains(err.Error(), "decrypt variable") {
		t.Fatalf("decrypt failure = %v", err)
	}

	// Preview: the dedicated set resolves plus the preview identity.
	db.rows["-- name: ListPreviewEnvVars "] = func() pgx.Rows {
		return &composecovRows{remaining: 1, blob: envBlob}
	}
	fqdn := "pr-4.example.test"
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, composecovPreviewUUID), PrID: 4, Fqdn: &fqdn}
	vars, err = r.plainEnvVars(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if vars["AKERDOCK_PR_ID"] != "4" || vars["AKERDOCK_FQDN"] != fqdn || vars["AKERDOCK_URL"] != "https://"+fqdn {
		t.Fatalf("preview vars = %v", vars)
	}
	if vars["unit"] != "cors="+fqdn {
		t.Fatalf("preview interpolation = %q", vars["unit"])
	}

	// A fork preview still resolves its own set (shared scopes skipped).
	r.preview.IsFork = true
	if _, err := r.plainEnvVars(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Preview decrypt failure propagates too.
	db.rows["-- name: ListPreviewEnvVars "] = func() pgx.Rows {
		return &composecovRows{remaining: 1, blob: []byte("garbage")}
	}
	if _, err := r.plainEnvVars(context.Background()); err == nil || !strings.Contains(err.Error(), "decrypt variable") {
		t.Fatalf("preview decrypt failure = %v", err)
	}
}

func TestComposecovEnsureMagicVariables(t *testing.T) {
	q, keyring, logger, db := composecovDeps(t)
	content := `
services:
  unit:
    image: nginx
    expose: ["3000"]
    environment:
      PASSWORD: ${SERVICE_PASSWORD_UNIT}
      FQDN: ${SERVICE_FQDN_UNIT}
      URL: ${SERVICE_URL_UNIT}
`
	plan := loadPlan(t, strings.ReplaceAll(strings.ReplaceAll(content, "${SERVICE_PASSWORD_UNIT}", "x"),
		"${SERVICE_FQDN_UNIT}", "y"))
	services := []string{"unit"}

	// Production with a server wildcard: the password is generated and
	// persisted, the FQDN/URL intent creates a component domain.
	wildcard := "apps.example.test"
	r := composecovRunner(t, q, keyring, logger, nil)
	r.server.WildcardDomain = &wildcard
	vars := map[string]string{}
	if err := r.ensureMagicVariables(context.Background(), content, plan, services, vars); err != nil {
		t.Fatal(err)
	}
	if vars["SERVICE_PASSWORD_UNIT"] == "" {
		t.Fatal("password not generated")
	}
	wantFQDN := "unit-" + composecovAppUUID[:8] + "." + wildcard
	if vars["SERVICE_FQDN_UNIT"] != wantFQDN || vars["SERVICE_URL_UNIT"] != "https://"+wantFQDN {
		t.Fatalf("magic fqdn = %q url = %q", vars["SERVICE_FQDN_UNIT"], vars["SERVICE_URL_UNIT"])
	}

	// An existing component domain wins over generation.
	db.rows["-- name: ListServiceComponentDomains "] = composecovPortRows(1, 3000)
	vars = map[string]string{}
	if err := r.ensureMagicVariables(context.Background(), content, plan, services, vars); err != nil {
		t.Fatal(err)
	}
	if vars["SERVICE_FQDN_UNIT"] != "unit" {
		t.Fatalf("existing domain fqdn = %q", vars["SERVICE_FQDN_UNIT"])
	}
	delete(db.rows, "-- name: ListServiceComponentDomains ")

	// No wildcard: the reference stays undefined.
	r2 := composecovRunner(t, q, keyring, logger, nil)
	vars = map[string]string{}
	if err := r2.ensureMagicVariables(context.Background(), content, plan, services, vars); err != nil {
		t.Fatal(err)
	}
	if _, ok := vars["SERVICE_FQDN_UNIT"]; ok {
		t.Fatal("fqdn must stay undefined without a wildcard")
	}

	// A conflicting generated domain loses: undefined, only warned.
	db.rowFns["-- name: CreateComponentDomain "] = func() pgx.Row {
		return composecovRow{err: pgx.ErrNoRows}
	}
	vars = map[string]string{}
	if err := r.ensureMagicVariables(context.Background(), content, plan, services, vars); err != nil {
		t.Fatal(err)
	}
	if _, ok := vars["SERVICE_FQDN_UNIT"]; ok {
		t.Fatal("conflicting fqdn must stay undefined")
	}
	delete(db.rowFns, "-- name: CreateComponentDomain ")

	// A raced insert defers to the stored value.
	db.execTags["-- name: CreateGeneratedEnvVar "] = "INSERT 0 0"
	vars = map[string]string{}
	if err := r.ensureMagicVariables(context.Background(), content, plan, services, vars); err != nil {
		t.Fatal(err)
	}
	if _, ok := vars["SERVICE_PASSWORD_UNIT"]; !ok {
		t.Fatal("raced credential must resolve from the store")
	}
	delete(db.execTags, "-- name: CreateGeneratedEnvVar ")

	// A preview persists its credentials in the preview set and resolves
	// FQDNs from its own derived routes.
	fqdn := "pr-6.example.test"
	rp := composecovRunner(t, q, keyring, logger, nil)
	rp.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, composecovPreviewUUID), PrID: 6, Fqdn: &fqdn}
	vars = map[string]string{}
	if err := rp.ensureMagicVariables(context.Background(), content, plan, services, vars); err != nil {
		t.Fatal(err)
	}
	if vars["SERVICE_PASSWORD_UNIT"] == "" {
		t.Fatal("preview password not generated")
	}
	if vars["SERVICE_FQDN_UNIT"] != fqdn {
		t.Fatalf("preview magic fqdn = %q", vars["SERVICE_FQDN_UNIT"])
	}

	// Variables already defined are never regenerated.
	prev := vars["SERVICE_PASSWORD_UNIT"]
	if err := rp.ensureMagicVariables(context.Background(), content, plan, services, vars); err != nil {
		t.Fatal(err)
	}
	if vars["SERVICE_PASSWORD_UNIT"] != prev {
		t.Fatal("existing variable was regenerated")
	}
}

func TestComposecovComposePreviewRoutes(t *testing.T) {
	q, keyring, logger, db := composecovDeps(t)
	content := `
services:
  unit:
    image: nginx
    expose: ["3000"]
  web:
    image: nginx
    expose: ["8000"]
    environment:
      FQDN: ${SERVICE_FQDN_WEB}
  silent:
    image: nginx
`
	plan := loadPlan(t, strings.ReplaceAll(content, "${SERVICE_FQDN_WEB}", "x"))

	// Without a resolved fqdn the preview runs unrouted; the map is cached.
	r := composecovRunner(t, q, keyring, logger, nil)
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, composecovPreviewUUID), PrID: 5}
	routes := r.composePreviewRoutes(context.Background(), content, plan)
	if len(routes) != 0 {
		t.Fatalf("unrouted preview = %v", routes)
	}
	if again := r.composePreviewRoutes(context.Background(), content, plan); len(again) != 0 {
		t.Fatal("cache must return the same map")
	}

	// Magic declaration + production parity: web via its magic ref, unit via
	// its component's domain; both served on "<service>-<base>".
	base := "pr-5.example.test"
	db.rows["-- name: ListServiceComponentDomains "] = composecovPortRows(1, 8081)
	db.rows["-- name: ListServiceComponents "] = composecovComponentRows(1, 3000)
	r = composecovRunner(t, q, keyring, logger, nil)
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, composecovPreviewUUID), PrID: 5, Fqdn: &base}
	routes = r.composePreviewRoutes(context.Background(), content, plan)
	if len(routes) != 2 {
		t.Fatalf("served routes = %v", routes)
	}
	if got := routes["web"]; got.FQDN != "web-"+base || got.Port != 8000 {
		t.Fatalf("web route = %+v", got)
	}
	if got := routes["unit"]; got.FQDN != "unit-"+base || got.Port != 8081 {
		t.Fatalf("unit route = %+v", got)
	}

	// Route templates (ADR-035): the {{service}} row patterns every served
	// service, an explicit row overrides the component matching its port.
	r = composecovRunner(t, q, keyring, logger, nil)
	r.app.Application.PreviewUrlTemplates = []byte(
		`[{"host":"{{service}}-{{pr_id}}.preview.test","port":8090},{"host":"explicit-{{pr_id}}.preview.test","port":3000}]`)
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, composecovPreviewUUID), PrID: 5, Fqdn: &base}
	routes = r.composePreviewRoutes(context.Background(), content, plan)
	if got := routes["unit"]; got.FQDN != "explicit-5.preview.test" || got.Port != 3000 {
		t.Fatalf("explicit template route = %+v", got)
	}
	if got := routes["web"]; got.FQDN != "web-5.preview.test" || got.Port != 8090 {
		t.Fatalf("service template route = %+v", got)
	}
}

func TestComposecovApplyComposePreviewRouting(t *testing.T) {
	composecovShrinkTimers(t)
	q, keyring, logger, _ := composecovDeps(t)
	content := "services:\n  web:\n    image: nginx\n    expose: [\"3000\"]\n    environment:\n      FQDN: ${SERVICE_FQDN_WEB}\n"
	plan := loadPlan(t, strings.ReplaceAll(content, "${SERVICE_FQDN_WEB}", "x"))
	fqdn := "pr-2.example.test"

	// A non-Traefik server routes nothing.
	r := composecovRunner(t, q, keyring, logger, &fake.Runtime{})
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, composecovPreviewUUID), PrID: 2, Fqdn: &fqdn}
	if err := r.applyComposePreviewRouting(context.Background(), content, plan, composecovPreviewUUID); err != nil {
		t.Fatal(err)
	}

	// Traefik: the preview's routes are rendered and verified.
	rt := composecovStackRuntime("composecov-net", composecovPreviewUUID)
	r = composecovRunner(t, q, keyring, logger, rt)
	r.server.ProxyType = store.ProxyTypeTraefik
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, composecovPreviewUUID), PrID: 2, Fqdn: &fqdn}
	if err := r.applyComposePreviewRouting(context.Background(), content, plan, composecovPreviewUUID); err != nil {
		t.Fatal(err)
	}
	hops := r.hops.(*hostfake.Ops)
	writes := hops.CallsTo(agentwire.MethodFileWrite)
	if len(writes) != 1 {
		t.Fatalf("routing writes = %d", len(writes))
	}
	written := string(writes[0].(agentwire.FileWriteParams).Content)
	if !strings.Contains(written, fqdn) {
		t.Fatalf("routing content misses the preview fqdn:\n%s", written)
	}
}

func TestComposecovReportFindings(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)
	r := composecovRunner(t, q, keyring, logger, nil)

	// Warnings alone pass, traced in the step.
	warn := []compose.Finding{{Code: "compose_warning", Severity: compose.Warning, Message: "heads up"}}
	if err := r.reportFindings(context.Background(), warn); err != nil {
		t.Fatal(err)
	}

	// Any error blocks.
	bad := append(warn, compose.Finding{Code: "compose_error", Severity: compose.Error, Service: "web", Message: "refused"})
	if err := r.reportFindings(context.Background(), bad); err == nil ||
		!strings.Contains(err.Error(), "compose validation failed") {
		t.Fatalf("blocking findings = %v", err)
	}

	// No findings at all short-circuits into a skipped step.
	if err := r.reportFindings(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestComposecovSyncComponents(t *testing.T) {
	q, keyring, logger, db := composecovDeps(t)
	engine := "postgresql"
	plan := &compose.Plan{Services: []compose.ServicePlan{
		{Name: "web", Image: "nginx:1.27", DefaultRoutePort: 8080},
		{Name: "db", Image: "postgres:16", IsDatabase: true, DatabaseEngine: engine},
	}}
	// Field name check happens at compile time; run the sync.
	r := composecovRunner(t, q, keyring, logger, nil)
	ids, err := r.syncComponents(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids["web"] == 0 || ids["db"] == 0 {
		t.Fatalf("component ids = %v", ids)
	}

	// Upsert failure names the component.
	db.rowFns["-- name: UpsertServiceComponent "] = func() pgx.Row {
		return composecovRow{err: errors.New("constraint violated")}
	}
	if _, err := r.syncComponents(context.Background(), plan); err == nil ||
		!strings.Contains(err.Error(), "sync component web") {
		t.Fatalf("upsert failure = %v", err)
	}
	delete(db.rowFns, "-- name: UpsertServiceComponent ")

	// The vanished-row cleanup failure propagates.
	db.execErrs["-- name: DeleteVanishedServiceComponents "] = errors.New("delete blocked")
	if _, err := r.syncComponents(context.Background(), plan); err == nil ||
		!strings.Contains(err.Error(), "delete blocked") {
		t.Fatalf("delete failure = %v", err)
	}
}

func TestComposecovSyncStackStorages(t *testing.T) {
	q, keyring, logger, db := composecovDeps(t)
	plan := &compose.Plan{
		Volumes:         map[string]string{"data": "uuid_data"},
		ExternalVolumes: map[string]string{"legacy": "legacy_vol"},
		Services: []compose.ServicePlan{
			{Name: "web", Mounts: []compose.MountPlan{
				{Type: "volume", Source: "uuid_data", Target: "/srv/data"},
				{Type: "bind", Source: "/host", Target: "/host"},
			}},
			{Name: "db", Mounts: []compose.MountPlan{
				{Type: "volume", Source: "uuid_data", Target: "/dup"},     // first mount wins
				{Type: "volume", Source: "legacy_vol", Target: "/legacy"}, // external
				{Type: "volume", Source: "unknown_vol", Target: "/x"},     // not declared
			}},
		},
	}
	r := composecovRunner(t, q, keyring, logger, nil)
	if err := r.syncStackStorages(context.Background(), plan); err != nil {
		t.Fatal(err)
	}

	db.execErrs["-- name: CreateGeneratedStorage "] = errors.New("insert blocked")
	if err := r.syncStackStorages(context.Background(), plan); err == nil {
		t.Fatal("insert failure must propagate")
	}
	delete(db.execErrs, "-- name: CreateGeneratedStorage ")

	db.execErrs["-- name: DeleteGeneratedStoragesForResource "] = errors.New("delete blocked")
	if err := r.syncStackStorages(context.Background(), plan); err == nil {
		t.Fatal("delete failure must propagate")
	}
}

func TestComposecovEnsureComposeImage(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)
	pullPlan := loadPlan(t, "services:\n  web:\n    image: ghcr.io/acme/web:1.2\n")
	buildPlan := loadPlan(t, `
services:
  app:
    build:
      context: ./app
      dockerfile: Dockerfile.dev
      args:
        FOO: bar
        NILARG:
`)
	sha := "0123456789abcdef0123456789abcdef01234567"

	// skip_build with the image still on the server: reused without a pull.
	rt := composecovRuntime()
	r := composecovRunner(t, q, keyring, logger, rt)
	r.d.SkipBuild = true
	img, err := r.ensureComposeImage(context.Background(), pullPlan.Services[0], "/src", sha)
	if err != nil {
		t.Fatal(err)
	}
	if img.Ref != "registry.example/app@sha256:feed" {
		t.Fatalf("reused ref = %q", img.Ref)
	}
	for _, c := range rt.CallNames() {
		if c == "ImagePull" {
			t.Fatal("skip_build must not pull a present image")
		}
	}

	// skip_build for a BUILT service reuses the sha-tagged local image.
	rt = composecovRuntime()
	r = composecovRunner(t, q, keyring, logger, rt)
	r.d.SkipBuild = true
	img, err = r.ensureComposeImage(context.Background(), buildPlan.Services[0], "/src", sha)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(img.Ref, ":"+sha[:12]) {
		t.Fatalf("reused build ref = %q", img.Ref)
	}

	// A build streams through the agent and resolves the produced image.
	rtBuild := composecovRuntime()
	var built agentwire.ImageBuildParams
	hops := &hostfake.Ops{BuildImageFn: func(_ context.Context, p agentwire.ImageBuildParams) (io.ReadCloser, error) {
		built = p
		return io.NopCloser(strings.NewReader("step 1/2\nstep 2/2\n")), nil
	}}
	r = composecovRunner(t, q, keyring, logger, rtBuild)
	r.hops = hops
	img, err = r.ensureComposeImage(context.Background(), buildPlan.Services[0], "/src", sha)
	if err != nil {
		t.Fatal(err)
	}
	if built.ContextDir != "/src/app" || built.Dockerfile != "Dockerfile.dev" {
		t.Fatalf("build params = %+v", built)
	}
	if built.BuildArgs["FOO"] != "bar" {
		t.Fatalf("build args = %v", built.BuildArgs)
	}
	if _, ok := built.BuildArgs["NILARG"]; ok {
		t.Fatal("nil build arg must be dropped")
	}
	if !strings.HasSuffix(img.Ref, ":"+sha[:12]) {
		t.Fatalf("built ref = %q — a built image is never digest-pinned", img.Ref)
	}

	// A failing build fails the service.
	r.hops = &hostfake.Ops{BuildImageFn: func(context.Context, agentwire.ImageBuildParams) (io.ReadCloser, error) {
		return nil, errors.New("buildkit unavailable")
	}}
	if _, err := r.ensureComposeImage(context.Background(), buildPlan.Services[0], "/src", sha); err == nil {
		t.Fatal("build failure must propagate")
	}

	// A failing pull fails the service.
	rt = composecovRuntime()
	rt.ImagePullFn = func(context.Context, string, imagetypes.PullOptions) (io.ReadCloser, error) {
		return nil, errors.New("registry unreachable")
	}
	r = composecovRunner(t, q, keyring, logger, rt)
	if _, err := r.ensureComposeImage(context.Background(), pullPlan.Services[0], "/src", sha); err == nil {
		t.Fatal("pull failure must propagate")
	}
}

func TestComposecovResolveComposeImage(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)
	plan := loadPlan(t, "services:\n  web:\n    image: nginx:1\n")
	sp := plan.Services[0]

	// The registry digest pins the pull; the image carries no healthcheck.
	rt2 := composecovRuntime()
	r := composecovRunner(t, q, keyring, logger, rt2)
	img, err := r.resolveComposeImage(context.Background(), sp, "nginx:1")
	if err != nil {
		t.Fatal(err)
	}
	if img.Ref != "registry.example/app@sha256:feed" || img.HasHealthcheck {
		t.Fatalf("resolved image = %+v", img)
	}

	// Inspect failure propagates.
	rt3 := composecovRuntime()
	rt3.ImageInspectFn = func(context.Context, string, ...client.ImageInspectOption) (imagetypes.InspectResponse, error) {
		return imagetypes.InspectResponse{}, errors.New("gone")
	}
	r = composecovRunner(t, q, keyring, logger, rt3)
	if _, err := r.resolveComposeImage(context.Background(), sp, "nginx:1"); err == nil {
		t.Fatal("inspect failure must propagate")
	}
}

func TestComposecovRoutedComponents(t *testing.T) {
	q, keyring, logger, db := composecovDeps(t)
	r := composecovRunner(t, q, keyring, logger, nil)

	// Component domains decide; without any, nothing is routed.
	routed, err := r.routedComponents(context.Background(), map[string]int64{"web": 1})
	if err != nil {
		t.Fatal(err)
	}
	if routed["web"] {
		t.Fatal("undomained component reported routed")
	}

	// A component domain routes its component.
	db.rows["-- name: ListServiceComponentDomains "] = composecovPortRows(1, 3000)
	routed, err = r.routedComponents(context.Background(), map[string]int64{"web": 1})
	if err != nil || !routed["web"] {
		t.Fatalf("domained component = %v %v", routed, err)
	}
	delete(db.rows, "-- name: ListServiceComponentDomains ")

	// An application-level domain routes the stack's resolved web component.
	db.rows["-- name: ListDomainsForApplication "] = composecovPortRows(1, 3000)
	db.rows["-- name: ListServiceComponents "] = composecovComponentRows(1, 3000)
	routed, err = r.routedComponents(context.Background(), map[string]int64{})
	if err != nil || !routed["unit"] {
		t.Fatalf("app-domain routing = %v %v", routed, err)
	}
}

// ocispecPlatformAlias keeps the ContainerCreateFn signatures readable.
type ocispecPlatformAlias = ocispec.Platform
