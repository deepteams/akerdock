package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/hostops"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/pguuid"
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

// RunInput records the command like Run; the input is host material the
// tests never re-read.
func (s *cleanupRemoteStub) RunInput(_ context.Context, command, _ string) (*sshexec.Result, error) {
	s.commands = append(s.commands, command)
	if s.run != nil {
		return s.run(command)
	}
	return s.result, s.err
}

func (*cleanupRemoteStub) Close() error { return nil }

// fixedSource hands every caller the same runtime — the job-test double of
// the per-server resolution.
type fixedSource struct {
	rt  dockerruntime.Runtime
	err error
}

func (s fixedSource) Runtime(context.Context, int64) (dockerruntime.Runtime, error) {
	return s.rt, s.err
}

// fixedHost is the ADR-054 twin: every caller gets the same host-ops.
type fixedHost struct {
	ops hostops.Ops
	err error
}

func (s fixedHost) HostOps(context.Context, int64) (hostops.Ops, error) {
	if s.ops == nil && s.err == nil {
		return &hostfake.Ops{}, nil
	}
	return s.ops, s.err
}

// cleanupFakeRuntime is a fake with the read paths every cleanup pass needs.
func cleanupFakeRuntime() *fake.Runtime {
	rt := &fake.Runtime{}
	rt.InfoFn = func(context.Context) (system.Info, error) {
		return system.Info{DockerRootDir: "/var/lib/docker"}, nil
	}
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return nil, nil
	}
	rt.NetworkListFn = func(context.Context, network.ListOptions) ([]network.Summary, error) {
		return nil, nil
	}
	return rt
}

func labelValues(t *testing.T, f filters.Args, key string) []string {
	t.Helper()
	values := f.Get(key)
	slices.Sort(values)
	return values
}

// TestServerCleanupStepInventory pins the destructive boundary: the complete
// step list, the positive managed-only filters of every typed prune, and the
// two host-side commands that stay on SSH.
func TestServerCleanupStepInventory(t *testing.T) {
	rt := cleanupFakeRuntime()
	remote := &cleanupRemoteStub{result: &sshexec.Result{Stdout: "Total reclaimed space: 1MB\n", ExitCode: 0}}
	h := &ServerCleanup{}

	steps := h.cleanupSteps(store.Server{CleanupPruneVolumes: true, CleanupPruneNetworks: true}, rt, remote)
	var names []string
	for _, step := range steps {
		names = append(names, step.name)
		if _, err := step.run(context.Background()); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}
	want := []string{
		"prune_build_cache", "prune_dangling_images", "prune_dead_candidates",
		"purge_tmp", "prune_anonymous_volumes", "prune_managed_networks",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("cleanup inventory = %v, want %v", names, want)
	}
	if defaults := h.cleanupSteps(store.Server{}, rt, remote); len(defaults) != 4 {
		t.Fatalf("default cleanup has %d steps, want the four non-destructive steps", len(defaults))
	}

	// Host-side commands: reclaim-all build cache with its warm floor, and a
	// tmp purge that neither misses dotfiles nor hides failures.
	if cmd := remote.commands[0]; !strings.Contains(cmd, "prune -af") || !strings.Contains(cmd, "--keep-storage 2GB") {
		t.Errorf("build cache command does not reclaim all unused cache with its reserve: %q", cmd)
	}
	if cmd := remote.commands[1]; !strings.Contains(cmd, "find ") ||
		strings.Contains(cmd, "/tmp/*") || strings.Contains(cmd, "echo done") {
		t.Errorf("tmp cleanup can miss dotfiles or hide errors: %q", cmd)
	}

	// Typed prunes: every one positively scoped to managed objects.
	for _, c := range rt.Calls() {
		switch c.Method {
		case "ImagesPrune":
			f := c.Args[0].(filters.Args)
			if got := labelValues(t, f, "label"); len(got) != 1 || got[0] != "akerdock.managed=true" {
				t.Errorf("image prune labels = %v", got)
			}
			if got := f.Get("dangling"); len(got) != 1 || got[0] != "true" {
				t.Errorf("image prune must stay dangling-only (tagged rollback artifacts survive): %v", got)
			}
		case "VolumesPrune", "NetworksPrune":
			f := c.Args[0].(filters.Args)
			if got := labelValues(t, f, "label"); len(got) != 1 || got[0] != "akerdock.managed=true" {
				t.Errorf("%s labels = %v", c.Method, got)
			}
		case "ContainerList":
			opts := c.Args[0].(containertypes.ListOptions)
			if !opts.All {
				t.Error("candidate cleanup must see every container state")
			}
			if got := labelValues(t, opts.Filters, "label"); len(got) != 1 || got[0] != "akerdock.managed=true" {
				t.Errorf("candidate listing labels = %v", got)
			}
		}
	}
}

