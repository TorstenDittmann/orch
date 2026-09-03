package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/agent/state"
	"change.codes/torsten/better-swarm/internal/discovery"
	"change.codes/torsten/better-swarm/internal/health"
	"change.codes/torsten/better-swarm/internal/ingress"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/network"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

var supervisorTestTime = time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)

func TestSupervisorEnsuresAdoptsUpdatesAndRemoves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	now := supervisorTestTime
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Discovery: discovery.NewFake()})
	first := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))

	statuses, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{first}})
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Phase != model.AllocationRunning || statuses[0].RestartCount != 0 {
		t.Fatalf("initial statuses = %#v", statuses)
	}
	firstObjectID := orchruntime.ObjectID(first.ID, first.SpecDigest)
	if operations := runtime.SnapshotOperations(); !reflect.DeepEqual(operations, []string{"ensure:" + firstObjectID}) {
		t.Fatalf("initial operations = %v", operations)
	}
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{first}}); err != nil {
		t.Fatal(err)
	}
	if operations := runtime.SnapshotOperations(); len(operations) != 1 {
		t.Fatalf("idempotent operations = %v", operations)
	}

	updated := first
	updated.Image = "sha256:" + strings.Repeat("b", 64)
	updated.ApplicationGeneration = 2
	updated.RevisionID = "revision-two"
	updated.SpecDigest = mustAllocationDigest(t, updated)
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{updated}}); err != nil {
		t.Fatal(err)
	}
	updatedObjectID := orchruntime.ObjectID(updated.ID, updated.SpecDigest)
	wantSuffix := []string{"ensure:" + updatedObjectID, "stop:" + firstObjectID, "remove:" + firstObjectID}
	operations := runtime.SnapshotOperations()
	if len(operations) < len(wantSuffix) || !reflect.DeepEqual(operations[len(operations)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("stateless replacement operations = %v, want suffix %v", operations, wantSuffix)
	}
	objects := runtime.SnapshotObjects()
	if len(objects) != 1 || !objects[updatedObjectID].Running {
		t.Fatalf("objects after update = %#v", objects)
	}

	if _, err := supervisor.Reconcile(ctx, model.Assignment{}); err != nil {
		t.Fatal(err)
	}
	if objects := runtime.SnapshotObjects(); len(objects) != 0 {
		t.Fatalf("objects after removal = %#v", objects)
	}
	states, err := cache.AllocationStates(ctx)
	if err != nil || len(states) != 0 {
		t.Fatalf("allocation state after removal = %#v, %v", states, err)
	}
}

func TestSupervisorKeepsPlan4ObjectDuringNetworkPreflight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	dns := discovery.NewFake()
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return supervisorTestTime },
		SupervisorServices{Discovery: dns, Ingress: &ingress.Fake{}})
	legacy := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	var err error
	legacy.SpecDigest, err = model.Plan4AllocationSpecDigest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacyObject := orchruntime.Object{ID: orchruntime.ObjectID(legacy.ID, legacy.SpecDigest),
		AllocationID: legacy.ID, SpecDigest: legacy.SpecDigest, Running: true, StartedAt: supervisorTestTime}
	runtime.InjectObject(legacyObject)
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	statuses, err := supervisor.Reconcile(ctx, model.Assignment{Network: networkConfig,
		Allocations: []model.AllocationDesired{legacy}})
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Phase != model.AllocationRunning ||
		statuses[0].ActiveSpecDigest != legacy.SpecDigest {
		t.Fatalf("legacy preflight status = %#v", statuses)
	}
	if operations := runtime.SnapshotOperations(); !reflect.DeepEqual(operations, []string{"network:" + networkConfig.NodeSubnet}) {
		t.Fatalf("legacy preflight mutated the running object: %v", operations)
	}
	if objects := runtime.SnapshotObjects(); len(objects) != 1 || !objects[legacyObject.ID].Running {
		t.Fatalf("legacy preflight objects = %#v", objects)
	}
}

func TestSupervisorRetainsPlan4ObjectWhenFirstNetworkedStartFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return supervisorTestTime },
		SupervisorServices{Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}})
	legacy := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	var err error
	legacy.SpecDigest, err = model.Plan4AllocationSpecDigest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacyObject := orchruntime.Object{ID: orchruntime.ObjectID(legacy.ID, legacy.SpecDigest),
		AllocationID: legacy.ID, SpecDigest: legacy.SpecDigest, Running: true, StartedAt: supervisorTestTime}
	runtime.InjectObject(legacyObject)
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	desired := legacy
	desired.NetworkGeneration = networkConfig.Generation
	desired.MaxSurge = 1
	desired.Rollback = rollbackFromAllocation(legacy)
	desired.SpecDigest = mustAllocationDigest(t, desired)
	runtime.SetEnsureError(desired.ID, errors.New("injected first networked start failure"))
	statuses, err := supervisor.Reconcile(ctx, model.Assignment{Network: networkConfig,
		Allocations: []model.AllocationDesired{desired}})
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Phase != model.AllocationBackoff ||
		statuses[0].ActiveSpecDigest != legacy.SpecDigest || statuses[0].ActiveGeneration != legacy.ApplicationGeneration {
		t.Fatalf("failed network migration status = %#v", statuses)
	}
	if objects := runtime.SnapshotObjects(); len(objects) != 1 || !objects[legacyObject.ID].Running {
		t.Fatalf("failed network migration removed the Plan #4 object: %#v", objects)
	}
	for _, operation := range runtime.SnapshotOperations() {
		if operation == "stop:"+legacyObject.ID || operation == "remove:"+legacyObject.ID {
			t.Fatalf("failed network migration mutated the Plan #4 object: %v", runtime.SnapshotOperations())
		}
	}
}

