package tui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type deploymentWorkspace struct {
	deploymentApp                         string
	deploymentHistory                     []*orchv1alpha1.Deployment
	deploymentCurrent                     *orchv1alpha1.Application
	deploymentBefore, deploymentNext      uint64
	deploymentSelection, deploymentScroll int
	deploymentDetail                      bool
	deploymentLoading                     bool
	deploymentError                       string
	deploymentGeneration                  uint64
	deploymentCancel                      context.CancelFunc
	deploymentDiff                        *orchv1alpha1.CompareDeploymentsResponse
	deploymentDiffGeneration              uint64
	deploymentDiffCancel                  context.CancelFunc
	deploymentDiffError                   string
}

type deploymentsMsg struct {
	generation uint64
	app        string
	history    *orchv1alpha1.ListDeploymentsResponse
	current    *orchv1alpha1.Application
	err        error
}
type deploymentDiffMsg struct {
	generation            uint64
	app, revision, target string
	response              *orchv1alpha1.CompareDeploymentsResponse
	err                   error
}
type rollbackMsg struct {
	request  *orchv1alpha1.RollbackApplicationRequest
	response *orchv1alpha1.ApplyApplicationResponse
	err      error
}

func (m *Model) cancelDeployments() {
	if m.deploymentCancel != nil {
		m.deploymentCancel()
		m.deploymentCancel = nil
	}
	if m.deploymentDiffCancel != nil {
		m.deploymentDiffCancel()
		m.deploymentDiffCancel = nil
	}
	m.deploymentGeneration++
	m.deploymentDiffGeneration++
	m.deploymentLoading = false
}

func (m *Model) openDeployments(app string) tea.Cmd {
	m.cancelDeployments()
	m.deploymentWorkspace = deploymentWorkspace{deploymentApp: app, deploymentGeneration: m.deploymentGeneration, deploymentDiffGeneration: m.deploymentDiffGeneration}
	m.setView(ViewDeployments)
	return m.refreshDeployments()
}

func (m *Model) refreshDeployments() tea.Cmd {
	if m.deploymentApp == "" {
		return nil
	}
	if m.deploymentCancel != nil {
		m.deploymentCancel()
	}
	m.deploymentGeneration++
	generation, app, before, provider := m.deploymentGeneration, m.deploymentApp, m.deploymentBefore, m.provider
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	m.deploymentCancel = cancel
	m.deploymentLoading = true
	return func() tea.Msg {
		defer cancel()
		current, err := provider.GetApplication(ctx, app)
		if err != nil {
			return deploymentsMsg{generation: generation, app: app, err: err}
		}
		history, err := provider.ListDeployments(ctx, app, before, 20)
		return deploymentsMsg{generation: generation, app: app, current: current, history: history, err: err}
	}
}

func (m *Model) selectedDeployment() *orchv1alpha1.Deployment {
	if m.deploymentSelection < 0 || m.deploymentSelection >= len(m.deploymentHistory) {
		return nil
	}
	return m.deploymentHistory[m.deploymentSelection]
}

func (m *Model) receiveDeployments(msg deploymentsMsg) tea.Cmd {
	if msg.generation != m.deploymentGeneration || msg.app != m.deploymentApp || m.view != ViewDeployments {
		return nil
	}
	m.deploymentLoading = false
	if msg.err != nil {
		m.deploymentError = safeError(msg.err)
		return nil
	}
	selected := m.selectedDeployment().GetRevisionId()
	target := m.deploymentCurrent.GetRevisionId()
	m.deploymentError = ""
	m.deploymentCurrent, m.deploymentHistory, m.deploymentNext = msg.current, msg.history.GetDeployments(), msg.history.GetNextBeforeGeneration()
	m.deploymentSelection = 0
	for index, d := range m.deploymentHistory {
		if d.GetRevisionId() == selected {
			m.deploymentSelection = index
			break
		}
	}
	if target != msg.current.GetRevisionId() || selected != m.selectedDeployment().GetRevisionId() {
		m.clearDeploymentDiff()
		if m.deploymentDetail {
			return m.compareSelectedDeployment()
		}
	}
	return nil
}

