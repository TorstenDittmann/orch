package tui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type ViewID int

const (
	ViewOverview ViewID = iota
	ViewApplications
	ViewNodes
	ViewEvents
	ViewLogs
	ViewHelp
)

var viewNames = []string{"Overview", "Applications", "Nodes", "Events", "Logs", "Help"}

type Snapshot struct {
	Cluster      *orchv1alpha1.Cluster
	Applications []*orchv1alpha1.ApplicationSummary
	Nodes        []*orchv1alpha1.Node
	Events       []*orchv1alpha1.Event
	EventCursor  uint64
	RefreshedAt  time.Time
}

type refreshMsg struct {
	generation uint64
	snapshot   Snapshot
	errors     []error
}

type detailMsg struct {
	name        string
	application *orchv1alpha1.Application
	err         error
}

type logsMsg struct {
	application string
	resource    string
	kind        string
	replica     uint32
	lines       []LogLine
	err         error
}

type mutationMsg struct {
	name     string
	response *orchv1alpha1.UpdateNodeResponse
	err      error
}

type tickMsg time.Time

type paletteItem struct {
	label       string
	kind        string
	name        string
	application string
	resource    string
	replica     uint32
	view        ViewID
}

type logSource struct {
	kind, application, resource string
	replica                     uint32
}

type displayLogLine struct {
	stream string
	text   string
}

type confirmation struct {
	name        string
	description string
	request     *orchv1alpha1.UpdateNodeRequest
	lifecycle   orchv1alpha1.NodeLifecycle
	ingress     bool
}

type Model struct {
	ctx       context.Context
	provider  Provider
	target    string
	transport string
	refresh   time.Duration
	cancel    context.CancelFunc

	width, height int
	view          ViewID
	previousView  ViewID
	selected      map[ViewID]int
	snapshot      Snapshot
	details       map[string]*orchv1alpha1.Application
	loading       bool
	generation    uint64
	lastError     string
	palette       bool
	paletteInput  textinput.Model
	paletteIndex  int
	search        bool
	searchInput   textinput.Model
	help          bool
	confirm       *confirmation
	mutationBusy  bool
	toast         string
	toastUntil    time.Time
	logs          []LogLine
	logsLoading   bool
	logSource     logSource
	logWrap       bool
	logFilter     string
	logStream     string
	logPaused     bool
	eventFilter   string
}

func New(ctx context.Context, provider Provider, targetName, transport string, refresh time.Duration) *Model {
	input := textinput.New()
	input.Prompt = "> "
	input.Placeholder = "Jump to a view, application, or node"
	input.CharLimit = 80
	search := textinput.New()
	search.Prompt = "/ "
	search.Placeholder = "Filter visible events or logs"
	search.CharLimit = 120
	return &Model{
		ctx: ctx, provider: provider, target: targetName, transport: transport, refresh: refresh,
		view: ViewOverview, previousView: ViewOverview, selected: make(map[ViewID]int),
		details: make(map[string]*orchv1alpha1.Application), paletteInput: input, searchInput: search,
		logWrap: true, logStream: "all",
	}
}

func (m *Model) Init() tea.Cmd { return tea.Batch(m.beginRefresh(), m.tick()) }

func (m *Model) tick() tea.Cmd {
	return tea.Tick(m.refresh, func(stamp time.Time) tea.Msg { return tickMsg(stamp) })
}

