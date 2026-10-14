package tui

import (
	"fmt"
	"image/color"
	"os"
	"strings"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type theme struct {
	primary, muted, good, warning, danger, surface, text color.Color
	plain                                                bool
}

func currentTheme() theme {
	return theme{
		primary: lipgloss.Color("#7C8CFF"), muted: lipgloss.Color("#73778C"), good: lipgloss.Color("#54D6A3"), warning: lipgloss.Color("#F2C66D"),
		danger: lipgloss.Color("#FF6B7A"), surface: lipgloss.Color("#252735"), text: lipgloss.Color("#E8E9F0"), plain: os.Getenv("NO_COLOR") != "",
	}
}

func (m *Model) View() tea.View {
	width, height := m.width, m.height
	if width == 0 {
		width, height = 100, 30
	}
	t := currentTheme()
	content := m.renderScreen(t, width, height)
	view := tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}

func (m *Model) renderScreen(t theme, width, height int) string {
	if width < 60 || height < 18 {
		return centerText(t, width, height, fmt.Sprintf("Terminal too small\n\nNeed at least 60x18 · current %dx%d", width, height))
	}
	header := m.renderHeader(t, width)
	tabs := m.renderTabs(t, width)
	footer := m.renderFooter(t, width)
	reserved := lipgloss.Height(header) + lipgloss.Height(tabs) + lipgloss.Height(footer)
	bodyHeight := max(1, height-reserved)
	body := m.renderBody(t, width, bodyHeight)
	base := lipgloss.JoinVertical(lipgloss.Left, header, tabs, fitHeight(body, bodyHeight, width), footer)
	if m.help {
		base = overlayCentered(base, m.renderHelp(t, min(width-8, 82)), width, height)
	}
	if m.palette {
		base = overlayCentered(base, m.renderPalette(t, min(width-8, 76)), width, height)
	}
	if m.search {
		base = overlayCentered(base, m.renderSearch(t, min(width-8, 76)), width, height)
	}
	if m.confirm != nil {
		base = overlayCentered(base, m.renderConfirmation(t, min(width-8, 70)), width, height)
	}
	return base
}

func (m *Model) renderHeader(t theme, width int) string {
	cluster := "connecting"
	if m.snapshot.Cluster != nil {
		cluster = sanitize(m.snapshot.Cluster.GetName())
	}
	connection := colorize(t, t.good, "● ONLINE")
	if m.lastError != "" {
		connection = colorize(t, t.warning, "● DEGRADED")
	} else if m.snapshot.Cluster == nil {
		connection = colorize(t, t.muted, "○ CONNECTING")
	}
	targetName := m.target
	if targetName == "" {
		targetName = "context"
	}
	left := bold(t, " ORCH ") + "  " + bold(t, cluster)
	right := fmt.Sprintf("%s  %s · %s  ", connection, sanitize(targetName), sanitize(m.transport))
	space := max(1, width-lipgloss.Width(left)-lipgloss.Width(right))
	return surface(t, truncate(left+strings.Repeat(" ", space)+right, width), width)
}

func (m *Model) renderTabs(t theme, width int) string {
	var tabs []string
	for index, name := range viewNames {
		label := fmt.Sprintf(" %d %s ", index+1, name)
		if ViewID(index) == m.view {
			label = active(t, label)
		} else {
			label = muted(t, label)
		}
		tabs = append(tabs, label)
	}
	start := 0
	for start < int(m.view) && lipgloss.Width(strings.Join(tabs[start:int(m.view)+1], " ")) > width {
		start++
	}
	return truncate(strings.Join(tabs[start:], " "), width)
}

func (m *Model) renderFooter(t theme, width int) string {
	left := " ↑↓/jk navigate   enter open   ctrl+p commands   ? help   q quit"
	if m.view == ViewJobs {
		left = " ↑↓ select  enter logs  c cancel  x retry  n older  N latest"
	} else if m.view == ViewDeployments {
		left = " ↑↓ scroll  enter compare  b rollback  n older  N latest  esc back"
	} else if m.view == ViewApplications {
		left = " ↑↓ select  enter runtimes  d deployments  o jobs  ctrl+p commands"
	} else if m.view == ViewNodes {
		left = " c cordon   u uncordon   d drain   i ingress   ctrl+p commands   ? help"
	} else if m.view == ViewEvents {
		left = " ↑↓/jk inspect   / filter   x clear   ctrl+p commands   ? help"
	} else if m.view == ViewLogs {
		left = " ↑↓/jk navigate   enter logs   h/l pane   / search   g group   s stream   p follow   G live"
	}
	right := ""
	if m.loading {
		right = "refreshing  "
	} else if !m.snapshot.RefreshedAt.IsZero() {
		right = "updated " + relativeTime(m.snapshot.RefreshedAt) + "  "
	}
	if m.toast != "" {
		left = " " + truncate(m.toast, max(10, width-lipgloss.Width(right)-2))
	}
	space := max(1, width-lipgloss.Width(left)-lipgloss.Width(right))
	return surface(t, truncate(left+strings.Repeat(" ", space)+right, width), width)
}

func (m *Model) renderBody(t theme, width, height int) string {
	if m.snapshot.Cluster == nil && m.lastError == "" {
		return centerText(t, width, height, "Connecting to the controller…\n\nNavigation remains available while data loads.")
	}
	var body string
	switch m.view {
	case ViewOverview:
		body = m.renderOverview(t, width, height)
	case ViewApplications:
		body = m.renderApplications(t, width, height)
	case ViewNodes:
		body = m.renderNodes(t, width, height)
	case ViewEvents:
		body = m.renderEvents(t, width, height)
	case ViewLogs:
		body = m.renderLogs(t, width, height)
	case ViewJobs:
		body = m.renderJobs(t, width, height)
	case ViewDeployments:
		body = m.renderDeployments(t, width, height)
	case ViewHelp:
		body = m.renderHelp(t, width-2)
	}
	if m.lastError != "" {
		banner := colorize(t, t.warning, " DEGRADED ") + " " + truncate(m.lastError, width-14)
		body = banner + "\n" + body
	}
	return body
}

func (m *Model) renderOverview(t theme, width, height int) string {
	healthy, progressing, failed := 0, 0, 0
	for _, application := range m.snapshot.Applications {
		switch application.GetExecutionStatus() {
		case orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY:
			healthy++
		case orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_FAILED, orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_DEGRADED:
			failed++
		default:
			progressing++
		}
	}
	connected, unavailable := 0, 0
	for _, node := range m.snapshot.Nodes {
		if node.GetConnectionStatus() == orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED {
			connected++
		} else {
			unavailable++
		}
	}
	cards := []string{
		metricCard(t, "APPLICATIONS", fmt.Sprint(len(m.snapshot.Applications)), fmt.Sprintf("%d healthy  %d moving  %d needs attention", healthy, progressing, failed), max(22, width/3-2)),
		metricCard(t, "NODES", fmt.Sprint(len(m.snapshot.Nodes)), fmt.Sprintf("%d connected  %d unavailable", connected, unavailable), max(22, width/3-2)),
		metricCard(t, "EVENT CURSOR", fmt.Sprint(m.snapshot.EventCursor), fmt.Sprintf("%d retained locally", len(m.snapshot.Events)), max(22, width/3-2)),
	}
	if width < 90 {
		cards = []string{
			metricCard(t, "CLUSTER", fmt.Sprintf("%d apps · %d nodes", len(m.snapshot.Applications), len(m.snapshot.Nodes)), fmt.Sprintf("%d healthy · %d connected · %d alerts", healthy, connected, failed+unavailable), width-2),
		}
	}
	result := lipgloss.JoinHorizontal(lipgloss.Top, cards...)
	result += "\n\n" + heading(t, "Recent activity") + "\n"
	events := m.snapshot.Events
	start := max(0, len(events)-max(3, height-lipgloss.Height(result)-3))
	if len(events) == 0 {
		result += empty(t, "No events yet", "Controller changes will appear here automatically.")
	}
	for _, event := range events[start:] {
		result += fmt.Sprintf("\n %s  %-34s %s", muted(t, formatTime(event.GetCreatedAt().AsTime())), truncate(sanitize(event.GetKind()), 34), truncate(sanitize(event.GetPayloadJson()), max(10, width-52)))
	}
	return result
}

func (m *Model) renderApplications(t theme, width, height int) string {
	if len(m.snapshot.Applications) == 0 {
		return empty(t, "No applications deployed", "Run orch deploy from your application directory, then refresh with r.")
	}
	listWidth := width
	if width >= 92 {
		listWidth = width * 45 / 100
	}
	rows := []string{heading(t, "Applications") + "\n" + muted(t, "   NAME                 STATUS          GENERATION")}
	visible := max(3, height-4)
	start := windowStart(m.selected[ViewApplications], visible, len(m.snapshot.Applications))
	for index := start; index < min(len(m.snapshot.Applications), start+visible); index++ {
		application := m.snapshot.Applications[index]
		marker := "  "
		if index == m.selected[ViewApplications] {
			marker = colorize(t, t.primary, "› ")
		}
		status := statusStyle(t, application.GetExecutionStatus().String())
		row := fmt.Sprintf("%s%-20s %-15s %d/%d", marker, truncate(application.GetName(), 20), status, application.GetObservedGeneration(), application.GetGeneration())
		rows = append(rows, truncate(row, listWidth-2))
	}
	list := panel(t, strings.Join(rows, "\n"), listWidth, height)
	if width < 92 {
		return list
	}
	selected := m.snapshot.Applications[m.selected[ViewApplications]]
	detail := m.renderApplicationDetail(t, selected, width-listWidth, height)
	return lipgloss.JoinHorizontal(lipgloss.Top, list, detail)
}

func (m *Model) renderApplicationDetail(t theme, summary *orchv1alpha1.ApplicationSummary, width, height int) string {
	lines := []string{
		heading(t, sanitize(summary.GetName())),
		statusStyle(t, summary.GetExecutionStatus().String()) + "  " + muted(t, relativeTime(summary.GetExecutionUpdatedAt().AsTime())),
		"",
		labelValue(t, "Desired", fmt.Sprint(summary.GetGeneration())),
		labelValue(t, "Observed", fmt.Sprint(summary.GetObservedGeneration())),
		labelValue(t, "Active", fmt.Sprint(summary.GetActiveGeneration())),
		labelValue(t, "Reason", summary.GetExecutionReason()),
	}
	if detail := m.details[summary.GetName()]; detail != nil {
		lines = append(lines, "", heading(t, "Runtime"))
		for _, allocation := range detail.GetAllocations()[:min(3, len(detail.GetAllocations()))] {
			lines = append(lines, fmt.Sprintf("%s #%d  %s/%s  %s", sanitize(allocation.GetService()), allocation.GetReplica(), enumLabel(allocation.GetPhase().String()), enumLabel(allocation.GetHealthStatus().String()), sanitize(allocation.GetNodeName())))
		}
		for _, route := range detail.GetRoutes()[:min(2, len(detail.GetRoutes()))] {
			lines = append(lines, fmt.Sprintf("HTTP %s  %s  %d upstreams", sanitize(route.GetDomain()), enumLabel(route.GetState().String()), route.GetUpstreamCount()))
		}
		for _, route := range detail.GetTcpRoutes()[:min(2, len(detail.GetTcpRoutes()))] {
			lines = append(lines, fmt.Sprintf("TCP :%d -> %s:%d  %s", route.GetPublicPort(), sanitize(route.GetService()), route.GetTargetPort(), enumLabel(route.GetState().String())))
		}
		for _, port := range detail.GetNodePorts()[:min(4, len(detail.GetNodePorts()))] {
			lines = append(lines, fmt.Sprintf("%s %s:%d -> %s:%d  %s", strings.ToUpper(sanitize(port.GetProtocol())), sanitize(port.GetNodeName()), port.GetNodePort(), sanitize(port.GetService()), port.GetTargetPort(), enumLabel(port.GetState().String())))
		}
		for _, job := range detail.GetCronJobs()[:min(2, len(detail.GetCronJobs()))] {
			lines = append(lines, fmt.Sprintf("CRON %s  %s  next %s", sanitize(job.GetJob()), enumLabel(job.GetLastState().String()), relativeTime(job.GetNextRunAt().AsTime())))
		}
		if len(detail.GetAllocations())+len(detail.GetCronJobs()) > 0 {
			lines = append(lines, "", muted(t, "enter runtimes · d deployments"))
		}
	} else {
		lines = append(lines, "", muted(t, "enter runtimes · d deployments"))
	}
	return panel(t, strings.Join(lines, "\n"), width, height)
}

func (m *Model) renderNodes(t theme, width, height int) string {
	if len(m.snapshot.Nodes) == 0 {
		return empty(t, "No nodes enrolled", "Create a join artifact and enroll an agent to add capacity.")
	}
	listWidth := width
	if width >= 92 {
		listWidth = width * 45 / 100
	}
	rows := []string{heading(t, "Nodes") + "\n" + muted(t, "   NAME                 CONNECTION      LIFECYCLE")}
	visible := max(3, height-4)
	start := windowStart(m.selected[ViewNodes], visible, len(m.snapshot.Nodes))
	for index := start; index < min(len(m.snapshot.Nodes), start+visible); index++ {
		node := m.snapshot.Nodes[index]
		marker := "  "
		if index == m.selected[ViewNodes] {
			marker = colorize(t, t.primary, "› ")
		}
		row := fmt.Sprintf("%s%-20s %-15s %s", marker, truncate(node.GetName(), 20), statusStyle(t, node.GetConnectionStatus().String()), enumLabel(node.GetLifecycle().String()))
		rows = append(rows, truncate(row, listWidth-2))
	}
	list := panel(t, strings.Join(rows, "\n"), listWidth, height)
	if width < 92 {
		return list
	}
	node := m.snapshot.Nodes[m.selected[ViewNodes]]
	detailLines := []string{
		heading(t, sanitize(node.GetName())),
		statusStyle(t, node.GetConnectionStatus().String()) + "  " + enumLabel(node.GetLifecycle().String()), "",
		labelValue(t, "CPU", fmt.Sprintf("%dm / %dm", node.GetAssignedCpuMillis(), node.GetAllocatableCpuMillis())),
		labelValue(t, "Memory", fmt.Sprintf("%s / %s", bytesLabel(node.GetAssignedMemoryBytes()), bytesLabel(node.GetAllocatableMemoryBytes()))),
		labelValue(t, "Network", node.GetNetworkState()), labelValue(t, "Ingress", fmt.Sprint(node.GetIngress())),
		labelValue(t, "Subnet", node.GetNodeSubnet()), labelValue(t, "Labels", sortedLabels(node.GetLabels())), "",
		muted(t, "c cordon · u uncordon · d drain · i ingress"),
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, list, panel(t, strings.Join(detailLines, "\n"), width-listWidth, height))
}

func (m *Model) renderEvents(t theme, width, height int) string {
	events := m.filteredEvents()
	if len(m.snapshot.Events) == 0 {
		return empty(t, "No events yet", "Durable cluster changes will appear here.")
	}
	if len(events) == 0 {
		return empty(t, "No matching events", "Press / to change the filter or x to clear it.")
	}
	filter := ""
	if m.eventFilter != "" {
		filter = " · filter " + fmt.Sprintf("%q", m.eventFilter)
	}
	rows := []string{heading(t, "Event timeline") + "  " + muted(t, fmt.Sprintf("%d/%d shown%s", len(events), len(m.snapshot.Events), filter))}
	visible := max(2, height-3)
	selected := min(m.selected[ViewEvents], len(events)-1)
	start := windowStart(selected, visible, len(events))
	for index := start; index < min(len(events), start+visible); index++ {
		event := events[index]
		marker := " "
		if index == selected {
			marker = colorize(t, t.primary, "›")
		}
		rows = append(rows, fmt.Sprintf("%s%6d  %s  %-35s %s", marker, event.GetSequence(), muted(t, formatTime(event.GetCreatedAt().AsTime())), truncate(sanitize(event.GetKind()), 35), truncate(sanitize(event.GetPayloadJson()), max(8, width-58))))
	}
	return panel(t, strings.Join(rows, "\n"), width, height)
}

func (m *Model) renderLogs(t theme, width, height int) string {
	runtimes := m.filteredRuntimes()
	if len(runtimes) == 0 {
		return empty(t, "No managed runtimes", "Deploy a service or wait for a cron run. Press / to clear or change the runtime filter.")
	}
	selected := runtimes[min(m.runtimeSelected, len(runtimes)-1)]
	if width < 78 {
		if m.runtimeFocus == 0 {
			return m.renderRuntimeList(t, width, height, runtimes)
		}
		if runtimeLogSource(selected) != m.logSource && !m.logsLoading {
			return m.renderRuntimeDetail(t, width, height, selected)
		}
		return m.renderRuntimeLogPane(t, width, height, selected)
	}
	listWidth := width * 38 / 100
	if width >= 112 {
		detailWidth := width * 27 / 100
		return lipgloss.JoinHorizontal(lipgloss.Top,
			m.renderRuntimeList(t, listWidth, height, runtimes),
			m.renderRuntimeDetail(t, detailWidth, height, selected),
			m.renderRuntimeLogPane(t, width-listWidth-detailWidth, height, selected))
	}
	right := m.renderRuntimeLogPane(t, width-listWidth, height, selected)
	if runtimeLogSource(selected) != m.logSource && !m.logsLoading {
		right = m.renderRuntimeDetail(t, width-listWidth, height, selected)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, m.renderRuntimeList(t, listWidth, height, runtimes), right)
}

func (m *Model) renderRuntimeList(t theme, width, height int, runtimes []*orchv1alpha1.RuntimeSummary) string {
	focus := ""
	if m.runtimeFocus == 0 {
		focus = " ●"
	}
	filter := ""
	if m.runtimeFilter != "" {
		filter = " · /" + m.runtimeFilter
	}
	rows := []string{heading(t, "Managed runtimes"+focus), muted(t, fmt.Sprintf("by %s%s", m.runtimeGroup, filter))}
	var entries []string
	selectedRow := 0
	lastGroup := ""
	for index, runtime := range runtimes {
		marker := "  "
		if index == m.runtimeSelected {
			marker = colorize(t, t.primary, "› ")
		}
		group := runtime.GetApplication()
		if m.runtimeGroup == "node" {
			group = runtime.GetNodeName()
			if group == "" {
				group = "unplaced"
			}
		}
		if group != lastGroup {
			entries = append(entries, "  "+heading(t, group))
			lastGroup = group
		}
		identity := runtime.GetResource()
		if runtime.GetKind() == orchv1alpha1.RuntimeKind_RUNTIME_KIND_SERVICE {
			identity += fmt.Sprintf(" #%d", runtime.GetReplica())
		}
		if index == m.runtimeSelected {
			selectedRow = len(entries)
		}
		if width < 38 {
			secondary := runtime.GetNodeName()
			if runtime.GetKind() == orchv1alpha1.RuntimeKind_RUNTIME_KIND_CRON_RUN {
				secondary = compactID(runtime.GetRunId())
			}
			entries = append(entries,
				truncate(marker+"  "+sanitize(identity), width-2),
				truncate("    "+muted(t, secondary+" · ")+statusStyle(t, runtimeState(runtime)), width-2))
		} else {
			state := runtimeState(runtime)
			identity = truncate(identity, max(8, width-lipgloss.Width(state)-8))
			entries = append(entries, truncate(fmt.Sprintf("%s  %s  %s", marker, sanitize(identity), statusStyle(t, state)), width-2))
		}
	}
	if m.snapshot.RuntimesTruncated {
		entries = append(entries, muted(t, "inventory truncated at 2,000 entries"))
	}
	visible := max(2, height-4)
	start := windowStart(selectedRow, visible, len(entries))
	rows = append(rows, entries[start:min(len(entries), start+visible)]...)
	return panel(t, strings.Join(rows, "\n"), width, height)
}

func (m *Model) renderRuntimeDetail(t theme, width, height int, runtime *orchv1alpha1.RuntimeSummary) string {
	identity := runtime.GetApplication() + "/" + runtime.GetResource()
	if runtime.GetKind() == orchv1alpha1.RuntimeKind_RUNTIME_KIND_SERVICE {
		identity += fmt.Sprintf(" #%d", runtime.GetReplica())
	} else {
		identity += " · " + shortID(runtime.GetRunId())
	}
	lines := []string{heading(t, identity), statusStyle(t, runtimeState(runtime)), "",
		labelValue(t, "Node", runtime.GetNodeName()), labelValue(t, "Connection", enumLabel(runtime.GetNodeConnection().String()))}
	if runtime.GetAddress() != "" {
		lines = append(lines, labelValue(t, "Address", runtime.GetAddress()))
	}
	if runtime.GetDeclaredPort() != 0 {
		lines = append(lines, labelValue(t, "Port", fmt.Sprint(runtime.GetDeclaredPort())))
	}
	if runtime.GetRevisionId() != "" {
		lines = append(lines, labelValue(t, "Revision", shortID(runtime.GetRevisionId())))
	}
	if runtime.GetSpecDigest() != "" {
		lines = append(lines, labelValue(t, "Spec", shortID(runtime.GetSpecDigest())))
	}
	if runtime.GetCpuMillis() != 0 || runtime.GetMemoryBytes() != 0 {
		lines = append(lines, labelValue(t, "Resources", fmt.Sprintf("%dm · %s", runtime.GetCpuMillis(), bytesLabel(runtime.GetMemoryBytes()))))
	}
	if runtime.GetKind() == orchv1alpha1.RuntimeKind_RUNTIME_KIND_SERVICE {
		lines = append(lines, labelValue(t, "Restarts", fmt.Sprint(runtime.GetRestartCount())))
	}
	if runtime.GetScheduledAt() != nil {
		lines = append(lines, labelValue(t, "Scheduled", relativeTime(runtime.GetScheduledAt().AsTime())))
	}
	if runtime.GetStartedAt() != nil {
		lines = append(lines, labelValue(t, "Started", relativeTime(runtime.GetStartedAt().AsTime())))
	}
	if runtime.GetFinishedAt() != nil {
		lines = append(lines, labelValue(t, "Finished", relativeTime(runtime.GetFinishedAt().AsTime())))
	}
	if runtime.GetUpdatedAt() != nil {
		lines = append(lines, labelValue(t, "Updated", relativeTime(runtime.GetUpdatedAt().AsTime())))
	}
	if runtime.GetReason() != "" {
		lines = append(lines, labelValue(t, "Reason", runtime.GetReason()))
	}
	if runtime.GetHasExitCode() {
		lines = append(lines, labelValue(t, "Exit", fmt.Sprint(runtime.GetExitCode())))
	}
	if runtime.GetMessage() != "" {
		lines = append(lines, "", muted(t, runtime.GetMessage()))
	}
	return panel(t, strings.Join(lines, "\n"), width, height)
}

func (m *Model) renderRuntimeLogPane(t theme, width, height int, runtime *orchv1alpha1.RuntimeSummary) string {
	source := runtimeLogSource(runtime)
	selectedSource := source == m.logSource
	focus := ""
	if m.runtimeFocus == 1 {
		focus = " ●"
	}
	state := "follow"
	if m.logPaused {
		state = "paused"
	}
	if m.logExited {
		state = "exited"
	}
	header := heading(t, "Logs"+focus) + "  " + muted(t, fmt.Sprintf("%s · %s · wrap %t", m.logStream, state, m.logWrap))
	if m.logsLoading && !selectedSource {
		return panel(t, header+"\n\n"+muted(t, "Opening "+source.application+"/"+source.resource+runtimeSuffix(runtime)+" logs…"), width, height)
	}
	if !selectedSource {
		return panel(t, header+"\n\n"+muted(t, "Press enter to open "+source.application+"/"+source.resource+runtimeSuffix(runtime)+" logs."), width, height)
	}
	if m.logsLoading && len(m.logs) == 0 {
		return panel(t, header+"\n\n"+muted(t, "Loading bounded logs…"), width, height)
	}
	lines := m.displayLogs()
	meta := fmt.Sprintf("%d lines · %.0f KiB", len(lines), float64(logBytes(m.logs))/1024)
	if m.logUnseen > 0 {
		meta += fmt.Sprintf(" · %d unseen", m.logUnseen)
	}
	rows := []string{header, muted(t, source.application+"/"+source.resource+runtimeSuffix(runtime)+" · "+meta)}
	if m.logNotice != "" {
		rows = append(rows, colorize(t, t.warning, "! "+m.logNotice))
	}
	if len(lines) == 0 {
		rows = append(rows, "", muted(t, "Connected · this process has not written to stdout or stderr yet."))
		return panel(t, strings.Join(rows, "\n"), width, height)
	}
	position := min(m.selected[ViewLogs], len(lines)-1)
	visible := max(2, height-len(rows)-2)
	start := windowStart(position, visible, len(lines))
	for index := start; index < min(len(lines), start+visible); index++ {
		line := lines[index]
		marker := " "
		if index == position && m.runtimeFocus == 1 {
			marker = colorize(t, t.primary, "›")
		}
		value := line.text
		if !m.logWrap {
			value = truncate(value, max(8, width-lipgloss.Width(line.stream)-6))
		}
		rows = append(rows, marker+muted(t, line.stream+" ")+value)
	}
	return panel(t, strings.Join(rows, "\n"), width, height)
}

func runtimeSuffix(runtime *orchv1alpha1.RuntimeSummary) string {
	if runtime.GetKind() == orchv1alpha1.RuntimeKind_RUNTIME_KIND_SERVICE {
		return fmt.Sprintf(" #%d", runtime.GetReplica())
	}
	return " " + shortID(runtime.GetRunId())
}

func compactID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:7] + "…"
}

