package agent

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/agent/state"
	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/health"
	"github.com/TorstenDittmann/orch/internal/ingress"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/network"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
	"github.com/TorstenDittmann/orch/internal/tcpingress"
)

var supervisorTestTime = time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)

const supervisorExecutableImage = "docker.io/library/busybox@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

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

func TestSupervisorReportsOneAllocationFailureAndReconcilesTheRest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runtime := orchruntime.NewFake()
	supervisor := NewSupervisor(supervisorCache(t), runtime, func() time.Time { return supervisorTestTime })
	assignment := ownershipMismatchAssignment(t, runtime)
	broken := assignment.Allocations[0]
	brokenID := orchruntime.ObjectID(broken.ID, broken.SpecDigest)

	statuses, err := supervisor.Reconcile(ctx, assignment)
	if !errors.Is(err, ErrAllocationReconcile) || !errors.Is(err, orchruntime.ErrOwnershipMismatch) {
		t.Fatalf("Reconcile() error = %v, want an aggregated ownership mismatch", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %#v, want both allocations", statuses)
	}
	for _, status := range statuses {
		switch status.ID {
		case broken.ID:
			if status.Phase != model.AllocationFailed || status.Reason != "reconcile_failed" || status.Message == "" {
				t.Fatalf("broken status = %#v", status)
			}
		default:
			if status.Phase != model.AllocationRunning {
				t.Fatalf("healthy allocation was not reconciled: %#v", status)
			}
		}
	}

	// The failure is not persisted as terminal: once the fault clears, the
	// next pass converges the allocation.
	delete(runtime.Objects, brokenID+"-duplicate")
	statuses, err = supervisor.Reconcile(ctx, assignment)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.Phase != model.AllocationRunning {
			t.Fatalf("status after repair = %#v", status)
		}
	}
}

func TestReconcileFailureStatusDropsReadyHealth(t *testing.T) {
	t.Parallel()
	readyAt := supervisorTestTime
	status := reconcileFailureStatus(model.AllocationStatus{
		Phase: model.AllocationRunning, HealthStatus: model.HealthReady, ReadyAt: &readyAt,
	}, errors.New("lost CNI cache"), supervisorTestTime)
	if status.Phase != model.AllocationFailed || status.HealthStatus == model.HealthReady || status.ReadyAt != nil {
		t.Fatalf("failure status = %#v, want a failed allocation that no longer claims Ready", status)
	}
}

// ownershipMismatchAssignment returns two allocations, the first of which has
// two runtime objects claiming its exact spec.
func ownershipMismatchAssignment(t *testing.T, runtime *orchruntime.Fake) model.Assignment {
	t.Helper()
	broken := supervisorAllocation(t, supervisorExecutableImage)
	broken.ID, broken.Replica = "alloc-a", 1
	broken.SpecDigest = mustAllocationDigest(t, broken)
	healthy := supervisorAllocation(t, supervisorExecutableImage)
	healthy.ID, healthy.Replica = "alloc-b", 2
	healthy.SpecDigest = mustAllocationDigest(t, healthy)
	brokenID := orchruntime.ObjectID(broken.ID, broken.SpecDigest)
	for _, id := range []string{brokenID, brokenID + "-duplicate"} {
		runtime.Objects[id] = orchruntime.Object{ID: id, AllocationID: broken.ID, SpecDigest: broken.SpecDigest, Running: true}
	}
	return model.Assignment{Allocations: []model.AllocationDesired{broken, healthy}}
}

