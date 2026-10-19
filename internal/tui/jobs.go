package tui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

type jobProvider interface {
	ListJobs(context.Context, string, string, uint32) (*orchv1alpha1.ListJobsResponse, error)
	CancelJob(context.Context, string) (*orchv1alpha1.OneOffJob, error)
	RetryJob(context.Context, string, string) (*orchv1alpha1.OneOffJob, error)
}

// cronRunProvider lists the retained runs of one cron job.
type cronRunProvider interface {
	ListCronRuns(context.Context, string, string, uint32) ([]*orchv1alpha1.JobRunStatus, error)
}

func (p ClientProvider) ListCronRuns(ctx context.Context, application, job string, limit uint32) ([]*orchv1alpha1.JobRunStatus, error) {
	return p.Client.ListCronRuns(ctx, application, job, limit)
}

func (p ClientProvider) ListJobs(ctx context.Context, app, before string, limit uint32) (*orchv1alpha1.ListJobsResponse, error) {
	return p.Client.ListJobs(ctx, app, before, limit)
}
func (p ClientProvider) CancelJob(ctx context.Context, id string) (*orchv1alpha1.OneOffJob, error) {
	return p.Client.CancelJob(ctx, id)
}
func (p ClientProvider) RetryJob(ctx context.Context, id, key string) (*orchv1alpha1.OneOffJob, error) {
	return p.Client.RetryJob(ctx, id, key)
}

type jobConfirmation struct{ id, action, key string }
type jobsWorkspace struct {
	jobsApp, jobsBefore, jobsNext, jobsError string
	jobs                                     []*orchv1alpha1.OneOffJob
	// jobsCron switches the view from one-off jobs to the app's cron runs.
	jobsCron              bool
	cronRuns              []*orchv1alpha1.JobRunStatus
	jobsSelection         int
	jobsGeneration        uint64
	jobsLoading, jobsBusy bool
	jobsCancel            context.CancelFunc
	jobsConfirm           *jobConfirmation
}
type jobsMsg struct {
	generation uint64
	response   *orchv1alpha1.ListJobsResponse
	err        error
	cron       []*orchv1alpha1.JobRunStatus
}
type jobMutationMsg struct {
	confirmation jobConfirmation
	job          *orchv1alpha1.OneOffJob
	err          error
}

func (m *Model) cancelJobs() {
	if m.jobsCancel != nil {
		m.jobsCancel()
		m.jobsCancel = nil
	}
	m.jobsGeneration++
	m.jobsLoading = false
}
func (m *Model) openJobs(app string) tea.Cmd {
	m.cancelJobs()
	generation := m.jobsGeneration
	m.jobsWorkspace = jobsWorkspace{jobsApp: app, jobsGeneration: generation}
	m.setView(ViewJobs)
	return m.refreshJobs()
}
func (m *Model) refreshJobs() tea.Cmd {
	if m.jobsCron {
		return m.refreshCronRuns()
	}
	provider, ok := m.provider.(jobProvider)
	if !ok {
		m.jobsError = "Job operations are unavailable"
		return nil
	}
	if m.jobsCancel != nil {
		m.jobsCancel()
	}
	m.jobsGeneration++
	generation := m.jobsGeneration
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	m.jobsCancel = cancel
	m.jobsLoading = true
	app, before := m.jobsApp, m.jobsBefore
	return func() tea.Msg {
		defer cancel()
		response, err := provider.ListJobs(ctx, app, before, 20)
		return jobsMsg{generation: generation, response: response, err: err}
	}
}

// refreshCronRuns loads the retained runs of every cron job of the app,
// newest first.
func (m *Model) refreshCronRuns() tea.Cmd {
	runs, ok := m.provider.(cronRunProvider)
	if !ok {
		m.jobsError = "Cron runs are unavailable"
		return nil
	}
	if m.jobsApp == "" {
		m.jobsError = "Open Jobs from an application with o to see its cron runs"
		return nil
	}
	if m.jobsCancel != nil {
		m.jobsCancel()
	}
	m.jobsGeneration++
	generation, app, provider := m.jobsGeneration, m.jobsApp, m.provider
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	m.jobsCancel = cancel
	m.jobsLoading = true
	return func() tea.Msg {
		defer cancel()
		application, err := provider.GetApplication(ctx, app)
		if err != nil {
			return jobsMsg{generation: generation, err: err}
		}
		all := []*orchv1alpha1.JobRunStatus{}
		for _, job := range application.GetCronJobs() {
			value, err := runs.ListCronRuns(ctx, app, job.GetJob(), 20)
			if err != nil {
				return jobsMsg{generation: generation, err: err}
			}
			all = append(all, value...)
		}
		sort.SliceStable(all, func(i, j int) bool {
			return all[i].GetScheduledAt().AsTime().After(all[j].GetScheduledAt().AsTime())
		})
		return jobsMsg{generation: generation, cron: all}
	}
}

