package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/agent/state"
	"change.codes/torsten/better-swarm/internal/model"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

var supervisorTestTime = time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)

func TestSupervisorEnsuresAdoptsUpdatesAndRemoves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := supervisorCache(t)
	runtime := orchruntime.NewFake()
	now := supervisorTestTime
	supervisor := NewSupervisor(cache, runtime, func() time.Time { return now })
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

func mustAllocationDigest(t *testing.T, allocation model.AllocationDesired) string {
	t.Helper()
	digest, err := model.AllocationSpecDigest(allocation)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
