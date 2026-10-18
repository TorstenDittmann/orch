package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

// nodeUsageRefresh is how often the selected node's usage history is read
// again; the controller buckets it by five minutes.
const nodeUsageRefresh = 30 * time.Second

const usageWindow = 24 * time.Hour

type nodeUsageMsg struct {
	name    string
	samples []*orchv1alpha1.NodeUsage
	err     error
}

type nodeUsageHistory struct {
	samples []*orchv1alpha1.NodeUsage
	readAt  time.Time
}

// refreshNodeUsage reads the selected node's history when it is missing or
// older than nodeUsageRefresh.
func (m *Model) refreshNodeUsage() tea.Cmd {
	if m.view != ViewNodes || len(m.snapshot.Nodes) == 0 {
		return nil
	}
	name := m.snapshot.Nodes[min(m.selected[ViewNodes], len(m.snapshot.Nodes)-1)].GetName()
	if history, ok := m.nodeUsage[name]; ok && time.Since(history.readAt) < nodeUsageRefresh {
		return nil
	}
	if m.nodeUsage == nil {
		m.nodeUsage = map[string]nodeUsageHistory{}
	}
	// Mark the read as started so a slow controller is not asked again on
	// every tick.
	m.nodeUsage[name] = nodeUsageHistory{samples: m.nodeUsage[name].samples, readAt: time.Now()}
	ctx, provider := m.ctx, m.provider
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		response, err := provider.GetNodeUsage(ctx, name)
		return nodeUsageMsg{name: name, samples: response.GetSamples(), err: err}
	}
}

func (m *Model) receiveNodeUsage(message nodeUsageMsg) {
	if message.err != nil {
		return // the sparkline keeps its last history
	}
	m.nodeUsage[message.name] = nodeUsageHistory{samples: message.samples, readAt: time.Now()}
}

// usageLines renders the node's current usage with a 24-hour sparkline each.
func (m *Model) usageLines(t theme, node *orchv1alpha1.Node, width int) []string {
	samples := m.nodeUsage[node.GetName()].samples
	usage := node.GetUsage()
	diskTotal := usage.GetDiskTotalBytes()
	for _, sample := range samples {
		diskTotal = max(diskTotal, sample.GetDiskTotalBytes())
	}
	rows := []struct {
		label   string
		current string
		total   uint64
		value   func(*orchv1alpha1.NodeUsage) uint64
	}{
		{"CPU", fmt.Sprintf("%s / %s cores", coresLabel(usage.GetCpuUsedMillis()), coresLabel(node.GetTotalCpuMillis())), node.GetTotalCpuMillis(), (*orchv1alpha1.NodeUsage).GetCpuUsedMillis},
		{"Memory", fmt.Sprintf("%s / %s", bytesLabel(usage.GetMemoryUsedBytes()), bytesLabel(node.GetTotalMemoryBytes())), node.GetTotalMemoryBytes(), (*orchv1alpha1.NodeUsage).GetMemoryUsedBytes},
		{"Disk", fmt.Sprintf("%s / %s", bytesLabel(usage.GetDiskUsedBytes()), bytesLabel(diskTotal)), diskTotal, (*orchv1alpha1.NodeUsage).GetDiskUsedBytes},
	}
	lines := []string{muted(t, "Usage · last 24h")}
	if usage == nil && len(samples) == 0 {
		return append(lines, muted(t, "No usage reported yet."))
	}
	valueWidth := 0
	for index := range rows {
		if usage == nil {
			rows[index].current = "no recent report"
		}
		valueWidth = max(valueWidth, len(rows[index].current))
	}
	for _, row := range rows {
		chart := sparkline(samples, row.value, row.total, max(8, width-15-valueWidth), time.Now())
		lines = append(lines, fmt.Sprintf("%s %s  %s", muted(t, fmt.Sprintf("%-12s", row.label)), colorize(t, t.primary, chart), sanitize(row.current)))
	}
	return lines
}

var sparkLevels = []rune("▁▂▃▄▅▆▇█")

// sparkline maps the last 24 hours onto width columns, each showing the
// highest share of total in it; columns without samples stay blank.
func sparkline(samples []*orchv1alpha1.NodeUsage, value func(*orchv1alpha1.NodeUsage) uint64, total uint64, width int, now time.Time) string {
	columns := make([]float64, width)
	for index := range columns {
		columns[index] = -1
	}
	start := now.Add(-usageWindow)
	for _, sample := range samples {
		at := sample.GetObservedAt().AsTime()
		if at.Before(start) || total == 0 {
			continue
		}
		column := min(width-1, int(at.Sub(start)*time.Duration(width)/usageWindow))
		columns[column] = max(columns[column], min(1, float64(value(sample))/float64(total)))
	}
	var builder strings.Builder
	for _, share := range columns {
		if share < 0 {
			builder.WriteRune(' ')
			continue
		}
		builder.WriteRune(sparkLevels[min(len(sparkLevels)-1, int(share*float64(len(sparkLevels))))])
	}
	return builder.String()
}

func coresLabel(millis uint64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", float64(millis)/1000), "0"), ".")
}