func logBytes(lines []LogLine) int {
	total := 0
	for _, line := range lines {
		total += len(line.Text)
	}
	return total
}

func (m *Model) renderHelp(t theme, width int) string {
	content := heading(t, "Keyboard guide") + `

Navigation                 Discovery
  1–7        switch view     ctrl+p / :  command palette
  tab        next view       /           find resource
  ↑↓ or jk   move            ?           toggle this guide
  enter      open            r           refresh now
  esc        back            q           quit

Node actions               Runtime workspace
  c           cordon          /           search text
  u           uncordon        n / N       next / prior match
  d           drain
  i           toggle ingress  s           stdout/stderr/all
                              w           toggle wrapping
                              p           pause/resume follow
                              g           group app/node
                              h / l       tree/log pane
                              G           jump to live output

Deployments (d from Applications)
  enter compare · b rollback · n older · N latest

Events
  /           filter kind or payload
  x           clear active filters

Mutations show their target and consequence before submission.
Data remains visible and is marked degraded when a refresh fails.`
	return panel(t, content, width, min(m.height-4, 24))
}

func (m *Model) renderPalette(t theme, width int) string {
	items := m.paletteItems()
	lines := []string{heading(t, "Command palette"), m.paletteInput.View(), ""}
	start := windowStart(m.paletteIndex, 9, len(items))
	for index := start; index < min(len(items), start+9); index++ {
		item := items[index]
		marker := "  "
		if index == m.paletteIndex {
			marker = colorize(t, t.primary, "› ")
		}
		lines = append(lines, marker+truncate(item.label, width-5))
	}
	if len(items) == 0 {
		lines = append(lines, muted(t, "  No matching commands or resources"))
	}
	return panel(t, strings.Join(lines, "\n"), width, min(15, len(lines)+3))
}