func (m *Model) beginRefresh() tea.Cmd {
	if m.cancel != nil {
		m.cancel()
	}
	m.generation++
	generation := m.generation
	ctx, cancel := context.WithTimeout(m.ctx, min(m.refresh, 10*time.Second))
	m.cancel = cancel
	m.loading = true
	after := m.snapshot.EventCursor
	return func() tea.Msg {
		defer cancel()
		var snapshot Snapshot
		var errs []error
		var mu sync.Mutex
		var wait sync.WaitGroup
		run := func(operation func() error) {
			wait.Add(1)
			go func() {
				defer wait.Done()
				if err := operation(); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}()
		}
		run(func() error {
			value, err := m.provider.GetCluster(ctx)
			snapshot.Cluster = value
			return labeledError("cluster", err)
		})
		run(func() error {
			value, err := m.provider.ListApplications(ctx)
			snapshot.Applications = value
			return labeledError("applications", err)
		})
		run(func() error {
			value, err := m.provider.ListNodes(ctx)
			snapshot.Nodes = value
			return labeledError("nodes", err)
		})
		run(func() error {
			if after == 0 {
				value, err := m.provider.ListRecentEvents(ctx, 200)
				if value != nil {
					snapshot.Events = value.GetEvents()
					snapshot.EventCursor = value.GetNextAfterSequence()
				}
				return labeledError("events", err)
			}
			value, err := m.provider.ListEvents(ctx, after, 200)
			if value != nil {
				snapshot.Events = value.GetEvents()
				snapshot.EventCursor = value.GetNextAfterSequence()
			}
			return labeledError("events", err)
		})
		wait.Wait()
		snapshot.RefreshedAt = time.Now()
		return refreshMsg{generation: generation, snapshot: snapshot, errors: errs}
	}
}

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = message.Width, message.Height
		return m, nil
	case tickMsg:
		if !m.toastUntil.IsZero() && !time.Time(message).Before(m.toastUntil) {
			m.toast = ""
			m.toastUntil = time.Time{}
		}
		commands := []tea.Cmd{m.beginRefresh(), m.tick()}
		if m.view == ViewLogs && !m.logPaused && m.logSource.application != "" && !m.logsLoading {
			m.logsLoading = true
			commands = append(commands, m.reloadLogs())
		}
		return m, tea.Batch(commands...)
	case refreshMsg:
		if message.generation != m.generation {
			return m, nil
		}
		m.loading = false
		m.applyRefresh(message)
		return m, nil
	case detailMsg:
		if message.err != nil {
			m.setToast("Application detail: " + safeError(message.err))
		} else if message.application != nil {
			m.details[message.name] = message.application
		}
		return m, nil
	case logsMsg:
		m.logsLoading = false
		if message.err != nil {
			m.setToast("Logs: " + safeError(message.err))
			return m, nil
		}
		source := logSource{kind: message.kind, application: message.application, resource: message.resource, replica: message.replica}
		changed := source != m.logSource
		m.logs = message.lines
		m.logSource = source
		if changed {
			m.selected[ViewLogs] = 0
			m.previousView, m.view = m.view, ViewLogs
		} else {
			m.clampSelection()
		}
		return m, nil
	case mutationMsg:
		m.mutationBusy = false
		if message.err != nil {
			m.setToast("Update failed: " + safeError(message.err))
			return m, nil
		}
		m.confirm = nil
		m.setToast(fmt.Sprintf("Updated %s · event %d", sanitize(message.name), message.response.GetEventSequence()))
		return m, m.beginRefresh()
	case tea.KeyPressMsg:
		return m.handleKey(message)
	case tea.MouseWheelMsg:
		switch message.Mouse().Button {
		case tea.MouseWheelUp:
			m.move(-3)
		case tea.MouseWheelDown:
			m.move(3)
		}
		return m, nil
	}
	return m, nil
}

