package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

func TestSparklineMapsADayOntoColumns(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	samples := []*orchv1alpha1.NodeUsage{
		{ObservedAt: timestamppb.New(now.Add(-25 * time.Hour)), CpuUsedMillis: 4000},
		{ObservedAt: timestamppb.New(now.Add(-23 * time.Hour)), CpuUsedMillis: 0},
		{ObservedAt: timestamppb.New(now.Add(-time.Minute)), CpuUsedMillis: 4000},
	}
	got := sparkline(samples, (*orchv1alpha1.NodeUsage).GetCpuUsedMillis, 4000, 6, now)
	if got != "▁    █" {
		t.Fatalf("sparkline = %q", got)
	}
	if blank := sparkline(samples, (*orchv1alpha1.NodeUsage).GetCpuUsedMillis, 0, 4, now); blank != "    " {
		t.Fatalf("sparkline without a total = %q", blank)
	}
}

func TestNodeViewLoadsAndShowsUsage(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	provider := fixtureProvider()
	now := time.Now()
	provider.nodes[0].TotalCpuMillis, provider.nodes[0].TotalMemoryBytes = 4000, 8<<30
	provider.nodes[0].Usage = &orchv1alpha1.NodeUsage{ObservedAt: timestamppb.New(now), CpuUsedMillis: 1500, MemoryUsedBytes: 2 << 30, DiskUsedBytes: 10 << 30, DiskTotalBytes: 40 << 30}
	provider.usage = []*orchv1alpha1.NodeUsage{{ObservedAt: timestamppb.New(now.Add(-time.Hour)), CpuUsedMillis: 2000, MemoryUsedBytes: 4 << 30, DiskUsedBytes: 10 << 30, DiskTotalBytes: 40 << 30}}
	model := New(context.Background(), provider, "production", "direct", time.Minute)
	model.snapshot = fixtureSnapshot(provider)
	model.view = ViewNodes
	command := model.refreshNodeUsage()
	if command == nil {
		t.Fatal("node view did not read usage")
	}
	_, _ = model.Update(command())
	if model.refreshNodeUsage() != nil {
		t.Fatal("usage was read again before it went stale")
	}
	model.width, model.height = 140, 40
	rendered := model.View().Content
	for _, want := range []string{"Usage · last 24h", "1.5 / 4 cores", "2.0Gi / 8.0Gi", "10.0Gi / 40.0Gi", "▅"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("node view lacks %q:\n%s", want, rendered)
		}
	}
}

func TestLogViewTogglesThePreviousInstance(t *testing.T) {
	provider := fixtureProvider()
	model := New(context.Background(), provider, "production", "direct", time.Minute)
	model.snapshot = fixtureSnapshot(provider)
	model.view = ViewLogs
	model.selectRuntimeID("web-web-1")
	_, _ = model.Update(model.openSelected()())
	_, command := model.handleKey(tea.KeyPressMsg{Code: 'P', Text: "P"})
	message := command().(logsMsg)
	if !message.previous {
		t.Fatalf("previous toggle loaded %#v", message)
	}
	_, _ = model.Update(message)
	if len(model.logs) != 1 || model.logs[0].Text != "previous instance output\n" || !model.logExited || !strings.Contains(model.logNotice, "previous") {
		t.Fatalf("previous logs = %#v exited=%t notice=%q", model.logs, model.logExited, model.logNotice)
	}
	_, command = model.handleKey(tea.KeyPressMsg{Code: 'P', Text: "P"})
	if message := command().(logsMsg); message.previous {
		t.Fatal("second toggle stayed on the previous instance")
	}
}