func TestServerCleanupThresholdSkipsWithoutPruning(t *testing.T) {
	q, keyring, _, logger, db := jobFlowDependencies(t)
	threshold := int32(80)
	db.cleanupThreshold = &threshold
	rt := cleanupFakeRuntime()
	remote := &cleanupRemoteStub{result: &sshexec.Result{Stdout: "79\n", ExitCode: 0}}
	handler := &ServerCleanup{
		Store: q, Keyring: keyring, Logger: logger, Docker: fixedSource{rt: rt},
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
		t.Fatalf("threshold cleanup ran host commands below the threshold: %v", remote.commands)
	}
	if names := rt.CallNames(); len(names) != 1 || names[0] != "Info" {
		t.Fatalf("threshold cleanup ran typed prunes below the threshold: %v", names)
	}
}

func TestServerCleanupExecutesCompleteManagedInventory(t *testing.T) {
	q, keyring, _, logger, db := jobFlowDependencies(t)
	db.truthy = true // enable the opt-in managed volume and network passes
	measurements := []string{"91\n", "43\n"}
	rt := cleanupFakeRuntime()
	remote := &cleanupRemoteStub{}
	remote.run = func(command string) (*sshexec.Result, error) {
		if strings.HasPrefix(command, "df -P ") {
			value := measurements[0]
			measurements = measurements[1:]
			return &sshexec.Result{Stdout: value, ExitCode: 0}, nil
		}
		return &sshexec.Result{Stdout: "Total reclaimed space: 1MB\n", ExitCode: 0}, nil
	}
	handler := &ServerCleanup{
		Store: q, Keyring: keyring, Logger: logger, Docker: fixedSource{rt: rt},
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
	// Host side: measure + build cache + tmp + measure.
	if len(remote.commands) != 4 {
		t.Fatalf("cleanup executed %d host commands, want 4: %v", len(remote.commands), remote.commands)
	}
	// Typed side: two measures plus the four managed passes — networks are an
	// owner-aware LIST + selective removals now, never a blanket prune (a
	// blanket prune deleted sleeping scale-to-zero stack networks and broke
	// their wake).
	counts := map[string]int{}
	for _, name := range rt.CallNames() {
		counts[name]++
	}
	for name, want := range map[string]int{
		"Info": 2, "ImagesPrune": 1, "ContainerList": 1, "VolumesPrune": 1, "NetworkList": 1, "NetworksPrune": 0,
	} {
		if counts[name] != want {
			t.Errorf("%s ran %d times, want %d (calls: %v)", name, counts[name], want, rt.CallNames())
		}
	}
}

func TestServerCleanupStopsAndReportsPruneFailure(t *testing.T) {
	q, keyring, _, logger, _ := jobFlowDependencies(t)
	rt := cleanupFakeRuntime()
	rt.ImagesPruneFn = func(context.Context, filters.Args) (image.PruneReport, error) {
		return image.PruneReport{}, errors.New("daemon refused prune")
	}
	remote := &cleanupRemoteStub{result: &sshexec.Result{Stdout: "90\n", ExitCode: 0}}
	handler := &ServerCleanup{
		Store: q, Keyring: keyring, Logger: logger, Docker: fixedSource{rt: rt},
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
	// Measure + build cache only: the failed prune stops the pass.
	if len(remote.commands) != 2 {
		t.Fatalf("cleanup continued host commands after a failed prune: %v", remote.commands)
	}
	for _, name := range rt.CallNames() {
		if name == "VolumesPrune" || name == "NetworksPrune" {
			t.Fatalf("cleanup continued typed prunes after a failure: %v", rt.CallNames())
		}
	}
}

// TestPruneDeadCandidatesMatchesExactManagedCandidates pins the orphan rule:
// only a managed container whose name is exactly its resource's `-next`
// candidate goes — component and preview naming included, a service that
// happens to be CALLED "next" excluded.
func TestPruneDeadCandidatesMatchesExactManagedCandidates(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return []containertypes.Summary{
			{ID: "single_candidate", Names: []string{"/app-next"}, Labels: map[string]string{"akerdock.resource_uuid": "app"}},
			{ID: "compose_candidate", Names: []string{"/app-web-next"}, Labels: map[string]string{"akerdock.resource_uuid": "app", "akerdock.component": "web"}},
			{ID: "final_service_named_next", Names: []string{"/app-next"}, Labels: map[string]string{"akerdock.resource_uuid": "app", "akerdock.component": "next"}},
			{ID: "preview_candidate", Names: []string{"/preview-web-next"}, Labels: map[string]string{"akerdock.resource_uuid": "app", "akerdock.preview_uuid": "preview", "akerdock.component": "web"}},
			{ID: "regular", Names: []string{"/app-web"}, Labels: map[string]string{"akerdock.resource_uuid": "app", "akerdock.component": "web"}},
		}, nil
	}
	summary, err := pruneDeadCandidates(context.Background(), rt)
	if err != nil {
		t.Fatal(err)
	}
	if summary != "3 dead candidates removed" {
		t.Fatalf("summary = %q", summary)
	}
	var removed []string
	for _, c := range rt.Calls() {
		if c.Method != "ContainerRemove" {
			continue
		}
		removed = append(removed, c.Args[0].(string))
		opts := c.Args[1].(containertypes.RemoveOptions)
		if !opts.Force || !opts.RemoveVolumes {
			t.Fatalf("candidate removal options = %+v, want force + anonymous volumes", opts)
		}
	}
	slices.Sort(removed)
	want := []string{"compose_candidate", "preview_candidate", "single_candidate"}
	if strings.Join(removed, ",") != strings.Join(want, ",") {
		t.Fatalf("removed candidates = %v, want %v", removed, want)
	}

	rt.ContainerRemoveFn = func(context.Context, string, containertypes.RemoveOptions) error {
		return errors.New("rm refused")
	}
	if _, err := pruneDeadCandidates(context.Background(), rt); err == nil {
		t.Fatal("a failed candidate removal was hidden")
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
	rt := &fake.Runtime{}
	rt.InfoFn = func(context.Context) (system.Info, error) {
		return system.Info{DockerRootDir: "/docker-root"}, nil
	}
	remote := &cleanupRemoteStub{result: &sshexec.Result{Stdout: "87\n", ExitCode: 0}}
	got, err := (&ServerCleanup{}).diskUsagePct(context.Background(), rt, remote)
	if err != nil || got != 87 {
		t.Fatalf("disk usage = %d, %v; want 87", got, err)
	}
	if len(remote.commands) != 1 || !strings.Contains(remote.commands[0], "df -P '/docker-root'") {
		t.Fatalf("measurement does not target Docker Root Dir: %v", remote.commands)
	}

	brokenInfo := &fake.Runtime{}
	brokenInfo.InfoFn = func(context.Context) (system.Info, error) {
		return system.Info{}, errors.New("daemon down")
	}
	if _, err := (&ServerCleanup{}).diskUsagePct(context.Background(), brokenInfo, remote); err == nil {
		t.Fatal("a failed daemon info was accepted")
	}
	emptyRoot := &fake.Runtime{}
	emptyRoot.InfoFn = func(context.Context) (system.Info, error) { return system.Info{}, nil }
	if _, err := (&ServerCleanup{}).diskUsagePct(context.Background(), emptyRoot, remote); err == nil {
		t.Fatal("an empty docker root was accepted")
	}
	for name, stub := range map[string]*cleanupRemoteStub{
		"transport": {err: errors.New("ssh lost")},
		"exit":      {result: &sshexec.Result{ExitCode: 1, Stderr: "df failed"}},
		"empty":     {result: &sshexec.Result{ExitCode: 0}},
		"range":     {result: &sshexec.Result{ExitCode: 0, Stdout: "101\n"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (&ServerCleanup{}).diskUsagePct(context.Background(), rt, stub); err == nil {
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

// ownerStoreStub scripts the two liveness lookups of the orphan prune.
type ownerStoreStub struct {
	resources map[string]bool
	previews  map[string]bool
}

func (s ownerStoreStub) ListLiveResourceUUIDs(_ context.Context, uuids []pgtype.UUID) ([]pgtype.UUID, error) {
	var out []pgtype.UUID
	for _, u := range uuids {
		if s.resources[pguuid.String(u)] {
			out = append(out, u)
		}
	}
	return out, nil
}

func (s ownerStoreStub) ListLivePreviewUUIDs(_ context.Context, uuids []pgtype.UUID) ([]pgtype.UUID, error) {
	var out []pgtype.UUID
	for _, u := range uuids {
		if s.previews[pguuid.String(u)] {
			out = append(out, u)
		}
	}
	return out, nil
}

// TestPruneOrphanManagedNetworks pins the wake-safety contract: a SLEEPING
// scale-to-zero resource's stack network looks unused to Docker (stopped
// containers hold no endpoints) but MUST survive the cleanup — pruning it
// used to break the next wake with "network not found". Only networks whose
// owner is gone are orphans; destination networks (no owner label) are never
// this step's business, and an in-use refusal means "not an orphan".
func TestPruneOrphanManagedNetworks(t *testing.T) {
	liveRes := "11111111-1111-4111-8111-111111111111"
	sleptPrev := "22222222-2222-4222-8222-222222222222"
	dead := "33333333-3333-4333-8333-333333333333"
	inUse := "44444444-4444-4444-8444-444444444444"

	rt := &fake.Runtime{}
	rt.NetworkListFn = func(context.Context, network.ListOptions) ([]network.Summary, error) {
		return []network.Summary{
			{ID: "n-live", Labels: map[string]string{"akerdock.managed": "true", "akerdock.resource_uuid": liveRes}},
			{ID: "n-slept", Labels: map[string]string{"akerdock.managed": "true", "akerdock.preview_uuid": sleptPrev, "akerdock.resource_uuid": sleptPrev}},
			{ID: "n-dead", Labels: map[string]string{"akerdock.managed": "true", "akerdock.resource_uuid": dead}},
			{ID: "n-dest", Labels: map[string]string{"akerdock.managed": "true"}},
			{ID: "n-inuse", Labels: map[string]string{"akerdock.managed": "true", "akerdock.resource_uuid": inUse}},
		}, nil
	}
	var removed []string
	rt.NetworkRemoveFn = func(_ context.Context, id string) error {
		if id == "n-inuse" {
			return fmt.Errorf("network has active endpoints: %w", cerrdefs.ErrConflict)
		}
		removed = append(removed, id)
		return nil
	}

	summary, err := pruneOrphanManagedNetworks(context.Background(), rt, ownerStoreStub{
		resources: map[string]bool{liveRes: true},
		previews:  map[string]bool{sleptPrev: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "n-dead" {
		t.Fatalf("removed = %v — only the dead owner's network is an orphan", removed)
	}
	if summary != "1 networks removed" {
		t.Fatalf("summary = %q", summary)
	}
}