func TestSupervisorRetainsStatelessRevisionOnFailedReplacement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	supervisor := NewSupervisor(cache, runtime, func() time.Time { return supervisorTestTime })
	old := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{old}}); err != nil {
		t.Fatal(err)
	}
	replacement := old
	replacement.Image = "sha256:" + strings.Repeat("b", 64)
	replacement.ApplicationGeneration = 2
	replacement.RevisionID = "revision-two"
	replacement.Rollback = rollbackFromAllocation(old)
	replacement.SpecDigest = mustAllocationDigest(t, replacement)
	runtime.SetEnsureError(replacement.ID, errors.New("injected start failure"))
	statuses, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{replacement}})
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Phase != model.AllocationBackoff || statuses[0].Reason != "replacement_rollback_retained" || statuses[0].RestartCount != 1 {
		t.Fatalf("replacement status = %#v", statuses)
	}
	objects := runtime.SnapshotObjects()
	oldID := orchruntime.ObjectID(old.ID, old.SpecDigest)
	if len(objects) != 1 || !objects[oldID].Running {
		t.Fatalf("old revision was not retained: %#v", objects)
	}
	operationCount := len(runtime.SnapshotOperations())
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{replacement}})
	if err != nil || statuses[0].Phase != model.AllocationBackoff || len(runtime.SnapshotOperations()) != operationCount {
		t.Fatalf("replacement retried before durable backoff: %#v, %v, operations %v", statuses, err, runtime.SnapshotOperations())
	}
}

func TestSupervisorBacksOffInitialStartWithoutRuntimeObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	now := supervisorTestTime
	supervisor := NewSupervisor(cache, runtime, func() time.Time { return now }, strings.NewReader(strings.Repeat("\x00", 64)))
	allocation := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	runtime.SetEnsureError(allocation.ID, errors.New("pull failed"))
	statuses, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil || statuses[0].Phase != model.AllocationBackoff || statuses[0].RestartCount != 1 {
		t.Fatalf("initial start failure = %#v, %v", statuses, err)
	}
	operationCount := len(runtime.SnapshotOperations())
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil || statuses[0].Phase != model.AllocationBackoff || statuses[0].Reason != "process_start_backoff" ||
		len(runtime.SnapshotOperations()) != operationCount {
		t.Fatalf("initial start retried before backoff: %#v, %v, operations %v", statuses, err, runtime.SnapshotOperations())
	}
	now = now.Add(3 * time.Second)
	runtime.SetEnsureError(allocation.ID, nil)
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil || statuses[0].Phase != model.AllocationRunning || statuses[0].RestartCount != 1 {
		t.Fatalf("initial start retry = %#v, %v", statuses, err)
	}
}

func TestSupervisorKeepsTerminalPoliciesStoppedWithoutRuntimeObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	supervisor := NewSupervisor(cache, runtime, func() time.Time { return supervisorTestTime })

	never := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	never.RestartPolicy = "never"
	never.SpecDigest = mustAllocationDigest(t, never)
	runtime.SetEnsureError(never.ID, errors.New("initial start failed"))
	statuses, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{never}})
	if err != nil || statuses[0].Phase != model.AllocationFailed {
		t.Fatalf("initial never failure = %#v, %v", statuses, err)
	}
	operationCount := len(runtime.SnapshotOperations())
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{never}})
	if err != nil || statuses[0].Phase != model.AllocationFailed || len(runtime.SnapshotOperations()) != operationCount {
		t.Fatalf("terminal failure retried: %#v, %v, operations %v", statuses, err, runtime.SnapshotOperations())
	}

	onFailure := never
	onFailure.ID = "alloc-on-failure"
	onFailure.RestartPolicy = "on-failure"
	onFailure.SpecDigest = mustAllocationDigest(t, onFailure)
	runtime.SetEnsureError(onFailure.ID, nil)
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{onFailure}})
	if err != nil || statuses[0].Phase != model.AllocationRunning {
		t.Fatalf("on-failure start = %#v, %v", statuses, err)
	}
	onFailureID := orchruntime.ObjectID(onFailure.ID, onFailure.SpecDigest)
	if err := runtime.Exit(onFailureID, 0); err != nil {
		t.Fatal(err)
	}
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{onFailure}})
	if err != nil || statuses[0].Phase != model.AllocationStopped {
		t.Fatalf("successful on-failure exit = %#v, %v", statuses, err)
	}
	if err := runtime.Remove(ctx, onFailureID); err != nil {
		t.Fatal(err)
	}
	operationCount = len(runtime.SnapshotOperations())
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{onFailure}})
	if err != nil || statuses[0].Phase != model.AllocationStopped || len(runtime.SnapshotOperations()) != operationCount {
		t.Fatalf("completed process was recreated: %#v, %v, operations %v", statuses, err, runtime.SnapshotOperations())
	}
}

func TestSupervisorUsesStopFirstForLocalVolumeReplacement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	supervisor := NewSupervisor(cache, runtime, func() time.Time { return supervisorTestTime })
	old := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	old.Mounts = []model.VolumeMount{{Name: "data", Target: "/data"}}
	old.StartBeforeStop = false
	old.SpecDigest = mustAllocationDigest(t, old)
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{old}}); err != nil {
		t.Fatal(err)
	}
	replacement := old
	replacement.Image = "sha256:" + strings.Repeat("b", 64)
	replacement.ApplicationGeneration = 2
	replacement.SpecDigest = mustAllocationDigest(t, replacement)
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{replacement}}); err != nil {
		t.Fatal(err)
	}
	oldID := orchruntime.ObjectID(old.ID, old.SpecDigest)
	newID := orchruntime.ObjectID(replacement.ID, replacement.SpecDigest)
	wantSuffix := []string{"stop:" + oldID, "remove:" + oldID, "ensure:" + newID}
	operations := runtime.SnapshotOperations()
	if len(operations) < len(wantSuffix) || !reflect.DeepEqual(operations[len(operations)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("volume replacement operations = %v, want suffix %v", operations, wantSuffix)
	}
}

