package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

type cleanupRemoteStub struct {
	result   *sshexec.Result
	err      error
	commands []string
	run      func(string) (*sshexec.Result, error)
}

func (s *cleanupRemoteStub) Run(_ context.Context, command string) (*sshexec.Result, error) {
	s.commands = append(s.commands, command)
	if s.run != nil {
		return s.run(command)
	}
	return s.result, s.err
}

func (*cleanupRemoteStub) Close() error { return nil }

func TestServerCleanupPruneInventory(t *testing.T) {
	prunes := serverCleanupPrunes(store.Server{
		CleanupPruneVolumes:  true,
		CleanupPruneNetworks: true,
	})
	byName := make(map[string]string, len(prunes))
	for _, prune := range prunes {
		byName[prune.name] = prune.cmd
	}
	for _, name := range []string{
		"prune_build_cache",
		"prune_dangling_images",
		"prune_dead_candidates",
		"purge_tmp",
		"prune_anonymous_volumes",
		"prune_managed_networks",
	} {
		if byName[name] == "" {
			t.Errorf("cleanup inventory is missing %q", name)
		}
	}
	if cmd := byName["prune_build_cache"]; !strings.Contains(cmd, "prune -af") ||
		!strings.Contains(cmd, "--keep-storage 2GB") {
		t.Errorf("build cache command does not reclaim all unused cache with its reserve: %q", cmd)
	}
	for _, name := range []string{"prune_dangling_images", "prune_anonymous_volumes", "prune_managed_networks"} {
		if !strings.Contains(byName[name], "label=akerdock.managed=true") {
			t.Errorf("%s is not positively scoped to managed objects: %q", name, byName[name])
		}
	}
	if cmd := byName["prune_dead_candidates"]; strings.Contains(cmd, "status=exited") ||
		!strings.Contains(cmd, "docker rm -fv") {
		t.Errorf("candidate cleanup is state-limited or leaves anonymous volumes: %q", cmd)
	}
	if cmd := byName["purge_tmp"]; !strings.Contains(cmd, "find ") ||
		strings.Contains(cmd, "/tmp/*") || strings.Contains(cmd, "echo done") {
		t.Errorf("tmp cleanup can miss dotfiles or hide errors: %q", cmd)
	}

	defaultPrunes := serverCleanupPrunes(store.Server{})
	if len(defaultPrunes) != 4 {
		t.Fatalf("default cleanup has %d steps, want the four non-destructive steps", len(defaultPrunes))
	}
}

func TestServerCleanupThresholdSkipsWithoutPruning(t *testing.T) {
	q, keyring, _, logger, db := jobFlowDependencies(t)
	threshold := int32(80)
	db.cleanupThreshold = &threshold
	remote := &cleanupRemoteStub{result: &sshexec.Result{Stdout: "79\n", ExitCode: 0}}
	handler := &ServerCleanup{
		Store: q, Keyring: keyring, Logger: logger,
		dial: func(context.Context, string, int, string, string, time.Duration, string) (cleanupRemote, error) {
			return remote, nil
		},
	}
	job := store.Job{
		ID: 1, JobType: TypeServerCleanup,
		Payload: []byte(`{"server_id":1,"reason":"threshold"}`),
	}

	result, err := handler.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := result.(map[string]any)
	if !ok || payload["status"] != "skipped" || payload["disk_pct"] != 79 {
		t.Fatalf("threshold cleanup result = %#v", result)
	}
	if len(remote.commands) != 1 {
		t.Fatalf("threshold cleanup ran destructive commands below the threshold: %v", remote.commands)
	}
}

func TestServerCleanupExecutesCompleteManagedInventory(t *testing.T) {
	q, keyring, _, logger, db := jobFlowDependencies(t)
	db.truthy = true // enable the opt-in managed volume and network passes
	measurements := []string{"91\n", "43\n"}
	remote := &cleanupRemoteStub{}
	remote.run = func(command string) (*sshexec.Result, error) {
		if strings.Contains(command, ".DockerRootDir") {
			value := measurements[0]
			measurements = measurements[1:]
			return &sshexec.Result{Stdout: value, ExitCode: 0}, nil
		}
		return &sshexec.Result{Stdout: "Total reclaimed space: 1MB\n", ExitCode: 0}, nil
	}
	handler := &ServerCleanup{
		Store: q, Keyring: keyring, Logger: logger,
		dial: func(context.Context, string, int, string, string, time.Duration, string) (cleanupRemote, error) {
			return remote, nil
		},
	}
	job := store.Job{
		ID: 1, JobType: TypeServerCleanup,
		Payload: []byte(`{"server_id":1,"reason":"manual"}`),
	}

	result, err := handler.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := result.(map[string]any)
	if !ok || payload["status"] != "completed" ||
		payload["disk_pct_before"] != 91 || payload["disk_pct_after"] != 43 {
		t.Fatalf("completed cleanup result = %#v", result)
	}
	if len(remote.commands) != 8 { // measure + six cleanup resources + measure
		t.Fatalf("cleanup executed %d remote commands, want 8: %v", len(remote.commands), remote.commands)
	}
	for _, prune := range serverCleanupPrunes(store.Server{
		CleanupPruneVolumes: true, CleanupPruneNetworks: true,
	}) {
		if !containsString(remote.commands, prune.cmd) {
			t.Errorf("cleanup did not execute %s", prune.name)
		}
	}
}

