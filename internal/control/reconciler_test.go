package control

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/store"
)

const executableImage = "docker.io/library/busybox@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestReconcilerLeavesApplicationsPendingWithoutAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	applyManifest(t, state, runtimeManifest(executableImage, ""), "revision-one", "application-key-one", reconcilerTestTime)
	reconciler := NewReconciler(state, cluster, func() time.Time { return reconcilerTestTime.Add(time.Minute) }, nil)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	application, err := state.Application(ctx, "hello")
	if err != nil || application.Execution.Status != model.ExecutionPending || application.Execution.Reason != "agent_unavailable" {
		t.Fatalf("application = %#v, %v", application.Execution, err)
	}
}

func TestReconcilerPlansDeterministicAllocationsAndPreservesUnaffectedSpecs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	firstManifest := `schema = "v1alpha1"
name = "hello"

[services.api]
image = "` + executableImage + `"
replicas = 2

[services.api.env]
MODE = "one"

[services.worker]
image = "` + executableImage + `"
command = ["/bin/sleep"]
args = ["3600"]
`
	applyManifest(t, state, firstManifest, "revision-one", "application-key-one", reconcilerTestTime)
	var published []model.Assignment
	reconciler := NewReconciler(state, cluster, func() time.Time { return reconcilerTestTime.Add(2 * time.Minute) }, func(assignment model.Assignment) {
		published = append(published, assignment)
	})
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || first.Generation != 1 || len(first.Allocations) != 3 || len(published) != 1 {
		t.Fatalf("first assignment = %#v, %v; callbacks %d", first, err, len(published))
	}
	for index, allocation := range first.Allocations {
		if index > 0 && first.Allocations[index-1].ID >= allocation.ID {
			t.Fatal("allocations are not ordered by opaque id")
		}
		if allocation.ApplicationGeneration != 1 || allocation.StartDeadlineSeconds != 120 || !allocation.AutoRollback || !allocation.StartBeforeStop {
			t.Fatalf("allocation = %#v", allocation)
		}
	}
	workerBefore := allocationForService(t, first, "worker", 1)
	apiBefore := allocationForService(t, first, "api", 1)
	nodeBefore, err := state.Node(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := state.EventsAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	nodeAfter, _ := state.Node(ctx)
	eventsAfter, _ := state.EventsAfter(ctx, 0, 100)
	if nodeAfter.AssignmentGeneration != nodeBefore.AssignmentGeneration || len(eventsAfter) != len(eventsBefore) || len(published) != 1 {
		t.Fatalf("unchanged reconcile advanced state: node %#v -> %#v, events %d -> %d, callbacks %d", nodeBefore, nodeAfter, len(eventsBefore), len(eventsAfter), len(published))
	}

	updatedManifest := strings.Replace(firstManifest, `MODE = "one"`, `MODE = "two"`, 1)
	applyManifest(t, state, updatedManifest, "revision-two", "application-key-two", reconcilerTestTime.Add(3*time.Minute))
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || second.Generation != 2 || len(second.Allocations) != 3 {
		t.Fatalf("second assignment = %#v, %v", second, err)
	}
	workerAfter := allocationForService(t, second, "worker", 1)
	apiAfter := allocationForService(t, second, "api", 1)
	if workerAfter.ID != workerBefore.ID || workerAfter.SpecDigest != workerBefore.SpecDigest {
		t.Fatalf("unaffected worker changed: %#v -> %#v", workerBefore, workerAfter)
	}
	if apiAfter.ID != apiBefore.ID || apiAfter.SpecDigest == apiBefore.SpecDigest {
		t.Fatalf("affected API identity/spec = %#v -> %#v", apiBefore, apiAfter)
	}
}

func TestReconcilerBlocksUnsupportedIntentWithStableReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		manifest string
		reason   string
	}{
		{name: "tag image", manifest: runtimeManifest("docker.io/library/busybox:latest", ""), reason: "unsupported_image_tag"},
		{name: "private registry", manifest: runtimeManifest("localhost:5000/example/app@sha256:"+strings.Repeat("a", 64), ""), reason: "private_registry_unsupported"},
		{name: "secret", manifest: runtimeManifest(executableImage, "\n[services.worker.secret_env]\nTOKEN = \"token\"\n"), reason: "secrets_unsupported"},
		{name: "constraint", manifest: runtimeManifest(executableImage, "\n[services.worker.constraints]\nregion = \"local\"\n"), reason: "constraints_unsupported"},
		{name: "HTTP", manifest: runtimeManifest(executableImage, "\n[services.worker.http]\nport = 8080\n"), reason: "http_unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			state, cluster := reconcilerStore(t)
			enrollCompatibleNode(t, state)
			applyManifest(t, state, test.manifest, "revision-one", "application-key-one", reconcilerTestTime)
			reconciler := NewReconciler(state, cluster, func() time.Time { return reconcilerTestTime.Add(time.Minute) }, nil)
			if err := reconciler.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			application, err := state.Application(ctx, "hello")
			if err != nil || application.Execution.Status != model.ExecutionBlocked || application.Execution.Reason != test.reason {
				t.Fatalf("execution = %#v, %v; want reason %q", application.Execution, err, test.reason)
			}
			assignment, err := state.CurrentAssignment(ctx, "node-one")
			if err != nil || len(assignment.Allocations) != 0 {
				t.Fatalf("blocked assignment = %#v, %v", assignment, err)
			}
			generation := assignment.Generation
			if err := reconciler.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			assignment, _ = state.CurrentAssignment(ctx, "node-one")
			if assignment.Generation != generation {
				t.Fatalf("stable blocker advanced assignment to %d", assignment.Generation)
			}
		})
	}
}

