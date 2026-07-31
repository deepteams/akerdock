package dockerruntime

import (
	"math"
	"testing"

	"github.com/docker/docker/api/types/container"
)

func TestCPUPercentMirrorsTheCLIFormula(t *testing.T) {
	s := container.StatsResponse{}
	s.PreCPUStats.CPUUsage.TotalUsage = 1_000_000
	s.PreCPUStats.SystemUsage = 10_000_000
	s.CPUStats.CPUUsage.TotalUsage = 2_000_000 // delta 1e6
	s.CPUStats.SystemUsage = 20_000_000        // delta 1e7
	s.CPUStats.OnlineCPUs = 4

	got := CPUPercent(s)
	if got == nil || math.Abs(*got-40.0) > 1e-9 { // 1e6/1e7 * 4 cpus * 100
		t.Fatalf("CPUPercent = %v, want 40.0", got)
	}
}

func TestCPUPercentFallsBackToPercpuCount(t *testing.T) {
	s := container.StatsResponse{}
	s.PreCPUStats.CPUUsage.TotalUsage = 0
	s.PreCPUStats.SystemUsage = 0
	s.PreCPUStats.CPUUsage.PercpuUsage = []uint64{1, 2} // 2 cpus, pre-v1.13 daemons
	s.CPUStats.CPUUsage.TotalUsage = 5
	s.CPUStats.SystemUsage = 100

	got := CPUPercent(s)
	if got == nil || math.Abs(*got-10.0) > 1e-9 { // 5/100 * 2 * 100
		t.Fatalf("CPUPercent = %v, want 10.0", got)
	}
}

func TestCPUPercentIsNilWithoutAPreviousSample(t *testing.T) {
	s := container.StatsResponse{}
	s.CPUStats.CPUUsage.TotalUsage = 5
	// One-shot shape: precpu zeroed, system deltas zero.
	if got := CPUPercent(s); got != nil {
		t.Fatalf("CPUPercent without precpu = %v, want nil (never a guessed 0)", got)
	}
}

func TestMemoryUsageExcludesReclaimableCache(t *testing.T) {
	s := container.StatsResponse{}
	s.MemoryStats.Usage = 100 << 20
	s.MemoryStats.Limit = 1 << 30
	s.MemoryStats.Stats = map[string]uint64{"inactive_file": 25 << 20} // cgroup v2

	used, limit, pct := MemoryUsage(s)
	if used == nil || *used != 75<<20 {
		t.Fatalf("used = %v, want 75MiB", used)
	}
	if limit == nil || *limit != 1<<30 {
		t.Fatalf("limit = %v", limit)
	}
	if pct == nil || math.Abs(*pct-float64(75<<20)/float64(1<<30)*100) > 1e-9 {
		t.Fatalf("percent = %v", pct)
	}

	s.MemoryStats.Stats = map[string]uint64{"total_inactive_file": 50 << 20} // cgroup v1
	used, _, _ = MemoryUsage(s)
	if used == nil || *used != 50<<20 {
		t.Fatalf("v1 used = %v, want 50MiB", used)
	}
}

func TestMemoryUsageIsNilForAStoppedContainer(t *testing.T) {
	used, limit, pct := MemoryUsage(container.StatsResponse{})
	if used != nil || limit != nil || pct != nil {
		t.Fatal("zeroed stats (stopped container) must yield nils")
	}
}
