package store

import (
	"context"
	"testing"

	"github.com/TorstenDittmann/orch/internal/model"
)

func TestNodeOSUpdateRestoreIsTakenOnceByANewBoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := executionStore(t)
	if err := state.SetNodeOSUpdateRestore(ctx, "node-one", model.NodeLifecycleActive, "boot-one"); err != nil {
		t.Fatal(err)
	}
	if name, lifecycle, err := state.TakeNodeOSUpdateRestore(ctx, "node-one", "boot-one"); err != nil || lifecycle != "" || name != "" {
		t.Fatalf("same boot took %q %q, %v", name, lifecycle, err)
	}
	if name, lifecycle, err := state.TakeNodeOSUpdateRestore(ctx, "node-one", "boot-two"); err != nil || lifecycle != model.NodeLifecycleActive || name != "node-1" {
		t.Fatalf("new boot took %q %q, %v", name, lifecycle, err)
	}
	if _, lifecycle, err := state.TakeNodeOSUpdateRestore(ctx, "node-one", "boot-three"); err != nil || lifecycle != "" {
		t.Fatalf("second take = %q, %v", lifecycle, err)
	}
	if err := state.SetNodeOSUpdateRestore(ctx, "node-one", model.NodeLifecycleCordoned, "boot-three"); err != nil {
		t.Fatal(err)
	}
	if err := state.SetNodeOSUpdateRestore(ctx, "node-one", "", "boot-three"); err != nil {
		t.Fatal(err)
	}
	if _, lifecycle, err := state.TakeNodeOSUpdateRestore(ctx, "node-one", "boot-four"); err != nil || lifecycle != "" {
		t.Fatalf("cleared restore was taken: %q, %v", lifecycle, err)
	}
	if err := state.SetNodeOSUpdateRestore(ctx, "missing", model.NodeLifecycleActive, "boot"); err != ErrNodeNotFound {
		t.Fatalf("missing node err = %v", err)
	}
}