func (m *Model) jobsCount() int {
	if m.jobsCron {
		return len(m.cronRuns)
	}
	return len(m.jobs)
}

func (m *Model) jobsUnloaded() bool {
	if m.jobsCron {
		return m.cronRuns == nil && m.jobsApp != ""
	}
	return m.jobs == nil
}

func (m *Model) selectedCronRun() *orchv1alpha1.JobRunStatus {
	if !m.jobsCron || m.jobsSelection < 0 || m.jobsSelection >= len(m.cronRuns) {
		return nil
	}
	return m.cronRuns[m.jobsSelection]
}
func (m *Model) selectedJob() *orchv1alpha1.OneOffJob {
	if m.jobsCron || m.jobsSelection < 0 || m.jobsSelection >= len(m.jobs) {
		return nil
	}
	return m.jobs[m.jobsSelection]
}
func (m *Model) receiveJobs(msg jobsMsg) {
	if m.view != ViewJobs || msg.generation != m.jobsGeneration {
		return
	}
	m.jobsLoading = false
	if msg.err != nil {
		m.jobsError = safeError(msg.err)
		return
	}
	if m.jobsCron {
		selected := m.selectedCronRun().GetId()
		m.cronRuns, m.jobsSelection, m.jobsError = msg.cron, 0, ""
		if m.cronRuns == nil {
			m.cronRuns = []*orchv1alpha1.JobRunStatus{}
		}
		for i, run := range m.cronRuns {
			if run.GetId() == selected {
				m.jobsSelection = i
				break
			}
		}
		return
	}
	selected := m.selectedJob().GetId()
	m.jobs = msg.response.GetJobs()
	m.jobsNext = msg.response.GetNextBeforeId()
	m.jobsSelection = 0
	m.jobsError = ""
	for i, job := range m.jobs {
		if job.GetId() == selected {
			m.jobsSelection = i
			break
		}
	}
}
func (m *Model) handleJobKey(key string) (bool, tea.Cmd) {
	if m.jobsConfirm != nil {
		if key == "esc" || key == "n" {
			if !m.jobsBusy {
				m.jobsConfirm = nil
			}
			return true, nil
		}
		if key == "y" && !m.jobsBusy {
			provider, ok := m.provider.(jobProvider)
			if !ok {
				return true, nil
			}
			confirmation := *m.jobsConfirm
			m.jobsBusy = true
			return true, func() tea.Msg {
				ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
				defer cancel()
				var job *orchv1alpha1.OneOffJob
				var err error
				if confirmation.action == "cancel" {
					job, err = provider.CancelJob(ctx, confirmation.id)
				} else {
					job, err = provider.RetryJob(ctx, confirmation.id, confirmation.key)
				}
				return jobMutationMsg{confirmation, job, err}
			}
		}
		return true, nil
	}
	switch key {
	case "up", "k", "down", "j":
		if count := m.jobsCount(); count > 0 {
			delta := 1
			if key == "up" || key == "k" {
				delta = -1
			}
			m.jobsSelection = (m.jobsSelection + delta + count) % count
		}
	case "r":
		return true, m.refreshJobs()
	case "t":
		m.cancelJobs()
		m.jobsCron, m.jobsSelection, m.jobsBefore, m.jobsNext, m.jobsError = !m.jobsCron, 0, "", "", ""
		m.jobs, m.cronRuns = nil, nil
		return true, m.refreshJobs()
	case "n", "N":
		if m.jobsCron {
			return true, nil
		}
		if key == "n" {
			if m.jobsNext == "" {
				return true, nil
			}
			m.jobsBefore = m.jobsNext
		} else {
			m.jobsBefore = ""
		}
		return true, m.refreshJobs()
	case "enter", "l":
		if run := m.selectedCronRun(); run != nil {
			m.logsLoading = true
			return true, m.loadLogSource(logSource{kind: "cron-log", application: run.GetApplication(), resource: run.GetJob(), runID: run.GetId()})
		}
		if job := m.selectedJob(); job != nil {
			m.logsLoading = true
			return true, m.loadLogSource(logSource{kind: "cron-log", application: job.GetApplication(), resource: job.GetStatus().GetJob(), runID: job.GetId()})
		}
	case "c", "x":
		if m.jobsCron {
			m.setToast("Cancel and retry apply to one-off jobs; press t to switch")
			return true, nil
		}
		job := m.selectedJob()
		if job == nil {
			return true, nil
		}
		terminal := job.GetStatus().GetFinishedAt() != nil
		if key == "c" && terminal {
			m.setToast("This run has already finished")
			return true, nil
		}
		if key == "x" && (!terminal || job.GetStatus().GetState() == orchv1alpha1.JobRunState_JOB_RUN_STATE_SUCCEEDED) {
			m.setToast("Retry is available for finished unsuccessful runs")
			return true, nil
		}
		action := "cancel"
		var token [16]byte
		if key == "x" {
			action = "retry"
			if _, err := rand.Read(token[:]); err != nil {
				m.jobsError = safeError(err)
				return true, nil
			}
		}
		m.jobsConfirm = &jobConfirmation{id: job.GetId(), action: action, key: hex.EncodeToString(token[:])}
	case "esc":
		m.setView(ViewApplications)
	default:
		return false, nil
	}
	return true, nil
}
func (m *Model) receiveJobMutation(msg jobMutationMsg) tea.Cmd {
	if m.jobsConfirm == nil || *m.jobsConfirm != msg.confirmation {
		return nil
	}
	m.jobsBusy = false
	if msg.err != nil {
		m.jobsError = safeError(msg.err)
		return nil
	}
	m.jobsConfirm = nil
	m.setToast("Job " + msg.confirmation.action + " accepted")
	if m.view == ViewJobs {
		return m.refreshJobs()
	}
	return nil
}
func (m *Model) renderJobs(t theme, width, height int) string {
	if m.jobsCron {
		return m.renderCronRuns(width, height)
	}
	lines := []string{"Jobs · " + m.jobsApp, "↑↓ select · enter logs · c cancel · x retry · n older · N latest · t cron runs"}
	if m.jobsError != "" {
		lines = append(lines, "Error: "+m.jobsError)
	}
	if m.jobsConfirm != nil {
		detail := "Cancellation completes when the agent confirms the process has stopped."
		if m.jobsConfirm.action == "retry" {
			detail = "Retry starts a new process and may repeat side effects."
		}
		lines = append(lines, "", strings.ToUpper(m.jobsConfirm.action)+" "+m.jobsConfirm.id+"?", detail, "y confirm · n/esc back")
		if m.jobsConfirm.action == "retry" {
			lines = append(lines, "Idempotency key: "+m.jobsConfirm.key)
		}
	} else {
		rows := max(1, height-9)
		start := max(0, m.jobsSelection-rows+1)
		for i := start; i < min(len(m.jobs), start+rows); i++ {
			job := m.jobs[i]
			mark := "  "
			if i == m.jobsSelection {
				mark = "> "
			}
			lines = append(lines, fmt.Sprintf("%s%s  %s/%s  %s", mark, job.GetId(), job.GetApplication(), job.GetService(), enumLabel(job.GetStatus().GetState().String())))
		}
		if len(m.jobs) == 0 {
			lines = append(lines, "No job runs yet. Start one with orch job run APP SERVICE -- COMMAND.")
		}
		if job := m.selectedJob(); job != nil {
			lines = append(lines, "", job.GetKind()+" · "+job.GetStatus().GetReason(), "Revision: "+job.GetRevisionId(), "Image: "+job.GetImage(), "Node: "+job.GetNodeId())
			if job.GetStatus().GetHasExitCode() {
				lines = append(lines, fmt.Sprintf("Exit code: %d", job.GetStatus().GetExitCode()))
			}
			if job.GetCancelRequested() && job.GetStatus().GetFinishedAt() == nil {
				lines = append(lines, "Cancellation requested; waiting for the owning agent")
			}
		}
	}
	for i := range lines {
		lines[i] = truncate(lines[i], max(1, width-2))
	}
	return strings.Join(lines[:min(len(lines), max(1, height))], "\n")
}