func TestSupervisorWithdrawsAndDrainsNetworkedStopFirstReplacement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	now := supervisorTestTime
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}})
	networkConfig, _ := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	old := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	old.Mounts = []model.VolumeMount{{Name: "data", Target: "/data"}}
	old.StartBeforeStop = false
	old.NetworkGeneration = networkConfig.Generation
	old.SpecDigest = mustAllocationDigest(t, old)
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{old}}); err != nil {
		t.Fatal(err)
	}
	replacement := old
	replacement.Image = "sha256:" + strings.Repeat("b", 64)
	replacement.ApplicationGeneration = 2
	replacement.RevisionID = "revision-two"
	replacement.Rollback = rollbackFromAllocation(old)
	replacement.SpecDigest = mustAllocationDigest(t, replacement)
	assignment := model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{replacement}}
	statuses, err := supervisor.Reconcile(ctx, assignment)
	if err != nil || statuses[0].Reason != "endpoint_draining" {
		t.Fatalf("withdrawal status = %#v, %v", statuses, err)
	}
	oldID := orchruntime.ObjectID(old.ID, old.SpecDigest)
	if objects := runtime.SnapshotObjects(); len(objects) != 1 || !objects[oldID].Running {
		t.Fatalf("old endpoint was stopped before its DNS drain: %#v", objects)
	}
	accepted, err := cache.ServingEndpoints(ctx)
	if err != nil || len(accepted) != 0 {
		t.Fatalf("accepted endpoints during drain = %#v, %v", accepted, err)
	}
	now = now.Add(time.Duration(discovery.RecordTTL)*time.Second - time.Nanosecond)
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if objects := runtime.SnapshotObjects(); len(objects) != 1 || !objects[oldID].Running {
		t.Fatalf("old endpoint did not survive the full DNS TTL: %#v", objects)
	}
	now = now.Add(time.Nanosecond)
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	newID := orchruntime.ObjectID(replacement.ID, replacement.SpecDigest)
	if objects := runtime.SnapshotObjects(); len(objects) != 1 || !objects[newID].Running {
		t.Fatalf("stop-first replacement objects = %#v", objects)
	}
}

func TestSupervisorPersistsRestartPolicyAndBackoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	now := supervisorTestTime
	supervisor := NewSupervisor(cache, runtime, func() time.Time { return now }, strings.NewReader(strings.Repeat("\x00", 64)))
	allocation := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}}); err != nil {
		t.Fatal(err)
	}
	id := orchruntime.ObjectID(allocation.ID, allocation.SpecDigest)
	if err := runtime.Exit(id, 7); err != nil {
		t.Fatal(err)
	}
	runtime.SetEnsureError(allocation.ID, errors.New("restart failed"))
	statuses, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].Phase != model.AllocationBackoff || statuses[0].RestartCount != 1 {
		t.Fatalf("failed restart status = %#v", statuses[0])
	}
	states, err := cache.AllocationStates(ctx)
	if err != nil || len(states) != 1 || states[0].NextAttemptAt == nil || !states[0].NextAttemptAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("persisted injected backoff = %#v, %v", states, err)
	}
	operationCount := len(runtime.SnapshotOperations())
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil || statuses[0].Phase != model.AllocationBackoff || len(runtime.SnapshotOperations()) != operationCount {
		t.Fatalf("early retry = %#v, %v, operations %v", statuses, err, runtime.SnapshotOperations())
	}
	now = now.Add(3 * time.Second)
	runtime.SetEnsureError(allocation.ID, nil)
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil || statuses[0].Phase != model.AllocationRunning || statuses[0].RestartCount != 2 {
		t.Fatalf("successful retry = %#v, %v", statuses, err)
	}

	never := allocation
	never.ID = "alloc-never"
	never.RestartPolicy = "never"
	never.SpecDigest = mustAllocationDigest(t, never)
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{never}}); err != nil {
		t.Fatal(err)
	}
	neverID := orchruntime.ObjectID(never.ID, never.SpecDigest)
	if err := runtime.Exit(neverID, 0); err != nil {
		t.Fatal(err)
	}
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{never}})
	if err != nil || statuses[0].Phase != model.AllocationStopped || statuses[0].Reason != "restart_policy_complete" {
		t.Fatalf("never restart status = %#v, %v", statuses, err)
	}
}

func TestSupervisorRoutesLogsAndExecOnlyToExactRunningObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	supervisor := NewSupervisor(cache, runtime, func() time.Time { return supervisorTestTime })
	allocation := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}}); err != nil {
		t.Fatal(err)
	}
	id := orchruntime.ObjectID(allocation.ID, allocation.SpecDigest)
	runtime.SetLogs(id, []orchruntime.LogFrame{{Stream: "stdout", Data: []byte("hello\n")}, {Stream: "stderr", Data: []byte("warning\n")}})
	logs, err := supervisor.TailLogs(ctx, allocation, 10)
	if err != nil || len(logs) != 2 || string(logs[0].Data) != "hello\n" || string(logs[1].Data) != "warning\n" {
		t.Fatalf("TailLogs() = %#v, %v", logs, err)
	}
	runtime.SetExecFrames(id, "/bin/echo", []orchruntime.ExecFrame{{Stream: "stdout", Data: []byte("ok\n")}, {Exited: true, ExitCode: 0}})
	frames, err := supervisor.Exec(ctx, allocation, []string{"/bin/echo", "ok"})
	if err != nil {
		t.Fatal(err)
	}
	var got []orchruntime.ExecFrame
	for frame := range frames {
		got = append(got, frame)
	}
	if len(got) != 2 || !got[1].Exited || got[1].ExitCode != 0 {
		t.Fatalf("exec frames = %#v", got)
	}
	wrong := allocation
	wrong.SpecDigest = "sha256:" + strings.Repeat("f", 64)
	if _, err := supervisor.TailLogs(ctx, wrong, 10); !errors.Is(err, orchruntime.ErrNotFound) {
		t.Fatalf("wrong-generation logs = %v", err)
	}
}

