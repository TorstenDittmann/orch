package control

import (
	"context"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
)

func TestDistributedReconcilerClearsResolvedVolumeBlocker(t *testing.T) {
	for _, reason := range []string{"volume_node_lost", "local_volume_pinned"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			state, reconciler, now := readyDistributedCluster(t, map[string]map[string]string{
				"node-one": {}, "node-two": {"disk": "local"},
			})
			manifest := newTestManifest("stateful", map[string]config.Service{
				"worker": {Image: executableImage, Constraints: map[string]string{"disk": "local"},
					Mounts: []config.Mount{{Source: "data", Target: "/data"}}},
			})
			manifest.Volumes = map[string]config.Volume{"data": {Driver: "local"}}
			applyManifest(t, state, manifest, "revision-one", "application-key-one", *now)
			*now = now.Add(time.Second)
			if err := reconciler.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			original, err := state.CurrentAssignment(ctx, "node-two")
			if err != nil {
				t.Fatal(err)
			}
			originalAllocation := allocationForService(t, original, "worker", 1)

			switch reason {
			case "volume_node_lost":
				*now = now.Add(distributedLossGrace + time.Second)
				if err := state.TouchAgent(ctx, "node-one", "boot-node-one", *now); err != nil {
					t.Fatal(err)
				}
			case "local_volume_pinned":
				if err := state.SetNodeLifecycle(ctx, "node-two", model.NodeLifecycleDraining, *now); err != nil {
					t.Fatal(err)
				}
			}
			// Repeated reconciles must continue to report a real blocker.
			for range 2 {
				if err := reconciler.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				application, err := state.Application(ctx, "stateful")
				if err != nil {
					t.Fatal(err)
				}
				if application.Execution.Status != model.ExecutionBlocked || application.Execution.Reason != reason {
					t.Fatalf("blocked execution = %#v", application.Execution)
				}
			}
			if err := state.SetNodeLifecycle(ctx, "node-two", model.NodeLifecycleActive, *now); err != nil {
				t.Fatal(err)
			}
			if err := state.TouchAgent(ctx, "node-two", "boot-node-two", *now); err != nil {
				t.Fatal(err)
			}
			if err := reconciler.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			assignment, err := state.CurrentAssignment(ctx, "node-two")
			if err != nil {
				t.Fatal(err)
			}
			allocation := allocationForService(t, assignment, "worker", 1)
			if allocation.ID != originalAllocation.ID || allocation.SpecDigest != originalAllocation.SpecDigest {
				t.Fatal("recovery replaced the existing allocation")
			}
			if claim, err := state.VolumeClaim(ctx, "stateful", "data"); err != nil || claim != "node-two" {
				t.Fatalf("volume claim changed: %q, %v", claim, err)
			}
			if err := state.AcknowledgeAssignment(ctx, "node-two", assignment.Generation, assignment.Digest); err != nil {
				t.Fatal(err)
			}
			ingressState := model.SubsystemNotConfigured
			if assignment.Network.IngressEnabled {
				ingressState = model.SubsystemReady
			}
			recorded, err := state.RecordAgentReport(ctx, model.AgentReport{
				NodeID: "node-two", BootID: "boot-node-two", Sequence: 2,
				AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest,
				Network: model.NetworkStatus{Generation: assignment.Network.Generation, State: model.NetworkReady,
					ClusterCIDR: assignment.Network.ClusterCIDR, NodeSubnet: assignment.Network.NodeSubnet,
					Gateway: assignment.Network.Gateway, DNSState: model.SubsystemReady, IngressState: ingressState},
				Allocations: []model.AllocationStatus{{
					ID: allocation.ID, Application: allocation.Application, Service: allocation.Service, Replica: allocation.Replica,
					ApplicationGeneration: allocation.ApplicationGeneration, SpecDigest: allocation.SpecDigest,
					Phase: model.AllocationRunning, HealthStatus: model.HealthNotConfigured, Address: "10.88.1.2",
					ActiveSpecDigest: allocation.SpecDigest, ActiveGeneration: allocation.ApplicationGeneration,
					ActiveRevisionID: allocation.RevisionID, UpdatedAt: *now,
				}},
				ReportedAt: *now,
			})
			if err != nil || !recorded {
				t.Fatalf("ready report = %t, %v", recorded, err)
			}
			application, err := state.Application(ctx, "stateful")
			if err != nil {
				t.Fatal(err)
			}
			if application.Execution.Status != model.ExecutionRunning || application.Execution.ActiveGeneration != 1 {
				t.Fatalf("recovered execution = %#v", application.Execution)
			}
		})
	}
}