func TestNodeCapabilityBlockersAreExplicit(t *testing.T) {
	t.Parallel()
	application := model.Application{Name: "hello", Generation: 1, RevisionID: "revision-one", CanonicalManifest: mustCanonicalManifest(t, runtimeManifest(executableImage, ""))}
	tests := []struct {
		name   string
		node   model.Node
		status string
		reason string
	}{
		{name: "not reported", node: model.Node{}, status: model.ExecutionPending, reason: "agent_capabilities_pending"},
		{name: "platform", node: model.Node{OperatingSystem: "darwin", Architecture: "arm64"}, status: model.ExecutionBlocked, reason: "unsupported_platform"},
		{name: "cgroups", node: model.Node{OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.4", Snapshotter: "overlayfs"}, status: model.ExecutionBlocked, reason: "cgroups_v2_required"},
		{name: "runtime", node: model.Node{OperatingSystem: "linux", Architecture: "amd64", Snapshotter: "overlayfs", CgroupsV2: true}, status: model.ExecutionBlocked, reason: "containerd_unavailable"},
		{name: "runtime version", node: model.Node{OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.5", Snapshotter: "overlayfs", CgroupsV2: true}, status: model.ExecutionBlocked, reason: "unsupported_runtime_version"},
		{name: "snapshotter", node: model.Node{OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.4", CgroupsV2: true}, status: model.ExecutionBlocked, reason: "snapshotter_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, blocked, err := planApplication("cluster-one", test.node, application)
			status := ""
			if blocked != nil {
				status = blocked.status
				if status == "" {
					status = model.ExecutionBlocked
				}
			}
			if err != nil || blocked == nil || status != test.status || blocked.reason != test.reason {
				t.Fatalf("planApplication() blocker = %#v, %v", blocked, err)
			}
		})
	}
}

func TestReconcilerPublishesRemovalTombstone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	applyManifest(t, state, runtimeManifest(executableImage, ""), "revision-one", "application-key-one", reconcilerTestTime)
	reconciler := NewReconciler(state, cluster, func() time.Time { return reconcilerTestTime.Add(time.Minute) }, nil)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := state.RemoveApplication(ctx, "hello", "application-remove-key", reconcilerTestTime.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assignment, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || assignment.Generation != 2 || len(assignment.Allocations) != 0 {
		t.Fatalf("removal assignment = %#v, %v", assignment, err)
	}
	application, err := state.Application(ctx, "hello")
	if err != nil || application.DeletedAt == nil || application.Execution.Status != model.ExecutionStopping || application.Generation != 1 {
		t.Fatalf("removed application = %#v, %v", application, err)
	}
}

var reconcilerTestTime = time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)

func reconcilerStore(t *testing.T) (*store.Store, model.Cluster) {
	t.Helper()
	state, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	cluster := model.Cluster{ID: "cluster-one", Name: "test", Endpoint: "https://controller.example:9443", Generation: 1, CreatedAt: reconcilerTestTime}
	if err := state.InitializeCluster(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	return state, cluster
}

func enrollCompatibleNode(t *testing.T, state *store.Store) {
	t.Helper()
	if _, err := state.EnrollAgent(context.Background(), model.EnrollAgent{
		NodeID: "node-one", Name: "node-1", PublicKeyHash: strings.Repeat("a", 64), CertificateSerial: "17",
		CertificatePEM: []byte("agent certificate"), CertificateNotAfter: reconcilerTestTime.AddDate(1, 0, 0),
		ClusterID: "cluster-one", ControllerEndpoint: "https://controller.example:9443", CACertificatePEM: []byte("CA certificate"),
		IdempotencyKey: "enrollment-key-one", RequestHash: strings.Repeat("1", 64), CreatedAt: reconcilerTestTime.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateAgentCapabilities(context.Background(), model.AgentCapabilities{
		NodeID: "node-one", BootID: "boot-one", OperatingSystem: "linux", Architecture: "amd64",
		RuntimeVersion: "containerd 2.3.4", Snapshotter: "overlayfs", CgroupsV2: true,
		SeenAt: reconcilerTestTime.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
}

func applyManifest(t *testing.T, state *store.Store, source, revision, key string, now time.Time) {
	t.Helper()
	manifest, err := config.Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := config.CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := config.Digest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ApplyApplication(context.Background(), model.ApplyApplication{
		Name: manifest.Name, RevisionID: revision, Digest: digest, CanonicalManifest: canonical,
		IdempotencyKey: key, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func mustCanonicalManifest(t *testing.T, source string) []byte {
	t.Helper()
	manifest, err := config.Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := config.CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func runtimeManifest(image, extra string) string {
	return `schema = "v1alpha1"
name = "hello"

[services.worker]
image = "` + image + `"
command = ["/bin/sleep"]
args = ["3600"]
` + extra
}

func allocationForService(t *testing.T, assignment model.Assignment, service string, replica uint32) model.AllocationDesired {
	t.Helper()
	for _, allocation := range assignment.Allocations {
		if allocation.Service == service && allocation.Replica == replica {
			return allocation
		}
	}
	t.Fatalf("allocation %s/%d not found in %#v", service, replica, assignment.Allocations)
	return model.AllocationDesired{}
}
