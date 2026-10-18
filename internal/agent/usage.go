package agent

import (
	"sync"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

// cpuTimes is one reading of the host's cumulative CPU time in clock ticks.
type cpuTimes struct {
	total, idle uint64
}

// usageSampler turns successive host readings into the usage reported with
// each heartbeat. CPU usage needs two readings, so the first sample has none.
type usageSampler struct {
	mu        sync.Mutex
	dataDir   string
	cpuMillis uint64
	previous  *cpuTimes
	read      func(dataDir string) (hostReading, error)
}

type hostReading struct {
	cpu                      cpuTimes
	memoryTotal, memoryFree  uint64
	diskTotal, diskAvailable uint64
}

func newUsageSampler(dataDir string, totalCPUMillis uint64) *usageSampler {
	return &usageSampler{dataDir: dataDir, cpuMillis: totalCPUMillis, read: readHost}
}

// Sample returns the usage since the previous call, or nil when the host
// cannot be read or there is no earlier CPU reading yet.
func (u *usageSampler) Sample() *orchv1alpha1.NodeResourceUsage {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	reading, err := u.read(u.dataDir)
	if err != nil {
		return nil
	}
	previous := u.previous
	u.previous = &reading.cpu
	if previous == nil || reading.cpu.total <= previous.total || reading.cpu.idle < previous.idle {
		return nil
	}
	elapsed := reading.cpu.total - previous.total
	idle := min(reading.cpu.idle-previous.idle, elapsed)
	usage := &orchv1alpha1.NodeResourceUsage{
		CpuUsedMillis:  u.cpuMillis * (elapsed - idle) / elapsed,
		DiskTotalBytes: reading.diskTotal,
	}
	if reading.memoryTotal > reading.memoryFree {
		usage.MemoryUsedBytes = reading.memoryTotal - reading.memoryFree
	}
	if reading.diskTotal > reading.diskAvailable {
		usage.DiskUsedBytes = reading.diskTotal - reading.diskAvailable
	}
	return usage
}