func TestSupervisorHealthGatesIngress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	prober := &health.Fake{}
	proxy := &ingress.Fake{}
	now := supervisorTestTime
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Prober: prober, Discovery: discovery.NewFake(), Ingress: proxy})
	allocation := supervisorHTTPAllocation(t, "alloc-web", "sha256:"+strings.Repeat("a", 64), 1)
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	assignment := model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{allocation}}

	statuses, err := supervisor.Reconcile(ctx, assignment)
	if err != nil || statuses[0].HealthStatus != model.HealthChecking {
		t.Fatalf("first health observation = %#v, %v", statuses, err)
	}
	if len(prober.Targets) != 1 || prober.Targets[0].Host != "web.hello.svc.internal" {
		t.Fatalf("health targets = %#v", prober.Targets)
	}
	routes, networkStatus := supervisor.Observations()
	if len(routes) != 1 || routes[0].State != model.RoutePending || len(proxy.Routes) != 0 || networkStatus.State != model.NetworkReady {
		t.Fatalf("gated observations = routes %#v, proxy %#v, network %#v", routes, proxy.Routes, networkStatus)
	}

	if statuses, err = supervisor.Reconcile(ctx, assignment); err != nil || len(prober.Targets) != 1 {
		t.Fatalf("probe ran before fixed interval: %#v, %v, probes %d", statuses, err, len(prober.Targets))
	}
	now = now.Add(health.ProbeInterval)
	statuses, err = supervisor.Reconcile(ctx, assignment)
	if err != nil || statuses[0].HealthStatus != model.HealthReady || statuses[0].ReadyAt == nil {
		t.Fatalf("ready health observation = %#v, %v", statuses, err)
	}
	routes, _ = supervisor.Observations()
	if len(routes) != 1 || routes[0].State != model.RouteActive || len(proxy.Routes) != 1 ||
		!reflect.DeepEqual(proxy.Routes[0].Upstreams, []string{"10.88.0.2:8080"}) {
		t.Fatalf("active routes = %#v, proxy %#v", routes, proxy.Routes)
	}
}

func TestSupervisorPromotesMetadataOnlyHealthyRevisionWithoutAnotherProbe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	prober := &health.Fake{}
	now := supervisorTestTime
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Prober: prober, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}})
	allocation := supervisorHTTPAllocation(t, "alloc-web", "sha256:"+strings.Repeat("a", 64), 1)
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	assignment := model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{allocation}}
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	now = now.Add(health.ProbeInterval)
	statuses, err := supervisor.Reconcile(ctx, assignment)
	if err != nil || statuses[0].HealthStatus != model.HealthReady || statuses[0].ActiveGeneration != 1 {
		t.Fatalf("initial healthy status = %#v, %v", statuses, err)
	}
	probes := len(prober.Targets)
	objectID := orchruntime.ObjectID(allocation.ID, allocation.SpecDigest)

	updated := allocation
	updated.ApplicationGeneration = 2
	updated.RevisionID = "revision-two"
	updated.Rollback = rollbackFromAllocation(allocation)
	updated.SpecDigest = mustAllocationDigest(t, updated)
	if updated.SpecDigest != allocation.SpecDigest {
		t.Fatal("metadata-only revision changed the runtime digest")
	}
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{
		Network: networkConfig, Allocations: []model.AllocationDesired{updated},
	})
	if err != nil || statuses[0].ActiveGeneration != 2 || statuses[0].ActiveRevisionID != "revision-two" ||
		statuses[0].Reason != "readiness_ready" {
		t.Fatalf("metadata-only healthy status = %#v, %v", statuses, err)
	}
	if len(prober.Targets) != probes {
		t.Fatalf("metadata-only revision triggered another probe: %d -> %d", probes, len(prober.Targets))
	}
	if object, exists := runtime.SnapshotObjects()[objectID]; !exists || !object.Running {
		t.Fatalf("metadata-only revision replaced the running object: %#v", runtime.SnapshotObjects())
	}
}

func TestSupervisorStartsDNSForAnEmptyNetworkAssignment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	dns := discovery.NewFake()
	supervisor := NewSupervisorWithServices(cache, orchruntime.NewFake(), nil,
		SupervisorServices{Discovery: dns, Ingress: &ingress.Fake{}})
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	statuses, err := supervisor.Reconcile(ctx, model.Assignment{Network: networkConfig})
	if err != nil || len(statuses) != 0 {
		t.Fatalf("empty network reconcile = %#v, %v", statuses, err)
	}
	_, address, started := dns.Current()
	if !started || address != "10.88.0.1:53" {
		t.Fatalf("DNS listener = %q, started %t", address, started)
	}
	_, networkStatus := supervisor.Observations()
	if networkStatus.State != model.NetworkReady || networkStatus.DNSState != model.SubsystemReady {
		t.Fatalf("network status = %#v", networkStatus)
	}
}

func TestSupervisorDoesNotReportNetworkReadyWithoutValidatedIngress(t *testing.T) {
	t.Parallel()
	supervisor := NewSupervisorWithServices(supervisorCache(t), orchruntime.NewFake(), nil,
		SupervisorServices{Discovery: discovery.NewFake()})
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Reconcile(context.Background(), model.Assignment{Network: networkConfig}); err != nil {
		t.Fatal(err)
	}
	_, networkStatus := supervisor.Observations()
	if networkStatus.State != model.NetworkFailed || networkStatus.Reason != "caddy_unavailable" ||
		networkStatus.IngressState != model.SubsystemNotConfigured {
		t.Fatalf("network status without ingress = %#v", networkStatus)
	}
}

func TestSupervisorRebindsDNSWhenUnfrozenNetworkChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dns := discovery.NewFake()
	supervisor := NewSupervisorWithServices(supervisorCache(t), orchruntime.NewFake(), nil,
		SupervisorServices{Discovery: dns, Ingress: &ingress.Fake{}})
	first, err := model.FirstNodeNetwork("10.88.0.0/16", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Network: first}); err != nil {
		t.Fatal(err)
	}
	second, err := model.FirstNodeNetwork("10.96.0.0/16", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Network: second}); err != nil {
		t.Fatal(err)
	}
	_, address, started := dns.Current()
	if !started || address != "10.96.0.1:53" || dns.Closed {
		t.Fatalf("rebound DNS listener = %q, started=%t, closed=%t", address, started, dns.Closed)
	}
	_, networkStatus := supervisor.Observations()
	if networkStatus.State != model.NetworkReady || networkStatus.Generation != 2 {
		t.Fatalf("rebound network status = %#v", networkStatus)
	}
}

func TestSupervisorReportsStableCIDRConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	runtime.NetworkError = fmt.Errorf("%w: overlaps 10.88.0.0/24", network.ErrCIDRConflict)
	supervisor := NewSupervisorWithServices(cache, runtime, nil,
		SupervisorServices{Discovery: discovery.NewFake()})
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	statuses, err := supervisor.Reconcile(ctx, model.Assignment{
		Network: networkConfig, Allocations: []model.AllocationDesired{supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))},
	})
	if err != nil || len(statuses) != 1 || statuses[0].Reason != "network_prepare_failed" {
		t.Fatalf("failed allocation status = %#v, %v", statuses, err)
	}
	_, networkStatus := supervisor.Observations()
	if networkStatus.State != model.NetworkFailed || networkStatus.Reason != "network_cidr_conflict" ||
		networkStatus.DNSState != model.SubsystemBlocked || networkStatus.IngressState != model.SubsystemBlocked {
		t.Fatalf("network conflict status = %#v", networkStatus)
	}
}

func TestSupervisorDoesNotActivateIngressWhenDNSStartupFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	prober := &health.Fake{}
	dns := discovery.NewFake()
	dns.StartErr = errors.New("address unavailable")
	proxy := &ingress.Fake{}
	now := supervisorTestTime
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Prober: prober, Discovery: dns, Ingress: proxy})
	allocation := supervisorHTTPAllocation(t, "alloc-web", "sha256:"+strings.Repeat("a", 64), 1)
	networkConfig, _ := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	assignment := model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{allocation}}
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	now = now.Add(health.ProbeInterval)
	statuses, err := supervisor.Reconcile(ctx, assignment)
	if err != nil || statuses[0].HealthStatus != model.HealthReady {
		t.Fatalf("ready allocation = %#v, %v", statuses, err)
	}
	routes, networkStatus := supervisor.Observations()
	if proxy.ApplyCalls != 0 || len(routes) != 1 || routes[0].State != model.RouteFailed ||
		routes[0].Reason != "dns_start_failed" || networkStatus.State != model.NetworkFailed {
		t.Fatalf("DNS-gated routes = %#v, network = %#v, ingress calls = %d", routes, networkStatus, proxy.ApplyCalls)
	}
	if endpoints, err := cache.ServingEndpoints(ctx); err != nil || len(endpoints) != 0 {
		t.Fatalf("accepted endpoints after DNS failure = %#v, %v", endpoints, err)
	}
	dns.StartErr = nil
	now = now.Add(health.ProbeInterval)
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	routes, networkStatus = supervisor.Observations()
	if proxy.ApplyCalls != 1 || len(routes) != 1 || routes[0].State != model.RouteActive || networkStatus.State != model.NetworkReady {
		t.Fatalf("recovered routes = %#v, network = %#v, ingress calls = %d", routes, networkStatus, proxy.ApplyCalls)
	}
	if endpoints, err := cache.ServingEndpoints(ctx); err != nil || len(endpoints) != 1 {
		t.Fatalf("accepted endpoints after recovery = %#v, %v", endpoints, err)
	}
}

func TestSupervisorRollsBackAfterThreeReadinessFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	prober := &health.Fake{}
	proxy := &ingress.Fake{}
	now := supervisorTestTime
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Prober: prober, Discovery: discovery.NewFake(), Ingress: proxy})
	networkConfig, _ := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	old := supervisorHTTPAllocation(t, "alloc-web", "sha256:"+strings.Repeat("a", 64), 1)
	old.MaxSurge = 1
	old.SpecDigest = mustAllocationDigest(t, old)
	assignment := model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{old}}
	for index := range 2 {
		if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			now = now.Add(health.ProbeInterval)
		}
	}
	replacement := old
	replacement.Image = "sha256:" + strings.Repeat("b", 64)
	replacement.ApplicationGeneration = 2
	replacement.RevisionID = "revision-two"
	replacement.Rollback = rollbackFromAllocation(old)
	replacement.SpecDigest = mustAllocationDigest(t, replacement)
	prober.Err = errors.New("not ready")
	assignment.Allocations = []model.AllocationDesired{replacement}
	var statuses []model.AllocationStatus
	for range 3 {
		now = now.Add(health.ProbeInterval)
		var err error
		statuses, err = supervisor.Reconcile(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
	}
	if statuses[0].Reason != "health_gate_rollback" || statuses[0].ActiveSpecDigest != old.SpecDigest ||
		statuses[0].ActiveGeneration != old.ApplicationGeneration || statuses[0].HealthStatus != model.HealthReady {
		t.Fatalf("rollback status = %#v", statuses[0])
	}
	objects := runtime.SnapshotObjects()
	oldID := orchruntime.ObjectID(old.ID, old.SpecDigest)
	if len(objects) != 1 || !objects[oldID].Running {
		t.Fatalf("rollback objects = %#v", objects)
	}
	runtime.SetLogs(oldID, []orchruntime.LogFrame{{Stream: "stdout", Data: []byte("prior revision\n")}})
	logs, err := supervisor.TailLogs(ctx, replacement, 10)
	if err != nil || len(logs) != 1 || string(logs[0].Data) != "prior revision\n" {
		t.Fatalf("rolled-back logs = %#v, %v", logs, err)
	}
	runtime.SetExecFrames(oldID, "/bin/true", []orchruntime.ExecFrame{{Exited: true, ExitCode: 0}})
	frames, err := supervisor.Exec(ctx, replacement, []string{"/bin/true"})
	if err != nil {
		t.Fatalf("rolled-back exec = %v", err)
	}
	frame := <-frames
	if !frame.Exited || frame.ExitCode != 0 {
		t.Fatalf("rolled-back exec frame = %#v", frame)
	}
	fallbackStatuses, err := supervisor.Reconcile(ctx, assignment)
	if err != nil || fallbackStatuses[0].HealthStatus != model.HealthReady {
		t.Fatalf("first fallback health failure = %#v, %v", fallbackStatuses, err)
	}
}

