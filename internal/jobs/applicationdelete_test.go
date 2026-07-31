package jobs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// deleteFakeRuntime answers every list the teardown makes with one container
// and nothing else.
func deleteFakeRuntime() *fake.Runtime {
	rt := &fake.Runtime{}
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return []containertypes.Summary{{ID: "svc", Names: []string{"/app-web"}}}, nil
	}
	rt.VolumeListFn = func(context.Context, volume.ListOptions) (volume.ListResponse, error) {
		return volume.ListResponse{}, nil
	}
	rt.NetworkListFn = func(context.Context, network.ListOptions) ([]network.Summary, error) {
		return nil, nil
	}
	return rt
}

// TestTeardownWorkloadRespectsTheVolumeBoundary pins the §20.6/INV-008 rules:
// containers and networks by managed+resource labels, host directories over
// SSH (previews included), preview volumes always — production volumes only
// on explicit request.
func TestTeardownWorkloadRespectsTheVolumeBoundary(t *testing.T) {
	rt := deleteFakeRuntime()
	remote := &cleanupRemoteStub{result: &sshexec.Result{ExitCode: 0}}
	h := &ApplicationDelete{}
	preview := store.Preview{Uuid: mustUUID(t, "22222222-2222-4222-8222-222222222222")}

	if err := h.teardownWorkload(context.Background(), rt, remote, "app", []store.Preview{preview}, false); err != nil {
		t.Fatal(err)
	}
	var volumeLists int
	var removed []string
	for _, c := range rt.Calls() {
		switch c.Method {
		case "ContainerRemove":
			removed = append(removed, c.Args[0].(string))
		case "VolumeList":
			volumeLists++
			f := c.Args[0].(volume.ListOptions).Filters
			labels := f.Get("label")
			slices.Sort(labels)
			if len(labels) != 2 || labels[0] != "akerdock.preview_uuid" || labels[1] != "akerdock.resource_uuid=app" {
				t.Fatalf("volume sweep filters = %v — only PREVIEW volumes go without delete_volumes", labels)
			}
		}
	}
	slices.Sort(removed)
	if strings.Join(removed, ",") != "app,app-next,svc" {
		t.Fatalf("removed containers = %v", removed)
	}
	if volumeLists != 1 {
		t.Fatalf("volume sweeps = %d, want the preview-only pass", volumeLists)
	}
	if len(remote.commands) != 1 || !strings.Contains(remote.commands[0], "rm -rf ") ||
		!strings.Contains(remote.commands[0], "/var/lib/akerdock/applications/app") ||
		!strings.Contains(remote.commands[0], "/var/lib/akerdock/previews/22222222-2222-4222-8222-222222222222") {
		t.Fatalf("host directory removal = %v", remote.commands)
	}

	// delete_volumes: the production sweep runs too.
	rt2 := deleteFakeRuntime()
	if err := h.teardownWorkload(context.Background(), rt2, &cleanupRemoteStub{result: &sshexec.Result{ExitCode: 0}}, "app", nil, true); err != nil {
		t.Fatal(err)
	}
	volumeLists = 0
	for _, name := range rt2.CallNames() {
		if name == "VolumeList" {
			volumeLists++
		}
	}
	if volumeLists != 2 {
		t.Fatalf("volume sweeps with delete_volumes = %d, want preview + production", volumeLists)
	}
}

func TestTeardownWorkloadReportsARealFailure(t *testing.T) {
	rt := deleteFakeRuntime()
	rt.ContainerRemoveFn = func(context.Context, string, containertypes.RemoveOptions) error {
		return errors.New("rm refused")
	}
	h := &ApplicationDelete{}
	err := h.teardownWorkload(context.Background(), rt, &cleanupRemoteStub{result: &sshexec.Result{ExitCode: 0}}, "app", nil, false)
	if err == nil || !strings.Contains(err.Error(), "container sweep") {
		t.Fatalf("teardown = %v, want the container sweep failure surfaced", err)
	}
}

// TestCollectRemnants pins §20.6.4: what a failed deletion left is read back
// — containers and volumes through the channel, the directory over SSH — and
// an unreachable daemon is recorded as unknown, never as empty.
func TestCollectRemnants(t *testing.T) {
	rt := deleteFakeRuntime()
	rt.VolumeListFn = func(context.Context, volume.ListOptions) (volume.ListResponse, error) {
		return volume.ListResponse{Volumes: []*volume.Volume{{Name: "app-data"}}}, nil
	}
	remote := &cleanupRemoteStub{result: &sshexec.Result{Stdout: "/var/lib/akerdock/applications/app\n", ExitCode: 0}}

	inventory := collectRemnants(context.Background(), rt, remote, "app")
	if got := inventory["containers"].([]string); len(got) != 1 || got[0] != "app-web" {
		t.Fatalf("containers = %v", got)
	}
	if got := inventory["volumes"].([]string); len(got) != 1 || got[0] != "app-data" {
		t.Fatalf("volumes = %v", got)
	}
	if got := inventory["files"].([]string); len(got) != 1 || got[0] != "/var/lib/akerdock/applications/app" {
		t.Fatalf("files = %v", got)
	}
	if _, hasErr := inventory["error"]; hasErr {
		t.Fatal("a readable inventory must not carry an error")
	}

	broken := &fake.Runtime{}
	broken.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return nil, errors.New("daemon gone")
	}
	inventory = collectRemnants(context.Background(), broken, remote, "app")
	if inventory["error"] == nil {
		t.Fatal("an unreadable server must record UNKNOWN remnants, not an empty inventory")
	}
	if _, has := inventory["containers"]; has {
		t.Fatal("an unknown inventory must not claim empty lists")
	}
}
