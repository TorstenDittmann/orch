package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

func TestNodeMutationsAreAtomicIdempotentAndRequireSafeRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	input := testEnrollAgent("node-one", "node-1", strings.Repeat("a", 64), "enrollment-key-one", strings.Repeat("1", 64))
	if _, err := state.EnrollAgent(ctx, input); err != nil {
		t.Fatal(err)
	}

	update := model.UpdateNode{
		NodeName: "node-1", Labels: map[string]string{"region": "eu-west"},
		SetLifecycle: true, Lifecycle: model.NodeLifecycleDraining,
		SetIngress: true, Ingress: false,
		IdempotencyKey: "node-update-key-one", RequestHash: strings.Repeat("2", 64), UpdatedAt: testCreatedAt.Add(time.Minute),
	}
	first, err := state.UpdateNode(ctx, update)
	if err != nil || !first.Changed || first.EventSequence != 3 || first.Node.Lifecycle != model.NodeLifecycleDraining ||
		first.Node.Labels["region"] != "eu-west" || first.Node.Ingress || len(first.Node.CertificatePEM) != 0 {
		t.Fatalf("UpdateNode() = %#v, %v", first, err)
	}
	replay, err := state.UpdateNode(ctx, update)
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("UpdateNode() replay = %#v, %v; want %#v", replay, err, first)
	}
	conflict := update
	conflict.RequestHash = strings.Repeat("3", 64)
	if _, err := state.UpdateNode(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("UpdateNode() conflict = %v", err)
	}

	remove := model.RemoveNode{
		NodeName: "node-1", IdempotencyKey: "node-remove-key-one", RequestHash: strings.Repeat("4", 64),
		RemovedAt: testCreatedAt.Add(2 * time.Minute),
	}
	if _, err := state.RemoveNode(ctx, remove); err == nil || !strings.Contains(err.Error(), "connected") {
		t.Fatalf("RemoveNode(disconnected) = %v", err)
	}
	remove.Connected = true
	removed, err := state.RemoveNode(ctx, remove)
	if err != nil || !removed.Changed || removed.Name != "node-1" || removed.EventSequence != 4 {
		t.Fatalf("RemoveNode() = %#v, %v", removed, err)
	}
	remove.Connected = false // Transport observations do not alter a durable replay.
	remove.RemovedAt = testCreatedAt.Add(3 * time.Minute)
	removedReplay, err := state.RemoveNode(ctx, remove)
	if err != nil || !reflect.DeepEqual(removedReplay, removed) {
		t.Fatalf("RemoveNode() replay = %#v, %v; want %#v", removedReplay, err, removed)
	}
	if _, err := state.NodeByName(ctx, "node-1"); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("removed NodeByName() = %v", err)
	}
	events, err := state.EventsAfter(ctx, 0, 10)
	if err != nil || len(events) != 4 || events[2].Kind != model.EventNodeUpdated || events[3].Kind != model.EventNodeRemoved {
		t.Fatalf("events = %#v, %v", events, err)
	}
}