func (m *Model) clearDeploymentDiff() {
	if m.deploymentDiffCancel != nil {
		m.deploymentDiffCancel()
		m.deploymentDiffCancel = nil
	}
	m.deploymentDiffGeneration++
	m.deploymentDiff, m.deploymentDiffError, m.deploymentScroll = nil, "", 0
}

func (m *Model) compareSelectedDeployment() tea.Cmd {
	m.clearDeploymentDiff()
	selected := m.selectedDeployment()
	if selected == nil || m.deploymentCurrent == nil {
		return nil
	}
	app, revision, target, generation, provider := m.deploymentApp, selected.GetRevisionId(), m.deploymentCurrent.GetRevisionId(), m.deploymentDiffGeneration, m.provider
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	m.deploymentDiffCancel = cancel
	return func() tea.Msg {
		defer cancel()
		response, err := provider.CompareDeployments(ctx, app, revision, target)
		return deploymentDiffMsg{generation: generation, app: app, revision: revision, target: target, response: response, err: err}
	}
}

func (m *Model) receiveDeploymentDiff(msg deploymentDiffMsg) {
	if m.view != ViewDeployments || msg.generation != m.deploymentDiffGeneration || msg.app != m.deploymentApp ||
		msg.revision != m.selectedDeployment().GetRevisionId() || msg.target != m.deploymentCurrent.GetRevisionId() {
		return
	}
	if msg.err != nil {
		m.deploymentDiffError = safeError(msg.err)
		return
	}
	m.deploymentDiff, m.deploymentDiffError = msg.response, ""
}

func (m *Model) handleDeploymentKey(key string) (bool, tea.Cmd) {
	switch key {
	case "up", "k", "down", "j":
		delta := 1
		if key == "up" || key == "k" {
			delta = -1
		}
		if m.deploymentDetail {
			m.deploymentScroll = min(m.deploymentScrollLimit(), max(0, min(m.deploymentScroll, m.deploymentScrollLimit())+delta))
		} else if len(m.deploymentHistory) > 0 {
			count := len(m.deploymentHistory)
			m.deploymentSelection = (m.deploymentSelection + delta + count) % count
			m.clearDeploymentDiff()
		}
	case "enter", "d":
		if m.selectedDeployment() != nil {
			m.deploymentDetail = true
			return true, m.compareSelectedDeployment()
		}
	case "esc", "h", "left":
		if m.deploymentDetail {
			m.deploymentDetail = false
		} else {
			m.setView(ViewApplications)
		}
	case "r":
		return true, tea.Batch(m.refreshDeployments(), m.compareSelectedDeployment())
	case "n", "N":
		if key == "n" && m.deploymentNext == 0 {
			return true, nil
		}
		m.deploymentBefore = m.deploymentNext
		if key == "N" {
			m.deploymentBefore = 0
		}
		m.deploymentHistory, m.deploymentSelection, m.deploymentDetail = nil, 0, false
		m.clearDeploymentDiff()
		return true, m.refreshDeployments()
	case "b":
		m.prepareRollback()
	case "home":
		m.deploymentScroll = 0
	case "end", "G":
		m.deploymentScroll = m.deploymentScrollLimit()
	default:
		return false, nil
	}
	return true, nil
}

func (m *Model) prepareRollback() {
	selected, current := m.selectedDeployment(), m.deploymentCurrent
	if selected == nil || current == nil || current.GetDeleted() || selected.GetGeneration() >= current.GetGeneration() {
		m.setToast("Select an older deployment to roll back")
		return
	}
	if m.deploymentError != "" || m.lastError != "" {
		m.setToast("Refresh the connection before rolling back")
		return
	}
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		m.setToast("Could not create rollback request")
		return
	}
	m.confirm = &confirmation{name: m.deploymentApp, rollback: &orchv1alpha1.RollbackApplicationRequest{
		Application: m.deploymentApp, RevisionId: selected.GetRevisionId(), ExpectedGeneration: current.GetGeneration(), IdempotencyKey: hex.EncodeToString(bytes),
	}, description: fmt.Sprintf("Restore generation %d as a new deployment.\nSource outcome: %s. Data and secret values are not restored.", selected.GetGeneration(), selected.GetOutcome())}
}