func (m *Model) renderCronRuns(width, height int) string {
	lines := []string{"Cron runs · " + m.jobsApp, "↑↓ select · enter logs · t one-off jobs · r refresh"}
	if m.jobsError != "" {
		lines = append(lines, "Error: "+m.jobsError)
	}
	rows := max(1, height-8)
	start := max(0, m.jobsSelection-rows+1)
	for i := start; i < min(len(m.cronRuns), start+rows); i++ {
		run := m.cronRuns[i]
		mark := "  "
		if i == m.jobsSelection {
			mark = "> "
		}
		lines = append(lines, fmt.Sprintf("%s%-20s %-10s %s", mark, truncate(run.GetJob(), 20), enumLabel(run.GetState().String()), relativeTime(run.GetScheduledAt().AsTime())))
	}
	if m.cronRuns != nil && len(m.cronRuns) == 0 {
		lines = append(lines, "No retained cron runs for this application.")
	}
	if run := m.selectedCronRun(); run != nil {
		lines = append(lines, "", "Run: "+run.GetId(), "Scheduled: "+run.GetScheduledAt().AsTime().Format(time.RFC3339))
		if run.GetHasExitCode() {
			lines = append(lines, fmt.Sprintf("Exit code: %d", run.GetExitCode()))
		}
		if reason := strings.TrimSpace(run.GetReason() + " " + run.GetMessage()); reason != "" {
			lines = append(lines, reason)
		}
	}
	for i := range lines {
		lines[i] = truncate(sanitize(lines[i]), max(1, width-2))
	}
	return strings.Join(lines[:min(len(lines), max(1, height))], "\n")
}
