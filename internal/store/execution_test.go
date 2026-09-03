package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

func TestAgentEnrollmentIsMultiNodeAndExactlyReplayable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	input := testEnrollAgent("node-one", "node-1", strings.Repeat("a", 64), "enrollment-key-one", strings.Repeat("1", 64))
	first, err := state.EnrollAgent(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || first.EventSequence != 2 || first.NodeID != input.NodeID {
		t.Fatalf("first enrollment = %#v", first)
	}
	replay, err := state.EnrollAgent(ctx, input)
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("enrollment replay = %#v, %v; want %#v", replay, err, first)
	}
	conflict := input
	conflict.RequestHash = strings.Repeat("2", 64)
	if _, err := state.EnrollAgent(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	second := testEnrollAgent("node-two", "node-2", strings.Repeat("b", 64), "enrollment-key-two", strings.Repeat("3", 64))
	if result, err := state.EnrollAgent(ctx, second); err != nil || !result.Changed {
		t.Fatalf("second node = %v", err)
	}
	wrongIdentity := input
	wrongIdentity.IdempotencyKey = "enrollment-key-three"
	wrongIdentity.RequestHash = strings.Repeat("4", 64)
	wrongIdentity.NodeID = "node-other"
	if _, err := state.EnrollAgent(ctx, wrongIdentity); !errors.Is(err, ErrNodeAlreadyEnrolled) {
		t.Fatalf("same key with wrong node identity = %v", err)
	}
	nodes, err := state.Nodes(ctx)
	if err != nil || len(nodes) != 2 || nodes[0].ClusterID != "cluster-one" || nodes[0].PublicKeyHash != input.PublicKeyHash ||
		nodes[1].Ordinal != 1 || !nodes[0].Ingress || nodes[1].Ingress {
		t.Fatalf("Nodes() = %#v, %v", nodes, err)
	}
	events, err := state.EventsAfter(ctx, 0, 10)
	if err != nil || len(events) != 3 || events[1].Kind != model.EventNodeEnrolled || events[2].Kind != model.EventNodeEnrolled {
		t.Fatalf("events = %#v, %v", events, err)
	}
}

