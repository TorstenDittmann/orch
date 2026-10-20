package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
)

func TestAgentReportRaisesFailureEventsOnce(t *testing.T) {
	ctx := context.Background()
	state := executionStore(t)
	cursor, err := state.NotificationCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	allocation := storeTestAllocation(t)
	allocation.NetworkGeneration = 1
	allocation.Mounts = []model.VolumeMount{{Name: "data", Target: "/data"}}
	allocation.VolumeBackups = []model.VolumeBackupDesired{{Volume: "data", Schedule: "0 3 * * *", Keep: 7}}
	allocation.SpecDigest, _ = model.AllocationSpecDigest(allocation)
	job := model.CronJobDesired{Application: "hello", Job: "report", ApplicationGeneration: 1,
		RevisionID: "revision-one", Image: allocation.Image, Schedule: "*/2 * * * *", CPUMillis: 100,
		MemoryBytes: 64 << 20, StartDeadlineSeconds: 30, TimeoutSeconds: 300, NetworkGeneration: 1}
	job.SpecDigest, _ = model.CronJobSpecDigest(job)
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	payload, digest, err := model.AssignmentPayloadWithJobs(networkConfig, []model.AllocationDesired{allocation}, []model.CronJobDesired{job})
	if err != nil {
		t.Fatal(err)
	}
	assignment, _, err := state.PublishAssignment(ctx, model.PublishAssignment{NodeID: "node-one", Digest: digest,
		Payload: payload, Network: networkConfig, Allocations: []model.AllocationDesired{allocation}, CronJobs: []model.CronJobDesired{job}, CreatedAt: testCreatedAt.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{NodeID: "node-one", BootID: "boot-one", OperatingSystem: "linux", Architecture: "amd64", SeenAt: testCreatedAt.Add(3 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", assignment.Generation, assignment.Digest); err != nil {
		t.Fatal(err)
	}
	firstFailure := testCreatedAt.Add(3 * time.Minute)
	exitCode := int32(2)
	report := func(sequence uint64, at, failedAt time.Time) {
		t.Helper()
		accepted, err := state.RecordAgentReport(ctx, model.AgentReport{NodeID: "node-one", BootID: "boot-one", Sequence: sequence,
			AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest, ReportedAt: at,
			Allocations: []model.AllocationStatus{{ID: allocation.ID, Application: allocation.Application, Service: allocation.Service,
				Replica: 1, ApplicationGeneration: 1, SpecDigest: allocation.SpecDigest, Phase: model.AllocationRunning,
				Address: "10.88.0.2", ActiveSpecDigest: allocation.SpecDigest, ActiveGeneration: 1, ActiveRevisionID: allocation.RevisionID, UpdatedAt: at}},
			CronJobs: []model.CronJobStatus{{Application: "hello", Job: "report", SpecDigest: job.SpecDigest,
				LastRunID: "run-one", LastState: model.JobRunFailed, UpdatedAt: at}},
			JobRuns: []model.JobRunStatus{{ID: "run-one", Application: "hello", Job: "report", SpecDigest: job.SpecDigest,
				ScheduledAt: firstFailure, State: model.JobRunFailed, ExitCode: &exitCode, Message: "exit status 2",
				StartedAt: &firstFailure, FinishedAt: &firstFailure, UpdatedAt: firstFailure}},
			VolumeBackupFailures: []model.VolumeBackupFailure{{Application: "hello", Volume: "data", FailedAt: failedAt, Message: "disk full"}},
			Network: model.NetworkStatus{Generation: 1, State: model.NetworkReady, ClusterCIDR: model.DefaultWorkloadCIDR,
				NodeSubnet: "10.88.0.0/24", Gateway: "10.88.0.1", DNSState: model.SubsystemReady, IngressState: model.SubsystemReady}})
		if err != nil || !accepted {
			t.Fatalf("RecordAgentReport() = %t, %v", accepted, err)
		}
	}
	report(1, testCreatedAt.Add(4*time.Minute), firstFailure)
	report(2, testCreatedAt.Add(5*time.Minute), firstFailure)
	report(3, testCreatedAt.Add(6*time.Minute), firstFailure.Add(time.Hour))

	events, err := state.EventsAfter(ctx, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Kind]++
		if event.Kind == model.EventCronRunFailed {
			var failed model.CronRunFailed
			if err := json.Unmarshal(event.Payload, &failed); err != nil || failed.Job != "report" || failed.ExitCode == nil || *failed.ExitCode != 2 {
				t.Fatalf("cron failure = %s", event.Payload)
			}
		}
		if event.Kind == model.EventVolumeBackupFailed {
			var failed model.VolumeBackupFailed
			if err := json.Unmarshal(event.Payload, &failed); err != nil || failed.Volume != "data" || failed.Message != "disk full" || failed.NodeID != "node-one" {
				t.Fatalf("backup failure = %s", event.Payload)
			}
		}
	}
	if counts[model.EventCronRunFailed] != 1 || counts[model.EventVolumeBackupFailed] != 2 {
		t.Fatalf("event counts = %v", counts)
	}
}

func TestAgentReportRejectsBackupFailuresOfOtherVolumes(t *testing.T) {
	ctx := context.Background()
	state := executionStore(t)
	allocation := storeTestAllocation(t)
	assignment, _, err := state.PublishAssignment(ctx, storeTestPublish(t, allocation))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{NodeID: "node-one", BootID: "boot-one", OperatingSystem: "linux", Architecture: "amd64", SeenAt: testCreatedAt.Add(3 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", assignment.Generation, assignment.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordAgentReport(ctx, model.AgentReport{NodeID: "node-one", BootID: "boot-one", Sequence: 1,
		AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest, ReportedAt: testCreatedAt.Add(4 * time.Minute),
		VolumeBackupFailures: []model.VolumeBackupFailure{{Application: "other", Volume: "data", FailedAt: testCreatedAt, Message: "x"}}}); err == nil {
		t.Fatal("a backup failure of a volume the node does not back up was accepted")
	}
}

func TestDeploymentFinishedEvent(t *testing.T) {
	ctx := context.Background()
	state := executionStore(t)
	cursor, err := state.NotificationCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	application, err := state.Application(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	execution := model.ApplicationExecution{Status: model.ExecutionHealthy, ObservedGeneration: application.Generation,
		ActiveGeneration: application.Generation, ActiveRevisionID: application.RevisionID, Reason: "ready", Message: "all ready",
		UpdatedAt: testCreatedAt.Add(2 * time.Minute)}
	for range 2 {
		if err := state.UpdateApplicationExecutions(ctx, map[string]model.ApplicationExecution{"hello": execution}); err != nil {
			t.Fatal(err)
		}
		execution.UpdatedAt = execution.UpdatedAt.Add(time.Minute)
	}
	events, err := state.EventsAfter(ctx, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	var finished []model.DeploymentFinished
	for _, event := range events {
		if event.Kind == model.EventDeploymentFinished {
			var payload model.DeploymentFinished
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			finished = append(finished, payload)
		}
	}
	if len(finished) != 1 || finished[0].Application != "hello" || finished[0].Outcome != "SUCCEEDED" || finished[0].RevisionID != application.RevisionID {
		t.Fatalf("deployment finished events = %+v", finished)
	}
}

func TestNotificationSecretIsReferenced(t *testing.T) {
	manifest := config.Manifest{Notifications: []config.Notification{{Kind: config.NotificationSlack, URLSecret: "slack-url", On: config.DefaultNotificationEvents}}}
	if !manifestReferencesSecret(manifest, "slack-url") {
		t.Fatal("a notification's URL secret is not referenced")
	}
	if manifestReferencesSecret(manifest, "other") {
		t.Fatal("an unused secret is referenced")
	}
}