func (m *Model) renderSearch(t theme, width int) string {
	scope := "events"
	if m.view == ViewLogs {
		if m.runtimeFocus == 0 {
			scope = "runtimes"
		} else {
			scope = "logs"
		}
	}
	content := heading(t, "Filter "+scope) + "\n\n" + m.searchInput.View() + "\n\n" + muted(t, "enter apply · esc close · empty clears")
	return panel(t, content, width, 7)
}

func (m *Model) renderConfirmation(t theme, width int) string {
	if m.confirm.rollback != nil {
		return m.renderRollbackConfirmation(t, width)
	}
	state := "y confirm · n/esc cancel"
	if m.mutationBusy {
		state = "Applying safely…"
	}
	currentLifecycle := enumLabel(m.confirm.lifecycle.String())
	requestedLifecycle := currentLifecycle
	if m.confirm.request.GetLifecycle() != orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_UNSPECIFIED {
		requestedLifecycle = enumLabel(m.confirm.request.GetLifecycle().String())
	}
	currentIngress, requestedIngress := m.confirm.ingress, m.confirm.ingress
	if m.confirm.request.GetSetIngress() {
		requestedIngress = m.confirm.request.GetIngress()
	}
	content := heading(t, "Confirm node operation") + "\n\n" +
		labelValue(t, "Target", m.target) + "\n" +
		labelValue(t, "Node", m.confirm.name) + "\n" +
		labelValue(t, "Lifecycle", fmt.Sprintf("%s -> %s", currentLifecycle, requestedLifecycle)) + "\n" +
		labelValue(t, "Ingress", fmt.Sprintf("%t -> %t", currentIngress, requestedIngress)) + "\n\n" +
		sanitize(m.confirm.description) + "\n\n" + colorize(t, t.warning, state)
	return panel(t, content, width, 14)
}

