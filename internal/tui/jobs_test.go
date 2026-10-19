package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeJobProvider struct {
	*fakeProvider
	entries                 []*orchv1alpha1.OneOffJob
	cancelled, retried, key string
}

func (p *fakeJobProvider) ListJobs(context.Context, string, string, uint32) (*orchv1alpha1.ListJobsResponse, error) {
	return &orchv1alpha1.ListJobsResponse{Jobs: p.entries}, nil
}
func (p *fakeJobProvider) CancelJob(_ context.Context, id string) (*orchv1alpha1.OneOffJob, error) {
	p.cancelled = id
	return p.entries[0], nil
}
func (p *fakeJobProvider) RetryJob(_ context.Context, id, key string) (*orchv1alpha1.OneOffJob, error) {
	p.retried, p.key = id, key
	return p.entries[0], nil
}
func TestJobWorkspaceGuardsActionsAndIgnoresStaleReads(t *testing.T) {
	p := &fakeJobProvider{fakeProvider: &fakeProvider{}, entries: []*orchv1alpha1.OneOffJob{{Id: "job_one", Application: "hello", Service: "web", Status: &orchv1alpha1.JobRunStatus{Job: "run-one", State: orchv1alpha1.JobRunState_JOB_RUN_STATE_RUNNING}}}}
	m := New(context.Background(), p, "lab", "direct", time.Minute)
	command := m.openJobs("hello")
	m.receiveJobs(command().(jobsMsg))
	stale := jobsMsg{generation: m.jobsGeneration - 1, response: &orchv1alpha1.ListJobsResponse{}}
	m.receiveJobs(stale)
	if m.selectedJob().GetId() != "job_one" {
		t.Fatal("stale response changed selection")
	}
	m.handleJobKey("x")
	if m.jobsConfirm != nil {
		t.Fatal("active run can be retried")
	}
	m.handleJobKey("c")
	if m.jobsConfirm == nil || p.cancelled != "" {
		t.Fatal("cancel was not guarded")
	}
	_, cmd := m.handleJobKey("y")
	msg := cmd().(jobMutationMsg)
	m.receiveJobMutation(msg)
	if p.cancelled != "job_one" {
		t.Fatal("cancel targeted wrong run")
	}
	p.entries[0].Status.State = orchv1alpha1.JobRunState_JOB_RUN_STATE_FAILED
	p.entries[0].Status.FinishedAt = timestamppb.Now()
	m.handleJobKey("x")
	_, cmd = m.handleJobKey("y")
	m.receiveJobMutation(cmd().(jobMutationMsg))
	if p.retried != "job_one" || len(p.key) != 32 {
		t.Fatal("retry identity/key lost")
	}
	for _, size := range [][2]int{{30, 10}, {80, 24}, {140, 40}} {
		m.width, m.height = size[0], size[1]
		if m.View().Content == "" {
			t.Fatal("empty jobs view")
		}
	}
}

type fakeCronProvider struct {
	*fakeJobProvider
	runs map[string][]*orchv1alpha1.JobRunStatus
}

func (p *fakeCronProvider) GetApplication(context.Context, string) (*orchv1alpha1.Application, error) {
	return &orchv1alpha1.Application{Name: "hello", CronJobs: []*orchv1alpha1.CronJobStatus{{Job: "backup"}, {Job: "report"}}}, nil
}
func (p *fakeCronProvider) ListCronRuns(_ context.Context, _, job string, _ uint32) ([]*orchv1alpha1.JobRunStatus, error) {
	return p.runs[job], nil
}

func TestJobWorkspaceShowsCronRunsNewestFirst(t *testing.T) {
	now := time.Now()
	p := &fakeCronProvider{fakeJobProvider: &fakeJobProvider{fakeProvider: &fakeProvider{}}, runs: map[string][]*orchv1alpha1.JobRunStatus{
		"backup": {{Id: "run_old", Application: "hello", Job: "backup", ScheduledAt: timestamppb.New(now.Add(-time.Hour))}},
		"report": {{Id: "run_new", Application: "hello", Job: "report", ScheduledAt: timestamppb.New(now)}},
	}}
	m := New(context.Background(), p, "lab", "direct", time.Minute)
	m.receiveJobs(m.openJobs("hello")().(jobsMsg))
	_, command := m.handleJobKey("t")
	m.receiveJobs(command().(jobsMsg))
	if len(m.cronRuns) != 2 || m.selectedCronRun().GetId() != "run_new" || m.selectedJob() != nil {
		t.Fatalf("cron runs = %v", m.cronRuns)
	}
	m.snapshot.Cluster, m.width, m.height = &orchv1alpha1.Cluster{Name: "lab"}, 100, 30
	if content := m.View().Content; !strings.Contains(content, "Cron runs · hello") || !strings.Contains(content, "report") {
		t.Fatalf("view = %s", content)
	}
	m.handleJobKey("c")
	if m.jobsConfirm != nil {
		t.Fatal("cron runs can be cancelled from the jobs view")
	}
	m.handleJobKey("j")
	if _, command = m.handleJobKey("enter"); command == nil || m.selectedCronRun().GetId() != "run_old" {
		t.Fatal("enter did not open the selected run's logs")
	}
}
