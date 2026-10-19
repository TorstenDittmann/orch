package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

func TestNodeUsageKeepsADayAndAveragesBuckets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	input := testEnrollAgent("node-one", "node-1", strings.Repeat("a", 64), "enrollment-key-one", strings.Repeat("1", 64))
	if _, err := state.EnrollAgent(ctx, input); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	record := func(at time.Time, cpu uint64) {
		t.Helper()
		usage := model.NodeUsage{ObservedAt: at, CPUUsedMillis: cpu, MemoryUsedBytes: cpu << 20, DiskUsedBytes: 10, DiskTotalBytes: 100}
		if err := state.RecordNodeUsage(ctx, "node-one", usage); err != nil {
			t.Fatal(err)
		}
	}
	record(start, 100)
	record(start.Add(10*time.Second), 300)
	record(start.Add(5*time.Minute), 500)

	history, err := state.NodeUsageHistory(ctx, "node-one", start.Add(-time.Hour), 5*time.Minute)
	if err != nil || len(history) != 2 {
		t.Fatalf("history = %#v, %v", history, err)
	}
	if !history[0].ObservedAt.Equal(start) || history[0].CPUUsedMillis != 200 || history[0].MemoryUsedBytes != 200<<20 || history[0].DiskTotalBytes != 100 {
		t.Fatalf("first bucket = %#v", history[0])
	}
	if history[1].CPUUsedMillis != 500 {
		t.Fatalf("second bucket = %#v", history[1])
	}
	latest, err := state.LatestNodeUsage(ctx)
	if err != nil || latest["node-one"].CPUUsedMillis != 500 || !latest["node-one"].ObservedAt.Equal(start.Add(5*time.Minute)) {
		t.Fatalf("latest = %#v, %v", latest, err)
	}

	record(start.Add(NodeUsageRetention+time.Minute), 700)
	history, err = state.NodeUsageHistory(ctx, "node-one", start.Add(-time.Hour), 5*time.Minute)
	if err != nil || len(history) != 2 || history[0].CPUUsedMillis != 500 || history[1].CPUUsedMillis != 700 {
		t.Fatalf("history after a day = %#v, %v", history, err)
	}
}
