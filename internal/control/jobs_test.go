package control

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/ingress"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/network"
	"github.com/TorstenDittmann/orch/internal/store"
)

func jobTestService(t *testing.T) (*service, context.Context, *time.Time) {
	t.Helper()
	return jobTestServiceWith(t, newTestManifest("hello", map[string]config.Service{"web": {Image: executableImage, Env: map[string]string{"MODE": "private-setting"}}}))
}

func jobTestServiceWith(t *testing.T, manifest *config.Manifest) (*service, context.Context, *time.Time) {
	t.Helper()
	state, cluster := reconcilerStore(t)
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	enrollCompatibleNode(t, state)
	if err := state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{NodeID: "node-one", BootID: "boot-one", OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.4", Snapshotter: "overlayfs", CgroupsV2: true, CNIVersion: network.CNISpecVersion, CNIPluginsVersion: network.CNIPluginsVersion, Nftables: true, DNSAvailable: true, CaddyVersion: ingress.RequiredCaddyVersion, TotalCPUMillis: 4000, TotalMemoryBytes: 8 << 30, AllocatableCPUMillis: 3500, AllocatableMemoryBytes: 7 << 30, SeenAt: reconcilerTestTime.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	now := reconcilerTestTime.Add(91 * time.Second)
	r := NewReconciler(state, cluster, func() time.Time { return now }, nil)
	applyManifest(t, state, manifest, "revision-first", "first-application-key", now)
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assignment, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	freezeReconcilerNetwork(t, state, assignment)
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assignment, err = state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	recordReadyAssignment(t, state, assignment, 2, now)
	s := newService(state, cluster, nil, nil, nil, nil, func() time.Time { return now })
	s.reconciler = r
	return s, ctx, &now
}
func reportJobResult(t *testing.T, s *service, ctx context.Context, id string, exit int32, at time.Time) {
	t.Helper()
	job, err := s.state.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := s.state.CurrentAssignment(ctx, job.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.state.AcknowledgeAssignment(ctx, job.NodeID, assignment.Generation, assignment.Digest); err != nil {
		t.Fatal(err)
	}
	node, err := s.state.NodeByID(ctx, job.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	run := job.Status
	run.State = model.JobRunFailed
	if exit == 0 {
		run.State = model.JobRunSucceeded
	}
	run.ExitCode = &exit
	run.StartedAt = &at
	run.FinishedAt = &at
	run.UpdatedAt = at
	run.Reason = "exit"
	var observations []model.CronJobStatus
	for _, template := range assignment.CronJobs {
		observations = append(observations, model.CronJobStatus{Application: template.Application, Job: template.Job, SpecDigest: template.SpecDigest, LastRunID: template.RunID, LastState: run.State, UpdatedAt: at})
	}
	_, err = s.state.RecordAgentReport(ctx, model.AgentReport{NodeID: job.NodeID, BootID: node.LastBootID, Sequence: node.LastReportSequence + 1, AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest, CronJobs: observations, JobRuns: []model.JobRunStatus{run}, Network: model.NetworkStatus{Generation: assignment.Network.Generation, State: model.NetworkReady, ClusterCIDR: assignment.Network.ClusterCIDR, NodeSubnet: assignment.Network.NodeSubnet, Gateway: assignment.Network.Gateway, DNSState: model.SubsystemReady, IngressState: model.SubsystemReady}, ReportedAt: at})
	if err != nil {
		t.Fatal(err)
	}
}
func TestOneOffAPIReplayCancelAndRetry(t *testing.T) {
	s, ctx, now := jobTestService(t)
	req := connect.NewRequest(&orchv1alpha1.RunJobRequest{Application: "hello", Service: "web", Command: []string{"/bin/task"}, Args: []string{"private-arg"}, IdempotencyKey: "job-request-first"})
	first, err := s.RunJob(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.RunJob(ctx, req)
	if err != nil || first.Msg.Job.Id != replay.Msg.Job.Id {
		t.Fatalf("replay: %v", err)
	}
	job, err := s.state.Job(ctx, first.Msg.Job.Id)
	if err != nil || job.Template.Image != executableImage || job.Template.Environment["MODE"] != "private-setting" {
		t.Fatalf("template: %+v %v", job, err)
	}
	data, _ := json.Marshal(first.Msg)
	if strings.Contains(string(data), "private-") {
		t.Fatalf("job metadata leaked command/environment: %s", data)
	}
	if _, err := s.RetryJob(ctx, connect.NewRequest(&orchv1alpha1.RetryJobRequest{Id: job.ID, IdempotencyKey: "job-retry-first-key"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("active retry: %v", err)
	}
	cancel, err := s.CancelJob(ctx, connect.NewRequest(&orchv1alpha1.CancelJobRequest{Id: job.ID}))
	if err != nil || !cancel.Msg.Job.CancelRequested || cancel.Msg.Job.Status.FinishedAt != nil {
		t.Fatalf("cancel must remain requested: %v", err)
	}
	*now = now.Add(time.Second)
	reportJobResult(t, s, ctx, job.ID, 42, *now)
	retry, err := s.RetryJob(ctx, connect.NewRequest(&orchv1alpha1.RetryJobRequest{Id: job.ID, IdempotencyKey: "job-retry-first-key"}))
	if err != nil || retry.Msg.Job.Id == job.ID || retry.Msg.Job.RetryOf != job.ID {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	retried, _ := s.state.Job(ctx, retry.Msg.Job.Id)
	if retried.Template.Image != job.Template.Image || retried.Template.Args[0] != job.Template.Args[0] {
		t.Fatal("retry changed immutable intent")
	}
	if _, err := s.RunJob(context.Background(), req); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unauthenticated run: %v", err)
	}
	conflict := connect.NewRequest(&orchv1alpha1.RunJobRequest{Application: "hello", Service: "web", Command: []string{"/other"}, IdempotencyKey: "job-request-first"})
	if _, err := s.RunJob(ctx, conflict); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("conflict: %v", err)
	}
	stale := connect.NewRequest(&orchv1alpha1.RunJobRequest{Application: "hello", Service: "web", Command: []string{"/bin/task"}, ExpectedRevisionId: "revision-old", IdempotencyKey: "job-request-stale"})
	if _, err := s.RunJob(ctx, stale); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("stale: %v", err)
	}
}
func TestPreDeployHookPreservesServicesUntilSuccess(t *testing.T) {
	s, ctx, now := jobTestService(t)
	previous, err := s.state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	manifest := newTestManifest("hello", map[string]config.Service{"web": {Image: "docker.io/library/busybox@sha256:" + strings.Repeat("b", 64)}})
	manifest.Deploy.PreDeploy = &config.PreDeployHook{Service: "web", Command: []string{"/bin/migrate"}, Timeout: "1m"}
	applyManifest(t, s.state, manifest, "revision-second", "second-application-key", *now)
	if err := s.reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	hook, err := s.state.HookJob(ctx, "hello", "revision-second")
	if err != nil {
		t.Fatal(err)
	}
	held, err := s.state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Allocations) != len(previous.Allocations) || held.Allocations[0].RevisionID != "revision-first" || len(held.CronJobs) != 1 {
		t.Fatalf("hook did not preserve old assignment: %+v", held)
	}
	if hook.Template.Image != manifest.Services["web"].Image {
		t.Fatal("hook used old image")
	}
	for range 3 {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	history, _ := s.state.Jobs(ctx, "hello", "", 100, false)
	if len(history) != 1 {
		t.Fatal("hook repeated during reconcile")
	}
	*now = now.Add(time.Second)
	reportJobResult(t, s, ctx, hook.ID, 1, *now)
	if err := s.reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	failed, _ := s.state.Application(ctx, "hello")
	held, _ = s.state.CurrentAssignment(ctx, "node-one")
	if failed.Execution.Status != model.ExecutionFailed || held.Allocations[0].RevisionID != "revision-first" {
		t.Fatalf("failed hook released deployment: %+v", failed.Execution)
	}
	retry, err := s.RetryJob(ctx, connect.NewRequest(&orchv1alpha1.RetryJobRequest{Id: hook.ID, IdempotencyKey: "retry-hook-request"}))
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	reportJobResult(t, s, ctx, retry.Msg.Job.Id, 0, *now)
	if err := s.reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	released, _ := s.state.CurrentAssignment(ctx, "node-one")
	if len(released.Allocations) != 1 || released.Allocations[0].RevisionID != "revision-second" || len(released.CronJobs) != 0 {
		t.Fatalf("successful hook did not release deployment: %+v", released)
	}
	node, _ := s.state.NodeByID(ctx, "node-one")
	recordReadyAssignment(t, s.state, released, node.LastReportSequence+1, *now)
	current, _ := s.state.Application(ctx, "hello")
	if current.Execution.Status != model.ExecutionRunning || current.Execution.ActiveRevisionID != "revision-second" {
		t.Fatalf("hook gate did not clear readiness: %+v", current.Execution)
	}
	if _, err := s.state.Job(ctx, hook.ID); err == store.ErrJobNotFound {
		t.Fatal("failed hook history vanished")
	}
}

func TestSupersededHookWaitsForCancellation(t *testing.T) {
	s, ctx, now := jobTestService(t)
	manifest := newTestManifest("hello", map[string]config.Service{"web": {Image: executableImage}})
	manifest.Deploy.PreDeploy = &config.PreDeployHook{Service: "web", Command: []string{"/bin/migrate"}, Timeout: "1m"}
	applyManifest(t, s.state, manifest, "revision-second", "second-application-key", *now)
	if err := s.reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	old, err := s.state.HookJob(ctx, "hello", "revision-second")
	if err != nil {
		t.Fatal(err)
	}
	manifest.Deploy.PreDeploy.Args = []string{"next"}
	applyManifest(t, s.state, manifest, "revision-third", "third-application-key", *now)
	if err := s.reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.state.Job(ctx, old.ID)
	if err != nil || !cancelled.CancelRequested || cancelled.Status.FinishedAt != nil {
		t.Fatalf("cancellation must await owner: %+v %v", cancelled, err)
	}
	if _, err := s.state.HookJob(ctx, "hello", "revision-third"); err != store.ErrJobNotFound {
		t.Fatalf("replacement overlapped old hook: %v", err)
	}
	assignment, err := s.state.CurrentAssignment(ctx, old.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(assignment.CronJobs) != 1 || assignment.CronJobs[0].RunID != old.ID || !assignment.CronJobs[0].CancelRequested {
		t.Fatalf("owner did not receive cancellation: %+v", assignment.CronJobs)
	}
	*now = now.Add(time.Second)
	reportJobResult(t, s, ctx, old.ID, 1, *now)
	if err := s.reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := s.state.HookJob(ctx, "hello", "revision-third")
	if err != nil || next.ID == old.ID {
		t.Fatalf("replacement did not start after acknowledgement: %+v %v", next, err)
	}
}

func TestRunCronJobStartsTheScheduledJobNowOnItsNode(t *testing.T) {
	manifest := newTestManifest("hello", map[string]config.Service{"web": {Image: executableImage}})
	manifest.CronJobs = map[string]config.CronJob{"nightly": {
		Image: executableImage, Schedule: "0 3 * * *", Command: []string{"/bin/backup"},
		Resources: config.Resources{CPU: "100m", Memory: "64Mi"}, StartDeadline: "1m", Timeout: "30m",
	}}
	s, ctx, now := jobTestServiceWith(t, manifest)
	request := connect.NewRequest(&orchv1alpha1.RunCronJobRequest{Application: "hello", Job: "nightly", IdempotencyKey: "cron-run-first-key"})
	response, err := s.RunCronJob(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.state.Job(ctx, response.Msg.Job.Id)
	if err != nil {
		t.Fatal(err)
	}
	if job.NodeID != "node-one" || job.Service != "cron/nightly" || job.Template.Schedule != "" ||
		len(job.Template.Command) != 1 || job.Template.Command[0] != "/bin/backup" || job.Template.TimeoutSeconds != 1800 {
		t.Fatalf("job = %+v", job)
	}
	if replay, err := s.RunCronJob(ctx, request); err != nil || replay.Msg.Job.Id != job.ID {
		t.Fatalf("replay = %v, %v", replay, err)
	}
	assignment, err := s.state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range assignment.CronJobs {
		found = found || item.RunID == job.ID
	}
	if !found {
		t.Fatalf("run not assigned: %+v", assignment.CronJobs)
	}
	*now = now.Add(time.Second)
	reportJobResult(t, s, ctx, job.ID, 1, *now)
	retry, err := s.RetryJob(ctx, connect.NewRequest(&orchv1alpha1.RetryJobRequest{Id: job.ID, IdempotencyKey: "cron-run-retry-key"}))
	if err != nil || retry.Msg.Job.RetryOf != job.ID || retry.Msg.Job.Service != "cron/nightly" || retry.Msg.Job.NodeId != "node-one" {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	if _, err := s.RunCronJob(ctx, connect.NewRequest(&orchv1alpha1.RunCronJobRequest{Application: "hello", Job: "missing", IdempotencyKey: "cron-run-missing-key"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("missing job = %v", err)
	}
}

func TestActiveJobRunsDropsRemovedCronJobs(t *testing.T) {
	application := model.Application{CanonicalManifest: json.RawMessage(`{"cron_jobs":{"nightly":{}}}`)}
	application.Execution.JobRuns = []model.JobRunStatus{
		{ID: "run_a", Job: "nightly", State: model.JobRunFailed},
		{ID: "run_b", Job: "removed", State: model.JobRunFailed},
		{ID: "job_c", Job: "run-c", State: model.JobRunFailed},
	}
	var kept []string
	for _, run := range activeJobRuns(application) {
		kept = append(kept, run.ID)
	}
	if strings.Join(kept, ",") != "run_a,job_c" {
		t.Fatalf("kept runs = %v, want the declared cron job and the one-off run", kept)
	}
}
