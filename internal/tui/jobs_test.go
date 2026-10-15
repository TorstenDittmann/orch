package tui

import (
	"context"
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