func TestSupervisorStartsDependenciesBeforeDependentsAndDoesNotStopRunningDependent(t *testing.T) {
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	supervisor := NewSupervisor(cache, runtime, func() time.Time { return supervisorTestTime })
	database := supervisorAllocation(t, supervisorExecutableImage)
	database.ID, database.Service, database.RestartPolicy = "alloc-z", "database", "never"
	database.SpecDigest = mustAllocationDigest(t, database)
	web := supervisorAllocation(t, supervisorExecutableImage)
	web.ID, web.Service, web.DependsOn = "alloc-a", "web", []string{"database"}
	web.SpecDigest = mustAllocationDigest(t, web)
	assignment := model.Assignment{Allocations: []model.AllocationDesired{web, database}}
	statuses, err := supervisor.Reconcile(ctx, assignment)
	if err != nil {
		t.Fatal(err)
	}
	operations := runtime.SnapshotOperations()
	if len(operations) != 2 || !strings.Contains(operations[0], database.ID) || !strings.Contains(operations[1], web.ID) {
		t.Fatalf("dependency start order = %v", operations)
	}
	if err := runtime.Exit(orchruntime.ObjectID(database.ID, database.SpecDigest), 1); err != nil {
		t.Fatal(err)
	}
	statuses, err = supervisor.Reconcile(ctx, assignment)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.Service == "web" && status.Phase != model.AllocationRunning {
			t.Fatalf("running dependent was disrupted: %#v", statuses)
		}
	}
}

func TestSupervisorSchedulesOneShotCronRunAndRecordsCompletion(t *testing.T) {
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	now := supervisorTestTime.Add(30 * time.Second)
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}})
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	job := model.CronJobDesired{Application: "hello", Job: "backup", ApplicationGeneration: 1,
		RevisionID: "revision-one", Image: supervisorExecutableImage, Schedule: "* * * * *", CPUMillis: 100,
		MemoryBytes: 64 << 20, StartDeadlineSeconds: 30, TimeoutSeconds: 60, NetworkGeneration: 1}
	job.SpecDigest, err = model.CronJobSpecDigest(job)
	if err != nil {
		t.Fatal(err)
	}
	assignment := model.Assignment{Network: networkConfig, CronJobs: []model.CronJobDesired{job}}
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 9, 2, 12, 1, 0, 0, time.UTC)
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	_, runs := supervisor.CronObservations()
	if len(runs) != 1 || runs[0].State != model.JobRunRunning {
		t.Fatalf("running cron observations = %#v", runs)
	}
	objects := runtime.SnapshotObjects()
	var objectID string
	for id, object := range objects {
		if strings.HasPrefix(object.AllocationID, "job_") {
			objectID = id
		}
	}
	if objectID == "" {
		t.Fatalf("cron runtime object not created: %#v", objects)
	}
	runtime.SetLogs(objectID, []orchruntime.LogFrame{{Stream: "stdout", Data: []byte("backup complete\n")}})
	if err := runtime.Exit(objectID, 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	_, runs = supervisor.CronObservations()
	if len(runs) != 1 || runs[0].State != model.JobRunSucceeded {
		t.Fatalf("completed cron observations = %#v", runs)
	}
	_, networkStatus := supervisor.Observations()
	if networkStatus.DrainingObjects != 0 {
		t.Fatalf("retained cron history counted as %d draining objects", networkStatus.DrainingObjects)
	}
	frames, err := supervisor.TailCronLogs(ctx, runs[0].ID, 20)
	if err != nil || len(frames) != 1 || string(frames[0].Data) != "backup complete\n" {
		t.Fatalf("retained cron logs = %#v, %v", frames, err)
	}
}