func TestServerCleanupStopsAndReportsPruneFailure(t *testing.T) {
	q, keyring, _, logger, _ := jobFlowDependencies(t)
	remote := &cleanupRemoteStub{}
	remote.run = func(command string) (*sshexec.Result, error) {
		if strings.Contains(command, ".DockerRootDir") {
			return &sshexec.Result{Stdout: "90\n", ExitCode: 0}, nil
		}
		if strings.HasPrefix(command, "docker image prune") {
			return &sshexec.Result{Stderr: "daemon refused prune\n", ExitCode: 7}, nil
		}
		return &sshexec.Result{ExitCode: 0}, nil
	}
	handler := &ServerCleanup{
		Store: q, Keyring: keyring, Logger: logger,
		dial: func(context.Context, string, int, string, string, time.Duration, string) (cleanupRemote, error) {
			return remote, nil
		},
	}
	job := store.Job{
		ID: 1, JobType: TypeServerCleanup,
		Payload: []byte(`{"server_id":1,"reason":"manual"}`),
	}

	if _, err := handler.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err == nil ||
		!strings.Contains(err.Error(), "prune_dangling_images") {
		t.Fatalf("cleanup accepted a failed prune: %v", err)
	}
	if len(remote.commands) != 3 {
		t.Fatalf("cleanup continued after a failed prune: %v", remote.commands)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestCleanupCandidateCommandMatchesExactManagedCandidates(t *testing.T) {
	bin := t.TempDir()
	logPath := filepath.Join(bin, "removed")
	docker := filepath.Join(bin, "docker")
	script := `#!/bin/sh
case "$1" in
  ps)
    printf '%s\n' single_candidate compose_candidate final_service_named_next preview_candidate regular
    ;;
  inspect)
    for last do :; done
    case "$last" in
      single_candidate)  printf '%s\n' '/app-next|app||' ;;
      compose_candidate) printf '%s\n' '/app-web-next|app||web' ;;
      final_service_named_next) printf '%s\n' '/app-next|app||next' ;;
      preview_candidate) printf '%s\n' '/preview-web-next|app|preview|web' ;;
      regular)           printf '%s\n' '/app-web|app||web' ;;
    esac
    ;;
  rm)
    id=$3
    if [ "${CLEANUP_FAIL_ID:-}" = "$id" ]; then
      exit 7
    fi
    printf '%s\n' "$id" >> "$CLEANUP_TEST_LOG"
    ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(failID string) error {
		cmd := exec.Command("sh", "-c", cleanupCandidatesCommand())
		cmd.Env = append(os.Environ(),
			"PATH="+bin+":"+os.Getenv("PATH"),
			"CLEANUP_TEST_LOG="+logPath,
			"CLEANUP_FAIL_ID="+failID,
		)
		return cmd.Run()
	}
	if err := run(""); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(raw))
	sort.Strings(got)
	want := []string{"compose_candidate", "preview_candidate", "single_candidate"}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("removed candidates = %v, want %v", got, want)
	}

	if err := run("single_candidate"); err == nil {
		t.Fatal("docker rm failure was hidden by the candidate cleanup command")
	}
}

func TestCleanupTmpCommandRemovesDotfilesAndPropagatesFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "visible"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".hidden-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-c", cleanupTmpCommand(dir)).CombinedOutput(); err != nil {
		t.Fatalf("tmp cleanup failed: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp cleanup left entries: %v", entries)
	}

	if err := os.WriteFile(filepath.Join(dir, "again"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "rm"), []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", cleanupTmpCommand(dir))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if err := cmd.Run(); err == nil {
		t.Fatal("rm failure was hidden by the tmp cleanup command")
	}
}

func TestCleanupDiskUsageUsesDockerRootAndRejectsUnknownValues(t *testing.T) {
	remote := &cleanupRemoteStub{result: &sshexec.Result{Stdout: "87\n", ExitCode: 0}}
	got, err := (&ServerCleanup{}).diskUsagePct(context.Background(), remote)
	if err != nil || got != 87 {
		t.Fatalf("disk usage = %d, %v; want 87", got, err)
	}
	if len(remote.commands) != 1 || !strings.Contains(remote.commands[0], ".DockerRootDir") ||
		strings.Contains(remote.commands[0], "df -P /var/lib/akerdock") {
		t.Fatalf("measurement does not target Docker Root Dir: %v", remote.commands)
	}

	for name, stub := range map[string]*cleanupRemoteStub{
		"transport": {err: errors.New("ssh lost")},
		"exit":      {result: &sshexec.Result{ExitCode: 1, Stderr: "df failed"}},
		"empty":     {result: &sshexec.Result{ExitCode: 0}},
		"range":     {result: &sshexec.Result{ExitCode: 0, Stdout: "101\n"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (&ServerCleanup{}).diskUsagePct(context.Background(), stub); err == nil {
				t.Fatal("invalid disk measurement was accepted")
			}
		})
	}
}

func TestServerCleanupDefersDurably(t *testing.T) {
	q, keyring, _, logger, db := jobFlowDependencies(t)
	canCleanup := false
	db.canCleanup = &canCleanup
	job := store.Job{
		ID: 42, JobType: TypeServerCleanup, Payload: []byte(`{"server_id":1,"reason":"manual"}`),
		MaxAttempts: 5,
	}
	result, err := (&ServerCleanup{Store: q, Keyring: keyring, Logger: logger}).Execute(
		context.Background(), job, queue.NewStepRecorder(q, job))
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := result.(map[string]any)
	if !ok || payload["status"] != "deferred" || payload["retry_job_uuid"] == "" {
		t.Fatalf("deferred cleanup result = %#v", result)
	}
}

func TestQueuedDeploymentWaitsForRunningCleanup(t *testing.T) {
	db := &jobFlowDB{startDeploymentBlocks: 1}
	q := store.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	run := &deploymentRun{
		h:      &DeploymentRun{Store: q, Logger: logger},
		d:      store.Deployment{ID: 1, Status: store.DeploymentStatusQueued},
		server: store.Server{ID: 2},
	}
	oldInterval := deploymentCleanupPollInterval
	deploymentCleanupPollInterval = time.Millisecond
	t.Cleanup(func() { deploymentCleanupPollInterval = oldInterval })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := run.setStatus(ctx, store.DeploymentStatusPreparing); err != nil {
		t.Fatal(err)
	}
	if db.startDeploymentCalls != 2 {
		t.Fatalf("deployment start attempts = %d, want 2", db.startDeploymentCalls)
	}
	// Keep the in-memory status as queued: resume() uses the status loaded at
	// job start to distinguish a fresh deployment from crash recovery.
	if run.d.Status != store.DeploymentStatusQueued {
		t.Fatalf("in-memory deployment status = %q", run.d.Status)
	}
}

func TestCleanupWaitDoesNotResurrectSupersededDeployment(t *testing.T) {
	status := store.DeploymentStatusSuperseded
	db := &jobFlowDB{startDeploymentBlocks: 1, deploymentStatus: &status}
	q := store.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	run := &deploymentRun{
		h:      &DeploymentRun{Store: q, Logger: logger},
		d:      store.Deployment{ID: 1, Status: store.DeploymentStatusQueued},
		server: store.Server{ID: 2},
	}

	err := run.setStatus(context.Background(), store.DeploymentStatusPreparing)
	var stateChanged *deploymentStateChangedError
	if !errors.As(err, &stateChanged) || stateChanged.status != store.DeploymentStatusSuperseded {
		t.Fatalf("superseded deployment start error = %v", err)
	}
	if db.startDeploymentCalls != 1 {
		t.Fatalf("superseded deployment was retried %d times", db.startDeploymentCalls)
	}
}

func TestDeploymentWaitsBeforeReservingBuildServer(t *testing.T) {
	db := &jobFlowDB{assignBuildServerBlocks: 1}
	q := store.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	run := &deploymentRun{
		h: &DeploymentRun{Store: q, Logger: logger},
		d: store.Deployment{ID: 1},
	}
	oldInterval := deploymentCleanupPollInterval
	deploymentCleanupPollInterval = time.Millisecond
	t.Cleanup(func() { deploymentCleanupPollInterval = oldInterval })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	buildServer := store.Server{ID: 9}
	if err := run.reserveBuildServer(ctx, buildServer); err != nil {
		t.Fatal(err)
	}
	if db.assignBuildServerCalls != 2 {
		t.Fatalf("build-server reservation attempts = %d, want 2", db.assignBuildServerCalls)
	}
	if run.d.BuildServerID == nil || *run.d.BuildServerID != buildServer.ID {
		t.Fatalf("reserved build server = %v, want %d", run.d.BuildServerID, buildServer.ID)
	}
}