func (m *Model) rollbackConfirmationCurrent() bool {
	req := m.confirm.rollback
	if m.confirm.rollbackRetry {
		return true
	}
	if m.deploymentError != "" || m.lastError != "" || m.deploymentCurrent.GetDeleted() || req.GetExpectedGeneration() != m.deploymentCurrent.GetGeneration() {
		return false
	}
	for _, app := range m.snapshot.Applications {
		if app.GetName() == req.GetApplication() {
			return app.GetGeneration() == req.GetExpectedGeneration()
		}
	}
	return false
}

func (m *Model) submitRollback(request *orchv1alpha1.RollbackApplicationRequest) tea.Cmd {
	provider, parent := m.provider, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 15*time.Second)
		defer cancel()
		response, err := provider.RollbackApplication(ctx, request)
		return rollbackMsg{request: request, response: response, err: err}
	}
}

func (m *Model) receiveRollback(msg rollbackMsg) tea.Cmd {
	m.mutationBusy = false
	if msg.err != nil {
		// Keep the exact request for an uncertain retry. The server checks replay
		// before generation conflicts, so a lost acceptance reply cannot double-apply.
		if m.confirm != nil && m.confirm.rollback == msg.request {
			m.confirm.rollbackRetry = true
		}
		m.setToast("Rollback failed: " + safeError(msg.err))
		return nil
	}
	m.confirm = nil
	m.setToast(fmt.Sprintf("Rollback accepted · generation %d", msg.response.GetGeneration()))
	m.deploymentBefore, m.deploymentSelection, m.deploymentDetail = 0, 0, false
	m.deploymentHistory = nil
	m.clearDeploymentDiff()
	return tea.Batch(m.beginRefresh(), m.refreshDeployments())
}

func (m *Model) renderDeployments(t theme, width, height int) string {
	if m.deploymentApp == "" {
		return empty(t, "Deployment history", "Select an application and press d, or use ctrl+p.")
	}
	if m.deploymentCurrent == nil {
		return empty(t, "Deployments / "+sanitize(m.deploymentApp), "Loading… "+sanitize(m.deploymentError))
	}
	if width < 100 && m.deploymentDetail {
		return m.renderDeploymentDetail(t, width, height)
	}
	listWidth := width
	if width >= 100 {
		listWidth = width * 40 / 100
	}
	current := m.deploymentCurrent
	lines := []string{heading(t, "Deployments / "+sanitize(m.deploymentApp)),
		fmt.Sprintf("Desired %d · active %d · %s", current.GetGeneration(), current.GetActiveGeneration(), enumLabel(current.GetExecutionStatus().String()))}
	if m.deploymentError != "" {
		lines = append(lines, "STALE: "+sanitize(m.deploymentError))
	}
	if current.GetDeleted() {
		lines = append(lines, "Application removed · history retained")
	}
	lines = append(lines, "", "   GEN  OUTCOME       CREATED")
	visible := max(1, height-len(lines)-3)
	start := windowStart(m.deploymentSelection, visible, len(m.deploymentHistory))
	for i := start; i < min(len(m.deploymentHistory), start+visible); i++ {
		d := m.deploymentHistory[i]
		marker := "  "
		if i == m.deploymentSelection {
			marker = "› "
		}
		flags := ""
		if d.GetGeneration() == current.GetGeneration() {
			flags += " desired"
		}
		if d.GetRevisionId() == current.GetActiveRevisionId() {
			flags += " active"
		}
		lines = append(lines, fmt.Sprintf("%s%-4d %-12s %s%s", marker, d.GetGeneration(), d.GetOutcome(), d.GetCreatedAt().AsTime().Format("Jan 02 15:04"), flags))
	}
	if len(m.deploymentHistory) == 0 {
		lines = append(lines, "No deployments on this page.")
	}
	lines = append(lines, "", "enter details / compare · b rollback")
	if m.deploymentNext != 0 {
		lines = append(lines, "n older page · N latest page")
	}
	list := panel(t, strings.Join(lines, "\n"), listWidth, height)
	if width < 100 {
		return list
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, list, m.renderDeploymentDetail(t, width-listWidth, height))
}