func TestSupervisorReportsCronScheduleThatNeverFires(t *testing.T) {
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	now := supervisorTestTime
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}})
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	newJob := func(name, expression string) model.CronJobDesired {
		job := model.CronJobDesired{Application: "hello", Job: name, ApplicationGeneration: 1,
			RevisionID: "revision-one", Image: supervisorExecutableImage, Schedule: expression, CPUMillis: 100,
			MemoryBytes: 64 << 20, StartDeadlineSeconds: 30, TimeoutSeconds: 60, NetworkGeneration: 1}
		job.SpecDigest, err = model.CronJobSpecDigest(job)
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	assignment := model.Assignment{Network: networkConfig, CronJobs: []model.CronJobDesired{newJob("backup", "* * * * *"), newJob("never", "0 0 30 2 *")}}
	for range 3 {
		if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	jobs, runs := supervisor.CronObservations()
	if len(jobs) != 2 || jobs[1].Job != "never" || jobs[1].Reason != "schedule_invalid" || jobs[1].NextRunAt != nil {
		t.Fatalf("cron job observations = %#v", jobs)
	}
	if jobs[0].NextRunAt == nil {
		t.Fatalf("valid job lost its schedule: %#v", jobs[0])
	}
	for _, run := range runs {
		if run.Job == "never" {
			t.Fatalf("impossible schedule produced run %#v", run)
		}
	}
}

func TestSupervisorPublishesReadyTCPRoute(t *testing.T) {
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	proxy := &fakeTCPIngress{}
	now := supervisorTestTime
	local := network.NewFake()
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now }, SupervisorServices{
		Prober: &health.Fake{}, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}, TCPIngress: proxy,
		NodePorts: local,
	})
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	allocation := supervisorAllocation(t, supervisorExecutableImage)
	allocation.NetworkGeneration = 1
	allocation.TCP = &model.TCPDesired{Port: 9418, PublicPort: 9418}
	allocation.SpecDigest = mustAllocationDigest(t, allocation)
	assignment := model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{allocation}}
	for range health.ReadySuccesses {
		if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
			t.Fatal(err)
		}
		now = now.Add(health.ProbeInterval)
	}
	routes := supervisor.TCPObservations()
	if len(routes) != 1 || routes[0].State != model.TCPRouteActive || routes[0].UpstreamCount != 1 || len(proxy.routes) != 1 {
		t.Fatalf("TCP routes = %#v, applied = %#v", routes, proxy.routes)
	}
	// Workloads on this node may reach the route's public port.
	if !slices.Equal(local.IngressPorts, []uint16{9418}) {
		t.Fatalf("ingress ports = %v, want [9418]", local.IngressPorts)
	}
}

type fakeTCPIngress struct{ routes []tcpingress.Route }

