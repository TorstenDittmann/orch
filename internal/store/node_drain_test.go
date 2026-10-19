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
