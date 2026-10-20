package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestUsageSamplerReportsCPUSinceThePreviousSample(t *testing.T) {
	t.Parallel()
	readings := []hostReading{
		{cpu: cpuTimes{total: 1000, idle: 800}, memoryTotal: 8 << 30, memoryFree: 6 << 30, diskTotal: 100, diskAvailable: 40},
		{cpu: cpuTimes{total: 1400, idle: 1100}, memoryTotal: 8 << 30, memoryFree: 5 << 30, diskTotal: 100, diskAvailable: 30},
	}
	sampler := &usageSampler{cpuMillis: 4000, read: func(string) (hostReading, error) {
		if len(readings) == 0 {
			return hostReading{}, errors.New("unreadable")
		}
		next := readings[0]
		readings = readings[1:]
		return next, nil
	}}
	if usage := sampler.Sample(); usage != nil {
		t.Fatalf("first sample = %#v, want none without an earlier CPU reading", usage)
	}
	usage := sampler.Sample()
	// 100 of 400 ticks were busy on four cores.
	if usage.GetCpuUsedMillis() != 1000 || usage.GetMemoryUsedBytes() != 3<<30 || usage.GetDiskUsedBytes() != 70 || usage.GetDiskTotalBytes() != 100 {
		t.Fatalf("usage = %#v", usage)
	}
	if usage := sampler.Sample(); usage != nil {
		t.Fatalf("unreadable host = %#v", usage)
	}
	var missing *usageSampler
	if missing.Sample() != nil {
		t.Fatal("a nil sampler reported usage")
	}
}

func TestAllocationSamplerReportsCPUPerAllocation(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	fake := orchruntime.NewFake()
	sampler := newAllocationSampler(fake)
	fake.CPU = []orchruntime.TaskCPU{
		{ObjectID: "a-1", AllocationID: "a", UsageMicros: 1_000_000, ObservedAt: start},
		{ObjectID: "b-1", AllocationID: "b", UsageMicros: 0, ObservedAt: start},
	}
	if usage := sampler.Sample(context.Background()); len(usage) != 0 {
		t.Fatalf("first sample = %v, want none", usage)
	}
	later := start.Add(10 * time.Second)
	fake.CPU = []orchruntime.TaskCPU{
		// 5 CPU-seconds in 10 seconds is half a core.
		{ObjectID: "a-1", AllocationID: "a", UsageMicros: 6_000_000, ObservedAt: later},
		// A replacement task has no earlier reading yet.
		{ObjectID: "b-2", AllocationID: "b", UsageMicros: 9_000_000, ObservedAt: later},
	}
	usage := sampler.Sample(context.Background())
	if len(usage) != 1 || usage[0].GetAllocationId() != "a" || usage[0].GetCpuUsedMillis() != 500 {
		t.Fatalf("usage = %v", usage)
	}
	var missing *allocationSampler
	if missing.Sample(context.Background()) != nil {
		t.Fatal("a nil sampler reported usage")
	}
}