func (f *fakeTCPIngress) Apply(_ context.Context, routes []tcpingress.Route) []model.TCPRouteStatus {
	f.routes = append([]tcpingress.Route(nil), routes...)
	result := make([]model.TCPRouteStatus, 0, len(routes))
	for _, route := range routes {
		result = append(result, model.TCPRouteStatus{Application: route.Application, Service: route.Service,
			PublicPort: route.PublicPort, TargetPort: route.TargetPort, State: model.TCPRouteActive,
			UpstreamCount: uint32(len(route.Upstreams)), UpdatedAt: supervisorTestTime})
	}
	return result
}
func (f *fakeTCPIngress) Close() error { return nil }

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
	operationCount := len(runtime.SnapshotOperations())
	statuses, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].Phase != model.AllocationBackoff || statuses[0].RestartCount != 1 || statuses[0].Reason != "process_exit_backoff" ||
		len(runtime.SnapshotOperations()) != operationCount {
		t.Fatalf("exited process status = %#v, operations %v", statuses[0], runtime.SnapshotOperations())
	}
	states, err := cache.AllocationStates(ctx)
	if err != nil || len(states) != 1 || states[0].NextAttemptAt == nil || !states[0].NextAttemptAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("persisted exit backoff = %#v, %v", states, err)
	}
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil || statuses[0].Phase != model.AllocationBackoff || len(runtime.SnapshotOperations()) != operationCount {
		t.Fatalf("early restart = %#v, %v, operations %v", statuses, err, runtime.SnapshotOperations())
	}
	now = now.Add(3 * time.Second)
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil || statuses[0].Phase != model.AllocationBackoff || statuses[0].RestartCount != 2 ||
		statuses[0].Reason != "process_start_backoff" {
		t.Fatalf("failed restart status = %#v, %v", statuses, err)
	}
	states, err = cache.AllocationStates(ctx)
	if err != nil || len(states) != 1 || states[0].NextAttemptAt == nil || !states[0].NextAttemptAt.Equal(now.Add(4*time.Second)) {
		t.Fatalf("persisted injected backoff = %#v, %v", states, err)
	}
	operationCount = len(runtime.SnapshotOperations())
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil || statuses[0].Phase != model.AllocationBackoff || len(runtime.SnapshotOperations()) != operationCount {
		t.Fatalf("early retry = %#v, %v, operations %v", statuses, err, runtime.SnapshotOperations())
	}
	now = now.Add(5 * time.Second)
	runtime.SetEnsureError(allocation.ID, nil)
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil || statuses[0].Phase != model.AllocationRunning || statuses[0].RestartCount != 2 {
		t.Fatalf("successful retry = %#v, %v", statuses, err)
	}
	if states, err = cache.AllocationStates(ctx); err != nil || len(states) != 1 || states[0].NextAttemptAt != nil {
		t.Fatalf("persisted running state = %#v, %v", states, err)
	}

	// A process that starts successfully and then exits again waits for the
	// next, longer backoff instead of restarting on every reconcile tick.
	if err := runtime.Exit(id, 7); err != nil {
		t.Fatal(err)
	}
	operationCount = len(runtime.SnapshotOperations())
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil || statuses[0].Phase != model.AllocationBackoff || statuses[0].RestartCount != 3 ||
		len(runtime.SnapshotOperations()) != operationCount {
		t.Fatalf("second exit = %#v, %v, operations %v", statuses, err, runtime.SnapshotOperations())
	}
	states, err = cache.AllocationStates(ctx)
	if err != nil || len(states) != 1 || states[0].NextAttemptAt == nil || !states[0].NextAttemptAt.Equal(now.Add(8*time.Second)) {
		t.Fatalf("persisted second exit backoff = %#v, %v", states, err)
	}
	now = now.Add(7 * time.Second)
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil || statuses[0].Phase != model.AllocationBackoff || len(runtime.SnapshotOperations()) != operationCount {
		t.Fatalf("restart before backoff elapsed = %#v, %v, operations %v", statuses, err, runtime.SnapshotOperations())
	}
	now = now.Add(2 * time.Second)
	statuses, err = supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil || statuses[0].Phase != model.AllocationRunning || statuses[0].RestartCount != 3 {
		t.Fatalf("restart after backoff = %#v, %v", statuses, err)
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
	continued, err := supervisor.ReadLogs(ctx, allocation, 10, "cursor")
	if err != nil || len(continued.Frames) != 0 || continued.NextCursor != "fake:"+id || continued.Exited {
		t.Fatalf("ReadLogs() = %#v, %v", continued, err)
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
	if statuses[0].ActiveSpecDigest != allocation.SpecDigest || statuses[0].ActiveGeneration != allocation.ApplicationGeneration ||
		statuses[0].ActiveRevisionID != allocation.RevisionID {
		t.Fatalf("running process is missing revision metadata while warming: %#v", statuses[0])
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

func TestSupervisorRestartedProcessRepassesReadiness(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	prober := &health.Fake{}
	proxy := &ingress.Fake{}
	now := supervisorTestTime
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Prober: prober, Discovery: discovery.NewFake(), Ingress: proxy}, strings.NewReader(strings.Repeat("\x00", 64)))
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
	if err != nil || statuses[0].HealthStatus != model.HealthReady {
		t.Fatalf("initial readiness = %#v, %v", statuses, err)
	}

	if err := runtime.Exit(orchruntime.ObjectID(allocation.ID, allocation.SpecDigest), 1); err != nil {
		t.Fatal(err)
	}
	statuses, err = supervisor.Reconcile(ctx, assignment)
	if err != nil || statuses[0].Phase != model.AllocationBackoff || statuses[0].HealthStatus != model.HealthChecking ||
		statuses[0].ReadyAt != nil || statuses[0].ConsecutiveSuccesses != 0 {
		t.Fatalf("exited process status = %#v, %v", statuses, err)
	}
	routes, _ := supervisor.Observations()
	if len(routes) != 1 || routes[0].State == model.RouteActive {
		t.Fatalf("routes while exited = %#v", routes)
	}

	now = now.Add(3 * time.Second)
	probes := len(prober.Targets)
	statuses, err = supervisor.Reconcile(ctx, assignment)
	if err != nil || statuses[0].Phase != model.AllocationRunning || statuses[0].HealthStatus != model.HealthChecking ||
		statuses[0].ReadyAt != nil {
		t.Fatalf("restarted process status = %#v, %v", statuses, err)
	}
	if len(prober.Targets) != probes+1 {
		t.Fatalf("restarted process reused a cached probe: %d -> %d", probes, len(prober.Targets))
	}
	routes, _ = supervisor.Observations()
	if len(routes) != 1 || routes[0].State == model.RouteActive {
		t.Fatalf("routes before restarted process passed readiness = %#v", routes)
	}
	if states, err := cache.AllocationStates(ctx); err != nil || len(states) != 1 ||
		states[0].HealthStatus != model.HealthChecking || states[0].ReadyAt != nil {
		t.Fatalf("persisted restarted health = %#v, %v", states, err)
	}

	now = now.Add(health.ProbeInterval)
	statuses, err = supervisor.Reconcile(ctx, assignment)
	if err != nil || statuses[0].HealthStatus != model.HealthReady || statuses[0].ReadyAt == nil {
		t.Fatalf("restarted readiness = %#v, %v", statuses, err)
	}
	routes, _ = supervisor.Observations()
	if len(routes) != 1 || routes[0].State != model.RouteActive {
		t.Fatalf("routes after restarted process passed readiness = %#v", routes)
	}
}

func TestSupervisorRestartedSlowStartGetsFullHealthDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	now := supervisorTestTime
	runtime.Now = func() time.Time { return now }
	prober := &health.Fake{Err: errors.New("still warming up")}
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now },
		SupervisorServices{Prober: prober, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}}, strings.NewReader(strings.Repeat("\x00", 64)))
	allocation := supervisorHTTPAllocation(t, "alloc-web", "sha256:"+strings.Repeat("a", 64), 1)
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	assignment := model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{allocation}}
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}

	// The container was created long before it exits and restarts, so a
	// deadline measured from creation would already be exceeded.
	now = now.Add(time.Hour)
	if err := runtime.Exit(orchruntime.ObjectID(allocation.ID, allocation.SpecDigest), 1); err != nil {
		t.Fatal(err)
	}
	if statuses, err := supervisor.Reconcile(ctx, assignment); err != nil || statuses[0].Phase != model.AllocationBackoff {
		t.Fatalf("exited process status = %#v, %v", statuses, err)
	}
	now = now.Add(3 * time.Second)
	restartedAt := now
	statuses, err := supervisor.Reconcile(ctx, assignment)
	if err != nil || statuses[0].Phase != model.AllocationRunning || statuses[0].HealthStatus != model.HealthChecking {
		t.Fatalf("restarted process status = %#v, %v", statuses, err)
	}
	for range health.UnhealthyFailures + 1 {
		now = now.Add(health.ProbeInterval)
		statuses, err = supervisor.Reconcile(ctx, assignment)
		if err != nil || statuses[0].HealthStatus != model.HealthChecking {
			t.Fatalf("restarted process lost its startup grace: %#v, %v", statuses, err)
		}
	}

	deadline := time.Duration(allocation.StartDeadlineSeconds) * time.Second
	now = restartedAt.Add(deadline - health.ProbeInterval)
	statuses, err = supervisor.Reconcile(ctx, assignment)
	if err != nil || statuses[0].Phase != model.AllocationRunning || statuses[0].Reason == "health_deadline_exceeded" {
		t.Fatalf("restarted slow start inside its deadline = %#v, %v", statuses, err)
	}
	now = restartedAt.Add(deadline)
	statuses, err = supervisor.Reconcile(ctx, assignment)
	if err != nil || statuses[0].Phase != model.AllocationFailed || statuses[0].Reason != "health_deadline_exceeded" {
		t.Fatalf("restarted slow start past its deadline = %#v, %v", statuses, err)
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

func TestSupervisorRollsBackAfterStartupHealthDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	prober := &health.Fake{}
	proxy := &ingress.Fake{}
	now := supervisorTestTime
	runtime.Now = func() time.Time { return now }
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
	if statuses[0].Reason != "readiness_checking" {
		t.Fatalf("candidate rolled back before its startup deadline: %#v", statuses[0])
	}
	object := runtime.SnapshotObjects()[orchruntime.ObjectID(replacement.ID, replacement.SpecDigest)]
	now = object.StartedAt.Add(time.Duration(replacement.StartDeadlineSeconds) * time.Second)
	var err error
	statuses, err = supervisor.Reconcile(ctx, assignment)
	if err != nil {
		t.Fatal(err)
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
	if statuses[0].ActiveSpecDigest != replacement.SpecDigest || statuses[0].HealthStatus != model.HealthChecking || len(proxy.Routes) != 0 {
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

func TestDesiredRoutesPublishesPublicHTTPForLocalAndRemoteEndpoints(t *testing.T) {
	t.Parallel()
	local := supervisorHTTPAllocation(t, "alloc-local", "sha256:"+strings.Repeat("b", 64), 1)
	local.HTTP = &model.HTTPDesired{Port: 8080, Public: true, HealthPath: "/healthz"}
	local.SpecDigest = mustAllocationDigest(t, local)
	routes, pending := desiredRoutes([]model.AllocationDesired{local}, nil,
		map[string]string{local.ID: "10.88.0.2"}, map[string]string{local.ID: local.SpecDigest}, supervisorTestTime)
	if len(pending) != 0 || len(routes) != 1 || routes[0].Domain != model.PublicHTTPRoute || routes[0].HTTPS ||
		!reflect.DeepEqual(routes[0].Upstreams, []string{"10.88.0.2:8080"}) {
		t.Fatalf("local public routes = %#v, pending = %#v", routes, pending)
	}

	endpoint := model.ServiceEndpoint{AllocationID: "alloc-remote", Application: "hello", Service: "web",
		NodeID: "node-two", Address: "10.88.1.2", Port: 8080, HTTPPublic: true}
	assignment := model.Assignment{Network: model.NetworkConfig{Generation: 1, Endpoints: []model.ServiceEndpoint{endpoint}}}
	allocations, serving, specs := remoteServingView(assignment, nil, nil)
	routes, pending = desiredRoutes(allocations, nil, serving, specs, supervisorTestTime)
	if len(pending) != 0 || len(routes) != 1 || routes[0].Domain != model.PublicHTTPRoute ||
		!reflect.DeepEqual(routes[0].Upstreams, []string{"10.88.1.2:8080"}) {
		t.Fatalf("remote public routes = %#v, pending = %#v", routes, pending)
	}
}

func TestRemoteEndpointCatalogFeedsDNSHTTPAndTCP(t *testing.T) {
	t.Parallel()
	endpoint := model.ServiceEndpoint{
		AllocationID: "alloc-remote", Application: "hello", Service: "web", NodeID: "node-two",
		Address: "10.88.1.2", Port: 8080, Domains: []string{"app.example.com"}, HTTPS: true, TCPPublicPort: 9000,
	}
	assignment := model.Assignment{Network: model.NetworkConfig{Generation: 1, Endpoints: []model.ServiceEndpoint{endpoint}}}
	allocations, serving, specs := remoteServingView(assignment, nil, nil)
	routes, pending := desiredRoutes(allocations, nil, serving, specs, supervisorTestTime)
	if len(pending) != 0 || len(routes) != 1 || routes[0].Domain != "app.example.com" ||
		!reflect.DeepEqual(routes[0].Upstreams, []string{"10.88.1.2:8080"}) {
		t.Fatalf("remote HTTP routes = %#v, pending = %#v", routes, pending)
	}
	tcpRoutes := desiredTCPRoutes(allocations, serving, specs)
	if len(tcpRoutes) != 1 || tcpRoutes[0].PublicPort != 9000 ||
		!reflect.DeepEqual(tcpRoutes[0].Upstreams, []string{"10.88.1.2"}) {
		t.Fatalf("remote TCP routes = %#v", tcpRoutes)
	}
	snapshot := discoverySnapshot(nil, nil, assignment.Network.Endpoints)
	name := discovery.ServiceName("hello", "web")
	if !reflect.DeepEqual(snapshot.Addresses[name], []netip.Addr{netip.MustParseAddr("10.88.1.2")}) {
		t.Fatalf("remote DNS snapshot = %#v", snapshot)
	}
}

func TestNodeLocalHTTPDoesNotPublishRemoteCertificatesOrCatchAll(t *testing.T) {
	t.Parallel()
	for _, public := range []bool{false, true} {
		endpoint := model.ServiceEndpoint{
			AllocationID: "alloc-remote", Application: "hello", Service: "web", NodeID: "node-two",
			Address: "10.88.1.2", Port: 8080, Domains: []string{"game.example.com"}, HTTPS: true,
			HTTPNodeLocal: true, HTTPPublic: public, TCPPublicPort: 9000,
		}
		assignment := model.Assignment{NodeID: "node-one", Network: model.NetworkConfig{
			Generation: 1, Endpoints: []model.ServiceEndpoint{endpoint},
		}}
		allocations, serving, specs := remoteServingView(assignment, nil, nil)
		routes, pending := desiredRoutes(allocations, nil, serving, specs, supervisorTestTime)
		if len(routes) != 0 || len(pending) != 0 {
			t.Fatalf("another node's HTTP route was published: routes=%#v pending=%#v", routes, pending)
		}
		if routes := desiredTCPRoutes(allocations, serving, specs); len(routes) != 1 {
			t.Fatalf("node-local HTTP changed independent TCP ingress: %#v", routes)
		}
		snapshot := discoverySnapshot(nil, nil, assignment.Network.Endpoints)
		if len(snapshot.Addresses[discovery.ServiceName("hello", "web")]) != 1 {
			t.Fatal("node-local HTTP removed private cross-node discovery")
		}
		assignment.NodeID = endpoint.NodeID
		allocations, serving, specs = remoteServingView(assignment, nil, nil)
		routes, _ = desiredRoutes(allocations, nil, serving, specs, supervisorTestTime)
		want := 1
		if public {
			want++
		}
		if len(routes) != want {
			t.Fatalf("owning node lost its HTTP routes: %#v", routes)
		}
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
	runtime.Now = func() time.Time { return now }
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
	now = newSecond.StartedAt.Add(time.Duration(updatedSecond.StartDeadlineSeconds) * time.Second)
	var err error
	statuses, err = supervisor.Reconcile(ctx, assignment)
	if err != nil {
		t.Fatal(err)
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
	runtime.Now = func() time.Time { return now }
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
	now = newWeb.StartedAt.Add(time.Duration(updatedWeb.StartDeadlineSeconds) * time.Second)
	var err error
	statuses, err = supervisor.Reconcile(ctx, assignment)
	if err != nil {
		t.Fatal(err)
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

func TestCronRunCarriesReadOnlyVolumeMounts(t *testing.T) {
	job := model.CronJobDesired{Application: "hello", Job: "backup", ApplicationGeneration: 1,
		RevisionID: "revision-one", Image: supervisorExecutableImage, Schedule: "* * * * *", CPUMillis: 100,
		MemoryBytes: 64 << 20, StartDeadlineSeconds: 30, TimeoutSeconds: 60, NetworkGeneration: 1,
		Mounts: []model.VolumeMount{{Name: "sqld", Target: "/sqld", ReadOnly: true}}}
	var err error
	job.SpecDigest, err = model.CronJobSpecDigest(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := orchruntime.ValidateCronJob(job); err != nil {
		t.Fatalf("read-only cron mount rejected: %v", err)
	}
	allocation, err := cronRunAllocation(job, "job_one")
	if err != nil {
		t.Fatal(err)
	}
	if len(allocation.Mounts) != 1 || allocation.Mounts[0] != job.Mounts[0] {
		t.Fatalf("run mounts = %#v", allocation.Mounts)
	}
	if err := orchruntime.ValidateAllocation(allocation); err != nil {
		t.Fatalf("run allocation rejected: %v", err)
	}
	writable := job
	writable.Mounts = []model.VolumeMount{{Name: "sqld", Target: "/sqld"}}
	if err := orchruntime.ValidateCronJob(writable); err == nil {
		t.Fatal("writable cron mount accepted")
	}
	unmounted := job
	unmounted.Mounts = nil
	digest, err := model.CronJobSpecDigest(unmounted)
	if err != nil || digest == job.SpecDigest {
		t.Fatalf("mounts do not change the cron spec digest: %v", err)
	}
}

func TestSupervisorPublishesNodePortsOnceReady(t *testing.T) {
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	publisher := network.NewFake()
	publisher.NodePortErrors = map[string]error{"tcp/8787": errors.New("TCP port 8787 is already bound on this node")}
	now := supervisorTestTime
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return now }, SupervisorServices{
		Prober: &health.Fake{}, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}, NodePorts: publisher,
	})
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	networkConfig.IngressEnabled = false
	allocation := supervisorAllocation(t, supervisorExecutableImage)
	allocation.NetworkGeneration = 1
	allocation.MaxSurge, allocation.MaxUnavailable, allocation.StartBeforeStop = 0, 1, false
	allocation.TCP = &model.TCPDesired{Port: 8787, NodePort: 8787}
	allocation.UDP = &model.UDPDesired{Port: 8790, NodePort: 8790}
	allocation.SpecDigest = mustAllocationDigest(t, allocation)
	assignment := model.Assignment{Network: networkConfig, Allocations: []model.AllocationDesired{allocation}}
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	pending := supervisor.NodePortObservations()
	if len(pending) != 2 || pending[0].State != model.TCPRoutePending || pending[1].State != model.TCPRoutePending || len(publisher.NodePorts) != 0 {
		t.Fatalf("pending node ports = %#v, published = %#v", pending, publisher.NodePorts)
	}
	for range health.ReadySuccesses {
		now = now.Add(health.ProbeInterval)
		if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
			t.Fatal(err)
		}
	}
	ports := supervisor.NodePortObservations()
	if len(ports) != 2 || ports[0].Protocol != "tcp" || ports[0].State != model.TCPRouteFailed ||
		ports[0].Reason != "node_port_unavailable" || ports[1].Protocol != "udp" || ports[1].State != model.TCPRouteActive {
		t.Fatalf("node ports = %#v", ports)
	}
	if len(publisher.NodePorts) != 1 || publisher.NodePorts[0].Key() != "udp/8790" || publisher.NodePorts[0].TargetPort != 8790 ||
		!publisher.NodePorts[0].Address.IsPrivate() {
		t.Fatalf("published = %#v", publisher.NodePorts)
	}
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Network: networkConfig}); err != nil {
		t.Fatal(err)
	}
	if len(publisher.NodePorts) != 0 || len(supervisor.NodePortObservations()) != 0 {
		t.Fatalf("withdrawn node ports remain: %#v", publisher.NodePorts)
	}
}

func TestRemoteEndpointKeepsBasicAuthAndRedirects(t *testing.T) {
	t.Parallel()
	auth := &model.HTTPBasicAuth{User: "admin", PasswordHash: "$argon2id$v=19$m=47104,t=1,p=1$c2FsdA$a2V5"}
	endpoint := model.ServiceEndpoint{
		AllocationID: "alloc-remote", Application: "hello", Service: "web", NodeID: "node-two",
		Address: "10.88.1.2", Port: 8080, Domains: []string{"api.example.com", "app.example.com"}, HTTPS: true,
		BasicAuth: auth, Redirects: map[string]string{"www.app.example.com": "app.example.com", "app.example.net": "app.example.com"},
	}
	assignment := model.Assignment{Network: model.NetworkConfig{Generation: 1, Endpoints: []model.ServiceEndpoint{endpoint}}}
	allocations, serving, specs := remoteServingView(assignment, nil, nil)
	routes, pending := desiredRoutes(allocations, nil, serving, specs, supervisorTestTime)
	if len(pending) != 0 || len(routes) != 2 {
		t.Fatalf("routes = %#v, pending = %#v", routes, pending)
	}
	if routes[0].Domain != "api.example.com" || routes[0].BasicAuth == nil || *routes[0].BasicAuth != *auth || len(routes[0].RedirectFrom) != 0 {
		t.Fatalf("api route = %#v", routes[0])
	}
	if routes[1].Domain != "app.example.com" || routes[1].BasicAuth == nil ||
		!reflect.DeepEqual(routes[1].RedirectFrom, []string{"app.example.net", "www.app.example.com"}) {
		t.Fatalf("app route = %#v", routes[1])
	}
}
