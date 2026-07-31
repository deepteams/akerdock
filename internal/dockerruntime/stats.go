package dockerruntime

import (
	"github.com/docker/docker/api/types/container"
)

// The CLI's stats columns (CPU%, MEM USAGE / LIMIT, MEM%) computed from the
// raw counters a stream=false snapshot carries — what `docker stats
// --no-stream` used to pre-format on the server. Formulas mirror the docker
// CLI's; incomputable figures return nil rather than a guessed zero.

// CPUPercent computes the CPU column from the sample and its precpu
// predecessor. It needs a stream=false snapshot: one-shot stats leave
// precpu_stats empty and the delta is incomputable.
func CPUPercent(s container.StatsResponse) *float64 {
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	if cpuDelta < 0 || sysDelta <= 0 {
		return nil
	}
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(s.PreCPUStats.CPUUsage.PercpuUsage))
	}
	if cpus == 0 {
		return nil
	}
	v := cpuDelta / sysDelta * cpus * 100
	return &v
}

// MemoryUsage computes used/limit/percent. Used excludes the reclaimable
// page cache, like the CLI: inactive_file on cgroup v2, total_inactive_file
// on v1.
func MemoryUsage(s container.StatsResponse) (used, limit *int64, percent *float64) {
	if s.MemoryStats.Limit == 0 {
		return nil, nil, nil
	}
	u := s.MemoryStats.Usage
	if cache, ok := s.MemoryStats.Stats["inactive_file"]; ok && cache < u {
		u -= cache
	} else if cache, ok := s.MemoryStats.Stats["total_inactive_file"]; ok && cache < u {
		u -= cache
	}
	usedV := int64(u)
	limitV := int64(s.MemoryStats.Limit)
	pct := float64(u) / float64(s.MemoryStats.Limit) * 100
	return &usedV, &limitV, &pct
}
