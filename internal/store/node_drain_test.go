package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/TorstenDittmann/orch/internal/model"
)

func TestNodeDrainBlockersOnlyReportForDrainingNodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	node := model.Node{ID: "node-one", AssignmentGeneration: 3, AcknowledgedGeneration: 2, DrainingObjects: 1}
	for lifecycle, want := range map[string][]string{
		model.NodeLifecycleActive:   nil,
		model.NodeLifecycleCordoned: nil,
		model.NodeLifecycleDraining: {"assignment_ack_pending", "runtime_objects_draining"},
	} {
		node.Lifecycle = lifecycle
		got, err := state.NodeDrainBlockers(ctx, node)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s blockers = %#v, %v; want %#v", lifecycle, got, err, want)
		}
	}
}

func TestNodeDrainBlockersReportReplicasThatCanMove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := executionStore(t)
	allocation := storeTestAllocation(t)
	pinned := storeTestAllocation(t)
	pinned.ID, pinned.Replica, pinned.Mounts = "alloc-two", 2, []model.VolumeMount{{Name: "data", Target: "/data"}}
	pinned.SpecDigest, _ = model.AllocationSpecDigest(pinned)
	if _, _, err := state.PublishAssignment(ctx, storeTestPublish(t, pinned)); err != nil {
		t.Fatal(err)
	}
	node, err := state.NodeByID(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	node.Lifecycle = model.NodeLifecycleDraining
	node.AcknowledgedGeneration = node.AssignmentGeneration
	if got, err := state.NodeDrainBlockers(ctx, node); err != nil || len(got) != 0 {
		t.Fatalf("pinned replica blockers = %#v, %v", got, err)
	}
	if _, _, err := state.PublishAssignment(ctx, storeTestPublish(t, allocation, pinned)); err != nil {
		t.Fatal(err)
	}
	node, err = state.NodeByID(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	node.Lifecycle = model.NodeLifecycleDraining
	node.AcknowledgedGeneration = node.AssignmentGeneration
	got, err := state.NodeDrainBlockers(ctx, node)
	if err != nil || !reflect.DeepEqual(got, []string{"replicas_moving"}) {
		t.Fatalf("blockers = %#v, %v", got, err)
	}
}
