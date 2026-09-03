package control

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/ingress"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/network"
	"change.codes/torsten/better-swarm/internal/security"
	"change.codes/torsten/better-swarm/internal/store"
)

const executableImage = "docker.io/library/busybox@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestReconcilerLeavesApplicationsPendingWithoutAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	applyManifest(t, state, runtimeManifest(executableImage), "revision-one", "application-key-one", reconcilerTestTime)
	reconciler := NewReconciler(state, cluster, func() time.Time { return reconcilerTestTime.Add(time.Minute) }, nil)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	application, err := state.Application(ctx, "hello")
	if err != nil || application.Execution.Status != model.ExecutionPending || application.Execution.Reason != "agent_unavailable" {
		t.Fatalf("application = %#v, %v", application.Execution, err)
	}
}

func TestReconcilerPublishesTCPDependenciesAndCronTemplates(t *testing.T) {
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	port := 9418
	manifest := newTestManifest("hello", map[string]config.Service{
		"gitd": {Image: executableImage, TCP: &config.TCP{Port: 9418, PublicPort: &port}},
		"web":  {Image: executableImage, DependsOn: []string{"gitd"}},
	})
	manifest.CronJobs = map[string]config.CronJob{"backup": {
		Image: executableImage, Schedule: "*/2 * * * *", Resources: config.Resources{CPU: "100m", Memory: "64Mi"},
		DependsOn: []string{"gitd"}, StartDeadline: "30s", Timeout: "5m",
	}}
	applyManifest(t, state, manifest, "revision-one", "application-key-one", reconcilerTestTime)
	reconciler := NewReconciler(state, cluster, func() time.Time { return reconcilerTestTime.Add(2 * time.Minute) }, nil)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	preflight, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	freezeReconcilerNetwork(t, state, preflight)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assignment, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	gitd := allocationForService(t, assignment, "gitd", 1)
	web := allocationForService(t, assignment, "web", 1)
	if gitd.TCP == nil || gitd.TCP.Port != 9418 || gitd.TCP.PublicPort != 9418 || len(web.DependsOn) != 1 || web.DependsOn[0] != "gitd" {
		t.Fatalf("published services = gitd %#v web %#v", gitd, web)
	}
	if len(assignment.CronJobs) != 1 || assignment.CronJobs[0].Job != "backup" || assignment.CronJobs[0].TimeoutSeconds != 300 || assignment.CronJobs[0].DependsOn[0] != "gitd" {
		t.Fatalf("published cron jobs = %#v", assignment.CronJobs)
	}
}

func TestTCPPublicPortOwnershipBlocksLaterApplication(t *testing.T) {
	port := 9418
	firstManifest := newTestManifest("first", map[string]config.Service{"git": {Image: executableImage, TCP: &config.TCP{Port: 9418, PublicPort: &port}}})
	secondPort := 9418
	secondManifest := newTestManifest("second", map[string]config.Service{"git": {Image: executableImage, TCP: &config.TCP{Port: 9418, PublicPort: &secondPort}}})
	applications := []model.Application{
		{Name: "second", Generation: 1, RevisionID: "revision-two", CanonicalManifest: mustCanonicalManifest(t, secondManifest), CreatedAt: reconcilerTestTime.Add(time.Second)},
		{Name: "first", Generation: 1, RevisionID: "revision-one", CanonicalManifest: mustCanonicalManifest(t, firstManifest), CreatedAt: reconcilerTestTime},
	}
	owners, err := desiredTCPPortOwners(applications)
	if err != nil || owners[9418] != "first/git" {
		t.Fatalf("owners = %#v, %v", owners, err)
	}
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	node := model.Node{OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.4",
		Snapshotter: "overlayfs", CgroupsV2: true, CNIVersion: network.CNISpecVersion,
		CNIPluginsVersion: network.CNIPluginsVersion, Nftables: true, DNSAvailable: true,
		CaddyVersion: ingress.RequiredCaddyVersion, NetworkGeneration: 1, NodeSubnet: networkConfig.NodeSubnet,
		NetworkState: model.NetworkReady, DNSState: model.SubsystemReady}
	_, blocked, err := planApplicationBundleForNetwork("cluster-one", node, applications[0], networkConfig, true, nil, owners, nil)
	if err != nil || blocked == nil || blocked.reason != "tcp_port_conflict" {
		t.Fatalf("blocked = %#v, %v", blocked, err)
	}
}

