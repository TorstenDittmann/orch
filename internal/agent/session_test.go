package agent

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/internal/model"
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