func TestAgentCapabilitiesRejectDuplicateMeshIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"node-1", "node-2"} {
		input := testEnrollAgent(fmt.Sprintf("node-%d", index+1), name, fmt.Sprintf("%064x", index+1),
			fmt.Sprintf("enrollment-key-%d", index+1), fmt.Sprintf("%064x", index+101))
		if _, err := state.EnrollAgent(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	key := base64.StdEncoding.EncodeToString(slices.Repeat([]byte{1}, 32))
	first := model.AgentCapabilities{NodeID: "node-1", BootID: "boot-1", SeenAt: testCreatedAt,
		AdvertiseAddress: "10.0.0.1", WireGuardPublicKey: key, WireGuardListenPort: 51820}
	if err := state.UpdateAgentCapabilities(ctx, first); err != nil {
		t.Fatal(err)
	}
	duplicate := first
	duplicate.NodeID = "node-2"
	duplicate.BootID = "boot-2"
	if err := state.UpdateAgentCapabilities(ctx, duplicate); !errors.Is(err, ErrMeshIdentityConflict) {
		t.Fatalf("duplicate mesh identity = %v", err)
	}
}

func TestPlacementAttemptsAreStableUntilAReplicaMoves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"node-1", "node-2"} {
		input := testEnrollAgent(fmt.Sprintf("node-%d", index+1), name, fmt.Sprintf("%064x", index+1),
			fmt.Sprintf("placement-enrollment-%d", index+1), fmt.Sprintf("%064x", index+201))
		if _, err := state.EnrollAgent(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := state.ApplyApplication(ctx, applicationInput(`{"name":"hello"}`, "placement-revision",
		"placement-application-key", testCreatedAt)); err != nil {
		t.Fatal(err)
	}
	first, err := state.EnsurePlacementAttempt(ctx, "hello/web/1", "node-1", "hello", "web", 1,
		"alloc_legacy", "initial_placement", testCreatedAt)
	if err != nil || first != "alloc_legacy" {
		t.Fatalf("first attempt = %q, %v", first, err)
	}
	retained, err := state.EnsurePlacementAttempt(ctx, "hello/web/1", "node-1", "hello", "web", 1,
		"", "retained", testCreatedAt.Add(time.Second))
	if err != nil || retained != first {
		t.Fatalf("retained attempt = %q, %v; want %q", retained, err, first)
	}
	second, err := state.EnsurePlacementAttempt(ctx, "hello/web/1", "node-2", "hello", "web", 1,
		"", "node_loss_replacement", testCreatedAt.Add(2*time.Second))
	if err != nil || second == first {
		t.Fatalf("moved attempt = %q, %v", second, err)
	}
	third, err := state.EnsurePlacementAttempt(ctx, "hello/web/1", "node-1", "hello", "web", 1,
		"", "moved_back", testCreatedAt.Add(3*time.Second))
	if err != nil || third == first || third == second {
		t.Fatalf("returned attempt = %q, %v; prior %q, %q", third, err, first, second)
	}
	if err := state.RetireInactivePlacementAttempts(ctx, nil, testCreatedAt.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	fourth, err := state.EnsurePlacementAttempt(ctx, "hello/web/1", "node-1", "hello", "web", 1,
		"", "recreated", testCreatedAt.Add(5*time.Second))
	if err != nil || fourth == first || fourth == second || fourth == third {
		t.Fatalf("recreated attempt = %q, %v; prior %q, %q, %q", fourth, err, first, second, third)
	}
}

func TestVolumeClaimsAreReleasedWithTheirNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"node-1", "node-2"} {
		input := testEnrollAgent(fmt.Sprintf("node-%d", index+1), name, fmt.Sprintf("%064x", index+1),
			fmt.Sprintf("volume-enrollment-%d", index+1), fmt.Sprintf("%064x", index+301))
		if _, err := state.EnrollAgent(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := state.ApplyApplication(ctx, applicationInput(`{"name":"hello"}`, "volume-revision",
		"volume-application-key", testCreatedAt)); err != nil {
		t.Fatal(err)
	}
	if err := state.ClaimVolume(ctx, "hello", "data", "node-1", testCreatedAt); err != nil {
		t.Fatal(err)
	}
	if err := state.ClaimVolume(ctx, "hello", "data", "node-2", testCreatedAt); err == nil {
		t.Fatal("ClaimVolume() on a second node succeeded")
	}

	if _, err := state.RemoveNode(ctx, model.RemoveNode{NodeName: "node-1", Lost: true,
		IdempotencyKey: "volume-node-remove-key", RequestHash: strings.Repeat("5", 64), RemovedAt: testCreatedAt.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if claim, err := state.VolumeClaim(ctx, "hello", "data"); err != nil || claim != "" {
		t.Fatalf("claim after lost-node removal = %q, %v", claim, err)
	}
	if err := state.ClaimVolume(ctx, "hello", "data", "node-2", testCreatedAt.Add(2*time.Minute)); err != nil {
		t.Fatalf("ClaimVolume() after lost-node removal = %v", err)
	}
}

func TestEnrollmentRejectsTwentyFirstActiveNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxActiveNodes; index++ {
		input := testEnrollAgent(
			fmt.Sprintf("node-%02d", index), fmt.Sprintf("worker-%02d", index), fmt.Sprintf("%064x", index+1),
			fmt.Sprintf("enrollment-key-%02d", index), fmt.Sprintf("%064x", index+101),
		)
		if _, err := state.EnrollAgent(ctx, input); err != nil {
			t.Fatalf("enroll node %d: %v", index+1, err)
		}
	}
	overflow := testEnrollAgent("node-overflow", "worker-overflow", strings.Repeat("f", 64), "enrollment-key-overflow", strings.Repeat("e", 64))
	if _, err := state.EnrollAgent(ctx, overflow); !errors.Is(err, ErrNodeLimitReached) {
		t.Fatalf("twenty-first enrollment = %v", err)
	}
}
