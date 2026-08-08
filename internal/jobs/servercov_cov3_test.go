package jobs

// Coverage tests for servercleanup.go: the Execute failure ladder, the
// deferred-cleanup edge cases and the error branches of every prune helper.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/volume"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

func servercovCleanupJob(reason string) store.Job {
	return store.Job{
		ID: 21, JobType: TypeServerCleanup,
		Payload: []byte(`{"server_id":1,"reason":"` + reason + `"}`),
	}
}

func TestServercovCleanupRejectsUnknownReason(t *testing.T) {
	q, keyring, _, logger, _ := servercovDeps(t)
	job := servercovCleanupJob("curiosity")
	_, err := (&ServerCleanup{Store: q, Keyring: keyring, Logger: logger}).
		Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err == nil || !strings.Contains(err.Error(), "unknown cleanup reason") {
		t.Fatalf("error = %v", err)
	}
}

func TestServercovCleanupExecuteFailureLadder(t *testing.T) {
	canCleanup := false
	tests := map[string]struct {
		prepare func(t *testing.T, db *servercovDB, h *ServerCleanup)
		want    string
	}{
		"server vanished": {
			prepare: func(_ *testing.T, db *servercovDB, _ *ServerCleanup) {
				db.rowErr["GetServerByID"] = errors.New("gone")
			},
			want: "server vanished",
		},
		"cleanup guard query fails": {
			prepare: func(_ *testing.T, db *servercovDB, _ *ServerCleanup) {
				db.rowErr["CanStartServerCleanup"] = errors.New("lock unavailable")
			},
			want: "lock unavailable",
		},
		"deferred enqueue fails": {
			prepare: func(_ *testing.T, db *servercovDB, _ *ServerCleanup) {
				db.inner.canCleanup = &canCleanup
				db.rowErr["EnqueueJob"] = errors.New("queue refused")
			},
			want: "queue refused",
		},
		"private key decrypt fails": {
			prepare: func(_ *testing.T, db *servercovDB, h *ServerCleanup) {
				h.Docker = fixedSource{rt: cleanupFakeRuntime()}
				db.rowAfter["GetPrivateKeyByID"] = servercovOverride(map[int]func(any){
					7: servercovBytes([]byte("garbage")),
				})
			},
			want: "private key decrypt",
		},
		"ssh dial fails": {
			prepare: func(_ *testing.T, _ *servercovDB, h *ServerCleanup) {
				h.Docker = fixedSource{rt: cleanupFakeRuntime()}
				h.dial = func(context.Context, string, int, string, string, time.Duration, string) (cleanupRemote, error) {
					return nil, errors.New("dial refused")
				}
			},
			want: "dial refused",
		},
		"measure before fails": {
			prepare: func(_ *testing.T, _ *servercovDB, h *ServerCleanup) {
				rt := cleanupFakeRuntime()
				rt.InfoFn = func(context.Context) (system.Info, error) {
					return system.Info{}, errors.New("daemon down")
				}
				h.Docker = fixedSource{rt: rt}
				h.dial = servercovCleanupDial(&cleanupRemoteStub{result: &sshexec.Result{Stdout: "50\n"}})
			},
			want: "daemon down",
		},
		"record completion fails": {
			prepare: func(_ *testing.T, db *servercovDB, h *ServerCleanup) {
				db.execErr["SetServerCleanupSchedule"] = errors.New("write refused")
				h.Docker = fixedSource{rt: cleanupFakeRuntime()}
				h.dial = servercovCleanupDial(&cleanupRemoteStub{result: &sshexec.Result{Stdout: "50\n"}})
			},
			want: "record cleanup completion",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			q, keyring, recorder, logger, db := servercovDeps(t)
			h := &ServerCleanup{Store: q, Keyring: keyring, Audit: recorder, Logger: logger}
			tc.prepare(t, db, h)
			job := servercovCleanupJob("manual")
			_, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestServercovCleanupMeasureAfterFailure(t *testing.T) {
	q, keyring, recorder, logger, _ := servercovDeps(t)
	dfCalls := 0
	remote := &cleanupRemoteStub{}
	remote.run = func(command string) (*sshexec.Result, error) {
		if strings.HasPrefix(command, "df -P ") {
			dfCalls++
			if dfCalls > 1 {
				return &sshexec.Result{ExitCode: 1, Stderr: "df broke"}, nil
			}
			return &sshexec.Result{Stdout: "90\n", ExitCode: 0}, nil
		}
		return &sshexec.Result{Stdout: "", ExitCode: 0}, nil
	}
	h := &ServerCleanup{
		Store: q, Keyring: keyring, Audit: recorder, Logger: logger,
		Docker: fixedSource{rt: cleanupFakeRuntime()},
		dial:   servercovCleanupDial(remote),
	}
	job := servercovCleanupJob("manual")
	_, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err == nil || !strings.Contains(err.Error(), "df broke") {
		t.Fatalf("error = %v", err)
	}
}

func TestServercovCleanupThresholdReachedPrunes(t *testing.T) {
	q, keyring, recorder, logger, db := servercovDeps(t)
	threshold := int32(80)
	db.inner.cleanupThreshold = &threshold
	measurements := []string{"90\n", "40\n"}
	remote := &cleanupRemoteStub{}
	remote.run = func(command string) (*sshexec.Result, error) {
		if strings.HasPrefix(command, "df -P ") {
			value := measurements[0]
			measurements = measurements[1:]
			return &sshexec.Result{Stdout: value, ExitCode: 0}, nil
		}
		return &sshexec.Result{Stdout: "", ExitCode: 0}, nil
	}
	h := &ServerCleanup{
		Store: q, Keyring: keyring, Audit: recorder, Logger: logger,
		Docker: fixedSource{rt: cleanupFakeRuntime()},
		dial:   servercovCleanupDial(remote),
	}
	job := servercovCleanupJob("threshold")
	result, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err != nil {
		t.Fatal(err)
	}
	payload := result.(map[string]any)
	if payload["status"] != "completed" || payload["disk_pct_before"] != 90 {
		t.Fatalf("threshold-reached result = %#v", result)
	}
}

func TestServercovCleanupDeferKeepsExplicitLockAndTeam(t *testing.T) {
	q, keyring, _, logger, db := servercovDeps(t)
	canCleanup := false
	db.inner.canCleanup = &canCleanup
	lock := "server:cleanup:custom"
	team := int64(7)
	job := servercovCleanupJob("cron")
	job.LockKey = &lock
	job.TeamID = &team
	result, err := (&ServerCleanup{Store: q, Keyring: keyring, Logger: logger}).
		Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["status"] != "deferred" {
		t.Fatalf("result = %#v", result)
	}
}

func servercovCleanupDial(remote cleanupRemote) cleanupDialFunc {
	return func(context.Context, string, int, string, string, time.Duration, string) (cleanupRemote, error) {
		return remote, nil
	}
}

func TestServercovCleanupSSHStepFailures(t *testing.T) {
	rt := cleanupFakeRuntime()
	h := &ServerCleanup{}

	transport := &cleanupRemoteStub{err: errors.New("connection lost")}
	steps := h.cleanupSteps(store.Server{}, rt, transport)
	if _, err := steps[0].run(context.Background()); err == nil {
		t.Fatal("transport failure was hidden")
	}
	exit := &cleanupRemoteStub{result: &sshexec.Result{ExitCode: 1, Stderr: "no space\nmore"}}
	steps = h.cleanupSteps(store.Server{}, rt, exit)
	if _, err := steps[0].run(context.Background()); err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("exit failure = %v", err)
	}
}

// servercovOwnerStore scripts the liveness lookups including their failures.
type servercovOwnerStore struct {
	live    map[string]bool
	resErr  error
	prevErr error
}

func (s servercovOwnerStore) ListLiveResourceUUIDs(_ context.Context, uuids []pgtype.UUID) ([]pgtype.UUID, error) {
	if s.resErr != nil {
		return nil, s.resErr
	}
	var out []pgtype.UUID
	for _, u := range uuids {
		if s.live[servercovUUIDString(u)] {
			out = append(out, u)
		}
	}
	return out, nil
}

func (s servercovOwnerStore) ListLivePreviewUUIDs(context.Context, []pgtype.UUID) ([]pgtype.UUID, error) {
	if s.prevErr != nil {
		return nil, s.prevErr
	}
	return nil, nil
}

func servercovUUIDString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", u.Bytes[0:4], u.Bytes[4:6], u.Bytes[6:8], u.Bytes[8:10], u.Bytes[10:16])
}

func TestServercovPrunePreviewImagesErrorBranches(t *testing.T) {
	dead := "33333333-3333-4333-8333-333333333333"

	listErr := &fake.Runtime{}
	listErr.ImageListFn = func(context.Context, image.ListOptions) ([]image.Summary, error) {
		return nil, errors.New("list refused")
	}
	if _, err := pruneDestroyedPreviewImages(context.Background(), listErr, servercovOwnerStore{}); err == nil {
		t.Fatal("image list failure was hidden")
	}

	rt := &fake.Runtime{}
	rt.ImageListFn = func(context.Context, image.ListOptions) ([]image.Summary, error) {
		return []image.Summary{
			{ID: "img-dead", Labels: map[string]string{"akerdock.preview_uuid": dead}},
			{ID: "img-gone", Labels: map[string]string{"akerdock.preview_uuid": dead}},
		}, nil
	}
	rt.ImageRemoveFn = func(_ context.Context, id string, _ image.RemoveOptions) ([]image.DeleteResponse, error) {
		if id == "img-gone" {
			return nil, fmt.Errorf("no such image: %w", cerrdefs.ErrNotFound)
		}
		return nil, nil
	}
	summary, err := pruneDestroyedPreviewImages(context.Background(), rt, servercovOwnerStore{})
	if err != nil || summary != "1 images removed" {
		t.Fatalf("summary = %q, %v", summary, err)
	}

	if _, err := pruneDestroyedPreviewImages(context.Background(), rt, servercovOwnerStore{resErr: errors.New("db down")}); err == nil {
		t.Fatal("owner lookup failure was hidden")
	}
	if _, err := pruneDestroyedPreviewImages(context.Background(), rt, servercovOwnerStore{prevErr: errors.New("db down")}); err == nil {
		t.Fatal("preview lookup failure was hidden")
	}
}

func TestServercovPrunePreviewVolumesErrorBranches(t *testing.T) {
	dead := "33333333-3333-4333-8333-333333333333"
	live := "11111111-1111-4111-8111-111111111111"

	listErr := &fake.Runtime{}
	listErr.VolumeListFn = func(context.Context, volume.ListOptions) (volume.ListResponse, error) {
		return volume.ListResponse{}, errors.New("list refused")
	}
	if _, err := pruneDestroyedPreviewVolumes(context.Background(), listErr, servercovOwnerStore{}); err == nil {
		t.Fatal("volume list failure was hidden")
	}

	empty := &fake.Runtime{}
	empty.VolumeListFn = func(context.Context, volume.ListOptions) (volume.ListResponse, error) {
		return volume.ListResponse{Volumes: []*volume.Volume{nil, {Name: "plain"}}}, nil
	}
	if summary, err := pruneDestroyedPreviewVolumes(context.Background(), empty, servercovOwnerStore{}); err != nil || summary != "0 volumes removed" {
		t.Fatalf("summary = %q, %v", summary, err)
	}

	rt := &fake.Runtime{}
	rt.VolumeListFn = func(context.Context, volume.ListOptions) (volume.ListResponse, error) {
		return volume.ListResponse{Volumes: []*volume.Volume{
			nil,
			{Name: "v-live", Labels: map[string]string{"akerdock.preview_uuid": live}},
			{Name: "v-dead", Labels: map[string]string{"akerdock.preview_uuid": dead}},
			{Name: "v-gone", Labels: map[string]string{"akerdock.preview_uuid": dead}},
			{Name: "v-used", Labels: map[string]string{"akerdock.preview_uuid": dead}},
		}}, nil
	}
	rt.VolumeRemoveFn = func(_ context.Context, name string, _ bool) error {
		switch name {
		case "v-gone":
			return fmt.Errorf("no such volume: %w", cerrdefs.ErrNotFound)
		case "v-used":
			return fmt.Errorf("volume in use: %w", cerrdefs.ErrConflict)
		}
		return nil
	}
	summary, err := pruneDestroyedPreviewVolumes(context.Background(), rt,
		servercovOwnerStore{live: map[string]bool{live: true}})
	if err != nil || summary != "1 volumes removed, 1 kept (still in use)" {
		t.Fatalf("summary = %q, %v", summary, err)
	}

	if _, err := pruneDestroyedPreviewVolumes(context.Background(), rt, servercovOwnerStore{resErr: errors.New("db down")}); err == nil {
		t.Fatal("owner lookup failure was hidden")
	}
}

func TestServercovPruneOrphanNetworksErrorBranches(t *testing.T) {
	dead := "33333333-3333-4333-8333-333333333333"

	listErr := &fake.Runtime{}
	listErr.NetworkListFn = func(context.Context, network.ListOptions) ([]network.Summary, error) {
		return nil, errors.New("list refused")
	}
	if _, err := pruneOrphanManagedNetworks(context.Background(), listErr, servercovOwnerStore{}); err == nil {
		t.Fatal("network list failure was hidden")
	}

	rt := &fake.Runtime{}
	rt.NetworkListFn = func(context.Context, network.ListOptions) ([]network.Summary, error) {
		return []network.Summary{
			{ID: "n-dead", Labels: map[string]string{"akerdock.resource_uuid": dead}},
			{ID: "n-gone", Labels: map[string]string{"akerdock.resource_uuid": dead}},
		}, nil
	}
	rt.NetworkRemoveFn = func(_ context.Context, id string) error {
		if id == "n-gone" {
			return fmt.Errorf("no such network: %w", cerrdefs.ErrNotFound)
		}
		return nil
	}
	summary, err := pruneOrphanManagedNetworks(context.Background(), rt, servercovOwnerStore{})
	if err != nil || summary != "1 networks removed" {
		t.Fatalf("summary = %q, %v", summary, err)
	}

	if _, err := pruneOrphanManagedNetworks(context.Background(), rt, servercovOwnerStore{resErr: errors.New("db down")}); err == nil {
		t.Fatal("owner lookup failure was hidden")
	}
}

func TestServercovPruneDeadCandidatesEdgeBranches(t *testing.T) {
	listErr := &fake.Runtime{}
	listErr.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return nil, errors.New("list refused")
	}
	if _, err := pruneDeadCandidates(context.Background(), listErr); err == nil {
		t.Fatal("container list failure was hidden")
	}

	rt := &fake.Runtime{}
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return []containertypes.Summary{
			{ID: "nameless", Labels: map[string]string{"akerdock.resource_uuid": "app"}},
			{ID: "unlabelled", Names: []string{"/app-next"}},
		}, nil
	}
	summary, err := pruneDeadCandidates(context.Background(), rt)
	if err != nil || summary != "0 dead candidates removed" {
		t.Fatalf("summary = %q, %v", summary, err)
	}
}