func TestAssignmentAckReportAndExecutionAreMonotonic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := executionStore(t)
	allocation := storeTestAllocation(t)
	published := storeTestPublish(t, allocation)
	published.ApplicationStates = map[string]model.ApplicationExecution{
		"hello": {Status: model.ExecutionPending, Reason: "assignment_pending", Message: "waiting", UpdatedAt: testCreatedAt.Add(2 * time.Minute)},
	}
	assignment, changed, err := state.PublishAssignment(ctx, published)
	if err != nil || !changed || assignment.Generation != 1 {
		t.Fatalf("PublishAssignment() = %#v, %v, %v", assignment, changed, err)
	}
	unchanged, changed, err := state.PublishAssignment(ctx, published)
	if err != nil || changed || unchanged.Generation != 1 {
		t.Fatalf("unchanged PublishAssignment() = %#v, %v, %v", unchanged, changed, err)
	}
	if err := state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{
		NodeID: "node-one", BootID: "boot-one", OperatingSystem: "linux", Architecture: "amd64",
		RuntimeVersion: "containerd 2.3.4", Snapshotter: "overlayfs", CgroupsV2: true,
		SeenAt: testCreatedAt.Add(3 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	report := model.AgentReport{
		NodeID: "node-one", BootID: "boot-one", Sequence: 1,
		AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest,
		ReportedAt: testCreatedAt.Add(4 * time.Minute),
		Allocations: []model.AllocationStatus{{
			ID: allocation.ID, Application: allocation.Application, Service: allocation.Service, Replica: allocation.Replica,
			ApplicationGeneration: allocation.ApplicationGeneration, SpecDigest: allocation.SpecDigest,
			Phase: model.AllocationRunning, Reason: "process_running", Message: "running", UpdatedAt: testCreatedAt.Add(4 * time.Minute),
		}},
	}
	if _, err := state.RecordAgentReport(ctx, report); !errors.Is(err, ErrStaleAgentReport) {
		t.Fatalf("report before ACK = %v", err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", 2, assignment.Digest); !errors.Is(err, ErrAssignmentNotFound) {
		t.Fatalf("future ACK = %v", err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", assignment.Generation, "sha256:"+strings.Repeat("f", 64)); err == nil {
		t.Fatal("wrong-digest ACK succeeded")
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", assignment.Generation, assignment.Digest); err != nil {
		t.Fatal(err)
	}
	accepted, err := state.RecordAgentReport(ctx, report)
	if err != nil || !accepted {
		t.Fatalf("RecordAgentReport() = %v, %v", accepted, err)
	}
	accepted, err = state.RecordAgentReport(ctx, report)
	if err != nil || accepted {
		t.Fatalf("duplicate report = %v, %v", accepted, err)
	}
	application, err := state.Application(ctx, "hello")
	if err != nil || application.Execution.Status != model.ExecutionRunning || application.Execution.ObservedGeneration != 1 || len(application.Execution.Allocations) != 1 {
		t.Fatalf("application execution = %#v, %v", application.Execution, err)
	}
	if observed := application.Execution.Allocations[0]; observed.NodeID != "node-one" || observed.NodeName != "node-1" {
		t.Fatalf("allocation placement identity = %#v", observed)
	}
	node, err := state.Node(ctx)
	if err != nil || node.AcknowledgedGeneration != 1 || node.LastReportSequence != 1 || node.LastBootID != "boot-one" {
		t.Fatalf("node cursor = %#v, %v", node, err)
	}

	for name, mutate := range map[string]func(*model.AgentReport){
		"wrong boot":                func(value *model.AgentReport) { value.BootID = "boot-old" },
		"wrong assignment":          func(value *model.AgentReport) { value.AssignmentDigest = "sha256:" + strings.Repeat("0", 64) },
		"wrong allocation identity": func(value *model.AgentReport) { value.Allocations[0].Service = "other" },
		"unauthorized active revision": func(value *model.AgentReport) {
			value.Allocations[0].ActiveSpecDigest = "sha256:" + strings.Repeat("f", 64)
			value.Allocations[0].ActiveGeneration = 1
			value.Allocations[0].ActiveRevisionID = "revision-one"
		},
		"public allocation address": func(value *model.AgentReport) { value.Allocations[0].Address = "203.0.113.9" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := report
			candidate.Sequence = 2
			candidate.Allocations = append([]model.AllocationStatus(nil), report.Allocations...)
			mutate(&candidate)
			if _, err := state.RecordAgentReport(ctx, candidate); err == nil {
				t.Fatalf("RecordAgentReport() accepted %s", name)
			}
		})
	}
}

func TestTCPReadinessAndRouteProduceHealthyExecution(t *testing.T) {
	ctx := context.Background()
	state := executionStore(t)
	allocation := storeTestAllocation(t)
	allocation.TCP = &model.TCPDesired{Port: 9418, PublicPort: 9418}
	allocation.NetworkGeneration = 1
	allocation.SpecDigest, _ = model.AllocationSpecDigest(allocation)
	job := model.CronJobDesired{Application: "hello", Job: "backup", ApplicationGeneration: 1,
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
	readyAt := testCreatedAt.Add(4 * time.Minute)
	report := model.AgentReport{NodeID: "node-one", BootID: "boot-one", Sequence: 1,
		AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest, ReportedAt: readyAt,
		Allocations: []model.AllocationStatus{{ID: allocation.ID, Application: allocation.Application, Service: allocation.Service,
			Replica: 1, ApplicationGeneration: 1, SpecDigest: allocation.SpecDigest, Phase: model.AllocationRunning,
			HealthStatus: model.HealthReady, Address: "10.88.0.2", ActiveSpecDigest: allocation.SpecDigest,
			ActiveGeneration: 1, ActiveRevisionID: allocation.RevisionID, ReadyAt: &readyAt, DeclaredPort: 9418, UpdatedAt: readyAt}},
		TCPRoutes: []model.TCPRouteStatus{{Application: "hello", Service: "worker", PublicPort: 9418, TargetPort: 9418,
			State: model.TCPRouteActive, UpstreamCount: 1, UpdatedAt: readyAt}},
		CronJobs: []model.CronJobStatus{{Application: "hello", Job: "backup", SpecDigest: job.SpecDigest,
			LastRunID: "job-one", LastState: model.JobRunSucceeded, UpdatedAt: readyAt}},
		JobRuns: []model.JobRunStatus{{ID: "job-one", Application: "hello", Job: "backup", SpecDigest: job.SpecDigest,
			ScheduledAt: readyAt.Add(-time.Minute), State: model.JobRunSucceeded, StartedAt: ptrTime(readyAt.Add(-time.Minute)),
			FinishedAt: &readyAt, UpdatedAt: readyAt}},
		Network: model.NetworkStatus{Generation: 1, State: model.NetworkReady, ClusterCIDR: model.DefaultWorkloadCIDR,
			NodeSubnet: "10.88.0.0/24", Gateway: "10.88.0.1", DNSState: model.SubsystemReady, IngressState: model.SubsystemReady}}
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || !accepted {
		t.Fatalf("RecordAgentReport() = %t, %v", accepted, err)
	}
	application, err := state.Application(ctx, "hello")
	if err != nil || application.Execution.Status != model.ExecutionHealthy || len(application.Execution.TCPRoutes) != 1 ||
		len(application.Execution.CronJobs) != 1 || len(application.Execution.JobRuns) != 1 {
		t.Fatalf("TCP execution = %#v, %v", application.Execution, err)
	}
	if route := application.Execution.TCPRoutes[0]; route.NodeID != "node-one" || route.NodeName != "node-1" {
		t.Fatalf("TCP route node identity = %#v", route)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func TestAgentReportEventsDescribeTransitionsWithoutRepeatingSteadyState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := executionStore(t)
	allocation := storeTestAllocation(t)
	allocation.Service = "web"
	allocation.HTTP = &model.HTTPDesired{
		Port: 8080, Domains: []string{"hello.example.com"}, HealthPath: "/ready",
	}
	var err error
	allocation.SpecDigest, err = model.AllocationSpecDigest(allocation)
	if err != nil {
		t.Fatal(err)
	}
	assignment, _, err := state.PublishAssignment(ctx, storeTestPublish(t, allocation))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{
		NodeID: "node-one", BootID: "boot-one", OperatingSystem: "linux", Architecture: "amd64",
		SeenAt: testCreatedAt.Add(3 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", assignment.Generation, assignment.Digest); err != nil {
		t.Fatal(err)
	}
	readyAt := testCreatedAt.Add(4 * time.Minute)
	report := model.AgentReport{
		NodeID: "node-one", BootID: "boot-one", Sequence: 1,
		AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest,
		ReportedAt: readyAt,
		Allocations: []model.AllocationStatus{{
			ID: allocation.ID, Application: allocation.Application, Service: allocation.Service, Replica: allocation.Replica,
			ApplicationGeneration: allocation.ApplicationGeneration, SpecDigest: allocation.SpecDigest,
			Phase: model.AllocationRunning, HealthStatus: model.HealthReady, Address: "10.88.0.2",
			ActiveSpecDigest: allocation.SpecDigest, ActiveGeneration: allocation.ApplicationGeneration,
			ActiveRevisionID: allocation.RevisionID, ReadyAt: &readyAt, DeclaredPort: 8080, UpdatedAt: readyAt,
		}},
		Routes: []model.RouteStatus{{
			Application: allocation.Application, Service: allocation.Service, Domain: "hello.example.com",
			State: model.RouteActive, CertificateState: "disabled", UpstreamCount: 1, UpdatedAt: readyAt,
		}},
	}
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || !accepted {
		t.Fatalf("ready report = %v, %v", accepted, err)
	}
	for _, kind := range []string{model.EventAllocationReady, model.EventRouteActive, model.EventApplicationHealthy} {
		if got := eventCount(t, state, kind); got != 1 {
			t.Fatalf("%s event count = %d, want 1", kind, got)
		}
	}
	report.Sequence++
	report.ReportedAt = report.ReportedAt.Add(time.Second)
	report.Allocations[0].UpdatedAt = report.ReportedAt
	report.Routes[0].UpdatedAt = report.ReportedAt
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || !accepted {
		t.Fatalf("steady ready report = %v, %v", accepted, err)
	}
	for _, kind := range []string{model.EventAllocationReady, model.EventRouteActive, model.EventApplicationHealthy} {
		if got := eventCount(t, state, kind); got != 1 {
			t.Fatalf("steady-state %s event count = %d, want 1", kind, got)
		}
	}
	report.Sequence++
	report.ReportedAt = report.ReportedAt.Add(time.Second)
	report.Allocations[0].HealthStatus = model.HealthUnhealthy
	report.Allocations[0].ReadyAt = nil
	report.Allocations[0].UpdatedAt = report.ReportedAt
	report.Routes[0].State = model.RouteFailed
	report.Routes[0].CertificateState = ""
	report.Routes[0].UpstreamCount = 0
	report.Routes[0].Reason = "upstream_unhealthy"
	report.Routes[0].UpdatedAt = report.ReportedAt
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || !accepted {
		t.Fatalf("failed report = %v, %v", accepted, err)
	}
	for _, kind := range []string{model.EventAllocationUnhealthy, model.EventRouteFailed} {
		if got := eventCount(t, state, kind); got != 1 {
			t.Fatalf("%s event count = %d, want 1", kind, got)
		}
	}
	report.Sequence++
	report.ReportedAt = report.ReportedAt.Add(time.Second)
	report.Allocations[0].UpdatedAt = report.ReportedAt
	report.Routes[0].UpdatedAt = report.ReportedAt
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || !accepted {
		t.Fatalf("steady failed report = %v, %v", accepted, err)
	}
	for _, kind := range []string{model.EventAllocationUnhealthy, model.EventRouteFailed} {
		if got := eventCount(t, state, kind); got != 1 {
			t.Fatalf("steady-state %s event count = %d, want 1", kind, got)
		}
	}
}

func TestAgentReportRecordsApplicationRolledBackOnlyAfterEveryReplicaConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := executionStore(t)
	if _, err := state.ApplyApplication(ctx, applicationInput(`{"name":"hello","release":2}`,
		"revision-two", "idempotency-key-two", testCreatedAt.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	old := storeTestAllocation(t)
	old.Service = "web"
	old.HTTP = &model.HTTPDesired{Port: 8080, Domains: []string{"hello.example.com"}, HealthPath: "/ready"}
	old.NetworkGeneration = 1
	old.SpecDigest, _ = model.AllocationSpecDigest(old)
	first := old
	first.ApplicationGeneration = 2
	first.RevisionID = "revision-two"
	first.Rollback = &model.RollbackSpec{
		ApplicationGeneration: 1, RevisionID: "revision-one", SpecDigest: old.SpecDigest,
		Image: old.Image, Command: old.Command, Args: old.Args, Environment: old.Environment,
		RestartPolicy: old.RestartPolicy, CPUMillis: old.CPUMillis, MemoryBytes: old.MemoryBytes,
		StartDeadlineSeconds: old.StartDeadlineSeconds, HTTP: old.HTTP, AutoRollback: true,
		StartBeforeStop: true, NetworkGeneration: 1,
	}
	first.HTTP = &model.HTTPDesired{
		Port: 8080, Domains: []string{"next.example.com"}, HealthPath: "/ready", HTTPS: true,
	}
	first.Image = "docker.io/library/busybox@sha256:" + strings.Repeat("b", 64)
	first.SpecDigest, _ = model.AllocationSpecDigest(first)
	second := first
	second.ID = "alloc-two"
	second.Replica = 2
	second.Image = old.Image
	second.SpecDigest, _ = model.AllocationSpecDigest(second)
	rollbackDigest := first.Rollback.SpecDigest
	second.Rollback = cloneRollbackForTest(first.Rollback)
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	payload, digest, err := model.AssignmentPayload(networkConfig, []model.AllocationDesired{first, second})
	if err != nil {
		t.Fatal(err)
	}
	assignment, _, err := state.PublishAssignment(ctx, model.PublishAssignment{
		NodeID: "node-one", Digest: digest, Payload: payload, Network: networkConfig,
		Allocations: []model.AllocationDesired{first, second}, CreatedAt: testCreatedAt.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{
		NodeID: "node-one", BootID: "boot-one", OperatingSystem: "linux", Architecture: "amd64",
		SeenAt: testCreatedAt.Add(3 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", assignment.Generation, assignment.Digest); err != nil {
		t.Fatal(err)
	}
	readyAt := testCreatedAt.Add(4 * time.Minute)
	status := func(allocation model.AllocationDesired, activeDigest string, activeGeneration uint64, activeRevision, reason string) model.AllocationStatus {
		address := "10.88.0.2"
		if allocation.Replica == 2 {
			address = "10.88.0.3"
		}
		return model.AllocationStatus{
			ID: allocation.ID, Application: allocation.Application, Service: allocation.Service,
			Replica: allocation.Replica, ApplicationGeneration: allocation.ApplicationGeneration,
			SpecDigest: allocation.SpecDigest, Phase: model.AllocationRunning, HealthStatus: model.HealthReady,
			Address: address, ActiveSpecDigest: activeDigest,
			ActiveGeneration: activeGeneration, ActiveRevisionID: activeRevision, ReadyAt: &readyAt,
			DeclaredPort: 8080, Reason: reason, UpdatedAt: readyAt,
		}
	}
	report := model.AgentReport{
		NodeID: "node-one", BootID: "boot-one", Sequence: 1,
		AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest,
		Allocations: []model.AllocationStatus{
			status(first, rollbackDigest, 1, "revision-one", "readiness_checking"),
			status(second, rollbackDigest, 1, "revision-one", "readiness_checking"),
		},
		Routes: []model.RouteStatus{{
			Application: "hello", Service: "web", Domain: "hello.example.com", State: model.RouteActive,
			CertificateState: "disabled", UpstreamCount: 2, UpdatedAt: readyAt,
		}},
		Network: model.NetworkStatus{
			Generation: 1, State: model.NetworkReady, ClusterCIDR: model.DefaultWorkloadCIDR,
			NodeSubnet: "10.88.0.0/24", Gateway: "10.88.0.1", DNSState: model.SubsystemReady,
			IngressState: model.SubsystemReady,
		},
		ReportedAt: readyAt,
	}
	for index := range report.Allocations {
		report.Allocations[index].HealthStatus = model.HealthChecking
		report.Allocations[index].ReadyAt = nil
	}
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || !accepted {
		t.Fatalf("in-progress rollout report = %t, %v", accepted, err)
	}
	application, err := state.Application(ctx, "hello")
	if err != nil || application.Execution.Status != model.ExecutionStarting ||
		application.Execution.Reason != "allocations_starting" || application.Execution.ActiveGeneration != 1 {
		t.Fatalf("in-progress rollout execution = %#v, %v", application.Execution, err)
	}
	if got := eventCount(t, state, model.EventApplicationRolledBack); got != 0 {
		t.Fatalf("rollout emitted rollback event count = %d", got)
	}

	report.Sequence++
	report.ReportedAt = report.ReportedAt.Add(time.Second)
	for index := range report.Allocations {
		report.Allocations[index] = status([]model.AllocationDesired{first, second}[index],
			rollbackDigest, 1, "revision-one", "readiness_checking")
		report.Allocations[index].UpdatedAt = report.ReportedAt
	}
	report.Routes[0].UpdatedAt = report.ReportedAt
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || !accepted {
		t.Fatalf("ready fallbacks during rollout = %t, %v", accepted, err)
	}
	application, err = state.Application(ctx, "hello")
	if err != nil || application.Execution.Status != model.ExecutionStarting ||
		application.Execution.ActiveGeneration != 1 {
		t.Fatalf("ready fallbacks were conflated with desired health = %#v, %v", application.Execution, err)
	}

	report.Sequence++
	report.ReportedAt = report.ReportedAt.Add(time.Second)
	for index := range report.Allocations {
		report.Allocations[index] = status([]model.AllocationDesired{first, second}[index],
			[]string{first.SpecDigest, second.SpecDigest}[index], 2, "revision-two", "desired_ready")
		report.Allocations[index].UpdatedAt = report.ReportedAt
	}
	report.Routes[0].UpdatedAt = report.ReportedAt
	if accepted, err := state.RecordAgentReport(ctx, report); err == nil || accepted {
		t.Fatalf("active rollback route with desired runtimes = %t, %v; want rejection", accepted, err)
	}

	report.Sequence++
	report.ReportedAt = report.ReportedAt.Add(time.Second)
	report.Allocations[0] = status(first, rollbackDigest, 1, "revision-one", "health_gate_rollback")
	report.Allocations[1] = status(second, second.SpecDigest, 2, "revision-two", "rollback_budget_wait")
	for index := range report.Allocations {
		report.Allocations[index].UpdatedAt = report.ReportedAt
	}
	report.Routes[0].UpdatedAt = report.ReportedAt
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || !accepted {
		t.Fatalf("mixed rollback report = %t, %v", accepted, err)
	}
	application, err = state.Application(ctx, "hello")
	if err != nil || application.Execution.Status != model.ExecutionDegraded ||
		application.Execution.Reason != "rollback_converging" || application.Execution.ActiveGeneration != 0 {
		t.Fatalf("mixed rollback execution = %#v, %v", application.Execution, err)
	}
	if got := eventCount(t, state, model.EventApplicationRolledBack); got != 0 {
		t.Fatalf("premature rollback event count = %d", got)
	}

	report.Sequence++
	report.ReportedAt = report.ReportedAt.Add(time.Second)
	report.Allocations[1] = status(second, rollbackDigest, 1, "revision-one", "health_gate_rollback")
	report.Allocations[1].UpdatedAt = report.ReportedAt
	report.Routes[0].UpdatedAt = report.ReportedAt
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || !accepted {
		t.Fatalf("converged rollback report = %t, %v", accepted, err)
	}
	application, err = state.Application(ctx, "hello")
	if err != nil || application.Execution.Status != model.ExecutionRolledBack ||
		application.Execution.ActiveGeneration != 1 || application.Execution.ActiveRevisionID != "revision-one" {
		t.Fatalf("rolled-back execution = %#v, %v", application.Execution, err)
	}
	if got := eventCount(t, state, model.EventApplicationRolledBack); got != 1 {
		t.Fatalf("rollback event count = %d, want 1", got)
	}
	report.Sequence++
	report.ReportedAt = report.ReportedAt.Add(time.Second)
	for index := range report.Allocations {
		report.Allocations[index].UpdatedAt = report.ReportedAt
	}
	report.Routes[0].UpdatedAt = report.ReportedAt
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || !accepted {
		t.Fatalf("steady rollback report = %t, %v", accepted, err)
	}
	if got := eventCount(t, state, model.EventApplicationRolledBack); got != 1 {
		t.Fatalf("steady rollback event count = %d, want 1", got)
	}
}

func cloneRollbackForTest(input *model.RollbackSpec) *model.RollbackSpec {
	result := *input
	result.Command = append([]string(nil), input.Command...)
	result.Args = append([]string(nil), input.Args...)
	result.Environment = make(map[string]string, len(input.Environment))
	for key, value := range input.Environment {
		result.Environment[key] = value
	}
	if input.HTTP != nil {
		http := *input.HTTP
		http.Domains = append([]string(nil), input.HTTP.Domains...)
		result.HTTP = &http
	}
	return &result
}

func TestSupersededAgentReportIsIgnoredBeforeNewAssignmentAck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := executionStore(t)
	allocation := storeTestAllocation(t)
	first, _, err := state.PublishAssignment(ctx, storeTestPublish(t, allocation))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{
		NodeID: "node-one", BootID: "boot-one", OperatingSystem: "linux", Architecture: "amd64",
		SeenAt: testCreatedAt.Add(3 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", first.Generation, first.Digest); err != nil {
		t.Fatal(err)
	}

	next := storeTestPublish(t, allocation)
	next.Allocations = nil
	next.Payload, next.Digest, err = model.AssignmentPayload(next.Network, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, changed, err := state.PublishAssignment(ctx, next)
	if err != nil || !changed || second.Generation != first.Generation+1 {
		t.Fatalf("second assignment = %#v, %t, %v", second, changed, err)
	}
	report := model.AgentReport{
		NodeID: "node-one", BootID: "boot-one", Sequence: 1,
		AssignmentGeneration: first.Generation, AssignmentDigest: first.Digest,
		ReportedAt: testCreatedAt.Add(4 * time.Minute),
	}
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || accepted {
		t.Fatalf("superseded report = %t, %v", accepted, err)
	}
	node, err := state.Node(ctx)
	if err != nil || node.AssignmentGeneration != second.Generation ||
		node.AcknowledgedGeneration != first.Generation || node.LastReportSequence != 0 {
		t.Fatalf("node cursor after ignored report = %#v, %v", node, err)
	}
}

func TestPreservedAssignmentReportCannotObserveANewerApplicationGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := executionStore(t)
	allocation := storeTestAllocation(t)
	assignment, _, err := state.PublishAssignment(ctx, storeTestPublish(t, allocation))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{
		NodeID: "node-one", BootID: "boot-one", OperatingSystem: "linux", Architecture: "amd64",
		SeenAt: testCreatedAt.Add(3 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", assignment.Generation, assignment.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ApplyApplication(ctx, applicationInput(`{"name":"hello","release":2}`,
		"revision-two", "idempotency-key-two", testCreatedAt.Add(4*time.Minute))); err != nil {
		t.Fatal(err)
	}

	reportedAt := testCreatedAt.Add(5 * time.Minute)
	report := model.AgentReport{
		NodeID: "node-one", BootID: "boot-one", Sequence: 1,
		AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest,
		ReportedAt: reportedAt,
		Allocations: []model.AllocationStatus{{
			ID: allocation.ID, Application: allocation.Application, Service: allocation.Service,
			Replica: allocation.Replica, ApplicationGeneration: allocation.ApplicationGeneration,
			SpecDigest: allocation.SpecDigest, Phase: model.AllocationRunning,
			HealthStatus: model.HealthNotConfigured, ActiveSpecDigest: allocation.SpecDigest,
			ActiveGeneration: allocation.ApplicationGeneration, ActiveRevisionID: allocation.RevisionID,
			Reason: "process_running", UpdatedAt: reportedAt,
		}},
	}
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || !accepted {
		t.Fatalf("preserved assignment report = %t, %v", accepted, err)
	}
	application, err := state.Application(ctx, "hello")
	if err != nil || application.Generation != 2 || application.Execution.Status != model.ExecutionPending ||
		application.Execution.ObservedGeneration != 0 {
		t.Fatalf("new desired generation was overwritten by a preserved report: %#v, %v", application, err)
	}
}

func eventCount(t *testing.T, state *Store, kind string) int {
	t.Helper()
	var count int
	if err := state.db.QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestAssignmentPayloadMustExactlyMatchDesiredRows(t *testing.T) {
	t.Parallel()
	state := executionStore(t)
	allocation := storeTestAllocation(t)
	input := storeTestPublish(t, allocation)
	input.Allocations = nil
	if _, _, err := state.PublishAssignment(context.Background(), input); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("PublishAssignment() = %v", err)
	}
	if _, err := state.CurrentAssignment(context.Background(), "node-one"); !errors.Is(err, ErrAssignmentNotFound) {
		t.Fatalf("CurrentAssignment() = %v", err)
	}
}

func TestAssignmentPublicationRejectsNonCanonicalAllocationOrder(t *testing.T) {
	t.Parallel()
	state := executionStore(t)
	first := storeTestAllocation(t)
	first.ID = "alloc-z"
	second := first
	second.ID = "alloc-a"
	input := storeTestPublish(t, first, second)
	if _, _, err := state.PublishAssignment(context.Background(), input); err == nil || !strings.Contains(err.Error(), "canonical order") {
		t.Fatalf("PublishAssignment() = %v", err)
	}
	if _, err := state.CurrentAssignment(context.Background(), "node-one"); !errors.Is(err, ErrAssignmentNotFound) {
		t.Fatalf("CurrentAssignment() = %v", err)
	}
}

func TestApplicationRemovalPublishesTombstoneAndPreservesHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := executionStore(t)
	allocation := storeTestAllocation(t)
	assignment, _, err := state.PublishAssignment(ctx, storeTestPublish(t, allocation))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{NodeID: "node-one", BootID: "boot-one", SeenAt: testCreatedAt.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", assignment.Generation, assignment.Digest); err != nil {
		t.Fatal(err)
	}
	removed, err := state.RemoveApplication(ctx, "hello", "application-removal-key-one", testCreatedAt.Add(2*time.Minute))
	if err != nil || !removed.Changed || removed.ExecutionStatus != model.ExecutionStopping {
		t.Fatalf("RemoveApplication() = %#v, %v", removed, err)
	}
	replay, err := state.RemoveApplication(ctx, "hello", "application-removal-key-one", testCreatedAt.Add(3*time.Minute))
	if err != nil || !reflect.DeepEqual(replay, removed) {
		t.Fatalf("removal replay = %#v, %v", replay, err)
	}
	empty := storeTestPublish(t)
	empty.ApplicationStates = map[string]model.ApplicationExecution{
		"hello": {Status: model.ExecutionStopping, Reason: "removal_pending", Message: "waiting", UpdatedAt: testCreatedAt.Add(3 * time.Minute)},
	}
	tombstone, changed, err := state.PublishAssignment(ctx, empty)
	if err != nil || !changed || len(tombstone.Allocations) != 0 {
		t.Fatalf("tombstone assignment = %#v, %v, %v", tombstone, changed, err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", tombstone.Generation, tombstone.Digest); err != nil {
		t.Fatal(err)
	}
	report := model.AgentReport{
		NodeID: "node-one", BootID: "boot-one", Sequence: 1,
		AssignmentGeneration: tombstone.Generation, AssignmentDigest: tombstone.Digest,
		ReportedAt: testCreatedAt.Add(4 * time.Minute), Allocations: []model.AllocationStatus{},
	}
	if accepted, err := state.RecordAgentReport(ctx, report); err != nil || !accepted {
		t.Fatalf("tombstone report = %v, %v", accepted, err)
	}
	application, err := state.Application(ctx, "hello")
	if err != nil || application.DeletedAt == nil || application.Execution.Status != model.ExecutionRemoved || application.Generation != 1 {
		t.Fatalf("removed application = %#v, %v", application, err)
	}
	var revisions int
	if err := state.db.QueryRowContext(ctx, `SELECT count(*) FROM application_revisions WHERE application_name = 'hello'`).Scan(&revisions); err != nil || revisions != 1 {
		t.Fatalf("revision count = %d, %v", revisions, err)
	}
}

func executionStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ApplyApplication(ctx, applicationInput(`{"name":"hello"}`, "revision-one", "idempotency-key-one", testCreatedAt)); err != nil {
		t.Fatal(err)
	}
	if _, err := state.EnrollAgent(ctx, testEnrollAgent("node-one", "node-1", strings.Repeat("a", 64), "enrollment-key-one", strings.Repeat("1", 64))); err != nil {
		t.Fatal(err)
	}
	return state
}

func testEnrollAgent(nodeID, name, keyHash, key, requestHash string) model.EnrollAgent {
	return model.EnrollAgent{
		NodeID: nodeID, Name: name, PublicKeyHash: keyHash, CertificateSerial: "17",
		CertificatePEM: []byte("agent certificate"), CertificateNotAfter: testCreatedAt.AddDate(1, 0, 0),
		ClusterID: "cluster-one", ControllerEndpoint: "https://controller.example:9443",
		CACertificatePEM: []byte("CA certificate"), IdempotencyKey: key, RequestHash: requestHash,
		CreatedAt: testCreatedAt.Add(time.Minute),
	}
}

func storeTestAllocation(t *testing.T) model.AllocationDesired {
	t.Helper()
	allocation := model.AllocationDesired{
		ID: "alloc-one", Application: "hello", Service: "worker", Replica: 1,
		ApplicationGeneration: 1, RevisionID: "revision-one",
		Image:   "docker.io/library/busybox@sha256:" + strings.Repeat("a", 64),
		Command: []string{"/bin/sleep"}, Args: []string{"3600"}, RestartPolicy: "always",
		CPUMillis: 100, MemoryBytes: 128 << 20, StartDeadlineSeconds: 30,
		AutoRollback: true, StartBeforeStop: true,
	}
	var err error
	allocation.SpecDigest, err = model.AllocationSpecDigest(allocation)
	if err != nil {
		t.Fatal(err)
	}
	return allocation
}

func storeTestPublish(t *testing.T, allocations ...model.AllocationDesired) model.PublishAssignment {
	t.Helper()
	payload, digest, err := model.AssignmentPayload(model.NetworkConfig{}, allocations)
	if err != nil {
		t.Fatal(err)
	}
	return model.PublishAssignment{
		NodeID: "node-one", Digest: digest, Payload: payload,
		Allocations: allocations, CreatedAt: testCreatedAt.Add(2 * time.Minute),
	}
}
