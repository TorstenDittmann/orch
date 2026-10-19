package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestReconnectJitterUsesInjectedRandomSource(t *testing.T) {
	maximum := 500 * time.Millisecond
	jitter, err := reconnectJitter(strings.NewReader(strings.Repeat("\x00", 16)), maximum)
	if err != nil {
		t.Fatalf("reconnectJitter: %v", err)
	}
	if jitter != 0 {
		t.Fatalf("jitter = %s, want zero from the injected all-zero source", jitter)
	}

	want := errors.New("random source failed")
	if _, err := reconnectJitter(errorReader{want}, maximum); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if jitter, err := reconnectJitter(errorReader{want}, 0); err != nil || jitter != 0 {
		t.Fatalf("zero maximum = (%s, %v), want (0, nil)", jitter, err)
	}
}

func TestReconnectBackoffStartsOverAfterAHealthySession(t *testing.T) {
	failed := errors.New("stream reset")
	if got := reconnectBackoff(maxReconnectBackoff, failed, stableSessionDuration); got != time.Second {
		t.Fatalf("after a stable session = %s, want 1s", got)
	}
	if got := reconnectBackoff(maxReconnectBackoff, nil, 0); got != time.Second {
		t.Fatalf("after a clean close = %s, want 1s", got)
	}
	if got := reconnectBackoff(8*time.Second, failed, time.Second); got != 8*time.Second {
		t.Fatalf("after a failed attempt = %s, want the current 8s", got)
	}
}

func TestReconcileCachedSurvivesAllocationFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	assignment := ownershipMismatchAssignment(t, runtime)
	assignment.NodeID, assignment.Generation, assignment.CreatedAt = "node-one", 1, supervisorTestTime
	var err error
	_, assignment.Digest, err = model.AssignmentPayloadWithJobs(assignment.Network, assignment.Allocations, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.CommitAssignment(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	runner := sessionRunner{cache: cache, supervisor: NewSupervisor(cache, runtime, func() time.Time { return supervisorTestTime })}

	// A persistent per-allocation fault must not stop the agent before it can
	// connect and receive the assignment that repairs it.
	if err := runner.reconcileCached(ctx); err != nil {
		t.Fatalf("reconcileCached() = %v, want nil", err)
	}
	healthy := assignment.Allocations[1]
	if _, ok := runtime.SnapshotObjects()[orchruntime.ObjectID(healthy.ID, healthy.SpecDigest)]; !ok {
		t.Fatalf("healthy allocation was not started: %v", runtime.SnapshotOperations())
	}
	if err := runner.reconcileCached(ctx); err != nil {
		t.Fatalf("second reconcileCached() = %v, want nil", err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := runner.reconcileCached(canceled); err == nil {
		t.Fatal("reconcileCached(canceled) = nil, want the cache read error")
	}
}

func TestAssignmentRuntimeFaultHoldsACKWithoutDroppingTheSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	runtime.ListError = errors.New("containerd is busy")
	runner := sessionRunner{bootID: "boot-one", cache: cache, supervisor: NewSupervisor(cache, runtime, func() time.Time { return supervisorTestTime })}
	runner.credentials.Config.NodeID = "node-one"
	_, digest, err := model.AssignmentPayloadWithJobs(model.NetworkConfig{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	message := &orchv1alpha1.ConnectResponse{Message: &orchv1alpha1.ConnectResponse_Assignment{Assignment: &orchv1alpha1.AssignmentSnapshot{
		NodeId: "node-one", Generation: 1, Digest: digest, CreatedAt: timestamppb.New(supervisorTestTime),
	}}}
	outbound := make(chan *orchv1alpha1.ConnectRequest, 8)

	if err := runner.handleControllerMessage(ctx, message, outbound, nil, newAgentOperations()); err != nil {
		t.Fatalf("handleControllerMessage() = %v, want the session kept", err)
	}
	if len(outbound) != 0 || !runner.ackPending {
		t.Fatalf("queued %d messages with ackPending=%t, want the ACK held", len(outbound), runner.ackPending)
	}

	runtime.ListError = nil
	if err := runner.handleControllerMessage(ctx, message, outbound, nil, newAgentOperations()); err != nil {
		t.Fatal(err)
	}
	if ack := (<-outbound).GetAssignmentAck(); ack == nil || ack.GetGeneration() != 1 || runner.ackPending {
		t.Fatalf("first queued message = %v, ackPending=%t, want the generation 1 ACK", ack, runner.ackPending)
	}
}

func TestAssignmentModelRejectsNonCanonicalAllocationOrder(t *testing.T) {
	t.Parallel()
	snapshot := &orchv1alpha1.AssignmentSnapshot{
		NodeId: "node-one", Generation: 1, Digest: "sha256:" + strings.Repeat("a", 64),
		CreatedAt: timestamppb.New(time.Now()),
		Allocations: []*orchv1alpha1.AllocationDesired{
			validWireAllocation(t, "alloc-b", 2),
			validWireAllocation(t, "alloc-a", 1),
		},
	}
	if _, err := assignmentModel(snapshot, "node-one"); err == nil || !strings.Contains(err.Error(), "canonical order") {
		t.Fatalf("assignmentModel() = %v", err)
	}
	snapshot.Allocations[1].Id = "alloc-b"
	if _, err := assignmentModel(snapshot, "node-one"); err == nil || !strings.Contains(err.Error(), "canonical order") {
		t.Fatalf("assignmentModel(duplicate) = %v", err)
	}
}

func TestAssignmentModelPreservesNodeLocalHTTP(t *testing.T) {
	t.Parallel()
	wire := validWireAllocation(t, "alloc-local", 1)
	snapshot := &orchv1alpha1.AssignmentSnapshot{
		NodeId: "node-one", Generation: 1, Digest: "sha256:" + strings.Repeat("a", 64),
		CreatedAt: timestamppb.New(supervisorTestTime), Allocations: []*orchv1alpha1.AllocationDesired{wire},
	}
	seed, err := assignmentModel(snapshot, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	allocation := seed.Allocations[0]
	allocation.NetworkGeneration = 1
	allocation.HTTP = &model.HTTPDesired{Port: 8080, Domains: []string{"local.example.com"}, HealthPath: "/health", HTTPS: true, NodeLocal: true}
	wire.NetworkGeneration = 1
	wire.SpecDigest, err = model.AllocationSpecDigest(allocation)
	if err != nil {
		t.Fatal(err)
	}
	wire.Http = &orchv1alpha1.HTTPDesired{Port: 8080, Domains: allocation.HTTP.Domains, HealthPath: "/health", Https: true, NodeLocal: true}
	network, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Network = &orchv1alpha1.NetworkConfig{
		Generation: network.Generation, ClusterCidr: network.ClusterCIDR, NodeSubnet: network.NodeSubnet,
		Gateway: network.Gateway, DnsZone: network.DNSZone,
		Endpoints: []*orchv1alpha1.ServiceEndpoint{{
			AllocationId: "alloc-local", Application: "hello", Service: "worker", NodeId: "node-one",
			Address: "10.88.0.2", Port: 8080, Domains: []string{"local.example.com"}, Https: true, HttpNodeLocal: true,
		}},
	}
	decoded, err := assignmentModel(snapshot, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Allocations[0].HTTP.NodeLocal || !decoded.Network.Endpoints[0].HTTPNodeLocal {
		t.Fatalf("node-local scope lost decoding assignment: %#v", decoded)
	}
}

func TestAssignmentModelRejectsTooManyAllocationsBeforeDecoding(t *testing.T) {
	t.Parallel()
	snapshot := &orchv1alpha1.AssignmentSnapshot{
		NodeId: "node-one", Generation: 1, Digest: "sha256:" + strings.Repeat("a", 64),
		CreatedAt:   timestamppb.New(time.Now()),
		Allocations: make([]*orchv1alpha1.AllocationDesired, maxAssignedAllocations+1),
	}
	if _, err := assignmentModel(snapshot, "node-one"); err == nil || !strings.Contains(err.Error(), "allocation limit") {
		t.Fatalf("assignmentModel() = %v", err)
	}
}

func TestAssignmentModelRejectsInvalidAllocationBeforeCaching(t *testing.T) {
	t.Parallel()
	item := validWireAllocation(t, "../escape", 1)
	snapshot := &orchv1alpha1.AssignmentSnapshot{
		NodeId: "node-one", Generation: 1, Digest: "sha256:" + strings.Repeat("a", 64),
		CreatedAt: timestamppb.New(time.Now()), Allocations: []*orchv1alpha1.AllocationDesired{item},
	}
	if _, err := assignmentModel(snapshot, "node-one"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("assignmentModel() = %v", err)
	}
}

func TestAssignmentModelRejectsAllocationForAnotherNetworkGeneration(t *testing.T) {
	t.Parallel()
	network, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &orchv1alpha1.AssignmentSnapshot{
		NodeId: "node-one", Generation: 1, Digest: "sha256:" + strings.Repeat("a", 64),
		CreatedAt: timestamppb.New(time.Now()), Allocations: []*orchv1alpha1.AllocationDesired{validWireAllocation(t, "alloc-one", 1)},
		Network: &orchv1alpha1.NetworkConfig{Generation: network.Generation, ClusterCidr: network.ClusterCIDR,
			NodeSubnet: network.NodeSubnet, Gateway: network.Gateway, DnsZone: network.DNSZone},
	}
	if _, err := assignmentModel(snapshot, "node-one"); err == nil || !strings.Contains(err.Error(), "network generation") {
		t.Fatalf("assignmentModel() = %v", err)
	}
}

func TestAssignmentModelAcceptsExactPlan4AllocationDuringNetworkPreflight(t *testing.T) {
	t.Parallel()
	network, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	item := validWireAllocation(t, "alloc-one", 1)
	legacy := model.AllocationDesired{
		ID: item.GetId(), Application: item.GetApplication(), Service: item.GetService(), Replica: item.GetReplica(),
		ApplicationGeneration: item.GetApplicationGeneration(), RevisionID: item.GetRevisionId(),
		Image: item.GetImage(), Command: item.GetCommand(), Args: item.GetArgs(), RestartPolicy: item.GetRestartPolicy(),
		CPUMillis: item.GetCpuMillis(), MemoryBytes: item.GetMemoryBytes(), StartDeadlineSeconds: item.GetStartDeadlineSeconds(),
		AutoRollback: item.GetAutoRollback(), StartBeforeStop: item.GetStartBeforeStop(),
	}
	item.SpecDigest, err = model.Plan4AllocationSpecDigest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &orchv1alpha1.AssignmentSnapshot{
		NodeId: "node-one", Generation: 2, CreatedAt: timestamppb.New(time.Now()),
		Allocations: []*orchv1alpha1.AllocationDesired{item},
		Network: &orchv1alpha1.NetworkConfig{Generation: network.Generation, ClusterCidr: network.ClusterCIDR,
			NodeSubnet: network.NodeSubnet, Gateway: network.Gateway, DnsZone: network.DNSZone},
	}
	assignmentForDigest := model.Assignment{Network: network, Allocations: []model.AllocationDesired{legacy}}
	assignmentForDigest.Allocations[0].SpecDigest = item.SpecDigest
	_, snapshot.Digest, err = model.AssignmentPayload(assignmentForDigest.Network, assignmentForDigest.Allocations)
	if err != nil {
		t.Fatal(err)
	}
	got, err := assignmentModel(snapshot, "node-one")
	if err != nil || len(got.Allocations) != 1 || got.Allocations[0].SpecDigest != item.SpecDigest {
		t.Fatalf("assignmentModel(legacy preflight) = %#v, %v", got, err)
	}
	item.MaxSurge = 1
	if _, err := assignmentModel(snapshot, "node-one"); err == nil || !strings.Contains(err.Error(), "spec digest") {
		t.Fatalf("assignmentModel(extended legacy allocation) = %v", err)
	}
}

func validWireAllocation(t *testing.T, id string, replica uint32) *orchv1alpha1.AllocationDesired {
	t.Helper()
	allocation := model.AllocationDesired{
		ID: id, Application: "hello", Service: "worker", Replica: replica,
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
	return &orchv1alpha1.AllocationDesired{
		Id: allocation.ID, Application: allocation.Application, Service: allocation.Service,
		Replica: allocation.Replica, ApplicationGeneration: allocation.ApplicationGeneration,
		RevisionId: allocation.RevisionID, SpecDigest: allocation.SpecDigest, Image: allocation.Image,
		Command: allocation.Command, Args: allocation.Args, RestartPolicy: allocation.RestartPolicy,
		CpuMillis: allocation.CPUMillis, MemoryBytes: allocation.MemoryBytes,
		StartDeadlineSeconds: allocation.StartDeadlineSeconds, AutoRollback: allocation.AutoRollback,
		StartBeforeStop: allocation.StartBeforeStop,
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

var _ io.Reader = errorReader{}

type recordingSender struct {
	sent []*orchv1alpha1.ConnectRequest
}

func (s *recordingSender) Send(request *orchv1alpha1.ConnectRequest) error {
	s.sent = append(s.sent, request)
	return nil
}

func TestControlMessagesBypassSaturatedOperationQueue(t *testing.T) {
	t.Parallel()
	outbound := make(chan *orchv1alpha1.ConnectRequest, 64)
	operationOutbound := make(chan *orchv1alpha1.ConnectRequest, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		// A large tail fills the operation queue and then blocks.
		for {
			select {
			case <-ctx.Done():
				return
			case operationOutbound <- operationResult(&orchv1alpha1.AgentOperationResult_Log{Log: &orchv1alpha1.TailLogsResponse{
				RequestId: "op-one", Data: make([]byte, maxOperationChunk),
			}}):
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(operationOutbound) < cap(operationOutbound) {
		if time.Now().After(deadline) {
			t.Fatal("operation queue did not saturate")
		}
		time.Sleep(time.Millisecond)
	}
	heartbeat := &orchv1alpha1.ConnectRequest{Message: &orchv1alpha1.ConnectRequest_Heartbeat{Heartbeat: &orchv1alpha1.AgentHeartbeat{BootId: "boot-one"}}}
	ack := &orchv1alpha1.ConnectRequest{Message: &orchv1alpha1.ConnectRequest_AssignmentAck{AssignmentAck: &orchv1alpha1.AssignmentAck{BootId: "boot-one", Generation: 1}}}
	if err := enqueueAgentRequest(outbound, heartbeat); err != nil {
		t.Fatalf("enqueue heartbeat while operation saturated: %v", err)
	}
	if err := enqueueAgentRequest(outbound, ack); err != nil {
		t.Fatalf("enqueue ACK while operation saturated: %v", err)
	}
	runner := &sessionRunner{}
	stream := &recordingSender{}
	for range 2 {
		if sent, err := runner.sendControl(stream, outbound); err != nil || !sent {
			t.Fatalf("sendControl() = %v, %v", sent, err)
		}
	}
	if sent, err := runner.sendControl(stream, outbound); err != nil || sent {
		t.Fatalf("sendControl(empty) = %v, %v", sent, err)
	}
	if len(stream.sent) != 2 || stream.sent[0] != heartbeat || stream.sent[1] != ack {
		t.Fatalf("sent = %v", stream.sent)
	}
	if !runner.assignmentAcknowledged {
		t.Fatal("sent ACK did not mark the assignment acknowledged")
	}
	cancel()
	<-producerDone
}
