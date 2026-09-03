package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

func TestAgentEnrollmentIsSingleNodeAndExactlyReplayable(t *testing.T) {
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
	if _, err := state.EnrollAgent(ctx, second); !errors.Is(err, ErrNodeAlreadyEnrolled) {
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
	if err != nil || len(nodes) != 1 || nodes[0].ClusterID != "cluster-one" || nodes[0].PublicKeyHash != input.PublicKeyHash {
		t.Fatalf("Nodes() = %#v, %v", nodes, err)
	}
	events, err := state.EventsAfter(ctx, 0, 10)
	if err != nil || len(events) != 2 || events[1].Kind != model.EventNodeEnrolled {
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
	node, err := state.Node(ctx)
	if err != nil || node.AcknowledgedGeneration != 1 || node.LastReportSequence != 1 || node.LastBootID != "boot-one" {
		t.Fatalf("node cursor = %#v, %v", node, err)
	}

	for name, mutate := range map[string]func(*model.AgentReport){
		"wrong boot":                func(value *model.AgentReport) { value.BootID = "boot-old" },
		"wrong assignment":          func(value *model.AgentReport) { value.AssignmentDigest = "sha256:" + strings.Repeat("0", 64) },
		"wrong allocation identity": func(value *model.AgentReport) { value.Allocations[0].Service = "other" },
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
	payload, err := json.Marshal(allocations)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return model.PublishAssignment{
		NodeID: "node-one", Digest: "sha256:" + hex.EncodeToString(digest[:]), Payload: payload,
		Allocations: allocations, CreatedAt: testCreatedAt.Add(2 * time.Minute),
	}
}
