package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

func TestDeploymentHistoryOutcomesRollbackAndReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	first := applicationInput(`{"name":"hello","services":{"web":{"replicas":1,"image":"first"}}}`, "revision-one", "idempotency-key-one", testCreatedAt)
	if _, err := state.ApplyApplication(ctx, first); err != nil {
		t.Fatal(err)
	}
	execution := model.ApplicationExecution{Status: model.ExecutionHealthy, ObservedGeneration: 1, ActiveGeneration: 1, ActiveRevisionID: first.RevisionID, Reason: "ready", UpdatedAt: testCreatedAt.Add(time.Second)}
	if err := state.UpdateApplicationExecutions(ctx, map[string]model.ApplicationExecution{"hello": execution}); err != nil {
		t.Fatal(err)
	}
	second := applicationInput(`{"name":"hello","services":{"web":{"replicas":1,"image":"second"}}}`, "revision-two", "idempotency-key-two", testCreatedAt.Add(time.Minute))
	if _, err := state.ApplyApplication(ctx, second); err != nil {
		t.Fatal(err)
	}
	// Reports for the old generation must never complete the new deployment.
	execution.UpdatedAt = testCreatedAt.Add(2 * time.Minute)
	if err := state.UpdateApplicationExecutions(ctx, map[string]model.ApplicationExecution{"hello": execution}); err != nil {
		t.Fatal(err)
	}
	d, err := state.Deployment(ctx, "hello", "revision-two")
	if err != nil || d.Outcome != "IN_PROGRESS" {
		t.Fatalf("stale observation: %+v %v", d, err)
	}
	execution.Status = model.ExecutionRolledBack
	execution.ObservedGeneration = 2
	execution.Reason = "health_gate_rollback"
	if err := state.UpdateApplicationExecutions(ctx, map[string]model.ApplicationExecution{"hello": execution}); err != nil {
		t.Fatal(err)
	}
	rollback := first
	rollback.RevisionID = "revision-three"
	rollback.IdempotencyKey = "idempotency-key-rollback"
	rollback.ExpectedGeneration = 2
	rollback.RollbackFromRevisionID = first.RevisionID
	rollback.CreatedAt = testCreatedAt.Add(3 * time.Minute)
	result, err := state.ApplyApplication(ctx, rollback)
	if err != nil || result.Generation != 3 || !result.Changed {
		t.Fatalf("rollback: %+v %v", result, err)
	}
	if _, err := state.ApplyApplication(ctx, second); err != nil {
		t.Fatal(err)
	} // exact old acceptance
	third := second
	third.RevisionID = "revision-four"
	third.IdempotencyKey = "idempotency-key-four"
	third.CreatedAt = testCreatedAt.Add(4 * time.Minute)
	if _, err := state.ApplyApplication(ctx, third); err != nil {
		t.Fatal(err)
	}
	replay, err := state.ApplyApplication(ctx, rollback)
	if err != nil || !reflect.DeepEqual(result, replay) {
		t.Fatalf("replay after newer deployment: %+v %v", replay, err)
	}
	history, next, err := state.Deployments(ctx, "hello", 0, 2)
	if err != nil || len(history) != 2 || next != 3 || history[0].Generation != 4 || history[1].Outcome != "SUPERSEDED" || history[1].RollbackFromRevisionID != first.RevisionID {
		t.Fatalf("page one: %+v %d %v", history, next, err)
	}
	history, next, err = state.Deployments(ctx, "hello", next, 2)
	if err != nil || len(history) != 2 || next != 0 || history[0].Outcome != "ROLLED_BACK" || history[1].Outcome != "SUCCEEDED" || history[1].FinishedAt == nil {
		t.Fatalf("page two: %+v %d %v", history, next, err)
	}
	if string(history[1].CanonicalManifest) != string(first.CanonicalManifest) {
		t.Fatal("rollback rewrote source")
	}
	if _, err := state.RemoveApplication(ctx, "hello", "idempotency-remove", testCreatedAt.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	replay, err = state.ApplyApplication(ctx, rollback)
	if err != nil || !reflect.DeepEqual(result, replay) {
		t.Fatalf("replay after removal: %+v %v", replay, err)
	}
	history, _, err = state.Deployments(ctx, "hello", 0, 1)
	if err != nil || history[0].Outcome != "REMOVED" || history[0].Current {
		t.Fatalf("removed: %+v %v", history, err)
	}
}