func TestReconcilerPlansDeterministicAllocationsAndPreservesUnaffectedSpecs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	firstManifest := reconcilerServicesManifest("one")
	applyManifest(t, state, firstManifest, "revision-one", "application-key-one", reconcilerTestTime)
	var published []model.Assignment
	reconciler := NewReconciler(state, cluster, func() time.Time { return reconcilerTestTime.Add(2 * time.Minute) }, func(assignment model.Assignment) {
		published = append(published, assignment)
	})
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	preflight, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || preflight.Generation != 1 || len(preflight.Allocations) != 0 || len(published) != 1 {
		t.Fatalf("network preflight assignment = %#v, %v; callbacks %d", preflight, err, len(published))
	}
	application, err := state.Application(ctx, "hello")
	if err != nil || application.Execution.Status != model.ExecutionPending || application.Execution.Reason != "network_initializing" {
		t.Fatalf("network-gated application = %#v, %v", application.Execution, err)
	}
	freezeReconcilerNetwork(t, state, preflight)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || first.Generation != 2 || len(first.Allocations) != 3 || len(published) != 2 {
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
	if nodeAfter.AssignmentGeneration != nodeBefore.AssignmentGeneration || len(eventsAfter) != len(eventsBefore) || len(published) != 2 {
		t.Fatalf("unchanged reconcile advanced state: node %#v -> %#v, events %d -> %d, callbacks %d", nodeBefore, nodeAfter, len(eventsBefore), len(eventsAfter), len(published))
	}

	updatedManifest := reconcilerServicesManifest("two")
	applyManifest(t, state, updatedManifest, "revision-two", "application-key-two", reconcilerTestTime.Add(3*time.Minute))
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || second.Generation != 3 || len(second.Allocations) != 3 {
		t.Fatalf("second assignment = %#v, %v", second, err)
	}
	workerAfter := allocationForService(t, second, "worker", 1)
	apiAfter := allocationForService(t, second, "api", 1)
	if workerAfter.ID != workerBefore.ID || workerAfter.SpecDigest != workerBefore.SpecDigest {
		t.Fatalf("unaffected worker changed: %#v -> %#v", workerBefore, workerAfter)
	}
	if workerAfter.Rollback == nil || workerAfter.Rollback.SpecDigest != workerAfter.SpecDigest ||
		workerAfter.Rollback.ApplicationGeneration != 1 || workerAfter.Rollback.RevisionID != "revision-one" {
		t.Fatalf("unaffected worker lacks application rollback authority: %#v", workerAfter)
	}
	if apiAfter.ID != apiBefore.ID || apiAfter.SpecDigest == apiBefore.SpecDigest {
		t.Fatalf("affected API identity/spec = %#v -> %#v", apiBefore, apiAfter)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	stable, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || stable.Generation != second.Generation ||
		allocationForService(t, stable, "worker", 1).Rollback == nil {
		t.Fatalf("stable rollback authorization = %#v, %v", stable, err)
	}
}

func TestReconcilerPreservesLegacyWorkloadsDuringNetworkPreflight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	applyManifest(t, state, runtimeManifest(executableImage), "revision-one", "application-key-one", reconcilerTestTime)
	application, err := state.Application(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	legacyNode := model.Node{
		OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.4",
		Snapshotter: "overlayfs", CgroupsV2: true, CNIVersion: network.CNISpecVersion,
		CNIPluginsVersion: network.CNIPluginsVersion, Nftables: true, DNSAvailable: true,
		CaddyVersion:      ingress.RequiredCaddyVersion,
		NetworkGeneration: 1, NodeSubnet: "10.88.0.0/24", NetworkState: model.NetworkReady,
		DNSState: model.SubsystemReady, IngressState: model.SubsystemReady,
	}
	legacy, blocked, err := planApplication("cluster-one", legacyNode, application)
	if err != nil || blocked != nil || len(legacy) != 1 {
		t.Fatalf("legacy plan = %#v, %#v, %v", legacy, blocked, err)
	}
	legacy[0].NetworkGeneration = 0
	legacy[0].MaxSurge = 0
	legacy[0].MaxUnavailable = 0
	legacy[0].Rollback = nil
	legacy[0].SpecDigest, err = model.Plan4AllocationSpecDigest(legacy[0])
	if err != nil {
		t.Fatal(err)
	}
	payload, digest, err := model.AssignmentPayload(model.NetworkConfig{}, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := state.PublishAssignment(ctx, model.PublishAssignment{
		NodeID: "node-one", Digest: digest, Payload: payload, Allocations: legacy,
		CreatedAt: reconcilerTestTime.Add(time.Minute),
	}); err != nil || !changed {
		t.Fatalf("publish legacy assignment = %t, %v", changed, err)
	}

	reconciler := NewReconciler(state, cluster, func() time.Time { return reconcilerTestTime.Add(2 * time.Minute) }, nil)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	preflight, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || preflight.Generation != 2 || preflight.Network.Generation != 1 || len(preflight.Allocations) != 1 ||
		preflight.Allocations[0].SpecDigest != legacy[0].SpecDigest || preflight.Allocations[0].NetworkGeneration != 0 {
		t.Fatalf("network preflight assignment = %#v, %v", preflight, err)
	}
	application, err = state.Application(ctx, "hello")
	if err != nil || application.Execution.Reason != "network_initializing" {
		t.Fatalf("preflight execution = %#v, %v", application.Execution, err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", preflight.Generation, preflight.Digest); err != nil {
		t.Fatal(err)
	}
	reportedAt := reconcilerTestTime.Add(3 * time.Minute)
	recorded, err := state.RecordAgentReport(ctx, model.AgentReport{
		NodeID: "node-one", BootID: "boot-one", Sequence: 1,
		AssignmentGeneration: preflight.Generation, AssignmentDigest: preflight.Digest,
		Allocations: []model.AllocationStatus{{
			ID: legacy[0].ID, Application: legacy[0].Application, Service: legacy[0].Service,
			Replica: legacy[0].Replica, ApplicationGeneration: legacy[0].ApplicationGeneration,
			SpecDigest: legacy[0].SpecDigest, Phase: model.AllocationRunning,
			HealthStatus: model.HealthNotConfigured, ActiveSpecDigest: legacy[0].SpecDigest,
			ActiveGeneration: legacy[0].ApplicationGeneration, ActiveRevisionID: legacy[0].RevisionID,
			UpdatedAt: reportedAt,
		}},
		Network: model.NetworkStatus{
			Generation: preflight.Network.Generation, State: model.NetworkReady,
			ClusterCIDR: preflight.Network.ClusterCIDR, NodeSubnet: preflight.Network.NodeSubnet,
			Gateway: preflight.Network.Gateway, DNSState: model.SubsystemReady,
			IngressState: model.SubsystemReady,
		},
		ReportedAt: reportedAt,
	})
	if err != nil || !recorded {
		t.Fatalf("record legacy preflight report = %t, %v", recorded, err)
	}
	frozen, err := state.Cluster(ctx)
	if err != nil || frozen.NetworkFrozenAt == nil {
		t.Fatalf("frozen cluster = %#v, %v", frozen, err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	networked, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || networked.Generation != 3 || len(networked.Allocations) != 1 {
		t.Fatalf("networked replacement assignment = %#v, %v", networked, err)
	}
	replacement := networked.Allocations[0]
	if replacement.NetworkGeneration != preflight.Network.Generation || replacement.SpecDigest == legacy[0].SpecDigest ||
		replacement.Rollback == nil || replacement.Rollback.SpecDigest != legacy[0].SpecDigest ||
		replacement.Rollback.NetworkGeneration != 0 || replacement.Rollback.ApplicationGeneration != legacy[0].ApplicationGeneration ||
		replacement.Rollback.RevisionID != legacy[0].RevisionID {
		t.Fatalf("networked replacement does not authorize the exact legacy rollback: %#v", replacement)
	}
}

func TestReconcilerPreservesAssignedWorkloadsWhenNodeCapabilityRegresses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	applyManifest(t, state, runtimeManifest(executableImage), "revision-one", "application-key-one", reconcilerTestTime)
	reconciler := NewReconciler(state, cluster, func() time.Time { return reconcilerTestTime.Add(2 * time.Minute) }, nil)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	preflight, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	freezeReconcilerNetwork(t, state, preflight)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	serving, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || len(serving.Allocations) != 1 {
		t.Fatalf("serving assignment = %#v, %v", serving, err)
	}
	if err := state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{
		NodeID: "node-one", BootID: "boot-two", OperatingSystem: "linux", Architecture: "amd64",
		RuntimeVersion: "containerd 2.3.4", Snapshotter: "overlayfs", CgroupsV2: true,
		CNIVersion: network.CNISpecVersion, CNIPluginsVersion: network.CNIPluginsVersion,
		Nftables: true, DNSAvailable: true, SeenAt: reconcilerTestTime.Add(3 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	preserved, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || preserved.Generation != serving.Generation || len(preserved.Allocations) != 1 ||
		preserved.Allocations[0].SpecDigest != serving.Allocations[0].SpecDigest {
		t.Fatalf("capability-blocked assignment = %#v, %v; want unchanged generation %d", preserved, err, serving.Generation)
	}
	application, err := state.Application(ctx, "hello")
	if err != nil || application.Execution.Status != model.ExecutionBlocked || application.Execution.Reason != "caddy_unavailable" {
		t.Fatalf("capability-blocked execution = %#v, %v", application.Execution, err)
	}
}

func TestReconcilerBlocksUnsupportedIntentWithStableReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		manifest *config.Manifest
		reason   string
	}{
		{name: "tag image", manifest: runtimeManifest("docker.io/library/busybox:latest"), reason: "unsupported_image_tag"},
		{name: "secret", manifest: runtimeManifest(executableImage, func(service *config.Service) {
			service.SecretEnv = map[string]string{"TOKEN": "token"}
		}), reason: "secrets_unsupported"},
		{name: "constraint", manifest: runtimeManifest(executableImage, func(service *config.Service) {
			service.Constraints = map[string]string{"region": "local"}
		}), reason: "constraints_unsupported"},
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

func TestReconcilerBlocksMissingSecretAndRollsReferencingWorkloadOnRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	applyManifest(t, state, runtimeManifest(executableImage, func(service *config.Service) {
		service.SecretEnv = map[string]string{"TOKEN": "token"}
	}), "revision-one", "application-key-one", reconcilerTestTime)
	box, err := security.NewSecretBox(bytes.Repeat([]byte{1}, security.SecretKeySize), bytes.NewReader(bytes.Repeat([]byte{2}, 4096)))
	if err != nil {
		t.Fatal(err)
	}
	reconciler := NewReconciler(state, cluster, func() time.Time { return reconcilerTestTime.Add(time.Minute) }, nil)
	reconciler.secretBox = box
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	application, err := state.Application(ctx, "hello")
	if err != nil || application.Execution.Reason != "secret_missing" {
		t.Fatalf("missing-secret execution = %#v, %v", application.Execution, err)
	}
	preflight, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	freezeReconcilerNetwork(t, state, preflight)
	if _, err := state.SetApplicationSecret(ctx, box, "hello", "token", "secv_one", []byte("first"), "secret-key-one-0001", reconcilerTestTime.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || len(first.Allocations) != 1 || first.Allocations[0].SecretEnvironment["TOKEN"].VersionID != "secv_one" {
		t.Fatalf("first secret assignment = %#v, %v", first, err)
	}
	hydrated, err := reconciler.HydrateAssignment(ctx, first)
	if err != nil || len(hydrated.Materials) != 1 || string(hydrated.Materials[0].Value) != "first" {
		t.Fatalf("hydrated assignment = %#v, %v", hydrated.Materials, err)
	}
	firstDigest := first.Allocations[0].SpecDigest
	if _, err := state.SetApplicationSecret(ctx, box, "hello", "token", "secv_two", []byte("second"), "secret-key-two-0002", reconcilerTestTime.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || len(second.Allocations) != 1 || second.Allocations[0].SpecDigest == firstDigest ||
		second.Allocations[0].SecretEnvironment["TOKEN"].VersionID != "secv_two" {
		t.Fatalf("rotated secret assignment = %#v, %v", second, err)
	}
}

func TestRegistryCredentialRotationUpdatesPullAuthorizationWithoutRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	box, err := security.NewSecretBox(bytes.Repeat([]byte{3}, security.SecretKeySize), bytes.NewReader(bytes.Repeat([]byte{4}, 4096)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetRegistryCredential(ctx, box, "registry.example:5443", "robot", "regv_one", []byte("first"), "registry-key-one-01", reconcilerTestTime); err != nil {
		t.Fatal(err)
	}
	image := "registry.example:5443/team/app@sha256:" + strings.Repeat("b", 64)
	applyManifest(t, state, runtimeManifest(image), "revision-one", "application-key-one", reconcilerTestTime)
	reconciler := NewReconciler(state, cluster, func() time.Time { return reconcilerTestTime.Add(time.Minute) }, nil)
	reconciler.secretBox = box
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	preflight, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	freezeReconcilerNetwork(t, state, preflight)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || len(first.Allocations) != 1 || first.Allocations[0].RegistryCredential == nil || first.Allocations[0].RegistryCredential.VersionID != "regv_one" {
		t.Fatalf("first registry assignment = %#v, %v", first, err)
	}
	if _, err := state.SetRegistryCredential(ctx, box, "registry.example:5443", "robot", "regv_two", []byte("second"), "registry-key-two-02", reconcilerTestTime.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || len(second.Allocations) != 1 || second.Allocations[0].RegistryCredential.VersionID != "regv_two" ||
		second.Allocations[0].SpecDigest != first.Allocations[0].SpecDigest || second.Generation <= first.Generation {
		t.Fatalf("rotated registry assignment = %#v, %v", second, err)
	}
}

func TestNodeCapabilityBlockersAreExplicit(t *testing.T) {
	t.Parallel()
	application := model.Application{Name: "hello", Generation: 1, RevisionID: "revision-one", CanonicalManifest: mustCanonicalManifest(t, runtimeManifest(executableImage))}
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
		{name: "CNI", node: model.Node{OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.4", Snapshotter: "overlayfs", CgroupsV2: true}, status: model.ExecutionBlocked, reason: "cni_unavailable"},
		{name: "CNI plugins", node: model.Node{OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.4", Snapshotter: "overlayfs", CgroupsV2: true, CNIVersion: network.CNISpecVersion}, status: model.ExecutionBlocked, reason: "cni_plugins_unavailable"},
		{name: "nftables", node: model.Node{OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.4", Snapshotter: "overlayfs", CgroupsV2: true, CNIVersion: network.CNISpecVersion, CNIPluginsVersion: network.CNIPluginsVersion}, status: model.ExecutionBlocked, reason: "nftables_required"},
		{name: "DNS", node: model.Node{OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.4", Snapshotter: "overlayfs", CgroupsV2: true, CNIVersion: network.CNISpecVersion, CNIPluginsVersion: network.CNIPluginsVersion, Nftables: true}, status: model.ExecutionBlocked, reason: "dns_unavailable"},
		{name: "Caddy", node: model.Node{OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.4", Snapshotter: "overlayfs", CgroupsV2: true, CNIVersion: network.CNISpecVersion, CNIPluginsVersion: network.CNIPluginsVersion, Nftables: true, DNSAvailable: true}, status: model.ExecutionBlocked, reason: "caddy_unavailable"},
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
	applyManifest(t, state, runtimeManifest(executableImage), "revision-one", "application-key-one", reconcilerTestTime)
	reconciler := NewReconciler(state, cluster, func() time.Time { return reconcilerTestTime.Add(time.Minute) }, nil)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	preflight, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil || len(preflight.Allocations) != 0 {
		t.Fatalf("network preflight assignment = %#v, %v", preflight, err)
	}
	freezeReconcilerNetwork(t, state, preflight)
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
	if err != nil || assignment.Generation != 3 || len(assignment.Allocations) != 0 {
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
		CNIVersion: network.CNISpecVersion, CNIPluginsVersion: network.CNIPluginsVersion,
		Nftables: true, DNSAvailable: true, CaddyVersion: ingress.RequiredCaddyVersion,
		SeenAt: reconcilerTestTime.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
}

func freezeReconcilerNetwork(t *testing.T, state *store.Store, assignment model.Assignment) {
	t.Helper()
	ctx := context.Background()
	if err := state.AcknowledgeAssignment(ctx, "node-one", assignment.Generation, assignment.Digest); err != nil {
		t.Fatal(err)
	}
	recorded, err := state.RecordAgentReport(ctx, model.AgentReport{
		NodeID: "node-one", BootID: "boot-one", Sequence: 1,
		AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest,
		Network: model.NetworkStatus{
			Generation: assignment.Network.Generation, State: model.NetworkReady,
			ClusterCIDR: assignment.Network.ClusterCIDR, NodeSubnet: assignment.Network.NodeSubnet,
			Gateway: assignment.Network.Gateway, DNSState: model.SubsystemReady,
			IngressState: model.SubsystemReady,
		},
		ReportedAt: reconcilerTestTime.Add(90 * time.Second),
	})
	if err != nil || !recorded {
		t.Fatalf("record ready network = %t, %v", recorded, err)
	}
	cluster, err := state.Cluster(ctx)
	if err != nil || cluster.NetworkFrozenAt == nil {
		t.Fatalf("frozen cluster = %#v, %v", cluster, err)
	}
}

func applyManifest(t *testing.T, state *store.Store, manifest *config.Manifest, revision, key string, now time.Time) {
	t.Helper()
	if err := config.Validate(manifest); err != nil {
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

func mustCanonicalManifest(t *testing.T, manifest *config.Manifest) []byte {
	t.Helper()
	if err := config.Validate(manifest); err != nil {
		t.Fatal(err)
	}
	canonical, err := config.CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func runtimeManifest(image string, mutations ...func(*config.Service)) *config.Manifest {
	service := config.Service{
		Image: image, Command: []string{"/bin/sleep"}, Args: []string{"3600"},
	}
	for _, mutate := range mutations {
		mutate(&service)
	}
	return newTestManifest("hello", map[string]config.Service{"worker": service})
}

func reconcilerServicesManifest(mode string) *config.Manifest {
	return newTestManifest("hello", map[string]config.Service{
		"api": {
			Image: executableImage, Replicas: 2, Env: map[string]string{"MODE": mode},
		},
		"worker": {
			Image: executableImage, Command: []string{"/bin/sleep"}, Args: []string{"3600"},
		},
	})
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
