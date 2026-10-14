package control

import (
	"context"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/store"
)

// readyDistributedCluster enrolls node-one and node-two with the given labels,
// publishes their preflight mesh assignments, and records ready reports.
func readyDistributedCluster(t *testing.T, labels map[string]map[string]string) (*store.Store, *Reconciler, *time.Time) {
	t.Helper()
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	enrollDistributedNode(t, state, "node-one", "node-1", "10.0.0.1", 'a', '1')
	enrollDistributedNode(t, state, "node-two", "node-2", "10.0.0.2", 'b', '2')
	for nodeID, nodeLabels := range labels {
		if err := state.UpdateNodeLabels(ctx, nodeID, nodeLabels); err != nil {
			t.Fatal(err)
		}
	}
	now := reconcilerTestTime.Add(2 * time.Minute)
	reconciler := NewReconciler(state, cluster, func() time.Time { return now }, nil)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for index, nodeID := range []string{"node-one", "node-two"} {
		assignment, err := state.CurrentAssignment(ctx, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if err := state.AcknowledgeAssignment(ctx, nodeID, assignment.Generation, assignment.Digest); err != nil {
			t.Fatal(err)
		}
		ingressState := model.SubsystemNotConfigured
		if assignment.Network.IngressEnabled {
			ingressState = model.SubsystemReady
		}
		recorded, err := state.RecordAgentReport(ctx, model.AgentReport{
			NodeID: nodeID, BootID: []string{"boot-node-one", "boot-node-two"}[index], Sequence: 1,
			AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest,
			Network: model.NetworkStatus{Generation: assignment.Network.Generation, State: model.NetworkReady,
				ClusterCIDR: assignment.Network.ClusterCIDR, NodeSubnet: assignment.Network.NodeSubnet,
				Gateway: assignment.Network.Gateway, DNSState: model.SubsystemReady, IngressState: ingressState},
			ReportedAt: now,
		})
		if err != nil || !recorded {
			t.Fatalf("ready report %s = %t, %v", nodeID, recorded, err)
		}
	}
	return state, reconciler, &now
}

func TestDistributedCronJobRunsOnItsReadOnlyVolumeNode(t *testing.T) {
	ctx := context.Background()
	state, reconciler, now := readyDistributedCluster(t, map[string]map[string]string{
		"node-one": {"role": "web"}, "node-two": {"role": "web", "disk": "local"},
	})
	manifest := newTestManifest("nmv", map[string]config.Service{
		"db": {
			Image: executableImage, Constraints: map[string]string{"disk": "local"},
			Mounts: []config.Mount{{Source: "sqld", Target: "/var/lib/sqld"}},
		},
	})
	manifest.Volumes = map[string]config.Volume{"sqld": {Driver: "local"}}
	manifest.CronJobs = map[string]config.CronJob{"db-backup": {
		Image: executableImage, Schedule: "0 3 * * *", Resources: config.Resources{CPU: "100m", Memory: "64Mi"},
		Constraints: map[string]string{"role": "web"}, StartDeadline: "30s", Timeout: "5m",
		Mounts: []config.Mount{{Source: "sqld", Target: "/sqld", ReadOnly: true}},
	}}
	applyManifest(t, state, manifest, "revision-one", "application-key-one", *now)

	*now = now.Add(time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []string{"node-one", "node-two"} {
		assignment, err := state.CurrentAssignment(ctx, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if len(assignment.CronJobs) != 0 {
			t.Fatalf("cron job placed on %s before its volume was claimed", nodeID)
		}
	}
	if claim, err := state.VolumeClaim(ctx, "nmv", "sqld"); err != nil || claim != "node-two" {
		t.Fatalf("volume claim = %q, %v", claim, err)
	}

	*now = now.Add(time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	one, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	two, err := state.CurrentAssignment(ctx, "node-two")
	if err != nil {
		t.Fatal(err)
	}
	if len(one.CronJobs) != 0 || len(two.CronJobs) != 1 {
		t.Fatalf("cron placement = node-one %#v node-two %#v", one.CronJobs, two.CronJobs)
	}
	job := two.CronJobs[0]
	if len(job.Mounts) != 1 || job.Mounts[0] != (model.VolumeMount{Name: "sqld", Target: "/sqld", ReadOnly: true}) {
		t.Fatalf("cron mounts = %#v", job.Mounts)
	}
}