func (m *Model) handleKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	stroke := key.Keystroke()
	if m.search {
		return m.handleSearch(key)
	}
	if m.palette {
		return m.handlePalette(key)
	}
	if m.confirm != nil {
		switch stroke {
		case "esc", "n":
			if !m.mutationBusy {
				m.confirm = nil
			}
		case "y":
			if !m.mutationBusy {
				if !m.confirmationCurrent() {
					m.setToast("Node state changed · review the action again")
					m.confirm = nil
					return m, nil
				}
				m.mutationBusy = true
				return m, m.mutate(*m.confirm)
			}
		}
		return m, nil
	}
	if m.help {
		if stroke == "?" || stroke == "esc" || stroke == "q" {
			m.help = false
		}
		return m, nil
	}
	switch stroke {
	case "q", "ctrl+c":
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case "?":
		m.help = true
	case "ctrl+p", ":":
		m.openPalette("")
		return m, m.paletteInput.Focus()
	case "/":
		if m.view == ViewEvents || m.view == ViewLogs {
			return m, m.openSearch()
		}
		m.openPalette("")
		return m, m.paletteInput.Focus()
	case "tab":
		m.setView(ViewID((int(m.view) + 1) % len(viewNames)))
	case "shift+tab":
		m.setView(ViewID((int(m.view) + len(viewNames) - 1) % len(viewNames)))
	case "1", "2", "3", "4", "5", "6":
		m.setView(ViewID(int(stroke[0] - '1')))
	case "r":
		if m.view == ViewLogs && m.logSource.application != "" {
			m.logsLoading = true
			return m, tea.Batch(m.beginRefresh(), m.reloadLogs())
		}
		return m, m.beginRefresh()
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "enter":
		return m, m.openSelected()
	case "esc":
		if m.view == ViewLogs {
			m.view = m.previousView
		}
	case "c", "u", "d", "i":
		if m.view == ViewNodes {
			m.prepareNodeAction(stroke)
		}
	case "w":
		if m.view == ViewLogs {
			m.logWrap = !m.logWrap
		}
	case "s":
		if m.view == ViewLogs {
			switch m.logStream {
			case "all":
				m.logStream = "stdout"
			case "stdout":
				m.logStream = "stderr"
			default:
				m.logStream = "all"
			}
			m.selected[ViewLogs] = 0
		}
	case "p":
		if m.view == ViewLogs {
			m.logPaused = !m.logPaused
		}
	case "n":
		if m.view == ViewLogs && m.logFilter != "" {
			m.move(1)
		}
	case "N":
		if m.view == ViewLogs && m.logFilter != "" {
			m.move(-1)
		}
	case "x":
		if m.view == ViewEvents {
			m.eventFilter = ""
			m.selected[ViewEvents] = 0
		} else if m.view == ViewLogs {
			m.logFilter = ""
			m.logStream = "all"
			m.selected[ViewLogs] = 0
		}
	}
	return m, nil
}

func (m *Model) openSearch() tea.Cmd {
	m.search = true
	value := m.eventFilter
	if m.view == ViewLogs {
		value = m.logFilter
	}
	m.searchInput.SetValue(value)
	m.searchInput.CursorEnd()
	return m.searchInput.Focus()
}

func (m *Model) handleSearch(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.Keystroke() {
	case "esc":
		m.search = false
		m.searchInput.Blur()
		return m, nil
	case "enter":
		m.applySearch(m.searchInput.Value())
		m.search = false
		m.searchInput.Blur()
		return m, nil
	}
	var command tea.Cmd
	m.searchInput, command = m.searchInput.Update(key)
	m.applySearch(m.searchInput.Value())
	return m, command
}

func (m *Model) applySearch(value string) {
	value = strings.TrimSpace(value)
	if m.view == ViewLogs {
		m.logFilter = value
		m.selected[ViewLogs] = 0
	} else {
		m.eventFilter = value
		m.selected[ViewEvents] = 0
	}
}

func (m *Model) applyRefresh(message refreshMsg) {
	hadEvents := len(m.snapshot.Events) != 0
	selectedApplication := ""
	if index := m.selected[ViewApplications]; index >= 0 && index < len(m.snapshot.Applications) {
		selectedApplication = m.snapshot.Applications[index].GetName()
	}
	selectedNode := ""
	if index := m.selected[ViewNodes]; index >= 0 && index < len(m.snapshot.Nodes) {
		selectedNode = m.snapshot.Nodes[index].GetName()
	}
	if message.snapshot.Cluster != nil {
		m.snapshot.Cluster = message.snapshot.Cluster
	}
	if message.snapshot.Applications != nil {
		m.snapshot.Applications = message.snapshot.Applications
	}
	if message.snapshot.Nodes != nil {
		m.snapshot.Nodes = message.snapshot.Nodes
	}
	if message.snapshot.Events != nil {
		if m.snapshot.EventCursor == 0 {
			m.snapshot.Events = message.snapshot.Events
		} else {
			m.snapshot.Events = append(m.snapshot.Events, message.snapshot.Events...)
			if len(m.snapshot.Events) > 1000 {
				m.snapshot.Events = m.snapshot.Events[len(m.snapshot.Events)-1000:]
			}
		}
		m.snapshot.EventCursor = message.snapshot.EventCursor
		if !hadEvents && len(m.snapshot.Events) != 0 {
			m.selected[ViewEvents] = len(m.filteredEvents()) - 1
		}
	}
	if len(message.errors) == 0 {
		m.snapshot.RefreshedAt = message.snapshot.RefreshedAt
		m.lastError = ""
	} else {
		parts := make([]string, 0, len(message.errors))
		for _, err := range message.errors {
			parts = append(parts, safeError(err))
		}
		m.lastError = strings.Join(parts, " · ")
	}
	m.clampSelection()
	if selectedApplication != "" {
		m.selectNamedApplication(selectedApplication)
	}
	if selectedNode != "" {
		m.selectNamedNode(selectedNode)
	}
}

