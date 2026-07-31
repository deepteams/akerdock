package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/docker/docker/api/types/container"

	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
)

// statsBody builds the daemon's stream=false snapshot JSON for a container
// using 4 CPUs at 40% and 75 MiB used of 1 GiB (25 MiB reclaimable cache).
func statsBody(t *testing.T) io.ReadCloser {
	t.Helper()
	return io.NopCloser(bytes.NewReader([]byte(`{
		"precpu_stats": {"cpu_usage": {"total_usage": 1000000}, "system_cpu_usage": 10000000},
		"cpu_stats": {"cpu_usage": {"total_usage": 2000000}, "system_cpu_usage": 20000000, "online_cpus": 4},
		"memory_stats": {"usage": 104857600, "limit": 1073741824, "stats": {"inactive_file": 26214400}}
	}`)))
}

func runningInspect(running bool) container.InspectResponse {
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{
		State: &container.State{Running: running},
	}}
}

func TestComponentMetricComputesTheCLIFigures(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return runningInspect(true), nil
	}
	rt.ContainerStatsFn = func(_ context.Context, name string, stream bool) (container.StatsResponseReader, error) {
		if stream {
			t.Fatal("metrics must use the stream=false snapshot (precpu filled)")
		}
		if name != "abc-web" {
			t.Fatalf("stats for %q", name)
		}
		return container.StatsResponseReader{Body: statsBody(t)}, nil
	}

	m := componentMetric(context.Background(), rt, "abc-web", "web")
	if m.Component == nil || *m.Component != "web" || m.Running == nil || !*m.Running {
		t.Fatalf("metric = %+v", m)
	}
	if m.CpuPercent == nil || *m.CpuPercent != 40.0 {
		t.Fatalf("cpu = %v, want 40.0", m.CpuPercent)
	}
	if m.MemoryBytes == nil || *m.MemoryBytes != 75<<20 {
		t.Fatalf("mem = %v, want 75MiB (cache excluded)", m.MemoryBytes)
	}
	if m.MemoryLimitBytes == nil || *m.MemoryLimitBytes != 1<<30 {
		t.Fatalf("limit = %v", m.MemoryLimitBytes)
	}
}

func TestComponentMetricStoppedContainerIsNotRunning(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return runningInspect(false), nil
	}
	m := componentMetric(context.Background(), rt, "abc-worker", "worker")
	if m.Running == nil || *m.Running || m.CpuPercent != nil {
		t.Fatalf("stopped metric = %+v", m)
	}
	// The stats call must not even be attempted on a stopped container.
	for _, c := range rt.CallNames() {
		if c == "ContainerStats" {
			t.Fatal("no stats call expected for a stopped container")
		}
	}
}

func TestComponentMetricMissingContainerIsNotRunning(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return container.InspectResponse{}, errors.New("no such container")
	}
	m := componentMetric(context.Background(), rt, "abc-gone", "gone")
	if m.Running == nil || *m.Running {
		t.Fatalf("missing metric = %+v", m)
	}
}