func (m *Model) deploymentDetailLines(width int) []string {
	d := m.selectedDeployment()
	if d == nil {
		return []string{"Select a deployment"}
	}
	current := m.deploymentCurrent
	lines := []string{fmt.Sprintf("%s / generation %d · %s", m.deploymentApp, d.GetGeneration(), d.GetOutcome()),
		"Revision: " + d.GetRevisionId(), "Created: " + d.GetCreatedAt().AsTime().Format(time.RFC3339),
		"Last observation: " + enumLabel(d.GetExecutionStatus().String()), d.GetReason(), d.GetMessage()}
	if d.GetFinishedAt() != nil {
		lines = append(lines, "Finished: "+d.GetFinishedAt().AsTime().Format(time.RFC3339))
	}
	if d.GetRollbackFromRevisionId() != "" {
		lines = append(lines, "Rollback source: "+d.GetRollbackFromRevisionId())
	}
	lines = append(lines, "", "Images")
	for _, image := range d.GetImages() {
		lines = append(lines, image.GetKind()+" "+image.GetName()+": "+image.GetImage())
	}
	lines = append(lines, "", fmt.Sprintf("Live rollout · desired %d / active %d", current.GetGeneration(), current.GetActiveGeneration()), enumLabel(current.GetExecutionStatus().String())+" · "+current.GetExecutionReason())
	for _, a := range current.GetAllocations() {
		lines = append(lines, fmt.Sprintf("%s #%d on %s: %s / %s (active %d)", a.GetService(), a.GetReplica(), a.GetNodeName(), enumLabel(a.GetPhase().String()), enumLabel(a.GetHealthStatus().String()), a.GetActiveApplicationGeneration()))
	}
	lines = append(lines, "", "Compare selected → current desired")
	if m.deploymentDiffError != "" {
		lines = append(lines, "Comparison unavailable: "+m.deploymentDiffError)
	} else if m.deploymentDiff == nil {
		lines = append(lines, "Press enter to compare")
	} else {
		if len(m.deploymentDiff.GetChanges()) == 0 {
			lines = append(lines, "No configuration changes")
		}
		for _, change := range m.deploymentDiff.GetChanges() {
			lines = append(lines, change.GetKind()+" "+change.GetPath()+": "+change.GetBefore()+" → "+change.GetAfter())
		}
		if m.deploymentDiff.GetTruncated() {
			lines = append(lines, "Comparison truncated to 500 changes")
		}
	}
	if m.deploymentError != "" {
		lines = append([]string{"STALE: " + m.deploymentError}, lines...)
	}
	return strings.Split(ansi.Hardwrap(sanitize(strings.Join(lines, "\n")), max(1, width-4), true), "\n")
}

func (m *Model) deploymentScrollLimit() int {
	t := currentTheme()
	width := m.width
	if width >= 100 {
		width -= width * 40 / 100
	}
	bodyHeight := m.height - lipgloss.Height(m.renderHeader(t, m.width)) - lipgloss.Height(m.renderTabs(t, m.width)) - lipgloss.Height(m.renderFooter(t, m.width))
	return max(0, len(m.deploymentDetailLines(width))-max(1, bodyHeight-4))
}

func (m *Model) renderDeploymentDetail(t theme, width, height int) string {
	wrapped := m.deploymentDetailLines(width)
	visible := max(1, height-4)
	offset := min(m.deploymentScroll, max(0, len(wrapped)-visible))
	content := strings.Join(wrapped[offset:min(len(wrapped), offset+visible)], "\n")
	content += fmt.Sprintf("\n%d–%d/%d · enter focus · ↑↓ scroll · esc back", offset+1, min(len(wrapped), offset+visible), len(wrapped))
	return panel(t, content, width, height)
}

func (m *Model) renderRollbackConfirmation(t theme, width int) string {
	req := m.confirm.rollback
	state := "y confirm · n/esc cancel"
	if m.mutationBusy {
		state = "Submitting rollback…"
	}
	retry := ""
	if m.confirm.rollbackRetry {
		retry = "\nRetry preserves the original request."
	}
	content := fmt.Sprintf("Confirm rollback\nTarget: %s\nApplication: %s\nReplace generation: %d\nSource: %s\n%s%s\n%s", truncate(m.target, width-14), req.GetApplication(), req.GetExpectedGeneration(), req.GetRevisionId(), m.confirm.description, retry, state)
	return panel(t, ansi.Hardwrap(sanitize(content), max(1, width-4), true), width, 0)
}