func TestRollbackRejectsStaleCrossApplicationAndCapacityWithoutMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	first := applicationInput(`{"name":"hello","services":{"web":{"replicas":150}}}`, "revision-one", "idempotency-key-one", testCreatedAt)
	if _, err := state.ApplyApplication(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := applicationInput(`{"name":"hello","services":{"web":{"replicas":1}}}`, "revision-two", "idempotency-key-two", testCreatedAt.Add(time.Minute))
	if _, err := state.ApplyApplication(ctx, second); err != nil {
		t.Fatal(err)
	}
	other := applicationInput(`{"name":"other","services":{"web":{"replicas":100}}}`, "other-revision", "idempotency-key-other", testCreatedAt)
	other.Name = "other"
	if _, err := state.ApplyApplication(ctx, other); err != nil {
		t.Fatal(err)
	}
	base := first
	base.RollbackFromRevisionID = first.RevisionID
	base.ExpectedGeneration = 2
	base.RevisionID = "rollback-revision"
	base.IdempotencyKey = "idempotency-rollback"
	for _, tc := range []struct {
		name   string
		mutate func(*model.ApplyApplication)
		want   error
	}{
		{"capacity", func(*model.ApplyApplication) {}, ErrAllocationLimit},
		{"stale", func(i *model.ApplyApplication) { i.ExpectedGeneration = 1 }, ErrGenerationConflict},
		{"foreign", func(i *model.ApplyApplication) { i.RollbackFromRevisionID = other.RevisionID }, ErrRevisionNotFound},
		{"current", func(i *model.ApplyApplication) { i.RollbackFromRevisionID = second.RevisionID }, ErrRollbackSource},
		{"conflicting key", func(i *model.ApplyApplication) { i.IdempotencyKey = second.IdempotencyKey }, ErrIdempotencyConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			tc.mutate(&input)
			if _, err := state.ApplyApplication(ctx, input); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
	history, _, err := state.Deployments(ctx, "hello", 0, 50)
	if err != nil || len(history) != 2 || history[0].Generation != 2 {
		t.Fatalf("failed rollback modified history: %+v %v", history, err)
	}
	if _, err := state.RemoveApplication(ctx, "hello", "idempotency-remove", testCreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ApplyApplication(ctx, base); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("rollback reactivated removed app: %v", err)
	}
}

func TestHistoryCursorRemainsStableAndNoopCreatesNoDeployment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	var input model.ApplyApplication
	for i := 1; i <= 4; i++ {
		input = applicationInput(fmt.Sprintf(`{"name":"hello","version":%d}`, i), fmt.Sprintf("revision-%d", i), fmt.Sprintf("idempotency-key-%d", i), testCreatedAt.Add(time.Duration(i)*time.Second))
		if _, err := state.ApplyApplication(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	_, cursor, err := state.Deployments(ctx, "hello", 0, 2)
	if err != nil || cursor != 3 {
		t.Fatalf("cursor %d %v", cursor, err)
	}
	input.RevisionID = "noop"
	input.IdempotencyKey = "idempotency-noop"
	if r, err := state.ApplyApplication(ctx, input); err != nil || r.Changed {
		t.Fatalf("noop %+v %v", r, err)
	}
	input = applicationInput(`{"name":"hello","version":5}`, "revision-5", "idempotency-key-5", testCreatedAt.Add(5*time.Second))
	if _, err := state.ApplyApplication(ctx, input); err != nil {
		t.Fatal(err)
	}
	page, _, err := state.Deployments(ctx, "hello", cursor, 2)
	if err != nil || len(page) != 2 || page[0].Generation != 2 || page[1].Generation != 1 {
		t.Fatalf("cursor changed: %+v %v", page, err)
	}
	if _, _, err := state.Deployments(ctx, "missing", 0, 20); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, _, err := state.Deployments(ctx, "hello", 0, 51); err == nil {
		t.Fatal("unbounded history allowed")
	}
}

func TestCronOnlyDeploymentWaitsForAcknowledgedObservedSchedule(t *testing.T) {
	ctx := context.Background()
	state := executionStore(t)
	input := applicationInput(`{"name":"hello","services":{},"cron_jobs":{"backup":{"image":"image"}}}`, "revision-cron", "idempotency-key-cron", testCreatedAt.Add(time.Minute))
	if _, err := state.ApplyApplication(ctx, input); err != nil {
		t.Fatal(err)
	}
	job := model.CronJobDesired{Application: "hello", Job: "backup", ApplicationGeneration: 2, RevisionID: input.RevisionID, Image: storeTestAllocation(t).Image, Schedule: "*/2 * * * *", CPUMillis: 100, MemoryBytes: 64 << 20, StartDeadlineSeconds: 30, TimeoutSeconds: 300, NetworkGeneration: 1}
	job.SpecDigest, _ = model.CronJobSpecDigest(job)
	network, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	payload, digest, err := model.AssignmentPayloadWithJobs(network, nil, []model.CronJobDesired{job})
	if err != nil {
		t.Fatal(err)
	}
	assignment, _, err := state.PublishAssignment(ctx, model.PublishAssignment{NodeID: "node-one", Digest: digest, Payload: payload, Network: network, CronJobs: []model.CronJobDesired{job}, CreatedAt: testCreatedAt.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{NodeID: "node-one", BootID: "boot-one", OperatingSystem: "linux", Architecture: "amd64", SeenAt: testCreatedAt.Add(3 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", assignment.Generation, assignment.Digest); err != nil {
		t.Fatal(err)
	}
	report := model.AgentReport{NodeID: "node-one", BootID: "boot-one", Sequence: 1, AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest, ReportedAt: testCreatedAt.Add(4 * time.Minute), Network: model.NetworkStatus{Generation: 1, State: model.NetworkReady, ClusterCIDR: network.ClusterCIDR, NodeSubnet: network.NodeSubnet, Gateway: network.Gateway, DNSState: model.SubsystemReady, IngressState: model.SubsystemReady}}
	if _, err := state.RecordAgentReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	d, err := state.Deployment(ctx, "hello", input.RevisionID)
	if err != nil || d.Outcome != "IN_PROGRESS" {
		t.Fatalf("unobserved cron: %+v %v", d, err)
	}
	report.Sequence++
	report.ReportedAt = report.ReportedAt.Add(time.Second)
	report.CronJobs = []model.CronJobStatus{{Application: "hello", Job: "backup", SpecDigest: job.SpecDigest, UpdatedAt: report.ReportedAt}}
	if _, err := state.RecordAgentReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	app, err := state.Application(ctx, "hello")
	if err != nil || app.Execution.Status != model.ExecutionRunning || app.Execution.ActiveGeneration != 2 || app.Execution.ActiveRevisionID != input.RevisionID {
		t.Fatalf("cron ready: %+v %v", app.Execution, err)
	}
	d, err = state.Deployment(ctx, "hello", input.RevisionID)
	if err != nil || d.Outcome != "SUCCEEDED" {
		t.Fatalf("cron outcome: %+v %v", d, err)
	}
}

func TestDeploymentMigrationPreservesOnlyKnownOutcomes(t *testing.T) {
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	first := applicationInput(`{"name":"hello","version":1}`, "revision-one", "idempotency-key-one", testCreatedAt)
	second := applicationInput(`{"name":"hello","version":2}`, "revision-two", "idempotency-key-two", testCreatedAt.Add(time.Minute))
	for _, input := range []model.ApplyApplication{first, second} {
		if _, err := state.ApplyApplication(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	execution := model.ApplicationExecution{Status: model.ExecutionHealthy, ObservedGeneration: 2, ActiveGeneration: 2, ActiveRevisionID: "revision-two", Reason: "ready", UpdatedAt: testCreatedAt.Add(2 * time.Minute)}
	if err := state.UpdateApplicationExecutions(ctx, map[string]model.ApplicationExecution{"hello": execution}); err != nil {
		t.Fatal(err)
	}
	// Recreate a version-eight fixture, which has revisions but no history table.
	if _, err := state.db.ExecContext(ctx, `DROP TABLE deployment_records`); err != nil {
		t.Fatal(err)
	}
	if _, err := state.db.ExecContext(ctx, `DROP TABLE one_off_jobs; ALTER TABLE application_revisions DROP COLUMN requested_manifest; DELETE FROM schema_migrations WHERE version >= 9`); err != nil {
		t.Fatal(err)
	}
	path := state.path
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	history, _, err := reopened.Deployments(ctx, "hello", 0, 20)
	if err != nil || len(history) != 2 || history[0].Outcome != "SUCCEEDED" || history[1].Outcome != "UNKNOWN" || history[1].FinishedAt != nil {
		t.Fatalf("migration invented history: %+v %v", history, err)
	}
	if _, err := reopened.db.ExecContext(ctx, `UPDATE application_revisions SET digest=? WHERE id=?`, second.Digest, first.RevisionID); err == nil {
		t.Fatal("migration weakened revision immutability")
	}
}

func TestConcurrentRollbackRequestsOnlyAdvanceOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	first := applicationInput(`{"name":"hello","version":1}`, "revision-one", "idempotency-key-one", testCreatedAt)
	second := applicationInput(`{"name":"hello","version":2}`, "revision-two", "idempotency-key-two", testCreatedAt.Add(time.Minute))
	for _, input := range []model.ApplyApplication{first, second} {
		if _, err := state.ApplyApplication(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		input := first
		input.ExpectedGeneration = 2
		input.RollbackFromRevisionID = first.RevisionID
		input.RevisionID = fmt.Sprintf("rollback-%d", i)
		input.IdempotencyKey = fmt.Sprintf("idempotency-rollback-%d", i)
		go func() { <-start; _, err := state.ApplyApplication(ctx, input); results <- err }()
	}
	close(start)
	success, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrGenerationConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
	history, _, err := state.Deployments(ctx, "hello", 0, 20)
	if err != nil || len(history) != 3 || history[0].Generation != 3 {
		t.Fatalf("concurrent history: %+v %v", history, err)
	}
}