func (m *Model) openPalette(value string) {
	m.palette = true
	m.paletteIndex = 0
	m.paletteInput.SetValue(value)
}

func (m *Model) handlePalette(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.Keystroke() {
	case "esc":
		m.palette = false
		m.paletteInput.Blur()
		return m, nil
	case "up":
		if m.paletteIndex > 0 {
			m.paletteIndex--
		}
		return m, nil
	case "down":
		if m.paletteIndex+1 < len(m.paletteItems()) {
			m.paletteIndex++
		}
		return m, nil
	case "enter":
		items := m.paletteItems()
		if len(items) != 0 {
			item := items[min(m.paletteIndex, len(items)-1)]
			m.setView(item.view)
			if item.kind == "application" {
				m.selectNamedApplication(item.name)
				m.palette = false
				m.paletteInput.Blur()
				return m, m.loadApplication(item.name)
			}
			if item.kind == "node" {
				m.selectNamedNode(item.name)
			}
			if item.kind == "workload-log" || item.kind == "cron-log" {
				m.palette = false
				m.paletteInput.Blur()
				m.logsLoading = true
				return m, m.loadLogSource(logSource{kind: item.kind, application: item.application, resource: item.resource, replica: item.replica})
			}
			if item.kind == "node-action" {
				m.prepareNodeAction(item.resource)
			}
			if item.kind == "refresh" {
				m.palette = false
				m.paletteInput.Blur()
				return m, m.beginRefresh()
			}
			if item.kind == "help" {
				m.help = true
			}
		}
		m.palette = false
		m.paletteInput.Blur()
		return m, nil
	}
	var command tea.Cmd
	m.paletteInput, command = m.paletteInput.Update(key)
	m.paletteIndex = 0
	return m, command
}