func metricCard(t theme, title, value, note string, width int) string {
	return panel(t, muted(t, title)+"\n"+bold(t, value)+"\n"+muted(t, note), width, 5)
}

func panel(t theme, content string, width, height int) string {
	style := lipgloss.NewStyle().Width(max(1, width-2)).Height(max(1, height-2)).Padding(0, 1)
	if !t.plain {
		style = style.Border(lipgloss.RoundedBorder()).BorderForeground(t.surface).Foreground(t.text)
	} else {
		style = style.Border(lipgloss.NormalBorder())
	}
	return style.Render(content)
}

func surface(t theme, content string, width int) string {
	style := lipgloss.NewStyle().Width(width)
	if !t.plain {
		style = style.Background(t.surface).Foreground(t.text)
	}
	return style.Render(content)
}

func active(t theme, value string) string {
	if t.plain {
		return "[" + strings.TrimSpace(value) + "]"
	}
	style := lipgloss.NewStyle().Bold(true)
	style = style.Foreground(t.text).Background(t.primary)
	return style.Render(value)
}

func bold(t theme, value string) string {
	value = sanitize(value)
	if t.plain {
		return value
	}
	return lipgloss.NewStyle().Bold(true).Render(value)
}
func muted(t theme, value string) string   { return colorize(t, t.muted, sanitize(value)) }
func heading(t theme, value string) string { return colorize(t, t.primary, bold(t, value)) }

