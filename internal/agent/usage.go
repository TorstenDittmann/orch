package agent

import (
	"context"
	"sort"
	"sync"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

// allocationUsageTimeout bounds the task metrics read so a slow runtime
// cannot delay a heartbeat.
const allocationUsageTimeout = 2 * time.Second

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

// allocationSampler turns successive task CPU readings into each
// allocation's CPU use since the previous heartbeat. Readings are kept per
// runtime object, so a replaced task starts over instead of reporting the
// difference between two containers.
type allocationSampler struct {
	mu       sync.Mutex
	read     func(context.Context) ([]orchruntime.TaskCPU, error)
	previous map[string]orchruntime.TaskCPU
}

func newAllocationSampler(runtime orchruntime.Runtime) *allocationSampler {
	reader, ok := runtime.(orchruntime.TaskCPUReader)
	if !ok {
		return nil
	}
	return &allocationSampler{read: reader.TaskCPU}
}

// Sample returns the CPU each allocation used since the previous call, in
// millicores, summed over its tasks. Tasks seen for the first time have none.
func (a *allocationSampler) Sample(ctx context.Context) []*orchv1alpha1.AllocationResourceUsage {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, allocationUsageTimeout)
	defer cancel()
	readings, err := a.read(ctx)
	if err != nil {
		return nil
	}
	current := make(map[string]orchruntime.TaskCPU, len(readings))
	used := make(map[string]uint64)
	for _, reading := range readings {
		current[reading.ObjectID] = reading
		previous, ok := a.previous[reading.ObjectID]
		if !ok || reading.AllocationID == "" || reading.UsageMicros < previous.UsageMicros {
			continue
		}
		elapsed := reading.ObservedAt.Sub(previous.ObservedAt).Microseconds()
		if elapsed <= 0 {
			continue
		}
		used[reading.AllocationID] += (reading.UsageMicros - previous.UsageMicros) * 1000 / uint64(elapsed)
	}
	a.previous = current
	usage := make([]*orchv1alpha1.AllocationResourceUsage, 0, len(used))
	for allocationID, millis := range used {
		usage = append(usage, &orchv1alpha1.AllocationResourceUsage{AllocationId: allocationID, CpuUsedMillis: millis})
	}
	sort.Slice(usage, func(i, j int) bool { return usage[i].GetAllocationId() < usage[j].GetAllocationId() })
	return usage
}