func (m *Model) paletteItems() []paletteItem {
	items := make([]paletteItem, 0, len(viewNames)+len(m.snapshot.Applications)+len(m.snapshot.Nodes))
	for index, name := range viewNames {
		items = append(items, paletteItem{label: name, kind: "view", view: ViewID(index)})
	}
	items = append(items,
		paletteItem{label: "Refresh cluster snapshot", kind: "refresh", view: m.view},
		paletteItem{label: "Open keyboard guide", kind: "help", view: m.view},
	)
	for _, application := range m.snapshot.Applications {
		items = append(items, paletteItem{label: "App  " + application.GetName(), kind: "application", name: application.GetName(), view: ViewApplications})
	}
	for _, node := range m.snapshot.Nodes {
		items = append(items, paletteItem{label: "Node " + node.GetName(), kind: "node", name: node.GetName(), view: ViewNodes})
	}
	if len(m.snapshot.Nodes) != 0 {
		items = append(items,
			paletteItem{label: "Action cordon selected node", kind: "node-action", resource: "c", view: ViewNodes},
			paletteItem{label: "Action uncordon selected node", kind: "node-action", resource: "u", view: ViewNodes},
			paletteItem{label: "Action drain selected node", kind: "node-action", resource: "d", view: ViewNodes},
			paletteItem{label: "Action toggle ingress on selected node", kind: "node-action", resource: "i", view: ViewNodes},
		)
	}
	for application, detail := range m.details {
		for _, allocation := range detail.GetAllocations() {
			items = append(items, paletteItem{
				label: "Logs " + application + "/" + allocation.GetService() + fmt.Sprintf(" #%d", allocation.GetReplica()),
				kind:  "workload-log", application: application, resource: allocation.GetService(), replica: allocation.GetReplica(), view: ViewLogs,
			})
		}
		for _, job := range detail.GetCronJobs() {
			items = append(items, paletteItem{
				label: "Cron logs " + application + "/" + job.GetJob() + " latest",
				kind:  "cron-log", application: application, resource: job.GetJob(), view: ViewLogs,
			})
		}
	}
	query := strings.ToLower(strings.TrimSpace(m.paletteInput.Value()))
	if query == "" {
		return items
	}
	filtered := items[:0]
	for _, item := range items {
		if fuzzyMatch(strings.ToLower(item.label), query) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func fuzzyMatch(value, query string) bool {
	for _, needle := range query {
		found := false
		for index, candidate := range value {
			if candidate == needle {
				value = value[index+utf8.RuneLen(candidate):]
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (m *Model) setView(view ViewID) {
	if view != m.view {
		m.previousView, m.view = m.view, view
	}
}

func (m *Model) move(delta int) {
	count := m.itemCount()
	if count == 0 {
		return
	}
	index := m.selected[m.view] + delta
	m.selected[m.view] = ((index % count) + count) % count
}

func (m *Model) itemCount() int {
	switch m.view {
	case ViewApplications:
		return len(m.snapshot.Applications)
	case ViewNodes:
		return len(m.snapshot.Nodes)
	case ViewEvents:
		return len(m.filteredEvents())
	case ViewLogs:
		return len(m.displayLogs())
	default:
		return 0
	}
}

func (m *Model) clampSelection() {
	for _, view := range []ViewID{ViewApplications, ViewNodes, ViewEvents, ViewLogs} {
		count := 0
		switch view {
		case ViewApplications:
			count = len(m.snapshot.Applications)
		case ViewNodes:
			count = len(m.snapshot.Nodes)
		case ViewEvents:
			count = len(m.filteredEvents())
		case ViewLogs:
			count = len(m.displayLogs())
		}
		if count == 0 {
			m.selected[view] = 0
		} else if m.selected[view] >= count {
			m.selected[view] = count - 1
		}
	}
}

func (m *Model) openSelected() tea.Cmd {
	if m.view != ViewApplications || len(m.snapshot.Applications) == 0 {
		return nil
	}
	name := m.snapshot.Applications[m.selected[ViewApplications]].GetName()
	if detail := m.details[name]; detail != nil && len(detail.GetAllocations()) != 0 {
		allocation := detail.GetAllocations()[0]
		m.logsLoading = true
		return m.loadLogSource(logSource{kind: "workload-log", application: name, resource: allocation.GetService(), replica: allocation.GetReplica()})
	}
	return m.loadApplication(name)
}

func (m *Model) loadApplication(name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		application, err := m.provider.GetApplication(ctx, name)
		return detailMsg{name: name, application: application, err: err}
	}
}

func (m *Model) loadLogSource(source logSource) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
		defer cancel()
		var lines []LogLine
		var err error
		if source.kind == "cron-log" {
			lines, err = m.provider.LoadCronLogs(ctx, source.application, source.resource, 500)
		} else {
			lines, err = m.provider.LoadLogs(ctx, source.application, source.resource, source.replica, 500)
		}
		return logsMsg{kind: source.kind, application: source.application, resource: source.resource, replica: source.replica, lines: lines, err: err}
	}
}

func (m *Model) reloadLogs() tea.Cmd { return m.loadLogSource(m.logSource) }

func (m *Model) filteredEvents() []*orchv1alpha1.Event {
	query := strings.ToLower(strings.TrimSpace(m.eventFilter))
	if query == "" {
		return m.snapshot.Events
	}
	result := make([]*orchv1alpha1.Event, 0, len(m.snapshot.Events))
	for _, event := range m.snapshot.Events {
		if strings.Contains(strings.ToLower(event.GetKind()), query) || strings.Contains(strings.ToLower(event.GetPayloadJson()), query) {
			result = append(result, event)
		}
	}
	return result
}

func (m *Model) displayLogs() []displayLogLine {
	query := strings.ToLower(strings.TrimSpace(m.logFilter))
	result := make([]displayLogLine, 0, len(m.logs))
	for _, chunk := range m.logs {
		if m.logStream != "all" && chunk.Stream != m.logStream {
			continue
		}
		for _, line := range strings.Split(strings.TrimSuffix(chunk.Text, "\n"), "\n") {
			if query != "" && !strings.Contains(strings.ToLower(line), query) {
				continue
			}
			result = append(result, displayLogLine{stream: chunk.Stream, text: line})
		}
	}
	return result
}

func (m *Model) prepareNodeAction(key string) {
	if len(m.snapshot.Nodes) == 0 {
		return
	}
	node := m.snapshot.Nodes[m.selected[ViewNodes]]
	request := &orchv1alpha1.UpdateNodeRequest{Name: node.GetName(), IdempotencyKey: idempotencyKey()}
	description := ""
	switch key {
	case "c":
		if node.GetLifecycle() != orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_ACTIVE {
			m.setToast("Cordon is available only for an active node")
			return
		}
		request.Lifecycle = orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_CORDONED
		description = "Cordon this node and stop new placements"
	case "u":
		if node.GetLifecycle() != orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_CORDONED {
			m.setToast("Uncordon is available only for a cordoned node")
			return
		}
		request.Lifecycle = orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_ACTIVE
		description = "Uncordon this node and allow placements"
	case "d":
		if node.GetLifecycle() != orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_ACTIVE && node.GetLifecycle() != orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_CORDONED {
			m.setToast("Drain is available only for an active or cordoned node")
			return
		}
		request.Lifecycle = orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_DRAINING
		description = "Begin draining workloads from this node"
	case "i":
		request.SetIngress = true
		request.Ingress = !node.GetIngress()
		if request.Ingress {
			description = "Enable ingress on this node"
		} else {
			description = "Disable ingress on this node"
		}
	}
	m.confirm = &confirmation{
		name: node.GetName(), description: description, request: request,
		lifecycle: node.GetLifecycle(), ingress: node.GetIngress(),
	}
}

func (m *Model) confirmationCurrent() bool {
	if m.confirm == nil {
		return false
	}
	for _, node := range m.snapshot.Nodes {
		if node.GetName() == m.confirm.name {
			return node.GetLifecycle() == m.confirm.lifecycle && node.GetIngress() == m.confirm.ingress
		}
	}
	return false
}

func (m *Model) setToast(value string) {
	m.toast = value
	m.toastUntil = time.Now().Add(5 * time.Second)
}

func (m *Model) mutate(confirmation confirmation) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
		defer cancel()
		response, err := m.provider.UpdateNode(ctx, confirmation.request)
		return mutationMsg{name: confirmation.name, response: response, err: err}
	}
}

func (m *Model) selectNamedApplication(name string) {
	for index, application := range m.snapshot.Applications {
		if application.GetName() == name {
			m.selected[ViewApplications] = index
			return
		}
	}
}

func (m *Model) selectNamedNode(name string) {
	for index, node := range m.snapshot.Nodes {
		if node.GetName() == name {
			m.selected[ViewNodes] = index
			return
		}
	}
}

func idempotencyKey() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("tui-%d-fallback", time.Now().UnixNano())
	}
	return "tui-" + hex.EncodeToString(value)
}

func labeledError(label string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", label, err)
}

func safeError(err error) string { return sanitize(err.Error()) }

func sanitize(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r == utf8.RuneError || unicode.IsControl(r) {
			return '�'
		}
		return r
	}, strings.ToValidUTF8(value, "�"))
}

func enumLabel(value string) string {
	for _, prefix := range []string{
		"EXECUTION_STATUS_", "NODE_CONNECTION_STATUS_", "NODE_LIFECYCLE_",
		"LOG_STREAM_", "JOB_RUN_STATE_", "ROUTE_STATE_", "HEALTH_STATUS_", "ALLOCATION_PHASE_",
	} {
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimPrefix(value, prefix)
			break
		}
	}
	return strings.ToLower(strings.ReplaceAll(value, "_", " "))
}

func sortedLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, sanitize(key)+"="+sanitize(labels[key]))
	}
	return strings.Join(values, "  ")
}