func colorize(t theme, color color.Color, value string) string {
	if t.plain {
		return value
	}
	return lipgloss.NewStyle().Foreground(color).Render(value)
}

func statusStyle(t theme, value string) string {
	label := enumLabel(value)
	color := t.warning
	upper := strings.ToUpper(value)
	if strings.Contains(upper, "HEALTHY") || strings.Contains(upper, "CONNECTED") || strings.Contains(upper, "SUCCEEDED") || strings.Contains(upper, "RUNNING") {
		color = t.good
	} else if strings.Contains(upper, "FAILED") || strings.Contains(upper, "UNREACHABLE") || strings.Contains(upper, "DEGRADED") {
		color = t.danger
	}
	return colorize(t, color, label)
}

func empty(t theme, title, guidance string) string {
	return "\n\n  " + heading(t, title) + "\n  " + muted(t, guidance)
}

func labelValue(t theme, label, value string) string {
	return fmt.Sprintf("%-12s %s", muted(t, label), sanitize(value))
}

func centerText(t theme, width, height int, value string) string {
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, muted(t, value))
}

func overlayCentered(base, overlay string, width, height int) string {
	baseLayer := lipgloss.NewLayer(base).X(0).Y(0).Z(0)
	overlayLayer := lipgloss.NewLayer(overlay).
		X(max(0, (width-lipgloss.Width(overlay))/2)).
		Y(max(0, (height-lipgloss.Height(overlay))/2)).Z(1)
	return lipgloss.NewCompositor(baseLayer, overlayLayer).Render()
}

