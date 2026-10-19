package control

import (
	"context"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
)

func TestNodeLocalHTTPRollbackStillRequiresIngress(t *testing.T) {
	allocation := model.AllocationDesired{
		HTTP:     &model.HTTPDesired{Port: 8080},
		Rollback: &model.RollbackSpec{HTTP: &model.HTTPDesired{Port: 8080, NodeLocal: true}},
	}
	if !allocationRequiresIngress(allocation) {
		t.Fatal("a serving node-local fallback must not move to a node without ingress")
	}
	allocation.Rollback = nil
	if allocationRequiresIngress(allocation) {
		t.Fatal("shared HTTP ingress must not constrain the workload's node")
	}
}

func TestNodeLocalHTTPPlacementRequiresAnIngressNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, reconciler, now := readyDistributedCluster(t, nil)
	if err := state.SetNodeIngress(ctx, "node-one", false); err != nil {
		t.Fatal(err)
	}
	if err := state.SetNodeIngress(ctx, "node-two", true); err != nil {
		t.Fatal(err)
	}
	manifest := newTestManifest("local", map[string]config.Service{
		"web": {Image: executableImage,
			HTTP: &config.HTTP{Port: 8080, Domains: []string{"local.example.com"}, HealthPath: "/health", HTTPS: true, NodeLocal: true}},
	})
	applyManifest(t, state, manifest, "revision-one", "application-key-one", *now)
	*now = now.Add(time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, node := range []string{"node-one", "node-two"} {
		assignment, err := state.CurrentAssignment(ctx, node)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if node == "node-two" {
			want = 1
		}
		if len(assignment.Allocations) != want {
			t.Fatalf("%s has %d allocations, want %d", node, len(assignment.Allocations), want)
		}
	}
}

func TestNodeLocalHTTPReachesEndpointCatalogAndWire(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, reconciler, now := readyDistributedCluster(t, map[string]map[string]string{
		"node-one": {"web": "yes"}, "node-two": {},
	})
	manifest := newTestManifest("local", map[string]config.Service{
		"web": {Image: executableImage, Constraints: map[string]string{"web": "yes"},
			HTTP: &config.HTTP{Port: 8080, Domains: []string{"local.example.com"}, HealthPath: "/health", HTTPS: true, NodeLocal: true}},
	})
	applyManifest(t, state, manifest, "revision-one", "application-key-one", *now)
	*now = now.Add(time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assignment, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || len(assignment.Allocations) != 1 {
		t.Fatalf("owner assignment = %#v, %v", assignment, err)
	}
	allocation := assignment.Allocations[0]
	if !allocation.HTTP.NodeLocal || !assignmentSnapshotMessage(assignment).GetAllocations()[0].GetHttp().GetNodeLocal() {
		t.Fatal("node-local HTTP scope was lost planning or encoding the allocation")
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", assignment.Generation, assignment.Digest); err != nil {
		t.Fatal(err)
	}
	recorded, err := state.RecordAgentReport(ctx, model.AgentReport{
		NodeID: "node-one", BootID: "boot-node-one", Sequence: 2,
		AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest,
		Network: model.NetworkStatus{Generation: assignment.Network.Generation, State: model.NetworkReady,
			ClusterCIDR: assignment.Network.ClusterCIDR, NodeSubnet: assignment.Network.NodeSubnet,
			Gateway: assignment.Network.Gateway, DNSState: model.SubsystemReady, IngressState: model.SubsystemReady},
		Allocations: []model.AllocationStatus{{
			ID: allocation.ID, Application: allocation.Application, Service: allocation.Service, Replica: 1,
			ApplicationGeneration: allocation.ApplicationGeneration, SpecDigest: allocation.SpecDigest,
			Phase: model.AllocationRunning, HealthStatus: model.HealthReady, Address: "10.88.0.2",
			ActiveSpecDigest: allocation.SpecDigest, ActiveGeneration: allocation.ApplicationGeneration,
			ActiveRevisionID: allocation.RevisionID, ReadyAt: now, DeclaredPort: 8080, UpdatedAt: *now,
		}}, ReportedAt: *now,
	})
	if err != nil || !recorded {
		t.Fatalf("ready report = %t, %v", recorded, err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	remote, err := state.CurrentAssignment(ctx, "node-two")
	if err != nil {
		t.Fatal(err)
	}
	if len(remote.Network.Endpoints) != 1 || !remote.Network.Endpoints[0].HTTPNodeLocal {
		t.Fatalf("private endpoint catalog lost HTTP scope: %#v", remote.Network.Endpoints)
	}
	if !assignmentSnapshotMessage(remote).GetNetwork().GetEndpoints()[0].GetHttpNodeLocal() {
		t.Fatal("wire endpoint catalog lost node-local HTTP scope")
	}
}
