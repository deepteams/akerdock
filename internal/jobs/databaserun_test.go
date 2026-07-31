package jobs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/pkg/stdcopy"

	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/docker/docker/api/types/container"
	volumetypes "github.com/docker/docker/api/types/volume"

	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/sshexec"
)

// TestDatabaseProvisionCreateSpec pins the first typed ContainerCreate of the
// migration: the password rides the create body over the channel (INV-003 as
// ADR-051 clarified — no argv, no host env file), the volume is labelled and
// mounted, the healthcheck mirrors §6.2, and the container carries the
// management labels.
func TestDatabaseProvisionCreateSpec(t *testing.T) {
	q, keyring, _, logger, db := jobFlowDependencies(t)
	_ = db
	row, err := q.GetDatabaseByID(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	rt := &fake.Runtime{}
	rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
		return volumetypes.Volume{}, fmt.Errorf("no such volume: %w", cerrdefs.ErrNotFound)
	}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: true, Health: &containertypes.Health{Status: "healthy"}},
		}}, nil
	}
	remote := &cleanupRemoteStub{result: &sshexec.Result{ExitCode: 0}}

	h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
	if err := h.provision(context.Background(), rt, remote, nil, row, "akerdock-net", "dbuuid"); err != nil {
		t.Fatalf("provision: %v", err)
	}

	var created bool
	for _, c := range rt.Calls() {
		switch c.Method {
		case "VolumeCreate":
			opts := c.Args[0].(volumetypes.CreateOptions)
			if opts.Name != "dbuuid_data" || opts.Labels["akerdock.resource_uuid"] != "dbuuid" {
				t.Fatalf("volume create = %+v", opts)
			}
		case "ContainerCreate":
			created = true
			cfg := c.Args[0].(*containertypes.Config)
			host := c.Args[1].(*containertypes.HostConfig)
			name := c.Args[4].(string)
			if name != "dbuuid" {
				t.Fatalf("container name = %q", name)
			}
			if !slices.Contains(cfg.Env, "POSTGRES_PASSWORD=unit-password") {
				t.Fatalf("the password must ride the typed create body, env = %v", cfg.Env)
			}
			if cfg.Healthcheck == nil || !strings.Contains(strings.Join(cfg.Healthcheck.Test, " "), "pg_isready") {
				t.Fatalf("healthcheck = %+v", cfg.Healthcheck)
			}
			if cfg.Labels["akerdock.managed"] != "true" || cfg.Labels["akerdock.type"] != "database" {
				t.Fatalf("labels = %v", cfg.Labels)
			}
			if string(host.NetworkMode) != "akerdock-net" ||
				!slices.Contains(host.Binds, "dbuuid_data:/var/lib/postgresql/data") {
				t.Fatalf("host config = %+v", host)
			}
			if host.RestartPolicy.Name != containertypes.RestartPolicyUnlessStopped {
				t.Fatalf("restart policy = %+v", host.RestartPolicy)
			}
			if len(host.PortBindings) != 0 {
				t.Fatalf("a private database must publish nothing: %v", host.PortBindings)
			}
		}
	}
	if !created {
		t.Fatal("no ContainerCreate recorded")
	}
	// The remove-then-create ordering: the stale container goes first.
	names := rt.CallNames()
	if slices.Index(names, "ContainerRemove") > slices.Index(names, "ContainerCreate") {
		t.Fatalf("stale container must be removed before the create: %v", names)
	}
}

// TestDatabaseWaitHealthyTimesOutWithLogs pins the failure diagnostic: the
// readiness budget expiring attaches the container's last lines.
func TestDatabaseWaitHealthyTimesOutWithLogs(t *testing.T) {
	oldTimeout, oldPoll := databaseReadyTimeout, databaseReadyPoll
	databaseReadyTimeout, databaseReadyPoll = 0, 0
	t.Cleanup(func() { databaseReadyTimeout, databaseReadyPoll = oldTimeout, oldPoll })

	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: true, Health: &containertypes.Health{Status: "starting"}},
		}}, nil
	}
	rt.ContainerLogsFn = logsReader("FATAL: bad config\n")

	err := (&DatabaseRun{}).waitHealthy(context.Background(), rt, "dbuuid")
	if err == nil || !strings.Contains(err.Error(), "did not become ready") || !strings.Contains(err.Error(), "FATAL: bad config") {
		t.Fatalf("waitHealthy = %v, want the timeout with the container's last lines", err)
	}
}

// logsReader scripts ContainerLogs with one multiplexed stdout payload.
func logsReader(payload string) func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
	return func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
		var buf bytes.Buffer
		_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stdout).Write([]byte(payload))
		return io.NopCloser(&buf), nil
	}
}