func TestSupervisorDoesNotPublishAnUnacceptedRollbackObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	prober := &health.Fake{}
	proxy := &ingress.Fake{}
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return supervisorTestTime },
		SupervisorServices{Prober: prober, Discovery: discovery.NewFake(), Ingress: proxy})
	networkConfig, _ := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	old := supervisorHTTPAllocation(t, "alloc-web", "sha256:"+strings.Repeat("a", 64), 1)
	old.MaxSurge = 1
	old.SpecDigest = mustAllocationDigest(t, old)
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{old}}); err != nil {
		t.Fatal(err)
	}
	replacement := old
	replacement.Image = "sha256:" + strings.Repeat("b", 64)
	replacement.ApplicationGeneration = 2
	replacement.RevisionID = "revision-two"
	replacement.Rollback = rollbackFromAllocation(old)
	replacement.SpecDigest = mustAllocationDigest(t, replacement)
	statuses, err := supervisor.Reconcile(ctx, model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{replacement}})
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].ActiveSpecDigest != "" || len(proxy.Routes) != 0 {
		t.Fatalf("unaccepted rollback was published: status %#v, routes %#v", statuses[0], proxy.Routes)
	}
	if endpoints, err := cache.ServingEndpoints(ctx); err != nil || len(endpoints) != 0 {
		t.Fatalf("accepted endpoints = %#v, %v", endpoints, err)
	}
}

func TestDesiredRoutesUseEachServingRevisionPort(t *testing.T) {
	t.Parallel()
	first := supervisorHTTPAllocation(t, "alloc-one", "sha256:"+strings.Repeat("b", 64), 2)
	first.HTTP.Port = 9090
	first.SpecDigest = mustAllocationDigest(t, first)
	old := first
	old.ApplicationGeneration = 1
	old.RevisionID = "revision-one"
	old.HTTP = &model.HTTPDesired{Port: 8080, Domains: []string{"app.example.com"}, HealthPath: "/healthz", HTTPS: true}
	old.SpecDigest = mustAllocationDigest(t, old)
	first.Rollback = rollbackFromAllocation(old)
	second := first
	second.ID = "alloc-two"
	second.Replica = 2
	second.SpecDigest = mustAllocationDigest(t, second)
	second.Rollback = rollbackFromAllocation(old)

	routes, pending := desiredRoutes([]model.AllocationDesired{first, second}, nil, map[string]string{
		first.ID: "10.88.0.2", second.ID: "10.88.0.3",
	}, map[string]string{
		first.ID: first.SpecDigest, second.ID: second.Rollback.SpecDigest,
	}, supervisorTestTime)
	if len(pending) != 0 || len(routes) != 1 || !reflect.DeepEqual(routes[0].Upstreams,
		[]string{"10.88.0.2:9090", "10.88.0.3:8080"}) {
		t.Fatalf("routes = %#v, pending = %#v", routes, pending)
	}
}

func TestDesiredRoutesSwitchTheHTTPContractOnlyAtServingBoundaries(t *testing.T) {
	t.Parallel()
	prior := supervisorHTTPAllocation(t, "alloc-one", "sha256:"+strings.Repeat("a", 64), 1)
	prior.HTTP = &model.HTTPDesired{Port: 8080, Domains: []string{"old.example.com"}, HealthPath: "/healthz", HTTPS: true}
	prior.SpecDigest = mustAllocationDigest(t, prior)
	current := prior
	current.ApplicationGeneration = 2
	current.RevisionID = "revision-two"
	current.HTTP = &model.HTTPDesired{Port: 9090, Domains: []string{"new.example.com"}, HealthPath: "/healthz"}
	current.Rollback = rollbackFromAllocation(prior)
	current.SpecDigest = mustAllocationDigest(t, current)

	routes, pending := desiredRoutes([]model.AllocationDesired{current}, []model.AllocationStatus{{
		Application: current.Application, Reason: "readiness_checking",
	}}, map[string]string{current.ID: "10.88.0.2"}, map[string]string{current.ID: prior.SpecDigest}, supervisorTestTime)
	if len(routes) != 1 || routes[0].Domain != "old.example.com" || !routes[0].HTTPS ||
		!reflect.DeepEqual(routes[0].Upstreams, []string{"10.88.0.2:8080"}) ||
		len(pending) != 1 || pending[0].Domain != "new.example.com" {
		t.Fatalf("pre-activation routes = %#v, pending = %#v", routes, pending)
	}

	second := current
	second.ID = "alloc-two"
	second.Replica = 2
	routes, pending = desiredRoutes([]model.AllocationDesired{current, second}, []model.AllocationStatus{
		{Application: current.Application, Reason: "readiness_checking"},
		{Application: second.Application, Reason: "readiness_ready"},
	}, map[string]string{current.ID: "10.88.0.2", second.ID: "10.88.0.3"}, map[string]string{
		current.ID: prior.SpecDigest, second.ID: second.SpecDigest,
	}, supervisorTestTime)
	if len(pending) != 0 || len(routes) != 1 || routes[0].Domain != "new.example.com" || routes[0].HTTPS ||
		!reflect.DeepEqual(routes[0].Upstreams, []string{"10.88.0.2:8080", "10.88.0.3:9090"}) {
		t.Fatalf("activated routes = %#v, pending = %#v", routes, pending)
	}

	routes, pending = desiredRoutes([]model.AllocationDesired{current, second}, []model.AllocationStatus{
		{Application: current.Application, Reason: "health_gate_rollback"},
		{Application: second.Application, Reason: "health_gate_rollback"},
	}, map[string]string{current.ID: "10.88.0.2", second.ID: "10.88.0.3"}, map[string]string{
		current.ID: prior.SpecDigest, second.ID: prior.SpecDigest,
	}, supervisorTestTime)
	if len(routes) != 1 || routes[0].Domain != "old.example.com" || !routes[0].HTTPS || len(pending) != 1 ||
		pending[0].Domain != "new.example.com" {
		t.Fatalf("rolled-back routes = %#v, pending = %#v", routes, pending)
	}
}

