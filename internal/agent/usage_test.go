package agent

import (
	"errors"
	"testing"
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