func fitHeight(value string, height, width int) string {
	lines := strings.Split(value, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	for index := range lines {
		lines[index] = truncate(lines[index], width)
	}
	return strings.Join(lines, "\n")
}

func truncate(value string, width int) string {
	if width <= 0 || lipgloss.Width(value) <= width {
		return value
	}
	return ansi.Truncate(value, width, "…")
}

func windowStart(selected, visible, total int) int {
	if total <= visible || selected < visible/2 {
		return 0
	}
	return min(selected-visible/2, total-visible)
}

func relativeTime(stamp time.Time) string {
	if stamp.IsZero() {
		return "never"
	}
	duration := time.Since(stamp)
	if duration < time.Minute {
		return fmt.Sprintf("%ds ago", max(0, int(duration.Seconds())))
	}
	if duration < time.Hour {
		return fmt.Sprintf("%dm ago", int(duration.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(duration.Hours()))
}

func formatTime(stamp time.Time) string {
	if stamp.IsZero() {
		return "--:--:--"
	}
	return stamp.Local().Format("15:04:05")
}

func bytesLabel(value uint64) string {
	if value >= 1<<30 {
		return fmt.Sprintf("%.1fGi", float64(value)/float64(1<<30))
	}
	if value >= 1<<20 {
		return fmt.Sprintf("%.0fMi", float64(value)/float64(1<<20))
	}
	return fmt.Sprintf("%dB", value)
}