func TestSupervisorEnforcesOneApplicationSurge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	now := supervisorTestTime
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}})
	networkConfig, _ := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	first := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	first.ID = "alloc-one"
	first.NetworkGeneration = 1
	first.MaxSurge = 1
	first.SpecDigest = mustAllocationDigest(t, first)
	second := first
	second.ID = "alloc-two"
	second.Replica = 2
	second.SpecDigest = mustAllocationDigest(t, second)
	assignment := model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{first, second}}
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	updatedFirst := first
	updatedFirst.Image = "sha256:" + strings.Repeat("b", 64)
	updatedFirst.ApplicationGeneration = 2
	updatedFirst.RevisionID = "revision-two"
	updatedFirst.Rollback = rollbackFromAllocation(first)
	updatedFirst.SpecDigest = mustAllocationDigest(t, updatedFirst)
	updatedSecond := second
	updatedSecond.Image = updatedFirst.Image
	updatedSecond.ApplicationGeneration = 2
	updatedSecond.RevisionID = "revision-two"
	updatedSecond.Rollback = rollbackFromAllocation(second)
	updatedSecond.SpecDigest = mustAllocationDigest(t, updatedSecond)
	statuses, err := supervisor.Reconcile(ctx, model.Assignment{Network: networkConfig,
		Allocations: []model.AllocationDesired{updatedFirst, updatedSecond}})
	if err != nil {
		t.Fatal(err)
	}
	queued := 0
	for _, status := range statuses {
		if status.Reason == "rollout_budget_wait" {
			queued++
		}
	}
	if queued != 1 || len(runtime.SnapshotObjects()) != 3 {
		t.Fatalf("surge statuses = %#v; objects = %#v", statuses, runtime.SnapshotObjects())
	}
	now = now.Add(time.Duration(discovery.RecordTTL) * time.Second)
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Network: networkConfig,
		Allocations: []model.AllocationDesired{updatedFirst, updatedSecond}}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.SnapshotObjects()) != 2 {
		t.Fatalf("drained surge objects = %#v", runtime.SnapshotObjects())
	}
}

