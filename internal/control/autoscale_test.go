package control

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/ingress"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/network"
	"github.com/TorstenDittmann/orch/internal/store"
)

func TestRecommendReplicas(t *testing.T) {
	t.Parallel()
	bounds := config.Autoscale{Min: 2, Max: 10, CPUPercent: 70}
	for _, test := range []struct {
		name        string
		current     int
		utilization float64
		want        int
	}{
		{"within tolerance", 4, 0.75, 4},
		{"scale up", 4, 1.0, 6},
		{"at most doubles", 2, 5.0, 4},
		{"stays at max", 9, 1.0, 10},
		{"scale down", 4, 0.2, 2},
		{"stays at min", 3, 0.0, 2},
	} {
		if got := recommendReplicas(test.current, test.utilization, 0.7, bounds); got != test.want {
			t.Errorf("%s: recommendReplicas(%d, %v) = %d, want %d", test.name, test.current, test.utilization, got, test.want)
		}
	}
}

func TestApplyServiceScalesRewritesOnlyAutoscaledServices(t *testing.T) {
	t.Parallel()
	manifest := reconcilerServicesManifest("one")
	api := manifest.Services["api"]
	api.Replicas, api.Autoscale = 1, &config.Autoscale{Min: 1, Max: 5, CPUPercent: 70}
	manifest.Services["api"] = api
	canonical := mustCanonicalManifest(t, manifest)
	applications := []model.Application{{Name: "hello", CanonicalManifest: canonical}}
	scales := map[string]map[string]store.ServiceScale{"hello": {"api": {Replicas: 9}}}
	if err := applyServiceScales(applications, scales); err != nil {
		t.Fatal(err)
	}
	var rewritten config.Manifest
	if err := json.Unmarshal(applications[0].CanonicalManifest, &rewritten); err != nil {
		t.Fatal(err)
	}
	// The stored count is clamped to the current maximum.
	if got := rewritten.Services["api"]; got.Replicas != 5 || got.Autoscale != nil {
		t.Fatalf("api = %#v", got)
	}
	if got := rewritten.Services["worker"]; got.Replicas != 1 {
		t.Fatalf("worker = %#v", got)
	}
	if err := config.Validate(&rewritten); err != nil {
		t.Fatalf("rewritten manifest is invalid: %v", err)
	}
}

func TestAutoscalerScalesUpAndWaitsBeforeScalingDown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	if err := state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{
		NodeID: "node-one", BootID: "boot-one", OperatingSystem: "linux", Architecture: "amd64",
		RuntimeVersion: "containerd 2.3.4", Snapshotter: "overlayfs", CgroupsV2: true,
		CNIVersion: network.CNISpecVersion, CNIPluginsVersion: network.CNIPluginsVersion,
		Nftables: true, DNSAvailable: true, CaddyVersion: ingress.RequiredCaddyVersion,
		TotalCPUMillis: 1000, TotalMemoryBytes: 4 << 30, AllocatableCPUMillis: 300, AllocatableMemoryBytes: 2 << 30,
		SeenAt: reconcilerTestTime.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	manifest := runtimeManifest(executableImage, func(service *config.Service) {
		service.Autoscale = &config.Autoscale{Min: 1, Max: 5, CPUPercent: 50}
	})
	applyManifest(t, state, manifest, "revision-one", "autoscale-apply-key", reconcilerTestTime)

	now := reconcilerTestTime.Add(2 * time.Minute)
	clock := func() time.Time { return now }
	reconciler := NewReconciler(state, cluster, clock, nil)
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
	if err != nil || len(assignment.Allocations) != 1 {
		t.Fatalf("first assignment = %#v, %v", assignment.Allocations, err)
	}
	sequence := uint64(2)
	ready := func() model.Assignment {
		t.Helper()
		assignment, err := state.CurrentAssignment(ctx, "node-one")
		if err != nil {
			t.Fatal(err)
		}
		recordReadyAssignment(t, state, assignment, sequence, now)
		sequence++
		if err := reconciler.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		return assignment
	}
	ready()

	scaler := newAutoscaler(state, reconciler.Reconcile, func(string) bool { return true }, clock)
	tick := func(millis uint64) {
		t.Helper()
		assignment, err := state.CurrentAssignment(ctx, "node-one")
		if err != nil {
			t.Fatal(err)
		}
		// Heartbeats keep the node reachable while the clock moves.
		if err := state.TouchAgent(ctx, "node-one", "boot-one", now); err != nil {
			t.Fatal(err)
		}
		for _, allocation := range assignment.Allocations {
			scaler.Record(allocation.ID, millis, now)
		}
		if err := scaler.evaluate(ctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(autoscaleInterval)
	}
	replicas := func() int {
		t.Helper()
		assignment, err := state.CurrentAssignment(ctx, "node-one")
		if err != nil {
			t.Fatal(err)
		}
		return len(assignment.Allocations)
	}

	// One replica at 100m of a 100m request is twice the 50% target.
	tick(100)
	if got := replicas(); got != 2 {
		t.Fatalf("replicas after load = %d, want 2", got)
	}
	ready()
	events, err := state.Events(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	scaled := false
	for _, event := range events {
		scaled = scaled || event.Kind == model.EventServiceScaled
	}
	if !scaled {
		t.Fatal("scale-up recorded no service.scaled event")
	}

	// Still overloaded, but the node has room for only one more 100m replica.
	now = now.Add(autoscaleWindow)
	tick(100)
	if got := replicas(); got != 3 {
		t.Fatalf("replicas at capacity = %d, want 3", got)
	}
	ready()
	application, err := state.Application(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if status := scaler.Status(application, nil)["worker"]; status.Limited == "" {
		t.Fatalf("status at capacity = %#v, want limited", status)
	}

	// Idle replicas lose one replica at a time, and only after the delay.
	for elapsed := time.Duration(0); elapsed < autoscaleDownDelay-autoscaleInterval; elapsed += autoscaleInterval {
		tick(5)
		if got := replicas(); got != 3 {
			t.Fatalf("replicas %s into the scale-down delay = %d, want 3", elapsed, got)
		}
	}
	for range 3 {
		tick(5)
	}
	if got := replicas(); got != 2 {
		t.Fatalf("replicas after the scale-down delay = %d, want 2", got)
	}
}