func TestSupervisorRollsBackEveryReplicaAfterOneCandidateFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	prober := &addressFailingProber{}
	now := supervisorTestTime
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Prober: prober, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}})
	networkConfig, _ := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	first := supervisorHTTPAllocation(t, "alloc-one", "sha256:"+strings.Repeat("a", 64), 1)
	first.Replica = 1
	first.SpecDigest = mustAllocationDigest(t, first)
	second := first
	second.ID = "alloc-two"
	second.Replica = 2
	second.SpecDigest = mustAllocationDigest(t, second)
	assignment := model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{first, second}}
	for range 2 {
		if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
			t.Fatal(err)
		}
		now = now.Add(health.ProbeInterval)
	}

	updatedFirst := first
	updatedFirst.Image = "sha256:" + strings.Repeat("b", 64)
	updatedFirst.ApplicationGeneration = 2
	updatedFirst.RevisionID = "revision-two"
	updatedFirst.Rollback = rollbackFromAllocation(first)
	updatedFirst.SpecDigest = mustAllocationDigest(t, updatedFirst)
	updatedSecond := second
	updatedSecond.Image = updatedFirst.Image
	updatedSecond.ApplicationGeneration = 2
	updatedSecond.RevisionID = "revision-two"
	updatedSecond.Rollback = rollbackFromAllocation(second)
	updatedSecond.SpecDigest = mustAllocationDigest(t, updatedSecond)
	assignment.Allocations = []model.AllocationDesired{updatedFirst, updatedSecond}

	// Activate replica one, drain its prior object, then allow replica two to
	// start. This creates the hard rollback case where one old object must be
	// recreated while the already accepted desired endpoint stays available.
	for range 2 {
		if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
			t.Fatal(err)
		}
		now = now.Add(health.ProbeInterval)
	}
	now = now.Add(time.Duration(discovery.RecordTTL) * time.Second)
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	now = now.Add(health.ProbeInterval)
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	newSecond := runtime.SnapshotObjects()[orchruntime.ObjectID(updatedSecond.ID, updatedSecond.SpecDigest)]
	if newSecond.Address == "" {
		t.Fatalf("second replacement was not started: %#v", runtime.SnapshotObjects())
	}
	prober.FailAddress = newSecond.Address

	var statuses []model.AllocationStatus
	for range health.UnhealthyFailures {
		now = now.Add(health.ProbeInterval)
		var err error
		statuses, err = supervisor.Reconcile(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
	}
	if statuses[0].ActiveSpecDigest != updatedFirst.SpecDigest || statuses[1].ActiveSpecDigest != second.SpecDigest {
		t.Fatalf("first rollback boundary = %#v", statuses)
	}

	// Once the failed surge object is gone, the budget admits recreation and
	// two successful probes of replica one's authorized fallback.
	for range 3 {
		now = now.Add(health.ProbeInterval)
		var err error
		statuses, err = supervisor.Reconcile(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range statuses {
		if status.Reason != "health_gate_rollback" || status.ActiveGeneration != 1 ||
			status.ActiveSpecDigest != first.SpecDigest || status.HealthStatus != model.HealthReady {
			t.Fatalf("application-wide rollback status = %#v", statuses)
		}
	}
	now = now.Add(time.Duration(discovery.RecordTTL) * time.Second)
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	objects := runtime.SnapshotObjects()
	if len(objects) != 2 || objects[orchruntime.ObjectID(first.ID, first.SpecDigest)].SpecDigest != first.SpecDigest ||
		objects[orchruntime.ObjectID(second.ID, second.SpecDigest)].SpecDigest != second.SpecDigest {
		t.Fatalf("rolled-back runtime objects = %#v", objects)
	}
}

func TestSupervisorRollsBackUnchangedServiceMetadataWithoutRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	prober := &addressFailingProber{}
	now := supervisorTestTime
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Prober: prober, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}})
	networkConfig, _ := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	worker := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	worker.ID = "alloc-a-worker"
	worker.NetworkGeneration = 1
	worker.MaxSurge = 1
	worker.SpecDigest = mustAllocationDigest(t, worker)
	web := supervisorHTTPAllocation(t, "alloc-b-web", "sha256:"+strings.Repeat("a", 64), 1)
	assignment := model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{worker, web}}
	for range 2 {
		if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
			t.Fatal(err)
		}
		now = now.Add(health.ProbeInterval)
	}
	workerObjectID := orchruntime.ObjectID(worker.ID, worker.SpecDigest)

	updatedWorker := worker
	updatedWorker.ApplicationGeneration = 2
	updatedWorker.RevisionID = "revision-two"
	updatedWorker.Rollback = rollbackFromAllocation(worker)
	updatedWorker.SpecDigest = mustAllocationDigest(t, updatedWorker)
	if updatedWorker.SpecDigest != worker.SpecDigest {
		t.Fatal("application metadata unexpectedly changed the worker runtime digest")
	}
	updatedWeb := web
	updatedWeb.ApplicationGeneration = 2
	updatedWeb.RevisionID = "revision-two"
	updatedWeb.Image = "sha256:" + strings.Repeat("b", 64)
	updatedWeb.Rollback = rollbackFromAllocation(web)
	updatedWeb.SpecDigest = mustAllocationDigest(t, updatedWeb)
	assignment.Allocations = []model.AllocationDesired{updatedWorker, updatedWeb}
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	newWeb := runtime.SnapshotObjects()[orchruntime.ObjectID(updatedWeb.ID, updatedWeb.SpecDigest)]
	if newWeb.Address == "" {
		t.Fatalf("updated web candidate was not started: %#v", runtime.SnapshotObjects())
	}
	prober.FailAddress = newWeb.Address
	var statuses []model.AllocationStatus
	for range health.UnhealthyFailures {
		now = now.Add(health.ProbeInterval)
		var err error
		statuses, err = supervisor.Reconcile(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range statuses {
		if status.Reason != "health_gate_rollback" || status.ActiveGeneration != 1 {
			t.Fatalf("application rollback status = %#v", statuses)
		}
	}
	objects := runtime.SnapshotObjects()
	if object, exists := objects[workerObjectID]; !exists || !object.Running {
		t.Fatalf("unchanged worker was restarted or removed: %#v", objects)
	}
	for _, operation := range runtime.SnapshotOperations() {
		if operation == "stop:"+workerObjectID || operation == "remove:"+workerObjectID {
			t.Fatalf("unchanged worker was mutated during metadata rollback: %v", runtime.SnapshotOperations())
		}
	}
}

type addressFailingProber struct {
	FailAddress string
}

func (p *addressFailingProber) Probe(_ context.Context, target health.Target) error {
	if target.Address.String() == p.FailAddress {
		return errors.New("selected endpoint is unhealthy")
	}
	return nil
}

func supervisorCache(t *testing.T) *state.Store {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	cache, err := state.Open(context.Background(), filepath.Join(directory, "agent.db"), "cluster-one", "node-one")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache
}

func supervisorAllocation(t *testing.T, image string) model.AllocationDesired {
	t.Helper()
	allocation := model.AllocationDesired{
		ID: "alloc-one", Application: "hello", Service: "worker", Replica: 1,
		ApplicationGeneration: 1, RevisionID: "revision-one", Image: image,
		Command: []string{"/bin/sleep"}, Args: []string{"3600"}, RestartPolicy: "always",
		CPUMillis: 100, MemoryBytes: 128 << 20, StartDeadlineSeconds: 30,
		AutoRollback: true, StartBeforeStop: true,
	}
	allocation.SpecDigest = mustAllocationDigest(t, allocation)
	return allocation
}

func supervisorHTTPAllocation(t *testing.T, id, image string, generation uint64) model.AllocationDesired {
	t.Helper()
	allocation := supervisorAllocation(t, image)
	allocation.ID = id
	allocation.Service = "web"
	allocation.ApplicationGeneration = generation
	allocation.NetworkGeneration = 1
	allocation.MaxSurge = 1
	allocation.HTTP = &model.HTTPDesired{Port: 8080, Domains: []string{"app.example.com"}, HealthPath: "/healthz", HTTPS: true}
	allocation.SpecDigest = mustAllocationDigest(t, allocation)
	return allocation
}

func rollbackFromAllocation(allocation model.AllocationDesired) *model.RollbackSpec {
	return &model.RollbackSpec{
		ApplicationGeneration: allocation.ApplicationGeneration, RevisionID: allocation.RevisionID,
		SpecDigest: allocation.SpecDigest, Image: allocation.Image,
		Command: append([]string(nil), allocation.Command...), Args: append([]string(nil), allocation.Args...),
		Environment: cloneStringMap(allocation.Environment), RestartPolicy: allocation.RestartPolicy,
		CPUMillis: allocation.CPUMillis, MemoryBytes: allocation.MemoryBytes,
		Mounts: append([]model.VolumeMount(nil), allocation.Mounts...), StartDeadlineSeconds: allocation.StartDeadlineSeconds,
		HTTP: allocation.HTTP, NetworkGeneration: allocation.NetworkGeneration,
		AutoRollback: allocation.AutoRollback, StartBeforeStop: allocation.StartBeforeStop,
	}
}

func mustAllocationDigest(t *testing.T, allocation model.AllocationDesired) string {
	t.Helper()
	digest, err := model.AllocationSpecDigest(allocation)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
